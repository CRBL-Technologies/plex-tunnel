package client

import (
	"context"
	"crypto/tls"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestMain(m *testing.M) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.InsecureSkipVerify = true
	http.DefaultTransport = transport

	os.Exit(m.Run())
}

func TestResolveTargetURL(t *testing.T) {
	baseTarget := "http://127.0.0.1:32400"

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr string
	}{
		{
			name: "normal path",
			path: "/library/metadata/123",
			want: "http://127.0.0.1:32400/library/metadata/123",
		},
		{
			name: "path with query",
			path: "/library?X-Plex-Token=abc",
			want: "http://127.0.0.1:32400/library?X-Plex-Token=abc",
		},
		{
			name:    "ssrf host override",
			path:    "//evil.com/steal",
			wantErr: "blocked: path must be a relative path",
		},
		{
			name:    "ssrf absolute url",
			path:    "http://evil.com/",
			wantErr: "blocked: path must be a relative path",
		},
		{
			name: "empty path defaults to root",
			path: "",
			want: "http://127.0.0.1:32400/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTargetURL(baseTarget, tt.path)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q, got nil", tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("error = %q, want %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveTargetURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunSession_RejectsPlaintextWebSocket(t *testing.T) {
	c := New(Config{
		ServerURL: "ws://example.test/tunnel",
	}, zerolog.Nop())

	err := c.runSession(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to connect over unencrypted ws://") {
		t.Fatalf("error = %q, want substring %q", err.Error(), "refusing to connect over unencrypted ws://")
	}
}
