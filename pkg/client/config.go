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
	// 0 = let the server grant based on tier. Non-zero pins a requested maximum that the server may clamp down.
	MaxConnections        int
	PingInterval          time.Duration
	PongTimeout           time.Duration
	MaxReconnectDelay     time.Duration
	ResponseChunkSize     int
	ResponseHeaderTimeout time.Duration
}

// LoadConfig reads client configuration from environment variables.
//
// Tunnel configuration:
//   PLEXTUNNEL_TOKEN              (required) Authentication token from the Portless dashboard.
//   PLEXTUNNEL_SERVER_URL         (required) WebSocket endpoint, e.g. wss://tunnel.example.com/tunnel
//   PLEXTUNNEL_PLEX_TARGET        (default "http://127.0.0.1:32400") Local Plex address to forward requests to.
//   PLEXTUNNEL_SUBDOMAIN          (optional) Fixed subdomain to request; if unset, the server assigns one.
//   PLEXTUNNEL_LOG_LEVEL          (default "info") Log verbosity: debug, info, warn, error.
//   PLEXTUNNEL_MAX_CONNECTIONS    (default server-assigned) Requested websocket pool size (1–32); server may grant fewer.
//
// Advanced / tuning:
//   PLEXTUNNEL_PING_INTERVAL          (default "30s")    WebSocket ping interval.
//   PLEXTUNNEL_PONG_TIMEOUT           (default "10s")    Time to wait for pong before treating the connection as dead.
//   PLEXTUNNEL_MAX_RECONNECT_DELAY    (default "60s")    Maximum backoff between reconnection attempts.
//   PLEXTUNNEL_RESPONSE_CHUNK_SIZE    (default "65536")  Response body chunk size in bytes (1024–4194304).
//   PLEXTUNNEL_RESPONSE_HEADER_TIMEOUT (default "30s")   Timeout waiting for Plex response headers.
//
// Debug:
//   PLEXTUNNEL_DEBUG_BANDWIDTH_LOGGING (default "false") Emit per-chunk timing logs; requires log level debug.
func LoadConfig() (Config, error) {
	cfg := Config{
		Token:                 strings.TrimSpace(os.Getenv("PLEXTUNNEL_TOKEN")),
		ServerURL:             strings.TrimSpace(os.Getenv("PLEXTUNNEL_SERVER_URL")),
		PlexTarget:            getenvDefault("PLEXTUNNEL_PLEX_TARGET", "http://127.0.0.1:32400"),
		Subdomain:             strings.TrimSpace(os.Getenv("PLEXTUNNEL_SUBDOMAIN")),
		LogLevel:              getenvDefault("PLEXTUNNEL_LOG_LEVEL", "info"),
		DebugBandwidthLog:     false,
		MaxConnections:        0,
		PingInterval:          30 * time.Second,
		PongTimeout:           10 * time.Second,
		MaxReconnectDelay:     60 * time.Second,
		ResponseChunkSize:     64 * 1024,
		ResponseHeaderTimeout: 30 * time.Second,
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

	if timeoutValue := strings.TrimSpace(os.Getenv("PLEXTUNNEL_RESPONSE_HEADER_TIMEOUT")); timeoutValue != "" {
		timeout, err := time.ParseDuration(timeoutValue)
		if err != nil {
			return Config{}, fmt.Errorf("invalid PLEXTUNNEL_RESPONSE_HEADER_TIMEOUT: %w", err)
		}
		cfg.ResponseHeaderTimeout = timeout
	}

	return cfg, nil
}

func getenvDefault(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
