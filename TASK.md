## Context

Our rosa P0 client fix (cdd72d5, 2026-04-09) removed the 60-second tunnel recycle that was masking a latent bug: `proxyRequestTimeout = 5 * time.Minute` (client.go:24) is a blanket timeout applied to ALL proxied HTTP requests, including SSE `/eventsource/notifications` streams that are indefinite by design. With long-lived tunnels, SSE now survives to the 5-minute wall, generating a guaranteed `context.DeadlineExceeded` → `RecordFailure()` every 5 minutes. After 5 consecutive SSE timeouts (~25 min with no other traffic), the global circuit breaker opens and rejects ALL request types — including `/media/` and `/library/` — for 30 seconds. The half-open probe catches another SSE retry, which times out, and the circuit never recovers. CEO observed this as a total outage with `consecutive_failures=21`.

## Goal

Ship two surgical fixes so that (A) SSE/long-lived streams are exempt from `proxyRequestTimeout`, and (B) our own `proxyRequestTimeout` deadline is never counted as a circuit breaker failure.

## Constraints / guardrails

- DO NOT delete or modify code not mentioned in this task.
- Client-only changes in `pkg/client/`. No proto changes, no server changes.
- Do not change circuit breaker thresholds, cooldown, or core Allow/RecordSuccess/RecordFailure logic.
- Do not add per-route circuit breakers or any other architectural changes.
- Do not touch the MsgWSOpen/MsgWSFrame/MsgWSClose warning path (pre-existing, unrelated).

## Tasks

### FIX A: Exempt SSE streams from proxyRequestTimeout

- [ ] In `handleHTTPRequest` (client.go:395), after `resp, err := c.client.Do(req)` succeeds (line 435) and before the body read loop (line 447), detect if the response is an SSE stream by checking `resp.Header.Get("Content-Type")` for `text/event-stream` (use `strings.HasPrefix` or `strings.Contains` to handle charset suffixes like `text/event-stream; charset=utf-8`).
- [ ] If the response IS an SSE stream, cancel the `proxyRequestTimeout` deadline by calling `reqCancel()` — but the cancel func is not accessible inside `handleHTTPRequest` because it's created in the caller at line 310. **Solution:** instead of cancelling, create the request context differently. In `handleHTTPRequest`, after detecting SSE, replace `ctx` with a new context derived from the parent (non-deadline) context. The cleanest approach: pass the parent session context alongside the deadline context, OR wrap the body-read section in a new context without the deadline. Pick the simplest approach — the key requirement is that `resp.Body.Read()` at line 449 is no longer bound by the 5-minute deadline for SSE streams.
- [ ] Log at Info level when SSE deadline exemption is applied: `"exempting SSE stream from proxy request timeout"` with the request_id.

### FIX B: Do not count proxyRequestTimeout as circuit breaker failure

- [ ] At client.go:495-497, where `readErr != nil` triggers `c.circuit.RecordFailure()`, add a guard: if `readErr` wraps `context.DeadlineExceeded` AND `ctx.Err() == context.DeadlineExceeded` (meaning OUR deadline fired, not an upstream timeout), skip `RecordFailure()`. Log at Debug level: `"skipping circuit breaker failure for proxy request timeout"`.
- [ ] Similarly check the `c.client.Do(req)` error path at line 436-439: if the error is `context.DeadlineExceeded` from our own deadline, skip `RecordFailure()`. Same guard: `errors.Is(err, context.DeadlineExceeded) && ctx.Err() == context.DeadlineExceeded`.

## Tests

### TestHandleHTTPRequest_SSEStreamExemptFromTimeout

In `client_test.go` or a new `client_sse_test.go`:
- Start an httptest server that serves `Content-Type: text/event-stream` with SSE data, waits for a signal, then closes.
- Call `handleHTTPRequest` with a context that has a very short deadline (e.g. 100ms).
- After 200ms (past the deadline), send the close signal to the SSE server.
- Assert that the response was streamed successfully (not killed by the deadline).
- Assert that the circuit breaker state is still `closed` (no RecordFailure called).

### TestHandleHTTPRequest_ProxyTimeoutDoesNotTripCircuitBreaker

In `client_test.go` or `client_sse_test.go`:
- Start an httptest server that serves a normal `Content-Type: application/json` response but delays the body for longer than the context deadline.
- Call `handleHTTPRequest` with a very short deadline (50ms).
- Assert that the request fails (expected — it IS a normal request that timed out).
- Assert that the circuit breaker state is still `closed` — FIX B prevents our own deadline from being counted.

### TestHandleHTTPRequest_RealUpstreamFailureStillTripsCircuitBreaker

Regression test to ensure FIX B doesn't suppress REAL upstream failures:
- Start an httptest server that returns 502 or closes the connection mid-body.
- Call `handleHTTPRequest` with a long deadline (5s).
- Assert that `RecordFailure()` IS called (circuit breaker state transitions toward open after threshold).

## Acceptance criteria

1. SSE streams (`Content-Type: text/event-stream`) are not killed by `proxyRequestTimeout`.
2. `context.DeadlineExceeded` from `proxyRequestTimeout` does not call `RecordFailure()`.
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
