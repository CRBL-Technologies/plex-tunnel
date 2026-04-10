## Context

CEO requires comprehensive inline documentation of all client env vars before dev→main merge. Issue #80 originally just added a docs-page pointer; now we need the full reference inline in the code.

## Goal

Add a comprehensive comment block documenting every PLEXTUNNEL_* environment variable in the two files where they are read: `pkg/client/config.go` (tunnel config) and `cmd/client/main.go` (UI config). Each var gets name, description, default, and whether it's required.

## Constraints / guardrails

- DO NOT delete or modify code not mentioned in this task.
- Only add comments — no functional code changes.
- Do not modify any test files.

## Tasks

- [ ] **config.go env var docs.** In `pkg/client/config.go`, replace the existing one-line comment at line 43 (the `// Client configuration environment variables are documented at:` and URL line) — wait, that comment is in main.go not config.go. In `pkg/client/config.go`, add a comment block above the `LoadConfig` function (currently at line 35). The comment block should document all 12 env vars loaded in this file. Use this exact format:

  ```go
  // LoadConfig reads client configuration from environment variables.
  //
  // Tunnel configuration:
  //   PLEXTUNNEL_TOKEN              (required) Authentication token from the Portless dashboard.
  //   PLEXTUNNEL_SERVER_URL         (required) WebSocket endpoint, e.g. wss://tunnel.example.com/tunnel
  //   PLEXTUNNEL_PLEX_TARGET        (default "http://127.0.0.1:32400") Local Plex address to forward requests to.
  //   PLEXTUNNEL_SUBDOMAIN          (optional) Fixed subdomain to request; if unset, the server assigns one.
  //   PLEXTUNNEL_LOG_LEVEL          (default "info") Log verbosity: debug, info, warn, error.
  //   PLEXTUNNEL_MAX_CONNECTIONS    (default server-assigned) Requested websocket pool size (1–32); server may grant fewer.
  //
  // Advanced / tuning:
  //   PLEXTUNNEL_PING_INTERVAL          (default "30s")    WebSocket ping interval.
  //   PLEXTUNNEL_PONG_TIMEOUT           (default "10s")    Time to wait for pong before treating the connection as dead.
  //   PLEXTUNNEL_MAX_RECONNECT_DELAY    (default "60s")    Maximum backoff between reconnection attempts.
  //   PLEXTUNNEL_RESPONSE_CHUNK_SIZE    (default "65536")  Response body chunk size in bytes (1024–4194304).
  //   PLEXTUNNEL_RESPONSE_HEADER_TIMEOUT (default "30s")   Timeout waiting for Plex response headers.
  //
  // Debug:
  //   PLEXTUNNEL_DEBUG_BANDWIDTH_LOGGING (default "false") Emit per-chunk timing logs; requires log level debug.
  ```

- [ ] **main.go env var docs.** In `cmd/client/main.go`, replace the existing two-line comment at lines 43–44 with a comprehensive block documenting the 5 UI env vars read in this file. Use this exact format:

  ```go
  // Web UI configuration (environment variables):
  //   PLEXTUNNEL_UI_LISTEN       (default "127.0.0.1:9090") Address for the status/settings UI. Set empty to disable.
  //   PLEXTUNNEL_UI_PASSWORD     (optional) Login password. Required when UI binds to a non-loopback address.
  //   PLEXTUNNEL_UI_USERNAME     (optional) Login username. If unset, the login form is password-only.
  //   PLEXTUNNEL_UI_SESSION_TTL  (default "168h") Session cookie lifetime (Go duration).
  //   PLEXTUNNEL_UI_ORIGIN       (optional) Override the expected HTTP Origin for CSRF checks (read in ui.go).
  //
  // For the full configuration reference see https://docs.portless.app/getting-started
  ```

## Tests

No new tests — comment-only changes.

## Acceptance criteria

1. `go build ./...` and `go vet ./...` pass (comments don't break compilation).
2. Every PLEXTUNNEL_* env var used by the client is documented in exactly one of the two comment blocks.
3. The docs-page URL is preserved in main.go.

## Verification

```bash
cd /home/dev/worktrees/paul/plex-tunnel
go build ./...
go vet ./...
go test ./...
```
