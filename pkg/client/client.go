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

	"github.com/CRBL-Technologies/plex-tunnel-proto/tunnel"
)

const (
	maxConcurrentStreams = 128
	maxPoolConnections   = 32
	proxyRequestTimeout  = 5 * time.Minute
)

type Client struct {
	cfg    Config
	logger zerolog.Logger
	client *http.Client

	streamSem chan struct{}
	stateMu   sync.RWMutex
	state     ConnectionStatus
}

type sessionPoolController struct {
	mu     sync.Mutex
	client *Client
	ctx    context.Context
	pool   *ConnectionPool
	errCh  chan<- error
}

func newSessionPoolController(
	client *Client,
	ctx context.Context,
	pool *ConnectionPool,
	errCh chan<- error,
) *sessionPoolController {
	return &sessionPoolController{
		client: client,
		ctx:    ctx,
		pool:   pool,
		errCh:  errCh,
	}
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
		Int("old_max_connections", oldMax).
		Int("new_max_connections", updatedMax).
		Msg("updated tunnel connection pool size")

	if promoted != nil {
		s.client.logger.Info().
			Str("session_id", s.pool.sessionID).
			Int("connection_index", promoted.index).
			Msg("promoted tunnel session control connection")
		s.client.startPoolPingLoop(s.ctx, s.pool, promoted)
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
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
		streamSem: make(chan struct{}, maxConcurrentStreams),
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
	requestedMaxConnections := c.cfg.MaxConnections
	if requestedMaxConnections < 1 {
		requestedMaxConnections = 1
	}

	controlConn, err := tunnel.DialWebSocket(ctx, c.cfg.ServerURL, nil)
	if err != nil {
		return fmt.Errorf("connect server websocket: %w", err)
	}

	c.logger.Info().Str("server", controlConn.RemoteAddr()).Msg("connected to server")

	if err := controlConn.Send(tunnel.Message{
		Type:            tunnel.MsgRegister,
		Token:           c.cfg.Token,
		Subdomain:       c.cfg.Subdomain,
		ProtocolVersion: tunnel.ProtocolVersion,
		MaxConnections:  requestedMaxConnections,
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

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	grantedMax := registerAck.MaxConnections
	if grantedMax > maxPoolConnections {
		grantedMax = maxPoolConnections
	}
	c.cfg.MaxConnections = grantedMax
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
		Int("max_connections", registerAck.MaxConnections).
		Msg("client registered")

	errCh := make(chan error, 1)
	session := newSessionPoolController(c, sessionCtx, pool, errCh)
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
	for {
		msg, err := connRef.conn.Receive()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && connRef.streams.Load() == 0 && ctx.Err() == nil {
				c.logger.Debug().
					Err(err).
					Str("session_id", session.pool.sessionID).
					Int("connection_index", connRef.index).
					Msg("retrying idle tunnel connection after read timeout")
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
			select {
			case c.streamSem <- struct{}{}:
			default:
				c.logger.Warn().Str("request_id", msg.ID).Msg("concurrent stream limit reached, rejecting request")
				_ = c.sendProxyError(connRef.conn, msg.ID, http.StatusServiceUnavailable, "client overloaded")
				continue
			}
			go func(request tunnel.Message) {
				defer func() { <-c.streamSem }()
				connRef.streams.Add(1)
				defer connRef.streams.Add(-1)

				reqCtx, reqCancel := context.WithTimeout(ctx, proxyRequestTimeout)
				defer reqCancel()
				if err := c.handleHTTPRequest(reqCtx, connRef, request); err != nil {
					c.logger.Warn().Err(err).Str("request_id", request.ID).Msg("failed to process proxied request")
				}
			}(msg)
		case tunnel.MsgPing:
			if err := connRef.conn.Send(tunnel.Message{Type: tunnel.MsgPong}); err != nil {
				return fmt.Errorf("send pong: %w", err)
			}
		case tunnel.MsgPong:
			connRef.lastPong.Store(time.Now().UnixNano())
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
			c.logger.Debug().
				Str("subdomain", msg.Subdomain).
				Str("session_id", msg.SessionID).
				Int("connection_index", connRef.index).
				Msg("received register ack")
		case tunnel.MsgMaxConnectionsUpdate:
			capped := msg.MaxConnections
			if capped > maxPoolConnections {
				capped = maxPoolConnections
			}
			session.resize(capped)
		case tunnel.MsgWSOpen, tunnel.MsgWSFrame, tunnel.MsgWSClose, tunnel.MsgKeyExchange:
			c.logger.Debug().Uint8("type", uint8(msg.Type)).Msg("ignoring unsupported websocket message type")
		default:
			c.logger.Debug().Uint8("type", uint8(msg.Type)).Msg("ignoring unsupported message type")
		}
	}
}

func (c *Client) pingLoop(ctx context.Context, conn *tunnel.WebSocketConnection, lastPong *atomic.Int64) error {
	ticker := time.NewTicker(c.cfg.PingInterval)
	defer ticker.Stop()

	// A pong is expected no later than one full ping interval plus timeout.
	disconnectAfter := c.cfg.PingInterval + c.cfg.PongTimeout

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			last := time.Unix(0, lastPong.Load())
			if time.Since(last) > disconnectAfter {
				return fmt.Errorf("pong timeout exceeded (%s)", disconnectAfter)
			}

			if err := conn.Send(tunnel.Message{Type: tunnel.MsgPing}); err != nil {
				return fmt.Errorf("send ping: %w", err)
			}
		}
	}
}

func (c *Client) handleHTTPRequest(ctx context.Context, connRef *poolConn, msg tunnel.Message) error {
	if msg.ID == "" {
		return fmt.Errorf("request without id")
	}
	conn := connRef.conn
	if msg.Method == "" && msg.Path == "" {
		c.logger.Warn().Str("request_id", msg.ID).Msg("rejected continuation request frame (not supported)")
		return c.sendProxyError(conn, msg.ID, http.StatusNotImplemented, "streaming requests not supported")
	}

	targetURL, err := resolveTargetURL(c.cfg.PlexTarget, msg.Path)
	if err != nil {
		c.logger.Warn().Err(err).Str("request_id", msg.ID).Msg("target path resolution failed")
		return c.sendProxyError(conn, msg.ID, http.StatusBadGateway, "bad gateway")
	}

	req, err := http.NewRequestWithContext(ctx, msg.Method, targetURL, bytes.NewReader(msg.Body))
	if err != nil {
		c.logger.Warn().Err(err).Str("request_id", msg.ID).Msg("failed to build proxied request")
		return c.sendProxyError(conn, msg.ID, http.StatusBadGateway, "bad gateway")
	}

	for key, values := range msg.Headers {
		if http.CanonicalHeaderKey(key) == "Host" {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Warn().Err(err).Str("request_id", msg.ID).Msg("upstream plex request failed")
		return c.sendProxyError(conn, msg.ID, http.StatusBadGateway, "upstream unavailable")
	}
	defer resp.Body.Close()

	requestLogger := c.logger.With().
		Str("request_id", msg.ID).
		Str("method", msg.Method).
		Str("path", msg.Path).
		Int("connection_index", connRef.index).
		Logger()

	chunk := make([]byte, c.cfg.ResponseChunkSize)
	headersSent := false
	chunkIndex := 0
	for {
		readStartedAt := time.Now()
		n, readErr := resp.Body.Read(chunk)
		readCompletedAt := time.Now()
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

	return nil
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
	return nil
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
			conn, err = c.joinSessionConnection(ctx, pool)
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
				c.logger.Warn().
					Err(err).
					Str("session_id", pool.sessionID).
					Int("connection_index", index).
					Dur("retry_in", delay).
					Msg("failed to connect tunnel session slot")

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
			c.startPoolPingLoop(session.ctx, pool, connRef)
		} else {
			c.startConnPingLoop(ctx, pool, connRef)
		}

		c.logger.Info().
			Str("session_id", pool.sessionID).
			Int("connection_index", index).
			Bool("control", isControl).
			Msg("tunnel session connection active")

		err := c.readLoop(ctx, session, connRef)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.logger.Warn().
				Err(err).
				Str("session_id", pool.sessionID).
				Int("connection_index", index).
				Bool("control", isControl).
				Msg("tunnel session connection disconnected")
		}

		remaining, promoted, controlLost := pool.remove(index)
		c.syncPoolStatus(pool)
		_ = connRef.conn.Close()

		if controlLost && promoted != nil {
			c.logger.Info().
				Str("session_id", pool.sessionID).
				Int("connection_index", promoted.index).
				Msg("promoted tunnel session control connection")
			c.startPoolPingLoop(session.ctx, pool, promoted)
			c.syncPoolStatus(pool)
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

func (c *Client) startPoolPingLoop(ctx context.Context, pool *ConnectionPool, connRef *poolConn) {
	pingCtx, cancel := context.WithCancel(ctx)
	pool.replacePingLoop(cancel)
	connRef.lastPong.Store(time.Now().UnixNano())

	go func() {
		if err := c.pingLoop(pingCtx, connRef.conn, &connRef.lastPong); err != nil && pingCtx.Err() == nil {
			c.logger.Warn().
				Err(err).
				Str("session_id", pool.sessionID).
				Int("connection_index", connRef.index).
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
		if err := c.pingLoop(pingCtx, connRef.conn, &connRef.lastPong); err != nil && pingCtx.Err() == nil {
			c.logger.Warn().
				Err(err).
				Int("connection_index", connRef.index).
				Msg("connection ping loop failed")
			_ = connRef.conn.Close()
		}
	}()
}

func (c *Client) joinSessionConnection(ctx context.Context, pool *ConnectionPool) (*tunnel.WebSocketConnection, error) {
	conn, err := tunnel.DialWebSocket(ctx, c.cfg.ServerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("connect session websocket: %w", err)
	}

	register := tunnel.Message{
		Type:            tunnel.MsgRegister,
		Token:           c.cfg.Token,
		Subdomain:       pool.subdomain,
		ProtocolVersion: tunnel.ProtocolVersion,
		SessionID:       pool.sessionID,
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

	return conn, nil
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
	if err := registerAck.Validate(); err != nil {
		return fmt.Errorf("server returned invalid register ack: %w", err)
	}
	return nil
}
