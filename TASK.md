## Context

Three GitHub issues (#77, #79, #80) need to be fixed in a single PR. These are low-risk UI/config changes.

## Goal

Fix client dashboard footer visibility, downgrade password-missing fatal to warn, and add a docs-pointer comment.

## Constraints / guardrails

- DO NOT delete or modify code not mentioned in this task.
- Keep changes minimal — no refactoring, no new features.
- Do not change any test files unless a new test is explicitly listed below.

## Tasks

- [ ] **#77 — Footer below the fold.** In `cmd/client/ui.go`, the branding footer `<div>` at line 438 is outside the `.wrap` container. The page has `min-height: 100vh` on `body` (line 186), which pushes the footer off-screen. Fix: use flexbox on `body` to make `.wrap` grow and keep the footer always visible at the bottom without scrolling. Specifically:
  1. On `body` (line 180–186), add `display: flex; flex-direction: column;`.
  2. On `.wrap` (line 187), add `flex: 1;` so it takes available space and pushes the footer to the bottom.
  3. Move the footer `<div>` (line 438) **inside** the `<div class="wrap">` container, just before the closing `</div>` of wrap (line 437). This keeps it part of the page flow.

- [ ] **#79 — Password fatal → warn.** In `cmd/client/main.go` line 55–56, change `logger.Fatal()` to `logger.Warn()`. The log message should stay the same. This way the client still warns about the missing password but does not exit.

- [ ] **#80 — Docs pointer comment.** In `cmd/client/main.go`, add a comment above the `uiListen` variable declaration (line 43) pointing users to the docs. Use this exact text:
  ```go
  // Client configuration environment variables are documented at:
  // https://docs.portless.app/getting-started
  ```

## Tests

No new tests needed — these are a CSS layout fix, a log-level change, and a comment.

## Acceptance criteria

1. The CRBL Technologies footer is visible without scrolling on a standard viewport.
2. Starting the client with `PLEXTUNNEL_UI_LISTEN=0.0.0.0:9090` and no password logs a warning but does NOT exit.
3. A docs-pointer comment exists above the UI env var block in main.go.

## Verification

```bash
cd /home/dev/worktrees/paul/plex-tunnel
go build ./...
go vet ./...
go test ./...
```
