# AppSec Audit (Client)

Date: 2026-04-02
Auditor: AppSec Engineer -- Authentication & Authorization
Scope: PR #30 (dev -> main) -- Changes to cmd/client/ui.go, cmd/client/main.go, pkg/client/client.go, pkg/client/pool.go, pkg/client/config.go, pkg/client/reconnect.go

## Summary

Findings identified: 7
- Critical: 0
- High: 1
- Medium: 3
- Low: 2
- Info: 1

## Finding 1: Raw Token Passed to Template Context Enables Accidental Leakage

Severity: High

Description:
The `statusPageData` struct includes the full `client.Config` object, which contains the raw plaintext `Token` field. While the current HTML template only references `{{.TokenMasked}}`, the raw token is accessible at `{{.Config.Token}}` within the template execution context. Any future template edit that references `.Config.Token` (or a debugging change that dumps the full struct) would silently expose the plaintext tunnel authentication token in the rendered HTML. The template data struct should never carry the raw secret.

Evidence:
- `cmd/client/ui.go:130-136` -- `statusPageData` embeds `client.Config` which contains `Token string` (see `pkg/client/config.go:13`)
- `cmd/client/ui.go:470-478` -- The full `cfg` (including raw `Token`) is passed as `Config: cfg` into the template data

Proposed Fix:
Create a sanitized view struct for the template that excludes the raw token entirely. For example:
```go
type configView struct {
    ServerURL      string
    Subdomain      string
    PlexTarget     string
    LogLevel       string
    MaxConnections int
}
```
Populate only the non-secret fields and use `configView` instead of `client.Config` in `statusPageData`. This removes the raw token from the template context entirely.

## Finding 2: Token Input Field Not Masked in Browser (type="text" Instead of type="password")

Severity: Medium

Description:
The server token input field in the settings form was changed from `type="password"` to a plain `<input>` (implicitly `type="text"`). This means the masked token value (`****abcd`) is rendered as visible text in the browser. While the value is already masked server-side, if a user types a new token, it will be fully visible on screen. This exposes the token to shoulder surfing and screen recording. The old code correctly used `type="password"` for this field.

Evidence:
- `cmd/client/ui.go:384` -- `<input name="token" value="{{.TokenMasked}}" required>` (no `type="password"`)
- The diff shows the old code had: `<input type="password" name="token" value="{{.Config.Token}}" required>`

Proposed Fix:
Restore `type="password"` on the token input field:
```html
<input type="password" name="token" value="{{.TokenMasked}}" required>
```

## Finding 3: UI Max Connections Validation Inconsistent with Backend Limits

Severity: Medium

Description:
The UI settings handler (`handleSettings`) validates `max_connections >= 1` but does not enforce the upper bound of 32, which is enforced both in `LoadConfig()` (`config.go:88`) and at the protocol level (`client.go:22-24` via `maxPoolConnections = 32`). A user submitting `max_connections=9999` through the UI form would pass UI validation but hit the cap silently in `runSession`. This creates a confusing UX at minimum, and means the UI-submitted config diverges from what actually runs.

Evidence:
- `cmd/client/ui.go:537-542` -- Only validates `maxConnections < 1`, no upper bound check
- `pkg/client/config.go:88-89` -- `LoadConfig` enforces `maxConnections > 32` returns error
- `pkg/client/client.go:207-209` -- `runSession` silently caps `grantedMax` at `maxPoolConnections (32)`

Proposed Fix:
Add upper bound validation in `handleSettings`:
```go
if convErr != nil || maxConnections < 1 || maxConnections > 32 {
    redirectWithMessage(w, r, "", "max connections must be between 1 and 32")
    return
}
```

## Finding 4: CSRF Origin Check Uses Attacker-Controllable Host Header

Severity: Medium

Description:
The CSRF protection in `handleSettings` constructs the allowed origin from `r.Host` (or `r.URL.Host`). The `Host` header is set by the client and can be spoofed if the UI is accessible through a non-TLS reverse proxy or directly. An attacker who can control the `Host` header (e.g., via DNS rebinding or a misconfigured proxy) could set `Host: evil.com` and then send `Origin: http://evil.com`, which would pass the CSRF check. This is a defense-in-depth concern -- the primary protection is that the UI defaults to loopback binding, and Basic Auth protects non-loopback deployments.

Evidence:
- `cmd/client/ui.go:495-520` -- Origin/Referer check builds the allowlist from `r.Host`
- No hardcoded expected host or additional validation against the configured listen address

Proposed Fix:
For defense in depth, consider also matching against the configured `PLEXTUNNEL_UI_LISTEN` address. Alternatively, use a CSRF token (synchronizer pattern) embedded in the form and validated server-side, which does not depend on Origin/Host at all.

## Finding 5: No Rate Limiting or Account Lockout on Basic Auth

Severity: Low

Description:
The Basic Auth middleware has no rate limiting, exponential backoff, or account lockout mechanism. An attacker with network access to the UI endpoint can perform unlimited brute-force attempts against the password. Since there is only a single username (`admin`), the attack surface is limited to password guessing only. The default loopback binding mitigates remote exploitation, but when `PLEXTUNNEL_UI_LISTEN` is set to a non-loopback address, this becomes relevant.

Evidence:
- `cmd/client/ui.go:452-458` -- Every request checks Basic Auth with no throttling or failure tracking

Proposed Fix:
Consider adding a simple per-IP rate limiter (e.g., `golang.org/x/time/rate` or a map of IP -> failure count with exponential cooldown) that returns HTTP 429 after N failed attempts within a time window.

## Finding 6: Missing Security Response Headers

Severity: Low

Description:
The UI HTTP responses do not include standard security headers: `X-Frame-Options`, `X-Content-Type-Options`, `Content-Security-Policy`, or `Cache-Control`. Without `X-Frame-Options: DENY`, the UI could be framed by a malicious page for clickjacking. Without `Cache-Control: no-store` on authenticated pages, credentials or sensitive config data could be cached by intermediary proxies.

Evidence:
- No security headers set anywhere in `cmd/client/ui.go` or `cmd/client/main.go`
- `cmd/client/ui.go:483` -- Only sets `Content-Type`, no other security headers

Proposed Fix:
Add a middleware or set headers in each handler:
```go
w.Header().Set("X-Frame-Options", "DENY")
w.Header().Set("X-Content-Type-Options", "nosniff")
w.Header().Set("Cache-Control", "no-store")
w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
```

## Finding 7: Token Masking Reveals Last 4 Characters Unconditionally

Severity: Info

Description:
The `maskToken` function always exposes the last 4 characters of the token (e.g., `****abcd`). For short tokens (5-8 characters), this reveals 50-80% of the token content. While the current token generation on the server side likely produces long tokens, there is no minimum token length enforcement on the client side, and users can set arbitrary tokens via the UI settings form. Exposing the last 4 characters also creates a fixed oracle that an attacker with UI read access could use to confirm whether they have the right token.

Evidence:
- `cmd/client/ui.go:22-28` -- `maskToken` shows last 4 chars for any token >4 chars
- `cmd/client/ui.go:529` -- The masked value is compared to determine whether the user changed the token in the form, meaning it is a functional part of the auth flow

Proposed Fix:
No immediate action required. For a more conservative approach, consider reducing the reveal to the last 2 characters, or masking differently for tokens shorter than 12 characters (e.g., show nothing).

## Notable Positive Controls

1. **Constant-time comparison for Basic Auth** -- `crypto/subtle.ConstantTimeCompare` is correctly used for password validation (`cmd/client/ui.go:454`), preventing timing side-channels.

2. **Token not exposed in API endpoint** -- The `/api/status` JSON endpoint correctly exposes only `token_set: bool`, not the raw token (`cmd/client/ui.go:596-606`). This is a significant improvement.

3. **CSRF protection on state-changing endpoint** -- The `handleSettings` POST endpoint validates Origin/Referer headers before processing the form (`cmd/client/ui.go:495-520`). While the implementation has the Host-header concern noted in Finding 4, having CSRF protection at all is a strong positive.

4. **Loopback default with non-loopback warning** -- The UI defaults to `127.0.0.1:9090` and explicitly warns when bound to a non-loopback address without a password (`cmd/client/main.go:46-47`). This is good security-by-default.

5. **HTTP server timeouts configured** -- `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, and `IdleTimeout` are all set (`cmd/client/main.go:53-56`), preventing slowloris and similar attacks. This was missing on the `main` branch.

6. **Method enforcement on all handlers** -- All HTTP handlers check the expected method and return 405 for others (`cmd/client/ui.go:465-467, 490-492, 582-584`).

7. **Input validation on settings form** -- Server URL scheme validation (`ws://`/`wss://`), Plex target scheme validation (`http://`/`https://`), log level parsing, and max connections numeric validation are all present.

8. **Token masking in UI prevents casual exposure** -- The token is masked before being sent to the HTML template, and the comparison logic (`submittedToken != maskToken(cfg.Token)`) correctly detects whether the user submitted a new token or left the mask unchanged.

9. **Concurrent stream semaphore** -- The new `streamSem` channel (`pkg/client/client.go:32, 268-274`) prevents unbounded goroutine creation from incoming requests, which is a good DoS mitigation.

10. **Upper bounds on pool connections and chunk sizes** -- `maxPoolConnections = 32` and `ResponseChunkSize` max of 4MB (`pkg/client/config.go:99`) prevent resource exhaustion through configuration manipulation.
