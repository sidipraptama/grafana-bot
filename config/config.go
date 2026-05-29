package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

type Config struct {
	ClaudeEndpoint string
	ClaudeModel    string
	ClaudeToken    string
	PrometheusURL  string
	AllowedNumbers map[string]bool
}

// secretPayload mirrors the JSON stored in AWS Secrets Manager.
type secretPayload struct {
	ClaudeEndpoint string `json:"CLAUDE_ENDPOINT"`
	ClaudeModel    string `json:"CLAUDE_MODEL"`
	ClaudeToken    string `json:"CLAUDE_TOKEN"`
	PrometheusURL  string `json:"PROMETHEUS_URL"`
	AllowedNumbers string `json:"ALLOWED_NUMBERS"`
}

// Load fetches config from AWS Secrets Manager.
// The secret name is read from the SECRET_NAME env var (default: "whatsapp-bot").
// The secret must be a JSON object with the keys defined in secretPayload.
func Load(ctx context.Context) (*Config, error) {
	secretName := os.Getenv("SECRET_NAME")
	if secretName == "" {
		secretName = "whatsapp-bot"
	}

	awscfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	svc := secretsmanager.NewFromConfig(awscfg)
	out, err := svc.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: &secretName,
	})
	if err != nil {
		return nil, fmt.Errorf("get secret %q: %w", secretName, err)
	}

	var p secretPayload
	if err := json.Unmarshal([]byte(*out.SecretString), &p); err != nil {
		return nil, fmt.Errorf("parse secret JSON: %w", err)
	}

	if p.ClaudeEndpoint == "" {
		p.ClaudeEndpoint = "https://bedrock-runtime.us-west-2.amazonaws.com"
	}
	if p.ClaudeModel == "" {
		p.ClaudeModel = "global.anthropic.claude-sonnet-4-5-20250929-v1:0"
	}
	if p.PrometheusURL == "" {
		p.PrometheusURL = "http://localhost:9090"
	}

	return &Config{
		ClaudeEndpoint: strings.TrimSpace(p.ClaudeEndpoint),
		ClaudeModel:    strings.TrimSpace(p.ClaudeModel),
		ClaudeToken:    strings.TrimSpace(p.ClaudeToken),
		PrometheusURL:  strings.TrimSpace(p.PrometheusURL),
		AllowedNumbers: parseNumbers(p.AllowedNumbers),
	}, nil
}

func parseNumbers(raw string) map[string]bool {
	set := make(map[string]bool)
	for _, n := range strings.Split(raw, ",") {
		n = strings.TrimSpace(n)
		if n != "" {
			set[n] = true
		}
	}
	return set
}
