package client

import (
	"bytes"
	"context"
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

type Client struct {
	cfg    Config
	logger zerolog.Logger
	client *http.Client

	stateMu sync.RWMutex
	state   ConnectionStatus
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
	conn, err := tunnel.DialWebSocket(ctx, c.cfg.ServerURL, nil)
	if err != nil {
		return fmt.Errorf("connect server websocket: %w", err)
	}
	defer conn.Close()

	c.updateStatus(func(s *ConnectionStatus) {
		s.Connected = true
		s.Server = conn.RemoteAddr()
		s.LastConnectedAt = time.Now()
		s.LastError = ""
		s.ReconnectAttempt = 0
	})
	c.logger.Info().Str("server", conn.RemoteAddr()).Msg("connected to server")

	if err := conn.Send(tunnel.Message{
		Type:            tunnel.MsgRegister,
		Token:           c.cfg.Token,
		Subdomain:       c.cfg.Subdomain,
		ProtocolVersion: tunnel.ProtocolVersion,
	}); err != nil {
		return fmt.Errorf("Connection failed during handshake. The server may be running an older protocol version. Ensure both client and server are updated. Details: %w", err)
	}

	registerAck, err := conn.Receive()
	if err != nil {
		return fmt.Errorf("Connection failed during handshake. The server may be running an older protocol version. Ensure both client and server are updated. Details: %w", err)
	}
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

	c.updateStatus(func(s *ConnectionStatus) {
		s.Subdomain = registerAck.Subdomain
	})
	c.logger.Info().Str("subdomain", registerAck.Subdomain).Msg("client registered")

	var lastPong atomic.Int64
	lastPong.Store(time.Now().UnixNano())

	errCh := make(chan error, 2)
	go c.readLoop(ctx, conn, &lastPong, errCh)
	go c.pingLoop(ctx, conn, &lastPong, errCh)

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func (c *Client) readLoop(ctx context.Context, conn *tunnel.WebSocketConnection, lastPong *atomic.Int64, errCh chan<- error) {
	for {
		msg, err := conn.Receive()
		if err != nil {
			sendErr(errCh, fmt.Errorf("read loop: %w", err))
			return
		}

		switch msg.Type {
		case tunnel.MsgHTTPRequest:
			go func(request tunnel.Message) {
				if err := c.handleHTTPRequest(ctx, conn, request); err != nil {
					c.logger.Warn().Err(err).Str("request_id", request.ID).Msg("failed to process proxied request")
				}
			}(msg)
		case tunnel.MsgPing:
			if err := conn.Send(tunnel.Message{Type: tunnel.MsgPong}); err != nil {
				sendErr(errCh, fmt.Errorf("send pong: %w", err))
				return
			}
		case tunnel.MsgPong:
			lastPong.Store(time.Now().UnixNano())
		case tunnel.MsgError:
			c.logger.Warn().Str("error", msg.Error).Msg("received server error")
		case tunnel.MsgRegisterAck:
			// Server may re-ack after reconnect or internal events.
			if msg.ProtocolVersion != tunnel.ProtocolVersion {
				sendErr(
					errCh,
					fmt.Errorf(
						"Server requires a different protocol version. Update your client or server. Client protocol version: %d, server protocol version: %d",
						tunnel.ProtocolVersion,
						msg.ProtocolVersion,
					),
				)
				return
			}
			c.logger.Debug().Str("subdomain", msg.Subdomain).Msg("received register ack")
		case tunnel.MsgWSOpen, tunnel.MsgWSFrame, tunnel.MsgWSClose, tunnel.MsgKeyExchange:
			c.logger.Debug().Uint8("type", uint8(msg.Type)).Msg("ignoring unsupported websocket message type")
		default:
			c.logger.Debug().Uint8("type", uint8(msg.Type)).Msg("ignoring unsupported message type")
		}
	}
}

func (c *Client) pingLoop(ctx context.Context, conn *tunnel.WebSocketConnection, lastPong *atomic.Int64, errCh chan<- error) {
	ticker := time.NewTicker(c.cfg.PingInterval)
	defer ticker.Stop()

	// A pong is expected no later than one full ping interval plus timeout.
	disconnectAfter := c.cfg.PingInterval + c.cfg.PongTimeout

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := time.Unix(0, lastPong.Load())
			if time.Since(last) > disconnectAfter {
				sendErr(errCh, fmt.Errorf("pong timeout exceeded (%s)", disconnectAfter))
				return
			}

			if err := conn.Send(tunnel.Message{Type: tunnel.MsgPing}); err != nil {
				sendErr(errCh, fmt.Errorf("send ping: %w", err))
				return
			}
		}
	}
}

func (c *Client) handleHTTPRequest(ctx context.Context, conn *tunnel.WebSocketConnection, msg tunnel.Message) error {
	if msg.ID == "" {
		return fmt.Errorf("request without id")
	}

	targetURL, err := resolveTargetURL(c.cfg.PlexTarget, msg.Path)
	if err != nil {
		return c.sendProxyError(conn, msg.ID, http.StatusBadGateway, fmt.Sprintf("invalid target path: %v", err))
	}

	req, err := http.NewRequestWithContext(ctx, msg.Method, targetURL, bytes.NewReader(msg.Body))
	if err != nil {
		return c.sendProxyError(conn, msg.ID, http.StatusBadGateway, fmt.Sprintf("build proxied request: %v", err))
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
		return c.sendProxyError(conn, msg.ID, http.StatusBadGateway, fmt.Sprintf("request to plex failed: %v", err))
	}
	defer resp.Body.Close()

	chunk := make([]byte, c.cfg.ResponseChunkSize)
	headersSent := false
	for {
		n, readErr := resp.Body.Read(chunk)
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
			if err := conn.Send(responseMsg); err != nil {
				return fmt.Errorf("send response chunk: %w", err)
			}
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

	if err := conn.Send(finalMsg); err != nil {
		return fmt.Errorf("send final response chunk: %w", err)
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

	rel, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("parse request path: %w", err)
	}

	return base.ResolveReference(rel).String(), nil
}

func sendErr(errCh chan<- error, err error) {
	select {
	case errCh <- err:
	default:
	}
}
