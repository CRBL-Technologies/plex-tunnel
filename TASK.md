# ECC Batch 4 — Fix TLS verification mutation in client tests

## Context

The ECC audit (2026-04-06) flagged a **Critical** finding in `pkg/client/client_test.go`:

> `TestMain` at `pkg/client/client_test.go:14` mutates `http.DefaultTransport` to set `InsecureSkipVerify=true` and never restores it. Any code in the binary that touches `http.DefaultClient` (including the proto WS dial fallback) silently accepts MITM certs for the whole test process lifetime.

The mutation was introduced in commit `69211f8` ("Fix ECC client review findings") to support the handshake tests in `pkg/client/client_handshake_test.go`, which use `httptest.NewTLSServer` and expect the client's `tunnel.DialWebSocket(...)` call in `runSession` to accept the test server's self-signed certificate.

### Why the mutation was necessary

`tunnel.DialWebSocket` in `plex-tunnel-proto v1.2.0` calls `nhooyr.io/websocket.Dial` with `DialOptions{HTTPHeader: headers}` and no `HTTPClient` — so nhooyr falls back to `http.DefaultClient`, which uses `http.DefaultTransport`. There is no hook to inject a scoped TLS config into that code path.

### Why "httptest.NewServer with ws://" (the report's alternative fix) is not viable

`runSession` at `pkg/client/client.go:179` hard-rejects any `ServerURL` that starts with `ws://`:

```go
if strings.HasPrefix(c.cfg.ServerURL, "ws://") {
    return fmt.Errorf("refusing to connect over unencrypted ws:// ...")
}
```

Using a plain-HTTP `httptest.NewServer` would force a `ws://` URL that `runSession` refuses before reaching the dial. The only ways around that are:
- Weaken the `ws://` rejection in production code (security regression, forbidden).
- Bypass `runSession` in the handshake tests (loses integration coverage, and the handshake tests are the whole point of `client_handshake_test.go`).

Both cross the scope line.

### Chosen approach: per-test scoped, cert-pinned transport

Delete `TestMain`. Add a test helper `withPinnedTLS(t, srv)` that each handshake test calls. The helper:

1. Saves the current `http.DefaultTransport`.
2. Clones `*http.Transport` from it and replaces `TLSClientConfig` with a new `tls.Config` whose `RootCAs` contains **only** `srv.Certificate()` — **no `InsecureSkipVerify`**.
3. Assigns the clone back to `http.DefaultTransport`.
4. Registers `t.Cleanup` to restore the original `http.DefaultTransport`.

This is the minimum-scope mutation achievable without a proto-level change or a production hook in `pkg/client`. Improvements over the current state:
- **Not MITM-open**: cert-pinned via `RootCAs` to a single specific test server. `InsecureSkipVerify` stays `false`.
- **Bounded lifetime**: mutation lasts one test, not the whole process. Restored deterministically via `t.Cleanup` (not a deferred function swallowed by `os.Exit`).
- **Non-TLS tests are untouched**: `TestResolveTargetURL` and `TestRunSession_RejectsPlaintextWebSocket` never see a mutated transport because they never call the helper.

Note: the tests in `pkg/client` do not use `t.Parallel()`, so there is no concurrency race on the global.

## Goal

Remove the global TLS verification mutation from `pkg/client` tests. Replace with a per-test, cert-pinned, `t.Cleanup`-restored transport swap in the handshake tests that actually need it. Preserve existing test coverage and behavior.

## Constraints / guardrails

- **DO NOT delete or modify code not mentioned in this task.** This includes every file under `pkg/client/` other than those listed in Tasks, every file under `cmd/`, and every production file in the repo.
- **DO NOT modify any production code in `pkg/client`** — no changes to `client.go`, `config.go`, `pool.go`, `reconnect.go`, `status.go`, `circuit.go`, `metrics.go`. This is a test-only fix.
- **DO NOT touch `plex-tunnel-proto`** (it is a different repo entirely) or bump `go.mod`.
- **DO NOT use `InsecureSkipVerify: true`** anywhere. The replacement transport must trust the specific test server's certificate via `RootCAs` and leave `InsecureSkipVerify` at its zero value (`false`).
- **DO NOT mutate `http.DefaultClient`** — only `http.DefaultTransport`, and only per-test, with `t.Cleanup` restoration.
- **DO NOT introduce `t.Parallel()`** in any of the handshake tests. The global swap is only safe because these tests run serially.
- **DO NOT delete or rewrite `TestResolveTargetURL` or `TestRunSession_RejectsPlaintextWebSocket`** — only remove the now-unnecessary `TestMain`, and leave those tests otherwise untouched.
- **DO NOT delete or rename any handshake test** in `client_handshake_test.go`. Only add the `withPinnedTLS(t, srv)` helper call at the top of each test that uses `httptest.NewTLSServer`.
- **DO NOT add any new dependencies.** Use only stdlib (`crypto/tls`, `crypto/x509`, `net/http`, `net/http/httptest`) and packages already imported by the file you're editing.
- **DO NOT touch any other finding** in the ECC report (`/home/dev/.claude-admin/reports/2026-04-06-ecc-review-portainer.md`). The High/Medium/Low items in Section 2 "plex-tunnel" are out of scope for this batch.

## Tasks

- [ ] **Task 1 — Delete `TestMain` from `pkg/client/client_test.go`.** Remove the entire function at lines 14–25 and the now-unused imports (`crypto/tls`, `os`). Keep `context`, `net/http` (still used by other tests? — check: no, `net/http` is not used by the remaining tests in `client_test.go`, so drop it too), `strings`, `testing`, and `github.com/rs/zerolog`. Double-check imports against the remaining two tests (`TestResolveTargetURL`, `TestRunSession_RejectsPlaintextWebSocket`) — only keep imports those tests actually need. Do not alter either remaining test body.

- [ ] **Task 2 — Add the `withPinnedTLS` helper in `pkg/client/client_handshake_test.go`.** Place the helper function near the bottom of the file, next to the existing `toWebSocketURL` helper. Signature:

  ```go
  // withPinnedTLS installs an http.DefaultTransport clone whose RootCAs
  // trusts only srv.Certificate(), then restores the original transport
  // via t.Cleanup. Do not call with t.Parallel active.
  func withPinnedTLS(t *testing.T, srv *httptest.Server) {
      t.Helper()
      if srv.Certificate() == nil {
          t.Fatal("withPinnedTLS: srv has no TLS certificate; use httptest.NewTLSServer")
      }
      pool := x509.NewCertPool()
      pool.AddCert(srv.Certificate())

      orig := http.DefaultTransport
      base, ok := orig.(*http.Transport)
      if !ok {
          t.Fatalf("withPinnedTLS: http.DefaultTransport is %T, want *http.Transport", orig)
      }
      clone := base.Clone()
      clone.TLSClientConfig = &tls.Config{RootCAs: pool}
      http.DefaultTransport = clone
      t.Cleanup(func() { http.DefaultTransport = orig })
  }
  ```

  Add the required imports (`crypto/tls`, `crypto/x509`) to `client_handshake_test.go`. `net/http` and `net/http/httptest` are already imported.

- [ ] **Task 3 — Call `withPinnedTLS(t, srv)` in every handshake test that uses `httptest.NewTLSServer`.** The affected tests are:
  - `TestRunSessionHandshakeSendsProtocolVersion` (around line 17)
  - `TestRunSessionProtocolVersionMismatchError` (around line 95)
  - `TestRunSessionOldServerHandshakeHint` (around line 132)
  - `TestRunSessionRequiresV2SessionMetadata` (around line 177)
  - `TestRunSessionExpandsConnectionPool` (around line 215)
  - `TestRunSessionHandlesMaxConnectionsUpdate` (around line 314)

  In each, add `withPinnedTLS(t, srv)` immediately after the `defer srv.Close()` line. Do not alter any other part of the test.

- [ ] **Task 4 — Verify no other test file in `pkg/client/` relies on `TestMain`'s mutation.** Grep `pkg/client/` for uses of `http.DefaultTransport`, `http.DefaultClient`, `httptest.NewTLSServer`, and `InsecureSkipVerify`. If any file outside `client_handshake_test.go` also hits a TLS test server through the proto dial path, it must also be updated to use `withPinnedTLS`. Current expectation (based on review): only `client_handshake_test.go` is affected, but confirm before finishing.

## Tests

After the change:

- `go test ./pkg/client/... -count=1 -race` must pass.
- Specifically these tests must still pass with the same assertions they have today:
  - `TestResolveTargetURL` (all subtests)
  - `TestRunSession_RejectsPlaintextWebSocket`
  - `TestRunSessionHandshakeSendsProtocolVersion`
  - `TestRunSessionProtocolVersionMismatchError`
  - `TestRunSessionOldServerHandshakeHint`
  - `TestRunSessionRequiresV2SessionMetadata`
  - `TestRunSessionExpandsConnectionPool`
  - `TestRunSessionHandlesMaxConnectionsUpdate`

No new test functions are required. The goal is to fix the TLS mutation while preserving exactly the current behavior and coverage.

## Acceptance criteria

1. `TestMain` is gone from `pkg/client/client_test.go`.
2. `http.DefaultTransport` is never mutated without a `t.Cleanup` that restores it.
3. No occurrence of `InsecureSkipVerify` anywhere under `pkg/client/`.
4. The helper `withPinnedTLS` exists in `client_handshake_test.go`, uses `RootCAs` with the test server's certificate, and has `InsecureSkipVerify` unset.
5. Every handshake test that calls `httptest.NewTLSServer` also calls `withPinnedTLS(t, srv)` immediately after.
6. No production file in `pkg/client/` is modified.
7. `go test ./pkg/client/... -count=1 -race` passes.
8. `go vet ./...` is clean.
9. `go build ./...` succeeds.

## Verification

```bash
cd /home/dev/worktrees/olivia/plex-tunnel

# Confirm no InsecureSkipVerify lingers
grep -RIn "InsecureSkipVerify" pkg/client/ && echo "FAIL: InsecureSkipVerify found" || echo "OK"

# Confirm TestMain is gone
grep -n "func TestMain" pkg/client/client_test.go && echo "FAIL: TestMain still present" || echo "OK"

# Confirm the production files weren't touched
git diff --name-only HEAD | grep -E '^pkg/client/' | grep -vE '_test\.go$' && echo "FAIL: prod file changed" || echo "OK"

# Build, vet, test
go build ./...
go vet ./...
go test ./pkg/client/... -count=1 -race
```

All four "OK" checks must print, and all three `go` commands must exit 0.
