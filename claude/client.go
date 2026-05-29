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
6. When the user mentions "private instance" use job="node-exporter-private", "public instance" use job="node-exporter-public".`

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
