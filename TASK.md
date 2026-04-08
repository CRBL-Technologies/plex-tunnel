# TASK: client UI form-based auth + session cookie + logout

## Context

PR #70 (already merged) added a startup safety check at `cmd/client/main.go:46-48` that
fatally refuses to start if `PLEXTUNNEL_UI_LISTEN` is non-loopback and
`PLEXTUNNEL_UI_PASSWORD` is empty. This is correct and stays.

While fixing the resulting startup failure on his NAS, the CEO discovered the existing
auth UX is bad: HTTP Basic Auth via browser popup, hardcoded username `"admin"` (see
`cmd/client/ui.go:472-481`), no logout button, and the user has to GUESS the username.

This task replaces HTTP Basic Auth with a proper form-based login + session cookie +
logout. Critically, when the user does NOT specify a username via env var, the login
form is **password-only** — they should not have to guess "admin".

This is a **breaking change** for any caller that was using HTTP Basic Auth (e.g.
scripts POSTing to `/api/status` with basic auth). There is no migration path.

## Goal

Replace HTTP Basic Auth on the client status UI with a form-based login that issues a
session cookie, supports an optional username (password-only mode by default), enforces
per-IP rate limiting on login, and provides a logout button.

## Constraints / guardrails

- **DO NOT delete or modify code not mentioned in this task.** Specifically, do NOT
  touch the controller (`clientController`), the settings handler logic, the status
  JSON shape, the metrics endpoint, the existing CSRF Origin/Referer check, or the
  existing token-masking logic.
- **DO NOT touch anything outside `cmd/client/`, `README.md`, and (if you create one)
  a new `CHANGELOG.md` at the repo root.**
- **DO NOT touch the listener or anything under `pkg/`.** That is being changed in a
  parallel PR on the server side.
- The existing `isLoopbackUIListen` check at `cmd/client/main.go:46` and its fatal log
  message stay **unchanged**.
- `/metrics` MUST remain unauthenticated. Do not gate it behind the new session check.
- Use `crypto/subtle.ConstantTimeCompare` for **both** the username and password
  comparisons. Compare lengths first as a separate constant-time-safe check (or pad to
  fixed length) so a length mismatch does not short-circuit and leak via timing.
- Cookie attributes must be exactly: `HttpOnly=true`, `Secure=(r.TLS != nil)`,
  `SameSite=http.SameSiteStrictMode`, `Path=/`.
- All shared state (session store map, rate-limit map) must be protected by
  `sync.Mutex` (or `sync.RWMutex` if read-heavy). The test suite will run with
  `go test -race` and must pass cleanly.
- No new external dependencies. Use only the Go standard library and what is already
  in `go.mod`.
- The login page HTML must be self-contained (no JS, no external CSS, no external
  fonts) and visually consistent with the existing status page styling at
  `cmd/client/ui.go` (same color tokens, same card pattern).

## Background — what already exists

- `cmd/client/main.go` reads `PLEXTUNNEL_UI_LISTEN` and `PLEXTUNNEL_UI_PASSWORD`,
  validates loopback, and constructs the UI handler at line 50-57.
- `cmd/client/ui.go` defines `uiHandler` with routes: `GET /`, `POST /settings`,
  `GET /api/status`, `GET /metrics` (unauthenticated). The basic-auth wrapper is at
  `cmd/client/ui.go:471-481`. The hardcoded `"admin"` username is on line 473.
- The status page template (`statusPageTmpl`) is an inline `html/template` at lines
  142-439. It defines the `--bg`, `--card`, `--surface`, `--border`, `--text`,
  `--muted`, `--accent`, `--accent-hover`, `--ok-*`, `--bad-*` CSS variables — reuse
  them for the login page and the logout button.
- The existing CSRF check at `cmd/client/ui.go:517-537` validates `Origin`/`Referer`
  against `allowedOrigin` (derived from `PLEXTUNNEL_UI_ORIGIN` or
  `"http://" + listenAddr`). The same check should apply to `POST /login` and
  `POST /logout`.
- There is **no `/healthz` endpoint** in the client today. Do not add one. Just make
  sure if one is added later it won't be silently auth-gated by accident — i.e. the
  auth gating MUST be a per-route allowlist or middleware decision, not a "deny by
  default for everything except /metrics" trap that someone has to remember.
- README env var table is at `README.md:60-74`.

## File-by-file plan

### NEW: `cmd/client/login.go`

Create this new file with:

1. **Session store**

   ```go
   type sessionStore struct {
       mu       sync.Mutex
       sessions map[string]time.Time // token -> createdAt
       ttl      time.Duration
   }
   ```

   Methods:
   - `newSessionStore(ttl time.Duration) *sessionStore`
   - `(s *sessionStore) Create() (string, error)` — generate 32 random bytes via
     `crypto/rand`, base64-url encode (no padding), store with `time.Now()`, return
     the token.
   - `(s *sessionStore) Validate(token string) bool` — return true iff present AND
     `time.Since(createdAt) <= ttl`. Sweep that token if expired.
   - `(s *sessionStore) Delete(token string)`
   - `(s *sessionStore) sweep()` — iterate and delete all expired tokens.
   - `(s *sessionStore) StartGC(ctx context.Context)` — goroutine that calls `sweep`
     every 1h until ctx is cancelled. Use `time.NewTicker` and `select` on
     `ctx.Done()`.

2. **Rate limiter**

   ```go
   type loginRateLimiter struct {
       mu      sync.Mutex
       buckets map[string]*loginBucket
   }
   type loginBucket struct {
       attempts    int
       windowStart time.Time
       blockedUntil time.Time
   }
   ```

   Method `Allow(ip string, now time.Time) bool`:
   - If `now.Before(blockedUntil)` → return false.
   - If `now.Sub(windowStart) > 1*time.Minute` → reset window: `attempts = 1`,
     `windowStart = now`, return true.
   - Else increment attempts. If `attempts > 10` → set
     `blockedUntil = now.Add(5*time.Minute)`, return false. Else return true.

   Use `r.RemoteAddr` (strip the port via `net.SplitHostPort`) for the IP key. This is
   local-network exposure, not behind a proxy — do NOT consult `X-Forwarded-For`.

   Add a periodic GC for stale buckets (sweep entries with `windowStart` older than
   10 minutes AND `now.After(blockedUntil)`) — same goroutine pattern as the session
   store, every 1h.

3. **Auth credential check**

   ```go
   type authConfig struct {
       Username string // empty = password-only mode
       Password string
   }

   func (a authConfig) Verify(submittedUsername, submittedPassword string) bool
   ```

   Logic:
   - If `a.Username == ""`:
     - If `submittedUsername != ""` → return false (reject any submitted username when
       not configured — this guards against a client sending one anyway).
     - Compare `submittedPassword` to `a.Password` via constant-time-safe path
       (length check first, then `subtle.ConstantTimeCompare`).
   - If `a.Username != ""`:
     - Compare both username and password constant-time-safely. Both must match.
   - Always do BOTH comparisons in the configured-username branch even if the first
     fails, so timing reveals only "configured / not configured", not "which field
     was wrong".

4. **Login page template** — `loginPageTmpl`, inline `html/template.Must(...)`,
   matching the existing styling. Template data:

   ```go
   type loginPageData struct {
       UsernameField bool   // true if PLEXTUNNEL_UI_USERNAME is set
       Error         string // populated if ?error=invalid is in the query
       Next          string // pass-through ?next= param to preserve target after login
   }
   ```

   Layout:
   - Centered card (max-width ~360px), reuse `--bg`, `--card`, `--border`, `--text`,
     `--accent` tokens.
   - Page title "Portless Client Login".
   - Form `method="post" action="/login"`.
   - Hidden input `name="next" value="{{.Next}}"`.
   - If `.UsernameField` → `<input name="username" type="text" autocomplete="username"
     required>` with a label.
   - Always: `<input name="password" type="password" autocomplete="current-password"
     required>` with a label.
   - Submit button "Sign in".
   - Error area (only if `.Error != ""`): "Invalid credentials" or "Too many attempts,
     try again later" — pick the right message based on the error code.

5. **Handlers**

   - `handleLoginGet(w, r)` — render the login page. Read `?error=` and `?next=` from
     the query.
   - `handleLoginPost(w, r)`:
     1. Apply CSRF Origin/Referer check (reuse the same logic as the settings handler;
        extract it into a small helper if helpful — but do NOT delete or modify the
        existing settings call site).
     2. Apply rate limit `Allow(...)`. If denied → HTTP 429, body `"too many
        attempts"`. Do NOT redirect; the spec wants a 429 for rate-limit denials.
     3. `r.ParseForm()` with a `MaxBytesReader` of 8 KiB.
     4. Look up `username` and `password` form values.
     5. Call `authConfig.Verify(...)`. If false → redirect to
        `/login?error=invalid&next=<...>` (303).
     6. On success: `sessionStore.Create()` → set cookie → redirect to the `next`
        value if it is a safe local path (must start with `/` and not `//`), else `/`.

   - `handleLogoutPost(w, r)`:
     1. Apply CSRF Origin/Referer check (same as login POST).
     2. Read the session cookie. If present, call `sessionStore.Delete(token)`.
     3. Set a `plextunnel_session` cookie with `MaxAge=-1`, same other attributes.
     4. Redirect to `/login` (303).

6. **Auth middleware** — `requireSession(next http.Handler) http.Handler`:
   - Read cookie `plextunnel_session`.
   - If absent OR `sessionStore.Validate(token) == false` →
     - For `GET` requests: 303 redirect to `/login?next=<r.URL.Path>` (URL-escape).
     - For non-GET (POST/PUT/...): 401 Unauthorized JSON `{"error":"unauthorized"}`.
   - Else `next.ServeHTTP(w, r)`.

   The middleware must be applied per-route, not globally. `/metrics` and `/login`
   and `/logout` (and any future `/healthz`) must NOT be wrapped.

### MODIFY: `cmd/client/ui.go`

1. Remove the basic auth wrapper at lines 471-481. The function `newUIHandler` should
   no longer take a `password string` argument by itself — instead take an
   `authConfig` and a `*sessionStore`. Update its signature and the call site in
   `main.go` accordingly.

2. Update `newUIHandler` to register routes:
   - `/login` → `handleLoginGet` (GET) / `handleLoginPost` (POST) — NOT wrapped by
     `requireSession`.
   - `/logout` → `handleLogoutPost` (POST only) — NOT wrapped.
   - `/` → `requireSession(handleIndex)`.
   - `/settings` → `requireSession(handleSettings)`.
   - `/api/status` → `requireSession(handleStatus)`.
   - `/metrics` → `promhttp.HandlerFor(...)` — NOT wrapped.

3. **Add a small logout button to the existing status page template** in
   `statusPageTmpl`. Place it in the top-right of the first `.panel` (or as a small
   header bar) — a `<form method="post" action="/logout">` containing only a
   `<button>Sign out</button>`. Style it small/unobtrusive (matching the existing
   button look but smaller). Do NOT change any other part of the template.

4. The `secured` security-headers wrapper stays, but it now wraps the entire mux
   (login page included). Keep `X-Frame-Options`, `X-Content-Type-Options`,
   `Cache-Control: no-store`.

### MODIFY: `cmd/client/main.go`

1. Read two new env vars:
   - `uiUsername := os.Getenv("PLEXTUNNEL_UI_USERNAME")` (default empty).
   - `sessionTTL`: parse from `PLEXTUNNEL_UI_SESSION_TTL` via `time.ParseDuration`.
     Default `7 * 24 * time.Hour` if unset. If set but unparseable → log fatal with a
     clear message naming the env var.

2. The existing safety check at line 46-48 stays unchanged.

3. Construct the session store and pass it (with the auth config and listenAddr) into
   `newUIHandler`. Start the GC goroutine with the root `ctx`.

4. Pass the auth config (`{Username: uiUsername, Password: uiPassword}`) into the
   handler. The existing `uiPassword` variable and the existing fatal-log behavior
   stay.

### MODIFY: `cmd/client/ui_test.go`

- `TestUIHandler_PasswordProtected` is no longer correct — basic auth is gone.
  REMOVE this test and replace it with the new tests in `login_test.go` (see Tests
  section). You may keep `TestUIHandler_IndexPage`, `TestUIHandler_StatusAPI`,
  `TestUIHandler_SettingsCSRFRejectsNoOrigin`, `TestMaskToken` — but update the
  ones that exercise authenticated routes (`/`, `/api/status`, `/settings`) to set up
  a valid session cookie first via a small test helper. The "no auth configured"
  case (empty password) means anyone can hit any route — preserve this code path so
  the existing tests still work in unprotected mode if you keep them that way.
- The test helper for "create authenticated request" should:
  - Build a `*sessionStore`, call `Create()`, and add a `plextunnel_session=<token>`
    cookie to the request.
  - Pass that store into `newUIHandler` so it shares state with the request.

### NEW: `cmd/client/login_test.go`

See Tests section below.

### MODIFY: `README.md`

Update the env var table at line 60-74. Replace the line for `PLEXTUNNEL_UI_PASSWORD`
and add three new rows. The full new block should look like:

```
| `PLEXTUNNEL_UI_LISTEN` | no | `127.0.0.1:9090` | Local status UI address; set empty to disable |
| `PLEXTUNNEL_UI_USERNAME` | no | — | Optional username for the UI login form. If unset, the form is password-only. |
| `PLEXTUNNEL_UI_PASSWORD` | no | — | Password for the UI login form. **Required** when UI is bound to a non-loopback address. |
| `PLEXTUNNEL_UI_SESSION_TTL` | no | `168h` | Session cookie lifetime (Go duration, e.g. `24h`, `168h`). |
```

Update the "Status UI" prose paragraph at line 78 to describe form-based login + the
optional username + the new env vars. Mention that this is a breaking change vs
prior versions and that any HTTP Basic Auth-based scripts must be updated.

### NEW: `CHANGELOG.md`

Create a new `CHANGELOG.md` at the repo root with a single entry:

```markdown
# Changelog

## Unreleased

### Breaking changes

- The client status UI no longer accepts HTTP Basic Auth. The previous hardcoded
  `admin` username is removed. Users must now log in via a form at `/login` and
  receive a session cookie. Existing scripts that POST to `/api/status` with
  HTTP Basic Auth will need to be updated to obtain and send the
  `plextunnel_session` cookie instead.

### Added

- `PLEXTUNNEL_UI_USERNAME` env var (optional). When set, the login form requires
  both username and password. When unset, the form is password-only — users do not
  have to guess a username.
- `PLEXTUNNEL_UI_SESSION_TTL` env var (optional, default `168h`). Sets the session
  cookie lifetime.
- Logout button in the status UI.
- Per-IP login rate limiting: max 10 POST `/login` attempts per minute per source IP;
  exceeded IPs are blocked for 5 minutes.
```

## Tasks

- [ ] Create `cmd/client/login.go` with session store, rate limiter, auth config,
      login template, and handlers as specified.
- [ ] Modify `cmd/client/ui.go`: remove basic auth wrapper, register new routes
      with per-route session middleware, add logout button to status template.
- [ ] Modify `cmd/client/main.go`: read new env vars, build session store + auth
      config, start GC goroutine, pass into handler.
- [ ] Update `cmd/client/ui_test.go`: remove `TestUIHandler_PasswordProtected`,
      adapt remaining tests to use a session cookie helper.
- [ ] Create `cmd/client/login_test.go` with the test cases listed below.
- [ ] Update `README.md` env var table and Status UI prose.
- [ ] Create `CHANGELOG.md` with the breaking-change entry.
- [ ] `go build ./...` succeeds.
- [ ] `go test ./... -race` passes.
- [ ] `gofmt -s -w cmd/client/` clean.
- [ ] `go vet ./...` clean.

## Tests

All in `cmd/client/login_test.go` unless noted. Each test must build a `uiHandler`
via `newUIHandler` with a fresh `*sessionStore` and a fresh `loginRateLimiter` so
tests are hermetic.

1. `TestLogin_PasswordOnly_Success` — `authConfig{Password: "pw"}`, POST `/login`
   with form `password=pw`, no `username` field. Expect 303 redirect to `/`, and a
   `Set-Cookie: plextunnel_session=...; HttpOnly; SameSite=Strict; Path=/` header.

2. `TestLogin_UsernameAndPassword_Success` — `authConfig{Username: "u", Password:
   "pw"}`, POST with both fields. Expect 303 + cookie set.

3. `TestLogin_UsernameSubmittedWhenNotConfigured_Reject` — `authConfig{Password:
   "pw"}` (no username). POST with `username=admin&password=pw`. Expect 303
   redirect to `/login?error=invalid` and NO cookie set.

4. `TestLogin_WrongPassword_Reject` — POST with `password=nope`. Expect 303 to
   `/login?error=invalid`, no cookie.

5. `TestLogin_NoPassword_Reject` — POST with empty body. Expect 303 to
   `/login?error=invalid`, no cookie.

6. `TestLogin_RateLimit_429OnEleventh` — Loop 11 POSTs from `RemoteAddr=1.2.3.4:5555`
   with wrong password. The 11th must respond `http.StatusTooManyRequests`.

7. `TestLogin_RateLimitWindowExpires` — Use a `loginRateLimiter` with an injectable
   `now` function (or call its internal method directly). Make 11 attempts at `t0`,
   then advance `now` by >1 minute and >5 minutes (past the block window), and the
   next attempt should be allowed.

8. `TestSession_MissingCookie_RedirectsToLogin` — GET `/` with no cookie.
   Expect 303 redirect, `Location` header containing `/login?next=%2F` (URL-escaped).

9. `TestSession_ExpiredCookie_RedirectsToLogin` — Use a `sessionStore` with TTL =
   1ms. Create token, sleep ~5ms, GET `/` with that cookie. Expect 303 to `/login`.

10. `TestSession_TamperedCookie_Reject` — GET `/` with cookie value
    `plextunnel_session=not-a-real-token`. Expect 303 to `/login`. Also test cookie
    value `!!!invalid base64!!!` — same expectation (must not panic).

11. `TestLogout_ClearsCookieAndRedirects` — POST `/logout` with a valid session
    cookie + matching `Origin` header. Expect 303 to `/login`, response should
    include `Set-Cookie: plextunnel_session=; Max-Age=0` (or `-1`), and a follow-up
    GET `/` with the same (now-invalidated) cookie should redirect to `/login`.

12. `TestMetrics_NoAuthRequired` — GET `/metrics` with no cookie. Expect 200 OK.

13. `TestSession_ConcurrentCreate_RaceFree` — spawn 100 goroutines each calling
    `sessionStore.Create()` concurrently and storing the result. After they all
    finish, all 100 tokens must be unique and all 100 must `Validate()` true. This
    test is the smoke test for `go test -race`.

14. `TestLogin_NextParamPreserved` — GET `/login?next=/settings` renders the form
    with hidden `next=/settings`. POST `/login?next=/settings` with valid creds →
    303 to `/settings`. Also test `next=//evil.com/x` → must be rejected and
    redirect to `/` instead (open-redirect guard).

15. `TestLogin_CSRF_RejectBadOrigin` — POST `/login` with `Origin: http://evil.test`
    → 403.

For the existing tests in `ui_test.go`, update or remove as needed. Specifically:
- `TestUIHandler_IndexPage` and `TestUIHandler_StatusAPI` must now set up a valid
  session cookie (use a helper).
- `TestUIHandler_PasswordProtected` is removed entirely.
- `TestUIHandler_SettingsCSRFRejectsNoOrigin` is fine as-is, but the request should
  also include a valid session cookie since `/settings` is now session-gated.

## Acceptance criteria

1. With `PLEXTUNNEL_UI_USERNAME` unset and `PLEXTUNNEL_UI_PASSWORD=secret`:
   - Visiting `/` while unauthenticated → 303 to `/login?next=%2F`.
   - The `/login` page shows ONE password field, no username field.
   - Submitting `password=secret` → cookie set, redirect to `/`.
   - Submitting `password=wrong` → back to `/login?error=invalid`.
   - Submitting `username=admin&password=secret` → REJECTED (no username allowed).
2. With `PLEXTUNNEL_UI_USERNAME=alice` and `PLEXTUNNEL_UI_PASSWORD=secret`:
   - The `/login` page shows BOTH a username and a password field.
   - Submitting `username=alice&password=secret` → cookie set, redirect to `/`.
   - Submitting `username=bob&password=secret` → back to `/login?error=invalid`.
3. After login, the status page renders normally and includes a "Sign out" button
   in the top-right. Clicking it issues `POST /logout`, clears the cookie, and
   redirects to `/login`.
4. `GET /metrics` works without any cookie at all (smoke test via curl in dev).
5. 11th rapid POST to `/login` from the same IP within a minute → HTTP 429.
6. `go test ./... -race` passes cleanly.
7. `go vet ./...` clean.
8. The startup safety check at `cmd/client/main.go:46-48` is **unchanged**.

## Verification

```bash
cd /home/dev/worktrees/paul/plex-tunnel
go build ./...
go vet ./...
gofmt -s -d cmd/client/
go test ./... -race
```

All four must succeed. `gofmt` should produce zero output.

## DO NOT

- Do NOT delete or rename `clientController`, `Snapshot`, `ApplyConfig`, `Start`,
  `Stop`, `maskToken`, `redirectWithMessage`, the existing `statusPageTmpl` (other
  than adding the logout button), `handleSettings` (other than wrapping it with
  `requireSession` at registration time), `handleStatus`, or `handleIndex`.
- Do NOT change the JSON shape returned by `/api/status`.
- Do NOT add any new dependencies to `go.mod`.
- Do NOT touch `pkg/`, `cmd/server/` (if it exists in this repo), `Dockerfile.client`,
  `docker-compose*.yml`, the workflows under `.github/`, or any test files outside
  `cmd/client/`.
- Do NOT change `isLoopbackUIListen` or its callsite. The fatal startup check is
  load-bearing — leave it alone.
- Do NOT add a `/healthz` endpoint. (If a future PR adds one, the per-route
  middleware design we're using makes it trivial — just don't wrap it. But that's
  not this PR.)
