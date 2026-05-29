package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatsapp-bot/claude"
	"whatsapp-bot/config"
	"whatsapp-bot/prom"
)

const metricsCacheTTL = 5 * time.Minute

// labelsToFetch are the label names whose values are injected into the Claude prompt.
var labelsToFetch = []string{"job", "instance", "service", "env"}

type cache struct {
	hints       []claude.MetricHint
	labels      map[string][]string
	expiry      time.Time
}

type Handler struct {
	wa             *whatsmeow.Client
	claude         *claude.Client
	prom           *prom.Client
	allowedNumbers map[string]bool

	mu    sync.Mutex
	cache cache
}

func NewHandler(wa *whatsmeow.Client, cfg *config.Config) *Handler {
	return &Handler{
		wa:             wa,
		claude:         claude.New(cfg.ClaudeEndpoint, cfg.ClaudeModel, cfg.ClaudeToken),
		prom:           prom.New(cfg.PrometheusURL),
		allowedNumbers: cfg.AllowedNumbers,
	}
}

func (h *Handler) allowed(msg *events.Message) bool {
	if len(h.allowedNumbers) == 0 {
		return true
	}
	return h.allowedNumbers[msg.Info.Sender.User]
}

func (h *Handler) Handle(evt interface{}) {
	msg, ok := evt.(*events.Message)
	if !ok {
		return
	}

	if !h.allowed(msg) {
		log.Printf("[whitelist] blocked message from %s", msg.Info.Sender.User)
		return
	}

	text := extractText(msg)
	if text == "" {
		log.Printf("[handler] ignored empty/non-text message from %s", msg.Info.Sender.User)
		return
	}

	log.Printf("[handler] received from %s: %q", msg.Info.Sender.User, text)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reply, err := h.answer(ctx, text)
	if err != nil {
		log.Printf("[handler] answer error: %v", err)
		reply = "Sorry, something went wrong. Please try again."
	}

	log.Printf("[handler] replying to %s: %q", msg.Info.Sender.User, reply)

	h.wa.SendMessage(ctx, msg.Info.Chat, &waE2E.Message{
		Conversation: proto.String(reply),
	})
}

func (h *Handler) answer(ctx context.Context, question string) (string, error) {
	hints, labels, err := h.getCache(ctx)
	if err != nil {
		log.Printf("cache refresh failed, proceeding without hints: %v", err)
	}

	promql, err := h.claude.Query(ctx, question, hints, labels)
	if err != nil {
		var clarErr *claude.ClarificationError
		if errors.As(err, &clarErr) {
			log.Printf("[claude] clarification needed: %s", clarErr.Message)
			return clarErr.Message, nil
		}
		return "", fmt.Errorf("claude: %w", err)
	}
	log.Printf("[claude] generated promql: %s", promql)

	// Ensure histogram queries always aggregate by (le) to return a single series.
	promql = ensureSumByLe(promql)

	result, err := h.prom.Query(ctx, promql)
	if err != nil {
		return "", fmt.Errorf("prometheus: %w", err)
	}
	log.Printf("[prom] result: %s", result)

	if result == "No data found." {
		log.Printf("[prom] no data, asking claude to refine")
		refined, rerr := h.claude.Refine(ctx, question, promql, hints, labels)
		if rerr == nil && refined != promql {
			refined = ensureSumByLe(refined)
			log.Printf("[claude] refined promql: %s", refined)
			if r2, rerr2 := h.prom.Query(ctx, refined); rerr2 == nil && r2 != "No data found." {
				result = r2
				promql = refined
			}
		}
		if result == "No data found." {
			return fmt.Sprintf("No data found for your question.\n_(tried: `%s`)_", promql), nil
		}
	}

	// Format the raw result into a friendly natural language response.
	friendly, ferr := h.claude.Format(ctx, question, result)
	if ferr != nil {
		log.Printf("[format] error: %v, falling back to raw", ferr)
		return fmt.Sprintf("%s\n\n_(query: `%s`)_", result, promql), nil
	}
	return fmt.Sprintf("%s\n\n_(query: `%s`)_", friendly, promql), nil
}

// ensureSumByLe rewrites histogram_quantile queries that are missing sum() by (le)
// so they always return a single aggregated series.
func ensureSumByLe(promql string) string {
	if !strings.Contains(promql, "histogram_quantile") || strings.Contains(promql, "sum(") {
		return promql
	}
	// Wrap rate() with sum(...) by (le)
	promql = strings.Replace(promql, ", rate(", ", sum(rate(", 1)
	// Insert ) by (le) before the second-to-last closing paren
	last := strings.LastIndex(promql, ")")
	if last < 0 {
		return promql
	}
	secondLast := strings.LastIndex(promql[:last], ")")
	if secondLast < 0 {
		return promql
	}
	return promql[:secondLast+1] + " by (le)" + promql[secondLast+1:]
}

// getCache returns cached metric hints and label values, refreshing when stale.
func (h *Handler) getCache(ctx context.Context) ([]claude.MetricHint, map[string][]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if time.Now().Before(h.cache.expiry) {
		return h.cache.hints, h.cache.labels, nil
	}

	// Fetch metric names + metadata
	names, err := h.prom.ListMetricNames(ctx)
	if err != nil {
		return nil, nil, err
	}
	meta, _ := h.prom.GetMetricMetadata(ctx)

	hints := make([]claude.MetricHint, 0, len(names))
	for _, name := range names {
		hint := claude.MetricHint{Name: name}
		if m, ok := meta[name]; ok {
			hint.Help = m.Help
			hint.Type = m.Type
		}
		hints = append(hints, hint)
	}

	// Fetch important label values so Claude knows real job/instance names
	labelMap := make(map[string][]string)
	for _, label := range labelsToFetch {
		vals, lerr := h.prom.ListLabelValues(ctx, label)
		if lerr == nil && len(vals) > 0 {
			labelMap[label] = vals
		}
	}
	log.Printf("[cache] refreshed: %d metrics, labels: %v", len(hints), labelMap)

	h.cache = cache{hints: hints, labels: labelMap, expiry: time.Now().Add(metricsCacheTTL)}
	return hints, labelMap, nil
}

func extractText(msg *events.Message) string {
	if c := msg.Message.GetConversation(); c != "" {
		return strings.TrimSpace(c)
	}
	if e := msg.Message.GetExtendedTextMessage(); e != nil {
		return strings.TrimSpace(e.GetText())
	}
	return ""
}
