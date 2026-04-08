# fix(client-ui): accept any host with matching port when bound to 0.0.0.0

## Context

PR #72 shipped form-based login + session cookies + logout for the client UI. The CSRF-like origin check at `cmd/client/login.go:440` (`originAllowed`) is correct for loopback binds but is broken when the client listens on `0.0.0.0:9090` (or `[::]:9090` — i.e. "bind to all interfaces").

Default behaviour today:

```go
allowedOrigin := os.Getenv("PLEXTUNNEL_UI_ORIGIN")
if allowedOrigin == "" {
    allowedOrigin = "http://" + listenAddr
}
```

When `listenAddr == "0.0.0.0:9090"` the default becomes `http://0.0.0.0:9090`. A real browser will never send `Origin: http://0.0.0.0:9090` — it sends the host the user typed (`http://192.168.1.50:9090`, `http://nas.local:9090`, etc.). Result: every login POST and logout POST is rejected with 403.

CEO hit this on his NAS today. Workaround for him: set `PLEXTUNNEL_UI_ORIGIN` explicitly. The default behaviour must not be broken.

While we are touching this, the same broken default also applies to `POST /settings` in `cmd/client/ui.go:529` (`handleSettings`), which has its own inlined copy of the same origin check logic. Fix it once at the helper level and have `handleSettings` call the helper, so the bug is repaired in `/login`, `/logout`, and `/settings` simultaneously and the duplication goes away.

## Goal

When the configured listen address is a "bind to all interfaces" address, the default origin check accepts any host as long as the scheme is http/https and the port matches the listener's port. When the listen address is concrete (loopback, LAN IP, hostname), the default behaviour is unchanged: exact match against `http://<listenAddr>`. The `PLEXTUNNEL_UI_ORIGIN` env var still wins over both.

## Constraints / guardrails

- **DO NOT delete or modify code not mentioned in this task.** In particular do NOT touch the session store, login rate limiter, login form template, settings handler beyond replacing its origin check, or anything in `pkg/`.
- Do NOT add new env vars. The only existing override knob (`PLEXTUNNEL_UI_ORIGIN`) keeps its current semantics.
- Do NOT change the `Origin` header / `Referer` header parsing semantics otherwise — same precedence (Origin first, Referer fallback), same "no header → reject" rule.
- Do NOT change the cookie code, the `requireSession` middleware, or the login rate limit.
- The "bind to all interfaces" detection must cover: host portion is literally `0.0.0.0`, `::`, `[::]`, or empty string. Use `net.SplitHostPort` to extract the host portion. If `SplitHostPort` fails, fall back to current "exact string match" behaviour for safety.
- Keep the change a focused single-purpose commit.

## Tasks

- [ ] In `cmd/client/ui.go`, change the `uiHandler` struct so that instead of (or in addition to) storing a single `allowedOrigin` string, it stores:
  - the explicit override (`PLEXTUNNEL_UI_ORIGIN`) if set, as `originOverride string`
  - the parsed host and port of the configured `listenAddr` (`listenHost`, `listenPort string`)
  - a boolean `bindAll bool` set to true when the host portion is `0.0.0.0`, `::`, `[::]`, or empty
  - You may keep `listenAddr` as-is for logging if needed; do not remove it.
- [ ] Update `newUIHandler` (`cmd/client/ui.go:460`) to populate those fields. Read `PLEXTUNNEL_UI_ORIGIN`, then `net.SplitHostPort(listenAddr)`. If `SplitHostPort` errors, set `bindAll = false` and keep the legacy behaviour (exact match against `"http://" + listenAddr`) by falling back through `originOverride = "http://" + listenAddr` so the existing semantics are preserved. The existing `allowedOrigin` field can be kept or removed — your choice — as long as the fields above are the source of truth for the new check.
- [ ] Rewrite `originAllowed` in `cmd/client/login.go:440` to use the new fields:
  1. If `h.originOverride != ""`, behave exactly like today: extract `scheme://host` from the Origin header (or Referer fallback) and require exact equality with `h.originOverride`.
  2. Else if `h.bindAll` is true, accept any Origin/Referer whose scheme is `http` or `https` and whose port equals `h.listenPort`. Host can be anything.
  3. Else (concrete listen host, no override), behave like today: exact equality against `"http://" + h.listenHost + ":" + h.listenPort`.
  4. Same precedence as today: prefer `Origin` header; if missing, parse `Referer`; if both missing, return false.
  5. Use `net/url.Parse` for the Referer path (already imported) and `net.SplitHostPort` to pull port off the parsed host. If parsing fails, return false.
- [ ] In `cmd/client/ui.go` `handleSettings` (lines ~529–555), delete the inline origin check and replace it with a call to `h.originAllowed(r)`. Same forbidden-on-fail behaviour. This is the only allowed change to `handleSettings`.
- [ ] In `cmd/client/login_test.go`, add the test cases listed in the **Tests** section below. Existing tests must keep passing unchanged — `newTestLoginHandler` already wires `listenAddr = "127.0.0.1:9090"` and existing tests use `Origin: http://127.0.0.1:9090`, so the legacy concrete-host path must keep working.
- [ ] Run `go build ./...` and `go test ./...` from the repo root. Both must be clean.

## Tests

Add these as new test functions in `cmd/client/login_test.go`. You may need a small helper that builds a `uiHandler` (or `http.Handler`) with a custom `listenAddr` and optional `PLEXTUNNEL_UI_ORIGIN` env var set via `t.Setenv`. Use `httptest.NewRecorder` and `httptest.NewRequest`. Each case posts to `/login` with form `password=pw` and the named `Origin` header, then asserts the listed status.

Required cases (one test function per scenario, or one table-driven test, your call):

1. `TestLogin_Origin_BindAll_AcceptsLANHost`
   listenAddr `0.0.0.0:9090`, Origin `http://192.168.1.50:9090` → 303 SeeOther (accepted, login succeeds)
2. `TestLogin_Origin_BindAll_AcceptsHostnameHost`
   listenAddr `0.0.0.0:9090`, Origin `http://nas.local:9090` → 303 SeeOther
3. `TestLogin_Origin_BindAll_AcceptsAnyHostWithMatchingPort`
   listenAddr `0.0.0.0:9090`, Origin `http://evil.com:9090` → 303 SeeOther.
   Comment in the test that we accept any host with matching port when bound to all interfaces — the port is the actual ingress, auth is via password, the origin check is a secondary defence.
4. `TestLogin_Origin_BindAll_RejectsPortMismatch`
   listenAddr `0.0.0.0:9090`, Origin `http://nas.local:8080` → 403
5. `TestLogin_Origin_BindAll_IPv6Wildcard`
   listenAddr `[::]:9090`, Origin `http://nas.local:9090` → 303 SeeOther (covers `[::]` host detection)
6. `TestLogin_Origin_ConcreteHost_AcceptsExactMatch`
   listenAddr `127.0.0.1:9090`, Origin `http://127.0.0.1:9090` → 303 SeeOther (legacy behaviour unchanged)
7. `TestLogin_Origin_ConcreteHost_RejectsOtherHost`
   listenAddr `127.0.0.1:9090`, Origin `http://192.168.1.50:9090` → 403 (legacy behaviour unchanged — concrete listener does NOT relax)
8. `TestLogin_Origin_ExplicitOverride_Accepts`
   `t.Setenv("PLEXTUNNEL_UI_ORIGIN", "https://ui.example.com")`, listenAddr `0.0.0.0:9090`, Origin `https://ui.example.com` → 303 SeeOther
9. `TestLogin_Origin_ExplicitOverride_RejectsMismatch`
   `t.Setenv("PLEXTUNNEL_UI_ORIGIN", "https://ui.example.com")`, listenAddr `0.0.0.0:9090`, Origin `http://192.168.1.50:9090` → 403 (override wins even on bind-all)

Notes for the tests:
- Use `t.Setenv` so the env var is reset after the test.
- For the helper that builds the handler with a custom listenAddr, follow the existing `newTestLoginHandler` pattern in `login_test.go:15` — same controller, same logger, same store/limiter, just a different `listenAddr` arg.
- The existing `TestLogin_CSRF_RejectBadOrigin` (`login_test.go:377`) must keep passing unchanged — it uses the default test handler (`127.0.0.1:9090`, no override) and `Origin: http://evil.test`, which is still a 403 under the new logic (concrete host, exact-match path).

## Acceptance criteria

- `originAllowed` returns true for the listed bind-all cases and false for the listed mismatch cases.
- `handleSettings` now delegates to `h.originAllowed(r)` and the duplicated inline check is gone.
- All new tests pass; all existing tests in `cmd/client/...` still pass.
- `go build ./...` clean. `go vet ./...` clean.
- No new env vars introduced. No changes outside `cmd/client/login.go`, `cmd/client/ui.go`, `cmd/client/login_test.go`.

## Verification

```
cd /home/dev/worktrees/paul/plex-tunnel
go build ./...
go vet ./...
go test ./...
```

All three must exit 0.
