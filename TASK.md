# TASK: Client security hardening

## Overview
Fix high/medium security audit findings in the client: SSRF path validation, error message disclosure, fail-closed UI auth, container hardening, and multi-frame request rejection.

---

## Change 1: SSRF path validation — reject paths with scheme or host

The `resolveTargetURL` function in `pkg/client/client.go` (line 447-459) uses `url.ResolveReference()` which treats paths like `//evil.com/steal` as a host override, enabling SSRF from the public endpoint through the tunnel client into the client's local network.

### Where to work
- `pkg/client/client.go` — the `resolveTargetURL` function (line 447-459)

### Current behavior
```go
func resolveTargetURL(baseTarget string, path string) (string, error) {
    base, err := url.Parse(baseTarget)
    rel, err := url.Parse(path)
    return base.ResolveReference(rel).String(), nil
}
```

### Desired behavior
Before calling `ResolveReference`, validate the parsed `rel` URL:
- If `rel.Scheme` is not empty, reject
- If `rel.Host` is not empty, reject (catches `//host/path`)
- If the path doesn't start with `/`, reject

Return an error like `"blocked: path must be a relative path"` for any of these cases.

### Notes
Add a test in `pkg/client/client_test.go` for `resolveTargetURL` covering:
- Normal path `/library/metadata/123` → OK
- Path with query `/library?X-Plex-Token=abc` → OK
- SSRF path `//evil.com/steal` → rejected
- SSRF path `http://evil.com/` → rejected
- Empty path → should default to `/`

---

## Change 2: Generic error messages — stop leaking internal details

The client sends detailed error messages back through the tunnel that leak internal network information (local IPs, ports, DNS failures, TLS errors).

### Where to work
- `pkg/client/client.go` — the `handleHTTPRequest` function (lines 310-337) and `sendProxyError` function (line 430)

### Current behavior
Three error paths send detailed errors:
- Line 318: `fmt.Sprintf("invalid target path: %v", err)`
- Line 323: `fmt.Sprintf("build proxied request: %v", err)`
- Line 337: `fmt.Sprintf("request to plex failed: %v", err)`

### Desired behavior
Replace all three with generic messages:
- Line 318: `"bad gateway"` (log the real error locally)
- Line 323: `"bad gateway"` (log the real error locally)
- Line 337: `"upstream unavailable"` (log the real error locally)

For each, log the actual error at Warn level with the request ID before sending the generic message. Example:
```go
c.logger.Warn().Err(err).Str("request_id", msg.ID).Msg("target path resolution failed")
return c.sendProxyError(conn, msg.ID, http.StatusBadGateway, "bad gateway")
```

---

## Change 3: Fail-closed UI auth on non-loopback addresses

When `PLEXTUNNEL_UI_TOKEN` is not set and the UI is bound to a non-loopback address, the UI currently just logs a warning and starts without any auth.

### Where to work
- `cmd/client/ui.go` — the `resolveUIToken` function (lines 140-158)

### Current behavior
Returns empty string (auth disabled) when no token is set, regardless of listen address. Just logs a warning for non-loopback.

### Desired behavior
When `PLEXTUNNEL_UI_TOKEN` is not set AND the listen address is non-loopback:
1. Auto-generate a random token using `generateUIToken()` (which already exists in the file)
2. Log the generated token at Info level so the user can find it:
   ```go
   token := generateUIToken()
   logger.Info().Str("addr", listenAddr).Str("token", token).Msg("UI bound to non-localhost — auto-generated UI token (set PLEXTUNNEL_UI_TOKEN to use your own)")
   return token
   ```

This way:
- Localhost users: no auth (no friction)
- Non-localhost users without explicit token: auto-generated token, auth required
- Non-localhost users with explicit token: their token, auth required

---

## Change 4: Container security hardening

The client Docker image runs as root with no capability restrictions.

### Where to work
- `Dockerfile.client`
- `docker-compose.yml`
- `docker-compose.client.yml`

### Desired behavior for Dockerfile.client
After `COPY --from=builder /plextunnel-client /usr/local/bin/plextunnel-client`, add:
```dockerfile
RUN addgroup -g 65532 -S plextunnel && adduser -u 65532 -S -G plextunnel -h /nonexistent -s /sbin/nologin plextunnel
USER plextunnel:plextunnel
```

### Desired behavior for docker-compose.yml and docker-compose.client.yml
Add to the plextunnel-client service:
```yaml
    user: "65532:65532"
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    read_only: true
```

Keep `network_mode: host` as-is (required for Plex access on localhost).

---

## Change 5: Reject continuation request frames

The client spawns a new upstream HTTP request for every `MsgHTTPRequest` frame. The protocol supports multi-frame streaming requests, but the client doesn't implement reassembly. This means continuation frames become duplicate requests.

### Where to work
- `pkg/client/client.go` — the message handling loop where `MsgHTTPRequest` is dispatched (around line 244-253) and `handleHTTPRequest` (lines 310-335)

### Current behavior
Every `MsgHTTPRequest` message spawns a goroutine calling `handleHTTPRequest`, which creates a new upstream HTTP request.

### Desired behavior
In `handleHTTPRequest`, after the `msg.ID == ""` check, add a check: if the message has a body but no Method and no Path, it's a continuation frame. Reject it:
```go
if msg.Method == "" && msg.Path == "" {
    c.logger.Warn().Str("request_id", msg.ID).Msg("rejected continuation request frame (not supported)")
    return c.sendProxyError(conn, msg.ID, http.StatusNotImplemented, "streaming requests not supported")
}
```

This is a safe interim fix — continuation frames are rejected explicitly instead of being misinterpreted as new requests.

---

## Constraints
- DO NOT delete or modify any existing code that is not explicitly mentioned in this task.
- Keep all existing tests passing.
- The `network_mode: host` in compose files must stay (Plex needs localhost access).

## Verification
```bash
go build ./...
go test ./...
```

## When Done
Commit all changes with a descriptive commit message. Do NOT modify TASK.md.
