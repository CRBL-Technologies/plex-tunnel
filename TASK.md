# TASK: Fix client UI security audit findings

## Overview
Security audit found 3 issues in the client web UI: missing auth for non-loopback bindings, CSRF bypass when Origin header absent, and no scheme validation on plex_target. Fix all three.

---

## Change 1: Add basic auth middleware for non-loopback UI bindings

### Where to work
- `cmd/client/ui.go` — `newUIHandler()` function (line ~403)
- `cmd/client/main.go` — where `PLEXTUNNEL_UI_LISTEN` is read (line ~42)

### Current behavior
The UI is completely unauthenticated. When bound to a non-loopback address (e.g. `0.0.0.0:9090` for Docker), anyone with network access can read config and change settings.

### Desired behavior
1. Read `PLEXTUNNEL_UI_PASSWORD` env var at startup in `main.go`, pass it into the UI handler.
2. In `newUIHandler`, accept a `password string` parameter.
3. If password is non-empty, wrap the mux with a basic auth middleware that checks username `admin` and password against the env var. Use `crypto/subtle.ConstantTimeCompare` for the password check.
4. At startup in `main.go`, if the listen address is NOT a loopback address (not `127.0.0.1`, `::1`, or `localhost`) AND `PLEXTUNNEL_UI_PASSWORD` is empty, log a WARNING: `"UI bound to non-loopback address without password — set PLEXTUNNEL_UI_PASSWORD to protect it"`.
5. The `/api/status` endpoint should also be behind the auth middleware (it's part of the same mux).

### Notes
- Use `crypto/subtle.ConstantTimeCompare` for the password comparison
- The basic auth realm should be "Portless Client"
- The middleware extracts credentials with `r.BasicAuth()`

---

## Change 2: Strengthen CSRF to check Referer when Origin is missing

### Where to work
- `cmd/client/ui.go` — `handleSettings()` function (line ~447)

### Current behavior
The CSRF check only validates when the `Origin` header is present. If `Origin` is absent, the POST proceeds unchecked.

### Desired behavior
1. If `Origin` header is present and non-empty, validate it against `http://` + host and `https://` + host (keep existing logic).
2. If `Origin` is empty, check the `Referer` header instead. Parse the Referer URL and compare its scheme+host against the same allowed origins.
3. If BOTH `Origin` and `Referer` are empty/absent, reject the request with 403 Forbidden.

### Notes
- Use `url.Parse` to extract scheme+host from Referer
- The comparison should be `refererOrigin == allowed || refererOrigin == allowedS`
- Where `refererOrigin = parsed.Scheme + "://" + parsed.Host`

---

## Change 3: Validate plex_target scheme in UI settings handler

### Where to work
- `cmd/client/ui.go` — `handleSettings()` function (line ~472)

### Current behavior
`plex_target` is accepted without any scheme validation. An attacker with UI access could set it to `file:///etc/passwd` or an internal cloud metadata URL.

### Desired behavior
After setting `cfg.PlexTarget`, validate that it has an `http://` or `https://` scheme:
```go
if parsed, err := url.Parse(cfg.PlexTarget); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
    redirectWithMessage(w, r, "", "plex target must be a valid http:// or https:// URL")
    return
}
```

Place this validation after `cfg.PlexTarget` is set and before the existing `cfg.ServerURL` validation.

---

## Tests

No new test files needed. The existing tests should still pass.

---

## Constraints
- DO NOT delete or modify any existing code that is not explicitly mentioned in this task.
- Keep all existing tests passing.
- DO NOT change the HTML template or CSS.
- DO NOT add new dependencies.
- Keep the fix minimal and focused.

## Verification
```bash
go build ./...
go test ./...
```

## When Done
Commit all changes with a descriptive commit message. Do NOT modify TASK.md.
