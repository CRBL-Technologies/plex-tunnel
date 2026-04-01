# TASK: Fix idle pool connection churn (client side)

## Overview

Idle tunnel pool connections are killed every ~70 seconds by the WebSocket read timeout.
Only the control connection (index 0) receives keepalive pings; all other pool connections
sit idle between requests. When `Receive()` times out, the client treats it as a fatal
error and tears down the connection. The server then sees EOF, and the client reconnects
the slot — creating constant churn (3-7 drops/minute with 10 connections).

This churn causes media downloads to fail when requests land on connections in the
middle of a timeout cycle.

Two fixes:
1. Treat read timeouts as non-fatal in `readLoop()` (same as the server-side fix).
2. Send pings on ALL pool connections, not just the control connection, as defense in
   depth to prevent the timeout from ever firing on idle connections.

---

## Change 1: Treat read timeouts as non-fatal in readLoop

### Where to work
- `pkg/client/client.go` — `readLoop()`, lines 249-253

### Current behavior
Any error from `connRef.conn.Receive()` causes an immediate `return fmt.Errorf(...)`,
which tears down the connection — even when the error is just a read timeout on an
idle connection with zero active streams.

### Desired behavior
When `Receive()` returns an error:
1. Check if the error is a `context.DeadlineExceeded` (the 70s read timeout).
2. Check if the connection has zero active streams (`connRef.streams.Load() == 0`).
3. If BOTH conditions are true AND `ctx.Err() == nil` (the session is not shutting down),
   `continue` the loop (retry the `Receive()` call) instead of returning.
4. If either condition is false, keep the existing behavior (return the error).

Use `errors.Is(err, context.DeadlineExceeded)` to detect the timeout.

Add `"errors"` to the import block if not already present.

### Notes
- Log a Debug-level message when retrying (`c.logger.Debug()`) so it's visible in
  debug mode.

---

## Change 2: Ping all pool connections, not just control

### Where to work
- `pkg/client/client.go` — around line 573 where `startPoolPingLoop` is called
  conditionally on `isControl`

### Current behavior
Line 573-574:
```go
if isControl {
    c.startPoolPingLoop(session.ctx, pool, connRef)
}
```
Only the control connection gets a ping loop. All other connections have no keepalive
traffic.

### Desired behavior
Start a per-connection ping loop for EVERY pool connection, not just the control
connection.

However, `startPoolPingLoop` currently uses `pool.replacePingLoop(cancel)` which
cancels the previous ping loop — this is designed for a single session-wide ping loop.
For per-connection pings, we need a different approach:

Create a new function `startConnPingLoop(ctx context.Context, connRef *poolConn)` that:
1. Creates a child context from `ctx`.
2. Stores the cancel function on the `poolConn` (you will need to add a `pingCancel`
   field to the `poolConn` struct — see Change 3).
3. Runs `pingLoop(childCtx, connRef.conn, &connRef.lastPong)` in a goroutine.
4. On error (and if childCtx is not cancelled), logs a warning and closes the connection.

Then at line 573, REPLACE the `if isControl` block:
```go
// Old:
if isControl {
    c.startPoolPingLoop(session.ctx, pool, connRef)
}

// New:
c.startConnPingLoop(ctx, connRef)
if isControl {
    c.startPoolPingLoop(session.ctx, pool, connRef)
}
```

Keep the existing `startPoolPingLoop` call for the control connection — that handles
session-level ping lifecycle (promotion, etc.). The per-connection ping loop is
additional and runs on every connection including control.

Actually wait — control connection would then have TWO ping loops. To avoid that, use
this approach instead:

```go
if isControl {
    c.startPoolPingLoop(session.ctx, pool, connRef)
} else {
    c.startConnPingLoop(ctx, connRef)
}
```

This way control uses the existing session-level ping loop, and non-control connections
each get their own ping loop.

### Notes
- The per-connection ping loop should use the slot's `ctx` (which is cancelled when
  the slot is removed), NOT `session.ctx`.
- On ping loop error, just close the connection — the existing reconnect logic in
  `runPoolSlot` will handle reconnection.

---

## Change 3: Add pingCancel field to poolConn

### Where to work
- `pkg/client/pool.go` — `poolConn` struct, line 30-35

### Current behavior
```go
type poolConn struct {
    conn     *tunnel.WebSocketConnection
    index    int
    streams  atomic.Int64
    lastPong atomic.Int64
}
```

### Desired behavior
Add a `pingCancel` field to store the cancel function for the per-connection ping loop:
```go
type poolConn struct {
    conn       *tunnel.WebSocketConnection
    index      int
    streams    atomic.Int64
    lastPong   atomic.Int64
    pingCancel context.CancelFunc
}
```

When the connection is removed from the pool or closed, the `pingCancel` should be
called if non-nil to stop the ping goroutine. Make sure the `remove()` function in
pool.go calls `pingCancel()` on the removed connection if the field is set.

---

## Tests

### Existing tests:
Run the existing test suite to make sure nothing breaks:
```bash
go test ./...
```

Do NOT create new test files for this change — it is defensive and verifiable through
the existing test suite and manual observation of logs.

---

## Constraints
- DO NOT delete or modify any existing code that is not explicitly mentioned in this task.
- DO NOT change any timeout values or constants.
- DO NOT modify the proto repo.
- DO NOT change the existing `startPoolPingLoop` function — it handles session-level
  control connection promotion and must remain.
- Keep all existing tests passing.

## Verification
```bash
cd /home/dev/github/plex-tunnel
go build ./...
go test ./...
```

## When Done
Commit all changes with a descriptive commit message. Do NOT modify TASK.md.
