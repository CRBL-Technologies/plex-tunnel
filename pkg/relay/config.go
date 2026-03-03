package relay

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	Listen            string
	TunnelListen      string
	Domain            string
	TokensFile        string
	LogLevel          string
	RequestTimeout    time.Duration
	AgentStaleTimeout time.Duration
	CleanupInterval   time.Duration
}

func LoadConfig() (Config, error) {
	cfg := Config{
		Listen:            getenvDefault("PLEXTUNNEL_RELAY_LISTEN", ":8080"),
		TunnelListen:      getenvDefault("PLEXTUNNEL_RELAY_TUNNEL_LISTEN", ":8081"),
		Domain:            strings.ToLower(strings.TrimSpace(os.Getenv("PLEXTUNNEL_RELAY_DOMAIN"))),
		TokensFile:        strings.TrimSpace(os.Getenv("PLEXTUNNEL_RELAY_TOKENS_FILE")),
		LogLevel:          getenvDefault("PLEXTUNNEL_RELAY_LOG_LEVEL", "info"),
		RequestTimeout:    2 * time.Minute,
		AgentStaleTimeout: 60 * time.Second,
		CleanupInterval:   15 * time.Second,
	}

	if cfg.Domain == "" {
		return Config{}, fmt.Errorf("PLEXTUNNEL_RELAY_DOMAIN is required")
	}
	if cfg.TokensFile == "" {
		return Config{}, fmt.Errorf("PLEXTUNNEL_RELAY_TOKENS_FILE is required")
	}

	if timeout := strings.TrimSpace(os.Getenv("PLEXTUNNEL_RELAY_REQUEST_TIMEOUT")); timeout != "" {
		parsed, err := time.ParseDuration(timeout)
		if err != nil {
			return Config{}, fmt.Errorf("invalid PLEXTUNNEL_RELAY_REQUEST_TIMEOUT: %w", err)
		}
		cfg.RequestTimeout = parsed
	}

	if stale := strings.TrimSpace(os.Getenv("PLEXTUNNEL_RELAY_AGENT_STALE_TIMEOUT")); stale != "" {
		parsed, err := time.ParseDuration(stale)
		if err != nil {
			return Config{}, fmt.Errorf("invalid PLEXTUNNEL_RELAY_AGENT_STALE_TIMEOUT: %w", err)
		}
		cfg.AgentStaleTimeout = parsed
	}

	if cleanup := strings.TrimSpace(os.Getenv("PLEXTUNNEL_RELAY_CLEANUP_INTERVAL")); cleanup != "" {
		parsed, err := time.ParseDuration(cleanup)
		if err != nil {
			return Config{}, fmt.Errorf("invalid PLEXTUNNEL_RELAY_CLEANUP_INTERVAL: %w", err)
		}
		cfg.CleanupInterval = parsed
	}

	return cfg, nil
}

func getenvDefault(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
