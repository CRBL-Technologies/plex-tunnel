package main

import (
	"context"
	"encoding/json"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"

	"github.com/CRBL-Technologies/plex-tunnel/pkg/client"
)

// maskToken returns a masked version of a token for safe display.
// Shows only the last 4 characters, e.g. "****abcd".
func maskToken(token string) string {
	token = strings.TrimSpace(token)
	if len(token) <= 4 {
		return "****"
	}
	return "****" + token[len(token)-4:]
}

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
	controller     *clientController
	logger         zerolog.Logger
	listenAddr     string
	originOverride string
	listenHost     string
	listenPort     string
	bindAll        bool
	auth           authConfig
	sessions       *sessionStore
	loginLimiter   *loginRateLimiter
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
  <title>Portless Client</title>
  <link rel="icon" type="image/svg+xml" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 36 36'%3E%3Crect width='36' height='36' rx='8' fill='%231C1917'/%3E%3Ctext x='50%25' y='50%25' dominant-baseline='central' text-anchor='middle' font-family='Inter,system-ui,sans-serif' font-weight='600' font-size='22' fill='%23D97706'%3EP%3C/text%3E%3C/svg%3E">
  <style>
    :root {
      --bg: #FFFBF5;
      --card: #fff;
      --surface: #FFF7ED;
      --border: #E5DDD3;
      --text: #1C1917;
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
    .full { grid-column: 1 / -1; }
    input, select {
      width: 100%;
      background: var(--surface);
      color: var(--text);
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 10px 12px;
      font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      box-sizing: border-box;
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
    .panel-header {
      display: flex;
      justify-content: flex-end;
      margin-bottom: 8px;
    }
    .logout-form {
      display: block;
    }
    .logout-button {
      padding: 6px 10px;
      font-size: .85rem;
    }
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
      <div class="panel-header">
        <form method="post" action="/logout" class="logout-form">
          <button type="submit" class="logout-button">Sign out</button>
        </form>
      </div>
      <div style="text-align:center;margin-bottom:0.75rem;">
        <svg width="48" height="48" viewBox="0 0 36 36" xmlns="http://www.w3.org/2000/svg" aria-label="Portless">
          <rect width="36" height="36" rx="8" fill="#1C1917"/>
          <text x="50%" y="50%" dominant-baseline="central" text-anchor="middle" font-family="system-ui,sans-serif" font-weight="600" font-size="22" fill="#D97706">P</text>
        </svg>
      </div>
      <h1 style="text-align:center;margin:0 0 8px;font-size:1.4rem;font-weight:600;"><span style="color:#D97706">P</span>ortless Client</h1>
      <h2 class="section-title">
        Connection Status
        <span class="info-bubble" tabindex="0" data-tip="Shows live tunnel state. Status updates every 5 seconds.">i</span>
      </h2>
      <div class="row">
        <div class="item">
          <span class="label">Status <span class="info-bubble" tabindex="0" data-tip="Current websocket/tunnel state between this client and the server.">i</span></span>
          <span id="s-connected">{{if .Status.Connected}}<span class="badge ok">CONNECTED</span>{{else}}<span class="badge bad">DISCONNECTED</span>{{end}}</span>
        </div>
        <div class="item">
          <span class="label">Server <span class="info-bubble" tabindex="0" data-tip="Remote tunnel server endpoint currently connected.">i</span></span>
          <span class="value" id="s-server">{{.Status.Server}}</span>
        </div>
        <div class="item">
          <span class="label">Subdomain <span class="info-bubble" tabindex="0" data-tip="Assigned subdomain for incoming traffic routing.">i</span></span>
          <span class="value" id="s-subdomain">{{.Status.Subdomain}}</span>
        </div>
        <div class="item">
          <span class="label">Session ID <span class="info-bubble" tabindex="0" data-tip="Logical tunnel session identifier shared across all pooled websocket connections.">i</span></span>
          <span class="value" id="s-session">{{if .Status.SessionID}}{{.Status.SessionID}}{{else}}-{{end}}</span>
        </div>
        <div class="item">
          <span class="label">Active Connections <span class="info-bubble" tabindex="0" data-tip="Currently connected websocket count in the active tunnel session.">i</span></span>
          <span class="value" id="s-active">{{.Status.ActiveConnections}}</span>
        </div>
        <div class="item">
          <span class="label">Pool Size <span class="info-bubble" tabindex="0" data-tip="Server-granted connection pool size for this session.">i</span></span>
          <span class="value" id="s-pool">{{.Status.MaxConnections}}</span>
        </div>
        <div class="item">
          <span class="label">Control Connection <span class="info-bubble" tabindex="0" data-tip="Connection index currently responsible for ping/pong and control duties.">i</span></span>
          <span class="value" id="s-control">{{.Status.ControlConnection}}</span>
        </div>
        <div class="item">
          <span class="label">Reconnect Attempt <span class="info-bubble" tabindex="0" data-tip="Number of current retry attempt after a disconnection.">i</span></span>
          <span class="value" id="s-reconnect">{{.Status.ReconnectAttempt}}</span>
        </div>
        <div class="item">
          <span class="label">Last Connected <span class="info-bubble" tabindex="0" data-tip="Timestamp of the last successful connection to server.">i</span></span>
          <span class="value" id="s-last-conn">{{formatTime .Status.LastConnectedAt}}</span>
        </div>
        <div class="item">
          <span class="label">Last Disconnected <span class="info-bubble" tabindex="0" data-tip="Timestamp of the latest disconnect event.">i</span></span>
          <span class="value" id="s-last-disc">{{formatTime .Status.LastDisconnectedAt}}</span>
        </div>
        <div class="item full">
          <span class="label">Last Error <span class="info-bubble" tabindex="0" data-tip="Most recent connection/proxy error reported by the client.">i</span></span>
          <span class="value" id="s-error">{{if .Status.LastError}}{{.Status.LastError}}{{else}}-{{end}}</span>
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
          <input name="subdomain" value="{{if .Config.Subdomain}}{{.Config.Subdomain}}{{else}}{{.Status.Subdomain}}{{end}}">
        </div>
        <div class="full">
          <span class="label">Server Token <span class="info-bubble" tabindex="0" data-tip="Authentication token from server tokens.json. Keep this secret.">i</span></span>
          <input name="token" value="{{.TokenMasked}}" required>
        </div>
        <div>
          <span class="label">Plex Target <span class="info-bubble" tabindex="0" data-tip="Local Plex URL this client forwards requests to (for host network: http://127.0.0.1:32400).">i</span></span>
          <input name="plex_target" value="{{.Config.PlexTarget}}">
        </div>
        <div>
          <span class="label">Log Level <span class="info-bubble" tabindex="0" data-tip="Logging verbosity: debug, info, warn, error.">i</span></span>
          <select name="log_level">
            <option value="debug"{{if eq .Config.LogLevel "debug"}} selected{{end}}>debug</option>
            <option value="info"{{if eq .Config.LogLevel "info"}} selected{{end}}>info</option>
            <option value="warn"{{if eq .Config.LogLevel "warn"}} selected{{end}}>warn</option>
            <option value="error"{{if eq .Config.LogLevel "error"}} selected{{end}}>error</option>
          </select>
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
  <script>
  function fmtTime(s){if(!s||s==="0001-01-01T00:00:00Z")return"-";var d=new Date(s);return d.toISOString().replace("T"," ").slice(0,19)}
  function poll(){
    fetch("/api/status").then(function(r){return r.json()}).then(function(d){
      var s=d.status;
      document.getElementById("s-connected").innerHTML=s.connected?'<span class="badge ok">CONNECTED</span>':'<span class="badge bad">DISCONNECTED</span>';
      document.getElementById("s-server").textContent=s.server||"";
      document.getElementById("s-subdomain").textContent=s.subdomain||"";
      document.getElementById("s-session").textContent=s.session_id||"-";
      document.getElementById("s-active").textContent=s.active_connections;
      document.getElementById("s-pool").textContent=s.max_connections;
      document.getElementById("s-control").textContent=s.control_connection;
      document.getElementById("s-reconnect").textContent=s.reconnect_attempt;
      document.getElementById("s-last-conn").textContent=fmtTime(s.last_connected_at);
      document.getElementById("s-last-disc").textContent=fmtTime(s.last_disconnected_at);
      document.getElementById("s-error").textContent=s.last_error||"-";
    }).catch(function(){});
  }
  setInterval(poll,5000);
  </script>
</body>
</html>`))

func newUIHandler(controller *clientController, logger zerolog.Logger, auth authConfig, sessions *sessionStore, loginLimiter *loginRateLimiter, listenAddr string) http.Handler {
	originOverride := os.Getenv("PLEXTUNNEL_UI_ORIGIN")
	listenHost, listenPort, err := net.SplitHostPort(listenAddr)
	bindAll := false
	if err != nil {
		if originOverride == "" {
			originOverride = "http://" + listenAddr
		}
	} else {
		bindAll = isBindAllHost(listenHost)
	}
	if sessions == nil {
		sessions = newSessionStore(7 * 24 * time.Hour)
	}
	if loginLimiter == nil {
		loginLimiter = newLoginRateLimiter()
	}
	h := &uiHandler{
		controller:     controller,
		logger:         logger,
		listenAddr:     listenAddr,
		originOverride: originOverride,
		listenHost:     listenHost,
		listenPort:     listenPort,
		bindAll:        bindAll,
		auth:           auth,
		sessions:       sessions,
		loginLimiter:   loginLimiter,
	}

	mux := http.NewServeMux()
	mux.Handle("GET /login", http.HandlerFunc(h.handleLoginGet))
	mux.Handle("POST /login", http.HandlerFunc(h.handleLoginPost))
	mux.Handle("POST /logout", http.HandlerFunc(h.handleLogoutPost))
	mux.Handle("GET /{$}", h.requireSession(http.HandlerFunc(h.handleIndex)))
	mux.Handle("POST /settings", h.requireSession(http.HandlerFunc(h.handleSettings)))
	mux.Handle("GET /api/status", h.requireSession(http.HandlerFunc(h.handleStatus)))
	mux.Handle("/metrics", promhttp.HandlerFor(client.MetricsRegistry, promhttp.HandlerOpts{}))

	// Wrap with security headers.
	secured := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})

	return secured
}

func isBindAllHost(host string) bool {
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0", "::", "[::]":
		return true
	default:
		return false
	}
}

func (h *uiHandler) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg, status := h.controller.Snapshot()
	masked := ""
	if cfg.Token != "" {
		masked = maskToken(cfg.Token)
	}
	// Clear raw token before passing to template — only the masked version is needed.
	templateCfg := cfg
	templateCfg.Token = ""
	data := statusPageData{
		Status:      status,
		Config:      templateCfg,
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

	if !h.originAllowed(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	if err := r.ParseForm(); err != nil {
		redirectWithMessage(w, r, "", "failed to parse form")
		return
	}

	cfg, _ := h.controller.Snapshot()
	submittedToken := strings.TrimSpace(r.FormValue("token"))
	if submittedToken != "" && submittedToken != maskToken(cfg.Token) {
		cfg.Token = submittedToken
	}
	cfg.ServerURL = strings.TrimSpace(r.FormValue("server_url"))
	cfg.Subdomain = strings.TrimSpace(r.FormValue("subdomain"))
	cfg.PlexTarget = strings.TrimSpace(r.FormValue("plex_target"))
	cfg.LogLevel = strings.TrimSpace(r.FormValue("log_level"))
	if raw := strings.TrimSpace(r.FormValue("max_connections")); raw != "" {
		maxConnections, convErr := strconv.Atoi(raw)
		if convErr != nil || maxConnections < 1 || maxConnections > 32 {
			redirectWithMessage(w, r, "", "max connections must be between 1 and 32")
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
