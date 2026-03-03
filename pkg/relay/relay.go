package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/antoinecorbel7/plex-tunnel/pkg/auth"
	"github.com/antoinecorbel7/plex-tunnel/pkg/tunnel"
)

var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

type Relay struct {
	cfg    Config
	logger zerolog.Logger
	tokens *auth.TokenStore
	router *Router
}

func New(cfg Config, logger zerolog.Logger) (*Relay, error) {
	store, err := auth.LoadTokenStore(cfg.TokensFile)
	if err != nil {
		return nil, fmt.Errorf("load token store: %w", err)
	}

	return &Relay{
		cfg:    cfg,
		logger: logger,
		tokens: store,
		router: NewRouter(),
	}, nil
}

func (r *Relay) Run(ctx context.Context) error {
	publicMux := http.NewServeMux()
	publicMux.HandleFunc("/", r.handleClientRequest)

	tunnelMux := http.NewServeMux()
	tunnelMux.HandleFunc("/tunnel", r.handleTunnel)
	tunnelMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})

	publicServer := &http.Server{
		Addr:    r.cfg.Listen,
		Handler: publicMux,
	}
	tunnelServer := &http.Server{
		Addr:    r.cfg.TunnelListen,
		Handler: tunnelMux,
	}

	errCh := make(chan error, 2)
	go func() {
		r.logger.Info().Str("addr", r.cfg.Listen).Msg("relay http server listening")
		if err := publicServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("relay http server: %w", err)
		}
	}()

	go func() {
		r.logger.Info().Str("addr", r.cfg.TunnelListen).Msg("relay tunnel server listening")
		if err := tunnelServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("relay tunnel server: %w", err)
		}
	}()

	go r.cleanupStaleAgents(ctx)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = publicServer.Shutdown(shutdownCtx)
		_ = tunnelServer.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

func (r *Relay) handleTunnel(w http.ResponseWriter, req *http.Request) {
	conn, err := tunnel.AcceptWebSocket(w, req)
	if err != nil {
		r.logger.Warn().Err(err).Msg("failed to accept tunnel websocket")
		return
	}

	register, err := conn.Receive()
	if err != nil {
		r.logger.Warn().Err(err).Msg("failed to receive register message")
		_ = conn.Close()
		return
	}

	if register.Type != tunnel.MsgRegister {
		_ = conn.Send(tunnel.Message{Type: tunnel.MsgError, Error: "first message must be register"})
		_ = conn.Close()
		return
	}

	subdomain, ok := r.tokens.Validate(register.Token, register.Subdomain)
	if !ok {
		_ = conn.Send(tunnel.Message{Type: tunnel.MsgError, Error: "invalid token or subdomain"})
		_ = conn.Close()
		return
	}

	session := newAgentSession(subdomain, conn, r.logger.With().Str("subdomain", subdomain).Logger())
	previous := r.router.Set(subdomain, session)
	if previous != nil {
		previous.close("superseded by a newer connection")
	}
	defer func() {
		r.router.Delete(subdomain, session)
		session.close("agent disconnected")
		r.logger.Info().Str("subdomain", subdomain).Msg("agent disconnected")
	}()

	if err := conn.Send(tunnel.Message{Type: tunnel.MsgRegisterAck, Subdomain: subdomain}); err != nil {
		r.logger.Warn().Err(err).Str("subdomain", subdomain).Msg("failed to send register ack")
		return
	}

	r.logger.Info().Str("subdomain", subdomain).Str("remote", conn.RemoteAddr()).Msg("agent connected")

	for {
		msg, err := conn.Receive()
		if err != nil {
			r.logger.Warn().Err(err).Str("subdomain", subdomain).Msg("tunnel receive error")
			return
		}

		session.touch()

		switch msg.Type {
		case tunnel.MsgPing:
			if err := conn.Send(tunnel.Message{Type: tunnel.MsgPong}); err != nil {
				r.logger.Warn().Err(err).Str("subdomain", subdomain).Msg("failed to send pong")
				return
			}
		case tunnel.MsgPong:
			// pong also refreshes liveness via touch above.
		case tunnel.MsgHTTPResponse:
			session.deliverResponse(msg)
		case tunnel.MsgError:
			r.logger.Warn().Str("subdomain", subdomain).Str("error", msg.Error).Msg("received agent error")
		default:
			r.logger.Debug().Str("subdomain", subdomain).Uint8("type", uint8(msg.Type)).Msg("ignoring unsupported message")
		}
	}
}

func (r *Relay) handleClientRequest(w http.ResponseWriter, req *http.Request) {
	subdomain, err := r.extractSubdomain(req.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	session, ok := r.router.Get(subdomain)
	if !ok {
		http.Error(w, "Tunnel not connected", http.StatusBadGateway)
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	requestID := uuid.NewString()
	pending := session.addPending(requestID)
	defer session.removePending(requestID)

	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}

	msg := tunnel.Message{
		Type:    tunnel.MsgHTTPRequest,
		ID:      requestID,
		Method:  req.Method,
		Path:    path,
		Headers: cloneAndFilterHeaders(req.Header),
		Body:    body,
	}

	if err := session.conn.Send(msg); err != nil {
		http.Error(w, "Tunnel not connected", http.StatusBadGateway)
		return
	}

	flusher, _ := w.(http.Flusher)
	wroteHeaders := false
	timer := time.NewTimer(r.cfg.RequestTimeout)
	defer timer.Stop()

	for {
		select {
		case <-req.Context().Done():
			return
		case <-session.done:
			if !wroteHeaders {
				http.Error(w, "Tunnel disconnected", http.StatusBadGateway)
			}
			return
		case <-timer.C:
			if !wroteHeaders {
				http.Error(w, "Tunnel request timeout", http.StatusGatewayTimeout)
			}
			return
		case response := <-pending:
			if response.Type == tunnel.MsgError {
				if !wroteHeaders {
					http.Error(w, "Tunnel error", http.StatusBadGateway)
				}
				return
			}

			if !wroteHeaders {
				copyResponseHeaders(w.Header(), response.Headers)
				status := response.Status
				if status == 0 {
					status = http.StatusBadGateway
				}
				w.WriteHeader(status)
				wroteHeaders = true
			}

			if len(response.Body) > 0 {
				if _, err := w.Write(response.Body); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}

			if response.EndStream {
				return
			}
			resetTimer(timer, r.cfg.RequestTimeout)
		}
	}
}

func (r *Relay) cleanupStaleAgents(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			for _, session := range r.router.Snapshot() {
				if now.Sub(session.lastSeen()) > r.cfg.AgentStaleTimeout {
					r.logger.Warn().Str("subdomain", session.subdomain).Msg("removing stale agent session")
					r.router.Delete(session.subdomain, session)
					session.close("stale session timeout")
				}
			}
		}
	}
}

func (r *Relay) extractSubdomain(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.Contains(host, ":") {
		if parsedHost, _, err := net.SplitHostPort(host); err == nil {
			host = parsedHost
		}
	}

	domain := strings.ToLower(strings.TrimSpace(r.cfg.Domain))
	if host == domain {
		return "", fmt.Errorf("missing subdomain in host")
	}

	suffix := "." + domain
	if !strings.HasSuffix(host, suffix) {
		return "", fmt.Errorf("host %q is outside relay domain", host)
	}

	subdomain := strings.TrimSuffix(host, suffix)
	if strings.Contains(subdomain, ".") {
		return "", fmt.Errorf("invalid subdomain")
	}
	if !auth.IsValidSubdomain(subdomain) {
		return "", fmt.Errorf("invalid subdomain")
	}

	return subdomain, nil
}

func cloneAndFilterHeaders(headers http.Header) map[string][]string {
	if len(headers) == 0 {
		return nil
	}

	filtered := make(map[string][]string, len(headers))
	for key, values := range headers {
		canonical := http.CanonicalHeaderKey(key)
		if _, skip := hopByHopHeaders[canonical]; skip {
			continue
		}
		if canonical == "Host" {
			continue
		}

		copied := make([]string, len(values))
		copy(copied, values)
		filtered[canonical] = copied
	}

	return filtered
}

func copyResponseHeaders(dst http.Header, src map[string][]string) {
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if _, skip := hopByHopHeaders[canonical]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func resetTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

type AgentSession struct {
	subdomain string
	conn      *tunnel.WebSocketConnection
	logger    zerolog.Logger

	lastSeenUnixNano atomic.Int64
	done             chan struct{}
	closeOnce        sync.Once
	pending          sync.Map // request id -> chan tunnel.Message
}

func newAgentSession(subdomain string, conn *tunnel.WebSocketConnection, logger zerolog.Logger) *AgentSession {
	session := &AgentSession{
		subdomain: subdomain,
		conn:      conn,
		logger:    logger,
		done:      make(chan struct{}),
	}
	session.touch()
	return session
}

func (s *AgentSession) addPending(requestID string) chan tunnel.Message {
	ch := make(chan tunnel.Message, 32)
	s.pending.Store(requestID, ch)
	return ch
}

func (s *AgentSession) removePending(requestID string) {
	s.pending.Delete(requestID)
}

func (s *AgentSession) deliverResponse(msg tunnel.Message) {
	value, ok := s.pending.Load(msg.ID)
	if !ok {
		return
	}

	ch, ok := value.(chan tunnel.Message)
	if !ok {
		return
	}

	select {
	case ch <- msg:
	case <-s.done:
	}
}

func (s *AgentSession) touch() {
	s.lastSeenUnixNano.Store(time.Now().UnixNano())
}

func (s *AgentSession) lastSeen() time.Time {
	return time.Unix(0, s.lastSeenUnixNano.Load())
}

func (s *AgentSession) close(reason string) {
	s.closeOnce.Do(func() {
		s.logger.Info().Str("reason", reason).Msg("closing session")
		close(s.done)
		_ = s.conn.Close()
	})
}
