package agent

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

type Agent struct {
	cfg    Config
	logger zerolog.Logger
	client *http.Client
}

func New(cfg Config, logger zerolog.Logger) *Agent {
	return &Agent{
		cfg:    cfg,
		logger: logger,
		client: &http.Client{},
	}
}

func (a *Agent) Run(ctx context.Context) error {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}

		err := a.runSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			attempt = 0
			continue
		}

		delay := BackoffDelay(attempt, a.cfg.MaxReconnectDelay)
		a.logger.Info().Err(err).Int("attempt", attempt+1).Dur("retry_in", delay).Msg("agent disconnected, reconnecting")
		attempt++

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil
		}
	}
}

func (a *Agent) runSession(ctx context.Context) error {
	conn, err := tunnel.DialWebSocket(ctx, a.cfg.RelayURL, nil)
	if err != nil {
		return fmt.Errorf("connect relay websocket: %w", err)
	}
	defer conn.Close()

	a.logger.Info().Str("relay", conn.RemoteAddr()).Msg("connected to relay")

	if err := conn.Send(tunnel.Message{
		Type:      tunnel.MsgRegister,
		Token:     a.cfg.Token,
		Subdomain: a.cfg.Subdomain,
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

	a.logger.Info().Str("subdomain", registerAck.Subdomain).Msg("agent registered")

	var lastPong atomic.Int64
	lastPong.Store(time.Now().UnixNano())

	errCh := make(chan error, 2)
	go a.readLoop(ctx, conn, &lastPong, errCh)
	go a.pingLoop(ctx, conn, &lastPong, errCh)

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func (a *Agent) readLoop(ctx context.Context, conn *tunnel.WebSocketConnection, lastPong *atomic.Int64, errCh chan<- error) {
	for {
		msg, err := conn.Receive()
		if err != nil {
			sendErr(errCh, fmt.Errorf("read loop: %w", err))
			return
		}

		switch msg.Type {
		case tunnel.MsgHTTPRequest:
			go func(request tunnel.Message) {
				if err := a.handleHTTPRequest(ctx, conn, request); err != nil {
					a.logger.Warn().Err(err).Str("request_id", request.ID).Msg("failed to process proxied request")
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
			a.logger.Warn().Str("error", msg.Error).Msg("received relay error")
		case tunnel.MsgRegisterAck:
			// Relay may re-ack after reconnect or internal events.
			a.logger.Debug().Str("subdomain", msg.Subdomain).Msg("received register ack")
		default:
			a.logger.Debug().Uint8("type", uint8(msg.Type)).Msg("ignoring unsupported message type")
		}
	}
}

func (a *Agent) pingLoop(ctx context.Context, conn *tunnel.WebSocketConnection, lastPong *atomic.Int64, errCh chan<- error) {
	ticker := time.NewTicker(a.cfg.PingInterval)
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
			if time.Since(last) > a.cfg.PongTimeout {
				sendErr(errCh, fmt.Errorf("pong timeout exceeded (%s)", a.cfg.PongTimeout))
				return
			}
		}
	}
}

func (a *Agent) handleHTTPRequest(ctx context.Context, conn *tunnel.WebSocketConnection, msg tunnel.Message) error {
	if msg.ID == "" {
		return fmt.Errorf("request without id")
	}

	targetURL, err := resolveTargetURL(a.cfg.PlexTarget, msg.Path)
	if err != nil {
		return a.sendProxyError(conn, msg.ID, http.StatusBadGateway, fmt.Sprintf("invalid target path: %v", err))
	}

	req, err := http.NewRequestWithContext(ctx, msg.Method, targetURL, bytes.NewReader(msg.Body))
	if err != nil {
		return a.sendProxyError(conn, msg.ID, http.StatusBadGateway, fmt.Sprintf("build proxied request: %v", err))
	}

	for key, values := range msg.Headers {
		if http.CanonicalHeaderKey(key) == "Host" {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return a.sendProxyError(conn, msg.ID, http.StatusBadGateway, fmt.Sprintf("request to plex failed: %v", err))
	}
	defer resp.Body.Close()

	chunk := make([]byte, a.cfg.ResponseChunkSize)
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

func (a *Agent) sendProxyError(conn *tunnel.WebSocketConnection, requestID string, status int, msg string) error {
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
