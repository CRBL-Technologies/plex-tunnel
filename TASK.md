# TASK: P2 Client Remaining Issues (#38, #39, #44, #45, #46, #47)

## Overview
Six remaining P2 client issues covering: configurable response header timeout, Prometheus metrics, install script checksum verification, mandatory UI password for non-loopback, additional tests, and a circuit breaker for failing upstream Plex.

---

## Change 1: ResponseHeaderTimeout configurable via env var (#38)

The `ResponseHeaderTimeout` field was already added to the `Config` struct. Now wire it up: set the default, parse the env var, and use it in `New()`.

### Where to work
- `pkg/client/config.go` — `LoadConfig()` function (around line 34-47 for default, and after line 116 for env parsing)
- `pkg/client/client.go` — `New()` function (line 103-117), use `cfg.ResponseHeaderTimeout` instead of hardcoded `30 * time.Second`

### Current behavior
- `ResponseHeaderTimeout` field exists on Config but has no default and no env var parsing.
- `New()` hardcodes `ResponseHeaderTimeout: 30 * time.Second` in the http.Transport.

### Desired behavior
- Default `ResponseHeaderTimeout` to `30 * time.Second` in `LoadConfig()`.
- Parse `PLEXTUNNEL_RESPONSE_HEADER_TIMEOUT` env var as a `time.Duration` (same pattern as `PLEXTUNNEL_PING_INTERVAL`).
- In `New()`, use `cfg.ResponseHeaderTimeout` instead of the hardcoded value.

---

## Change 2: Prometheus metrics endpoint (#39)

Add basic Prometheus metrics to the client and expose them on the UI HTTP server.

### Where to work
- `go.mod` — add `github.com/prometheus/client_golang` dependency
- `pkg/client/metrics.go` — new file for metric definitions
- `pkg/client/client.go` — increment metrics in relevant places
- `cmd/client/main.go` — register `/metrics` endpoint on the UI server

### Current behavior
No metrics are exposed.

### Desired behavior
Define these metrics in a new `pkg/client/metrics.go` file:
- `portless_client_active_streams` (Gauge) — current number of active proxy streams. Increment when a stream starts in `handleStream` (or equivalent proxy handler), decrement when it completes.
- `portless_client_requests_total` (CounterVec, labels: `status_code`) — total proxied requests, labeled by HTTP status code. Increment after each proxy response.
- `portless_client_connected` (Gauge, 0 or 1) — whether the client is currently connected to the server. Set to 1 when connected, 0 when disconnected. Update in `updateStatus` calls where `Connected` is set.

In `cmd/client/main.go`, add a `/metrics` endpoint using `promhttp.Handler()` on the existing UI `mux` (around line 452 in `ui.go`, inside `newUIHandler`). The metrics endpoint should be inside the password-protected handler if a password is set.

### Notes
- Run `go get github.com/prometheus/client_golang/prometheus` and `go get github.com/prometheus/client_golang/prometheus/promhttp` to add the dependency.
- Use `promauto` for registering metrics to keep it simple.
- Keep metric names prefixed with `portless_client_`.

---

## Change 3: Install script checksum verification (#44)

Add SHA256 checksum verification to the install script.

### Where to work
- `scripts/install.sh`

### Current behavior
The script downloads the binary and installs it without verifying integrity.

### Desired behavior
After downloading the binary, also download `${BIN}.sha256` from the same release. Verify the binary with `sha256sum -c` (or `shasum -a 256 -c` as fallback for macOS). If verification fails, remove the downloaded binary and exit with an error.

### Notes
- The checksum file format should be: `<hash>  <filename>` (standard sha256sum output format).
- Try `sha256sum` first, fall back to `shasum -a 256` for macOS compatibility.

---

## Change 4: Require UI password for non-loopback binds (#45)

Change from a warning to a fatal error when the UI is bound to a non-loopback address without a password.

### Where to work
- `cmd/client/main.go` — lines 46-48

### Current behavior
```go
if !isLoopbackUIListen(uiListen) && uiPassword == "" {
    logger.Warn().Msg("UI bound to non-loopback address without password — set PLEXTUNNEL_UI_PASSWORD to protect it")
}
```

### Desired behavior
```go
if !isLoopbackUIListen(uiListen) && uiPassword == "" {
    logger.Fatal().Msg("UI bound to non-loopback address without password — set PLEXTUNNEL_UI_PASSWORD to protect it")
}
```

Change `logger.Warn()` to `logger.Fatal()`. That's it — `zerolog.Fatal()` calls `os.Exit(1)` after logging.

---

## Change 5: Additional tests (#46)

Add tests for the UI settings form, pool resize with active streams, and error paths.

### Where to work
- `cmd/client/ui_test.go` — new file
- `pkg/client/pool_test.go` — append new tests

### Tests

#### UI tests (`cmd/client/ui_test.go`):
- **TestUIHandler_IndexPage**: GET `/` returns 200 with HTML containing "Portless Client".
- **TestUIHandler_StatusAPI**: GET `/api/status` returns 200 with valid JSON containing `status` and `config` keys.
- **TestUIHandler_PasswordProtected**: When password is set, requests without Basic Auth return 401. Requests with correct auth return 200.
- **TestUIHandler_SettingsCSRFRejectsNoOrigin**: POST `/settings` with no Origin or Referer header returns 403.
- **TestMaskToken**: Test `maskToken()` with various inputs: empty string returns "****", short token (<=4 chars) returns "****", longer token shows last 4 chars.

#### Pool tests (append to `pkg/client/pool_test.go`):
- **TestPoolResize_ClampsBounds**: Test that Resize clamps to 1 minimum and `maxPoolConnections` (32) maximum.
- **TestPoolResize_ScaleDown_ClosesActiveConnections**: Verify that active connections in removed slots are closed.

### Notes
- For UI tests, create a `clientController` with a mock config and use `httptest` to test handlers directly.
- The `maskToken` function is in `cmd/client/ui.go` — test it from `cmd/client/ui_test.go` in `package main`.
- For pool tests, follow the existing pattern using `testSocketPair` and `newConnectionPool`.

---

## Change 6: Circuit breaker for failing upstream Plex (#47)

Add a simple circuit breaker to fail fast when the upstream Plex server is repeatedly failing.

### Where to work
- `pkg/client/circuit.go` — new file for circuit breaker logic
- `pkg/client/client.go` — integrate circuit breaker into the proxy path
- `pkg/client/circuit_test.go` — new file for circuit breaker tests

### Current behavior
Every request is proxied to Plex regardless of how many consecutive failures have occurred.

### Desired behavior
Implement a simple consecutive-failure circuit breaker:
- Track consecutive proxy failures (non-2xx responses or connection errors to the upstream Plex target).
- After `N` consecutive failures (default 5), enter "open" state and reject requests immediately with a 503 for a cooldown period (default 30 seconds).
- After the cooldown, allow one request through ("half-open"). If it succeeds, reset the counter and close the circuit. If it fails, re-open for another cooldown period.
- On any successful request, reset the failure counter.
- Use `sync.Mutex` for thread safety.
- Log state transitions at Info level.

### Notes
- Keep it simple: a struct with `consecutiveFailures int`, `state string` (closed/open/half-open), `lastFailureTime time.Time`, a mutex, and configurable `threshold int` and `cooldown time.Duration`.
- The circuit breaker should be a field on `Client`, initialized in `New()`.
- Check the circuit before making the proxy request. Record success/failure after.

### Tests (`pkg/client/circuit_test.go`):
- **TestCircuitBreaker_ClosedAfterInit**: New breaker starts closed.
- **TestCircuitBreaker_OpensAfterThreshold**: After N failures, state is open.
- **TestCircuitBreaker_RejectsWhenOpen**: When open, `Allow()` returns false.
- **TestCircuitBreaker_HalfOpenAfterCooldown**: After cooldown, one request is allowed.
- **TestCircuitBreaker_ResetsOnSuccess**: A success resets failures and closes circuit.

---

## Constraints
- DO NOT delete or modify any existing code that is not explicitly mentioned in this task.
- Keep all existing tests passing.
- DO NOT change the `ConnectionStatus` struct.
- DO NOT modify existing test files except `pkg/client/pool_test.go` (append only).
- For metrics, use standard Prometheus client library patterns.
- For the circuit breaker, keep it self-contained — do not import external circuit breaker libraries.

## Verification
```bash
cd /home/dev/github/plex-tunnel
go build ./...
go test ./... -race -count=1
go vet ./...
```

## When Done
Commit all changes with a descriptive commit message. Do NOT modify TASK.md.
