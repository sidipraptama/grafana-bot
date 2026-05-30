package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

type Config struct {
	TelegramToken  string
	ClaudeEndpoint string
	ClaudeModel    string
	ClaudeToken    string
	PrometheusURL  string
	AllowedUsers   map[int64]bool
}

type secretPayload struct {
	TelegramToken  string `json:"TELEGRAM_BOT_TOKEN"`
	ClaudeEndpoint string `json:"CLAUDE_ENDPOINT"`
	ClaudeModel    string `json:"CLAUDE_MODEL"`
	ClaudeToken    string `json:"CLAUDE_TOKEN"`
	PrometheusURL  string `json:"PROMETHEUS_URL"`
	AllowedUsers   string `json:"ALLOWED_USERS"`
}

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
		p.ClaudeEndpoint = "https://bedrock-runtime.ap-southeast-3.amazonaws.com"
	}
	if p.ClaudeModel == "" {
		p.ClaudeModel = "global.anthropic.claude-haiku-4-5-20251001-v1:0"
	}
	if p.PrometheusURL == "" {
		p.PrometheusURL = "http://localhost:9090"
	}

	return &Config{
		TelegramToken:  strings.TrimSpace(p.TelegramToken),
		ClaudeEndpoint: strings.TrimSpace(p.ClaudeEndpoint),
		ClaudeModel:    strings.TrimSpace(p.ClaudeModel),
		ClaudeToken:    strings.TrimSpace(p.ClaudeToken),
		PrometheusURL:  strings.TrimSpace(p.PrometheusURL),
		AllowedUsers:   parseUserIDs(p.AllowedUsers),
	}, nil
}

func parseUserIDs(raw string) map[int64]bool {
	set := make(map[int64]bool)
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			set[id] = true
		}
	}
	return set
}
