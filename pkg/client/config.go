package client

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Token             string
	ServerURL         string
	PlexTarget        string
	Subdomain         string
	LogLevel          string
	DebugBandwidthLog bool
	PingInterval      time.Duration
	PongTimeout       time.Duration
	MaxReconnectDelay time.Duration
	ResponseChunkSize int
}

func LoadConfig() (Config, error) {
	cfg := Config{
		Token:             strings.TrimSpace(os.Getenv("PLEXTUNNEL_TOKEN")),
		ServerURL:         strings.TrimSpace(os.Getenv("PLEXTUNNEL_SERVER_URL")),
		PlexTarget:        getenvDefault("PLEXTUNNEL_PLEX_TARGET", "http://127.0.0.1:32400"),
		Subdomain:         strings.TrimSpace(os.Getenv("PLEXTUNNEL_SUBDOMAIN")),
		LogLevel:          getenvDefault("PLEXTUNNEL_LOG_LEVEL", "info"),
		DebugBandwidthLog: false,
		PingInterval:      30 * time.Second,
		PongTimeout:       10 * time.Second,
		MaxReconnectDelay: 60 * time.Second,
		ResponseChunkSize: 64 * 1024,
	}

	if cfg.Token == "" {
		return Config{}, fmt.Errorf("PLEXTUNNEL_TOKEN is required")
	}
	if cfg.ServerURL == "" {
		return Config{}, fmt.Errorf("PLEXTUNNEL_SERVER_URL is required")
	}

	if raw := strings.TrimSpace(os.Getenv("PLEXTUNNEL_DEBUG_BANDWIDTH_LOGGING")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid PLEXTUNNEL_DEBUG_BANDWIDTH_LOGGING: %w", err)
		}
		cfg.DebugBandwidthLog = parsed
	}

	if pingValue := strings.TrimSpace(os.Getenv("PLEXTUNNEL_PING_INTERVAL")); pingValue != "" {
		ping, err := time.ParseDuration(pingValue)
		if err != nil {
			return Config{}, fmt.Errorf("invalid PLEXTUNNEL_PING_INTERVAL: %w", err)
		}
		cfg.PingInterval = ping
	}

	if timeoutValue := strings.TrimSpace(os.Getenv("PLEXTUNNEL_PONG_TIMEOUT")); timeoutValue != "" {
		timeout, err := time.ParseDuration(timeoutValue)
		if err != nil {
			return Config{}, fmt.Errorf("invalid PLEXTUNNEL_PONG_TIMEOUT: %w", err)
		}
		cfg.PongTimeout = timeout
	}

	if backoffValue := strings.TrimSpace(os.Getenv("PLEXTUNNEL_MAX_RECONNECT_DELAY")); backoffValue != "" {
		delay, err := time.ParseDuration(backoffValue)
		if err != nil {
			return Config{}, fmt.Errorf("invalid PLEXTUNNEL_MAX_RECONNECT_DELAY: %w", err)
		}
		cfg.MaxReconnectDelay = delay
	}

	if chunkValue := strings.TrimSpace(os.Getenv("PLEXTUNNEL_RESPONSE_CHUNK_SIZE")); chunkValue != "" {
		chunkSize, err := strconv.Atoi(chunkValue)
		if err != nil {
			return Config{}, fmt.Errorf("invalid PLEXTUNNEL_RESPONSE_CHUNK_SIZE: %w", err)
		}
		if chunkSize < 1024 {
			return Config{}, fmt.Errorf("PLEXTUNNEL_RESPONSE_CHUNK_SIZE must be >= 1024")
		}
		cfg.ResponseChunkSize = chunkSize
	}

	return cfg, nil
}

func getenvDefault(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
