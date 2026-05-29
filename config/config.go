package config

import "os"

type Config struct {
	ClaudeEndpoint string
	ClaudeModel    string
	ClaudeToken    string
	PrometheusURL  string
}

func Load() *Config {
	return &Config{
		ClaudeEndpoint: getEnv("CLAUDE_ENDPOINT", "https://bedrock-runtime.us-west-2.amazonaws.com"),
		ClaudeModel:    getEnv("CLAUDE_MODEL", "global.anthropic.claude-sonnet-4-5-20250929-v1:0"),
		ClaudeToken:    os.Getenv("CLAUDE_TOKEN"),
		PrometheusURL:  getEnv("PROMETHEUS_URL", "http://localhost:9090"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
