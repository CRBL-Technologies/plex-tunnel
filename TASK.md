# Leased tunnel pool — multi-repo implementation

## Context

The current data plane multiplexes per-tenant requests over a small set of
WebSocket lanes that all share framed bookkeeping (a 32-slot pending-response
queue per request goroutine). Three production incidents over the last 6 weeks
trace back to that design:

1. **Backpressure HOL:** when one downstream client stops draining, the tunnel
   read loop stalls and starves keepalives + other in-flight requests on the
   same session.
2. **Historical 2m write-deadline truncation (already hot-fixed):** the public
   data path used to set `http.Server.WriteTimeout`, producing committed
   `200/206` headers with short bodies and downstream `unexpected EOF`. The
   server **must not** reintroduce a public or tunnel `WriteTimeout`.
3. **2026-04-08/09 SSE saturation:** Plex SSE long-polls
   (`/:/eventsource/notifications`) hold lanes for ~20s. ~5 devices × ~5
   long-polls = ~25 lanes occupied for hours, plus 4-9-lane downloads needing
   to stripe — pool exhaustion → tenant 503 storms.

The CTO and CEO have signed off on a leased-tunnel-pool redesign. The
canonical plan lives in `crbl-infra/infrastructure/plex-tunnel-leased-pool.md`
(commit `8501887`). **That doc is canonical. If this TASK.md disagrees with
the doc, the doc wins. If your implementation contradicts the doc, the bug is
in your implementation — fix the implementation, not the doc.**

## Goal

Replace the framed multiplexed data plane with a leased-exclusive tunnel pool:

- Each NAS session keeps **1 control WebSocket** + **N data WebSockets**.
- The control WS multiplexes small/control-shaped HTTP traffic (SSE,
  identity, metadata, hubs, timeline POSTs, plus the existing non-HTTP
  control plane).
- Data WS lanes are **leased exclusively** for the lifetime of one in-flight
  HTTP request (or one striping segment).
- Pool size is **per-tenant per-tier**, sourced from `auth.TokenEntry`. The
  wire `MaxConnections` field continues to mean **total slots**, so the
  server advertises `data_tunnels + 1`.
- On idle data-pool exhaustion, return **`429 Too Many Requests`** with
  `Retry-After: 1`. Striping must degrade (down to 1 lane) before rejecting.
- Registration is gated by a **leased-pool capability** advertised by the
  client. Old clients keep legacy behaviour or are rejected — the server
  never silently treats a non-capable client as lease-capable.

This is a **3-repo change**. Implement in the order **proto → server →
client**, because server and client both depend on the proto bump.

---

## Constraints / guardrails (read these before touching anything)

1. **DO NOT delete or modify code not mentioned in this task.** If a function,
   constant, or struct field is not named below, leave it alone. If you think
   something needs to be removed, surface it in the PR body and stop — do not
   remove it on your own initiative.
2. **DO NOT reintroduce `WriteTimeout` on `publicServer` or `tunnelServer`**
   in `plex-tunnel-server/pkg/server/server.go:236-272`. The inline comment
   block there explains the 2026-04-08 truncation incident; preserve both the
   behaviour and the comment.
3. **DO NOT replace `nhooyr.io/websocket`** or change the framing format
   beyond what is explicitly listed under the proto section. The wire is
   length-prefixed binary `tunnel.Message`s and stays that way.
4. **DO NOT regress the per-request drainer goroutine** in
   `plex-tunnel-server/pkg/server/session.go:24-130` (`pendingRequest`,
   `deliverResponse`). The buffered per-request channel + drainer is the
   reason backpressure stopped tearing down sessions; the new leased-data
   path keeps that pattern, just with one in-flight request per data conn
   instead of N.
5. **DO NOT design for hypothetical futures.** No QUIC scaffolding, no
   MPTCP, no generic protocol mux, no operator feature flag. The migration
   gate is the protocol-version capability — that is the only flag.
6. **DO NOT push directly to `main` or `dev`.** All changes go through the
   feature branches that already exist (`feat/leased-tunnel-pool-proto`,
   `feat/leased-tunnel-pool`, `feat/leased-tunnel-pool-client`).
7. **DO NOT use `--no-verify`, `--amend`, or `git push --force`** for any
   reason. If a hook fails, fix the underlying problem and commit again.
8. **DO NOT assume the existing 5/10/20 plan defaults are correct.** They
   represent the *old* total-conn model (1 multiplexed conn). The new model
   has total = `data + 1`. Update plan defaults explicitly under the server
   section below; don't just edit one and forget the others.

---

## Repo 1: `plex-tunnel-proto`

**Branch:** `feat/leased-tunnel-pool-proto`
**Worktree:** `/home/dev/worktrees/paul/plex-tunnel-proto`
**PR base:** `main`

### Files in scope

- `tunnel/message.go`
- `tunnel/message_test.go` (new tests, do not delete existing)

### Changes

#### P1. Bump `ProtocolVersion` to `3`

`tunnel/message.go:30` currently declares:

```go
const ProtocolVersion uint16 = 2
```

Bump to `3`. **Both peers must speak v3** to use leased-pool semantics. The
server-side compat gate (server section) rejects anything that isn't exactly
`3` once the cutover lands. v2 stays around only as a *symbol* for client
backwards-compat builds; the server stops accepting it.

#### P2. Add capability bits to the `Message` struct

`tunnel/message.go:34` defines the `Message` struct. Add a single new field:

```go
// Capabilities is a bitmask of optional protocol features negotiated during
// register/register-ack. Peers MUST treat unknown bits as reserved-zero.
Capabilities uint32 `json:"capabilities,omitempty"`
```

Place it after `MaxConnections int` so the JSON layout stays grouped with the
session-handshake fields.

Add a typed constant block above the `ProtocolVersion` line:

```go
// Capability bits are advertised on MsgRegister/MsgRegisterAck and govern
// optional protocol behaviour for the lifetime of the session.
const (
    // CapLeasedPool indicates the peer supports the leased-tunnel-pool
    // data plane (1 control WS + N data WS, leased-exclusive data lanes).
    // Both peers MUST advertise this bit for the server to enable
    // leased-pool routing for the session.
    CapLeasedPool uint32 = 1 << 0
)
```

Do **not** add any other capability bits in this PR.

#### P3. Update `Validate()` for the new fields

`tunnel/message.go:60` `Validate()`:

- `MsgRegister`: require `m.ProtocolVersion >= ProtocolVersion`. (Already
  enforced — confirm the bumped constant flows through.)
- `MsgRegisterAck`: same.
- No new validation rules for `Capabilities` itself — `0` is a legal value
  meaning "no optional capabilities". Unknown bits MUST be ignored, not
  rejected (forward-compat).

#### P4. Tests

Add to `tunnel/message_test.go` (create the file if it doesn't exist; do
**not** wipe existing tests in any sibling file):

- `TestProtocolVersionIsThree` — guards against accidental down-revs.
- `TestCapLeasedPoolBitIsOne` — guards the wire value.
- `TestValidateRegisterAcceptsLeasedPoolCapability` — Register message with
  `Capabilities: CapLeasedPool` and `ProtocolVersion: 3` validates clean.
- `TestValidateRegisterIgnoresUnknownCapabilityBits` — `Capabilities: 0xFF00`
  validates clean (forward-compat).
- `TestValidateRejectsOlderProtocolVersion` — `ProtocolVersion: 2` on a
  Register message returns an error mentioning the minimum.
- Round-trip a Register message through `NewFrame` + `frame.MarshalBinary` +
  decode and assert the `Capabilities` field survives. (Use whatever existing
  helper handles encode/decode in this repo — do not invent a new codec.)

### Verification

```
cd /home/dev/worktrees/paul/plex-tunnel-proto
go vet ./...
go build ./...
go test ./...
```

All three must pass. Do not skip `go vet`.

---

## Repo 2: `plex-tunnel-server`

**Branch:** `feat/leased-tunnel-pool`
**Worktree:** `/home/dev/worktrees/paul/plex-tunnel-server`
**PR base:** `dev`

### Files in scope

- `pkg/auth/plan.go` — update plan defaults
- `pkg/server/config.go` — add pool env vars
- `pkg/server/server.go` — registration gate, granted-max derivation
- `pkg/server/session.go` — control vs data conn typing, leased checkout
- `pkg/server/proxy.go` — classify + lease + 429 path
- `pkg/server/striping.go` — striping cap, lease-from-data-pool only
- `pkg/server/classifier.go` (NEW) — path classification table
- `pkg/server/metrics.go` — new counters/histograms
- `pkg/server/router.go` — only if needed for classifier dependency wiring
- Tests under `pkg/server/*_test.go`
- `go.mod` — bump `plex-tunnel-proto` to the new commit (after proto merges)

### Changes

#### S1. Plan defaults (`pkg/auth/plan.go`)

`pkg/auth/plan.go:43-65` `PlanDefaultsMap`:

The wire `MaxConnections` continues to mean **total slots = data + 1**. Update:

| Plan | Old `MaxConnections` | New `MaxConnections` | Why |
|---|---:|---:|---|
| Free | `5` | `4` | 3 data + 1 control |
| Pro | `10` | `11` | 10 data + 1 control |
| Max | `20` | `21` | 20 data + 1 control |

Also update the migration / DB seed if `pkg/auth/plan.go` (or
`pkg/auth/store.go` / `migrations/`) carries the seed values; grep for
`max_connections` in `pkg/auth/` and flag any DB migration that needs an
accompanying SQL update. **If a migration is needed, write it; do not skip.**
If the plan values come from a runtime SQL `UPDATE`, update
`pkg/auth/plan.go:96` `GetPlanSettings` callers' bootstrap path so a fresh DB
seeds the right defaults.

`PlanDefaultsMap` is the in-memory fallback used when the DB has no row.
Both must agree.

#### S2. Pool env vars (`pkg/server/config.go`)

Add to the `Config` struct at `pkg/server/config.go:15`:

```go
// PoolFreeData is the number of leased data tunnels granted to the Free tier.
// Wire MaxConnections = PoolFreeData + 1 (control). Defaults to 3.
PoolFreeData int
// PoolProData — Pro tier data tunnels. Defaults to 10.
PoolProData int
// PoolMaxData — Max tier data tunnels. Defaults to 20.
PoolMaxData int
// PoolMaxAbsolute clamps every tier's effective data pool. Defaults to 64.
PoolMaxAbsolute int
```

In `LoadConfig` (find it via grep, around the existing
`parseIntEnv("PLEXTUNNEL_SERVER_TRUSTED_PROXIES"...` block at
`pkg/server/config.go:220`), add:

```go
PoolFreeData:    parseIntEnv("PLEXTUNNEL_POOL_FREE", 3),
PoolProData:     parseIntEnv("PLEXTUNNEL_POOL_PRO", 10),
PoolMaxData:     parseIntEnv("PLEXTUNNEL_POOL_MAX", 20),
PoolMaxAbsolute: parseIntEnv("PLEXTUNNEL_POOL_MAX_ABSOLUTE", 64),
```

(use the existing helper, do not introduce a new one.)

Add a `Config.Validate()` clause: each pool value must be >= 1, and
`PoolMaxAbsolute >= 1`. If a pool exceeds `PoolMaxAbsolute`, clamp at
load-time and emit a warn log via the existing logger pattern.

#### S3. Granted-max derivation (`pkg/server/server.go`)

`pkg/server/server.go:1092` `grantedMaxConnections`:

The current implementation clamps `requested` by `entry.MaxConnections` and
`r.maxConnsPerSubdomain()`. Extend it as follows. The function MUST:

1. Resolve the **tier-based data tunnel cap** from `entry.Plan` using the
   new env-driven config:
   - `PlanFree` → `cfg.PoolFreeData`
   - `PlanPro`  → `cfg.PoolProData`
   - `PlanMax`  → `cfg.PoolMaxData`
   - empty / unknown → `cfg.PoolFreeData` (safest default)
2. Clamp by `cfg.PoolMaxAbsolute` (data only).
3. Compute total slots = `dataTunnels + 1` (the +1 is the control WS).
4. Clamp by `entry.MaxConnections` if non-zero (per-token override or
   plan-derived total — already total-shaped, no off-by-one).
5. Clamp by `r.maxConnsPerSubdomain()` if non-zero (server-wide ceiling).
6. Clamp by `requested` (client never gets more than it asked for).
7. Floor at `2` (1 data + 1 control). A session cannot operate with zero
   data tunnels; if even Free is misconfigured to 0 data, return an error
   from `registerNewSession` rather than silently granting 1.

The current comment that says `granted = requested` semantics are preserved,
but the clamp order changes. Add a unit test that exercises every clamp.

#### S4. Compat gate at registration (`pkg/server/server.go`)

`pkg/server/server.go:559` currently rejects any client whose
`ProtocolVersion != tunnel.ProtocolVersion`. Once the proto bump lands the
constant is `3` so this gate will already reject v2 clients — but the gate
must **also** require the leased-pool capability bit:

After the existing `register.ProtocolVersion != tunnel.ProtocolVersion`
check (`pkg/server/server.go:559-570`), add:

```go
if register.Capabilities&tunnel.CapLeasedPool == 0 {
    _ = conn.Send(tunnel.Message{
        Type: tunnel.MsgError,
        Error: "tunnel client does not advertise leased-pool capability; upgrade plex-tunnel client",
    })
    _ = conn.Close()
    return
}
```

Mirror the bit on the `MsgRegisterAck` reply at
`pkg/server/server.go:615-621`:

```go
if err := conn.Send(tunnel.Message{
    Type:            tunnel.MsgRegisterAck,
    Subdomain:       session.subdomain,
    ProtocolVersion: tunnel.ProtocolVersion,
    SessionID:       session.sessionID,
    MaxConnections:  grantedMax,
    Capabilities:    tunnel.CapLeasedPool,
}); err != nil {
```

Both `registerNewSession` and `joinSession` flow through this code path —
do not duplicate the gate; verify both paths are covered.

#### S5. Path classifier (NEW: `pkg/server/classifier.go`)

Create a new file `pkg/server/classifier.go`. It exports one function:

```go
package server

// RouteClass distinguishes traffic that runs on the singleton control
// WebSocket from traffic that leases a data WebSocket exclusively.
type RouteClass int

const (
    RouteClassData    RouteClass = iota // leases a data tunnel exclusively
    RouteClassControl                   // multiplexes on the control tunnel
)

// ClassifyRequest returns the route class for an inbound proxied request.
// The classification table comes from
// crbl-infra/infrastructure/plex-tunnel-leased-pool.md "Traffic
// classification" — keep that doc and this function in sync.
//
// Unmatched paths default to data by design: never risk surprise large
// bodies on the singleton control channel. Widen the control allowlist
// only from observed traffic captures (and update the doc in the same PR).
func ClassifyRequest(method, path string) RouteClass {
    // ...
}
```

The body MUST implement the table from the doc. Required matches (case
insensitive on the prefix, exact on the literal path components):

**Control class:**
- `GET /:/eventsource/notifications` and any `GET /:/eventsource/*`
- `GET /identity`
- `GET /media/providers` and `GET /media/providers/*`
- `GET /library/metadata/<id>` and `GET /library/metadata/<id>/*`
   *(but NOT `/library/parts/...`, which is data)*
- `GET /library/sections` and `GET /library/sections/*`
- `GET /hubs` and `GET /hubs/*`
- `GET /status/sessions` and `GET /status/sessions/*`
- `POST /:/timeline` and `POST /:/timeline/*`

**Data class (explicit, even though data is the default):**
- `GET /downloadQueue/<id>/media*`
- `GET /library/parts/<id>/file*`
- `GET /library/parts/<id>/*` (any other path with that prefix; the spec
  documents that ranged reads stay on data)
- `GET /video/:/transcode/*`
- `GET /photo/:/transcode/*`

**Default:** any unmatched method/path returns `RouteClassData`.

WebSocket-upgrade requests (handled by `handleWebSocketPassthrough` at
`pkg/server/proxy.go:711`) keep their existing path; do not classify them
through this function — they already have a dedicated handler.

Add a per-route-class metric counter (see S10).

#### S6. Session typing: control conn + data pool (`pkg/server/session.go`)

`pkg/server/session.go:132` `ClientSession` already has a `controlConn` field
and a `conns []*sessionConn` slice. **Re-purpose `conns` as the data pool
only**, and make the control/data distinction a hard invariant:

- The first connection that registers for a session becomes the control
  conn. Subsequent joins go into the data pool.
- The control conn is **never** in `conns`. The data pool size is
  `grantedMax - 1` (subtract the control slot).
- `addConnection` (`pkg/server/session.go:174`) becomes typed:
  `addControlConnection` and `addDataConnection`. Choose which one based on
  whether `s.controlConn == nil` at the time of join.
- `removeConnection` becomes `removeControlConnection` /
  `removeDataConnection`. The transient-promotion behaviour described in
  the doc (§Lifecycle and §Proposed architecture) means: when the control
  conn dies, the **oldest idle data conn** is transiently promoted as
  control until a replacement registers. Track promotion with a
  `controlPromotedFrom *sessionConn` field; clear it when the new control
  conn arrives. Reject any new lease attempts for the promoted conn while
  it serves as control.
- The promotion trigger is **either** control-conn EOF **or** breach of
  the consecutive-keepalive-failure threshold (define as `3` in a
  `const controlKeepaliveFailureThreshold = 3` in this file). Wire the
  threshold check into the existing ping/pong path; if there is no
  per-conn keepalive bookkeeping yet, add a small `lastPongAt atomic.Int64`
  on `sessionConn` and check it in the existing keepalive loop.

Add a `leaseDataConn` method:

```go
// leaseDataConn returns an idle data conn for exclusive use, or
// (nil, false) if no idle data conn is available. The caller MUST call
// releaseDataConn when the request finishes (success or failure).
func (s *ClientSession) leaseDataConn(requestID string) (*sessionConn, chan tunnel.Message, bool)
```

Implementation rules:

- A data conn is **idle** iff `connRef.activeStreams.Load() == 0` AND it is
  not the currently-promoted control conn. CAS the count from `0` to `1`
  to take the lease atomically (use a spin over `atomic.Int64.CompareAndSwap`
  rather than holding the session lock).
- If no idle data conn exists, return `(nil, nil, false)`. **Do not block,
  do not queue.** The caller turns this into a 429.
- On a successful lease, register a `pendingRequest` exactly the same way
  `addPendingSelected` does today (`pkg/server/session.go:309`), so the
  per-request drainer goroutine still serializes responses without
  blocking the WS read loop.

Add a `leaseControlConn` method that behaves analogously but **does not
require exclusive ownership** — the control conn can multiplex many
in-flight requests. Use the same `pendingRequest` machinery.

Both lease methods return `(*sessionConn, chan tunnel.Message, bool)` so the
caller (`proxy.go`) does not need to know which class it got back. The third
return is `false` only on failure.

#### S7. Proxy routing + 429 (`pkg/server/proxy.go`)

`pkg/server/proxy.go:347` `handleClientRequest`:

After `maybeHandleStripedClientRequest` returns `false`
(`pkg/server/proxy.go:408`) and **before** `reservePendingRequest` at line
`413`, classify the request:

```go
class := ClassifyRequest(req.Method, requestPath)
if r.metrics != nil {
    r.metrics.ObserveRouteClass(class)
}
```

Replace the existing call to `r.reservePendingRequest(...)` with a new
helper `reserveLeasedRequest(subdomain, requestID, session, class)` that:

- For `RouteClassControl`: calls `session.leaseControlConn(requestID)`.
- For `RouteClassData`: calls `session.leaseDataConn(requestID)`.
- On data-class lease failure with no idle data conn:
  - emit a `lease_reject_total` metric tagged with the tier
  - return `(nil, nil, nil, false, true)` so the caller can write
    `429 Too Many Requests` with `Retry-After: 1`

When the caller sees a leased-data 429 case, write:

```go
w.Header().Set("Retry-After", "1")
http.Error(w, "Tunnel data pool exhausted", http.StatusTooManyRequests)
```

This is **distinct** from the existing
`Service temporarily overloaded → 503` path used by `globalInflight`. Do
not collapse them.

For control-class exhaustion (the spec also caps per-tenant SSE
concurrency), reuse the same 429 pattern but with a separate metric label
`class="control"`. The exact SSE cap is an open question in the doc — for
this PR, hard-code a generous default of `8` concurrent SSE long-polls per
tenant, exposed via `cfg.MaxControlSSEPerTenant` env var
`PLEXTUNNEL_CONTROL_SSE_MAX` (default `8`). Track active SSE count on the
session via `session.activeControlSSE atomic.Int64`. SSE classification:
any control-class GET to `/:/eventsource/*`. **Reject excess SSE with 429,
do not spill onto the data pool.**

`handleWebSocketPassthrough` (`pkg/server/proxy.go:711`) is unchanged for
this PR — WS upgrades still take a leased data conn. Update the call to use
`leaseDataConn` instead of `reservePendingRequest`.

#### S8. Striping (`pkg/server/striping.go`)

`pkg/server/striping.go:527` `stripingConnections` currently slices off
`snapshot[1:]`. Replace this with the new typed data pool. Striping leases
**from the data pool only** — never from the control conn or the promoted
control conn.

Striping width derivation in `pkg/server/striping.go:18-22`:

Add a per-session helper:

```go
// stripingMaxLanes returns the per-tier striping ceiling.
//   M_cap = max(1, floor(data_tunnels / 2))
func (r *Server) stripingMaxLanes(dataTunnels int) int {
    cap := dataTunnels / 2
    if cap < 1 {
        return 1
    }
    return cap
}
```

`buildStripedRequestCandidate` (`pkg/server/striping.go:397`) currently
requires `len(snapshot) >= 3`. Replace with:

- compute `dataTunnels := session.dataPoolSize()`
- compute `mCap := r.stripingMaxLanes(dataTunnels)`
- compute `idle := session.idleDataConnCount()`
- granted lanes `M = min(idle, mCap, requestedLanes)` where requested comes
  from the existing buffer-size heuristic
- if `M < 1`: skip striping (the caller falls back to the single-lease
  path, which may itself 429 if no data conn is idle)
- if `M >= 1` but less than the original requested width: **degrade
  gracefully**, do not abort. Emit a metric `striping_width_granted` with
  the requested vs granted values.

`executeStripedSegment` (`pkg/server/striping.go:611`) and the lane-pinning
in `addPendingOnConn` (`pkg/server/striping.go:120`) keep the same shape but
must lease via the new `leaseDataConn` so the lease accounting is unified.
On segment failure **before bytes are committed downstream**, the segment
may be retried on another idle data conn (per the doc Failure-modes
section). Once any byte for a segment has been written downstream, that
specific segment is not transparently retried.

#### S9. Failure modes / retry semantics

`pkg/server/proxy.go:509` `sendHTTPRequestStream` and the subsequent
response drain loop (`pkg/server/proxy.go:532`):

- Track a `wroteAnyByte bool` next to the existing `wroteHeaders bool` on
  the response loop (line ~527). Headers committed = no header retry;
  body bytes committed = no body retry. Both flip the flag.
- On `pending` channel error / `MsgError` / leased-conn death **before**
  `wroteHeaders == true`, the data path may attempt **one** retry **only
  if** the request is `GET` (or `HEAD`) and either has no `Range` header
  or has a `Range` header (`Range`-aware servers are idempotent for the
  retry). Use `req.Method == http.MethodGet || req.Method == http.MethodHead`
  as the gate. Drop POST / PUT / DELETE / PATCH retries entirely.
- Retry leases a fresh idle data conn via `leaseDataConn`. If no idle conn
  is available for the retry, return 429 instead of the original 502 — the
  retry path must not hide pool exhaustion.
- Control-channel requests may retry once **if idempotent and no response
  bytes were emitted**, same gating.

Encode these rules in a new helper at the bottom of `proxy.go`:

```go
// canRetryIdempotent reports whether a request is safe to transparently
// retry on a fresh tunnel after a transport-level failure that occurred
// BEFORE any response bytes were committed downstream.
func canRetryIdempotent(req *http.Request, wroteAnyByte bool) bool {
    if wroteAnyByte {
        return false
    }
    switch req.Method {
    case http.MethodGet, http.MethodHead:
        return true
    }
    return false
}
```

Do not introduce general HTTP retry middleware. This single helper is the
only retry policy.

#### S10. Observability (`pkg/server/metrics.go`)

Add to the existing metrics struct (find it via grep — it lives in
`pkg/server/metrics.go`):

- `controlWSConnected` / `controlWSDisconnected` — counter pair, labelled
  by tier
- `controlRequestLatency` — histogram (p50/p95/p99 derivable), labelled by
  route class
- `controlInflight` — gauge
- `dataConnState` — gauge with label `state` ∈ {idle, leased, reconnecting}
- `leaseWaitTime` — histogram (will be near-zero in the lease-or-429 model
  but is the right primitive for observability)
- `leaseHoldTime` — histogram, labelled by route class
- `leaseRejects` — counter, labelled by tier and class
- `stripingWidthRequested` / `stripingWidthGranted` — histograms
- `stripingSegmentRetries` / `stripingSegmentFailures` — counters
- `routeClassTotal` — counter labelled by class (`control`, `data`)
- `unmatchedRouteTotal` — counter labelled by **normalised path prefix**
  (do not use raw paths — high-cardinality is a Prometheus footgun;
  normalise `/library/metadata/12345` → `/library/metadata/:id`)
- `controlSSEConcurrent` — gauge
- `poolHighWater` — gauge labelled by tier

`isDownloadPath` and `isSSEPath` at `pkg/server/metrics.go:164-178` are
fine — leave them. The classifier above is the new authority for routing,
but the existing helpers still feed download-failure classification.

Access logs (`pkg/server/proxy.go:463`): add `route_class` (control / data),
`tunnel_id` (the `connRef.index`), `session_id`, `lease_duration_ms`. The
existing `truncated` / `write_deadline_hit` fields stay.

#### S11. Tests (`pkg/server/*_test.go`)

These are **mandatory**. The PR is not done without all of them passing.

- `TestGrantedMaxConnections_Tiers` — Free → 4, Pro → 11, Max → 21, with
  `PoolMaxAbsolute` clamping a misconfigured tier of 1000 down to 65 (64
  data + 1 control).
- `TestClassifyRequest_TableDriven` — one assertion per row in the doc
  table, plus an "unmatched defaults to data" case and a
  `POST /library/metadata/123` case (POST is unmatched → data).
- `TestLeaseDataConn_ReturnsExclusive` — two parallel leases against a
  pool of size 2 succeed, third blocks-not, 429-instead.
- `TestLeaseDataConn_PromotedControlIsExcluded` — promoting a data conn as
  control marks it ineligible for new leases.
- `TestSessionControlConnectionIsolatedUnderDataBackpressure` — simulate
  a slow downstream consumer holding a data lease, verify the control
  conn keepalive ping/pong loop continues and the session is not torn
  down. (Use a `tunnel.WebSocketConnection` test double or a real
  loopback WS — pick whichever the existing tests already use and stay
  consistent.)
- `TestRetryIdempotentBeforeBytesCommitted` — simulate a leased data conn
  dying before headers are written; verify a single retry on a fresh
  data conn succeeds. Then simulate the same death after one body byte
  has been written; verify no retry is attempted and the client sees a
  truncated/aborted response.
- `TestRetryNonIdempotentNotRetried` — `POST /:/timeline` failure pre-commit
  is not retried.
- `TestStaleDataConnReplaced` — disconnect a data conn, verify the pool
  count drops, the session does not die, and a fresh data conn join
  refills the slot.
- `TestSSELongPollUsesControlChannel` — `GET /:/eventsource/notifications`
  classifies to control, holds the control conn for the simulated 20s,
  and does NOT consume any data lease.
- `TestSSEConcurrencyCap429` — exceed `cfg.MaxControlSSEPerTenant`,
  verify 429 + `Retry-After: 1` and no spill onto data pool.
- `TestStripingDegradesGracefully` — request 5 lanes with 2 idle data
  conns; verify granted=2, no error.
- `TestStripingMCapPerTier` — Free=1, Pro=5, Max=10.
- `TestPoolExhaustionReturns429WithRetryAfter` — fully busy data pool,
  next data-class request gets 429 with `Retry-After: 1`.
- `TestRegisterRejectsClientWithoutLeasedPoolCapability` — Register with
  `Capabilities: 0` returns the documented error.

#### S12. `go.mod` bump

After the proto PR merges, bump `plex-tunnel-proto` in
`plex-tunnel-server/go.mod` to the new merge commit:

```
cd /home/dev/worktrees/paul/plex-tunnel-server
go get github.com/CRBL-Technologies/plex-tunnel-proto@<merge-commit-sha>
go mod tidy
```

Run `go vet ./... && go build ./... && go test ./...` and commit the
`go.mod` / `go.sum` delta.

### Verification

```
cd /home/dev/worktrees/paul/plex-tunnel-server
go vet ./...
go build ./...
go test ./...
```

All three must pass. No skipped tests, no `t.Skip` calls in the new tests.

---

## Repo 3: `plex-tunnel`

**Branch:** `feat/leased-tunnel-pool-client`
**Worktree:** `/home/dev/worktrees/paul/plex-tunnel`
**PR base:** `dev`

### Files in scope

- `pkg/client/client.go`
- `pkg/client/pool.go`
- `pkg/client/*_test.go`
- `go.mod` — bump proto

### Changes

#### C1. Advertise the leased-pool capability

`pkg/client/client.go:190` is where the client builds and sends the
`MsgRegister`. Add `Capabilities: tunnel.CapLeasedPool` to that message.

`pkg/client/client.go:720` `joinSessionConnection` does the same for
secondary slots — add the capability bit there too.

#### C2. Validate the server's capability bit on the ack

`pkg/client/client.go:777` `validateRegisterAck` currently checks the
protocol version. Extend it:

```go
if ack.Capabilities&tunnel.CapLeasedPool == 0 {
    return fmt.Errorf("server did not acknowledge leased-pool capability; refusing to use legacy data plane")
}
```

The client refuses to fall back to legacy mode. There is no legacy mode in
the new server — if the server doesn't ack the bit, the connection is
unusable.

#### C3. Promotion trigger surface

`pkg/client/client.go:343` `pingLoop` is the keepalive sender. The doc
defines the promotion trigger as **either control-conn EOF or breach of
the consecutive-keepalive-failure threshold**.

- Add `consecutiveControlPingFailures int` (or atomic) on the
  `sessionPoolController` (`pkg/client/client.go:38`).
- In `pingLoop`, on each pong: reset the counter to 0. On each missed
  pong (timer expiry without a pong): increment.
- Threshold: `const controlKeepaliveFailureThreshold = 3`. On reaching
  the threshold, treat it as a control EOF — call the existing
  control-loss path that triggers `pool.remove(controlIndex)` and
  promotion logic at `pkg/client/client.go:567` `maintainPoolSlot`.
- The promotion path itself already exists in `pkg/client/pool.go:102`
  `remove` and `pkg/client/client.go:567` (`controlLost, promoted` return
  values). Confirm it still works after C4 and add tests.

#### C4. Typed control vs data slots in `ConnectionPool`

`pkg/client/pool.go:17` `ConnectionPool` already tracks `controlIndex`.
This is fine. **Do not** rewrite the slot machinery — just enforce the
invariants:

- `Resize` (`pkg/client/pool.go:195`) must never close the control conn
  during a downsize. The current code already promotes if `controlIndex
  >= newMax`; verify this still holds and add a test
  `TestResizeNeverClosesControl`.
- `add` (`pkg/client/pool.go:59`) currently sets `controlIndex` only when
  `activeBefore == 0`. This is correct for cold start; document the
  invariant in a doc comment above the function.
- Add a method `IsControlSlot(index int) bool` so callers in `client.go`
  can decide whether a given slot's request handler should refuse data
  leases. (Not strictly required if the server-side enforcement is the
  source of truth, but cheap and useful for tests.)

#### C5. Tests (`pkg/client/*_test.go`)

- `TestRegisterAdvertisesLeasedPoolCapability` — fake server reads
  the Register message, asserts `Capabilities&CapLeasedPool != 0`.
- `TestRegisterRejectsAckWithoutLeasedPoolBit` — fake server sends an ack
  with `Capabilities: 0`, client tears down the session.
- `TestPingLoopPromotesAfterThreeMissedPongs` — fake server stops
  responding to pings; after 3 missed pongs the client triggers control
  loss / promotion (use a stub or in-memory transport).
- `TestResizeNeverClosesControl` — pool of 5, control at index 0, resize
  to 1, verify the control conn survives.
- `TestPoolPromotionPicksOldestIdleData` — kill control, verify the
  oldest idle data conn is promoted.

#### C6. `go.mod` bump

Same as S12, against `/home/dev/worktrees/paul/plex-tunnel`.

### Verification

```
cd /home/dev/worktrees/paul/plex-tunnel
go vet ./...
go build ./...
go test ./...
```

---

## Cross-repo: `crbl-infra` doc update

After server + client merge to `dev` and staging is bouncing successfully,
update `/home/dev/github/crbl-infra/infrastructure/plex-tunnel-server.md`
to reflect:

- New tier-derived pool sizing (Free 4 / Pro 11 / Max 21 total slots).
- New env vars: `PLEXTUNNEL_POOL_FREE`, `PLEXTUNNEL_POOL_PRO`,
  `PLEXTUNNEL_POOL_MAX`, `PLEXTUNNEL_POOL_MAX_ABSOLUTE`,
  `PLEXTUNNEL_CONTROL_SSE_MAX`.
- Note that `plex-tunnel-leased-pool.md` is the canonical design doc and
  link to it.
- Status of the leased-pool design changes from "in flight" to "shipped".

`infrastructure/plex-tunnel-leased-pool.md` itself does **not** need to
change unless implementation discovered a contradiction with the doc — in
which case **stop and escalate to the CTO** before patching either side.

PR body for the server PR MUST include:

- A line stating the **minimum compatible client version** (the new proto
  v3 client build tag / commit).
- The required **deployment order** (server first, then client — or vice
  versa, decide based on the rollback gate analysis below and document).
- An `INFRA-CHECK:` line confirming the `crbl-infra` doc was updated, OR
  `INFRA-CHECK: no doc change required because <reason>` if applicable.

---

## Acceptance criteria

The CTO will only accept the merged PRs if **all** of these hold:

1. Proto v3 + `CapLeasedPool` bit is wire-visible in `MsgRegister` and
   `MsgRegisterAck`.
2. Server rejects v2 clients and v3 clients without `CapLeasedPool` at
   registration with a non-silent error message.
3. Plan defaults are 4 / 11 / 21 in **both** `PlanDefaultsMap` and the
   live DB (verified by querying staging after deploy).
4. `PLEXTUNNEL_POOL_*` env vars override the defaults at boot and the
   override is reflected in the granted `MaxConnections` of a fresh
   register.
5. `GET /:/eventsource/notifications` runs on the control conn under load
   and does NOT consume a data lease (verified by metrics + access logs).
6. A saturated data pool returns `429 Too Many Requests` with
   `Retry-After: 1`, **not** 503 or queued waits.
7. Striping degrades from requested width down to 1, refuses only at 0.
8. Idempotent GET retry on transport failure works pre-commit, does not
   retry post-commit.
9. Killing the control conn promotes the oldest idle data conn within
   the keepalive failure threshold; the session does not drop.
10. All new tests pass; no `WriteTimeout` was reintroduced on
    `publicServer` / `tunnelServer`; no existing test was deleted.
11. Staging deploy completes cleanly and a real Plex client connects and
    streams without 502s.
12. `crbl-infra/infrastructure/plex-tunnel-server.md` reflects the new
    topology in the same PR cycle.

---

## Verification (full sweep)

```
# proto
cd /home/dev/worktrees/paul/plex-tunnel-proto
go vet ./... && go build ./... && go test ./...

# server
cd /home/dev/worktrees/paul/plex-tunnel-server
go vet ./... && go build ./... && go test ./...

# client
cd /home/dev/worktrees/paul/plex-tunnel
go vet ./... && go build ./... && go test ./...
```

Then push, open PRs (proto → main first; server + client → dev), watch CI,
merge proto, bump deps in server/client, rerun CI, merge server + client,
deploy staging, smoke-test with a real Plex client.

**Stop and report to the CTO once staging is updated.** The CTO will
telegram the CEO when staging is ready for live testing.
