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
	slotCancels  []context.CancelFunc
	sessionID    string
	subdomain    string
	server       string
	maxConns     int
	controlIndex int
	pingCancel   context.CancelFunc
}

type poolConn struct {
	conn       *tunnel.WebSocketConnection
	index      int
	streams    atomic.Int64
	lastPong   atomic.Int64
	pingCancel context.CancelFunc
}

type poolSnapshot struct {
	active       int
	controlIndex int
	maxConns     int
}

func newConnectionPool(server, subdomain, sessionID string, maxConns int) *ConnectionPool {
	if maxConns < 1 {
		maxConns = 1
	}
	return &ConnectionPool{
		conns:        make([]*poolConn, maxConns),
		slotCancels:  make([]context.CancelFunc, maxConns),
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

	if index < 0 || index >= len(p.conns) {
		return nil, false
	}

	activeBefore := p.activeCountLocked()
	controlMissing := p.controlIndex < 0 || p.controlIndex >= len(p.conns) || p.conns[p.controlIndex] == nil

	connRef := &poolConn{
		conn:  conn,
		index: index,
	}
	connRef.lastPong.Store(time.Now().UnixNano())
	p.conns[index] = connRef
	if controlMissing && activeBefore == 0 {
		p.controlIndex = index
	}
	return connRef, index == p.controlIndex
}

func (p *ConnectionPool) setConnPingCancel(index int, cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if index < 0 || index >= len(p.conns) || p.conns[index] == nil {
		return
	}
	p.conns[index].pingCancel = cancel
}

func (p *ConnectionPool) setSlotCancel(index int, cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if index < 0 || index >= len(p.slotCancels) {
		return
	}
	p.slotCancels[index] = cancel
}

func (p *ConnectionPool) remove(index int) (remaining int, promoted *poolConn, controlLost bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if index < 0 || index >= len(p.conns) || p.conns[index] == nil {
		return p.activeCountLocked(), nil, false
	}

	removedConn := p.conns[index]
	p.conns[index] = nil
	controlLost = index == p.controlIndex

	if removedConn.pingCancel != nil {
		removedConn.pingCancel()
		removedConn.pingCancel = nil
	}

	if controlLost && p.pingCancel != nil {
		p.pingCancel()
		p.pingCancel = nil
	}

	remaining = p.activeCountLocked()
	if remaining == 0 {
		if len(p.conns) > 0 {
			p.controlIndex = 0
		}
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
	if promoted = p.conns[nextIndex]; promoted != nil && promoted.pingCancel != nil {
		promoted.pingCancel()
		promoted.pingCancel = nil
	}
	return remaining, promoted, true
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
		maxConns:     p.maxConns,
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

func (p *ConnectionPool) maxConnections() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.maxConns
}

func (p *ConnectionPool) Resize(newMax int) (oldMax, updatedMax int, promoted *poolConn) {
	if newMax < 1 {
		newMax = 1
	}
	if newMax > maxPoolConnections {
		newMax = maxPoolConnections
	}

	p.mu.Lock()
	oldMax = p.maxConns
	if newMax == oldMax {
		p.mu.Unlock()
		return oldMax, oldMax, nil
	}

	if newMax > oldMax {
		p.conns = append(p.conns, make([]*poolConn, newMax-oldMax)...)
		p.slotCancels = append(p.slotCancels, make([]context.CancelFunc, newMax-oldMax)...)
		p.maxConns = newMax
		p.mu.Unlock()
		return oldMax, newMax, nil
	}

	removedConns := make([]*poolConn, 0, oldMax-newMax)
	removedCancels := make([]context.CancelFunc, 0, oldMax-newMax)
	removedPingCancels := make([]context.CancelFunc, 0, oldMax-newMax)
	var pingCancel context.CancelFunc

	if p.controlIndex >= newMax {
		for i := 0; i < newMax; i++ {
			if p.conns[i] == nil {
				continue
			}
			p.controlIndex = i
			promoted = p.conns[i]
			break
		}
		if promoted == nil {
			p.controlIndex = 0
		}
		pingCancel = p.pingCancel
		p.pingCancel = nil
		if promoted != nil && promoted.pingCancel != nil {
			removedPingCancels = append(removedPingCancels, promoted.pingCancel)
			promoted.pingCancel = nil
		}
	}

	for i := oldMax - 1; i >= newMax; i-- {
		if p.conns[i] != nil {
			if p.conns[i].pingCancel != nil {
				removedPingCancels = append(removedPingCancels, p.conns[i].pingCancel)
				p.conns[i].pingCancel = nil
			}
			removedConns = append(removedConns, p.conns[i])
			p.conns[i] = nil
		}
		if p.slotCancels[i] != nil {
			removedCancels = append(removedCancels, p.slotCancels[i])
			p.slotCancels[i] = nil
		}
	}

	p.conns = p.conns[:newMax]
	p.slotCancels = p.slotCancels[:newMax]
	p.maxConns = newMax
	p.mu.Unlock()

	if pingCancel != nil {
		pingCancel()
	}
	for _, cancel := range removedPingCancels {
		cancel()
	}
	for _, cancel := range removedCancels {
		cancel()
	}
	for _, connRef := range removedConns {
		// Wait briefly for active streams to finish before closing.
		for i := 0; i < 50; i++ {
			if connRef.streams.Load() == 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = connRef.conn.Close()
	}

	return oldMax, newMax, promoted
}

func (p *ConnectionPool) close() {
	p.mu.Lock()
	conns := make([]*poolConn, 0, len(p.conns))
	connPingCancels := make([]context.CancelFunc, 0, len(p.conns))
	for _, connRef := range p.conns {
		if connRef != nil {
			if connRef.pingCancel != nil {
				connPingCancels = append(connPingCancels, connRef.pingCancel)
				connRef.pingCancel = nil
			}
			conns = append(conns, connRef)
		}
	}
	slotCancels := make([]context.CancelFunc, 0, len(p.slotCancels))
	for _, cancel := range p.slotCancels {
		if cancel != nil {
			slotCancels = append(slotCancels, cancel)
		}
	}
	cancel := p.pingCancel
	p.pingCancel = nil
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, cancel := range connPingCancels {
		cancel()
	}
	for _, cancel := range slotCancels {
		cancel()
	}
	for _, connRef := range conns {
		_ = connRef.conn.Close()
	}
}
