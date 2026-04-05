# TASK: Fix ECC review findings (HIGH + 5 MEDIUM)

## Overview
Fix six issues found during ECC security review of the client codebase. One HIGH (data race) and five MEDIUM severity issues.

---

## Change 1: Delete data race on c.cfg.MaxConnections (HIGH)

**File:** `pkg/client/client.go` — line 218

### Current behavior
Line 218 writes `c.cfg.MaxConnections = grantedMax` without holding a lock, while other goroutines read `c.cfg.*` concurrently. This is a data race.

### Desired behavior
Delete line 218 entirely (`c.cfg.MaxConnections = grantedMax`). The `grantedMax` value is already passed to `newConnectionPool()` on line 219, so this mutation is unnecessary and introduces a race.

### Notes
- Only delete the one line. Do not change anything else in this function.

---

## Change 2: Make ws:// a fatal error (MEDIUM)

**File:** `pkg/client/client.go` — lines 179–181

### Current behavior
When `c.cfg.ServerURL` starts with `ws://`, the code logs a `Warn()` level message but continues connecting. The commit message that added this claimed it was upgraded to an error, but it was not.

### Desired behavior
Replace the `Warn()` log with a hard error that prevents connection. Return an error from `runSession` when the URL uses `ws://`:

```go
if strings.HasPrefix(c.cfg.ServerURL, "ws://") {
    return fmt.Errorf("refusing to connect over unencrypted ws:// — tunnel token would be sent in plaintext; use wss:// instead")
}
```

Remove the logger.Warn() call entirely — it should be a returned error, not a log line.

---

## Change 3: Fix circuit breaker half-open cooldown reset bug (MEDIUM)

**File:** `pkg/client/circuit.go` — lines 74–91 (RecordFailure method)

### Current behavior
When a failure occurs in the half-open state, `RecordFailure()` sets `c.lastFailureTime = time.Now()` (line 79) before checking state (line 81). This means a failure during half-open resets the cooldown timer via `lastFailureTime`, so a flapping upstream keeps the circuit permanently open — it never gets a chance to half-open again because the cooldown window keeps resetting.

### Desired behavior
Add a dedicated `halfOpenAt` timestamp field to the `circuitBreaker` struct. Use this timestamp (instead of `lastFailureTime`) for the open→half-open transition in `Allow()`.

Specifically:

1. Add field `halfOpenAt time.Time` to the `circuitBreaker` struct.
2. In `Allow()`, change the open-state check from `time.Since(c.lastFailureTime) < c.cooldown` to `time.Since(c.halfOpenAt) < c.cooldown`.
3. When transitioning TO the open state (in `RecordFailure`, both from half-open and from closed/default), set `c.halfOpenAt = time.Now()`.
4. In `RecordSuccess()`, reset `c.halfOpenAt = time.Time{}` alongside the existing `c.lastFailureTime` reset.
5. Keep `c.lastFailureTime = time.Now()` on line 79 — that field still records when the last failure occurred, it just should not drive the cooldown window.

### Notes
- The `halfOpenAt` field records when the circuit *entered* the open state, not when the last failure happened. Subsequent failures in the open or half-open state must NOT update `halfOpenAt`.

---

## Change 4: Remove redundant HasPrefix("****") token guard (MEDIUM)

**File:** `cmd/client/ui.go` — line 547

### Current behavior
```go
if submittedToken != "" && submittedToken != maskToken(cfg.Token) && !strings.HasPrefix(submittedToken, "****") {
```

The `!strings.HasPrefix(submittedToken, "****")` check silently discards any valid token that happens to start with `****`. This is an over-broad guard — the `submittedToken != maskToken(cfg.Token)` check already handles the masked-value case.

### Desired behavior
Remove the `&& !strings.HasPrefix(submittedToken, "****")` condition:

```go
if submittedToken != "" && submittedToken != maskToken(cfg.Token) {
```

---

## Change 5: Use custom Prometheus registry (MEDIUM)

**File:** `pkg/client/metrics.go`

### Current behavior
Uses `promauto.NewGauge` / `promauto.NewCounterVec` which registers on `prometheus.DefaultRegistry`. In e2e tests or multi-instance scenarios, creating a second client will panic on duplicate metric registration.

### Desired behavior
1. Create a package-level custom registry: `var MetricsRegistry = prometheus.NewRegistry()`
2. Create a `promauto.Factory` wrapping the custom registry: `var factory = promauto.With(MetricsRegistry)`
3. Replace all `promauto.NewGauge(...)` and `promauto.NewCounterVec(...)` calls with `factory.NewGauge(...)` and `factory.NewCounterVec(...)`.
4. Export `MetricsRegistry` so callers can use it with `promhttp.HandlerFor(MetricsRegistry, ...)` if needed.

### Notes
- The metric variable names (`activeStreamsMetric`, `requestsTotalMetric`, `connectedMetric`) and the helper functions (`observeProxyResponse`, `setConnectedMetric`) should remain unchanged.
- The metric names and labels should remain unchanged.

---

## Change 6: Release lock between drain iterations in pool.Resize (MEDIUM)

**File:** `pkg/client/pool.go` — lines 272–281

### Current behavior
After releasing `p.mu` at line 261, the drain loop at lines 272-281 iterates over `removedConns` sequentially, blocking for up to 5s per connection (50 iterations × 100ms sleep). The entire `Resize` function blocks the caller for `len(removedConns) × 5s` worst case. Since callers hold a session-level lock while calling `Resize`, this blocks all session operations during scale-down.

### Desired behavior
Move the drain loop into a goroutine so `Resize` returns immediately after collecting the removed connections and releasing the lock. The goroutine drains each connection asynchronously:

```go
go func() {
    for _, connRef := range removedConns {
        for i := 0; i < 50; i++ {
            if connRef.streams.Load() == 0 {
                break
            }
            time.Sleep(100 * time.Millisecond)
        }
        _ = connRef.conn.Close()
    }
}()
```

This way `Resize` returns immediately and the drain happens in the background.

### Notes
- The cancel calls for `pingCancel`, `removedPingCancels`, and `removedCancels` should still happen synchronously before the return — only the slow drain-and-close loop should be async.

---

## Tests

### Circuit breaker test (`pkg/client/circuit_test.go`):
- **TestCircuitBreaker_HalfOpenFailureDoesNotResetCooldown**: Create a breaker with threshold=1, cooldown=50ms. Record a failure to open it. Sleep past cooldown. Call Allow() (enters half-open). Record another failure (reopens). Verify that calling Allow() again immediately returns false (still open). Sleep past cooldown again. Verify Allow() returns true (enters half-open again). This confirms that a failure in half-open does not extend the cooldown window.

### Client ws:// test (`pkg/client/client_test.go`):
- **TestRunSession_RejectsPlaintextWebSocket**: Create a Client with ServerURL="ws://example.test/tunnel" and call runSession. Assert that the returned error contains "refusing to connect over unencrypted ws://".

---

## Constraints
- DO NOT delete or modify any existing code that is not explicitly mentioned in this task.
- DO NOT change any existing test assertions — only add new tests.
- Keep all existing tests passing.

## Verification
```bash
cd /home/dev/github/plex-tunnel && go build ./... && go test -race -count=1 ./...
```

## When Done
Commit all changes with a descriptive commit message. Do NOT modify TASK.md.
