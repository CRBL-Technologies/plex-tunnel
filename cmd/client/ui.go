package main

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/antoinecorbel7/plex-tunnel/pkg/client"
)

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
	Status  client.ConnectionStatus
	Config  client.Config
	Message string
	Error   string
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
  <title>PlexTunnel Client UI</title>
  <style>
    :root {
      --bg: #0b0f14;
      --bg-soft: #141b23;
      --card: #101722;
      --border: #2a3648;
      --text: #e7edf5;
      --muted: #9fb0c4;
      --ok: #12b886;
      --bad: #e03131;
      --accent: #4dabf7;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: "Space Grotesk", "Segoe UI", sans-serif;
      color: var(--text);
      background: radial-gradient(circle at 10% 20%, #1a2330, var(--bg) 45%);
      min-height: 100vh;
    }
    .wrap { max-width: 960px; margin: 24px auto; padding: 0 16px; }
    .panel {
      background: linear-gradient(180deg, var(--card), var(--bg-soft));
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 18px;
      margin-bottom: 16px;
    }
    h1 { margin: 0 0 8px; font-size: 1.4rem; }
    h2 { margin: 0 0 12px; font-size: 1.05rem; color: var(--muted); }
    .row { display: flex; flex-wrap: wrap; gap: 12px; }
    .item { flex: 1 1 220px; }
    .label { color: var(--muted); font-size: .85rem; display: block; margin-bottom: 4px; }
    .value { font-family: "IBM Plex Mono", Menlo, monospace; font-size: .95rem; word-break: break-word; }
    .badge {
      display: inline-block;
      border-radius: 999px;
      padding: 4px 10px;
      font-size: .85rem;
      font-weight: 700;
      border: 1px solid var(--border);
    }
    .ok { background: rgba(18,184,134,.15); color: #7ef0cb; border-color: rgba(18,184,134,.45); }
    .bad { background: rgba(224,49,49,.15); color: #ffb1b1; border-color: rgba(224,49,49,.45); }
    form { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
    .full { grid-column: 1 / -1; }
    input {
      width: 100%;
      background: #0d141e;
      color: var(--text);
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 10px 12px;
      font-family: "IBM Plex Mono", Menlo, monospace;
    }
    button {
      border: 0;
      border-radius: 8px;
      padding: 10px 14px;
      cursor: pointer;
      background: var(--accent);
      color: #051321;
      font-weight: 700;
    }
    .msg { margin-top: 10px; font-size: .9rem; color: #94e2ff; }
    .err { margin-top: 10px; font-size: .9rem; color: #ffb1b1; }
    @media (max-width: 700px) {
      form { grid-template-columns: 1fr; }
    }
  </style>
</head>
<body>
  <div class="wrap">
    <div class="panel">
      <h1>PlexTunnel Client</h1>
      <h2>Connection Status (auto-refresh every 5s)</h2>
      <div class="row">
        <div class="item">
          <span class="label">Status</span>
          {{if .Status.Connected}}<span class="badge ok">CONNECTED</span>{{else}}<span class="badge bad">DISCONNECTED</span>{{end}}
        </div>
        <div class="item">
          <span class="label">Server</span>
          <span class="value">{{.Status.Server}}</span>
        </div>
        <div class="item">
          <span class="label">Subdomain</span>
          <span class="value">{{.Status.Subdomain}}</span>
        </div>
        <div class="item">
          <span class="label">Reconnect Attempt</span>
          <span class="value">{{.Status.ReconnectAttempt}}</span>
        </div>
        <div class="item">
          <span class="label">Last Connected</span>
          <span class="value">{{formatTime .Status.LastConnectedAt}}</span>
        </div>
        <div class="item">
          <span class="label">Last Disconnected</span>
          <span class="value">{{formatTime .Status.LastDisconnectedAt}}</span>
        </div>
        <div class="item full">
          <span class="label">Last Error</span>
          <span class="value">{{if .Status.LastError}}{{.Status.LastError}}{{else}}-{{end}}</span>
        </div>
      </div>
    </div>

    <div class="panel">
      <h2>Runtime Settings (applied immediately)</h2>
      <form method="post" action="/settings">
        <div>
          <span class="label">Server URL</span>
          <input name="server_url" value="{{.Config.ServerURL}}" required>
        </div>
        <div>
          <span class="label">Subdomain</span>
          <input name="subdomain" value="{{.Config.Subdomain}}">
        </div>
        <div class="full">
          <span class="label">Server Token</span>
          <input type="password" name="token" value="{{.Config.Token}}" required>
        </div>
        <div>
          <span class="label">Plex Target</span>
          <input name="plex_target" value="{{.Config.PlexTarget}}">
        </div>
        <div>
          <span class="label">Log Level</span>
          <input name="log_level" value="{{.Config.LogLevel}}">
        </div>
        <div class="full">
          <button type="submit">Apply And Restart Client</button>
        </div>
      </form>
      {{if .Message}}<div class="msg">{{.Message}}</div>{{end}}
      {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
    </div>
  </div>
</body>
</html>`))

func newUIHandler(controller *clientController, logger zerolog.Logger) http.Handler {
	h := &uiHandler{
		controller: controller,
		logger:     logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", h.handleIndex)
	mux.HandleFunc("/settings", h.handleSettings)
	mux.HandleFunc("/api/status", h.handleStatus)
	return mux
}

func (h *uiHandler) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg, status := h.controller.Snapshot()
	data := statusPageData{
		Status:  status,
		Config:  cfg,
		Message: strings.TrimSpace(r.URL.Query().Get("message")),
		Error:   strings.TrimSpace(r.URL.Query().Get("error")),
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

	if err := r.ParseForm(); err != nil {
		redirectWithMessage(w, r, "", "failed to parse form")
		return
	}

	cfg, _ := h.controller.Snapshot()
	cfg.Token = strings.TrimSpace(r.FormValue("token"))
	cfg.ServerURL = strings.TrimSpace(r.FormValue("server_url"))
	cfg.Subdomain = strings.TrimSpace(r.FormValue("subdomain"))
	cfg.PlexTarget = strings.TrimSpace(r.FormValue("plex_target"))
	cfg.LogLevel = strings.TrimSpace(r.FormValue("log_level"))

	if cfg.Token == "" {
		redirectWithMessage(w, r, "", "token is required")
		return
	}
	if cfg.ServerURL == "" {
		redirectWithMessage(w, r, "", "server URL is required")
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
			ServerURL  string `json:"server_url"`
			Subdomain  string `json:"subdomain"`
			PlexTarget string `json:"plex_target"`
			LogLevel   string `json:"log_level"`
			TokenSet   bool   `json:"token_set"`
		} `json:"config"`
	}{
		Status: status,
	}
	payload.Config.ServerURL = cfg.ServerURL
	payload.Config.Subdomain = cfg.Subdomain
	payload.Config.PlexTarget = cfg.PlexTarget
	payload.Config.LogLevel = cfg.LogLevel
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
