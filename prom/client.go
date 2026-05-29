package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: 10 * time.Second}}
}

type queryResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]interface{}    `json:"value"`
		} `json:"result"`
	} `json:"data"`
	Error string `json:"error"`
}

func (c *Client) Query(ctx context.Context, query string) (string, error) {
	params := url.Values{}
	params.Set("query", query)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/query?"+params.Encode(), nil)
	if err != nil {
		return "", err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var result queryResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if result.Status != "success" {
		return "", fmt.Errorf("prometheus: %s", result.Error)
	}
	if len(result.Data.Result) == 0 {
		return "No data found.", nil
	}

	var sb strings.Builder
	for i, r := range result.Data.Result {
		val := r.Value[1].(string)
		if len(result.Data.Result) > 1 {
			// Multi-result: show instance label for context
			instance := r.Metric["instance"]
			if instance == "" {
				instance = r.Metric["job"]
			}
			sb.WriteString(fmt.Sprintf("• %s: %s\n", instance, val))
		} else {
			sb.WriteString(val)
		}
		if i >= 4 {
			sb.WriteString(fmt.Sprintf("... and %d more", len(result.Data.Result)-i-1))
			break
		}
	}
	return strings.TrimSpace(sb.String()), nil
}
