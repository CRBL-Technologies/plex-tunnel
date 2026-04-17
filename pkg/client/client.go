package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"nhooyr.io/websocket"

	"github.com/CRBL-Technologies/plex-tunnel-proto/tunnel"
)

var errProxyRequestTimeout = errors.New("proxy request timeout")

const (
	// maxDataStreams caps concurrent data-path requests (downloads, media).
	// Preserves the pre-split 128-slot capacity for downloads.
	maxDataStreams = 128
	// maxControlStreams caps concurrent control-path requests (browsing,
	// SSE, metadata). Kept small but generous - the long-lived SSE stream
	// occupies one slot and browsing bursts are typically <20 concurrent.
	maxControlStreams   = 32
	maxPoolConnections  = 32
	proxyRequestTimeout = 5 * time.Minute

	controlKeepaliveFailureThreshold = 3
)

type Client struct {
	cfg     Config
	logger  zerolog.Logger
	client  *http.Client
	circuit *circuitBreaker

	dataSem    chan struct{}
	controlSem chan struct{}
	stateMu    sync.RWMutex
	state      ConnectionStatus
}

type sessionPoolController struct {
	mu                             sync.Mutex
	client                         *Client
	ctx                            context.Context
	pool                           *ConnectionPool
	errCh                          chan<- error
	consecutiveControlPingFailures atomic.Int32
	wsFlowControlEnabled           atomic.Bool
	wsRegistry                     *wsStreamRegistry
}

func newSessionPoolController(
	client *Client,
	ctx context.Context,
	pool *ConnectionPool,
	errCh chan<- error,
	flowControlEnabled bool,
) *sessionPoolController {
	controller := &sessionPoolController{
		client: client,
		ctx:    ctx,
		pool:   pool,
		errCh:  errCh,
	}
	controller.wsFlowControlEnabled.Store(flowControlEnabled)
	controller.wsRegistry = newWSStreamRegistry(ctx, client, pool)
	return controller
}

func (s *sessionPoolController) wsFlowControl() bool {
	return s.wsFlowControlEnabled.Load()
}

func (s *sessionPoolController) reconcileWSFlowControlAck(logger zerolog.Logger, capabilities uint32) {
	acked := capabilities&tunnel.CapWSFlowControl != 0
	current := s.wsFlowControlEnabled.Load()
	if acked == current {
		return
	}

	logger.Warn().
		Bool("current_ws_flow_control_enabled", current).
		Bool("ack_ws_flow_control_enabled", acked).
		Msg("ignoring websocket flow-control capability change on register ack")
}

func (s *sessionPoolController) startSlot(index int, initialConn *tunnel.WebSocketConnection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startSlotLocked(index, initialConn)
}

func (s *sessionPoolController) startSlotLocked(index int, initialConn *tunnel.WebSocketConnection) {
	slotCtx, cancel := context.WithCancel(s.ctx)
	s.pool.setSlotCancel(index, cancel)
	go s.client.maintainPoolSlot(slotCtx, s, index, initialConn)
}

func (s *sessionPoolController) resize(newMax int) {
	s.mu.Lock()
	oldMax, updatedMax, promoted := s.pool.Resize(newMax)
	if updatedMax == oldMax {
		s.mu.Unlock()
		return
	}

	s.client.logger.Info().
		Str("subdomain", s.pool.subdomain).
		Str("session_id", s.pool.sessionID).
		Int("old_max_connections", oldMax).
		Int("new_max_connections", updatedMax).
		Msg("updated tunnel connection pool size")

	if promoted != nil {
		promotedLogger := s.client.slotLogger(s.pool, promoted.index)
		promotedLogger.Info().
			Int("promoted_from_index", promoted.index).
			Msg("promoted tunnel session control connection")
		s.client.startPoolPingLoop(s.ctx, s, s.pool, promoted)
	}

	for index := oldMax; index < updatedMax; index++ {
		s.startSlotLocked(index, nil)
	}
	s.mu.Unlock()

	s.client.syncPoolStatus(s.pool)
	s.client.updateStatus(func(status *ConnectionStatus) {
		status.MaxConnections = updatedMax
	})
}

func New(cfg Config, logger zerolog.Logger) *Client {
	return &Client{
		cfg:    cfg,
		logger: logger,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
			},
		},
		circuit:    newCircuitBreaker(circuitBreakerDefaultThreshold, circuitBreakerDefaultCooldown, logger),
		dataSem:    make(chan struct{}, maxDataStreams),
		controlSem: make(chan struct{}, maxControlStreams),
	}
}

func (c *Client) SnapshotStatus() ConnectionStatus {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.state
}

func (c *Client) updateStatus(fn func(*ConnectionStatus)) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	fn(&c.state)
	setConnectedMetric(c.state.Connected)
}

func (c *Client) Run(ctx context.Context) error {
	attempt := 0
	c.updateStatus(func(s *ConnectionStatus) {
		s.Connected = false
		s.ReconnectAttempt = 0
		s.LastError = ""
	})
	for {
		if ctx.Err() != nil {
			return nil
		}

		err := c.runSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			attempt = 0
			continue
		}

		delay := BackoffDelay(attempt, c.cfg.MaxReconnectDelay)
		c.updateStatus(func(s *ConnectionStatus) {
			s.Connected = false
			s.LastError = err.Error()
			s.ReconnectAttempt = attempt + 1
			s.LastDisconnectedAt = time.Now()
		})
		c.logger.Info().Err(err).Int("attempt", attempt+1).Dur("retry_in", delay).Msg("client disconnected, reconnecting")
		attempt++

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil
		}
	}
}

func (c *Client) runSession(ctx context.Context) error {
	if strings.HasPrefix(c.cfg.ServerURL, "ws://") {
		return fmt.Errorf("refusing to connect over unencrypted ws:// — tunnel token would be sent in plaintext; use wss:// instead")
	}

	controlConn, err := tunnel.DialTunnelWebSocket(ctx, c.cfg.ServerURL, nil)
	if err != nil {
		return fmt.Errorf("connect server websocket: %w", err)
	}

	c.logger.Info().Str("server", controlConn.RemoteAddr()).Msg("connected to server")

	if err := controlConn.Send(tunnel.Message{
		Type:            tunnel.MsgRegister,
		Token:           c.cfg.Token,
		Subdomain:       c.cfg.Subdomain,
		ProtocolVersion: tunnel.ProtocolVersion,
		MaxConnections:  c.cfg.MaxConnections,
		Capabilities:    tunnel.CapLeasedPool | tunnel.CapWSFlowControl,
	}); err != nil {
		_ = controlConn.Close()
		return fmt.Errorf("Connection failed during handshake. The server may be running an older protocol version. Ensure both client and server are updated. Details: %w", err)
	}

	registerAck, err := controlConn.Receive()
	if err != nil {
		_ = controlConn.Close()
		return fmt.Errorf("Connection failed during handshake. The server may be running an older protocol version. Ensure both client and server are updated. Details: %w", err)
	}
	if err := validateRegisterAck(registerAck); err != nil {
		_ = controlConn.Close()
		return err
	}
	flowControlEnabled := wsFlowControlFromAck(registerAck)

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	grantedMax := registerAck.MaxConnections
	if grantedMax > maxPoolConnections {
		grantedMax = maxPoolConnections
	}
	pool := newConnectionPool(controlConn.RemoteAddr(), registerAck.Subdomain, registerAck.SessionID, grantedMax)
	defer pool.close()

	c.updateStatus(func(s *ConnectionStatus) {
		s.Connected = true
		s.Server = controlConn.RemoteAddr()
		s.Subdomain = registerAck.Subdomain
		s.SessionID = registerAck.SessionID
		s.MaxConnections = registerAck.MaxConnections
		s.ActiveConnections = 0
		s.ControlConnection = 0
		s.LastConnectedAt = time.Now()
		s.LastError = ""
		s.ReconnectAttempt = 0
	})
	c.logger.Info().
		Str("subdomain", registerAck.Subdomain).
		Str("session_id", registerAck.SessionID).
		Str("tunnel_id", "control").
		Str("route_class", "control").
		Int("max_connections", registerAck.MaxConnections).
		Msg("client registered")

	errCh := make(chan error, 1)
	session := newSessionPoolController(c, sessionCtx, pool, errCh, flowControlEnabled)
	for index := 0; index < registerAck.MaxConnections; index++ {
		var initialConn *tunnel.WebSocketConnection
		if index == 0 {
			initialConn = controlConn
		}
		session.startSlot(index, initialConn)
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func (c *Client) readLoop(ctx context.Context, session *sessionPoolController, connRef *poolConn) error {
	return c.readLoopWithConnection(ctx, session, connRef, connRef.conn)
}

func (c *Client) readLoopWithConnection(ctx context.Context, session *sessionPoolController, connRef *poolConn, conn tunnel.Connection) error {
	for {
		msg, err := conn.Receive()
		if err != nil {
			// A quiet Receive during an active stream is not a teardown signal.
			// Lane health is owned by the pong watchdog in pingLoop, so readLoop
			// must keep retrying websocket read deadlines while the parent context
			// is still alive. This avoids the 2026-04-08 staging lane teardown.
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				sinceLastPongMs := int64(-1)
				if lastPong := connRef.lastPong.Load(); lastPong > 0 {
					sinceLastPongMs = time.Since(time.Unix(0, lastPong)).Milliseconds()
				}
				c.logger.Debug().
					Err(err).
					Str("session_id", session.pool.sessionID).
					Int("connection_index", connRef.index).
					Int64("streams", connRef.streams.Load()).
					Int64("since_last_pong_ms", sinceLastPongMs).
					Msg("retrying tunnel connection after read timeout")
				select {
				case <-time.After(100 * time.Millisecond):
				case <-ctx.Done():
					return ctx.Err()
				}
				continue
			}
			return fmt.Errorf("read loop: %w", err)
		}

		switch msg.Type {
		case tunnel.MsgHTTPRequest:
			isControl := ClassifyRequest(msg.Method, msg.Path) == RouteClassControl
			release, ok := c.tryAcquireStreamSlot(isControl)
			if !ok {
				routeClass := "data"
				if isControl {
					routeClass = "control"
				}
				c.logger.Warn().
					Str("request_id", msg.ID).
					Str("route_class", routeClass).
					Msg("concurrent stream limit reached, rejecting request")
				_ = c.sendProxyError(connRef.conn, msg.ID, http.StatusServiceUnavailable, "client overloaded")
				continue
			}
			go func(request tunnel.Message) {
				defer release()
				connRef.streams.Add(1)
				activeStreamsMetric.Inc()
				defer connRef.streams.Add(-1)
				defer activeStreamsMetric.Dec()

				reqCtx, reqCancel := context.WithTimeoutCause(ctx, proxyRequestTimeout, errProxyRequestTimeout)
				defer reqCancel()
				if err := c.handleHTTPRequest(ctx, reqCtx, session.pool, connRef, request); err != nil {
					requestLogger := c.requestLogger(session.pool, connRef, request)
					requestLogger.Warn().Err(err).Msg("failed to process proxied request")
				}
			}(msg)
		case tunnel.MsgPing:
			if err := conn.Send(tunnel.Message{Type: tunnel.MsgPong}); err != nil {
				return fmt.Errorf("send pong: %w", err)
			}
		case tunnel.MsgPong:
			connRef.lastPong.Store(time.Now().UnixNano())
			if session.pool.IsControlSlot(connRef.index) {
				session.consecutiveControlPingFailures.Store(0)
			}
		case tunnel.MsgError:
			c.logger.Warn().Str("error", msg.Error).Msg("received server error")
		case tunnel.MsgRegisterAck:
			// Server may re-ack after reconnect or internal events.
			if msg.ProtocolVersion != tunnel.ProtocolVersion {
				return fmt.Errorf(
					"Server requires a different protocol version. Update your client or server. Client protocol version: %d, server protocol version: %d",
					tunnel.ProtocolVersion,
					msg.ProtocolVersion,
				)
			}
			if msg.Capabilities&tunnel.CapLeasedPool == 0 {
				return fmt.Errorf("server did not acknowledge leased-pool capability; refusing to use legacy data plane")
			}
			ackLogger := c.slotLogger(session.pool, connRef.index)
			session.reconcileWSFlowControlAck(ackLogger, msg.Capabilities)
			ackLogger.Info().Msg("received register ack")
		case tunnel.MsgMaxConnectionsUpdate:
			capped := msg.MaxConnections
			if capped > maxPoolConnections {
				capped = maxPoolConnections
			}
			session.resize(capped)
		case tunnel.MsgWSOpen:
			if c.dropWSMessageIfNotControl(session, connRef, msg) {
				continue
			}
			if err := session.wsRegistry.open(ctx, connRef.conn, msg, session.wsFlowControl()); err != nil {
				return err
			}
		case tunnel.MsgWSFrame:
			if c.dropWSMessageIfNotControl(session, connRef, msg) {
				continue
			}
			if len(msg.Body) > wsInitialWindowBytes {
				return fmt.Errorf("received oversize websocket frame body: %d > %d", len(msg.Body), wsInitialWindowBytes)
			}
			session.wsRegistry.frame(msg)
		case tunnel.MsgWSClose:
			if c.dropWSMessageIfNotControl(session, connRef, msg) {
				continue
			}
			session.wsRegistry.close(msg)
		case tunnel.MsgWSWindowUpdate:
			if c.dropWSMessageIfNotControl(session, connRef, msg) {
				continue
			}
			if !session.wsFlowControl() {
				slotLogger := c.slotLogger(session.pool, connRef.index)
				slotLogger.Warn().
					Str("stream_id", msg.ID).
					Msg("received websocket window update while flow control is disabled")
				continue
			}
			if err := session.wsRegistry.windowUpdate(msg); err != nil {
				if sendErr := connRef.conn.Send(tunnel.Message{
					Type:  tunnel.MsgError,
					ID:    msg.ID,
					Error: err.Error(),
				}); sendErr != nil {
					return fmt.Errorf("send websocket flow-control error: %w", sendErr)
				}
				if sendErr := connRef.conn.Send(tunnel.Message{
					Type:   tunnel.MsgWSClose,
					ID:     msg.ID,
					Status: int(websocket.StatusPolicyViolation),
				}); sendErr != nil {
					return fmt.Errorf("send websocket flow-control close: %w", sendErr)
				}
				session.wsRegistry.close(tunnel.Message{
					Type:   tunnel.MsgWSClose,
					ID:     msg.ID,
					Status: int(websocket.StatusPolicyViolation),
				})
			}
		case tunnel.MsgKeyExchange:
			c.logger.Warn().Uint8("type", uint8(msg.Type)).Msg("received unsupported message type — server may require a client update")
		default:
			c.logger.Warn().Uint8("type", uint8(msg.Type)).Msg("received unknown message type — server may require a client update")
		}
	}
}

func (c *Client) pingLoop(ctx context.Context, conn tunnel.Connection, lastPong *atomic.Int64, consecutiveFailures *atomic.Int32) error {
	ticker := time.NewTicker(c.cfg.PingInterval)
	defer ticker.Stop()

	// A pong is expected no later than one full ping interval plus timeout.
	disconnectAfter := c.cfg.PingInterval + c.cfg.PongTimeout
	lastObservedPong := lastPong.Load()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			currentLastPong := lastPong.Load()
			if currentLastPong != lastObservedPong {
				lastObservedPong = currentLastPong
				if consecutiveFailures != nil {
					consecutiveFailures.Store(0)
				}
			}

			last := time.Unix(0, currentLastPong)
			if time.Since(last) > disconnectAfter {
				if consecutiveFailures == nil {
					return fmt.Errorf("pong timeout exceeded (%s)", disconnectAfter)
				}
				failures := consecutiveFailures.Add(1)
				if failures >= controlKeepaliveFailureThreshold {
					return fmt.Errorf("control pong timeout exceeded after %d missed pongs (%s)", failures, disconnectAfter)
				}
			}

			if err := conn.Send(tunnel.Message{Type: tunnel.MsgPing}); err != nil {
				return fmt.Errorf("send ping: %w", err)
			}
		}
	}
}

func (c *Client) handleHTTPRequest(parentCtx, timeoutCtx context.Context, pool *ConnectionPool, connRef *poolConn, msg tunnel.Message) error {
	if msg.ID == "" {
		return fmt.Errorf("request without id")
	}
	conn := connRef.conn
	requestLogger := c.requestLogger(pool, connRef, msg)
	if msg.Method == "" && msg.Path == "" {
		requestLogger.Warn().Msg("rejected continuation request frame (not supported)")
		return c.sendProxyError(conn, msg.ID, http.StatusNotImplemented, "streaming requests not supported")
	}

	targetURL, err := resolveTargetURL(c.cfg.PlexTarget, msg.Path)
	if err != nil {
		requestLogger.Warn().Err(err).Msg("target path resolution failed")
		return c.sendProxyError(conn, msg.ID, http.StatusBadGateway, "bad gateway")
	}

	req, err := http.NewRequestWithContext(parentCtx, msg.Method, targetURL, bytes.NewReader(msg.Body))
	if err != nil {
		requestLogger.Warn().Err(err).Msg("failed to build proxied request")
		return c.sendProxyError(conn, msg.ID, http.StatusBadGateway, "bad gateway")
	}

	for key, values := range cloneForwardHeaders(msg.Headers) {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if !c.circuit.Allow() {
		requestLogger.Info().Msg("rejecting proxied request while circuit breaker is open")
		return c.sendProxyError(conn, msg.ID, http.StatusServiceUnavailable, "upstream temporarily unavailable")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		if errors.Is(err, errProxyRequestTimeout) || context.Cause(timeoutCtx) == errProxyRequestTimeout || parentCtx.Err() != nil {
			if parentCtx.Err() != nil {
				requestLogger.Debug().Err(err).Msg("skipping circuit breaker failure for parent context cancellation")
			} else {
				requestLogger.Debug().Err(err).Msg("skipping circuit breaker failure for proxy request timeout")
			}
		} else {
			c.circuit.RecordFailure()
		}
		requestLogger.Warn().Err(err).Msg("upstream plex request failed")
		return c.sendProxyError(conn, msg.ID, http.StatusBadGateway, "upstream unavailable")
	}
	defer resp.Body.Close()
	upstreamFailure := resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices
	isSSE := isEventStreamContentType(resp.Header.Get("Content-Type"))
	if isSSE {
		requestLogger.Info().Msg("exempting SSE stream from proxy request timeout")
	}

	chunk := make([]byte, c.cfg.ResponseChunkSize)
	headersSent := false
	chunkIndex := 0
	for {
		readStartedAt := time.Now()
		readCompletedAt := readStartedAt
		var n int
		var readErr error
		if isSSE {
			n, readErr = resp.Body.Read(chunk)
			readCompletedAt = time.Now()
		} else {
			readResultCh := make(chan struct {
				n           int
				err         error
				completedAt time.Time
			}, 1)
			go func() {
				n, err := resp.Body.Read(chunk)
				readResultCh <- struct {
					n           int
					err         error
					completedAt time.Time
				}{
					n:           n,
					err:         err,
					completedAt: time.Now(),
				}
			}()

			select {
			case readResult := <-readResultCh:
				n = readResult.n
				readErr = readResult.err
				readCompletedAt = readResult.completedAt
			case <-timeoutCtx.Done():
				requestLogger.Debug().Err(context.Cause(timeoutCtx)).Msg("skipping circuit breaker failure for proxy request timeout")
				_ = resp.Body.Close()
				return fmt.Errorf("read proxied response body: %w", context.Cause(timeoutCtx))
			}
		}
		if n > 0 {
			responseMsg := tunnel.Message{
				Type: tunnel.MsgHTTPResponse,
				ID:   msg.ID,
				Body: append([]byte(nil), chunk[:n]...),
			}
			if !headersSent {
				responseMsg.Status = resp.StatusCode
				responseMsg.Headers = tunnel.CloneHeaders(resp.Header)
				headersSent = true
			}
			var sendTiming tunnel.SendTiming
			if c.cfg.DebugBandwidthLog {
				sendTiming, err = conn.SendWithTiming(responseMsg)
			} else {
				err = conn.Send(responseMsg)
			}
			if err != nil {
				if upstreamFailure {
					c.circuit.RecordFailure()
				} else {
					c.circuit.RecordSuccess()
				}
				return fmt.Errorf("send response chunk: %w", err)
			}
			if c.cfg.DebugBandwidthLog {
				requestLogger.Debug().
					Int("chunk_index", chunkIndex).
					Int("bytes", n).
					Bool("end_stream", responseMsg.EndStream).
					Int("status", responseMsg.Status).
					Int64("plex_read_ms", readCompletedAt.Sub(readStartedAt).Milliseconds()).
					Int64("tunnel_write_ms", sendTiming.Total().Milliseconds()).
					Int64("write_lock_wait_ms", sendTiming.WriteLockWait.Milliseconds()).
					Int64("frame_encode_ms", sendTiming.FrameEncode.Milliseconds()).
					Int64("ws_write_ms", sendTiming.WebSocketWrite.Milliseconds()).
					Msg("proxied response chunk timing")
			}
			chunkIndex++
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if errors.Is(readErr, errProxyRequestTimeout) || context.Cause(timeoutCtx) == errProxyRequestTimeout || parentCtx.Err() != nil {
				if parentCtx.Err() != nil {
					requestLogger.Debug().Err(readErr).Msg("skipping circuit breaker failure for parent context cancellation")
				} else {
					requestLogger.Debug().Err(readErr).Msg("skipping circuit breaker failure for proxy request timeout")
				}
			} else {
				c.circuit.RecordFailure()
			}
			return fmt.Errorf("read proxied response body: %w", readErr)
		}
	}

	finalMsg := tunnel.Message{Type: tunnel.MsgHTTPResponse, ID: msg.ID, EndStream: true}
	if !headersSent {
		finalMsg.Status = resp.StatusCode
		finalMsg.Headers = tunnel.CloneHeaders(resp.Header)
	}

	var finalSendTiming tunnel.SendTiming
	if c.cfg.DebugBandwidthLog {
		finalSendTiming, err = conn.SendWithTiming(finalMsg)
	} else {
		err = conn.Send(finalMsg)
	}
	if err != nil {
		if upstreamFailure {
			c.circuit.RecordFailure()
		} else {
			c.circuit.RecordSuccess()
		}
		return fmt.Errorf("send final response chunk: %w", err)
	}
	if c.cfg.DebugBandwidthLog {
		requestLogger.Debug().
			Int("chunk_index", chunkIndex).
			Int("bytes", len(finalMsg.Body)).
			Bool("end_stream", finalMsg.EndStream).
			Int("status", finalMsg.Status).
			Int64("tunnel_write_ms", finalSendTiming.Total().Milliseconds()).
			Int64("write_lock_wait_ms", finalSendTiming.WriteLockWait.Milliseconds()).
			Int64("frame_encode_ms", finalSendTiming.FrameEncode.Milliseconds()).
			Int64("ws_write_ms", finalSendTiming.WebSocketWrite.Milliseconds()).
			Msg("proxied response chunk timing")
	}

	if upstreamFailure {
		c.circuit.RecordFailure()
	} else {
		c.circuit.RecordSuccess()
	}
	observeProxyResponse(resp.StatusCode)

	return nil
}

func (c *Client) requestLogger(pool *ConnectionPool, connRef *poolConn, msg tunnel.Message) zerolog.Logger {
	logger := c.slotLogger(pool, connRef.index).With().
		Str("request_id", msg.ID).
		Str("path", msg.Path).
		Str("method", msg.Method)
	return logger.Logger()
}

func (c *Client) slotLogger(pool *ConnectionPool, index int) zerolog.Logger {
	logger := c.logger.With()
	if pool != nil {
		logger = logger.Str("subdomain", pool.subdomain).
			Str("session_id", pool.sessionID)
	}
	routeClass := "data"
	tunnelID := fmt.Sprintf("%d", index)
	if pool != nil && pool.IsControlSlot(index) {
		routeClass = "control"
		tunnelID = "control"
	}
	logger = logger.Str("tunnel_id", tunnelID).
		Str("route_class", routeClass)
	return logger.Logger()
}

// dropWSMessageIfNotControl enforces the ADR 0001 rule that tunneled
// WebSocket traffic (MsgWSOpen / MsgWSFrame / MsgWSClose / MsgWSWindowUpdate)
// rides only on the control-lane tunnel connection. A server-side routing
// bug that delivers a WS message on a data-lane connection is logged and
// dropped — the session is kept alive because tearing it down would amplify
// a server misroute into a client-side reconnect storm.
func (c *Client) dropWSMessageIfNotControl(session *sessionPoolController, connRef *poolConn, msg tunnel.Message) bool {
	if session.pool.IsControlSlot(connRef.index) {
		return false
	}
	slotLogger := c.slotLogger(session.pool, connRef.index)
	slotLogger.Warn().
		Uint8("type", uint8(msg.Type)).
		Str("stream_id", msg.ID).
		Msg("dropping websocket message received on data lane")
	return true
}

func (c *Client) sendProxyError(conn *tunnel.WebSocketConnection, requestID string, status int, msg string) error {
	errMsg := tunnel.Message{
		Type:   tunnel.MsgHTTPResponse,
		ID:     requestID,
		Status: status,
		Headers: map[string][]string{
			"Content-Type": {"text/plain; charset=utf-8"},
		},
		Body:      []byte(msg),
		EndStream: true,
	}
	if sendErr := conn.Send(errMsg); sendErr != nil {
		return fmt.Errorf("send proxy error: %w", sendErr)
	}
	observeProxyResponse(status)
	return nil
}

// tryAcquireStreamSlot attempts to take a slot from the control or data
// stream semaphore. Control-lane traffic (browsing, SSE, metadata) is
// gated by controlSem; all other traffic is gated by dataSem. A saturated
// dataSem MUST NOT block controlSem, which is the entire point of the
// split - see #89.
//
// On success, returns a release func that MUST be called exactly once
// when the request completes. On saturation, returns (nil, false).
func (c *Client) tryAcquireStreamSlot(isControl bool) (release func(), ok bool) {
	sem := c.dataSem
	if isControl {
		sem = c.controlSem
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	default:
		return nil, false
	}
}

func resolveTargetURL(baseTarget string, path string) (string, error) {
	base, err := url.Parse(baseTarget)
	if err != nil {
		return "", fmt.Errorf("parse base target: %w", err)
	}
	if path == "" {
		path = "/"
	}

	rel, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("parse request path: %w", err)
	}
	if rel.Scheme != "" || rel.Host != "" || !strings.HasPrefix(rel.Path, "/") {
		return "", fmt.Errorf("blocked: path must be a relative path")
	}

	return base.ResolveReference(rel).String(), nil
}

func resolveWebSocketTargetURL(baseTarget string, path string) (string, error) {
	targetURL, err := resolveTargetURL(baseTarget, path)
	if err != nil {
		return "", err
	}

	parsed, err := url.Parse(targetURL)
	if err != nil {
		return "", fmt.Errorf("parse resolved target: %w", err)
	}

	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported plex target scheme %q", parsed.Scheme)
	}

	return parsed.String(), nil
}

func cloneForwardHeaders(headers map[string][]string) http.Header {
	if len(headers) == 0 {
		return nil
	}

	filtered := make(http.Header, len(headers))
	for key, values := range headers {
		canonical := http.CanonicalHeaderKey(key)
		switch canonical {
		case "Host", "Connection", "Keep-Alive", "Proxy-Authorization",
			"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
			continue
		}

		copied := make([]string, len(values))
		copy(copied, values)
		filtered[canonical] = copied
	}

	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func isEventStreamContentType(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")
}

func sendErr(errCh chan<- error, err error) {
	select {
	case errCh <- err:
	default:
	}
}

func (c *Client) maintainPoolSlot(
	ctx context.Context,
	session *sessionPoolController,
	index int,
	initialConn *tunnel.WebSocketConnection,
) {
	pool := session.pool
	conn := initialConn
	attempt := 0

	if conn == nil && index > 0 {
		select {
		case <-time.After(time.Duration(index) * poolJoinStagger):
		case <-ctx.Done():
			return
		}
	}

	for {
		if ctx.Err() != nil {
			return
		}

		if conn == nil {
			var err error
			conn, err = c.joinSessionConnection(ctx, pool, session.wsFlowControl())
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if pool.activeCount() == 0 {
					sendErr(session.errCh, fmt.Errorf("all tunnel connections lost: %w", err))
					return
				}

				delay := BackoffDelay(attempt, poolRepairMaxLag)
				attempt++
				slotLogger := c.slotLogger(pool, index)
				slotLogger.Warn().Err(err).Dur("retry_in", delay).Msg("failed to connect tunnel session slot")

				select {
				case <-time.After(delay):
					continue
				case <-ctx.Done():
					return
				}
			}
			attempt = 0
		}

		if ctx.Err() != nil {
			_ = conn.Close()
			return
		}

		connRef, isControl := pool.add(index, conn)
		if connRef == nil {
			_ = conn.Close()
			return
		}
		c.syncPoolStatus(pool)

		if isControl {
			c.startPoolPingLoop(session.ctx, session, pool, connRef)
		} else {
			c.startConnPingLoop(ctx, pool, connRef)
		}

		slotLogger := c.slotLogger(pool, index)
		slotLogger.Info().Msg("tunnel session connection active")

		err := c.readLoop(ctx, session, connRef)
		if ctx.Err() != nil {
			return
		}
		if isControl {
			session.wsRegistry.closeAll(websocket.StatusGoingAway, "control tunnel closed")
		}
		if err != nil {
			slotLogger := c.slotLogger(pool, index)
			slotLogger.Warn().Err(err).Msg("tunnel session connection disconnected")
		}

		remaining, promoted, controlLost := pool.remove(index)
		c.syncPoolStatus(pool)
		_ = connRef.conn.Close()

		if controlLost && promoted != nil {
			promotedLogger := c.slotLogger(pool, promoted.index)
			promotedLogger.Info().
				Int("promoted_from_index", promoted.index).
				Msg("promoted tunnel session control connection")
			c.startPoolPingLoop(session.ctx, session, pool, promoted)
			c.syncPoolStatus(pool)
		}
		if controlLost && promoted == nil && remaining > 0 {
			sendErr(session.errCh, fmt.Errorf("control lost with no idle promotion candidate"))
			return
		}

		if remaining == 0 {
			sendErr(session.errCh, fmt.Errorf("all tunnel connections lost"))
			return
		}

		conn = nil
		delay := BackoffDelay(attempt, poolRepairMaxLag)
		attempt++
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
	}
}

func (c *Client) startPoolPingLoop(ctx context.Context, session *sessionPoolController, pool *ConnectionPool, connRef *poolConn) {
	pingCtx, cancel := context.WithCancel(ctx)
	pool.replacePingLoop(cancel)
	connRef.lastPong.Store(time.Now().UnixNano())
	session.consecutiveControlPingFailures.Store(0)

	go func() {
		if err := c.pingLoop(pingCtx, connRef.conn, &connRef.lastPong, &session.consecutiveControlPingFailures); err != nil && pingCtx.Err() == nil {
			slotLogger := c.slotLogger(pool, connRef.index)
			slotLogger.Warn().
				Err(err).
				Int32("consecutive_failures", session.consecutiveControlPingFailures.Load()).
				Msg("control connection ping loop failed")
			_ = connRef.conn.Close()
		}
	}()
}

func (c *Client) startConnPingLoop(ctx context.Context, pool *ConnectionPool, connRef *poolConn) {
	pingCtx, cancel := context.WithCancel(ctx)
	pool.setConnPingCancel(connRef.index, cancel)
	connRef.lastPong.Store(time.Now().UnixNano())

	go func() {
		if err := c.pingLoop(pingCtx, connRef.conn, &connRef.lastPong, nil); err != nil && pingCtx.Err() == nil {
			slotLogger := c.slotLogger(pool, connRef.index)
			slotLogger.Warn().Err(err).Msg("connection ping loop failed")
			_ = connRef.conn.Close()
		}
	}()
}

func (c *Client) joinSessionConnection(ctx context.Context, pool *ConnectionPool, expectedWSFlowControl bool) (*tunnel.WebSocketConnection, error) {
	conn, err := tunnel.DialTunnelWebSocket(ctx, c.cfg.ServerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("connect session websocket: %w", err)
	}

	register := tunnel.Message{
		Type:            tunnel.MsgRegister,
		Token:           c.cfg.Token,
		Subdomain:       pool.subdomain,
		ProtocolVersion: tunnel.ProtocolVersion,
		SessionID:       pool.sessionID,
		Capabilities:    tunnel.CapLeasedPool | tunnel.CapWSFlowControl,
	}
	if err := conn.Send(register); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send session join register: %w", err)
	}

	registerAck, err := conn.Receive()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("receive session join register ack: %w", err)
	}
	if err := validateRegisterAck(registerAck); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if registerAck.SessionID != pool.sessionID {
		_ = conn.Close()
		return nil, fmt.Errorf("server returned mismatched session id %q for join %q", registerAck.SessionID, pool.sessionID)
	}
	expectedMaxConnections := pool.maxConnections()
	if registerAck.MaxConnections != expectedMaxConnections {
		_ = conn.Close()
		return nil, fmt.Errorf("server returned mismatched max connections %d, want %d", registerAck.MaxConnections, expectedMaxConnections)
	}
	if registerAck.Subdomain != pool.subdomain {
		_ = conn.Close()
		return nil, fmt.Errorf("server returned mismatched subdomain %q for session %q", registerAck.Subdomain, pool.subdomain)
	}
	if wsFlowControlFromAck(registerAck) != expectedWSFlowControl {
		_ = conn.Close()
		return nil, fmt.Errorf("server join ack diverged on websocket flow-control capability (session=%t, join=%t)", expectedWSFlowControl, wsFlowControlFromAck(registerAck))
	}

	return conn, nil
}

// wsFlowControlFromAck reports whether the given RegisterAck advertises
// CapWSFlowControl. Extracted so the derivation is unit-testable and shared
// between runSession (which latches the session-level flag) and
// joinSessionConnection (which rejects divergent join acks).
func wsFlowControlFromAck(registerAck tunnel.Message) bool {
	return registerAck.Capabilities&tunnel.CapWSFlowControl != 0
}

func (c *Client) syncPoolStatus(pool *ConnectionPool) {
	snapshot := pool.snapshot()
	c.updateStatus(func(s *ConnectionStatus) {
		s.Connected = snapshot.active > 0
		s.Server = pool.server
		s.Subdomain = pool.subdomain
		s.SessionID = pool.sessionID
		s.ActiveConnections = snapshot.active
		s.MaxConnections = snapshot.maxConns
		s.ControlConnection = snapshot.controlIndex
	})
}

func validateRegisterAck(registerAck tunnel.Message) error {
	if registerAck.Type == tunnel.MsgError {
		if strings.Contains(strings.ToLower(registerAck.Error), "unsupported tunnel protocol version") {
			return fmt.Errorf("Server requires a different protocol version. Update your client or server. Client protocol version: %d", tunnel.ProtocolVersion)
		}
		return fmt.Errorf("server rejected registration: %s", registerAck.Error)
	}
	if registerAck.Type != tunnel.MsgRegisterAck {
		return fmt.Errorf("unexpected first message type after register: %d", registerAck.Type)
	}
	if registerAck.ProtocolVersion != tunnel.ProtocolVersion {
		return fmt.Errorf(
			"Server requires a different protocol version. Update your client or server. Client protocol version: %d, server protocol version: %d",
			tunnel.ProtocolVersion,
			registerAck.ProtocolVersion,
		)
	}
	if registerAck.Capabilities&tunnel.CapLeasedPool == 0 {
		return fmt.Errorf("server did not acknowledge leased-pool capability; refusing to use legacy data plane")
	}
	if err := registerAck.Validate(); err != nil {
		return fmt.Errorf("server returned invalid register ack: %w", err)
	}
	return nil
}
