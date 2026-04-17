# Client-side WebSocket tunneling + multiplexing flow control (#92)

## Context

Issue `#92` on `plex-tunnel`. The binding specification for protocol changes is
ADR 0001 in `plex-tunnel-proto` PR #24, readable at
`/home/dev/worktrees/paul/plex-tunnel-proto/docs/adr/0001-websocket-multiplexing-flow-control.md`.
Read it in full before starting.

State on the ground:

- **Proto is NOT tagged yet.** PR #24 currently contains the ADR text only;
  the symbols `MsgWSWindowUpdate`, `CapWSFlowControl`, `WindowIncrement` do
  not exist as Go code. Paul (proto lead) will land them on branch
  `proto/ws-multiplexing` at `/home/dev/worktrees/paul/plex-tunnel-proto`.
  You MUST NOT edit that worktree — that's paul's scope.
- Once paul's code lands in that worktree, we consume it via a
  `go.mod` `replace` directive pointing at the worktree path. Noah (the lead)
  will add the `replace` directive before spawning you. If the symbols are
  missing at the time you start, stop and report back — do not invent
  placeholders.
- The client today has **no WebSocket-tunneling implementation**. In
  `pkg/client/client.go:365` the read-loop explicitly warns on
  `MsgWSOpen`/`MsgWSFrame`/`MsgWSClose` as "unsupported message type" and
  drops them. The server (`plex-tunnel-server`) already emits these — see
  `pkg/server/proxy.go:1072-1286` in that repo for the server-side reference
  implementation. This task builds the client-side counterpart from scratch.
- On top of that, we implement the ADR's credit-based flow control so a
  single busy WebSocket stream on the control socket cannot starve its
  peers.

## Goal

The client must:

1. Proxy tunneled WebSocket streams end-to-end: receive `MsgWSOpen` from the
   server, dial upstream Plex, ack with `MsgWSOpen`, pump frames in both
   directions, handle `MsgWSClose` in either direction.
2. Advertise `tunnel.CapWSFlowControl` in Register; record whether the server
   acks it in RegisterAck; when both sides advertise it, enforce per-stream
   credit-based flow control exactly as specified in ADR 0001.
3. Fall back cleanly to no-credit behavior when the server does not
   advertise `CapWSFlowControl` (older server), so old-server / new-client
   sessions continue to work during the staging/prod rollout window.

## Constraints / guardrails

- **DO NOT delete or modify code not mentioned in this task.**
- DO NOT touch `plex-tunnel-proto` at any path. Paul owns it. If a proto
  symbol is missing or shaped wrong, stop and surface it to noah.
- DO NOT touch the server repo. Grace owns it.
- DO NOT change the server-facing protocol shape; everything comes from
  ADR 0001. If the ADR conflicts with what feels right, the ADR wins.
- The WebSocket streams ride on the **control** tunnel connection (same
  socket as Register/Ping/MsgHTTPRequest traffic), per ADR. Do NOT route
  them through the data pool.
- Reuse the existing `nhooyr.io/websocket` dependency for upstream dials.
  Do NOT add `coder/websocket` or a second WS library.
- Logging: use zerolog via `c.logger` / the existing `requestLogger` /
  `slotLogger` helpers. Do NOT introduce a new logger.
- Metrics: the existing `pkg/client/metrics.go` has the pattern; wire new
  counters through it rather than directly calling prometheus.
- No panics in tunnel goroutines. Surface errors via return/MsgError, not
  via `panic`.
- Go 1.25, match existing formatting (gofmt, go vet, errcheck-clean).

## ADR-locked constants (use these exact names/values)

From ADR 0001:

- `tunnel.MsgWSWindowUpdate` = message type `14` (already declared in proto
  PR #24; do not redeclare).
- `tunnel.CapWSFlowControl` = `1 << 1` (bit flag on `Capabilities` in
  `Register` / `RegisterAck`).
- `tunnel.Message.WindowIncrement` — `uint32`, JSON tag `window_increment`.
- Initial per-stream send window: **65536 bytes** (64 KiB). Define as a
  named constant in `pkg/client/` (e.g., `wsInitialWindowBytes = 65536`).
- Threshold for emitting `MsgWSWindowUpdate`: once **≥ 32768 bytes**
  (`wsInitialWindowBytes / 2`) have been *consumed* (written to upstream
  Plex) since the last window update for that stream.
- Maximum cumulative pending credit per stream: `2^31 - 1`. If an incoming
  `MsgWSWindowUpdate` would push the pending credit past that bound, treat
  as a stream-scoped `FLOW_CONTROL_ERROR`.
- Maximum `MsgWSFrame.Body` length on the wire: `wsInitialWindowBytes`
  (65536 bytes). A larger received `MsgWSFrame` is a connection-level
  protocol error — tear down the session.

## Architecture

A new file `pkg/client/websocket.go` owns WebSocket stream state:

- A `wsStreamRegistry` (per `Client`, or per control connection — pick
  whatever maps cleanly onto the existing struct; one registry per logical
  client session is fine) maps `streamID (string) → *wsStream`.
- A `wsStream` owns:
  - The `*websocket.Conn` dialed to upstream Plex.
  - Two goroutines: `upstreamReader` (upstream → tunnel) and
    `upstreamWriter` (tunnel → upstream).
  - A `sendCredit` counter (starts at 65536) and a `sync.Cond` or a
    buffered-channel equivalent so the upstreamReader blocks when credit
    hits zero and wakes when a `MsgWSWindowUpdate` replenishes it.
  - A `consumedSinceUpdate` counter on the receive (upstream-write) side
    that drives `MsgWSWindowUpdate` emission.
  - A per-stream `sync.Mutex` guarding the credit counters.
- Registry operations are concurrency-safe (RWMutex around the map itself).

The new file provides:

```go
// Starts a stream, dials upstream, launches pumps.
// The `flowControlEnabled` bool comes from the RegisterAck result and
// controls whether credit accounting is enforced and MsgWSWindowUpdate
// emitted.
func (r *wsStreamRegistry) open(ctx context.Context, conn *tunnel.WebSocketConnection, msg tunnel.Message, flowControlEnabled bool) error

// Dispatches an inbound MsgWSFrame to the right stream's upstream writer.
// If the stream was already closed (late frame), silently drops.
func (r *wsStreamRegistry) frame(msg tunnel.Message)

// Closes a stream in response to server-originated MsgWSClose (or local
// error). Idempotent.
func (r *wsStreamRegistry) close(msg tunnel.Message)

// Replenishes send credit on a stream in response to MsgWSWindowUpdate.
// Returns an error that callers convert to a stream-scoped violation
// (MsgError + MsgWSClose 1008 for that stream only) on zero/overflow.
func (r *wsStreamRegistry) windowUpdate(msg tunnel.Message) error
```

## Tasks

- [ ] **Add `go.mod` replace** pointing at paul's worktree and bump to the
      draft proto version. Noah will do this step before spawning you; if
      `tunnel.MsgWSWindowUpdate` / `tunnel.CapWSFlowControl` /
      `tunnel.Message.WindowIncrement` are still unresolved when you start,
      stop and report.

- [ ] **Advertise `CapWSFlowControl` on Register** at
      `pkg/client/client.go:204-206` (control-connection handshake in
      `runSession`) AND at `pkg/client/client.go:865-867` (data-pool join
      handshake in `joinSessionConnection`-adjacent code). OR the flag into
      the existing `tunnel.CapLeasedPool` advertisement. Example:
      `Capabilities: tunnel.CapLeasedPool | tunnel.CapWSFlowControl`.

- [ ] **Record server capability in RegisterAck.** In the initial register
      ack handler (around `pkg/client/client.go:916-930`, inside
      `runSession`), extract a `flowControlEnabled := registerAck.Capabilities&tunnel.CapWSFlowControl != 0`
      and make it available to the WS dispatch path. The re-ack handler at
      `pkg/client/client.go:345-358` must apply the same rule and keep the
      state coherent (a server that drops the cap on re-ack is a protocol
      regression — log a warning and keep the previous value; do NOT turn
      on credit accounting once it was off, and do NOT turn it off once it
      was on for in-flight streams).

- [ ] **Remove the unsupported-message warn** at
      `pkg/client/client.go:365-366` for the three WS types (keep the warn
      for `MsgKeyExchange` only, which remains unsupported on the client),
      and replace with dispatch into the new `wsStreamRegistry`:
      - `MsgWSOpen` → `registry.open(...)`
      - `MsgWSFrame` → `registry.frame(msg)` (validate body length ≤ 64 KiB
        on receive; oversize = connection-level protocol error; return
        from `readLoop` with an error that tears down the session).
      - `MsgWSClose` → `registry.close(msg)`
      - `MsgWSWindowUpdate` (new case) → `registry.windowUpdate(msg)`;
        a returned error is stream-scoped: send `MsgError` with a reason
        string and `MsgWSClose{Status: 1008, ID: msg.ID}` for that stream
        only, then keep `readLoop` running.

- [ ] **Implement `wsStreamRegistry.open` in `pkg/client/websocket.go`:**
      - Resolve the upstream URL by swapping scheme on `c.cfg.PlexTarget`:
        `http→ws`, `https→wss`. Reuse or parallel the `resolveTargetURL`
        logic at `pkg/client/client.go:679-697` — do NOT duplicate its
        relative-path safety check; factor out a helper if needed.
      - Dial upstream with `websocket.Dial(ctx, url, &websocket.DialOptions{
        HTTPHeader: forwardedHeaders})`. Strip hop-by-hop headers the same
        way `handleHTTPRequest` does at `pkg/client/client.go:435-446`.
      - Set `upstreamConn.SetReadLimit(wsInitialWindowBytes)`.
      - On dial error: send `tunnel.Message{Type: tunnel.MsgError, ID: msg.ID,
        Error: "<reason>"}` and return — do NOT send MsgWSOpen ack.
      - On dial success: register the stream, send
        `tunnel.Message{Type: tunnel.MsgWSOpen, ID: msg.ID}` as ack (matches
        server expectation at `pkg/server/proxy.go:1116`), launch the two
        pumps.
      - Before dialing, take a **control** stream slot via
        `c.tryAcquireStreamSlot(true)` at `pkg/client/client.go:666` — WS
        streams live on the control socket, same as ADR says. On slot
        saturation, send `MsgError` (no MsgWSOpen ack) and stop.

- [ ] **Implement the upstream-reader pump (upstream Plex → tunnel):**
      - Loop on `upstreamConn.Read(ctx)`.
      - For each read: split into chunks of at most `wsInitialWindowBytes`
        (64 KiB). Preserve stream ordering by doing the sends sequentially
        on this goroutine; WS frame/message boundary preservation across
        splits is explicitly out-of-scope per ADR. Each chunk is a
        `tunnel.Message{Type: MsgWSFrame, ID: streamID, Body: chunk,
        WSBinary: msgType == websocket.MessageBinary}`.
      - Before each send: if `flowControlEnabled`, acquire credit for
        `len(chunk)` bytes — block on the stream's credit cond var if
        insufficient. Decrement by `len(chunk)` before send. If
        `!flowControlEnabled`, skip credit entirely.
      - Send via the control connection (reuse the per-connection write
        lock; `tunnel.WebSocketConnection.Send` already takes the lock).
      - On upstream-read error: send `MsgWSClose{ID: streamID}` (Status
        only set on known-cause closes, to mirror server), tear down the
        stream via the registry.

- [ ] **Implement the upstream-writer pump (tunnel → upstream Plex):**
      - Served from a per-stream buffered channel the `frame(msg)`
        dispatcher writes into. Size the channel at 1 or 2; we don't want
        unbounded buffering — credit accounting on the server side bounds
        it.
      - On each frame: write to upstream (`upstreamConn.Write(ctx,
        wsBinaryOrText, msg.Body)`), then track consumed bytes for window
        updates: `consumedSinceUpdate += len(msg.Body)`. When
        `consumedSinceUpdate >= wsInitialWindowBytes/2` AND
        `flowControlEnabled`, emit `MsgWSWindowUpdate{ID: streamID,
        WindowIncrement: uint32(consumedSinceUpdate)}` and reset to 0.
      - On upstream-write error: send `MsgWSClose{ID: streamID}`, tear
        down.

- [ ] **Implement `wsStreamRegistry.close`:** close the upstream conn with
      the status code from `msg.Status` (default `websocket.StatusNormalClosure`),
      cancel both pumps, deregister, and release the control stream slot.
      Must be idempotent (the pumps may race to close). Any `MsgWSFrame`
      that arrives after `close` for the same stream ID is silently
      discarded in `frame(msg)` (per ADR line 78-79).

- [ ] **Implement `wsStreamRegistry.windowUpdate`:** resolve the stream;
      if unknown (closed/never existed), silently ignore.
      - If `msg.WindowIncrement == 0` → return
        `errWindowIncrementZero` (stream-scoped).
      - If `len(msg.Body) != 0` → return `errWindowUpdateBody`
        (stream-scoped).
      - Atomically add the increment to pending credit under the stream
        mutex. If `pending + increment > 2^31 - 1` → return
        `errFlowControlOverflow` (stream-scoped).
      - Otherwise, add and wake any waiter on the credit cond var.
      The caller (read-loop dispatch) converts any of those errors into
      `MsgError` + `MsgWSClose{Status: 1008}` for that stream ID only.

- [ ] **Legacy fallback behavior:** when `flowControlEnabled == false`,
      all four bullets above apply *except*:
      - No credit check in the upstream-reader pump (just send frames).
      - No `MsgWSWindowUpdate` emission in the upstream-writer pump.
      - If the client *receives* a `MsgWSWindowUpdate` while flow control
        is disabled — that's a server-side protocol violation (the server
        promised not to emit them per ADR). Log a warning, drop the
        message, do NOT tear down (be liberal in what we accept).
      - The 64 KiB per-frame cap still applies (it's protocol-wide, not
        flow-control-gated).

- [ ] **Graceful shutdown:** when `readLoop` exits (tunnel teardown), the
      registry's owning context is canceled; each active stream closes its
      upstream conn with `StatusGoingAway` and exits its pumps. Verify no
      goroutine leaks.

## Tests

Place WebSocket tests in a new file `pkg/client/client_websocket_test.go`.
Use the same `httptest`/`nhooyr.io/websocket` harness pattern as
`pkg/client/client_handshake_test.go` and `pkg/client/client_sse_test.go`.

Required tests:

1. `TestRegisterAdvertisesCapWSFlowControl` — the outbound Register carries
   both `CapLeasedPool` and `CapWSFlowControl`.
2. `TestFlowControlEnabledWhenServerAcksCap` — after RegisterAck with
   `CapLeasedPool|CapWSFlowControl`, the client's internal flag is true.
3. `TestFlowControlDisabledWhenServerOmitsCap` — server acks with only
   `CapLeasedPool` → flag false, no `MsgWSWindowUpdate` emitted for the
   rest of the test.
4. `TestWSOpenDialsUpstreamAndAcks` — server sends `MsgWSOpen`; client
   dials the fake upstream; client emits `MsgWSOpen` ack with same ID.
5. `TestWSOpenDialFailureSendsMsgError` — upstream refuses connection;
   client emits `MsgError` with same ID, no ack.
6. `TestWSFrameServerToClientWritesUpstream` — server sends `MsgWSFrame`;
   bytes appear on the fake upstream socket; text vs binary flag honored.
7. `TestWSWindowUpdateEmittedAfterHalfWindow` — server sends frames
   totaling ≥ 32768 bytes; client emits exactly one
   `MsgWSWindowUpdate{WindowIncrement: >= 32768}` for that stream.
8. `TestWSFrameClientToServerSplitsAt64KiB` — upstream sends a single
   128-KiB message; client emits 2 `MsgWSFrame` each ≤ 64 KiB, same ID,
   preserving byte order.
9. `TestWSSenderBlocksOnZeroCredit` — drive client to zero credit by
   emitting 64 KiB upstream without `MsgWSWindowUpdate`; assert no
   further `MsgWSFrame` for 100ms; send `MsgWSWindowUpdate{32768}`;
   assert next frame arrives within 200ms.
10. `TestWSWindowUpdateZeroIncrementIsStreamScopedError` — server sends
    `MsgWSWindowUpdate{WindowIncrement: 0}`; client responds with
    `MsgError` + `MsgWSClose{Status: 1008}` for that stream only; other
    streams on the same session continue to work (open a second stream
    after and verify it proxies successfully).
11. `TestWSWindowUpdateBodyIsStreamScopedError` — same as (10) but with
    non-empty body.
12. `TestWSCreditOverflowIsStreamScopedError` — window update that would
    push credit past `2^31 - 1` triggers the same stream-scoped error.
13. `TestWSCloseFromServerTearsDownUpstream` — server sends `MsgWSClose`;
    upstream `conn.Read` returns an error within 200ms.
14. `TestWSLateFrameAfterCloseDropped` — server sends `MsgWSClose`, then
    `MsgWSFrame` with same ID; client does not crash, does not write to
    upstream, does not send a second `MsgWSClose`.
15. `TestWSOversizeFrameFromServerTearsDownSession` — server sends
    `MsgWSFrame` with body length `65537`; client closes the session (the
    fake server sees the tunnel WS close).
16. `TestWSLegacyModeNoWindowUpdates` — flow control disabled; drive
    ≥ 32768 bytes upstream; assert no `MsgWSWindowUpdate` emitted within
    500ms.
17. `TestWSStreamSlotGatedByControlSemaphore` — saturate the control
    stream slot pool; client refuses a new `MsgWSOpen` with `MsgError`
    (no ack, no upstream dial attempt).

## Acceptance criteria

- All listed tests pass.
- `go test ./...` from repo root is green.
- `go vet ./...` is clean.
- Running against a fresh `plex-tunnel-server` (any version that advertises
  `CapLeasedPool|CapWSFlowControl`): a real browser `ws://…/:/websockets/notifications`
  handshake through the tunnel reaches a real Plex server and receives
  notifications. (Manual smoke test; not automated.)
- Running against a `plex-tunnel-server` build that advertises only
  `CapLeasedPool` (legacy): WebSocket streams still work, no
  `MsgWSWindowUpdate` is emitted, no errors are logged for receive-side
  mechanics.
- No new goroutine leaks (compare `goleak` if available; otherwise the
  test harness's existing leak pattern).

## Verification

```bash
cd /home/dev/worktrees/noah/plex-tunnel
go build ./...
go vet ./...
go test ./... -count=1
```

## Follow-up (not in scope for this PR)

- Drop the `go.mod` replace directive and bump to the tagged proto version
  once `plex-tunnel-proto` PR #24 merges and is tagged. Tracking: proto PR
  #24.
- Preserving WebSocket message/frame boundaries across 64 KiB chunks is
  out of scope per ADR 0001; if a future use case needs it, a follow-up
  ADR will specify the semantics.
