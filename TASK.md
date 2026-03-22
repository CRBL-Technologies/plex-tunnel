# Task: Handle MsgMaxConnectionsUpdate from server

## Context

The server can now push `MsgMaxConnectionsUpdate` (message type 12) to connected clients when an admin changes a token's `max_connections` via the admin UI. The client currently ignores this message (`"ignoring unsupported message type"` in the default branch of the readLoop switch). This means:

- **Scale down**: server closes excess connections, but client keeps retrying them ("session connection pool full" errors in a retry loop)
- **Scale up**: client never learns it can open more connections until it fully reconnects

## Requirements

### 1. Handle `MsgMaxConnectionsUpdate` in the read loop

In `pkg/client/client.go`, the `readLoop` method (line 170) has a switch on `msg.Type`. Add a case for `tunnel.MsgMaxConnectionsUpdate`:

- Extract `msg.MaxConnections` (guaranteed >= 1 by server validation)
- Call a new method on the pool to resize it
- Log the change at Info level: old max, new max

### 2. Add `Resize(newMax int)` method to `ConnectionPool`

In `pkg/client/pool.go`, add a method that adjusts the pool size:

**Scale down** (newMax < current maxConns):
- Close and nil out connections in slots `[newMax, maxConns)` — iterate from the highest index down
- If the control connection is in a removed slot, promote the lowest remaining connection before closing it
- Shrink `p.conns` slice to `newMax`
- Update `p.maxConns`

**Scale up** (newMax > current maxConns):
- Extend `p.conns` slice with nil entries up to `newMax`
- Update `p.maxConns`
- Return `(oldMax, newMax)` so the caller knows which new slot indices to start maintaining

**No change** (newMax == current maxConns): return early, no-op

### 3. Launch new `maintainPoolSlot` goroutines for scale-up

Back in `client.go`, after calling `pool.Resize()`, if the pool grew (newMax > oldMax), spawn `maintainPoolSlot` goroutines for each new index `[oldMax, newMax)`. These goroutines need the same `ctx`, `pool`, and `errCh` that the existing slots use.

This means the readLoop needs access to these values. The simplest approach: pass a callback/channel from the session runner to the readLoop, or store `pool` and `errCh` on the Client (scoped to the session). Choose whatever is cleanest — the `pool`, `errCh`, and `sessionCtx` are already in scope in `runSession` (line 129+).

### 4. Shut down maintainPoolSlot goroutines for removed slots

When scaling down, the `maintainPoolSlot` goroutines for the removed indices must stop. The cleanest approach: give each slot its own cancellable context derived from sessionCtx. When Resize removes a slot, cancel that slot's context. The maintainPoolSlot loop already checks `ctx.Err()` — this will cause it to exit cleanly.

### 5. Update status after resize

After resizing, call `c.syncPoolStatus(pool)` and `c.updateStatus(...)` to update `MaxConnections` in the connection status.

### 6. Tests

Add tests in `pkg/client/pool_test.go`:

- `TestPoolResize_ScaleDown`: pool with 4 slots, 3 active connections → resize to 2 → verify slots 2-3 closed, active count correct
- `TestPoolResize_ScaleUp`: pool with 2 slots → resize to 4 → verify new nil slots exist, maxConns updated
- `TestPoolResize_NoChange`: resize to same value → no-op
- `TestPoolResize_ScaleDown_PromotesControl`: control connection is in a slot that gets removed → verify promotion happens

## Important constraints

- DO NOT delete or modify any existing code that is not explicitly mentioned in this task
- DO NOT change the tunnel proto package — `MsgMaxConnectionsUpdate` already exists
- The `maintainPoolSlot` goroutines for removed slots must exit cleanly (context cancellation or a "slot removed" signal) — they must not keep retrying
- When scaling down, gracefully close connections (let in-flight streams drain if possible, but don't block indefinitely — a short grace period or immediate close is fine)
- Commit your changes when done with a descriptive commit message
