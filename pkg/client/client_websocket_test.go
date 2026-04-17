package client

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"nhooyr.io/websocket"

	"github.com/CRBL-Technologies/plex-tunnel-proto/tunnel"
)

func TestRegisterAdvertisesCapWSFlowControl(t *testing.T) {
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
			Capabilities:    tunnel.CapLeasedPool | tunnel.CapWSFlowControl,
		}); err != nil {
			serverErrCh <- err
			return
		}

		drainUntilClose(conn)
	}))
	defer srv.Close()
	withPinnedTLS(t, srv)

	client := New(Config{
		Token:             "token-123",
		ServerURL:         toWebSocketURL(srv.URL),
		PlexTarget:        "http://127.0.0.1:32400",
		MaxConnections:    1,
		PingInterval:      time.Hour,
		PongTimeout:       time.Hour,
		MaxReconnectDelay: time.Second,
		ResponseChunkSize: 1024,
	}, zerolog.Nop())

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	if err := client.runSession(ctx); err != nil {
		t.Fatalf("runSession() error = %v", err)
	}

	registerMsg := mustReceiveRegisterMessage(t, registerCh)
	if registerMsg.Capabilities&tunnel.CapLeasedPool == 0 {
		t.Fatal("register capabilities missing CapLeasedPool")
	}
	if registerMsg.Capabilities&tunnel.CapWSFlowControl == 0 {
		t.Fatal("register capabilities missing CapWSFlowControl")
	}

	assertNoAsyncError(t, serverErrCh)
}

func TestFlowControlEnabledWhenServerAcksCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	session := newSessionPoolController(
		New(Config{}, zerolog.Nop()),
		ctx,
		newConnectionPool("server", "subdomain", "session", 1),
		make(chan error, 1),
		(tunnel.CapLeasedPool&tunnel.CapWSFlowControl) != 0 || (tunnel.CapWSFlowControl != 0),
	)

	if !session.wsFlowControl() {
		t.Fatal("session wsFlowControl() = false, want true")
	}
}

func TestFlowControlDisabledWhenServerOmitsCap(t *testing.T) {
	upstream := newWSUpstreamServer(t)
	harness := newWSControlHarness(t, false, upstream.plexTarget())
	harness.startReadLoop()

	if harness.session.wsFlowControl() {
		t.Fatal("session wsFlowControl() = true, want false")
	}

	const streamID = "legacy-disabled"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: streamID, Path: "/notifications"})
	ack := harness.receiveClientMessage(t, 2*time.Second)
	if ack.Type != tunnel.MsgWSOpen || ack.ID != streamID {
		t.Fatalf("open ack = %#v, want MsgWSOpen with id %q", ack, streamID)
	}

	accepted := upstream.waitConn(t, 2*time.Second)
	defer accepted.conn.CloseNow()

	payloadA := bytes.Repeat([]byte("a"), wsWindowUpdateThreshold/2)
	payloadB := bytes.Repeat([]byte("b"), wsWindowUpdateThreshold/2)

	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSFrame, ID: streamID, Body: payloadA})
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSFrame, ID: streamID, Body: payloadB})

	_, gotA := readUpstreamMessage(t, accepted.conn, 2*time.Second)
	_, gotB := readUpstreamMessage(t, accepted.conn, 2*time.Second)
	if !bytes.Equal(gotA, payloadA) {
		t.Fatalf("first upstream payload length = %d, want %d", len(gotA), len(payloadA))
	}
	if !bytes.Equal(gotB, payloadB) {
		t.Fatalf("second upstream payload length = %d, want %d", len(gotB), len(payloadB))
	}

	harness.expectNoClientMessage(t, 250*time.Millisecond)
}

func TestWSOpenDialsUpstreamAndAcks(t *testing.T) {
	upstream := newWSUpstreamServer(t)
	harness := newWSControlHarness(t, true, upstream.plexTarget())
	harness.startReadLoop()

	const streamID = "ws-open"
	harness.sendServerMessage(tunnel.Message{
		Type: tunnel.MsgWSOpen,
		ID:   streamID,
		Path: "/:/websockets/notifications",
	})

	ack := harness.receiveClientMessage(t, 2*time.Second)
	if ack.Type != tunnel.MsgWSOpen {
		t.Fatalf("ack type = %v, want %v", ack.Type, tunnel.MsgWSOpen)
	}
	if ack.ID != streamID {
		t.Fatalf("ack id = %q, want %q", ack.ID, streamID)
	}

	accepted := upstream.waitConn(t, 2*time.Second)
	defer accepted.conn.CloseNow()
	if accepted.path != "/:/websockets/notifications" {
		t.Fatalf("upstream path = %q, want %q", accepted.path, "/:/websockets/notifications")
	}
}

func TestWSOpenDialFailureSendsMsgError(t *testing.T) {
	harness := newWSControlHarness(t, true, "http://127.0.0.1:1")
	harness.startReadLoop()

	const streamID = "ws-open-fail"
	harness.sendServerMessage(tunnel.Message{
		Type: tunnel.MsgWSOpen,
		ID:   streamID,
		Path: "/unreachable",
	})

	msg := harness.receiveClientMessage(t, 2*time.Second)
	if msg.Type != tunnel.MsgError {
		t.Fatalf("message type = %v, want %v", msg.Type, tunnel.MsgError)
	}
	if msg.ID != streamID {
		t.Fatalf("message id = %q, want %q", msg.ID, streamID)
	}

	harness.expectNoClientMessage(t, 150*time.Millisecond)
}

func TestWSFrameServerToClientWritesUpstream(t *testing.T) {
	upstream := newWSUpstreamServer(t)
	harness := newWSControlHarness(t, true, upstream.plexTarget())
	harness.startReadLoop()

	const streamID = "server-to-client"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: streamID, Path: "/socket"})
	ack := harness.receiveClientMessage(t, 2*time.Second)
	if ack.Type != tunnel.MsgWSOpen {
		t.Fatalf("open ack type = %v, want %v", ack.Type, tunnel.MsgWSOpen)
	}

	accepted := upstream.waitConn(t, 2*time.Second)
	defer accepted.conn.CloseNow()

	textPayload := []byte("hello text")
	binaryPayload := []byte{0x00, 0x01, 0x02, 0x03}

	harness.sendServerMessage(tunnel.Message{
		Type: tunnel.MsgWSFrame,
		ID:   streamID,
		Body: textPayload,
	})
	msgType, body := readUpstreamMessage(t, accepted.conn, 2*time.Second)
	if msgType != websocket.MessageText {
		t.Fatalf("text message type = %v, want %v", msgType, websocket.MessageText)
	}
	if !bytes.Equal(body, textPayload) {
		t.Fatalf("text body = %q, want %q", body, textPayload)
	}

	harness.sendServerMessage(tunnel.Message{
		Type:     tunnel.MsgWSFrame,
		ID:       streamID,
		Body:     binaryPayload,
		WSBinary: true,
	})
	msgType, body = readUpstreamMessage(t, accepted.conn, 2*time.Second)
	if msgType != websocket.MessageBinary {
		t.Fatalf("binary message type = %v, want %v", msgType, websocket.MessageBinary)
	}
	if !bytes.Equal(body, binaryPayload) {
		t.Fatalf("binary body = %v, want %v", body, binaryPayload)
	}
}

func TestWSWindowUpdateEmittedAfterHalfWindow(t *testing.T) {
	upstream := newWSUpstreamServer(t)
	harness := newWSControlHarness(t, true, upstream.plexTarget())
	harness.startReadLoop()

	const streamID = "window-update"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: streamID, Path: "/socket"})
	harness.expectOpenAck(t, streamID)

	accepted := upstream.waitConn(t, 2*time.Second)
	defer accepted.conn.CloseNow()

	frameA := bytes.Repeat([]byte("a"), wsWindowUpdateThreshold/2)
	frameB := bytes.Repeat([]byte("b"), wsWindowUpdateThreshold/2)

	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSFrame, ID: streamID, Body: frameA})
	readUpstreamMessage(t, accepted.conn, 2*time.Second)
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSFrame, ID: streamID, Body: frameB})
	readUpstreamMessage(t, accepted.conn, 2*time.Second)

	update := harness.receiveClientMessage(t, 2*time.Second)
	if update.Type != tunnel.MsgWSWindowUpdate {
		t.Fatalf("message type = %v, want %v", update.Type, tunnel.MsgWSWindowUpdate)
	}
	if update.ID != streamID {
		t.Fatalf("message id = %q, want %q", update.ID, streamID)
	}
	if update.WindowIncrement < uint32(wsWindowUpdateThreshold) {
		t.Fatalf("window increment = %d, want >= %d", update.WindowIncrement, wsWindowUpdateThreshold)
	}

	harness.expectNoClientMessage(t, 150*time.Millisecond)
}

func TestWSFrameClientToServerSplitsAt64KiB(t *testing.T) {
	upstream := newWSUpstreamServer(t)
	harness := newWSControlHarness(t, false, upstream.plexTarget())
	harness.startReadLoop()

	const streamID = "client-to-server-split"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: streamID, Path: "/socket"})
	harness.expectOpenAck(t, streamID)

	accepted := upstream.waitConn(t, 2*time.Second)
	defer accepted.conn.CloseNow()

	payload := bytes.Repeat([]byte("x"), 2*wsInitialWindowBytes)
	writeUpstreamMessage(t, accepted.conn, websocket.MessageBinary, payload)

	first := harness.receiveClientMessage(t, 2*time.Second)
	second := harness.receiveClientMessage(t, 2*time.Second)

	if first.Type != tunnel.MsgWSFrame || second.Type != tunnel.MsgWSFrame {
		t.Fatalf("frame types = %v / %v, want %v", first.Type, second.Type, tunnel.MsgWSFrame)
	}
	if first.ID != streamID || second.ID != streamID {
		t.Fatalf("frame ids = %q / %q, want %q", first.ID, second.ID, streamID)
	}
	if len(first.Body) > wsInitialWindowBytes || len(second.Body) > wsInitialWindowBytes {
		t.Fatalf("frame lengths = %d / %d, want <= %d", len(first.Body), len(second.Body), wsInitialWindowBytes)
	}
	if !first.WSBinary || !second.WSBinary {
		t.Fatal("expected both split frames to preserve WSBinary=true")
	}

	combined := append(append([]byte(nil), first.Body...), second.Body...)
	if !bytes.Equal(combined, payload) {
		t.Fatalf("combined payload length = %d, want %d", len(combined), len(payload))
	}

	harness.expectNoClientMessage(t, 150*time.Millisecond)
}

func TestWSSenderBlocksOnZeroCredit(t *testing.T) {
	upstream := newWSUpstreamServer(t)
	harness := newWSControlHarness(t, true, upstream.plexTarget())
	harness.startReadLoop()

	const streamID = "credit-block"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: streamID, Path: "/socket"})
	harness.expectOpenAck(t, streamID)

	accepted := upstream.waitConn(t, 2*time.Second)
	defer accepted.conn.CloseNow()

	writeUpstreamMessage(t, accepted.conn, websocket.MessageBinary, bytes.Repeat([]byte("a"), wsInitialWindowBytes))
	first := harness.receiveClientMessage(t, 2*time.Second)
	if first.Type != tunnel.MsgWSFrame || len(first.Body) != wsInitialWindowBytes {
		t.Fatalf("first frame = %#v, want 64KiB MsgWSFrame", first)
	}

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		writeUpstreamMessage(t, accepted.conn, websocket.MessageBinary, bytes.Repeat([]byte("b"), wsWindowUpdateThreshold))
	}()

	harness.expectNoClientMessage(t, 100*time.Millisecond)

	harness.sendServerMessage(tunnel.Message{
		Type:            tunnel.MsgWSWindowUpdate,
		ID:              streamID,
		WindowIncrement: wsWindowUpdateThreshold,
	})

	second := harness.receiveClientMessage(t, 200*time.Millisecond)
	if second.Type != tunnel.MsgWSFrame {
		t.Fatalf("second frame type = %v, want %v", second.Type, tunnel.MsgWSFrame)
	}
	if len(second.Body) != wsWindowUpdateThreshold {
		t.Fatalf("second frame length = %d, want %d", len(second.Body), wsWindowUpdateThreshold)
	}

	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for blocked upstream write to complete")
	}
}

func TestWSWindowUpdateZeroIncrementIsStreamScopedError(t *testing.T) {
	upstream := newWSUpstreamServer(t)
	harness := newWSControlHarness(t, true, upstream.plexTarget())
	harness.startReadLoop()

	const stream1 = "zero-increment"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: stream1, Path: "/socket"})
	harness.expectOpenAck(t, stream1)
	firstConn := upstream.waitConn(t, 2*time.Second)
	defer firstConn.conn.CloseNow()

	harness.sendServerMessage(tunnel.Message{
		Type:            tunnel.MsgWSWindowUpdate,
		ID:              stream1,
		WindowIncrement: 0,
	})

	errMsg := harness.receiveClientMessage(t, 2*time.Second)
	closeMsg := harness.receiveClientMessage(t, 2*time.Second)
	if errMsg.Type != tunnel.MsgError || errMsg.ID != stream1 {
		t.Fatalf("error msg = %#v, want MsgError for %q", errMsg, stream1)
	}
	if closeMsg.Type != tunnel.MsgWSClose || closeMsg.ID != stream1 || closeMsg.Status != int(websocket.StatusPolicyViolation) {
		t.Fatalf("close msg = %#v, want MsgWSClose 1008 for %q", closeMsg, stream1)
	}

	const stream2 = "zero-increment-still-works"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: stream2, Path: "/socket"})
	harness.expectOpenAck(t, stream2)
	secondConn := upstream.waitConn(t, 2*time.Second)
	defer secondConn.conn.CloseNow()

	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSFrame, ID: stream2, Body: []byte("ok")})
	_, body := readUpstreamMessage(t, secondConn.conn, 2*time.Second)
	if string(body) != "ok" {
		t.Fatalf("second stream body = %q, want %q", body, "ok")
	}
}

func TestWSWindowUpdateBodyIsStreamScopedError(t *testing.T) {
	upstream := newWSUpstreamServer(t)
	harness := newWSControlHarness(t, true, upstream.plexTarget())
	harness.startReadLoop()

	const stream1 = "body-error"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: stream1, Path: "/socket"})
	harness.expectOpenAck(t, stream1)
	firstConn := upstream.waitConn(t, 2*time.Second)
	defer firstConn.conn.CloseNow()

	harness.sendServerMessage(tunnel.Message{
		Type:            tunnel.MsgWSWindowUpdate,
		ID:              stream1,
		WindowIncrement: 1,
		Body:            []byte("not-empty"),
	})

	errMsg := harness.receiveClientMessage(t, 2*time.Second)
	closeMsg := harness.receiveClientMessage(t, 2*time.Second)
	if errMsg.Type != tunnel.MsgError || errMsg.ID != stream1 {
		t.Fatalf("error msg = %#v, want MsgError for %q", errMsg, stream1)
	}
	if closeMsg.Type != tunnel.MsgWSClose || closeMsg.ID != stream1 || closeMsg.Status != int(websocket.StatusPolicyViolation) {
		t.Fatalf("close msg = %#v, want MsgWSClose 1008 for %q", closeMsg, stream1)
	}

	const stream2 = "body-error-still-works"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: stream2, Path: "/socket"})
	harness.expectOpenAck(t, stream2)
	secondConn := upstream.waitConn(t, 2*time.Second)
	defer secondConn.conn.CloseNow()

	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSFrame, ID: stream2, Body: []byte("ok")})
	_, body := readUpstreamMessage(t, secondConn.conn, 2*time.Second)
	if string(body) != "ok" {
		t.Fatalf("second stream body = %q, want %q", body, "ok")
	}
}

func TestWSCreditOverflowIsStreamScopedError(t *testing.T) {
	upstream := newWSUpstreamServer(t)
	harness := newWSControlHarness(t, true, upstream.plexTarget())
	harness.startReadLoop()

	const stream1 = "overflow"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: stream1, Path: "/socket"})
	harness.expectOpenAck(t, stream1)
	firstConn := upstream.waitConn(t, 2*time.Second)
	defer firstConn.conn.CloseNow()

	harness.sendServerMessage(tunnel.Message{
		Type:            tunnel.MsgWSWindowUpdate,
		ID:              stream1,
		WindowIncrement: uint32(wsMaxPendingCredit - wsInitialWindowBytes + 1),
	})

	errMsg := harness.receiveClientMessage(t, 2*time.Second)
	closeMsg := harness.receiveClientMessage(t, 2*time.Second)
	if errMsg.Type != tunnel.MsgError || errMsg.ID != stream1 {
		t.Fatalf("error msg = %#v, want MsgError for %q", errMsg, stream1)
	}
	if closeMsg.Type != tunnel.MsgWSClose || closeMsg.ID != stream1 || closeMsg.Status != int(websocket.StatusPolicyViolation) {
		t.Fatalf("close msg = %#v, want MsgWSClose 1008 for %q", closeMsg, stream1)
	}

	const stream2 = "overflow-still-works"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: stream2, Path: "/socket"})
	harness.expectOpenAck(t, stream2)
	secondConn := upstream.waitConn(t, 2*time.Second)
	defer secondConn.conn.CloseNow()

	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSFrame, ID: stream2, Body: []byte("ok")})
	_, body := readUpstreamMessage(t, secondConn.conn, 2*time.Second)
	if string(body) != "ok" {
		t.Fatalf("second stream body = %q, want %q", body, "ok")
	}
}

func TestWSCloseFromServerTearsDownUpstream(t *testing.T) {
	upstream := newWSUpstreamServer(t)
	harness := newWSControlHarness(t, true, upstream.plexTarget())
	harness.startReadLoop()

	const streamID = "server-close"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: streamID, Path: "/socket"})
	harness.expectOpenAck(t, streamID)

	accepted := upstream.waitConn(t, 2*time.Second)
	defer accepted.conn.CloseNow()

	harness.sendServerMessage(tunnel.Message{
		Type:   tunnel.MsgWSClose,
		ID:     streamID,
		Status: int(websocket.StatusNormalClosure),
	})

	expectUpstreamReadError(t, accepted.conn, 200*time.Millisecond)
}

func TestWSLateFrameAfterCloseDropped(t *testing.T) {
	upstream := newWSUpstreamServer(t)
	harness := newWSControlHarness(t, true, upstream.plexTarget())
	harness.startReadLoop()

	const streamID = "late-frame"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: streamID, Path: "/socket"})
	harness.expectOpenAck(t, streamID)

	accepted := upstream.waitConn(t, 2*time.Second)
	defer accepted.conn.CloseNow()

	harness.sendServerMessage(tunnel.Message{
		Type:   tunnel.MsgWSClose,
		ID:     streamID,
		Status: int(websocket.StatusNormalClosure),
	})
	expectUpstreamReadError(t, accepted.conn, 200*time.Millisecond)

	harness.sendServerMessage(tunnel.Message{
		Type: tunnel.MsgWSFrame,
		ID:   streamID,
		Body: []byte("late"),
	})

	harness.expectNoClientMessage(t, 150*time.Millisecond)
}

func TestWSOversizeFrameFromServerTearsDownSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := New(Config{
		PlexTarget:            "http://127.0.0.1:32400",
		PingInterval:          time.Hour,
		PongTimeout:           time.Hour,
		ResponseChunkSize:     1024,
		ResponseHeaderTimeout: 30 * time.Second,
	}, zerolog.Nop())

	pair := newTunnelMessagePair(t)
	pool := newConnectionPool("server", "subdomain", "session", 1)
	errCh := make(chan error, 1)
	session := newSessionPoolController(client, ctx, pool, errCh, true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		client.maintainPoolSlot(ctx, session, 0, pair.client)
	}()

	time.Sleep(25 * time.Millisecond)
	pair.server.Send(tunnel.Message{
		Type: tunnel.MsgWSFrame,
		ID:   "oversize",
		Body: bytes.Repeat([]byte("x"), wsInitialWindowBytes+1),
	})

	waitForTunnelClose(t, pair.server, 2*time.Second)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for maintainPoolSlot to exit after oversize frame")
	}
}

func TestWSLegacyModeNoWindowUpdates(t *testing.T) {
	upstream := newWSUpstreamServer(t)
	harness := newWSControlHarness(t, false, upstream.plexTarget())
	harness.startReadLoop()

	const streamID = "legacy-no-window-update"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: streamID, Path: "/socket"})
	harness.expectOpenAck(t, streamID)

	accepted := upstream.waitConn(t, 2*time.Second)
	defer accepted.conn.CloseNow()

	frameA := bytes.Repeat([]byte("a"), wsWindowUpdateThreshold/2)
	frameB := bytes.Repeat([]byte("b"), wsWindowUpdateThreshold/2)

	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSFrame, ID: streamID, Body: frameA})
	readUpstreamMessage(t, accepted.conn, 2*time.Second)
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSFrame, ID: streamID, Body: frameB})
	readUpstreamMessage(t, accepted.conn, 2*time.Second)

	harness.expectNoClientMessage(t, 500*time.Millisecond)
}

func TestWSStreamSlotGatedByControlSemaphore(t *testing.T) {
	upstream := newWSUpstreamServer(t)
	harness := newWSControlHarness(t, true, upstream.plexTarget())
	harness.startReadLoop()

	for i := 0; i < maxControlStreams; i++ {
		harness.client.controlSem <- struct{}{}
	}

	const streamID = "control-slot-saturated"
	harness.sendServerMessage(tunnel.Message{Type: tunnel.MsgWSOpen, ID: streamID, Path: "/socket"})

	msg := harness.receiveClientMessage(t, 2*time.Second)
	if msg.Type != tunnel.MsgError || msg.ID != streamID {
		t.Fatalf("message = %#v, want MsgError for %q", msg, streamID)
	}

	harness.expectNoClientMessage(t, 150*time.Millisecond)
	if upstream.acceptCount() != 0 {
		t.Fatalf("upstream accept count = %d, want 0", upstream.acceptCount())
	}
}

type wsControlHarness struct {
	t       *testing.T
	ctx     context.Context
	cancel  context.CancelFunc
	client  *Client
	session *sessionPoolController
	connRef *poolConn
	pair    *tunnelMessagePair
	done    chan error
	msgCh   chan tunnel.Message
	errCh   chan error
}

func newWSControlHarness(t *testing.T, flowControlEnabled bool, plexTarget string) *wsControlHarness {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	client := New(Config{
		PlexTarget:            plexTarget,
		PingInterval:          time.Hour,
		PongTimeout:           time.Hour,
		ResponseChunkSize:     1024,
		ResponseHeaderTimeout: 30 * time.Second,
	}, zerolog.Nop())

	pair := newTunnelMessagePair(t)
	pool := newConnectionPool("server", "subdomain", "session-1", 1)
	connRef, _ := pool.add(0, pair.client)
	session := newSessionPoolController(client, ctx, pool, make(chan error, 1), flowControlEnabled)

	harness := &wsControlHarness{
		t:       t,
		ctx:     ctx,
		cancel:  cancel,
		client:  client,
		session: session,
		connRef: connRef,
		pair:    pair,
		msgCh:   make(chan tunnel.Message, 64),
		errCh:   make(chan error, 1),
	}

	go func() {
		for {
			msg, err := harness.pair.server.Receive()
			if err != nil {
				select {
				case harness.errCh <- err:
				default:
				}
				return
			}
			harness.msgCh <- msg
		}
	}()

	t.Cleanup(func() {
		harness.cancel()
		_ = harness.pair.client.Close()
		if harness.done != nil {
			select {
			case <-harness.done:
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for websocket readLoop shutdown")
			}
		}
	})

	return harness
}

func (h *wsControlHarness) startReadLoop() {
	h.t.Helper()
	if h.done != nil {
		return
	}

	h.done = make(chan error, 1)
	go func() {
		h.done <- h.client.readLoopWithConnection(h.ctx, h.session, h.connRef, h.connRef.conn)
	}()
}

func (h *wsControlHarness) sendServerMessage(msg tunnel.Message) {
	h.t.Helper()
	if err := h.pair.server.Send(msg); err != nil {
		h.t.Fatalf("server Send(%v) error = %v", msg.Type, err)
	}
}

func (h *wsControlHarness) receiveClientMessage(t *testing.T, timeout time.Duration) tunnel.Message {
	t.Helper()
	select {
	case msg := <-h.msgCh:
		return msg
	case err := <-h.errCh:
		t.Fatalf("Receive() error = %v", err)
	case <-time.After(timeout):
		t.Fatal("timed out waiting for tunnel message")
	}
	return tunnel.Message{}
}

func (h *wsControlHarness) expectNoClientMessage(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case msg := <-h.msgCh:
		t.Fatalf("expected no tunnel message, but received %#v", msg)
	case err := <-h.errCh:
		t.Fatalf("Receive() error = %v", err)
	case <-time.After(timeout):
	}
}

func (h *wsControlHarness) expectOpenAck(t *testing.T, streamID string) {
	t.Helper()
	msg := h.receiveClientMessage(t, 2*time.Second)
	if msg.Type != tunnel.MsgWSOpen || msg.ID != streamID {
		t.Fatalf("open ack = %#v, want MsgWSOpen with id %q", msg, streamID)
	}
}

type wsAcceptedConn struct {
	conn *websocket.Conn
	path string
}

type wsUpstreamServer struct {
	t        *testing.T
	srv      *httptest.Server
	acceptCh chan wsAcceptedConn
	errCh    chan error
	connsMu  sync.Mutex
	conns    []*websocket.Conn
	accepts  atomic.Int32
}

func newWSUpstreamServer(t *testing.T) *wsUpstreamServer {
	t.Helper()

	server := &wsUpstreamServer{
		t:        t,
		acceptCh: make(chan wsAcceptedConn, 16),
		errCh:    make(chan error, 1),
	}

	server.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			sendTestErr(server.errCh, "websocket.Accept() error = %v", err)
			return
		}

		server.connsMu.Lock()
		server.conns = append(server.conns, conn)
		server.connsMu.Unlock()
		server.accepts.Add(1)

		select {
		case server.acceptCh <- wsAcceptedConn{conn: conn, path: r.URL.Path}:
		default:
			sendTestErr(server.errCh, "accept channel full")
			conn.CloseNow()
		}
	}))

	t.Cleanup(func() {
		server.srv.Close()
		server.connsMu.Lock()
		for _, conn := range server.conns {
			conn.CloseNow()
		}
		server.connsMu.Unlock()
		assertNoAsyncError(t, server.errCh)
	})

	return server
}

func (s *wsUpstreamServer) plexTarget() string {
	return s.srv.URL
}

func (s *wsUpstreamServer) waitConn(t *testing.T, timeout time.Duration) wsAcceptedConn {
	t.Helper()
	select {
	case accepted := <-s.acceptCh:
		return accepted
	case err := <-s.errCh:
		t.Fatalf("upstream server error: %v", err)
	case <-time.After(timeout):
		t.Fatal("timed out waiting for upstream websocket accept")
	}
	return wsAcceptedConn{}
}

func (s *wsUpstreamServer) acceptCount() int32 {
	return s.accepts.Load()
}

func writeUpstreamMessage(t *testing.T, conn *websocket.Conn, msgType websocket.MessageType, body []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := conn.Write(ctx, msgType, body); err != nil {
		t.Fatalf("upstream Write() error = %v", err)
	}
}

func readUpstreamMessage(t *testing.T, conn *websocket.Conn, timeout time.Duration) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	msgType, body, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("upstream Read() error = %v", err)
	}
	return msgType, body
}

func expectUpstreamReadError(t *testing.T, conn *websocket.Conn, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Fatal("upstream Read() error = nil, want close error")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("upstream Read() error = %v, want websocket close error", err)
	}
}

func waitForTunnelClose(t *testing.T, conn *tunnel.WebSocketConnection, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	_, err := conn.ReceiveContext(ctx)
	if err == nil {
		t.Fatal("expected tunnel websocket close, but a message arrived")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReceiveContext() error = %v, want tunnel websocket close", err)
	}
}

func mustReceiveRegisterMessage(t *testing.T, registerCh <-chan tunnel.Message) tunnel.Message {
	t.Helper()
	select {
	case registerMsg := <-registerCh:
		return registerMsg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for register message")
	}
	return tunnel.Message{}
}
