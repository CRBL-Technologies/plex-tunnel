# TASK: rosa audit P0 — client (60s recycle + cap collapse + busy promotion)

## Context

Rosa's audit of the leased-tunnel-pool client merge (2026-04-08, commit `cb21218`) flagged two production blockers. Sam's analysis of CEO's 60-second download failure on staging traced the same codepath.

Live production breakage:
- **CEO's download dies at 56 seconds**, reliably. Any file bigger than what streams through under ~30s fails.
- **Paid tiers collapse to Free-tier connection counts.** A Pro user getting 4 tunnels instead of 11. Client is hardcoded to request 4.

Background the rest of this task assumes:
- **Proto PR** `fix/rosa-p0-proto` (PR #20 on plex-tunnel-proto) adds `tunnel.DialTunnelWebSocket()` and the `tunnel.TunnelReadTimeout = 0` constant. That PR must be merged to proto's `dev` before this task's implementation can build — bump `go.mod` to the merged commit.
- **Server PR** `fix/rosa-p0-server` is the sibling of this task and is landing in parallel (bob is driving it). Server mirrors the recv-loop fix, fixes `grantedMaxConnections`, and adds its own regression tests. Do NOT touch server code.
- **60-second recycle RCA** (full version in paul's synthesis): the 30s per-`Receive()` deadline in the proto transport fires on long-lived tunnels streaming large responses. The client's readLoop misinterprets `context.DeadlineExceeded + streams > 0` as a fatal disconnect and closes the tunnel mid-stream, killing the in-flight download. Ping/pong is the real liveness signal.
- **Cap collapse RCA — client side**: `config.go:42` hardcodes `MaxConnections: 4` and `client.go:174-195` sends exactly that on the register. Even after bob removes the server-side clamps, the server will honor the client's `requested = 4` unless the client stops sending it.
- **Busy promotion RCA**: `pool.go:135-148` (`remove()`) promotes the first non-nil slot when control is lost, without checking whether the slot is currently streaming a response. A busy data tunnel promoted to control continues serving its stream while also now handling control duties — brittle and semantically wrong.

## Goal

Ship the **client side** of the P0 rosa audit fix: kill the 60-second recycle by migrating to the proto's long-lived tunnel dial path and removing the busy-disconnect branch; stop hardcoding `MaxConnections: 4` so the server's tier grant actually takes effect; fix the busy-promotion fallback to force a clean session restart instead; add regression tests.

## Constraints / guardrails

- **DO NOT delete or modify code not mentioned in this task.**
- **DO NOT touch the server repo (`/home/dev/worktrees/paul/plex-tunnel-server`).** Server fixes are bob's job on `fix/rosa-p0-server`.
- **DO NOT touch the proto repo.** Alice shipped that in PR #20.
- **DO NOT change the wire protocol.** The register message format is stable; we're just changing what value we put in `MaxConnections`.
- **DO NOT remove the `MaxConnections` field from `Config`** — keep it as an optional override. Just change the default value.
- **Backward compat with old server**: when the client sends `MaxConnections = 0`, an older server (pre-clamp-removal) may still clamp to its own default. That's acceptable — the client should accept whatever the server returns in the ack. Do NOT fail the handshake if the ack's `MaxConnections` is lower than what we would have preferred.
- **proto bump**: update `go.mod` to depend on the merged proto commit before writing code that references `tunnel.DialTunnelWebSocket`. Don't hardcode a `replace` directive.

## Required proto version

This task requires `plex-tunnel-proto` at or past the commit where `fix/rosa-p0-proto` landed on `dev`. Run:

```bash
cd /home/dev/worktrees/paul/plex-tunnel
go get github.com/CRBL-Technologies/plex-tunnel-proto@dev
go mod tidy
```

After this, `tunnel.DialTunnelWebSocket` and `tunnel.TunnelReadTimeout` must be available. If not, stop — proto merge hasn't happened yet.

## CRITICAL — rosa's notes from the proto audit

These three points are non-negotiable. Read them before starting and verify them before declaring done.

1. **Migrate EVERY tunnel call site, not just the obvious ones.** Before writing any code, grep the entire `plex-tunnel` repo (not just `pkg/client`) for all of these:
   ```bash
   cd /home/dev/worktrees/paul/plex-tunnel
   grep -rn "DialWebSocket\|AcceptWebSocket\|WebSocketTransport" --include="*.go"
   ```
   For each hit, decide: is this a long-lived tunnel lane (control or data tunnel between client and server), or is it short-lived plumbing (e.g. a test helper, an unrelated WS-proxy passthrough)? Only long-lived tunnel lanes migrate to `DialTunnelWebSocket`. If you find a site that is ambiguous — STOP and ask. The two known sites are listed in task 1; if grep reveals more, surface them before migrating.

2. **Downstream `errors.Is` checks are now reliable.** Alice's proto PR normalized `ReceiveContext` so that when the parent context is canceled or times out, the returned error wraps `ctx.Err()` (so `errors.Is(err, context.DeadlineExceeded)` and `errors.Is(err, context.Canceled)` work correctly). The readLoop retry branch in task 2 depends on this. Do NOT add string-matching fallbacks — trust `errors.Is`.

3. **Keepalive ping/pong is now the ONLY backstop for stuck reads.** With `TunnelReadTimeout = 0`, the proto layer will block forever on a hung WebSocket. Liveness is decided exclusively by the existing ping/pong machinery (`startPoolPingLoop` for control, `startConnPingLoop` for data, `lastPong` watchdog in `pingLoop`). Before declaring done, verify:
   - Both control and data tunnels still launch their pingLoop after migration to `DialTunnelWebSocket`.
   - The pong-watchdog still triggers a reconnect when `lastPong` goes stale (search for `lastPong` and `PongTimeout` to confirm the existing path is intact).
   - The migration does NOT accidentally bypass `setConnPingCancel` / `replacePingLoop` registration.
   If any of those are broken by the migration, stop and ask — do not "fix" by re-introducing a read deadline.

## Tasks

### 1. Migrate tunnel dial sites to long-lived entry point

Two call sites in `pkg/client/client.go`:

- Around line 183 in `runSession`: `tunnel.DialWebSocket(ctx, c.cfg.ServerURL, nil)` — this is the **control tunnel** initial dial.
- Around line 713 in `joinSessionConnection`: `tunnel.DialWebSocket(ctx, c.cfg.ServerURL, nil)` — this is the **data tunnel** join dial.

- [ ] Replace both with `tunnel.DialTunnelWebSocket(ctx, c.cfg.ServerURL, nil)`.
- [ ] Leave any other `DialWebSocket` sites unchanged — none should exist in `pkg/client`, but double-check with a grep. If you find one, ask.

### 2. Remove the busy-disconnect branch from readLoop

File: `pkg/client/client.go`, function `readLoop` around line 257-275.

Current shape:

```go
for {
    msg, err := connRef.conn.Receive()
    if err != nil {
        if errors.Is(err, context.DeadlineExceeded) && connRef.streams.Load() == 0 && ctx.Err() == nil {
            c.logger.Debug()...Msg("retrying idle tunnel connection after read timeout")
            select {
            case <-time.After(100 * time.Millisecond):
            case <-ctx.Done():
                return ctx.Err()
            }
            continue
        }
        return fmt.Errorf("read loop: %w", err)
    }
    ...
}
```

With the proto migration in task 1, `Receive()` on a tunnel will not return `DeadlineExceeded` under normal operation. But the retry path must remain correct for:
- A transitional period where the proto version isn't yet bumped (defensive)
- Any explicit parent-context timeout a caller might set

- [ ] **Delete the `connRef.streams.Load() == 0` restriction** on the idle-retry branch. Retry `DeadlineExceeded` regardless of stream count. Real liveness is decided by:
  - The `pingLoop` on this connection (already running via `startPoolPingLoop` for control or `startConnPingLoop` for data)
  - `ctx.Err() != nil` — if the session context is done, we exit
- [ ] The `ctx.Err() == nil` guard stays. If session ctx is canceled, we must propagate.
- [ ] Add a DEBUG structured log when `DeadlineExceeded` fires, with fields: `session_id`, `connection_index`, `streams`, `since_last_pong_ms`. Use `c.logger.Debug()`.
- [ ] Keep the 100ms sleep on retry — it prevents a tight loop if something is really wrong.

### 3. Stop hardcoding `MaxConnections` in the client register

File: `pkg/client/config.go`, around line 42.

- [ ] Change `MaxConnections: 4` → `MaxConnections: 0`.
- [ ] Update the env-var loader at line 97-106: when `PLEXTUNNEL_MAX_CONNECTIONS` is unset, keep `cfg.MaxConnections = 0`. When it IS set, keep the existing behavior (1-32 range check). Intent: power users can still pin a lower value; the default is "trust the server".
- [ ] Add a comment above the field: `// 0 = let the server grant based on tier. Non-zero pins a requested maximum that the server may clamp down.`

File: `pkg/client/client.go`, around line 174-195 in `runSession`:

- [ ] The current code:

  ```go
  requestedMaxConnections := c.cfg.MaxConnections
  if requestedMaxConnections < 1 {
      requestedMaxConnections = 1
  }
  ```

  Change to: when `c.cfg.MaxConnections == 0`, send `0` in the register message (no local floor). When `c.cfg.MaxConnections > 0`, send that value. Remove the `< 1 → 1` floor.

- [ ] The register send then uses `MaxConnections: c.cfg.MaxConnections` directly (can be zero).
- [ ] Accept whatever `registerAck.MaxConnections` the server returns, as the current code already does at line 214-217. **No change needed at the ack site** — just verify that the existing code handles `registerAck.MaxConnections > c.cfg.MaxConnections` gracefully (it currently does; it caps at `maxPoolConnections`).

### 4. Fix busy-slot promotion in `pool.remove()`

File: `pkg/client/pool.go`, function `remove` around line 102-149.

Current fallback at lines 135-148:

```go
nextIndex := -1
for i, connRef := range p.conns {
    if connRef == nil {
        continue
    }
    nextIndex = i
    break
}
p.controlIndex = nextIndex
if promoted = p.conns[nextIndex]; promoted != nil && promoted.pingCancel != nil {
    promoted.pingCancel()
    promoted.pingCancel = nil
}
return remaining, promoted, true
```

The fallback promotes the first non-nil slot even if it's actively streaming.

- [ ] **Scan for the first idle slot (`connRef.streams.Load() == 0`) instead of the first non-nil slot.**
- [ ] If **no idle slot exists** (all remaining slots are busy), return `promoted = nil, controlLost = true`. The caller is `maintainPoolSlot` at line 650: when `controlLost && promoted == nil`, the session cannot recover — force a full session restart by sending an error to `session.errCh`, which causes `runSession` to return and the outer retry loop to reconnect with a fresh sessionID.
- [ ] The caller site also needs a small update: in `maintainPoolSlot` around line 654-661, the current code promotes via `c.startPoolPingLoop(...)` when `controlLost && promoted != nil`. Keep that branch. Add a new branch: when `controlLost && promoted == nil && remaining > 0`, send `fmt.Errorf("control lost with no idle promotion candidate")` to `session.errCh` and return from `maintainPoolSlot`. The `runSession` outer loop will tear down the session and reconnect fresh.
- [ ] Do NOT leak pingCancel references. Audit the code to ensure the old control's pingCancel is still cleaned up correctly in the "no idle candidate" branch — follow the existing pattern at lines 114-122 which already handles removed-conn pingCancel cleanup.

### 5. Regression test: readLoop does not close tunnel on busy DeadlineExceeded

File: `pkg/client/client_test.go` (or a new `readloop_test.go` in the same package if the existing file is crowded).

- [ ] **`TestReadLoop_BusyTunnelSurvivesDeadline`** — use the `testSocketPair` helper that already exists in `pool_test.go` (or an equivalent). Set `connRef.streams` to 1 (busy). Run the client's `readLoop` in a goroutine against a fake connection whose `Receive()` returns `context.DeadlineExceeded` once, then `MsgPong`, then blocks. Assert:
  - `readLoop` does NOT return after the first `DeadlineExceeded`.
  - The tunnel connection is NOT closed.
  - `lastPong` is updated when the second message arrives.
  - Cancel the session context and assert `readLoop` returns cleanly.

### 6. Regression test: pool.remove refuses busy promotion

File: `pkg/client/pool_test.go`, adjacent to the existing `TestPoolResize_*` tests.

- [ ] **`TestPoolRemove_NoBusyPromote`** — build a pool with 4 slots: slot 0 = control (busy), slot 1 = data (busy, `streams = 1`), slot 2 = data (idle, `streams = 0`), slot 3 = data (busy, `streams = 1`). Call `pool.remove(0)` (remove control). Assert:
  - `controlLost == true`
  - `promoted != nil`
  - `promoted.index == 2` (the only idle slot)
- [ ] **`TestPoolRemove_AllBusyForcesFullReconnect`** — build a pool with 3 slots: all busy (`streams = 1` each). Call `pool.remove(0)`. Assert:
  - `controlLost == true`
  - `promoted == nil`
  - `remaining == 2`
- [ ] **`TestPoolRemove_FindsIdleAcrossAllSlots`** — build a pool where slot 0 (control) is removed, slots 1-4 busy, slot 5 idle. Assert promoted.index == 5. Edge-case test for the scan.

### 7. Regression test: client register uses configured MaxConnections

File: `pkg/client/client_handshake_test.go` (it already exists and exercises the register path).

- [ ] **`TestClient_RegisterUsesConfiguredMaxConnections`** — start a fake server that captures the register message. Run `runSession` with:
  - Case A: `cfg.MaxConnections = 0` → register should carry `MaxConnections = 0`.
  - Case B: `cfg.MaxConnections = 11` → register should carry `MaxConnections = 11`.
  - Case C: env override `PLEXTUNNEL_MAX_CONNECTIONS=8` → register should carry `MaxConnections = 8`. (If the handshake test file doesn't already do env manipulation, skip this sub-case and add a dedicated `TestLoadConfig_MaxConnectionsOverride` in `config_test.go`.)

### 8. Bump proto version

- [ ] `go get github.com/CRBL-Technologies/plex-tunnel-proto@dev`
- [ ] `go mod tidy`
- [ ] Verify `tunnel.DialTunnelWebSocket` and `tunnel.TunnelReadTimeout` compile.
- [ ] Commit `go.mod` / `go.sum` changes as part of the same commit as task 1.

## Tests to run

```bash
cd /home/dev/worktrees/paul/plex-tunnel
go vet ./...
go test -race ./... -count=1
```

All must pass. If existing tests fail, read the error — likely a real interaction with these changes. Do NOT silence failing tests. If one seems wrong in light of the new architecture, stop and ask.

## Acceptance criteria

- Client register with default config sends `MaxConnections = 0`.
- Client accepts the server's tier-derived grant (11 for Pro, 21 for Max, 4 for Free) without complaint.
- `pool.remove()` never promotes a slot with `streams > 0`.
- Client's readLoop retries `DeadlineExceeded` regardless of `streams` count.
- Control + data tunnel dials use `DialTunnelWebSocket`.
- All existing client tests pass with `-race`.
- New regression tests pass.
- `go vet ./...` clean.

## Verification

```bash
cd /home/dev/worktrees/paul/plex-tunnel
go vet ./...
go test -race ./... -count=1
```

INFRA-CHECK: no doc change required — this PR does not touch docker-compose, Caddyfile, CI workflows, or trust-boundary code. It's a client-side behavior fix.

## Notes for the reviewer

- This PR depends on `fix/rosa-p0-proto` being on `dev` first.
- The server-side fixes are in sibling PR `fix/rosa-p0-server` (bob). The two PRs are independent at the wire level — an old client + new server is fine (server just returns a larger grant than the old client asked for, which the old client accepts at line 214-217), and a new client + old server is also fine (old server clamps the new client's `0` request back to its legacy default, which the new client accepts).
- The "delayed pool expansion" observation from sam's logs (24s to reach 10 tunnels) is NOT in this PR — P1.
- MsgCancel propagation (downstream-disconnect → upstream abort) is NOT in this PR — P1.
- HEAD idempotent retry and logging context gaps are NOT in this PR — P1.
