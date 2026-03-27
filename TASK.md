# TASK: Remove UI token authentication from client web UI

## Overview
The client web UI has a separate "UI token" auth system that is confusing to users. When the UI listens on a non-loopback address, it auto-generates a random token and requires users to enter it before accessing the client dashboard. Users confuse this with the tunnel token from the server dashboard, leading to "Invalid token" errors. The UI token provides no real security value — remove it entirely.

---

## Change 1: Remove UI token from `uiHandler` struct and `newUIHandler`

### Where to work
- `cmd/client/ui.go` — `uiHandler` struct (line 118), `newUIHandler` function (line 472)

### Current behavior
The `uiHandler` struct has a `uiToken string` field. `newUIHandler` accepts a `uiToken string` parameter, stores it, and conditionally wraps the mux with `tokenAuthMiddleware`.

### Desired behavior
- Remove the `uiToken` field from the `uiHandler` struct.
- Change `newUIHandler` signature to `func newUIHandler(controller *clientController, logger zerolog.Logger) http.Handler` (remove the `uiToken` parameter).
- Remove the `if uiToken != "" { return tokenAuthMiddleware(uiToken, mux) }` block — always return the bare `mux`.
- Remove the `/login` route registration from the mux (`mux.HandleFunc("/login", h.handleLogin)`).

---

## Change 2: Remove token auth functions and login page

### Where to work
- `cmd/client/ui.go` — remove the following functions and variables entirely:
  - `generateUIToken()` (line 131)
  - `resolveUIToken()` (line 140)
  - `loginPageHTML` constant (line 168)
  - `loginPageTmpl` variable (line 197)
  - `tokenAuthMiddleware()` (line 199)
  - `handleLogin()` method (line 600)

Also remove any imports that become unused after these deletions (e.g., `crypto/rand`, `crypto/subtle`, `encoding/hex`, `html/template` if only used by the login page).

---

## Change 3: Remove UI token usage from `main.go`

### Where to work
- `cmd/client/main.go` — around line 42-51

### Current behavior
```go
uiToken := resolveUIToken(uiListen, logger)
if uiToken != "" {
    logger.Info().Msg("UI token authentication enabled")
}

srv := &http.Server{
    Addr:    uiListen,
    Handler: newUIHandler(controller, logger, uiToken),
}
```

### Desired behavior
```go
srv := &http.Server{
    Addr:    uiListen,
    Handler: newUIHandler(controller, logger),
}
```

Remove the `resolveUIToken` call, the `uiToken` variable, and the "UI token authentication enabled" log line.

---

## Change 4: Remove `PLEXTUNNEL_UI_TOKEN` from config/documentation

### Where to work
- Search for any references to `PLEXTUNNEL_UI_TOKEN` in config files, env examples, docker-compose files, or documentation and remove them.

---

## Change 5: Update tests

### Where to work
- Search for any tests that reference `uiToken`, `tokenAuthMiddleware`, `handleLogin`, `resolveUIToken`, or `generateUIToken` and remove or update them.

---

## Constraints
- DO NOT delete or modify any existing code that is not explicitly mentioned in this task.
- Keep all existing tests passing.
- Make sure there are no unused imports after the changes.

## Verification
```bash
go build ./...
go test ./...
```

## When Done
Commit all changes with a descriptive commit message. Do NOT modify TASK.md.
