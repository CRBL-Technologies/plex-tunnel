package client

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CRBL-Technologies/plex-tunnel-proto/tunnel"
)

const (
	poolJoinStagger  = 150 * time.Millisecond
	poolRepairMaxLag = 4 * time.Second
)

type ConnectionPool struct {
	mu sync.RWMutex

	conns        []*poolConn
	sessionID    string
	subdomain    string
	server       string
	maxConns     int
	controlIndex int
	pingCancel   context.CancelFunc
}

type poolConn struct {
	conn     *tunnel.WebSocketConnection
	index    int
	streams  atomic.Int64
	lastPong atomic.Int64
}

type poolSnapshot struct {
	active       int
	controlIndex int
}

func newConnectionPool(server, subdomain, sessionID string, maxConns int) *ConnectionPool {
	if maxConns < 1 {
		maxConns = 1
	}
	return &ConnectionPool{
		conns:        make([]*poolConn, maxConns),
		sessionID:    sessionID,
		subdomain:    subdomain,
		server:       server,
		maxConns:     maxConns,
		controlIndex: 0,
	}
}

func (p *ConnectionPool) add(index int, conn *tunnel.WebSocketConnection) (*poolConn, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	connRef := &poolConn{
		conn:  conn,
		index: index,
	}
	connRef.lastPong.Store(time.Now().UnixNano())
	p.conns[index] = connRef
	return connRef, index == p.controlIndex
}

func (p *ConnectionPool) remove(index int) (remaining int, promoted *poolConn, controlLost bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if index < 0 || index >= len(p.conns) || p.conns[index] == nil {
		return p.activeCountLocked(), nil, false
	}

	p.conns[index] = nil
	controlLost = index == p.controlIndex

	if controlLost && p.pingCancel != nil {
		p.pingCancel()
		p.pingCancel = nil
	}

	remaining = p.activeCountLocked()
	if remaining == 0 {
		return 0, nil, controlLost
	}
	if !controlLost {
		return remaining, nil, false
	}

	nextIndex := -1
	for i, connRef := range p.conns {
		if connRef == nil {
			continue
		}
		nextIndex = i
		break
	}
	p.controlIndex = nextIndex
	return remaining, p.conns[nextIndex], true
}

func (p *ConnectionPool) replacePingLoop(cancel context.CancelFunc) {
	p.mu.Lock()
	oldCancel := p.pingCancel
	p.pingCancel = cancel
	p.mu.Unlock()

	if oldCancel != nil {
		oldCancel()
	}
}

func (p *ConnectionPool) snapshot() poolSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return poolSnapshot{
		active:       p.activeCountLocked(),
		controlIndex: p.controlIndex,
	}
}

func (p *ConnectionPool) activeCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.activeCountLocked()
}

func (p *ConnectionPool) activeCountLocked() int {
	active := 0
	for _, connRef := range p.conns {
		if connRef != nil {
			active++
		}
	}
	return active
}

func (p *ConnectionPool) close() {
	p.mu.Lock()
	conns := make([]*poolConn, 0, len(p.conns))
	for _, connRef := range p.conns {
		if connRef != nil {
			conns = append(conns, connRef)
		}
	}
	cancel := p.pingCancel
	p.pingCancel = nil
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, connRef := range conns {
		_ = connRef.conn.Close()
	}
}
