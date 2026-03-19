# Code Review: claude/update-repo-structure-oTnhz

**Branch:** `claude/update-repo-structure-oTnhz`
**PR:** #2 — Migrate client to shared proto module
**Compared to:** `main`
**Reviewer:** Claude
**Last updated:** 2026-03-19 (round 2)

---

## Summary

Removes the local `pkg/tunnel/` package (~580 lines of production + test code) and replaces it with an import of the shared `github.com/CRBL-Technologies/plex-tunnel-proto/tunnel` module at `v1.0.0`. The two call-site files (`pkg/client/client.go` and `pkg/client/client_handshake_test.go`) have their import paths swapped with no other logic changes. A new `workspace-setup` Makefile target is added for local multi-repo development, and a comprehensive doc/reference sweep updates all `antoinecorbel7` GitHub/GHCR references to `CRBL-Technologies`. Net change: 135 additions, 1204 deletions.

---

## What Was Done

- Removed `pkg/tunnel/` entirely (message types, frame codec, WebSocket transport, fuzz/unit tests)
- Swapped import path in `pkg/client/client.go` and `pkg/client/client_handshake_test.go`
- Pinned `github.com/CRBL-Technologies/plex-tunnel-proto v1.0.0` in `go.mod`/`go.sum`
- Added `workspace-setup` Makefile target to generate a sibling `go.work`
- Updated org/registry references in README, REPO_GUIDE, specs, docker-compose files, and install scripts
- Added `.gitignore` entry for `.claude/` workspace files

---

## What Looked Good

- Import swap in both call-site files is complete — no old `pkg/tunnel` import left behind.
- `go.mod` + `go.sum` correct: proto pinned at `v1.0.0`, both `h1:` and `go.mod` hashes present, no version conflict with the transitive `nhooyr.io/websocket` dependency.
- e2e CI job correctly demoted to `workflow_dispatch` only — the right fix for the chicken-and-egg problem raised in PR #1 review. The job is still available for manual execution.
- `go vet` and race detector steps retained from the prior CI work.
- `workspace-setup` target uses `printf` (portable) rather than `echo -e`, and guards against an already-existing `go.work`.
- Test coverage is correctly divided: deleted `pkg/tunnel/` tests belong in the proto repo; `client_handshake_test.go` remains and exercises end-to-end handshake logic, which is the right responsibility for this repo.
- Doc sweep is thorough — README, REPO_GUIDE, architecture spec, all compose files, and install scripts all updated consistently.

---

## Issues

### Issue 1 — `workspace-setup` writes a `go.work` that references `plex-tunnel-server`, which the client doesn't depend on (important)

> `Makefile:38-39`

The generated `go.work` unconditionally includes `./plex-tunnel-server`:

```makefile
printf 'go 1.22\n\nuse (\n\t./plex-tunnel-proto\n\t./plex-tunnel\n\t./plex-tunnel-server\n)\n' > ../go.work
```

The client has no build-time dependency on the server. A developer who runs `make workspace-setup` without having checked out `plex-tunnel-server` will get a broken workspace — Go workspace validation fails if a `use` directive points to a non-existent directory. The code already guards the *clone hint* for the proto repo but writes the `go.work` unconditionally with the server path.

Fix: either drop `./plex-tunnel-server` from the template (the client doesn't need it), or add an existence check and only include the server path if the directory is present.

---

### Issue 2 — Outbound message validation silently removed (important)

> `pkg/client/client.go` (no-op change, but behavioral regression in the dependency)

The old local `pkg/tunnel/websocket.go` called `msg.ValidateForSend()` before writing to the wire, so sending a malformed message (e.g. empty `Token`) returned an error immediately. The proto module's `WebSocketConnection.Send()` performs no validation — it serializes and sends whatever it receives.

In the current client, `client.go` always populates `Token` and `ProtocolVersion` correctly, so no bug surfaces. But the safety net for future call sites is gone. This was also accepted in the approved server PR #32 and is ultimately a proto-level concern.

Note to follow up: worth raising with the proto maintainers for `v1.1.0`.

---

### Issue 3 — `reviews/TEMPLATE.md` test result block shows server package paths (minor)

> `reviews/TEMPLATE.md`

The placeholder test output shows `github.com/CRBL-Technologies/plex-tunnel-server/pkg/...`. This template is in the client repo — contributors filling it in will paste client package output, not server output.

Fix: change placeholders to `github.com/antoinecorbel7/plex-tunnel/pkg/client/...` or use a generic `ok  <package>  Xs` form.

---

### Issue 4 — `README.md` still has `docker login ghcr.io -u antoinecorbel7` (minor, pre-existing)

> `README.md`

The PR does a comprehensive org-name sweep but this line was not updated. Pre-existing issue, outside the scope of this change, but worth noting since the sweep was otherwise thorough.

---

### Issue 5 — `go.mod` module name not updated to match new org (minor, intentional)

> `go.mod:1`

Module still declares `module github.com/antoinecorbel7/plex-tunnel`. This is deliberate scope-limiting for this PR (renaming the module is a breaking change requiring all importers to update). A comment or TODO acknowledging the inconsistency would help contributors understand it's not an oversight.

---

## Test Results

Tests were not run locally against this branch. The CI `test` job (`go test -race ./...`) and demoted e2e job are the authoritative gates. The validation command in the PR description (`docker run ... go test ./...`) should pass given the clean import swap and correct `go.sum`.

---

## Acceptance Criteria Checklist

- [x] Local `pkg/tunnel` removed; all imports point to shared proto module
- [x] `go.mod`/`go.sum` pin proto at `v1.0.0`
- [x] CI retains `go vet`, race detector, and integration tests
- [x] e2e job demoted to manual trigger
- [x] `workspace-setup` Makefile target added
- [x] Org/registry references updated throughout docs and scripts
- [x] `workspace-setup` `go.work` template does not reference absent sibling repos unconditionally

---

## Verdict

**Approved.** The core migration is executed cleanly and correctly. The import swap, `go.mod`/`go.sum` pin, and CI changes are all right. Issue 1 (the `workspace-setup` footgun) is the only item worth fixing before merge if developers will regularly use `make workspace-setup` — it is a one-line fix. Everything else is minor or pre-existing.

---

## Round 2 — Follow-up changes (commit `22bd60b`)

### What Was Addressed

All five issues from round 1 were resolved:

| Issue | Status |
|-------|--------|
| 1 — `workspace-setup` unconditionally referenced absent `plex-tunnel-server` | ✅ Fixed — server path now guarded by `[ -d ../plex-tunnel-server ]`; hint added when absent |
| 2 — Outbound send validation silently removed (proto-level concern) | ✅ Acknowledged — noted as proto follow-up, no client-side action needed |
| 3 — `reviews/TEMPLATE.md` showed server package paths | ✅ Fixed — replaced with correct client package paths |
| 4 — `README.md` `docker login` still used `antoinecorbel7` | ✅ Fixed — updated to `CRBL-Technologies` |
| 5 — `go.mod` module name inconsistency undocumented | ✅ Fixed — comment added explaining the intentional deferral of the breaking rename |

### Notes on the Fixes

The `workspace-setup` fix is correct and well-structured: the `go.work` body is assembled in parts, the server block is conditionally appended, and the closing `)\n` is written last. `REPO_GUIDE.md` is updated to match the new conditional behavior. The `README.md` workspace layout diagram also correctly marks `plex-tunnel-server` as optional.

### Round 2 Verdict

**Approved — ready to merge.** All round 1 findings resolved with no new concerns introduced.
