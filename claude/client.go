package claude

import (
	"bytes"
	"context"
	"encoding/base64"
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
Convert natural language questions into PromQL instant queries.

Rules:
- Respond with ONLY the PromQL expression. No explanation, no markdown, no backticks.
- Use ONLY metric names from the list below — never invent metric names.
- Prefer simple label selectors; omit label filters unless required by the question.
- For rates, use a 5m window by default.`

// MetricHint carries the name and optional help text for one metric.
type MetricHint struct {
	Name string
	Help string
	Type string
}

// buildSystemPrompt constructs a system prompt that includes the actual
// metric names scraped from Prometheus so Claude never guesses wrong names.
func buildSystemPrompt(metrics []MetricHint) string {
	if len(metrics) == 0 {
		return baseSystemPrompt
	}

	var sb strings.Builder
	sb.WriteString(baseSystemPrompt)
	sb.WriteString("\n\nAvailable metrics (name — description):\n")
	for _, m := range metrics {
		if m.Help != "" {
			sb.WriteString(fmt.Sprintf("- %s — %s\n", m.Name, m.Help))
		} else {
			sb.WriteString(fmt.Sprintf("- %s\n", m.Name))
		}
	}
	return sb.String()
}

// Query translates a natural-language question into a PromQL expression.
// Pass the metrics slice from prom.Client so Claude knows exactly which
// metric names exist; pass nil to fall back to the generic prompt.
func (c *Client) Query(ctx context.Context, question string, metrics []MetricHint) (string, error) {
	return c.query(ctx, buildSystemPrompt(metrics), question)
}

// Refine is called when a previous PromQL returned no data. It asks Claude
// to produce an alternative query given the failed attempt.
func (c *Client) Refine(ctx context.Context, question, failedQuery string, metrics []MetricHint) (string, error) {
	system := buildSystemPrompt(metrics) +
		"\n\nIMPORTANT: The query below returned no results. Generate a corrected query." +
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
	req.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString([]byte(c.token)))

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
	return strings.TrimSpace(out), nil
}
