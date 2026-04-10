# TASK: split client stream semaphore into control + data (#89)

## Context

Issue: CRBL-Technologies/plex-tunnel#89. On 2026-04-10 21:04–21:05Z the CEO
could not browse the Plex library while a striped download was running. The
client logged a burst of `"concurrent stream limit reached, rejecting request"`
warnings and the server observed 503 / `bytes_out=17` on
`/:/eventsource/notifications` (the rejection body).

Root cause: `pkg/client/client.go` uses a single 128-slot semaphore,
`c.streamSem` (field declared at line 37, constant `maxConcurrentStreams = 128`
at line 24, initialized in `New` at line 124). All inbound `MsgHTTPRequest`
frames acquire from the same channel (line 299). A striped download places
hundreds of segments in-flight; once the 128 slots are full, every subsequent
request — including browsing, SSE, and metadata — is rejected with
`StatusServiceUnavailable`.

The fix approach from the issue is: split the semaphore into two — one for
control-path requests (browsing, SSE, metadata) and one for data-path requests
(downloads, media). The client already has pool-slot classification via
`session.pool.IsControlSlot(connRef.index)` (used at line 325 and line 616).
Use that classification at the point of semaphore acquisition.

Why slot classification is a sound proxy for request class, given how the
server routes traffic:

- Striped downloads are the source of saturation. The server explicitly
  excludes the control lane from stripe plans: `stripingConnections` in
  `plex-tunnel-server/pkg/server/striping.go:572` returns `snapshot[1:]`,
  skipping index 0. During a striped download, lane 0 (the control lane)
  receives zero segments.
- Non-striped requests flow through `selectLeastLoadedConn` on the server,
  which scans all lanes. In steady state the control lane is the least loaded
  (it carries the long-lived SSE stream and occasional metadata), so browsing
  and metadata requests naturally land on it anyway.

So on the client, "request arrived on control slot" is an effective signal
for "this is control-path traffic" for exactly the case the issue is about.

## Goal

A saturated data-path semaphore on the client MUST NOT block control-lane
traffic. Downloads get their own 128 slots; control traffic gets a separate
pool of 32 slots that is untouched by download load.

## Constraints / guardrails

- DO NOT delete or modify any code not explicitly mentioned in this task.
- DO NOT change files outside `pkg/client/client.go` and
  `pkg/client/client_readloop_test.go`.
- DO NOT touch `pkg/client/pool.go`, `pkg/client/metrics.go`,
  `pkg/client/config.go`, `pkg/client/reconnect.go`, `pkg/client/circuit.go`,
  or any file under `cmd/`.
- DO NOT introduce new dependencies. No new Go modules, no new imports
  beyond what is already in `client.go` / `client_readloop_test.go`.
- DO NOT change the 128 data-path capacity. The control semaphore is
  ADDITIONAL capacity (32 slots), not taken from data.
- DO NOT change any behavior for requests outside the `MsgHTTPRequest`
  read-loop arm. Pong handling, ack handling, ping loops, reconnect: all
  untouched.
- KEEP variable names and logging patterns consistent with the existing
  style. Use `zerolog` structured fields.

## Tasks

### 1. Replace the single semaphore constant and field

In `pkg/client/client.go`:

- **Line 24**: delete `maxConcurrentStreams = 128`. Add in the same `const`
  block:
  ```go
  // maxDataStreams caps concurrent data-path requests (downloads, media).
  // Preserves the pre-split 128-slot capacity for downloads.
  maxDataStreams = 128
  // maxControlStreams caps concurrent control-path requests (browsing,
  // SSE, metadata). Kept small but generous — the long-lived SSE stream
  // occupies one slot and browsing bursts are typically <20 concurrent.
  maxControlStreams = 32
  ```

- **Line 37**: replace the field
  ```go
  streamSem chan struct{}
  ```
  with
  ```go
  dataSem    chan struct{}
  controlSem chan struct{}
  ```

- **Line 124** (inside `New`): replace
  ```go
  streamSem: make(chan struct{}, maxConcurrentStreams),
  ```
  with
  ```go
  dataSem:    make(chan struct{}, maxDataStreams),
  controlSem: make(chan struct{}, maxControlStreams),
  ```

### 2. Add `tryAcquireStreamSlot` helper

Add a method on `*Client` immediately AFTER `sendProxyError` (current line
641). The method selects the control or data semaphore based on the class
argument, attempts a non-blocking acquire, and returns a release closure.

```go
// tryAcquireStreamSlot attempts to take a slot from the control or data
// stream semaphore. Control-lane traffic (browsing, SSE, metadata) is
// gated by controlSem; all other traffic is gated by dataSem. A saturated
// dataSem MUST NOT block controlSem, which is the entire point of the
// split — see #89.
//
// On success, returns a release func that MUST be called exactly once
// when the request completes. On saturation, returns (nil, false).
func (c *Client) tryAcquireStreamSlot(isControl bool) (release func(), ok bool) {
	sem := c.dataSem
	if isControl {
		sem = c.controlSem
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	default:
		return nil, false
	}
}
```

### 3. Rewire the `MsgHTTPRequest` branch in `readLoopWithConnection`

In `pkg/client/client.go` around lines 297–318 (the `case tunnel.MsgHTTPRequest:`
arm). Replace the current semaphore `select` and the `defer func() { <-c.streamSem }()`
with calls to `tryAcquireStreamSlot`. Classify the request by checking
`session.pool.IsControlSlot(connRef.index)`. Add a `route_class` structured
field to the rejection log line.

Expected final shape:

```go
case tunnel.MsgHTTPRequest:
	isControl := session.pool.IsControlSlot(connRef.index)
	release, ok := c.tryAcquireStreamSlot(isControl)
	if !ok {
		routeClass := "data"
		if isControl {
			routeClass = "control"
		}
		c.logger.Warn().
			Str("request_id", msg.ID).
			Str("route_class", routeClass).
			Msg("concurrent stream limit reached, rejecting request")
		_ = c.sendProxyError(connRef.conn, msg.ID, http.StatusServiceUnavailable, "client overloaded")
		continue
	}
	go func(request tunnel.Message) {
		defer release()
		connRef.streams.Add(1)
		activeStreamsMetric.Inc()
		defer connRef.streams.Add(-1)
		defer activeStreamsMetric.Dec()

		reqCtx, reqCancel := context.WithTimeoutCause(ctx, proxyRequestTimeout, errProxyRequestTimeout)
		defer reqCancel()
		if err := c.handleHTTPRequest(ctx, reqCtx, session.pool, connRef, request); err != nil {
			requestLogger := c.requestLogger(session.pool, connRef, request)
			requestLogger.Warn().Err(err).Msg("failed to process proxied request")
		}
	}(msg)
```

Do not change any other arm of the `switch msg.Type` block.

## Tests

Add four tests to `pkg/client/client_readloop_test.go`. They exercise
`tryAcquireStreamSlot` directly; no new test helpers or fixtures are needed.
Use `client := New(Config{}, zerolog.Nop())` to build a client for each
test.

### `TestTryAcquireStreamSlot_ControlAndDataUseSeparatePools`

Assert independence of the two semaphores:

1. Build a fresh client.
2. Verify `cap(client.dataSem) == maxDataStreams` and
   `cap(client.controlSem) == maxControlStreams`.
3. Acquire one control slot and one data slot; both must return `ok=true`.
4. After acquisition, verify `len(client.controlSem) == 1` and
   `len(client.dataSem) == 1`.
5. Call both release functions and verify both channels drain to length 0.

### `TestTryAcquireStreamSlot_DataSaturationDoesNotBlockControl`

This is the regression test for #89:

1. Build a fresh client.
2. Fill `client.dataSem` to capacity with `maxDataStreams` direct sends
   (`client.dataSem <- struct{}{}`) in a loop.
3. Call `client.tryAcquireStreamSlot(false)` — expect `ok=false`, release
   must be nil.
4. Call `client.tryAcquireStreamSlot(true)` — expect `ok=true` (this is the
   assertion that matters for the CEO). Hold the release.
5. Call release, then call `client.tryAcquireStreamSlot(true)` again —
   expect `ok=true` (verifies release actually frees the slot).

### `TestTryAcquireStreamSlot_ControlSaturationDoesNotBlockData`

Symmetric test:

1. Build a fresh client.
2. Fill `client.controlSem` to capacity with `maxControlStreams` sends.
3. `tryAcquireStreamSlot(true)` — expect `ok=false`.
4. `tryAcquireStreamSlot(false)` — expect `ok=true`.

### `TestTryAcquireStreamSlot_ReleaseReturnsSlot`

Minimal round-trip:

1. Build a fresh client.
2. Acquire a data slot; verify `len(client.dataSem) == 1`.
3. Release; verify `len(client.dataSem) == 0`.
4. Repeat for a control slot.

Use `t.Fatalf` on any failed assertion with a descriptive message.

## Acceptance criteria

1. `maxConcurrentStreams` and `c.streamSem` are removed from `client.go`.
   No references remain anywhere in the repo.
2. `maxDataStreams = 128` and `maxControlStreams = 32` are defined as
   constants in the existing `const` block at the top of `client.go`.
3. `*Client` has both `dataSem` and `controlSem` fields, initialized in
   `New`.
4. `*Client` has the `tryAcquireStreamSlot(isControl bool)` helper with the
   signature and behavior specified in Task 2.
5. The `MsgHTTPRequest` branch in `readLoopWithConnection` classifies by
   `session.pool.IsControlSlot(connRef.index)`, calls
   `tryAcquireStreamSlot`, and includes `route_class` in the rejection log.
6. The four new tests in `client_readloop_test.go` pass.
7. All pre-existing tests in `pkg/client/...` still pass.
8. `gofmt -l pkg/client` produces no output.
9. `go vet ./...` is clean.
10. `go build ./...` succeeds.
11. No files outside `pkg/client/client.go` and
    `pkg/client/client_readloop_test.go` are modified.

## Verification

```bash
cd /home/dev/worktrees/alice/plex-tunnel
gofmt -l pkg/client
go vet ./...
go test ./pkg/client/... -race -count=1 -timeout=60s
go build ./...
```

All five commands must succeed. `gofmt -l` must print nothing.
