package client

import "testing"

func TestLoadConfigDebugBandwidthLog(t *testing.T) {
	t.Setenv("PLEXTUNNEL_TOKEN", "token")
	t.Setenv("PLEXTUNNEL_SERVER_URL", "wss://example.test/tunnel")
	t.Setenv("PLEXTUNNEL_DEBUG_BANDWIDTH_LOGGING", "true")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.DebugBandwidthLog {
		t.Fatal("expected DebugBandwidthLog true")
	}
}

func TestLoadConfigInvalidDebugBandwidthLog(t *testing.T) {
	t.Setenv("PLEXTUNNEL_TOKEN", "token")
	t.Setenv("PLEXTUNNEL_SERVER_URL", "wss://example.test/tunnel")
	t.Setenv("PLEXTUNNEL_DEBUG_BANDWIDTH_LOGGING", "definitely")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for invalid debug bandwidth logging env")
	}
}

func TestLoadConfigMaxConnections(t *testing.T) {
	t.Setenv("PLEXTUNNEL_TOKEN", "token")
	t.Setenv("PLEXTUNNEL_SERVER_URL", "wss://example.test/tunnel")
	t.Setenv("PLEXTUNNEL_MAX_CONNECTIONS", "6")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxConnections != 6 {
		t.Fatalf("MaxConnections = %d, want 6", cfg.MaxConnections)
	}
}

func TestLoadConfigInvalidMaxConnections(t *testing.T) {
	t.Setenv("PLEXTUNNEL_TOKEN", "token")
	t.Setenv("PLEXTUNNEL_SERVER_URL", "wss://example.test/tunnel")
	t.Setenv("PLEXTUNNEL_MAX_CONNECTIONS", "0")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for invalid max connections env")
	}
}
