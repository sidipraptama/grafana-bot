package bot

import (
	"context"
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

// metricsCacheTTL controls how long discovered metric names are reused.
const metricsCacheTTL = 5 * time.Minute

type Handler struct {
	wa             *whatsmeow.Client
	claude         *claude.Client
	prom           *prom.Client
	allowedNumbers map[string]bool

	mu          sync.Mutex
	cachedHints []claude.MetricHint
	cacheExpiry time.Time
}

func NewHandler(wa *whatsmeow.Client, cfg *config.Config) *Handler {
	return &Handler{
		wa:             wa,
		claude:         claude.New(cfg.ClaudeEndpoint, cfg.ClaudeModel, cfg.ClaudeToken),
		prom:           prom.New(cfg.PrometheusURL),
		allowedNumbers: cfg.AllowedNumbers,
	}
}

// allowed returns true if the sender is whitelisted, or if no whitelist is configured.
func (h *Handler) allowed(msg *events.Message) bool {
	if len(h.allowedNumbers) == 0 {
		return true
	}
	sender := msg.Info.Sender.User // the number part of the JID, e.g. "628123456789"
	return h.allowedNumbers[sender]
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
	hints, err := h.getMetricHints(ctx)
	if err != nil {
		log.Printf("metric discovery failed, proceeding without hints: %v", err)
		hints = nil
	}

	// First attempt
	promql, err := h.claude.Query(ctx, question, hints)
	if err != nil {
		return "", fmt.Errorf("claude: %w", err)
	}
	log.Printf("[claude] generated promql: %s", promql)

	result, err := h.prom.Query(ctx, promql)
	if err != nil {
		return "", fmt.Errorf("prometheus: %w", err)
	}
	log.Printf("[prom] result: %s", result)

	// Retry once when Prometheus returned no data
	if result == "No data found." {
		log.Printf("[prom] no data, asking claude to refine")
		refined, rerr := h.claude.Refine(ctx, question, promql, hints)
		if rerr == nil && refined != promql {
			log.Printf("[claude] refined promql: %s", refined)
			if r2, rerr2 := h.prom.Query(ctx, refined); rerr2 == nil && r2 != "No data found." {
				return fmt.Sprintf("%s\n\n_(query: `%s`)_", r2, refined), nil
			}
		}
		return fmt.Sprintf("No data found for your question.\n_(tried: `%s`)_", promql), nil
	}

	return fmt.Sprintf("%s\n\n_(query: `%s`)_", result, promql), nil
}

// getMetricHints returns cached metric hints, refreshing when stale.
func (h *Handler) getMetricHints(ctx context.Context) ([]claude.MetricHint, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if time.Now().Before(h.cacheExpiry) {
		return h.cachedHints, nil
	}

	names, err := h.prom.ListMetricNames(ctx)
	if err != nil {
		return nil, err
	}

	meta, _ := h.prom.GetMetricMetadata(ctx) // best-effort; ignore error

	hints := make([]claude.MetricHint, 0, len(names))
	for _, name := range names {
		h := claude.MetricHint{Name: name}
		if m, ok := meta[name]; ok {
			h.Help = m.Help
			h.Type = m.Type
		}
		hints = append(hints, h)
	}

	h.cachedHints = hints
	h.cacheExpiry = time.Now().Add(metricsCacheTTL)
	return hints, nil
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
