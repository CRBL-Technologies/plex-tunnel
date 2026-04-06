package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			SessionID:       "sess-1",
			MaxConnections:  1,
		}); err != nil {
			serverErrCh <- err
			return
		}

		<-r.Context().Done()
	}))
	defer srv.Close()
	withPinnedTLS(t, srv)

	cfg := Config{
		Token:             "token-123",
		ServerURL:         toWebSocketURL(srv.URL),
		PlexTarget:        "http://127.0.0.1:32400",
		MaxConnections:    1,
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
		if registerMsg.MaxConnections != 1 {
			t.Fatalf("register max_connections = %d, want 1", registerMsg.MaxConnections)
		}
		if registerMsg.SessionID != "" {
			t.Fatalf("register session_id = %q, want empty", registerMsg.SessionID)
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
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	withPinnedTLS(t, srv)

	cfg := Config{
		Token:             "token-123",
		ServerURL:         toWebSocketURL(srv.URL),
		PlexTarget:        "http://127.0.0.1:32400",
		MaxConnections:    1,
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
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	withPinnedTLS(t, srv)

	cfg := Config{
		Token:             "token-123",
		ServerURL:         toWebSocketURL(srv.URL),
		PlexTarget:        "http://127.0.0.1:32400",
		MaxConnections:    1,
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

func TestRunSessionRequiresV2SessionMetadata(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := tunnel.AcceptWebSocket(w, r)
		if err != nil {
			return
		}
		defer conn.Close()

		_, _ = conn.Receive()
		_ = conn.Send(tunnel.Message{
			Type:            tunnel.MsgRegisterAck,
			Subdomain:       "myplex",
			ProtocolVersion: tunnel.ProtocolVersion,
		})
	}))
	defer srv.Close()
	withPinnedTLS(t, srv)

	cfg := Config{
		Token:             "token-123",
		ServerURL:         toWebSocketURL(srv.URL),
		PlexTarget:        "http://127.0.0.1:32400",
		MaxConnections:    1,
		PingInterval:      time.Hour,
		PongTimeout:       time.Hour,
		MaxReconnectDelay: time.Second,
		ResponseChunkSize: 1024,
	}

	c := New(cfg, zerolog.Nop())
	err := c.runSession(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "server returned invalid register ack") {
		t.Fatalf("expected invalid register ack error, got %v", err)
	}
}

func TestRunSessionExpandsConnectionPool(t *testing.T) {
	serverErrCh := make(chan error, 8)
	registerCh := make(chan tunnel.Message, 4)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			SessionID:       "sess-1",
			MaxConnections:  3,
		}); err != nil {
			serverErrCh <- err
			return
		}

		<-r.Context().Done()
	}))
	defer srv.Close()
	withPinnedTLS(t, srv)

	cfg := Config{
		Token:             "token-123",
		ServerURL:         toWebSocketURL(srv.URL),
		PlexTarget:        "http://127.0.0.1:32400",
		MaxConnections:    3,
		PingInterval:      time.Hour,
		PongTimeout:       time.Hour,
		MaxReconnectDelay: time.Second,
		ResponseChunkSize: 1024,
	}

	c := New(cfg, zerolog.Nop())
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	if err := c.runSession(ctx); err != nil {
		t.Fatalf("runSession() error = %v", err)
	}

	received := make([]tunnel.Message, 0, 3)
	for len(received) < 3 {
		select {
		case registerMsg := <-registerCh:
			received = append(received, registerMsg)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for pooled register messages, got %d", len(received))
		}
	}

	var newSessionCount int
	var joinSessionCount int
	for _, registerMsg := range received {
		if registerMsg.ProtocolVersion != tunnel.ProtocolVersion {
			t.Fatalf("register protocol_version = %d, want %d", registerMsg.ProtocolVersion, tunnel.ProtocolVersion)
		}
		if registerMsg.SessionID == "" {
			if registerMsg.MaxConnections != 3 {
				t.Fatalf("new session register max_connections = %d, want 3", registerMsg.MaxConnections)
			}
			newSessionCount++
			continue
		}
		if registerMsg.SessionID != "sess-1" {
			t.Fatalf("join session_id = %q, want sess-1", registerMsg.SessionID)
		}
		if registerMsg.MaxConnections != 0 {
			t.Fatalf("join register max_connections = %d, want 0", registerMsg.MaxConnections)
		}
		joinSessionCount++
	}

	if newSessionCount != 1 {
		t.Fatalf("new session register count = %d, want 1", newSessionCount)
	}
	if joinSessionCount != 2 {
		t.Fatalf("join session register count = %d, want 2", joinSessionCount)
	}

	select {
	case err := <-serverErrCh:
		t.Fatalf("server error: %v", err)
	default:
	}
}

func TestRunSessionHandlesMaxConnectionsUpdate(t *testing.T) {
	serverErrCh := make(chan error, 8)
	registerCh := make(chan tunnel.Message, 4)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		if registerMsg.SessionID == "" {
			if err := conn.Send(tunnel.Message{
				Type:            tunnel.MsgRegisterAck,
				Subdomain:       "myplex",
				ProtocolVersion: tunnel.ProtocolVersion,
				SessionID:       "sess-1",
				MaxConnections:  1,
			}); err != nil {
				serverErrCh <- err
				return
			}

			time.Sleep(50 * time.Millisecond)
			if err := conn.Send(tunnel.Message{
				Type:           tunnel.MsgMaxConnectionsUpdate,
				MaxConnections: 3,
			}); err != nil {
				serverErrCh <- err
				return
			}

			<-r.Context().Done()
			return
		}

		if err := conn.Send(tunnel.Message{
			Type:            tunnel.MsgRegisterAck,
			Subdomain:       "myplex",
			ProtocolVersion: tunnel.ProtocolVersion,
			SessionID:       "sess-1",
			MaxConnections:  3,
		}); err != nil {
			serverErrCh <- err
			return
		}

		<-r.Context().Done()
	}))
	defer srv.Close()
	withPinnedTLS(t, srv)

	cfg := Config{
		Token:             "token-123",
		ServerURL:         toWebSocketURL(srv.URL),
		PlexTarget:        "http://127.0.0.1:32400",
		MaxConnections:    1,
		PingInterval:      time.Hour,
		PongTimeout:       time.Hour,
		MaxReconnectDelay: time.Second,
		ResponseChunkSize: 1024,
	}

	c := New(cfg, zerolog.Nop())
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()

	if err := c.runSession(ctx); err != nil {
		t.Fatalf("runSession() error = %v", err)
	}

	received := make([]tunnel.Message, 0, 3)
	for len(received) < 3 {
		select {
		case registerMsg := <-registerCh:
			received = append(received, registerMsg)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for resize-triggered register messages, got %d", len(received))
		}
	}

	var newSessionCount int
	var joinSessionCount int
	for _, registerMsg := range received {
		if registerMsg.ProtocolVersion != tunnel.ProtocolVersion {
			t.Fatalf("register protocol_version = %d, want %d", registerMsg.ProtocolVersion, tunnel.ProtocolVersion)
		}
		if registerMsg.SessionID == "" {
			if registerMsg.MaxConnections != 1 {
				t.Fatalf("new session register max_connections = %d, want 1", registerMsg.MaxConnections)
			}
			newSessionCount++
			continue
		}
		if registerMsg.SessionID != "sess-1" {
			t.Fatalf("join session_id = %q, want sess-1", registerMsg.SessionID)
		}
		if registerMsg.MaxConnections != 0 {
			t.Fatalf("join register max_connections = %d, want 0", registerMsg.MaxConnections)
		}
		joinSessionCount++
	}

	if newSessionCount != 1 {
		t.Fatalf("new session register count = %d, want 1", newSessionCount)
	}
	if joinSessionCount != 2 {
		t.Fatalf("join session register count = %d, want 2", joinSessionCount)
	}

	if status := c.SnapshotStatus(); status.MaxConnections != 3 {
		t.Fatalf("status.MaxConnections = %d, want 3", status.MaxConnections)
	}

	select {
	case err := <-serverErrCh:
		t.Fatalf("server error: %v", err)
	default:
	}
}

func toWebSocketURL(httpURL string) string {
	if strings.HasPrefix(httpURL, "https://") {
		return "wss://" + strings.TrimPrefix(httpURL, "https://")
	}
	return "ws://" + strings.TrimPrefix(httpURL, "http://")
}

// withPinnedTLS installs an http.DefaultTransport clone whose RootCAs
// trusts only srv.Certificate(), then restores the original transport
// via t.Cleanup. Do not call with t.Parallel active.
func withPinnedTLS(t *testing.T, srv *httptest.Server) {
	t.Helper()
	if srv.Certificate() == nil {
		t.Fatal("withPinnedTLS: srv has no TLS certificate; use httptest.NewTLSServer")
	}
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	orig := http.DefaultTransport
	base, ok := orig.(*http.Transport)
	if !ok {
		t.Fatalf("withPinnedTLS: http.DefaultTransport is %T, want *http.Transport", orig)
	}
	clone := base.Clone()
	clone.TLSClientConfig = &tls.Config{RootCAs: pool}
	http.DefaultTransport = clone
	t.Cleanup(func() { http.DefaultTransport = orig })
}
