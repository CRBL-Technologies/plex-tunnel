package client

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var subdomainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func isValidSubdomain(s string) bool {
	return len(s) <= 63 && subdomainPattern.MatchString(s)
}

type Config struct {
	Token             string
	ServerURL         string
	PlexTarget        string
	Subdomain         string
	LogLevel          string
	DebugBandwidthLog bool
	MaxConnections    int
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
		MaxConnections:    4,
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
	if parsed, err := url.Parse(cfg.ServerURL); err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") {
		return Config{}, fmt.Errorf("PLEXTUNNEL_SERVER_URL must be a ws:// or wss:// URL")
	}
	if cfg.Subdomain != "" {
		if !isValidSubdomain(cfg.Subdomain) {
			return Config{}, fmt.Errorf("PLEXTUNNEL_SUBDOMAIN contains invalid characters (use lowercase letters, numbers, and hyphens)")
		}
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

	if maxConnectionsValue := strings.TrimSpace(os.Getenv("PLEXTUNNEL_MAX_CONNECTIONS")); maxConnectionsValue != "" {
		maxConnections, err := strconv.Atoi(maxConnectionsValue)
		if err != nil {
			return Config{}, fmt.Errorf("invalid PLEXTUNNEL_MAX_CONNECTIONS: %w", err)
		}
		if maxConnections < 1 || maxConnections > 32 {
			return Config{}, fmt.Errorf("PLEXTUNNEL_MAX_CONNECTIONS must be between 1 and 32")
		}
		cfg.MaxConnections = maxConnections
	}

	if chunkValue := strings.TrimSpace(os.Getenv("PLEXTUNNEL_RESPONSE_CHUNK_SIZE")); chunkValue != "" {
		chunkSize, err := strconv.Atoi(chunkValue)
		if err != nil {
			return Config{}, fmt.Errorf("invalid PLEXTUNNEL_RESPONSE_CHUNK_SIZE: %w", err)
		}
		if chunkSize < 1024 || chunkSize > 4*1024*1024 {
			return Config{}, fmt.Errorf("PLEXTUNNEL_RESPONSE_CHUNK_SIZE must be between 1024 and 4194304")
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
