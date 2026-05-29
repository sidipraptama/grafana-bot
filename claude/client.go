package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

const systemPrompt = `You are a Prometheus metrics assistant for a URL shortener service running on AWS EC2.
Convert natural language questions into PromQL queries.

Available metrics:
- CPU usage %: 100 - (avg(rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)
- Memory usage %: 100 * (1 - (node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes))
- Disk usage %: 100 * (1 - (node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"}))
- Disk I/O read: rate(node_disk_read_bytes_total[5m])
- Disk I/O write: rate(node_disk_written_bytes_total[5m])
- Network in: rate(node_network_receive_bytes_total{device!="lo"}[5m])
- Network out: rate(node_network_transmit_bytes_total{device!="lo"}[5m])
- HTTP request rate: rate(http_server_request_count_total[5m])
- HTTP error rate: rate(http_server_request_count_total{http_response_status_code=~"5.."}[5m])
- Active goroutines: go_goroutines
- Service up: up

Respond with ONLY the PromQL query. No explanation, no markdown, no backticks.`

func (c *Client) Query(ctx context.Context, question string) (string, error) {
	body, _ := json.Marshal(bedrockRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        256,
		System:           systemPrompt,
		Messages:         []message{{Role: "user", Content: question}},
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
	return result.Content[0].Text, nil
}
