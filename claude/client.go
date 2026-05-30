package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Client struct {
	endpoint string
	model    string
	token    string
	http     *http.Client
}

func New(endpoint, model, token string) *Client {
	return &Client{endpoint: endpoint, model: model, token: token, http: &http.Client{}}
}

type bedrockRequest struct {
	AnthropicVersion string    `json:"anthropic_version"`
	MaxTokens        int       `json:"max_tokens"`
	System           string    `json:"system,omitempty"`
	Messages         []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type bedrockResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// ConversationTurn holds one exchange between user and bot.
type ConversationTurn struct {
	Question string // what the user asked
	Answer   string // what the bot replied (plain text, no HTML)
}

const baseSystemPrompt = `You are a Prometheus metrics assistant for a team's infrastructure on Telegram.
Convert natural language questions into a single PromQL instant query.

STRICT RULES:
1. Respond with ONLY ONE single PromQL expression. Never combine multiple queries with commas or semicolons.
2. Use ONLY metric names from the Available Metrics list — never invent names.
3. Use ONLY label values from the Available Label Values list — never invent label values.
4. ALWAYS include team="group4" in every query — mandatory to scope data to this team only.
5. For rates/counters use a 5m window. For histograms use histogram_quantile().
6. Use MINIMUM label selectors. Only add env= or instance= when user explicitly mentions them.
7. If a question requires multiple metrics (e.g. "check CPU, memory and disk"), pick the MOST important one and query only that. Do not combine.

CLARIFICATION RULE (only exception to rule 1):
If the question uses vague pronouns ("it", "the instance", "the server", "the service", "the db")
AND the conversation history does not clarify what is being referred to,
respond with a short clarification question instead of a PromQL expression.
Ask about ALL missing dimensions in one question. Available dimensions:
- Instance: private, public, or both
- Environment: prod or staging
- Service: url-shortener, postgres, redis, rabbitmq
- Time range: e.g. last 24h, last 7 days

Examples:
- "Is it up?" → "Which instance (private/public) and environment (prod/staging) do you mean?"
- "How's the CPU?" → "Which instance (private/public) and environment (prod/staging)?"
- "Any issues?" → "Which service and environment are you asking about?"
- "Is the DB ok?" → "Which database — postgres or redis? And which environment (prod/staging)?"
- "Check the memory" → "Which instance (private/public) and environment (prod/staging)?"

Do NOT ask if the question already specifies enough context to answer directly.
Do NOT ask about env if user already said "production" or "prod" or "staging".

Job context:
- "private instance/server/EC2"       → job="node-exporter-private"
- "public instance/server/EC2"        → job="node-exporter-public"
- "both instances" / "all instances"  → job=~"node-exporter-private|node-exporter-public"
- "grafana instance/server" / "infra server" / "monitoring server" → use host="ip-172-31-162-139", team="group4" (no job filter). Exact query patterns:
    Status:  up{host="ip-172-31-162-139",team="group4"}
    CPU:     100 - (avg(rate(node_cpu_seconds_total{mode="idle",host="ip-172-31-162-139",team="group4"}[5m])) * 100)
    Memory:  (1 - node_memory_MemAvailable_bytes{host="ip-172-31-162-139",team="group4"} / node_memory_MemTotal_bytes{host="ip-172-31-162-139",team="group4"}) * 100
    Disk:    (1 - node_filesystem_avail_bytes{host="ip-172-31-162-139",team="group4",mountpoint="/"} / node_filesystem_size_bytes{host="ip-172-31-162-139",team="group4",mountpoint="/"}) * 100
    Disk IO: rate(node_disk_read_bytes_total{host="ip-172-31-162-139",team="group4"}[5m])
- "app/service/HTTP/latency/p50/p95/p99" → job="url-shortener" with http_server_request_duration_seconds_bucket
- "is the app/service up/running?" → url-shortener has NO up metric; use rate(http_server_request_duration_seconds_count{job="url-shortener",team="group4"}[5m])
- "postgres/database/db"              → job="postgresql"
- "redis/cache"                       → job="redis"
- "rabbitmq/queue"                    → job="rabbitmq"
- "prometheus/monitoring metrics"     → job="prometheus"

For "upgrade needed?" / "resource saturation?" / "should I scale?" questions — check memory usage % as the primary indicator:
100 * (1 - avg by (job) (node_memory_MemAvailable_bytes{job=~"node-exporter-.*",team="group4"}) / avg by (job) (node_memory_MemTotal_bytes{job=~"node-exporter-.*",team="group4"}))

For "are instances up?" queries — ALWAYS aggregate to avoid duplicate series:
max by (job) (up{job=~"node-exporter-.*",team="group4"})
For a specific instance: max by (job) (up{job="node-exporter-private",team="group4"})

Latency percentiles — ALWAYS use sum by (le):
histogram_quantile(0.NN, sum(rate(http_server_request_duration_seconds_bucket{job="url-shortener",team="group4"}[5m])) by (le))

Historical queries:
- "any downtime last Xd?"        → min_over_time(up{job="...",team="group4"}[Xd])
- "how many times down last Xd?" → changes(up{job="...",team="group4"}[Xd])
- "this week"/"last 7 days"      → [7d], "today"/"last 24h" → [24h], "this month" → [30d]
- NEVER use a plain instant metric for time-range questions — use _over_time functions

url-shortener app metric label rules:
- Valid filters: job, http_route, http_request_method, http_response_status_code, env, host
- NEVER use instance= on these metrics
- "public/private" does NOT apply to app metrics — one url-shortener service only
- Exclude health checks: http_route!="/health"
- Filter errors: http_response_status_code=~"5.."`

// MetricHint carries the name and optional help text for one metric.
type MetricHint struct {
	Name string
	Help string
	Type string
}

func buildSystemPrompt(metrics []MetricHint, labels map[string][]string) string {
	var sb strings.Builder
	sb.WriteString(baseSystemPrompt)

	if len(labels) > 0 {
		sb.WriteString("\n\nAvailable Label Values:\n")
		for labelName, values := range labels {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", labelName, strings.Join(values, ", ")))
		}
	}

	if len(metrics) > 0 {
		sb.WriteString("\nAvailable Metrics (name — description):\n")
		for _, m := range metrics {
			if m.Help != "" {
				sb.WriteString(fmt.Sprintf("- %s — %s\n", m.Name, m.Help))
			} else {
				sb.WriteString(fmt.Sprintf("- %s\n", m.Name))
			}
		}
	}

	return sb.String()
}

// buildUserMessage prepends conversation history to the current question
// so Claude can resolve pronouns and follow-up answers correctly.
func buildUserMessage(history []ConversationTurn, question string) string {
	if len(history) == 0 {
		return question
	}
	var sb strings.Builder
	sb.WriteString("Previous conversation:\n")
	for _, t := range history {
		sb.WriteString(fmt.Sprintf("User: %s\nBot: %s\n", t.Question, t.Answer))
	}
	sb.WriteString("\nCurrent message: ")
	sb.WriteString(question)
	return sb.String()
}

// Query translates a natural-language question into a PromQL expression.
func (c *Client) Query(ctx context.Context, question string, metrics []MetricHint, labels map[string][]string, history []ConversationTurn) (string, error) {
	return c.query(ctx, buildSystemPrompt(metrics, labels), buildUserMessage(history, question))
}

// Refine generates an alternative query when the previous one returned no data.
func (c *Client) Refine(ctx context.Context, question, failedQuery string, metrics []MetricHint, labels map[string][]string, history []ConversationTurn) (string, error) {
	system := buildSystemPrompt(metrics, labels) +
		"\n\nThe query below returned no results. Generate a corrected query using only the available label values above." +
		"\nFailed query: " + failedQuery
	return c.query(ctx, system, buildUserMessage(history, question))
}

// Format converts a raw Prometheus result into a friendly plain-text reply.
// promql is passed for context so Claude can correctly interpret the metric type.
func (c *Client) Format(ctx context.Context, question, promql, result string) (string, error) {
	system := `You are a friendly infrastructure assistant replying on Telegram.
Convert the raw Prometheus result into a short, natural response that directly answers the question.
Rules:
- Output plain text only — no markdown, no asterisks, no backticks, no underscores
- Use the PromQL to understand what metric is being reported (bytes, seconds, ratio, count, etc.)
- Bytes to human readable: 1073741824 → "1 GB", 536870912 → "512 MB", 107374182 → "~100 MB"
- Seconds to ms: 0.003 → "3ms", 0.0003 → "0.3ms"
- Filesystem bytes = disk/storage space, NOT memory
- Single "1" for an up/min_over_time(up) query → "Yes, it is up ✅" / "No downtime detected ✅"
- Single "0" for an up/min_over_time(up) query → "No, it is down ❌" / "There was downtime ❌"
- Multiple up results → list each by job name with its status
- Positive number for request rate health → "Yes, the service is running ✅"
- "0" for request rate health → "The service appears to be down ❌"
- For changes(up): 0 → "Stable, no state changes ✅", >0 → "X state changes detected ⚠️"
- For CPU percentage: result is already a ratio, multiply by 100 and add %
- Be concise (max 2 sentences), directly answer the question
- Do not mention PromQL or Prometheus`

	msg := fmt.Sprintf("Question: %s\nPromQL used: %s\nResult: %s", question, promql, result)
	return c.rawQuery(ctx, system, msg)
}

// rawQuery calls the API and returns the text as-is, without PromQL/clarification detection.
// Used for formatting responses where we expect natural language, not PromQL.
func (c *Client) rawQuery(ctx context.Context, system, userMsg string) (string, error) {
	body, _ := json.Marshal(bedrockRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        300,
		System:           system,
		Messages:         []message{{Role: "user", Content: userMsg}},
	})

	url := fmt.Sprintf("%s/model/%s/invoke", c.endpoint, c.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("claude API %d: %s", resp.StatusCode, raw)
	}

	var result bedrockResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if len(result.Content) == 0 {
		return "", fmt.Errorf("empty response")
	}
	return strings.TrimSpace(result.Content[0].Text), nil
}

func (c *Client) query(ctx context.Context, system, userMsg string) (string, error) {
	body, _ := json.Marshal(bedrockRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        300,
		System:           system,
		Messages:         []message{{Role: "user", Content: userMsg}},
	})

	url := fmt.Sprintf("%s/model/%s/invoke", c.endpoint, c.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("claude API %d: %s", resp.StatusCode, raw)
	}

	var result bedrockResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if len(result.Content) == 0 {
		return "", fmt.Errorf("empty response")
	}

	out := strings.TrimSpace(result.Content[0].Text)
	out = strings.TrimPrefix(out, "```promql")
	out = strings.TrimPrefix(out, "```")
	out = strings.TrimSuffix(out, "```")
	out = strings.TrimSpace(out)

	// Single-line: check if it's a clarification question before treating as PromQL.
	if !strings.Contains(out, "\n") {
		if isQuestion(out) {
			return "", &ClarificationError{Message: out}
		}
		return out, nil
	}

	// Multi-line: try to extract a PromQL expression.
	if promql := extractPromQL(out); promql != "" {
		return promql, nil
	}

	return "", &ClarificationError{Message: out}
}

// isQuestion returns true if the text looks like a natural language question.
func isQuestion(s string) bool {
	if strings.HasSuffix(s, "?") {
		return true
	}
	lower := strings.ToLower(s)
	starters := []string{"which", "are you", "what ", "how ", "do you", "could you", "can you", "please clarify"}
	for _, q := range starters {
		if strings.HasPrefix(lower, q) {
			return true
		}
	}
	return false
}

// ClarificationError is returned when Claude asks a question instead of returning PromQL.
type ClarificationError struct {
	Message string
}

func (e *ClarificationError) Error() string { return "clarification: " + e.Message }

func extractPromQL(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if isPromQL(line) {
			return line
		}
	}
	return ""
}

func isPromQL(s string) bool {
	if s == "" || len(s) < 2 {
		return false
	}
	first := s[0]
	if !(first >= 'a' && first <= 'z' || first == '_') {
		return false
	}
	starters := []string{"i ", "i'", "you ", "the ", "this ", "here ", "however", "since ", "please", "could ", "which ", "are ", "what ", "how "}
	lower := strings.ToLower(s)
	for _, w := range starters {
		if strings.HasPrefix(lower, w) {
			return false
		}
	}
	return true
}
