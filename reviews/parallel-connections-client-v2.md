# Code Review: claude/review-bandwidth-notes-2oZ1b (client v2 implementation)

**Branch:** `claude/review-bandwidth-notes-2oZ1b`
**Compared to:** `main`
**Reviewer:** Claude
**Last updated:** 2026-03-20 (round 2)

---

## Summary

Implements Phase 3 of the parallel connections feature. `runSession` grows from
managing a single WebSocket connection to establishing a session with a
server-granted connection pool. A new `ConnectionPool` type (`pool.go`) holds a
fixed-size slice of `*poolConn` indexed by connection slot. One
`maintainPoolSlot` goroutine per slot handles dialling, join-register handshake,
read loop, connection promotion on control loss, and per-slot reconnect backoff.
Ping/pong runs on the control connection only and is cancelled and re-launched
when control is promoted to another slot. The UI and status type are updated to
expose session ID, active connections, pool size, and a max connections config
field.

---

## What Was Done

- `pool.go` (new): `ConnectionPool` with fixed `[]*poolConn` slice, `add`,
  `remove`, `close`, `replacePingLoop`, `snapshot`, and count helpers.
  `poolConn` holds the connection, its slot index, active stream count, and last
  pong timestamp.
- `client.go`: `runSession` sends v2 `Register` with `MaxConnections`, validates
  the v2 `RegisterAck`, creates a pool from the server-granted metadata, and
  launches one `maintainPoolSlot` goroutine per slot; `readLoop` now takes
  `*poolConn` and tracks `streams` atomics per connection; `pingLoop` moved to
  control connection only via `startPoolPingLoop`; `joinSessionConnection`
  dials, sends a join `Register` (with `SessionID` and `Subdomain`), and
  validates all fields of the returned ack; `validateRegisterAck` extracted as a
  shared helper; `syncPoolStatus` updates the status snapshot from the pool.
- `config.go`: `MaxConnections int` field, `PLEXTUNNEL_MAX_CONNECTIONS` env var,
  default `4`.
- `status.go`: `SessionID`, `ActiveConnections`, `MaxConnections`,
  `ControlConnection` added.
- `cmd/client/ui.go`: Status panel shows session ID, active connections, pool
  size; settings form adds max connections input with server-side validation.
- `client_handshake_test.go` (new): Five tests — initial register shape, protocol
  mismatch, old server hint, missing v2 session metadata rejection, pool
  expansion (1 new-session + 2 join-session registers verified).
- `go.mod` updated to latest proto pre-release with v2 session fields.

---

## What Looked Good

- **Fixed-size indexed slice for the pool.** Pre-allocating `[]*poolConn` keyed
  by slot index makes `add(index, conn)` and `remove(index)` O(1) and avoids
  index drift across reconnects. Each slot always maps to the same position,
  which simplifies promotion logic.

- **`maintainPoolSlot` owns its entire lifecycle.** Stagger, dial, read-loop,
  removal, promotion, and backoff are all in one goroutine per slot. Clean
  separation — no shared state between slots beyond the pool mutex.

- **Control promotion is correct.** `pool.remove` cancels the old ping context
  and returns the lowest-index surviving connection as `promoted`. The caller
  immediately calls `startPoolPingLoop` on it. `lastPong` is reset before the
  new loop starts, preventing a false-positive pong timeout caused by the gap
  between the old and new loops.

- **`joinSessionConnection` includes `Subdomain` in the join register.** The
  spec doesn't explicitly require it, but including it means the server's
  `tokens.Validate` receives the correct subdomain on the join path, which
  avoids the subdomain-mismatch edge case flagged in the server review (Issue 1
  of that review) for this client. Both sides are consistent.

- **`validateRegisterAck` shared between initial and join paths.** Calling
  `registerAck.Validate()` as the final step means proto-level field checks
  (missing `SessionID`, zero `MaxConnections`) produce a clear "server returned
  invalid register ack" error rather than a silent wrong-path failure.

- **`sendErr` non-blocking with buffered channel.** `errCh` is size 1. Multiple
  slots racing to report "all connections lost" are safe — the first wins, the
  rest drop. `runSession` only needs one error to trigger the reconnection loop.

- **Test coverage.** `TestRunSessionExpandsConnectionPool` verifies the actual
  wire messages: exactly one new-session register and two join-session registers
  arrive with the correct `SessionID`, `MaxConnections`, and `ProtocolVersion`.
  `TestRunSessionRequiresV2SessionMetadata` pins the hard-fail behaviour on a
  missing `SessionID` in the ack.

---

## Issues

### Issue 1 — `joinSessionConnection` sends `MaxConnections` on join; server ignores it but it creates a misleading contract (minor)

> `pkg/client/client.go:532`

```go
register := tunnel.Message{
    ...
    MaxConnections: pool.maxConns,
}
```

The spec's join handshake doesn't include `MaxConnections` — the pool size is
already negotiated at session creation. The server's `joinSession` path doesn't
read this field. Sending it is harmless but slightly misleading: someone reading
the join register would expect the server to act on it. Consider omitting
`MaxConnections` from the join register, matching the spec's handshake diagram
exactly.

### Issue 2 — `pool.remove` lowest-index promotion scans the full slice (observation)

> `pkg/client/pool.go:93`

```go
for i, connRef := range p.conns {
    if connRef == nil {
        continue
    }
    if nextIndex == -1 || i < nextIndex {
        nextIndex = i
    }
}
```

Because `p.conns` is indexed and iterated in order, the first non-nil entry
found is already the lowest index — the `i < nextIndex` branch can never be
true after the first match. The loop is correct but the inner condition is
redundant. Can simplify to `break` after the first non-nil entry. No behaviour
change needed.

---

## Test Results

```
ok  github.com/antoinecorbel7/plex-tunnel/pkg/client  22.044s
```

All tests pass, race detector clean.

---

## Acceptance Criteria Checklist

From the Phase 3 TODO in `specs/parallel-connections-design.md`:

- [x] Add `MaxConnections int` to `Config`. Env var `PLEXTUNNEL_MAX_CONNECTIONS`, default `4`.
- [x] Implement `ConnectionPool` in `pkg/client/pool.go`.
- [x] Update `runSession()`: send v2 `Register` with `MaxConnections`; require v2 `RegisterAck` with session metadata; store session ID; dial remaining connections staggered ~150 ms apart.
- [x] Update `handleHTTPRequest` — stream pinned to the connection it was dispatched on (server-side dispatch; client sends response chunks on the receiving connection).
- [x] Ping/pong on control connection (index 0) only.
- [x] On control connection drop: promote lowest-index surviving connection, re-dial replacement.
- [x] Update `go.mod` to new proto release.
- [ ] On data connection drop: streams pinned to that connection fail and are retried by the HTTP client. (Connections do drop and reconnect correctly, but there is no explicit test for in-flight request failure on connection drop — worth adding before merge to confirm the server's `failPendingForConn` path works end-to-end.)

---

## Verdict

**Approved** with one recommended fix before merge: drop `MaxConnections` from
the join register (Issue 1) to match the spec. Issue 2 is an observation with no
behaviour impact. The missing test for in-flight request failure on connection
drop is noted — it can ship as a follow-up if the server-side test covers it.

---

## Round 2 — Follow-up changes (commit `db8d31d`)

### What changed

All three round 1 findings addressed:

**Issue 1 resolved.** `MaxConnections` removed from the join register in
`joinSessionConnection`. The field is no longer sent on the join path, matching
the spec's handshake diagram exactly.

**Issue 2 resolved.** `pool.remove`'s promotion loop simplified — the redundant
`i < nextIndex` condition replaced with a `break` after the first non-nil entry,
since the slice is iterated in order and the first match is always the
lowest index.

**Test updated.** `TestRunSessionExpandsConnectionPool` now checks
`MaxConnections` only on the new-session register (where it is required), not on
join registers (where it is no longer sent). The assertion is moved inside the
`SessionID == ""` branch, tightening the contract.

**Proto pre-release bumped** to pick up any proto-side changes made alongside
this work.

### Correctness

- Removing `MaxConnections` from the join register is safe: the server's
  `joinSession` path never read that field, so no server-side behaviour changes.
- The `break`-on-first-match simplification is correct: `pool.remove` iterates
  `p.conns` from index 0, so the first non-nil entry is by definition the
  lowest index. The previous `i < nextIndex` branch could never be true after
  the first match.

### Test results

```
ok  github.com/antoinecorbel7/plex-tunnel/pkg/client  21.984s
```

All tests pass, race detector clean.

### Verdict

**Approved.** All round 1 findings resolved. No remaining blocking review
items. Follow-up still recommended: add an explicit end-to-end test covering an
in-flight request pinned to a data connection that drops mid-stream.
