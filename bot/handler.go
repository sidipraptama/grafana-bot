package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"whatsapp-bot/claude"
	"whatsapp-bot/config"
	"whatsapp-bot/prom"
)

const (
	metricsCacheTTL = 5 * time.Minute
	maxHistory      = 6 // 3 back-and-forth turns per user
)

var labelsToFetch = []string{"job", "instance", "service", "env", "team", "host"}

type metricsCache struct {
	hints  []claude.MetricHint
	labels map[string][]string
	expiry time.Time
}

type Handler struct {
	bot          *tgbotapi.BotAPI
	claude       *claude.Client
	prom         *prom.Client
	allowedUsers map[int64]bool

	cacheMu sync.Mutex
	cache   metricsCache

	histMu  sync.Mutex
	history map[int64][]claude.ConversationTurn
}

func NewHandler(bot *tgbotapi.BotAPI, cfg *config.Config) *Handler {
	return &Handler{
		bot:          bot,
		claude:       claude.New(cfg.ClaudeEndpoint, cfg.ClaudeModel, cfg.ClaudeToken),
		prom:         prom.New(cfg.PrometheusURL),
		allowedUsers: cfg.AllowedUsers,
		history:      make(map[int64][]claude.ConversationTurn),
	}
}

func (h *Handler) allowed(userID int64) bool {
	if len(h.allowedUsers) == 0 {
		return true
	}
	return h.allowedUsers[userID]
}

func (h *Handler) Handle(update tgbotapi.Update) {
	msg := update.Message
	if msg == nil {
		return
	}

	if !h.allowed(msg.From.ID) {
		log.Printf("[whitelist] blocked user %d (@%s)", msg.From.ID, msg.From.UserName)
		return
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	// Handle /start and greetings directly without calling Claude.
	if isGreeting(text) {
		welcome := "Hi! 👋 I'm your infrastructure metrics assistant.\n\nAsk me anything about your servers, e.g:\n- Are the prod instances up?\n- What's the p99 latency right now?\n- Any downtime this week on the private instance?\n- How's the CPU on the public prod server?"
		out := tgbotapi.NewMessage(msg.Chat.ID, welcome)
		h.bot.Send(out)
		return
	}

	log.Printf("[handler] received from %d (@%s): %q", msg.From.ID, msg.From.UserName, text)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reply, err := h.answer(ctx, msg.From.ID, text)
	if err != nil {
		log.Printf("[handler] answer error: %v", err)
		reply = "Sorry, something went wrong. Please try again."
	}

	log.Printf("[handler] replying to %d: %q", msg.From.ID, reply)

	// Store the turn in history (strip HTML tags for clean context in future prompts)
	h.addHistory(msg.From.ID, text, stripHTML(reply))

	out := tgbotapi.NewMessage(msg.Chat.ID, reply)
	out.ParseMode = "HTML"
	if _, sendErr := h.bot.Send(out); sendErr != nil {
		log.Printf("[handler] html send failed (%v), retrying as plain text", sendErr)
		out.ParseMode = ""
		out.Text = stripHTML(reply)
		h.bot.Send(out)
	}
}

func (h *Handler) answer(ctx context.Context, userID int64, question string) (string, error) {
	hints, labels, err := h.getMetricsCache(ctx)
	if err != nil {
		log.Printf("cache refresh failed, proceeding without hints: %v", err)
	}

	history := h.getHistory(userID)

	promql, err := h.claude.Query(ctx, question, hints, labels, history)
	if err != nil {
		var clarErr *claude.ClarificationError
		if errors.As(err, &clarErr) {
			log.Printf("[claude] asking clarification: %s", clarErr.Message)
			return clarErr.Message, nil
		}
		return "", fmt.Errorf("claude: %w", err)
	}
	log.Printf("[claude] generated promql: %s", promql)

	promql = ensureSumByLe(promql)

	result, err := h.prom.Query(ctx, promql)
	if err != nil {
		return "", fmt.Errorf("prometheus: %w", err)
	}
	log.Printf("[prom] result: %s", result)

	if result == "No data found." {
		log.Printf("[prom] no data, asking claude to refine")
		refined, rerr := h.claude.Refine(ctx, question, promql, hints, labels, history)
		if rerr == nil && refined != promql {
			refined = ensureSumByLe(refined)
			log.Printf("[claude] refined promql: %s", refined)
			if r2, rerr2 := h.prom.Query(ctx, refined); rerr2 == nil && r2 != "No data found." {
				result = r2
				promql = refined
			}
		}
		if result == "No data found." {
			return fmt.Sprintf("No data found for your question.\n\n<i>tried: <code>%s</code></i>", html.EscapeString(promql)), nil
		}
	}

	friendly, ferr := h.claude.Format(ctx, question, promql, result)
	if ferr != nil {
		log.Printf("[format] error: %v, falling back to raw", ferr)
		return fmt.Sprintf("%s\n\n<i>query: <code>%s</code></i>", html.EscapeString(result), html.EscapeString(promql)), nil
	}
	return fmt.Sprintf("%s\n\n<i>query: <code>%s</code></i>", friendly, html.EscapeString(promql)), nil
}

// getHistory returns a copy of the conversation history for a user.
func (h *Handler) getHistory(userID int64) []claude.ConversationTurn {
	h.histMu.Lock()
	defer h.histMu.Unlock()
	src := h.history[userID]
	out := make([]claude.ConversationTurn, len(src))
	copy(out, src)
	return out
}

// addHistory appends a turn and trims to maxHistory entries.
func (h *Handler) addHistory(userID int64, question, answer string) {
	h.histMu.Lock()
	defer h.histMu.Unlock()
	hist := append(h.history[userID], claude.ConversationTurn{Question: question, Answer: answer})
	if len(hist) > maxHistory {
		hist = hist[len(hist)-maxHistory:]
	}
	h.history[userID] = hist
}

// getMetricsCache returns cached metric hints and label values, refreshing when stale.
func (h *Handler) getMetricsCache(ctx context.Context) ([]claude.MetricHint, map[string][]string, error) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()

	if time.Now().Before(h.cache.expiry) {
		return h.cache.hints, h.cache.labels, nil
	}

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

	labelMap := make(map[string][]string)
	for _, label := range labelsToFetch {
		vals, lerr := h.prom.ListLabelValues(ctx, label)
		if lerr == nil && len(vals) > 0 {
			labelMap[label] = vals
		}
	}
	log.Printf("[cache] refreshed: %d metrics, labels: %v", len(hints), labelMap)

	h.cache = metricsCache{hints: hints, labels: labelMap, expiry: time.Now().Add(metricsCacheTTL)}
	return hints, labelMap, nil
}

// ensureSumByLe rewrites histogram_quantile queries missing sum() by (le).
func ensureSumByLe(promql string) string {
	if !strings.Contains(promql, "histogram_quantile") || strings.Contains(promql, "sum(") {
		return promql
	}
	promql = strings.Replace(promql, ", rate(", ", sum(rate(", 1)
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

func isGreeting(text string) bool {
	lower := strings.ToLower(strings.TrimPrefix(text, "/"))
	greetings := []string{"start", "hello", "hi", "hey", "help", "halo", "hei"}
	for _, g := range greetings {
		if lower == g || strings.HasPrefix(lower, g+" ") {
			return true
		}
	}
	return false
}

func stripHTML(s string) string {
	s = strings.ReplaceAll(s, "<b>", "")
	s = strings.ReplaceAll(s, "</b>", "")
	s = strings.ReplaceAll(s, "<i>", "")
	s = strings.ReplaceAll(s, "</i>", "")
	s = strings.ReplaceAll(s, "<code>", "")
	s = strings.ReplaceAll(s, "</code>", "")
	return html.UnescapeString(s)
}
