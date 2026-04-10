## Context

Issue CRBL-Technologies/plex-tunnel#84: when `parentCtx` is canceled while an SSE stream
is open (session teardown / reconnect), the body-read error propagates to
`RecordFailure()` because the existing guard only checks for `errProxyRequestTimeout`.
This causes spurious circuit breaker trips.

The HTTP request is created with `parentCtx` at `pkg/client/client.go:414`, so when
that context is canceled, `resp.Body.Read` returns a context-canceled error that falls
through to `RecordFailure()`.

## Goal

Skip `RecordFailure()` when the error is caused by `parentCtx` cancellation, using the
same guard pattern already in place for `errProxyRequestTimeout`.

## Constraints / guardrails

- DO NOT delete or modify code not mentioned in this task.
- Only touch `pkg/client/client.go` and `pkg/client/client_sse_test.go`.
- Do not change the `circuitBreaker` type or any other file.

## Tasks

- [ ] In `pkg/client/client.go`, function `handleHTTPRequest` (line ~397), at every
      `RecordFailure()` call site, add a `parentCtx.Err() != nil` guard that skips the
      failure recording and logs a debug message. There are 5 call sites:

      1. **Line ~442** — after `c.client.Do(req)` fails.
         Add `parentCtx.Err() != nil` as an additional OR condition in the existing
         `if` that checks `errProxyRequestTimeout`.

      2. **Line ~514** — after `conn.Send(responseMsg)` fails in the chunk loop
         (inside `if upstreamFailure`).
         Wrap the `RecordFailure()` call so it is skipped when `parentCtx.Err() != nil`.

      3. **Line ~543** — after body `readErr` (not EOF, not timeout).
         Add `parentCtx.Err() != nil` as an additional OR condition in the existing
         `if` that checks `errProxyRequestTimeout`.

      4. **Line ~563** — after `conn.Send(finalMsg)` fails (inside `if upstreamFailure`).
         Wrap the `RecordFailure()` call so it is skipped when `parentCtx.Err() != nil`.

      5. **Line ~583** — end-of-function `if upstreamFailure` block.
         Wrap the `RecordFailure()` call so it is skipped when `parentCtx.Err() != nil`.

      For each site, add a debug log like:
      ```go
      requestLogger.Debug().Err(err).Msg("skipping circuit breaker failure for parent context cancellation")
      ```

- [ ] In `pkg/client/client_sse_test.go`, add a new test:
      `TestHandleHTTPRequest_ParentCtxCancelDuringSSEDoesNotTripCircuitBreaker`

      The test should:
      1. Start an SSE upstream (Content-Type: text/event-stream) that sends one event
         then blocks on a channel.
      2. Create a cancelable `parentCtx` (via `context.WithCancel`).
      3. Create a `timeoutCtx` with a long timeout (5s) so it does NOT fire.
      4. Call `handleHTTPRequest` in a goroutine.
      5. Sleep briefly (~100ms) to let the first SSE event arrive.
      6. Cancel `parentCtx`.
      7. Assert `handleHTTPRequest` returns an error (context canceled).
      8. Assert `client.circuit.stateValue() == circuitStateClosed` — the breaker must
         NOT have tripped.

## Tests

- `TestHandleHTTPRequest_ParentCtxCancelDuringSSEDoesNotTripCircuitBreaker` — verifies
  parent-cancel during SSE does not trip the breaker.
- All existing tests in `pkg/client/` must still pass.

## Acceptance criteria

1. `go test ./pkg/client/... -count=1` passes.
2. The new test specifically asserts circuit stays closed on parent cancel.
3. No unrelated files modified.

## Verification

```bash
cd /home/dev/worktrees/alice/plex-tunnel
go test ./pkg/client/... -count=1 -v
go vet ./...
```
