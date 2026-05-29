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

const baseSystemPrompt = `You are a Prometheus metrics assistant.
Convert natural language questions into a single PromQL instant query.

STRICT RULES — violating any rule makes the answer useless:
1. Respond with ONLY the PromQL expression. No words, no explanation, no markdown, no backticks, no options.
2. Use ONLY metric names from the Available Metrics list — never invent names.
3. Use ONLY label values from the Available Label Values list — never invent label values.
4. If the question is ambiguous, make your best guess. NEVER ask for clarification.
5. For rates/counters use a 5m window. For histograms use histogram_quantile().
6. Use the MINIMUM label selectors needed. Only add instance= or env= filters when the user explicitly mentions a specific instance or environment. Never add extra labels that weren't asked for.

Job context — use this to map user intent to the correct job label:
- "private instance" / "private server" / "private EC2"  → node-exporter-private  (OS/system metrics for the private EC2)
- "public instance"  / "public server"  / "public EC2"   → node-exporter-public   (OS/system metrics for the public EC2)
- "app" / "service" / "HTTP" / "request" / "latency" / "p50" / "p95" / "p99" / "response time" → job="url-shortener" (application metrics: use http_server_request_duration_seconds_bucket for latency)
- "postgres" / "database" / "db"         → job="postgresql"
- "redis" / "cache"                      → job="redis"
- "rabbitmq" / "queue" / "message queue" → job="rabbitmq"
- "prometheus" / "monitoring"            → job="prometheus"

For latency percentiles (p50/p95/p99) ALWAYS use this exact pattern (sum by le is mandatory):
histogram_quantile(0.NN, sum(rate(http_server_request_duration_seconds_bucket{job="url-shortener"}[5m])) by (le))

For historical / time-range questions, use these patterns:
- "any downtime in last Xd?" → min_over_time(up{job="..."}[Xd])  — returns 0 if ever down, 1 if always up
- "how many times down in last Xd?" → changes(up{job="..."}[Xd])
- "average CPU over last Xd?" → avg_over_time(node_cpu_seconds_total{...}[Xd])
- "average latency over last Xd?" → avg_over_time(http_server_request_duration_seconds_bucket{...}[Xd])
- "this week" or "last 7 days" → use [7d], "today" or "last 24h" → use [24h], "this month" → use [30d]
- NEVER use a plain instant metric like up{...} for questions about a time range — always use an _over_time function

Label rules for url-shortener app metrics (http_server_request_duration_seconds_*):
- Valid filter labels: job, http_route, http_request_method, http_response_status_code, env, host
- NEVER use instance= on these metrics — it does not exist
- "public" or "private" does NOT apply to app metrics — there is only one url-shortener service
- To exclude health checks: http_route!="/health"
- To filter errors: http_response_status_code=~"5.."
- For overall latency (no specific route): use only job="url-shortener", no other filters`

// MetricHint carries the name and optional help text for one metric.
type MetricHint struct {
	Name string
	Help string
	Type string
}

// buildSystemPrompt constructs a system prompt with real metric names and label values.
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

// Query translates a natural-language question into a PromQL expression.
func (c *Client) Query(ctx context.Context, question string, metrics []MetricHint, labels map[string][]string) (string, error) {
	return c.query(ctx, buildSystemPrompt(metrics, labels), question)
}

// Refine is called when a previous PromQL returned no data. It asks Claude
// to produce an alternative query given the failed attempt.
func (c *Client) Refine(ctx context.Context, question, failedQuery string, metrics []MetricHint, labels map[string][]string) (string, error) {
	system := buildSystemPrompt(metrics, labels) +
		"\n\nThe query below returned no results. Generate a corrected query using only the available label values above." +
		"\nFailed query: " + failedQuery
	return c.query(ctx, system, question)
}

// Format converts a raw Prometheus result into a friendly conversational reply.
func (c *Client) Format(ctx context.Context, question, result string) (string, error) {
	system := `You are a friendly infrastructure assistant replying on WhatsApp.
Convert the raw Prometheus result into one short, natural sentence.
Rules:
- Seconds to ms: 0.003 → "3ms", 0.0003 → "0.3ms"
- "1" for an up/status query → "Yes, it is up ✅"
- "0" for an up/status query → "No, it is down ❌"
- "1" for a min_over_time(up) query → "No downtime detected, it was up the entire period ✅"
- "0" for a min_over_time(up) query → "There was downtime during the period ❌"
- For changes(up) result: 0 → "No state changes, stable the entire period ✅", >0 → "X state changes detected"
- For percentages multiply by 100 and add %
- Be concise, max 2 sentences
- Do not mention PromQL or Prometheus`

	msg := fmt.Sprintf("Question: %s\nResult: %s", question, result)
	return c.query(ctx, system, msg)
}

func (c *Client) query(ctx context.Context, system, userMsg string) (string, error) {
	body, _ := json.Marshal(bedrockRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        256,
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

	// Strip accidental markdown fences Claude might still emit
	out := strings.TrimSpace(result.Content[0].Text)
	out = strings.TrimPrefix(out, "```promql")
	out = strings.TrimPrefix(out, "```")
	out = strings.TrimSuffix(out, "```")
	out = strings.TrimSpace(out)

	// If Claude returned a single-line expression, we're done.
	// If it returned verbose text, try to extract the last PromQL-looking line.
	// If nothing looks like PromQL, return the text as a ClarificationError so
	// the caller can forward it to the user.
	if !strings.Contains(out, "\n") {
		return out, nil
	}

	if promql := extractPromQL(out); promql != "" {
		return promql, nil
	}

	return "", &ClarificationError{Message: out}
}

// ClarificationError is returned when Claude responds with a question or
// explanation instead of a PromQL expression.
type ClarificationError struct {
	Message string
}

func (e *ClarificationError) Error() string { return "clarification: " + e.Message }

// extractPromQL scans lines bottom-up for the first PromQL-like expression.
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

// isPromQL returns true if the line looks like a PromQL expression rather
// than natural language — starts with a lowercase letter or underscore and
// does not begin with common English sentence starters.
func isPromQL(s string) bool {
	if s == "" || len(s) < 2 {
		return false
	}
	first := s[0]
	if !(first >= 'a' && first <= 'z' || first == '_') {
		return false
	}
	// Reject lines that look like English sentences
	starters := []string{"i ", "i'", "you ", "the ", "this ", "here ", "however", "since ", "please", "could "}
	lower := strings.ToLower(s)
	for _, w := range starters {
		if strings.HasPrefix(lower, w) {
			return false
		}
	}
	return true
}
