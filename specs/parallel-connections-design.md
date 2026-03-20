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

### Backward Compatibility

- **Protocol version 1 clients** continue to work unchanged. The server treats
  them as a single-connection session internally.
- **Protocol version 2 clients** connecting to a version 1 server will receive
  a `RegisterAck` without `session_id` or `max_connections`. The client falls
  back to single-connection mode.
- The `MaxConnections` field defaults to `0` when absent, which both sides
  interpret as "1 connection, legacy mode."

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
2. Bump `ProtocolVersion` to `2`.
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

### Phase 1: Proto + Server (no client changes needed)

1. Add `SessionID` and `MaxConnections` to proto `Message`.
2. Bump protocol version to 2.
3. Server: implement `Session` struct and session manager.
4. Server: handle "join session" registration.
5. Server: read loops on all session connections.
6. Server: request dispatch across pool.
7. **Backward compatible:** version 1 clients still work (single-connection
   session).

### Phase 2: Client connection pool

1. Client: implement `ConnectionPool`.
2. Client: session establishment (dial control, expand pool).
3. Client: stream pinning and assignment.
4. Client: connection failure/recovery within a session.
5. Client: control/data traffic separation.

### Phase 3: Tuning and observability

1. Increase default chunk size for pooled sessions.
2. Add per-connection metrics (bytes sent, streams active, write latency).
3. Pool status in web UI (connections active, per-connection throughput).
4. Server-side per-session bandwidth metrics.

### Phase 4 (future): QUIC transport

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

4. **Server proto migration:** The server still has a local `pkg/tunnel/` copy
   and has not migrated to the shared proto module. Protocol version 2 changes
   need to land in the proto module first. The server migration
   (ARCHITECTURE.md section 10.1) should be completed before or as part of
   this work.
