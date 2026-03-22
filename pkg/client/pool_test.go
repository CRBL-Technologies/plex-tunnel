package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/CRBL-Technologies/plex-tunnel-proto/tunnel"
)

type testSocketPair struct {
	client *tunnel.WebSocketConnection
	server *tunnel.WebSocketConnection
	closed chan struct{}
	once   sync.Once
}

func newTestSocketPair(t *testing.T) *testSocketPair {
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

	pair := &testSocketPair{
		client: clientConn,
		server: serverConn,
		closed: make(chan struct{}),
	}

	go func() {
		_, _ = serverConn.Receive()
		pair.once.Do(func() {
			close(pair.closed)
		})
	}()

	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
		select {
		case <-pair.closed:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for websocket pair cleanup")
		}
	})

	return pair
}

func waitForClose(t *testing.T, closed <-chan struct{}) {
	t.Helper()

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for websocket close")
	}
}

func TestPoolResize_ScaleDown(t *testing.T) {
	conn0 := newTestSocketPair(t)
	conn2 := newTestSocketPair(t)
	conn3 := newTestSocketPair(t)

	pool := newConnectionPool("server", "subdomain", "session", 4)
	pool.conns[0] = &poolConn{conn: conn0.client, index: 0}
	pool.conns[2] = &poolConn{conn: conn2.client, index: 2}
	pool.conns[3] = &poolConn{conn: conn3.client, index: 3}

	slot2Canceled := make(chan struct{})
	slot3Canceled := make(chan struct{})
	pool.slotCancels[2] = func() { close(slot2Canceled) }
	pool.slotCancels[3] = func() { close(slot3Canceled) }

	oldMax, newMax, promoted := pool.Resize(2)
	if oldMax != 4 {
		t.Fatalf("oldMax = %d, want 4", oldMax)
	}
	if newMax != 2 {
		t.Fatalf("newMax = %d, want 2", newMax)
	}
	if promoted != nil {
		t.Fatalf("promoted = %+v, want nil", promoted)
	}
	if pool.maxConns != 2 {
		t.Fatalf("pool.maxConns = %d, want 2", pool.maxConns)
	}
	if len(pool.conns) != 2 {
		t.Fatalf("len(pool.conns) = %d, want 2", len(pool.conns))
	}
	if pool.conns[0] == nil {
		t.Fatal("pool.conns[0] = nil, want active connection")
	}
	if pool.conns[1] != nil {
		t.Fatalf("pool.conns[1] = %+v, want nil", pool.conns[1])
	}
	if pool.activeCount() != 1 {
		t.Fatalf("pool.activeCount() = %d, want 1", pool.activeCount())
	}

	waitForClose(t, slot2Canceled)
	waitForClose(t, slot3Canceled)
	waitForClose(t, conn2.closed)
	waitForClose(t, conn3.closed)

	select {
	case <-conn0.closed:
		t.Fatal("slot 0 connection was closed during scale down")
	default:
	}
}

func TestPoolResize_ScaleUp(t *testing.T) {
	pool := newConnectionPool("server", "subdomain", "session", 2)

	oldMax, newMax, promoted := pool.Resize(4)
	if oldMax != 2 {
		t.Fatalf("oldMax = %d, want 2", oldMax)
	}
	if newMax != 4 {
		t.Fatalf("newMax = %d, want 4", newMax)
	}
	if promoted != nil {
		t.Fatalf("promoted = %+v, want nil", promoted)
	}
	if pool.maxConns != 4 {
		t.Fatalf("pool.maxConns = %d, want 4", pool.maxConns)
	}
	if len(pool.conns) != 4 {
		t.Fatalf("len(pool.conns) = %d, want 4", len(pool.conns))
	}
	if len(pool.slotCancels) != 4 {
		t.Fatalf("len(pool.slotCancels) = %d, want 4", len(pool.slotCancels))
	}
	if pool.conns[2] != nil {
		t.Fatalf("pool.conns[2] = %+v, want nil", pool.conns[2])
	}
	if pool.conns[3] != nil {
		t.Fatalf("pool.conns[3] = %+v, want nil", pool.conns[3])
	}
}

func TestPoolResize_NoChange(t *testing.T) {
	conn0 := newTestSocketPair(t)

	pool := newConnectionPool("server", "subdomain", "session", 2)
	pool.conns[0] = &poolConn{conn: conn0.client, index: 0}

	oldMax, newMax, promoted := pool.Resize(2)
	if oldMax != 2 {
		t.Fatalf("oldMax = %d, want 2", oldMax)
	}
	if newMax != 2 {
		t.Fatalf("newMax = %d, want 2", newMax)
	}
	if promoted != nil {
		t.Fatalf("promoted = %+v, want nil", promoted)
	}
	if len(pool.conns) != 2 {
		t.Fatalf("len(pool.conns) = %d, want 2", len(pool.conns))
	}
	if pool.conns[0] == nil || pool.conns[0].conn != conn0.client {
		t.Fatal("slot 0 connection changed during no-op resize")
	}

	select {
	case <-conn0.closed:
		t.Fatal("slot 0 connection was closed during no-op resize")
	default:
	}
}

func TestPoolResize_ScaleDown_PromotesControl(t *testing.T) {
	conn1 := newTestSocketPair(t)
	conn3 := newTestSocketPair(t)

	pool := newConnectionPool("server", "subdomain", "session", 4)
	pool.conns[1] = &poolConn{conn: conn1.client, index: 1}
	pool.conns[3] = &poolConn{conn: conn3.client, index: 3}
	pool.controlIndex = 3

	pingCanceled := make(chan struct{})
	pool.pingCancel = func() {
		close(pingCanceled)
	}

	oldMax, newMax, promoted := pool.Resize(2)
	if oldMax != 4 {
		t.Fatalf("oldMax = %d, want 4", oldMax)
	}
	if newMax != 2 {
		t.Fatalf("newMax = %d, want 2", newMax)
	}
	if promoted == nil {
		t.Fatal("promoted = nil, want slot 1 connection")
	}
	if promoted.index != 1 {
		t.Fatalf("promoted.index = %d, want 1", promoted.index)
	}
	if pool.controlIndex != 1 {
		t.Fatalf("pool.controlIndex = %d, want 1", pool.controlIndex)
	}

	waitForClose(t, pingCanceled)
	waitForClose(t, conn3.closed)

	select {
	case <-conn1.closed:
		t.Fatal("promoted control connection was closed during scale down")
	default:
	}
}
