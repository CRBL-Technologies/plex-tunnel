package client

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/rs/zerolog"
	"nhooyr.io/websocket"

	"github.com/CRBL-Technologies/plex-tunnel-proto/tunnel"
)

const (
	wsInitialWindowBytes           = 65536
	wsWindowUpdateThreshold        = wsInitialWindowBytes / 2
	wsMaxPendingCredit       int64 = math.MaxInt32
	wsFrameQueueSize               = 2
	wsUpstreamReadLimitBytes       = 8 * 1024 * 1024
)

var (
	errWindowIncrementZero = errors.New("FLOW_CONTROL_ERROR: websocket window increment must be greater than zero")
	errWindowUpdateBody    = errors.New("FLOW_CONTROL_ERROR: websocket window update body must be empty")
	errFlowControlOverflow = errors.New("FLOW_CONTROL_ERROR: websocket pending credit overflow")
)

type wsStreamRegistry struct {
	ctx    context.Context
	client *Client
	pool   *ConnectionPool

	mu      sync.RWMutex
	streams map[string]*wsStream
}

type wsStream struct {
	registry            *wsStreamRegistry
	id                  string
	controlConn         *tunnel.WebSocketConnection
	upstreamConn        *websocket.Conn
	logger              zerolog.Logger
	flowControlEnabled  bool
	ctx                 context.Context
	cancel              context.CancelFunc
	releaseSlot         func()
	frames              chan tunnel.Message
	closeOnce           sync.Once
	creditMu            sync.Mutex
	creditCond          *sync.Cond
	sendCredit          int64
	consumedSinceUpdate int64
	closed              bool
}

func newWSStreamRegistry(ctx context.Context, client *Client, pool *ConnectionPool) *wsStreamRegistry {
	registry := &wsStreamRegistry{
		ctx:     ctx,
		client:  client,
		pool:    pool,
		streams: make(map[string]*wsStream),
	}

	go func() {
		<-ctx.Done()
		registry.closeAll(websocket.StatusGoingAway, "tunnel closed")
	}()

	return registry
}

func (r *wsStreamRegistry) open(ctx context.Context, conn *tunnel.WebSocketConnection, msg tunnel.Message, flowControlEnabled bool) error {
	release, ok := r.client.tryAcquireStreamSlot(true)
	if !ok {
		if err := sendWSError(ctx, conn, msg.ID, "client overloaded"); err != nil {
			return fmt.Errorf("send websocket overload error: %w", err)
		}
		return nil
	}

	targetURL, err := resolveWebSocketTargetURL(r.client.cfg.PlexTarget, msg.Path)
	if err != nil {
		release()
		if sendErr := sendWSError(ctx, conn, msg.ID, err.Error()); sendErr != nil {
			return fmt.Errorf("send websocket target error: %w", sendErr)
		}
		return nil
	}

	upstreamConn, _, err := websocket.Dial(ctx, targetURL, &websocket.DialOptions{
		HTTPHeader: cloneForwardHeaders(msg.Headers),
	})
	if err != nil {
		release()
		if sendErr := sendWSError(ctx, conn, msg.ID, fmt.Sprintf("websocket dial upstream: %v", err)); sendErr != nil {
			return fmt.Errorf("send websocket dial error: %w", sendErr)
		}
		return nil
	}
	upstreamConn.SetReadLimit(wsUpstreamReadLimitBytes)

	streamCtx, cancel := context.WithCancel(r.ctx)
	stream := &wsStream{
		registry:           r,
		id:                 msg.ID,
		controlConn:        conn,
		upstreamConn:       upstreamConn,
		logger:             r.streamLogger(msg.ID, msg.Path),
		flowControlEnabled: flowControlEnabled,
		ctx:                streamCtx,
		cancel:             cancel,
		releaseSlot:        release,
		frames:             make(chan tunnel.Message, wsFrameQueueSize),
		sendCredit:         wsInitialWindowBytes,
	}
	stream.creditCond = sync.NewCond(&stream.creditMu)

	if !r.register(stream) {
		stream.close(websocket.StatusPolicyViolation, "duplicate stream id")
		if err := sendWSError(ctx, conn, msg.ID, "websocket stream already exists"); err != nil {
			return fmt.Errorf("send websocket duplicate-stream error: %w", err)
		}
		return nil
	}

	if err := conn.SendContext(streamCtx, tunnel.Message{
		Type: tunnel.MsgWSOpen,
		ID:   msg.ID,
	}); err != nil {
		stream.close(websocket.StatusGoingAway, "tunnel open ack failed")
		return fmt.Errorf("send websocket open ack: %w", err)
	}

	go stream.upstreamReader()
	go stream.upstreamWriter()
	return nil
}

func (r *wsStreamRegistry) frame(msg tunnel.Message) {
	stream := r.lookup(msg.ID)
	if stream == nil {
		return
	}

	select {
	case <-stream.ctx.Done():
	case stream.frames <- msg:
	}
}

func (r *wsStreamRegistry) close(msg tunnel.Message) {
	stream := r.lookup(msg.ID)
	if stream == nil {
		return
	}

	status := websocket.StatusNormalClosure
	if msg.Status > 0 {
		status = websocket.StatusCode(msg.Status)
	}
	stream.close(status, msg.Error)
}

func (r *wsStreamRegistry) windowUpdate(msg tunnel.Message) error {
	stream := r.lookup(msg.ID)
	if stream == nil {
		return nil
	}
	if msg.WindowIncrement == 0 {
		return errWindowIncrementZero
	}
	if len(msg.Body) != 0 {
		return errWindowUpdateBody
	}
	return stream.addSendCredit(msg.WindowIncrement)
}

func (r *wsStreamRegistry) closeAll(status websocket.StatusCode, reason string) {
	r.mu.RLock()
	streams := make([]*wsStream, 0, len(r.streams))
	for _, stream := range r.streams {
		streams = append(streams, stream)
	}
	r.mu.RUnlock()

	for _, stream := range streams {
		stream.close(status, reason)
	}
}

func (r *wsStreamRegistry) register(stream *wsStream) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.streams[stream.id]; exists {
		return false
	}

	r.streams[stream.id] = stream
	activeStreamsMetric.Inc()
	return true
}

func (r *wsStreamRegistry) deregister(stream *wsStream) {
	r.mu.Lock()
	defer r.mu.Unlock()

	current, exists := r.streams[stream.id]
	if !exists || current != stream {
		return
	}

	delete(r.streams, stream.id)
	activeStreamsMetric.Dec()
}

func (r *wsStreamRegistry) lookup(id string) *wsStream {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.streams[id]
}

func (r *wsStreamRegistry) streamLogger(streamID string, path string) zerolog.Logger {
	logger := r.client.logger.With()
	if r.pool != nil {
		logger = logger.
			Str("subdomain", r.pool.subdomain).
			Str("session_id", r.pool.sessionID)
	}
	return logger.
		Str("tunnel_id", "control").
		Str("route_class", "control").
		Str("stream_id", streamID).
		Str("path", path).
		Logger()
}

func (s *wsStream) upstreamReader() {
	for {
		msgType, data, err := s.upstreamConn.Read(s.ctx)
		if err != nil {
			if s.ctx.Err() != nil {
				s.close(websocket.StatusNormalClosure, "")
				return
			}

			closeMsg := tunnel.Message{
				Type: tunnel.MsgWSClose,
				ID:   s.id,
			}
			if status := websocket.CloseStatus(err); status != -1 {
				closeMsg.Status = int(status)
			}
			if sendErr := s.controlConn.SendContext(s.ctx, closeMsg); sendErr != nil && s.ctx.Err() == nil {
				s.logger.Warn().Err(sendErr).Msg("failed to send websocket close after upstream read error")
			}
			s.close(websocket.StatusGoingAway, "")
			return
		}

		if len(data) == 0 {
			if err := s.sendFrameToTunnel(msgType, nil); err != nil {
				if s.ctx.Err() == nil && !errors.Is(err, context.Canceled) {
					s.logger.Warn().Err(err).Msg("failed to forward websocket frame to tunnel")
				}
				s.close(websocket.StatusGoingAway, "")
				return
			}
			continue
		}

		for start := 0; start < len(data); start += wsInitialWindowBytes {
			end := start + wsInitialWindowBytes
			if end > len(data) {
				end = len(data)
			}
			if err := s.sendFrameToTunnel(msgType, data[start:end]); err != nil {
				if s.ctx.Err() == nil && !errors.Is(err, context.Canceled) {
					s.logger.Warn().Err(err).Msg("failed to forward websocket frame to tunnel")
				}
				s.close(websocket.StatusGoingAway, "")
				return
			}
		}
	}
}

func (s *wsStream) upstreamWriter() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case msg := <-s.frames:
			wsMsgType := websocket.MessageText
			if msg.WSBinary {
				wsMsgType = websocket.MessageBinary
			}

			if err := s.upstreamConn.Write(s.ctx, wsMsgType, msg.Body); err != nil {
				if s.ctx.Err() != nil {
					s.close(websocket.StatusNormalClosure, "")
					return
				}

				closeMsg := tunnel.Message{
					Type: tunnel.MsgWSClose,
					ID:   s.id,
				}
				if status := websocket.CloseStatus(err); status != -1 {
					closeMsg.Status = int(status)
				}
				if sendErr := s.controlConn.SendContext(s.ctx, closeMsg); sendErr != nil && s.ctx.Err() == nil {
					s.logger.Warn().Err(sendErr).Msg("failed to send websocket close after upstream write error")
				}
				s.close(websocket.StatusGoingAway, "")
				return
			}

			if !s.flowControlEnabled {
				continue
			}

			increment := s.accountConsumed(len(msg.Body))
			if increment == 0 {
				continue
			}

			if err := s.controlConn.SendContext(s.ctx, tunnel.Message{
				Type:            tunnel.MsgWSWindowUpdate,
				ID:              s.id,
				WindowIncrement: uint32(increment),
			}); err != nil {
				if s.ctx.Err() == nil {
					s.logger.Warn().Err(err).Msg("failed to send websocket window update")
				}
				s.close(websocket.StatusGoingAway, "")
				return
			}
		}
	}
}

func (s *wsStream) sendFrameToTunnel(msgType websocket.MessageType, body []byte) error {
	if s.flowControlEnabled && !s.waitAndConsumeSendCredit(len(body)) {
		return context.Canceled
	}

	frame := tunnel.Message{
		Type:     tunnel.MsgWSFrame,
		ID:       s.id,
		Body:     append([]byte(nil), body...),
		WSBinary: msgType == websocket.MessageBinary,
	}
	if err := s.controlConn.SendContext(s.ctx, frame); err != nil {
		return fmt.Errorf("send websocket frame: %w", err)
	}
	return nil
}

func (s *wsStream) waitAndConsumeSendCredit(size int) bool {
	required := int64(size)

	s.creditMu.Lock()
	defer s.creditMu.Unlock()

	for !s.closed && s.sendCredit < required {
		s.creditCond.Wait()
	}
	if s.closed {
		return false
	}

	s.sendCredit -= required
	return true
}

func (s *wsStream) addSendCredit(increment uint32) error {
	s.creditMu.Lock()
	defer s.creditMu.Unlock()

	if s.closed {
		return nil
	}

	if s.sendCredit+int64(increment) > wsMaxPendingCredit {
		return errFlowControlOverflow
	}

	s.sendCredit += int64(increment)
	s.creditCond.Broadcast()
	return nil
}

func (s *wsStream) accountConsumed(size int) int64 {
	if size <= 0 {
		return 0
	}

	s.creditMu.Lock()
	defer s.creditMu.Unlock()

	s.consumedSinceUpdate += int64(size)
	if s.consumedSinceUpdate < wsWindowUpdateThreshold {
		return 0
	}

	increment := s.consumedSinceUpdate
	s.consumedSinceUpdate = 0
	return increment
}

func (s *wsStream) close(status websocket.StatusCode, reason string) {
	s.closeOnce.Do(func() {
		s.creditMu.Lock()
		s.closed = true
		s.creditCond.Broadcast()
		s.creditMu.Unlock()

		s.cancel()
		s.registry.deregister(s)
		if s.releaseSlot != nil {
			s.releaseSlot()
			s.releaseSlot = nil
		}

		if status <= 0 {
			status = websocket.StatusNormalClosure
		}
		if err := s.upstreamConn.Close(status, reason); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Debug().Err(err).Msg("websocket upstream close returned error")
		}
	})
}

func sendWSError(ctx context.Context, conn *tunnel.WebSocketConnection, streamID string, reason string) error {
	if conn == nil {
		return nil
	}
	return conn.SendContext(ctx, tunnel.Message{
		Type:  tunnel.MsgError,
		ID:    streamID,
		Error: reason,
	})
}
