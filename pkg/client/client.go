package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/antoinecorbel7/plex-tunnel/pkg/tunnel"
)

type Client struct {
	cfg    Config
	logger zerolog.Logger
	client *http.Client
}

func New(cfg Config, logger zerolog.Logger) *Client {
	return &Client{
		cfg:    cfg,
		logger: logger,
		client: &http.Client{},
	}
}

func (c *Client) Run(ctx context.Context) error {
	attempt := 0
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
	conn, err := tunnel.DialWebSocket(ctx, c.cfg.RelayURL, nil)
	if err != nil {
		return fmt.Errorf("connect relay websocket: %w", err)
	}
	defer conn.Close()

	c.logger.Info().Str("relay", conn.RemoteAddr()).Msg("connected to relay")

	if err := conn.Send(tunnel.Message{
		Type:      tunnel.MsgRegister,
		Token:     c.cfg.Token,
		Subdomain: c.cfg.Subdomain,
	}); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	registerAck, err := conn.Receive()
	if err != nil {
		return fmt.Errorf("receive register ack: %w", err)
	}
	if registerAck.Type == tunnel.MsgError {
		return fmt.Errorf("relay rejected registration: %s", registerAck.Error)
	}
	if registerAck.Type != tunnel.MsgRegisterAck {
		return fmt.Errorf("unexpected first message type after register: %d", registerAck.Type)
	}

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
			c.logger.Warn().Str("error", msg.Error).Msg("received relay error")
		case tunnel.MsgRegisterAck:
			// Relay may re-ack after reconnect or internal events.
			c.logger.Debug().Str("subdomain", msg.Subdomain).Msg("received register ack")
		default:
			c.logger.Debug().Uint8("type", uint8(msg.Type)).Msg("ignoring unsupported message type")
		}
	}
}

func (c *Client) pingLoop(ctx context.Context, conn *tunnel.WebSocketConnection, lastPong *atomic.Int64, errCh chan<- error) {
	ticker := time.NewTicker(c.cfg.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := conn.Send(tunnel.Message{Type: tunnel.MsgPing}); err != nil {
				sendErr(errCh, fmt.Errorf("send ping: %w", err))
				return
			}

			last := time.Unix(0, lastPong.Load())
			if time.Since(last) > c.cfg.PongTimeout {
				sendErr(errCh, fmt.Errorf("pong timeout exceeded (%s)", c.cfg.PongTimeout))
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
