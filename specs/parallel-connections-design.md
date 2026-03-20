# Parallel Tunnel Connections — Design Spec

> **Status:** Proposal
> **Date:** 2026-03-20
> **Context:** [bandwidth-investigation-2026-03-20.md](bandwidth-investigation-2026-03-20.md)

---

## Problem

A single WebSocket connection (single TCP stream) between client and server
creates two bottlenecks:

1. **TCP bandwidth-delay product limit.** A single TCP connection's throughput
   is bounded by `window_size / RTT`. On a high-latency link (e.g. 100 ms RTT),
   even with modern window scaling, a single connection cannot saturate the
   available bandwidth. This is a fundamental TCP property — not a bug in the
   code.

2. **Head-of-line blocking via `writeMu`.** All concurrent HTTP streams share
   one WebSocket, serialized by a single write mutex. One slow 64 KiB media
   chunk blocks every other in-flight request — including small control traffic
   like `/media/providers`.

The investigation confirmed that `ws_write_ms` (the actual TCP send) is the
dominant cost, and `write_lock_wait_ms` amplifies it across all concurrent
requests. Plex read time, frame encoding, and server-side delivery are not
bottlenecks.

---

## Solution: Connection Pool per Session

Replace the single WebSocket connection with a **pool of N parallel WebSocket
connections** grouped into a **session**.

Each connection is an independent TCP stream with its own congestion window,
write buffer, and write mutex. Streams are distributed across connections so
that:

- Aggregate bandwidth scales linearly with connection count (up to link
  capacity).
- A slow write on one connection does not block streams on other connections.
- Control traffic is isolated from bulk media transfers.

This is the same approach used by Cloudflare Tunnel (4 parallel QUIC/HTTP2
connections), browsers (6 connections per origin), and GridFTP (parallel TCP
streams). It is proven, compatible with existing infrastructure (Caddy,
firewalls, NAT), and does not require UDP or any non-standard network support.

### Why not QUIC?

QUIC would be the theoretically ideal transport — independent streams with no
TCP head-of-line blocking, built-in multiplexing, modern congestion control.
However:

- QUIC requires UDP, which is blocked or throttled on some networks.
- The existing Caddy reverse-proxy setup uses TCP/WebSocket.
- A WebSocket connection pool provides the same practical benefits (parallel
  congestion windows, no cross-stream HOL blocking) while working everywhere.

The architecture below is designed so that QUIC can be added as an alternative
transport later without changing the session/stream model. If a QUIC transport
is added, a single QUIC connection with N streams would be functionally
equivalent to N WebSocket connections — the pool abstraction handles both cases.

---

## Protocol Changes (Version 2)

### Session Concept

Protocol version 2 introduces the concept of a **session**: a group of tunnel
connections that belong to the same client registration. The server treats all
connections in a session as a single logical tunnel.

- A session is identified by a server-assigned `session_id` (opaque string,
  e.g. UUID).
- The first connection in a session is the **control connection**. It performs
  the initial registration handshake.
- Subsequent connections **join** the session by presenting the same
  `session_id` and `token`.
- All connections in a session share the same subdomain routing.
- If the control connection drops, the session remains alive as long as at
  least one connection exists. Any remaining connection can assume control
  duties.

### Message Changes

Two fields are added to the `Message` struct:

```go
type Message struct {
    // ... existing fields ...

    // SessionID identifies the session this connection belongs to.
    // Set in Register (empty = new session, non-empty = join existing).
    // Set in RegisterAck (server-assigned session ID).
    SessionID string `json:"session_id,omitempty"`

    // MaxConnections is the maximum number of parallel connections
    // allowed for this session.
    // Set in Register (client's requested pool size).
    // Set in RegisterAck (server's granted pool size, may be lower).
    MaxConnections int `json:"max_connections,omitempty"`
}
```

No new message types are required. The existing `MsgRegister` /
`MsgRegisterAck` handshake is extended.

### Handshake: New Session

```
Client                                    Server
  |                                         |
  |--- Register --------------------------->|
  |    token: "abc123"                      |
  |    subdomain: "myserver"                |
  |    protocol_version: 2                  |
  |    session_id: ""          (new)        |
  |    max_connections: 4      (requested)  |
  |                                         |
  |                       Validate token ---|
  |                       Create session ---|
  |                       Assign subdomain -|
  |                                         |
  |<-- RegisterAck -------------------------|
  |    subdomain: "myserver"                |
  |    protocol_version: 2                  |
  |    session_id: "sess-uuid" (assigned)   |
  |    max_connections: 4      (granted)    |
  |                                         |
  |  (connection 0 is now the control conn) |
```

### Handshake: Join Session

```
Client                                    Server
  |                                         |
  |--- Register --------------------------->|
  |    token: "abc123"                      |
  |    protocol_version: 2                  |
  |    session_id: "sess-uuid" (join)       |
  |                                         |
  |                       Validate token ---|
  |                       Verify session ---|
  |                       Check pool limit -|
  |                                         |
  |<-- RegisterAck -------------------------|
  |    session_id: "sess-uuid"              |
  |    protocol_version: 2                  |
  |                                         |
  |  (connection added to session pool)     |
```

When joining, the server validates:
- The token matches the session's original token.
- The session exists and is active.
- The pool has not reached `max_connections`.

If any check fails, the server responds with `MsgError` and closes the
connection.

### Compatibility and Rollout

This design now assumes a **coordinated cutover**:

- `plex-tunnel-proto`, `plex-tunnel-server`, and `plex-tunnel` all move to
  protocol version 2 together before the feature is considered deployable.
- The existing handshake rule of "exact protocol version match required" stays
  in place.
- Mixed-version compatibility is out of scope for the initial implementation.

Implications for implementation:

- We do **not** need dual-stack server registration logic for v1 and v2.
- We do **not** need client fallback from a v2 client to a v1 server.
- We **do** need the protocol/session changes to land in proto first, then be
  consumed by both server and client before end-to-end testing.

The session fields still remain `omitempty` on the wire, but that is now a
schema convenience rather than a rollout mechanism.

---

## Stream Assignment Strategy

When the client has multiple connections available, it must decide which
connection to use for each outgoing message (response chunk). The strategy
balances throughput, fairness, and control traffic isolation.

### Connection Roles

| Role | Connection Index | Traffic |
|------|-----------------|---------|
| **Control** | 0 | Pings, pongs, small responses (< threshold), end-stream markers |
| **Data** | 1 through N-1 | Media response chunks (bulk data) |

If only one connection is available (pool size 1), it carries all traffic.

### Assignment Rules

1. **New stream starts:** When the client begins proxying a new HTTP response,
   it picks a data connection for the stream. The stream is **pinned** to that
   connection for all subsequent chunks. This avoids reordering and keeps TCP
   pipelining efficient.

2. **Connection selection:** Among data connections, pick the one with the
   **fewest in-flight streams**. Ties broken by round-robin. This balances load
   without requiring per-chunk decisions.

3. **Small responses:** Responses under a configurable threshold (e.g. 128 KB
   total, or responses that complete in a single chunk) can use the control
   connection. This keeps small API calls fast even when data connections are
   saturated.

4. **Ping/pong:** Always on the control connection. If the control connection
   is lost, promote the lowest-index data connection to control.

### Why Pin Streams to Connections

Allowing a single stream to hop between connections would:
- Require the server to reassemble out-of-order chunks per stream (complexity).
- Defeat TCP pipelining within a connection (each connection's write buffer
  works best with sequential data).
- Add reordering latency on the server side.

Pinning avoids all of this. The downside — one slow connection can slow one
stream — is acceptable because other streams on other connections are unaffected.

---

## Client Architecture

### ConnectionPool

```go
// ConnectionPool manages N parallel tunnel connections to the server.
type ConnectionPool struct {
    mu          sync.RWMutex
    conns       []*poolConn           // indexed 0..N-1
    sessionID   string
    maxConns    int
    assignNext  atomic.Uint64         // round-robin counter
}

type poolConn struct {
    conn        tunnel.Connection     // underlying WebSocket connection
    index       int
    streams     atomic.Int64          // number of in-flight streams
    control     bool                  // true if this is the control connection
}
```

### Lifecycle

```
Client.Run()
  |
  +-- runSession()
       |
       +-- Dial connection 0 (control)
       +-- Send Register (session_id="", max_connections=N)
       +-- Receive RegisterAck (session_id, granted max_connections)
       +-- Store session_id, granted pool size
       |
       +-- Launch pool expansion goroutine:
       |     for i := 1; i < grantedPoolSize; i++ {
       |         go dialAndJoin(sessionID, i)
       |     }
       |
       +-- Launch readLoop per connection
       +-- Launch pingLoop on control connection
       +-- Wait for fatal error
```

### Connection Failure Handling

- If a **data connection** drops: remove from pool, re-dial and re-join the
  session. Streams that were pinned to it fail with an error; the server will
  see the HTTP response stall and eventually timeout. New streams are assigned
  to remaining connections.

- If the **control connection** drops: promote the lowest-index surviving
  connection to control (move ping/pong duties). Re-dial a replacement
  connection and add it to the pool.

- If **all connections** drop: the session is dead. Re-enter the full
  reconnection loop (new session, exponential backoff).

- Individual connection re-dials use a short backoff (1-4 seconds) since the
  session is still alive.

### Pool Resizing

The pool size is fixed for the lifetime of a session (determined at
registration). Dynamic resizing is not in scope for the initial implementation
but the architecture does not preclude it — a future `MsgPoolResize` message
could allow the server to adjust `max_connections` mid-session.

---

## Server Architecture

### Session Manager

The server's routing table changes from `subdomain -> Connection` to
`subdomain -> Session`:

```go
type Session struct {
    mu           sync.RWMutex
    id           string
    subdomain    string
    token        string
    conns        []*sessionConn
    maxConns     int
    responseChs  map[string]chan tunnel.Message  // request_id -> response channel
    assignNext   atomic.Uint64
}

type sessionConn struct {
    conn    tunnel.Connection
    index   int
    streams atomic.Int64
}
```

### Request Dispatch

When the server receives an HTTP request to proxy:

1. Look up session by subdomain.
2. Pick a connection from the session's pool (least-streams, same strategy as
   client).
3. Send `MsgHTTPRequest` on that connection.
4. The response may arrive on **any** connection in the session (routed by
   `request_id` as before).

This means the server must run a read loop on **every** connection in the
session and route `MsgHTTPResponse` messages to the correct response channel
by request ID.

### Connection Join

When a connection sends `Register` with a non-empty `session_id`:

1. Look up the session by ID.
2. Verify the token matches.
3. Verify the pool is not full.
4. Add the connection to the session's pool.
5. Start a read loop for the new connection.
6. Send `RegisterAck`.

---

## Offer Integration (Tiering)

The `max_connections` field is the natural mechanism for plan-based tiering:

| Plan | `max_connections` | Expected Throughput |
|------|-------------------|---------------------|
| Free | 1 | Current behavior (single TCP stream) |
| Basic | 4 | ~4x throughput improvement |
| Pro | 8 | ~8x throughput improvement |

The server enforces this per-token. The client requests its desired pool size;
the server grants up to the token's plan limit. The client respects the granted
value.

This is transparent to the user: more connections = more bandwidth, enforced
server-side, no client-side configuration needed. If a user upgrades their
plan, the next session they establish will get a larger pool.

The `max_connections` grant could also be dynamic per server load — during high
server utilization, the server could temporarily reduce pool sizes. This is a
future optimization; for now, static per-plan limits are sufficient.

---

## Chunk Size Considerations

With head-of-line blocking eliminated (each stream on its own connection),
larger chunk sizes become safe and beneficial:

- Fewer chunks per stream = fewer frame headers = less overhead.
- Each chunk still holds the connection's write mutex, but only competes with
  ping/pong on the same connection (not other streams).
- Recommended default increase: 64 KiB -> 256 KiB (with `max_connections >= 2`).

The client could auto-tune chunk size based on granted pool size, but a
simple static increase is sufficient for the initial implementation.

---

## Proto Module Changes

All protocol changes live in `plex-tunnel-proto`:

1. Add `SessionID` and `MaxConnections` fields to `Message`.
2. Bump `ProtocolVersion` to `2` as part of the coordinated cutover.
3. Update validation:
   - `Register` with `protocol_version: 2` should have `max_connections >= 1`.
   - `RegisterAck` with `protocol_version: 2` should have `session_id` and
     `max_connections`.
4. No changes to frame encoding — the new fields are just additional JSON
   metadata fields.

The `WebSocketConnection` struct and `Send`/`Receive` methods are unchanged.
The pool is built on top of multiple `WebSocketConnection` instances.

---

## Implementation Order

### Phase 1: Proto groundwork

1. Add `SessionID` and `MaxConnections` to proto `Message`.
2. Bump `ProtocolVersion` to `2`.
3. Make `Register` / `RegisterAck` validation enforce the required v2 session
   fields.

### Phase 2: Server session support

1. Server: implement `Session` struct and session manager.
2. Server: handle "join session" registration.
3. Server: read loops on all session connections.
4. Server: request dispatch across pool.
5. Keep the existing exact-version handshake behavior; reject non-v2 clients.

### Phase 3: Client connection pool

1. Client: implement `ConnectionPool`.
2. Client: session establishment (dial control, expand pool).
3. Client: require the v2 `RegisterAck` session metadata and fail otherwise.
4. Client: stream pinning and assignment.
5. Client: connection failure/recovery within a session.
6. Client: control/data traffic separation.

### Phase 4: Tuning and observability

1. Increase default chunk size for pooled sessions.
2. Add per-connection metrics (bytes sent, streams active, write latency).
3. Pool status in web UI (connections active, per-connection throughput).
4. Server-side per-session bandwidth metrics.

### Phase 5 (future): QUIC transport

1. Implement `QUICTransport` behind the existing `Transport` interface.
2. A single QUIC connection with N streams replaces N WebSocket connections.
3. No changes to session/stream/assignment logic — the pool abstraction
   handles both.
4. Fallback: try QUIC first, fall back to WebSocket pool if UDP is blocked.

---

## What This Does Not Solve

- **Raw link capacity:** If the client's upload bandwidth is 10 Mbps, no
  number of connections will exceed 10 Mbps. Multiple connections help
  *saturate* the link; they cannot exceed it.

- **Server-to-browser bottleneck:** If the server's downstream path to the
  Plex app is slow, client-side parallelism does not help. The investigation
  showed this is not currently a bottleneck, but it could become one at higher
  throughput.

- **Plex-local bottleneck:** If reading from the local Plex server is slow
  (disk I/O, transcoding), more connections do not help. The investigation
  confirmed this is not currently a bottleneck.

---

## Open Questions

1. **Default pool size:** Should the default be 4 (Cloudflare's choice) or
   start lower (2) and measure? Recommendation: start with 4, it is well
   proven.

2. **Connection establishment rate:** Should pool connections dial in parallel
   or sequentially with a small delay? Parallel is faster but creates a burst
   of TLS handshakes. A stagger of 100-200 ms per connection is probably fine.

3. **Stream pinning vs. chunk-level distribution:** This spec recommends
   stream pinning for simplicity. Chunk-level distribution could theoretically
   improve throughput for a single large stream across multiple connections,
   but adds reordering complexity. Worth revisiting after measuring pinning
   performance.

4. ~~**Server proto migration:** The server still has a local `pkg/tunnel/` copy
   and has not migrated to the shared proto module.~~ **Resolved.** The server
   already imports `github.com/CRBL-Technologies/plex-tunnel-proto` and has no
   local tunnel package. Protocol version 2 changes can land in the proto
   module immediately.

---

## Implementation TODO

> **Rollout decision: coordinated cutover.**
> `plex-tunnel-proto`, `plex-tunnel-server`, and `plex-tunnel` will all move to
> protocol version 2 together. Exact version matching remains in place, so
> mixed v1/v2 deployments are out of scope for this initial implementation.
>
> Deployment order: proto → server → clients.

Each item below is tagged with the repo it lives in: **[proto]**, **[server]**,
or **[client]**.

### Phase 1 — Proto (`plex-tunnel-proto`)

- [ ] Add `SessionID string` and `MaxConnections int` to `Message`. Both
      fields are `omitempty`.
- [ ] Update `Validate()` for version-aware checks:
  - `MsgRegister` with `ProtocolVersion == 2`: require `MaxConnections >= 1`.
  - `MsgRegisterAck` with `ProtocolVersion == 2`: require `SessionID` non-empty
    and `MaxConnections >= 1`.
  - Do **not** add v1 guards inside `Validate()`. The existing exact-version
    check in `handleTunnel` (`register.ProtocolVersion != tunnel.ProtocolVersion`)
    rejects non-v2 clients before `Validate()` is called on session fields.
- [ ] Bump `ProtocolVersion` to `2`.
- [ ] Tag and release (e.g. `v1.1.0`).

### Phase 2 — Server (`plex-tunnel-server`)

- [ ] Add session store: `map[sessionID]*Session`, protected by a mutex.
      `Session` holds `id`, `subdomain`, `token`, `maxConns`, and a slice of
      `*WebSocketConnection`.
- [ ] Update `handleTunnel` registration path:
  - v2 client, `SessionID == ""`: create a new session, assign a UUID, return
    `RegisterAck` with `SessionID` and granted `MaxConnections` (capped by
    token plan limit).
  - v2 client, `SessionID != ""`: join an existing session — validate token
    matches, check pool is not full, add connection, return `RegisterAck`.
- [ ] Start a per-connection read loop for every connection added to a session.
      Route `MsgHTTPResponse` messages by `request_id` to the correct waiting
      channel regardless of which connection they arrive on.
- [ ] Update `handleClientRequest` dispatch: when session has multiple
      connections, pick the one with the fewest in-flight streams (least-streams
      strategy), same as described in the Stream Assignment section above.
- [ ] Update `go.mod` to the new proto release.
- [ ] Keep exact-version rejection in place for non-v2 clients.

### Phase 3 — Client (`plex-tunnel`)

- [ ] Add `MaxConnections int` to `Config`. Env var:
      `PLEXTUNNEL_MAX_CONNECTIONS`, default `4`. Only takes effect when the
      server grants a v2 ack.
- [ ] Implement `ConnectionPool` in `pkg/client/pool.go` (see architecture
      section above for the `poolConn` and assignment logic).
- [ ] Update `runSession()`:
  - Send `Register` with `ProtocolVersion: 2`, `MaxConnections: cfg.MaxConnections`.
  - Require `RegisterAck.ProtocolVersion == 2` and `RegisterAck.SessionID != ""`.
  - Store session ID, dial remaining connections in parallel (staggered ~150 ms
    apart), each sending a join `Register` with the session ID.
- [ ] Update `handleHTTPRequest` to accept the connection to send on as a
      parameter; the pool assigns a connection per stream and pins it for all
      chunks of that stream.
- [ ] Ping/pong on the control connection (index 0) only.
- [ ] On data connection drop: remove from pool, re-dial and re-join; streams
      pinned to that connection fail and are retried by the HTTP client.
- [ ] On control connection drop: promote lowest-index surviving connection to
      control (take over ping/pong), re-dial a replacement.
- [ ] Update `go.mod` to the new proto release.

### Phase 4 — Tuning & Observability (both repos, after Phase 2+3)

- [ ] **[client]** Increase default `ResponseChunkSize` to `262144` (256 KiB)
      when the granted pool size is >= 2. Keep 64 KiB for single-connection
      sessions for `MaxConnections == 1`.
- [ ] **[server]** Add per-connection metrics: bytes sent, active streams,
      p99 write latency.
- [ ] **[client]** Add pool status to the web UI: connections active,
      per-connection throughput.
- [ ] **[client/server]** Consider retiring `PLEXTUNNEL_DEBUG_BANDWIDTH_LOGGING`
      once per-connection metrics replace its diagnostic value.
