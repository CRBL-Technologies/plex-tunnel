package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/CRBL-Technologies/plex-tunnel/pkg/client"
)

func newTestUIController() *clientController {
	return newClientController(context.Background(), client.Config{
		Token:          "token-1234",
		ServerURL:      "wss://example.test/tunnel",
		PlexTarget:     "http://127.0.0.1:32400",
		LogLevel:       "info",
		MaxConnections: 4,
	}, zerolog.Nop())
}

func newTestSessionStore() *sessionStore {
	return newSessionStore(7 * 24 * time.Hour)
}

func addAuthenticatedSession(t *testing.T, req *http.Request, store *sessionStore) {
	t.Helper()

	token, err := store.Create()
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	req.AddCookie(&http.Cookie{
		Name:  sessionCookieName,
		Value: token,
	})
}

func TestUIHandler_IndexPage(t *testing.T) {
	store := newTestSessionStore()
	handler := newUIHandler(
		newTestUIController(),
		zerolog.Nop(),
		authConfig{Password: "secret"},
		store,
		newLoginRateLimiter(),
		"127.0.0.1:9090",
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	addAuthenticatedSession(t, req, store)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "Portless Client") {
		t.Fatalf("body missing expected title: %q", rec.Body.String())
	}
}

func TestUIHandler_StatusAPI(t *testing.T) {
	store := newTestSessionStore()
	handler := newUIHandler(
		newTestUIController(),
		zerolog.Nop(),
		authConfig{Password: "secret"},
		store,
		newLoginRateLimiter(),
		"127.0.0.1:9090",
	)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	addAuthenticatedSession(t, req, store)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to parse JSON response: %v", err)
	}
	if _, ok := payload["status"]; !ok {
		t.Fatalf("missing status key in response: %s", rec.Body.String())
	}
	if _, ok := payload["config"]; !ok {
		t.Fatalf("missing config key in response: %s", rec.Body.String())
	}
}

func TestUIHandler_SettingsCSRFRejectsNoOrigin(t *testing.T) {
	store := newTestSessionStore()
	handler := newUIHandler(
		newTestUIController(),
		zerolog.Nop(),
		authConfig{Password: "secret"},
		store,
		newLoginRateLimiter(),
		"127.0.0.1:9090",
	)

	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader("server_url=wss://example.test/tunnel"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	addAuthenticatedSession(t, req, store)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestMaskToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{name: "empty", token: "", want: "****"},
		{name: "short", token: "abcd", want: "****"},
		{name: "trimmed short", token: " ab ", want: "****"},
		{name: "long", token: "token-1234", want: "****1234"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskToken(tt.token); got != tt.want {
				t.Fatalf("maskToken(%q) = %q, want %q", tt.token, got, tt.want)
			}
		})
	}
}
