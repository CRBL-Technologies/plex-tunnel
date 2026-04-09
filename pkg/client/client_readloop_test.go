package client

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CRBL-Technologies/plex-tunnel-proto/tunnel"
	"github.com/rs/zerolog"
)

func TestReadLoop_BusyTunnelSurvivesDeadline(t *testing.T) {
	client := New(Config{}, zerolog.Nop())
	session := &sessionPoolController{
		pool: newConnectionPool("server", "subdomain", "session", 1),
	}
	connRef := &poolConn{index: 0}
	connRef.streams.Store(1)

	blockedReceive := make(chan struct{})
	fakeConn := &scriptedReadLoopConn{
		results: []readLoopReceiveResult{
			{err: context.DeadlineExceeded},
			{msg: tunnel.Message{Type: tunnel.MsgPong}},
			{wait: blockedReceive, err: context.Canceled},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- client.readLoopWithConnection(ctx, session, connRef, fakeConn)
	}()

	time.Sleep(250 * time.Millisecond)

	select {
	case err := <-done:
		t.Fatalf("readLoop returned early after active-stream deadline: %v", err)
	default:
	}

	if connRef.lastPong.Load() == 0 {
		t.Fatal("expected readLoop to process MsgPong after retrying the read timeout")
	}
	if fakeConn.closeCalls.Load() != 0 {
		t.Fatalf("connection close calls = %d, want 0", fakeConn.closeCalls.Load())
	}

	cancel()
	close(blockedReceive)

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("readLoop error = %v, want wrapped context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for readLoop to exit after context cancellation")
	}
}

type readLoopReceiveResult struct {
	msg  tunnel.Message
	err  error
	wait <-chan struct{}
}

type scriptedReadLoopConn struct {
	results    []readLoopReceiveResult
	closeCalls atomic.Int32
}

func (c *scriptedReadLoopConn) Send(tunnel.Message) error {
	return nil
}

func (c *scriptedReadLoopConn) Receive() (tunnel.Message, error) {
	if len(c.results) == 0 {
		return tunnel.Message{}, context.DeadlineExceeded
	}

	result := c.results[0]
	c.results = c.results[1:]

	if result.wait != nil {
		<-result.wait
	}

	return result.msg, result.err
}

func (c *scriptedReadLoopConn) Close() error {
	c.closeCalls.Add(1)
	return nil
}

func (c *scriptedReadLoopConn) RemoteAddr() string {
	return "test"
}
