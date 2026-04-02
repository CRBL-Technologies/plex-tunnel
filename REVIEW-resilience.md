# Resilience Audit (Client)

Date: 2026-04-02
Auditor: Resilience Engineer
Scope: Production release PR #30 (dev -> main). All changed files plus related code in pkg/client/ and cmd/client/.

## Summary

Findings identified: 7
- Critical: 0
- High: 1
- Medium: 3
- Low: 2
- Info: 1

---

## Finding 1: UI max_connections form field has no upper bound validation

Severity: High

Description:
The web UI settings form validates that `max_connections` is `>= 1` but does not enforce the upper bound of 32 that exists in both `LoadConfig()` (config.go:88) and the `maxPoolConnections` constant (client.go:23). A user can submit `max_connections=10000` through the UI, and the client will request that many connections from the server. While the server-granted value is later capped at `maxPoolConnections` (32) in `runSession()`, the initial outgoing register message will request an absurdly high value, and the `Config.MaxConnections` field will carry this inflated value until the server responds.

More importantly, if the server is permissive or compromised and grants a large value, the client-side cap in `runSession()` (line 208) protects against allocating more than 32 pool slots, but the unchecked UI input represents a defense-in-depth gap.

Evidence:
- `cmd/client/ui.go:537-542` -- validates `maxConnections < 1` only, no upper bound
- `pkg/client/config.go:88` -- `LoadConfig()` correctly validates `maxConnections > 32`
- `pkg/client/client.go:208-209` -- runtime cap at `maxPoolConnections`

Proposed Fix:
Add upper bound validation in `handleSettings()` to match the config validation:
```go
if convErr != nil || maxConnections < 1 || maxConnections > 32 {
    redirectWithMessage(w, r, "", "max connections must be an integer between 1 and 32")
    return
}
```

---

## Finding 2: `c.cfg.MaxConnections` mutated without synchronization in `runSession`

Severity: Medium

Description:
In `runSession()` at line 211, the client writes `c.cfg.MaxConnections = grantedMax` directly on the `Config` struct. The `Config` struct is also read concurrently by the UI handler via `controller.Snapshot()` which reads `c.cfg` under `controller.mu.RLock()`. However, the `Client.cfg` field itself is not protected by the same mutex -- `controller.mu` protects the `clientController` fields, not the `Client.cfg` field. The `Client` struct uses `stateMu` for `state` but nothing protects `cfg`.

In practice the race is narrow because `cfg` is only written once per session start and `Snapshot()` reads it through the controller's copy. However, `c.cfg.MaxConnections` is mutated on the `Client` instance, which is separate from the controller's `cfg` copy. The `SnapshotStatus()` path reads `status.MaxConnections` (set via `updateStatus`), so the practical impact is limited. But the direct mutation of `c.cfg` without synchronization is a latent data race that could surface under `go test -race`.

Evidence:
- `pkg/client/client.go:211` -- `c.cfg.MaxConnections = grantedMax` (unsynchronized write)
- `pkg/client/client.go:171` -- `requestedMaxConnections := c.cfg.MaxConnections` (read at session start)
- `cmd/client/ui.go:111-123` -- `Snapshot()` reads controller's copy, not Client's

Proposed Fix:
Remove the mutation of `c.cfg.MaxConnections` entirely. The granted max is already stored in the pool and in the connection status. If the config value is needed for the next session's request, pass it explicitly rather than mutating the shared config.

---

## Finding 3: No request body size limit on UI form POST

Severity: Medium

Description:
The `/settings` endpoint calls `r.ParseForm()` without first limiting the request body size via `http.MaxBytesReader`. While the HTTP server has `ReadTimeout: 15s` which provides some protection, a slow trickle of data could still cause the server to buffer a large form body in memory before the timeout fires. Go's default `ParseForm` reads up to 10MB for POST bodies. Since the UI server is typically local, risk is limited, but for non-loopback deployments this is a concern.

Evidence:
- `cmd/client/ui.go:522` -- `r.ParseForm()` with no body size limit
- `cmd/client/main.go:53-56` -- ReadTimeout is 15s but no MaxBytesReader

Proposed Fix:
Wrap the request body with `http.MaxBytesReader` before parsing:
```go
r.Body = http.MaxBytesReader(w, r.Body, 4096) // form data should be tiny
if err := r.ParseForm(); err != nil {
    ...
}
```

---

## Finding 4: `readLoop` idle-timeout retry has no backoff or attempt limit

Severity: Medium

Description:
When a connection is idle (no active streams) and a `context.DeadlineExceeded` error occurs on `Receive()`, the read loop retries immediately with `continue` (line 255-262). If the underlying WebSocket connection is in a persistent deadline-exceeded state (e.g., a read deadline is set and keeps firing), this creates a tight CPU-spinning loop with only a debug log per iteration.

While this appears to be triggered by WebSocket read deadlines that fire when idle, there is no backoff between retries and no maximum retry count. A pathological case could burn CPU indefinitely.

Evidence:
- `pkg/client/client.go:254-262` -- `continue` on `DeadlineExceeded` with no backoff
- The condition checks `connRef.streams.Load() == 0 && ctx.Err() == nil`

Proposed Fix:
Add a small sleep or counter-based limit to prevent tight-looping:
```go
if errors.Is(err, context.DeadlineExceeded) && connRef.streams.Load() == 0 && ctx.Err() == nil {
    // The read deadline fired on an idle connection; this is expected.
    // Continue to the next receive call (the deadline resets on each call).
    continue
}
```
If the WebSocket library resets the deadline on each `Receive()` call, the tight loop risk may be mitigated by the library. Verify this is the case; if not, add `time.Sleep(100 * time.Millisecond)` before `continue`.

---

## Finding 5: Per-connection ping loop goroutine leak window during pool removal

Severity: Low

Description:
When `pool.remove()` is called, it cancels the removed connection's `pingCancel` inside the lock (lines 114-117). The `startConnPingLoop` goroutine checks `pingCtx.Err()` after `pingLoop` returns an error (line 660). However, there is a brief window: if `pingLoop` returns an error at the same instant the context is canceled, the goroutine calls `connRef.conn.Close()` on an already-closing connection. This is benign (Close is idempotent on WebSocket connections) but worth documenting. The goroutine itself exits cleanly.

Evidence:
- `pkg/client/pool.go:110-117` -- `remove()` cancels `pingCancel`
- `pkg/client/client.go:659-666` -- `startConnPingLoop` goroutine

Proposed Fix:
No code change needed. The behavior is correct -- `Close()` is safe to call multiple times. This is an informational note that the design handles this edge case correctly.

---

## Finding 6: HTTP client transport has no total connection limit

Severity: Low

Description:
The `http.Transport` in `New()` sets `MaxIdleConns: 100` and `MaxIdleConnsPerHost: 10` but does not set `MaxConnsPerHost`. This means the client can open an unbounded number of simultaneous connections to the Plex target if many proxied requests arrive concurrently. The `streamSem` (128 concurrent streams) provides an indirect cap, but each stream could open a new TCP connection to Plex if the idle pool is exhausted.

With 128 concurrent streams and the default Go transport behavior, this could result in up to 128 simultaneous TCP connections to the local Plex server. For a local service this is generally fine, but it is worth documenting.

Evidence:
- `pkg/client/client.go:108-113` -- `http.Transport` config, no `MaxConnsPerHost`
- `pkg/client/client.go:22` -- `maxConcurrentStreams = 128`

Proposed Fix:
Consider adding `MaxConnsPerHost: 32` (or similar) to the transport to provide explicit back-pressure at the TCP level:
```go
Transport: &http.Transport{
    MaxIdleConns:          100,
    MaxIdleConnsPerHost:   10,
    MaxConnsPerHost:       32,
    IdleConnTimeout:       90 * time.Second,
    ResponseHeaderTimeout: 30 * time.Second,
},
```

---

## Finding 7: Graceful shutdown does not drain in-flight proxy requests

Severity: Info

Description:
When the client process receives SIGINT/SIGTERM, the root context is canceled, which propagates to all active `readLoop` and `handleHTTPRequest` goroutines. The `http.Server.Shutdown()` call (main.go:71) gracefully drains UI HTTP connections with a 5-second timeout, but there is no equivalent drain period for in-flight tunnel proxy requests.

Active proxy requests get their context canceled immediately when the signal fires. This means responses being streamed back through the tunnel will be interrupted. For a media streaming proxy, this could result in playback interruptions during client restarts.

The current behavior (immediate cancellation) is acceptable for the use case -- Plex clients handle reconnection -- but a future enhancement could add a grace period before canceling the session context.

Evidence:
- `cmd/client/main.go:69-73` -- UI server has graceful shutdown with 5s timeout
- `cmd/client/ui.go:55-69` -- `startLocked()` runner context derived from root context
- `pkg/client/client.go:280` -- `reqCtx` derived from session context, canceled on shutdown

Proposed Fix:
No immediate fix required. This is a design observation. If graceful drain is desired in the future, insert a delay between signal receipt and session context cancellation, allowing in-flight requests to complete.

---

## Notable Positive Controls

1. **Stream concurrency limiter** (`streamSem` with `maxConcurrentStreams = 128`): Properly bounds the number of concurrent proxy goroutines, rejecting excess requests with a 503. The semaphore pattern is correctly implemented with `select`/`default` for non-blocking acquire and deferred release.

2. **Per-request timeout** (`proxyRequestTimeout = 5 * time.Minute`): Each proxied HTTP request gets its own context with a 5-minute deadline, preventing individual slow requests from leaking goroutines.

3. **Pool connection cap** (`maxPoolConnections = 32`): Server-granted connection counts are capped client-side at 32, providing defense-in-depth against a misbehaving server.

4. **Response chunk size bounds** (config.go:99): Chunk size is bounded between 1KB and 4MB, preventing memory exhaustion from misconfiguration.

5. **Exponential backoff with jitter** (reconnect.go): Both session-level and slot-level reconnections use proper exponential backoff with capped jitter, preventing thundering herd on server recovery.

6. **HTTP server timeouts** (main.go:53-56): `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, and `IdleTimeout` are all set on the UI HTTP server, mitigating slowloris attacks.

7. **Pool resize safety** (pool.go:195-274): The `Resize()` method correctly collects cancels and connections under the lock, then releases them outside the lock to avoid deadlocks. Ping loop cancels for per-connection and pool-level pings are both handled.

8. **Idle connection churn fix**: The `readLoop` correctly distinguishes between idle-timeout reads (which should retry) and genuine errors (which should tear down the connection), reducing unnecessary reconnection overhead.
