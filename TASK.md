## Context

Our rosa P0 client fix (cdd72d5, 2026-04-09) removed the 60-second tunnel recycle that was masking a latent bug: `proxyRequestTimeout = 5 * time.Minute` (client.go:24) is a blanket timeout applied to ALL proxied HTTP requests, including SSE `/eventsource/notifications` streams that are indefinite by design. With long-lived tunnels, SSE now survives to the 5-minute wall, generating a guaranteed `context.DeadlineExceeded` → `RecordFailure()` every 5 minutes. After 5 consecutive SSE timeouts (~25 min with no other traffic), the global circuit breaker opens and rejects ALL request types. CEO observed this as a total outage with `consecutive_failures=21`.

## Goal

Ship two surgical fixes so that (A) SSE/long-lived streams are exempt from `proxyRequestTimeout`, and (B) our own `proxyRequestTimeout` deadline is never counted as a circuit breaker failure.

## Constraints / guardrails

- DO NOT delete or modify code not mentioned in this task.
- Client-only changes in `pkg/client/`. No proto changes, no server changes.
- Do not change circuit breaker thresholds, cooldown, or core Allow/RecordSuccess/RecordFailure logic.
- Do not add per-route circuit breakers or any other architectural changes.
- Do not touch the MsgWSOpen/MsgWSFrame/MsgWSClose warning path (pre-existing, unrelated).
- Do NOT cancel or remove the request context deadline after reading response headers — the request context at client.go:310 governs the whole Do + body-read lifetime; cancelling it kills SSE body reads too.

## Tasks

### FIX A: Exempt SSE streams from proxyRequestTimeout

**Approach: split header-timeout from body-timeout.** The HTTP transport already has `ResponseHeaderTimeout: 30s` (config.go:48, applied at client.go:118) which bounds the wait for response headers. We use the parent session context (no deadline) for the HTTP request itself, and enforce `proxyRequestTimeout` only on the body-read loop for non-SSE responses.

- [ ] In the goroutine at client.go:303-316, change the request context creation. Instead of:
  ```go
  reqCtx, reqCancel := context.WithTimeout(ctx, proxyRequestTimeout)
  defer reqCancel()
  if err := c.handleHTTPRequest(reqCtx, session.pool, connRef, request); err != nil {
  ```
  Change to:
  ```go
  reqCtx, reqCancel := context.WithTimeoutCause(ctx, proxyRequestTimeout, errProxyRequestTimeout)
  defer reqCancel()
  if err := c.handleHTTPRequest(ctx, reqCtx, session.pool, connRef, request); err != nil {
  ```
  This passes both the parent context (`ctx`, no deadline) AND the timeout context (`reqCtx`) into `handleHTTPRequest`.

- [ ] Add a package-level sentinel error above the constants:
  ```go
  var errProxyRequestTimeout = errors.New("proxy request timeout")
  ```

- [ ] Change the `handleHTTPRequest` signature to accept both contexts:
  ```go
  func (c *Client) handleHTTPRequest(parentCtx, timeoutCtx context.Context, pool *ConnectionPool, connRef *poolConn, msg tunnel.Message) error {
  ```

- [ ] Inside `handleHTTPRequest`:
  - Use `parentCtx` (no deadline) for `http.NewRequestWithContext` at line ~412. This means the HTTP request is bounded only by `ResponseHeaderTimeout` (30s) for headers, and has no body-read deadline by default. This is safe because dead peers are detected by tunnel ping/pong (client.go:356).
  - After `resp, err := c.client.Do(req)` succeeds and we have response headers, detect SSE:
    ```go
    isSSE := isEventStreamContentType(resp.Header.Get("Content-Type"))
    if isSSE {
        requestLogger.Info().Msg("exempting SSE stream from proxy request timeout")
    }
    ```
  - In the body read loop (the `for { n, readErr := resp.Body.Read(chunk) ... }` block starting at ~line 447): add a check at the top of each iteration. If NOT SSE, check if `timeoutCtx` has expired:
    ```go
    if !isSSE {
        select {
        case <-timeoutCtx.Done():
            return fmt.Errorf("read proxied response body: %w", context.Cause(timeoutCtx))
        default:
        }
    }
    ```
  - This means SSE streams read indefinitely (protected by tunnel ping/pong), while normal requests are still bounded by `proxyRequestTimeout`.

- [ ] Add the helper:
  ```go
  func isEventStreamContentType(contentType string) bool {
      return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")
  }
  ```

### FIX B: Cause-aware circuit breaker failure suppression

Use `context.WithTimeoutCause` with the sentinel `errProxyRequestTimeout` so we can distinguish our own timeout from upstream/session/parent cancellations.

- [ ] At the body-read error path (currently client.go:495-497), where `readErr != nil` triggers `c.circuit.RecordFailure()`, add a guard:
  ```go
  if readErr != nil {
      if errors.Is(readErr, errProxyRequestTimeout) || context.Cause(timeoutCtx) == errProxyRequestTimeout {
          requestLogger.Debug().Err(readErr).Msg("skipping circuit breaker failure for proxy request timeout")
      } else {
          c.circuit.RecordFailure()
      }
      return fmt.Errorf("read proxied response body: %w", readErr)
  }
  ```

- [ ] At the `c.client.Do(req)` error path (currently client.go:436-439), apply the same guard:
  ```go
  if err != nil {
      if errors.Is(err, errProxyRequestTimeout) || context.Cause(timeoutCtx) == errProxyRequestTimeout {
          requestLogger.Debug().Err(err).Msg("skipping circuit breaker failure for proxy request timeout")
      } else {
          c.circuit.RecordFailure()
      }
      requestLogger.Warn().Err(err).Msg("upstream plex request failed")
      return c.sendProxyError(conn, msg.ID, http.StatusBadGateway, "upstream unavailable")
  }
  ```

## Tests

All tests should be in a new file `pkg/client/client_sse_test.go`.

### TestHandleHTTPRequest_SSEStreamExemptFromTimeout

- Start an `httptest` server that serves `Content-Type: text/event-stream` and writes SSE data, waits for a signal channel, then closes.
- Create a Client with a circuit breaker (threshold=1 for easy assertion).
- Call `handleHTTPRequest` with a `parentCtx` (no deadline) and a `timeoutCtx` with a very short deadline (e.g. 100ms via `context.WithTimeoutCause`).
- After 200ms (past the deadline), send the close signal to the SSE server.
- Assert that response chunks were streamed successfully (i.e. the handler did not error due to deadline).
- Assert that the circuit breaker state is still `closed` (no RecordFailure called).

### TestHandleHTTPRequest_ProxyTimeoutDoesNotTripCircuitBreaker

- Start an `httptest` server that serves `Content-Type: application/json` but delays the body for longer than the timeout.
- Call `handleHTTPRequest` with a very short `timeoutCtx` deadline (50ms).
- Assert that the request fails with an error wrapping `errProxyRequestTimeout`.
- Assert that the circuit breaker state is still `closed` — FIX B prevents our own timeout from being counted.

### TestHandleHTTPRequest_RealUpstreamFailureStillTripsCircuitBreaker

Regression test to ensure FIX B doesn't suppress real upstream failures:
- Start an `httptest` server that closes the connection mid-body (or returns and immediately closes).
- Call `handleHTTPRequest` with a long `timeoutCtx` deadline (5s).
- Assert that `RecordFailure()` IS called (circuit breaker state transitions toward open after threshold).

## Acceptance criteria

1. SSE streams (`Content-Type: text/event-stream`) are not killed by `proxyRequestTimeout`.
2. `proxyRequestTimeout` expiry does not call `RecordFailure()` — keyed on the `errProxyRequestTimeout` sentinel cause, NOT bare `context.DeadlineExceeded`.
3. Real upstream failures (connection refused, 5xx, mid-body close) still call `RecordFailure()`.
4. All existing tests pass. New tests pass.
5. `go vet ./...` clean, `go fmt ./...` clean.

## Verification

```
go fmt ./...
go vet ./...
go test ./...
go test -race ./pkg/client/...
make build
```
