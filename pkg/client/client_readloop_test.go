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

func TestTryAcquireStreamSlot_ControlAndDataUseSeparatePools(t *testing.T) {
	client := New(Config{}, zerolog.Nop())

	if cap(client.dataSem) != maxDataStreams {
		t.Fatalf("dataSem capacity = %d, want %d", cap(client.dataSem), maxDataStreams)
	}
	if cap(client.controlSem) != maxControlStreams {
		t.Fatalf("controlSem capacity = %d, want %d", cap(client.controlSem), maxControlStreams)
	}

	releaseControl, ok := client.tryAcquireStreamSlot(true)
	if !ok {
		t.Fatalf("tryAcquireStreamSlot(true) ok = %v, want true", ok)
	}
	if releaseControl == nil {
		t.Fatalf("tryAcquireStreamSlot(true) release = nil, want non-nil")
	}

	releaseData, ok := client.tryAcquireStreamSlot(false)
	if !ok {
		t.Fatalf("tryAcquireStreamSlot(false) ok = %v, want true", ok)
	}
	if releaseData == nil {
		t.Fatalf("tryAcquireStreamSlot(false) release = nil, want non-nil")
	}

	if len(client.controlSem) != 1 {
		t.Fatalf("controlSem length = %d, want 1", len(client.controlSem))
	}
	if len(client.dataSem) != 1 {
		t.Fatalf("dataSem length = %d, want 1", len(client.dataSem))
	}

	releaseControl()
	releaseData()

	if len(client.controlSem) != 0 {
		t.Fatalf("controlSem length after release = %d, want 0", len(client.controlSem))
	}
	if len(client.dataSem) != 0 {
		t.Fatalf("dataSem length after release = %d, want 0", len(client.dataSem))
	}
}

func TestTryAcquireStreamSlot_DataSaturationDoesNotBlockControl(t *testing.T) {
	client := New(Config{}, zerolog.Nop())

	for i := 0; i < maxDataStreams; i++ {
		client.dataSem <- struct{}{}
	}

	release, ok := client.tryAcquireStreamSlot(false)
	if ok {
		t.Fatalf("tryAcquireStreamSlot(false) ok = %v, want false", ok)
	}
	if release != nil {
		t.Fatalf("tryAcquireStreamSlot(false) release = non-nil, want nil")
	}

	release, ok = client.tryAcquireStreamSlot(true)
	if !ok {
		t.Fatalf("tryAcquireStreamSlot(true) ok = %v, want true", ok)
	}
	if release == nil {
		t.Fatalf("tryAcquireStreamSlot(true) release = nil, want non-nil")
	}

	release()

	release, ok = client.tryAcquireStreamSlot(true)
	if !ok {
		t.Fatalf("tryAcquireStreamSlot(true) after release ok = %v, want true", ok)
	}
	if release == nil {
		t.Fatalf("tryAcquireStreamSlot(true) after release = nil, want non-nil")
	}
	release()
}

func TestTryAcquireStreamSlot_ControlSaturationDoesNotBlockData(t *testing.T) {
	client := New(Config{}, zerolog.Nop())

	for i := 0; i < maxControlStreams; i++ {
		client.controlSem <- struct{}{}
	}

	release, ok := client.tryAcquireStreamSlot(true)
	if ok {
		t.Fatalf("tryAcquireStreamSlot(true) ok = %v, want false", ok)
	}
	if release != nil {
		t.Fatalf("tryAcquireStreamSlot(true) release = non-nil, want nil")
	}

	release, ok = client.tryAcquireStreamSlot(false)
	if !ok {
		t.Fatalf("tryAcquireStreamSlot(false) ok = %v, want true", ok)
	}
	if release == nil {
		t.Fatalf("tryAcquireStreamSlot(false) release = nil, want non-nil")
	}
	release()
}

func TestTryAcquireStreamSlot_ReleaseReturnsSlot(t *testing.T) {
	client := New(Config{}, zerolog.Nop())

	releaseData, ok := client.tryAcquireStreamSlot(false)
	if !ok {
		t.Fatalf("tryAcquireStreamSlot(false) ok = %v, want true", ok)
	}
	if releaseData == nil {
		t.Fatalf("tryAcquireStreamSlot(false) release = nil, want non-nil")
	}
	if len(client.dataSem) != 1 {
		t.Fatalf("dataSem length after acquire = %d, want 1", len(client.dataSem))
	}

	releaseData()

	if len(client.dataSem) != 0 {
		t.Fatalf("dataSem length after release = %d, want 0", len(client.dataSem))
	}

	releaseControl, ok := client.tryAcquireStreamSlot(true)
	if !ok {
		t.Fatalf("tryAcquireStreamSlot(true) ok = %v, want true", ok)
	}
	if releaseControl == nil {
		t.Fatalf("tryAcquireStreamSlot(true) release = nil, want non-nil")
	}
	if len(client.controlSem) != 1 {
		t.Fatalf("controlSem length after acquire = %d, want 1", len(client.controlSem))
	}

	releaseControl()

	if len(client.controlSem) != 0 {
		t.Fatalf("controlSem length after release = %d, want 0", len(client.controlSem))
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
