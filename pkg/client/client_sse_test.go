package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/CRBL-Technologies/plex-tunnel-proto/tunnel"
)

func TestHandleHTTPRequest_SSEStreamExemptFromTimeout(t *testing.T) {
	serverErrCh := make(chan error, 1)
	closeStream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher, ok := w.(http.Flusher)
		if !ok {
			sendTestErr(serverErrCh, "response writer does not support flushing")
			return
		}

		if _, err := fmt.Fprint(w, "data: hello\n\n"); err != nil {
			return
		}
		flusher.Flush()

		<-closeStream
	}))
	defer upstream.Close()

	pair := newTunnelMessagePair(t)
	collector := startTunnelMessageCollector(pair.server)

	client := newHandleHTTPRequestTestClient(upstream.URL)
	parentCtx := context.Background()
	timeoutCtx, cancel := context.WithTimeoutCause(parentCtx, 100*time.Millisecond, errProxyRequestTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- client.handleHTTPRequest(parentCtx, timeoutCtx, nil, &poolConn{
			conn:  pair.client,
			index: 0,
		}, tunnel.Message{
			ID:     "req-sse",
			Method: http.MethodGet,
			Path:   "/events",
		})
	}()

	time.Sleep(200 * time.Millisecond)
	close(closeStream)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handleHTTPRequest() error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE request to finish")
	}

	messages := collector.waitForEndStream(t)
	combinedBody := joinMessageBodies(messages)
	if !strings.Contains(combinedBody, "data: hello\n\n") {
		t.Fatalf("streamed body = %q, want SSE payload", combinedBody)
	}
	if client.circuit.stateValue() != circuitStateClosed {
		t.Fatalf("circuit state = %q, want %q", client.circuit.stateValue(), circuitStateClosed)
	}
	assertNoAsyncError(t, serverErrCh)
}

func TestHandleHTTPRequest_ProxyTimeoutDoesNotTripCircuitBreaker(t *testing.T) {
	serverErrCh := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		flusher, ok := w.(http.Flusher)
		if !ok {
			sendTestErr(serverErrCh, "response writer does not support flushing")
			return
		}
		flusher.Flush()

		<-r.Context().Done()
	}))
	defer upstream.Close()

	pair := newTunnelMessagePair(t)
	collector := startTunnelMessageCollector(pair.server)
	defer collector.stop(t, pair.client)

	client := newHandleHTTPRequestTestClient(upstream.URL)
	parentCtx := context.Background()
	timeoutCtx, cancel := context.WithTimeoutCause(parentCtx, 50*time.Millisecond, errProxyRequestTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- client.handleHTTPRequest(parentCtx, timeoutCtx, nil, &poolConn{
			conn:  pair.client,
			index: 0,
		}, tunnel.Message{
			ID:     "req-timeout",
			Method: http.MethodGet,
			Path:   "/slow",
		})
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxy timeout to interrupt blocked body read")
	}
	if err == nil {
		t.Fatal("handleHTTPRequest() error = nil, want timeout error")
	}
	if !errors.Is(err, errProxyRequestTimeout) {
		t.Fatalf("handleHTTPRequest() error = %v, want wrapped errProxyRequestTimeout", err)
	}
	if client.circuit.stateValue() != circuitStateClosed {
		t.Fatalf("circuit state = %q, want %q", client.circuit.stateValue(), circuitStateClosed)
	}
	assertNoAsyncError(t, serverErrCh)
}

func TestHandleHTTPRequest_RealUpstreamFailureStillTripsCircuitBreaker(t *testing.T) {
	serverErrCh := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			sendTestErr(serverErrCh, "response writer does not support hijacking")
			return
		}

		conn, buf, err := hijacker.Hijack()
		if err != nil {
			sendTestErr(serverErrCh, "Hijack() error = %v", err)
			return
		}
		defer conn.Close()

		if _, err := fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 32\r\n\r\n{\"partial\":true"); err != nil {
			sendTestErr(serverErrCh, "write hijacked response: %v", err)
			return
		}
		if err := buf.Flush(); err != nil {
			sendTestErr(serverErrCh, "flush hijacked response: %v", err)
		}
	}))
	defer upstream.Close()

	pair := newTunnelMessagePair(t)
	collector := startTunnelMessageCollector(pair.server)
	defer collector.stop(t, pair.client)

	client := newHandleHTTPRequestTestClient(upstream.URL)
	parentCtx := context.Background()
	timeoutCtx, cancel := context.WithTimeoutCause(parentCtx, 5*time.Second, errProxyRequestTimeout)
	defer cancel()

	err := client.handleHTTPRequest(parentCtx, timeoutCtx, nil, &poolConn{
		conn:  pair.client,
		index: 0,
	}, tunnel.Message{
		ID:     "req-upstream-failure",
		Method: http.MethodGet,
		Path:   "/broken",
	})
	if err == nil {
		t.Fatal("handleHTTPRequest() error = nil, want upstream read failure")
	}
	if errors.Is(err, errProxyRequestTimeout) {
		t.Fatalf("handleHTTPRequest() error = %v, want real upstream failure", err)
	}
	if client.circuit.stateValue() != circuitStateOpen {
		t.Fatalf("circuit state = %q, want %q", client.circuit.stateValue(), circuitStateOpen)
	}
	assertNoAsyncError(t, serverErrCh)
}

func newHandleHTTPRequestTestClient(plexTarget string) *Client {
	client := New(Config{
		PlexTarget:            plexTarget,
		ResponseChunkSize:     8,
		ResponseHeaderTimeout: 30 * time.Second,
	}, zerolog.Nop())
	client.circuit = newCircuitBreaker(1, time.Hour, zerolog.Nop())
	return client
}

type tunnelMessagePair struct {
	client *tunnel.WebSocketConnection
	server *tunnel.WebSocketConnection
}

func newTunnelMessagePair(t *testing.T) *tunnelMessagePair {
	t.Helper()

	acceptedCh := make(chan *tunnel.WebSocketConnection, 1)
	serverErrCh := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := tunnel.AcceptWebSocket(w, r)
		if err != nil {
			serverErrCh <- err
			return
		}
		acceptedCh <- conn
	}))
	t.Cleanup(srv.Close)

	clientConn, err := tunnel.DialWebSocket(context.Background(), toWebSocketURL(srv.URL), nil)
	if err != nil {
		t.Fatalf("DialWebSocket() error = %v", err)
	}

	var serverConn *tunnel.WebSocketConnection
	select {
	case serverConn = <-acceptedCh:
	case err := <-serverErrCh:
		t.Fatalf("AcceptWebSocket() error = %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for accepted websocket connection")
	}

	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})

	return &tunnelMessagePair{
		client: clientConn,
		server: serverConn,
	}
}

type tunnelMessageCollector struct {
	done     chan struct{}
	messages []tunnel.Message
	err      error
	mu       sync.Mutex
}

func startTunnelMessageCollector(conn *tunnel.WebSocketConnection) *tunnelMessageCollector {
	collector := &tunnelMessageCollector{
		done: make(chan struct{}),
	}

	go func() {
		defer close(collector.done)
		for {
			msg, err := conn.Receive()
			collector.mu.Lock()
			if err != nil {
				collector.err = err
				collector.mu.Unlock()
				return
			}
			collector.messages = append(collector.messages, msg)
			collector.mu.Unlock()
			if msg.EndStream {
				return
			}
		}
	}()

	return collector
}

func (c *tunnelMessageCollector) waitForEndStream(t *testing.T) []tunnel.Message {
	t.Helper()

	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tunnel response messages")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err != nil {
		t.Fatalf("collector receive error = %v", c.err)
	}
	if len(c.messages) == 0 {
		t.Fatal("collector received no tunnel messages")
	}
	if !c.messages[len(c.messages)-1].EndStream {
		t.Fatal("collector did not receive end-of-stream message")
	}

	messages := make([]tunnel.Message, len(c.messages))
	copy(messages, c.messages)
	return messages
}

func (c *tunnelMessageCollector) stop(t *testing.T, conn *tunnel.WebSocketConnection) {
	t.Helper()

	_ = conn.Close()

	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tunnel collector shutdown")
	}
}

func joinMessageBodies(messages []tunnel.Message) string {
	var builder strings.Builder
	for _, msg := range messages {
		builder.Write(msg.Body)
	}
	return builder.String()
}

func assertNoAsyncError(t *testing.T, errCh <-chan error) {
	t.Helper()

	select {
	case err := <-errCh:
		t.Fatalf("async test error: %v", err)
	default:
	}
}

func sendTestErr(errCh chan<- error, format string, args ...any) {
	select {
	case errCh <- fmt.Errorf(format, args...):
	default:
	}
}
