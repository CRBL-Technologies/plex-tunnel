package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"nhooyr.io/websocket"

	"github.com/CRBL-Technologies/plex-tunnel-proto/tunnel"
)

func TestRunSessionHandshakeSendsProtocolVersion(t *testing.T) {
	serverErrCh := make(chan error, 1)
	registerCh := make(chan tunnel.Message, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := tunnel.AcceptWebSocket(w, r)
		if err != nil {
			serverErrCh <- err
			return
		}
		defer conn.Close()

		registerMsg, err := conn.Receive()
		if err != nil {
			serverErrCh <- err
			return
		}
		registerCh <- registerMsg

		if err := conn.Send(tunnel.Message{
			Type:            tunnel.MsgRegisterAck,
			Subdomain:       "myplex",
			ProtocolVersion: tunnel.ProtocolVersion,
		}); err != nil {
			serverErrCh <- err
			return
		}

		<-r.Context().Done()
	}))
	defer srv.Close()

	cfg := Config{
		Token:             "token-123",
		ServerURL:         toWebSocketURL(srv.URL),
		PlexTarget:        "http://127.0.0.1:32400",
		PingInterval:      time.Hour,
		PongTimeout:       time.Hour,
		MaxReconnectDelay: time.Second,
		ResponseChunkSize: 1024,
	}

	c := New(cfg, zerolog.Nop())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	if err := c.runSession(ctx); err != nil {
		t.Fatalf("runSession() error = %v", err)
	}

	select {
	case registerMsg := <-registerCh:
		if registerMsg.Type != tunnel.MsgRegister {
			t.Fatalf("register message type = %v, want %v", registerMsg.Type, tunnel.MsgRegister)
		}
		if registerMsg.ProtocolVersion != tunnel.ProtocolVersion {
			t.Fatalf("register protocol_version = %d, want %d", registerMsg.ProtocolVersion, tunnel.ProtocolVersion)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for register message")
	}

	select {
	case err := <-serverErrCh:
		t.Fatalf("server error: %v", err)
	default:
	}
}

func TestRunSessionProtocolVersionMismatchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := tunnel.AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		defer conn.Close()

		_, _ = conn.Receive()
		_ = conn.Send(tunnel.Message{
			Type:  tunnel.MsgError,
			Error: "unsupported tunnel protocol version",
		})
	}))
	defer srv.Close()

	cfg := Config{
		Token:             "token-123",
		ServerURL:         toWebSocketURL(srv.URL),
		PlexTarget:        "http://127.0.0.1:32400",
		PingInterval:      time.Hour,
		PongTimeout:       time.Hour,
		MaxReconnectDelay: time.Second,
		ResponseChunkSize: 1024,
	}

	c := New(cfg, zerolog.Nop())
	err := c.runSession(context.Background())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Server requires a different protocol version. Update your client or server.") {
		t.Fatalf("expected protocol mismatch guidance, got %v", err)
	}
}

func TestRunSessionOldServerHandshakeHint(t *testing.T) {
	serverErrCh := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			serverErrCh <- err
			return
		}
		defer wsConn.Close(websocket.StatusNormalClosure, "")

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := wsConn.Write(ctx, websocket.MessageText, []byte(`{"type":2,"subdomain":"legacy"}`)); err != nil {
			serverErrCh <- err
		}
	}))
	defer srv.Close()

	cfg := Config{
		Token:             "token-123",
		ServerURL:         toWebSocketURL(srv.URL),
		PlexTarget:        "http://127.0.0.1:32400",
		PingInterval:      time.Hour,
		PongTimeout:       time.Hour,
		MaxReconnectDelay: time.Second,
		ResponseChunkSize: 1024,
	}

	c := New(cfg, zerolog.Nop())
	err := c.runSession(context.Background())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Connection failed during handshake. The server may be running an older protocol version. Ensure both client and server are updated.") {
		t.Fatalf("expected old server guidance, got %v", err)
	}

	select {
	case err := <-serverErrCh:
		t.Fatalf("server error: %v", err)
	default:
	}
}

func toWebSocketURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}
