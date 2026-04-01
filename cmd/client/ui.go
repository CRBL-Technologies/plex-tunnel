package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/CRBL-Technologies/plex-tunnel/pkg/client"
)

const tokenPlaceholder = "\x00unchanged\x00"

type clientController struct {
	rootCtx context.Context
	logger  zerolog.Logger

	mu     sync.RWMutex
	cfg    client.Config
	runner *client.Client
	cancel context.CancelFunc
	done   chan struct{}
}

func newClientController(rootCtx context.Context, cfg client.Config, logger zerolog.Logger) *clientController {
	return &clientController{
		rootCtx: rootCtx,
		logger:  logger,
		cfg:     cfg,
	}
}

func (c *clientController) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startLocked()
}

func (c *clientController) startLocked() {
	runCtx, cancel := context.WithCancel(c.rootCtx)
	runner := client.New(c.cfg, c.logger)
	done := make(chan struct{})

	c.runner = runner
	c.cancel = cancel
	c.done = done

	go func() {
		defer close(done)
		if err := runner.Run(runCtx); err != nil {
			c.logger.Error().Err(err).Msg("client runner exited with error")
		}
	}()
}

func (c *clientController) Stop() {
	c.mu.Lock()
	cancel := c.cancel
	done := c.done
	c.cancel = nil
	c.done = nil
	c.runner = nil
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (c *clientController) ApplyConfig(cfg client.Config) {
	c.mu.Lock()
	cancel := c.cancel
	done := c.done
	c.cfg = cfg
	c.cancel = nil
	c.done = nil
	c.runner = nil
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.startLocked()
}

func (c *clientController) Snapshot() (client.Config, client.ConnectionStatus) {
	c.mu.RLock()
	cfg := c.cfg
	runner := c.runner
	c.mu.RUnlock()

	status := client.ConnectionStatus{}
	if runner != nil {
		status = runner.SnapshotStatus()
	}

	return cfg, status
}

type uiHandler struct {
	controller *clientController
	logger     zerolog.Logger
}

type statusPageData struct {
	Status      client.ConnectionStatus
	Config      client.Config
	TokenMasked string
	Message     string
	Error       string
}

var statusPageTmpl = template.Must(template.New("status").Funcs(template.FuncMap{
	"formatTime": func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.Format("2006-01-02 15:04:05")
	},
}).Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <meta http-equiv="refresh" content="5">
  <title>Portless Client</title>
  <link rel="icon" type="image/svg+xml" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 36 36'%3E%3Crect width='36' height='36' rx='8' fill='%231a1a2e'/%3E%3Ctext x='50%25' y='54%25' dominant-baseline='central' text-anchor='middle' font-family='system-ui,sans-serif' font-weight='700' font-size='22' fill='%23D97706'%3EP%3C/text%3E%3C/svg%3E">
  <style>
    :root {
      --bg: #FFFBF5;
      --card: #fff;
      --surface: #FFF7ED;
      --border: #E5DDD3;
      --text: #1a1a1a;
      --muted: #6B5E50;
      --accent: #D97706;
      --accent-hover: #B45309;
      --ok-bg: #f0fdf4;
      --ok-border: #b2f2bb;
      --ok-text: #2b8a3e;
      --bad-bg: #fff5f5;
      --bad-border: #ffc9c9;
      --bad-text: #c92a2a;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: system-ui, -apple-system, sans-serif;
      color: var(--text);
      background: var(--bg);
      min-height: 100vh;
    }
    .wrap { max-width: 960px; margin: 24px auto; padding: 0 16px; }
    .panel {
      background: var(--card);
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 18px;
      margin-bottom: 16px;
      box-shadow: 0 10px 30px rgba(15, 23, 42, 0.05);
    }
    h1 {
      margin: 0 0 8px;
      font-size: 1.4rem;
      text-align: center;
    }
    h2 {
      margin: 0 0 12px;
      color: var(--muted);
      font-size: .78rem;
      font-weight: 600;
      letter-spacing: .08em;
      text-transform: uppercase;
    }
    .section-title {
      display: inline-flex;
      align-items: center;
      gap: 8px;
    }
    .info-bubble {
      position: relative;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 18px;
      height: 18px;
      border-radius: 50%;
      border: 1px solid var(--border);
      background: var(--card);
      color: var(--muted);
      font-size: 12px;
      font-weight: 700;
      cursor: help;
      user-select: none;
      outline: none;
    }
    .info-bubble::before {
      content: attr(data-tip);
      position: absolute;
      left: 50%;
      bottom: 130%;
      transform: translateX(-50%);
      min-width: 220px;
      max-width: 320px;
      padding: 8px 10px;
      border-radius: 8px;
      border: 1px solid var(--border);
      background: var(--card);
      color: var(--muted);
      font-size: 12px;
      line-height: 1.35;
      white-space: normal;
      text-align: left;
      opacity: 0;
      pointer-events: none;
      transition: opacity .12s ease-in-out;
      z-index: 20;
      box-shadow: 0 8px 24px rgba(15, 23, 42, 0.08);
    }
    .info-bubble:hover::before,
    .info-bubble:focus::before {
      opacity: 1;
    }
    .row { display: flex; flex-wrap: wrap; gap: 12px; }
    .item { flex: 1 1 220px; }
    .label {
      color: var(--muted);
      font-size: .78rem;
      font-weight: 600;
      letter-spacing: .08em;
      text-transform: uppercase;
      display: inline-flex;
      align-items: center;
      gap: 6px;
      margin-bottom: 4px;
    }
    .value {
      font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      font-size: .95rem;
      word-break: break-word;
    }
    .badge {
      display: inline-block;
      border-radius: 999px;
      padding: 4px 10px;
      font-size: .85rem;
      font-weight: 700;
      border: 1px solid var(--border);
    }
    .ok { background: var(--ok-bg); color: var(--ok-text); border-color: var(--ok-border); }
    .bad { background: var(--bad-bg); color: var(--bad-text); border-color: var(--bad-border); }
    form { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
    .full { grid-column: 1 / -1; flex: 1 1 100%; }
    input {
      width: 100%;
      background: var(--surface);
      color: var(--text);
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 10px 12px;
      font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    }
    button {
      border: 0;
      border-radius: 8px;
      padding: 10px 14px;
      cursor: pointer;
      background: var(--accent);
      color: var(--text);
      font-weight: 700;
      transition: background-color .12s ease-in-out;
    }
    button:hover { background: var(--accent-hover); }
    .msg { margin-top: 10px; font-size: .9rem; color: var(--ok-text); }
    .err { margin-top: 10px; font-size: .9rem; color: var(--bad-text); }
    @media (max-width: 700px) {
      form { grid-template-columns: 1fr; }
    }
  </style>
</head>
<body>
  <div class="wrap">
    <div class="panel">
      <div style="text-align:center;margin-bottom:0.75rem;">
        <svg width="48" height="48" viewBox="0 0 36 36" xmlns="http://www.w3.org/2000/svg" aria-label="Portless">
          <rect width="36" height="36" rx="8" fill="#1a1a2e"/>
          <text x="50%" y="54%" dominant-baseline="central" text-anchor="middle" font-family="system-ui,sans-serif" font-weight="700" font-size="22" fill="#D97706">P</text>
        </svg>
      </div>
      <h1>Portless Client</h1>
      <h2 class="section-title">
        Connection Status
        <span class="info-bubble" tabindex="0" data-tip="Shows live tunnel state. This page auto-refreshes every 5 seconds.">i</span>
      </h2>
      <div class="row">
        <div class="item">
          <span class="label">Status <span class="info-bubble" tabindex="0" data-tip="Current websocket/tunnel state between this client and the server.">i</span></span>
          {{if .Status.Connected}}<span class="badge ok">CONNECTED</span>{{else}}<span class="badge bad">DISCONNECTED</span>{{end}}
        </div>
        <div class="item">
          <span class="label">Server <span class="info-bubble" tabindex="0" data-tip="Remote tunnel server endpoint currently connected.">i</span></span>
          <span class="value">{{.Status.Server}}</span>
        </div>
        <div class="item">
          <span class="label">Subdomain <span class="info-bubble" tabindex="0" data-tip="Assigned subdomain for incoming traffic routing.">i</span></span>
          <span class="value">{{.Status.Subdomain}}</span>
        </div>
        <div class="item">
          <span class="label">Session ID <span class="info-bubble" tabindex="0" data-tip="Logical tunnel session identifier shared across all pooled websocket connections.">i</span></span>
          <span class="value">{{if .Status.SessionID}}{{.Status.SessionID}}{{else}}-{{end}}</span>
        </div>
        <div class="item">
          <span class="label">Active Connections <span class="info-bubble" tabindex="0" data-tip="Currently connected websocket count in the active tunnel session.">i</span></span>
          <span class="value">{{.Status.ActiveConnections}}</span>
        </div>
        <div class="item">
          <span class="label">Pool Size <span class="info-bubble" tabindex="0" data-tip="Server-granted connection pool size for this session.">i</span></span>
          <span class="value">{{.Status.MaxConnections}}</span>
        </div>
        <div class="item">
          <span class="label">Control Connection <span class="info-bubble" tabindex="0" data-tip="Connection index currently responsible for ping/pong and control duties.">i</span></span>
          <span class="value">{{.Status.ControlConnection}}</span>
        </div>
        <div class="item">
          <span class="label">Reconnect Attempt <span class="info-bubble" tabindex="0" data-tip="Number of current retry attempt after a disconnection.">i</span></span>
          <span class="value">{{.Status.ReconnectAttempt}}</span>
        </div>
        <div class="item">
          <span class="label">Last Connected <span class="info-bubble" tabindex="0" data-tip="Timestamp of the last successful connection to server.">i</span></span>
          <span class="value">{{formatTime .Status.LastConnectedAt}}</span>
        </div>
        <div class="item">
          <span class="label">Last Disconnected <span class="info-bubble" tabindex="0" data-tip="Timestamp of the latest disconnect event.">i</span></span>
          <span class="value">{{formatTime .Status.LastDisconnectedAt}}</span>
        </div>
        <div class="item full">
          <span class="label">Last Error <span class="info-bubble" tabindex="0" data-tip="Most recent connection/proxy error reported by the client.">i</span></span>
          <span class="value">{{if .Status.LastError}}{{.Status.LastError}}{{else}}-{{end}}</span>
        </div>
      </div>
    </div>

    <div class="panel">
      <h2 class="section-title">
        Runtime Settings
        <span class="info-bubble" tabindex="0" data-tip="Changes are applied immediately and restart the client connection loop. Keep token and server URL matched with server-side config.">i</span>
      </h2>
      <form method="post" action="/settings">
        <div>
          <span class="label">Server URL <span class="info-bubble" tabindex="0" data-tip="Tunnel websocket endpoint, usually wss://your-subdomain.domain.tld/tunnel.">i</span></span>
          <input name="server_url" value="{{.Config.ServerURL}}" required>
        </div>
        <div>
          <span class="label">Subdomain <span class="info-bubble" tabindex="0" data-tip="Requested client subdomain. Must match token rules on server.">i</span></span>
          <input name="subdomain" value="{{.Config.Subdomain}}">
        </div>
        <div class="full">
          <span class="label">Server Token <span class="info-bubble" tabindex="0" data-tip="Authentication token from server tokens.json. Keep this secret.">i</span></span>
          <input type="password" name="token" value="{{.TokenMasked}}" required>
        </div>
        <div>
          <span class="label">Plex Target <span class="info-bubble" tabindex="0" data-tip="Local Plex URL this client forwards requests to (for host network: http://127.0.0.1:32400).">i</span></span>
          <input name="plex_target" value="{{.Config.PlexTarget}}">
        </div>
        <div>
          <span class="label">Log Level <span class="info-bubble" tabindex="0" data-tip="Logging verbosity: debug, info, warn, error.">i</span></span>
          <input name="log_level" value="{{.Config.LogLevel}}">
        </div>
        <div>
          <span class="label">Max Connections <span class="info-bubble" tabindex="0" data-tip="Requested parallel websocket pool size for protocol v2 sessions. The server may grant a lower value.">i</span></span>
          <input name="max_connections" value="{{.Config.MaxConnections}}">
        </div>
        <div class="full">
          <button type="submit">Apply And Restart Client</button>
        </div>
      </form>
      {{if .Message}}<div class="msg">{{.Message}}</div>{{end}}
      {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
    </div>
  </div>
  <div style="text-align:center;padding:1.5rem 0 0.5rem;font-size:0.8rem;color:var(--muted);">
    A <a href="https://crbl.io" style="color:var(--accent);text-decoration:none;font-weight:600;">CRBL Technologies</a> product
  </div>
</body>
</html>`))

func newUIHandler(controller *clientController, logger zerolog.Logger, password string) http.Handler {
	h := &uiHandler{
		controller: controller,
		logger:     logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", h.handleIndex)
	mux.HandleFunc("/settings", h.handleSettings)
	mux.HandleFunc("/api/status", h.handleStatus)

	if password == "" {
		return mux
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, providedPassword, ok := r.BasicAuth()
		if !ok || username != "admin" || subtle.ConstantTimeCompare([]byte(providedPassword), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Portless Client"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		mux.ServeHTTP(w, r)
	})
}

func (h *uiHandler) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg, status := h.controller.Snapshot()
	masked := tokenPlaceholder
	if cfg.Token == "" {
		masked = ""
	}
	data := statusPageData{
		Status:      status,
		Config:      cfg,
		TokenMasked: masked,
		Message:     strings.TrimSpace(r.URL.Query().Get("message")),
		Error:       strings.TrimSpace(r.URL.Query().Get("error")),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statusPageTmpl.Execute(w, data); err != nil {
		h.logger.Error().Err(err).Msg("render status page")
	}
}

func (h *uiHandler) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	allowed := "http://" + host
	allowedS := "https://" + host
	if origin := r.Header.Get("Origin"); origin != "" {
		if origin != allowed && origin != allowedS {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	} else if referer := r.Header.Get("Referer"); referer != "" {
		parsed, err := url.Parse(referer)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		refererOrigin := parsed.Scheme + "://" + parsed.Host
		if refererOrigin != allowed && refererOrigin != allowedS {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	} else {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if err := r.ParseForm(); err != nil {
		redirectWithMessage(w, r, "", "failed to parse form")
		return
	}

	cfg, _ := h.controller.Snapshot()
	submittedToken := strings.TrimSpace(r.FormValue("token"))
	if submittedToken != "" && submittedToken != tokenPlaceholder {
		cfg.Token = submittedToken
	}
	cfg.ServerURL = strings.TrimSpace(r.FormValue("server_url"))
	cfg.Subdomain = strings.TrimSpace(r.FormValue("subdomain"))
	cfg.PlexTarget = strings.TrimSpace(r.FormValue("plex_target"))
	cfg.LogLevel = strings.TrimSpace(r.FormValue("log_level"))
	if raw := strings.TrimSpace(r.FormValue("max_connections")); raw != "" {
		maxConnections, convErr := strconv.Atoi(raw)
		if convErr != nil || maxConnections < 1 {
			redirectWithMessage(w, r, "", "max connections must be an integer >= 1")
			return
		}
		cfg.MaxConnections = maxConnections
	}

	if cfg.Token == "" {
		redirectWithMessage(w, r, "", "token is required")
		return
	}
	if cfg.ServerURL == "" {
		redirectWithMessage(w, r, "", "server URL is required")
		return
	}
	if parsed, err := url.Parse(cfg.PlexTarget); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		redirectWithMessage(w, r, "", "plex target must be a valid http:// or https:// URL")
		return
	}
	if parsed, err := url.Parse(cfg.ServerURL); err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") {
		redirectWithMessage(w, r, "", "server URL must be a valid ws:// or wss:// URL")
		return
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}

	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		redirectWithMessage(w, r, "", "invalid log level")
		return
	}
	zerolog.SetGlobalLevel(level)

	h.controller.ApplyConfig(cfg)
	h.logger.Info().
		Str("server_url", cfg.ServerURL).
		Str("subdomain", cfg.Subdomain).
		Msg("applied runtime settings from web UI")

	redirectWithMessage(w, r, "settings applied and client restarted", "")
}

func (h *uiHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg, status := h.controller.Snapshot()
	payload := struct {
		Status client.ConnectionStatus `json:"status"`
		Config struct {
			ServerURL      string `json:"server_url"`
			Subdomain      string `json:"subdomain"`
			PlexTarget     string `json:"plex_target"`
			LogLevel       string `json:"log_level"`
			MaxConnections int    `json:"max_connections"`
			TokenSet       bool   `json:"token_set"`
		} `json:"config"`
	}{
		Status: status,
	}
	payload.Config.ServerURL = cfg.ServerURL
	payload.Config.Subdomain = cfg.Subdomain
	payload.Config.PlexTarget = cfg.PlexTarget
	payload.Config.LogLevel = cfg.LogLevel
	payload.Config.MaxConnections = cfg.MaxConnections
	payload.Config.TokenSet = cfg.Token != ""

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(payload)
}

func redirectWithMessage(w http.ResponseWriter, r *http.Request, message string, errMessage string) {
	values := url.Values{}
	if message != "" {
		values.Set("message", message)
	}
	if errMessage != "" {
		values.Set("error", errMessage)
	}

	target := "/"
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
