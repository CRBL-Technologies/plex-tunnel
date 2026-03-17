# Code Review: claude/implement-protocol-change-XFHId

**Branch:** `claude/implement-protocol-change-XFHId`
**Compared to:** `main`
**Reviewer:** Claude
**Last updated:** 2026-03-17 (round 2)

---

## Round 2 — Follow-up changes (commit `70ad3bc`)

### Changes reviewed

| File | Change |
|---|---|
| `pkg/tunnel/message.go` | Split `Validate()` into `Validate()` (lenient, for decode) and `ValidateForSend()` (strict, for encode) |
| `pkg/tunnel/frame.go` | `encodeMessagePayload` now calls `ValidateForSend()`; added clarifying comment on `maxFrameSectionLength` |
| `pkg/tunnel/tunnel_test.go` | Updated existing test to reflect lenient decode behaviour; added `TestMessageValidateForSend` |
| `specs/client-binary-protocol-upgrade.md` | Added note that WebSocket/KeyExchange types are stubs not yet functional end-to-end |

### Issues Resolved

- **Issue 1 (blocking)** — `Validate()` no longer rejects `ProtocolVersion == 0` on receive. Old servers that omit the field will decode successfully; the client then catches the mismatch at the application layer with a clear error message. ✅
- **Issue 2 (blocking)** — Same fix applies symmetrically for `MsgRegister` from old clients. ✅
- **Issue 3 (minor)** — Comment added to `frame.go:12` explaining the `uint32` cap. ✅
- **Issue 4 (observation)** — Spec now explicitly documents stub types as not yet functional. ✅

### New issues

None. The `ValidateForSend` / `Validate` split is clean and well-tested.

### Test Results (round 2)

```
ok  	github.com/antoinecorbel7/plex-tunnel/pkg/tunnel	1.032s  (race detector)
ok  	github.com/antoinecorbel7/plex-tunnel/pkg/client	6.156s  (race detector)
```

All tests pass with `-race`. No new failures.

### Verdict (round 2)

**Approved.** All blocking issues from round 1 are resolved. The design is solid, backward-compatible, and well-covered by tests. Ready to merge once the server-side is updated to set `ProtocolVersion` in `MsgRegisterAck`.

---

<!-- Round 1 preserved below -->

---

**Last updated (round 1):** 2026-03-16

---

## Summary

This branch upgrades the plex-tunnel wire protocol from JSON-over-WebSocket text frames to a custom binary framing format (protocol version 1). Each message is now wrapped in a 9-byte header (1-byte type, 4-byte metadata length, 4-byte body length) followed by JSON-encoded metadata and a raw binary body. A `ProtocolVersion` field is introduced on `MsgRegister` / `MsgRegisterAck` to allow explicit version negotiation at handshake time. New message types for WebSocket proxying (`MsgWSOpen`, `MsgWSFrame`, `MsgWSClose`) and key exchange (`MsgKeyExchange`) are also defined, though not yet implemented on the server side.

---

## What Was Done

- **`pkg/tunnel/frame.go`** (new): `Frame` struct with `MarshalBinary` / `UnmarshalFrame`; `encodeMessagePayload` and `decodeMessagePayload` helpers used by the websocket layer.
- **`pkg/tunnel/message.go`**: Added `ProtocolVersion = 1` constant; added `MsgWSOpen`, `MsgWSFrame`, `MsgWSClose`, `MsgKeyExchange` message types; added `ProtocolVersion uint16` field to `Message`; moved `Body` to binary frame (tagged `json:"-"`); added validation for new message types.
- **`pkg/tunnel/websocket.go`**: Switched `Send` / `Receive` from `wsjson.Read/Write` (text frames) to `conn.Write/Read` with binary frames using the new encode/decode helpers; removed `wsjson` import.
- **`pkg/client/client.go`**: Sends `ProtocolVersion` in `MsgRegister`; validates `ProtocolVersion` on `MsgRegisterAck` in both `runSession` and `readLoop`; improved error messages for handshake failures; handles new message types in the read loop.
- **`pkg/tunnel/frame_test.go`** (new): Round-trip tests for `MarshalBinary`/`UnmarshalFrame` and edge cases (empty body, large payload, header truncation, length mismatch).
- **`pkg/tunnel/benchmark_test.go`** (new): Benchmark for encode/decode throughput.
- **`pkg/tunnel/websocket_test.go`** (new): Integration test of `Send`/`Receive` over an in-process WebSocket pair.
- **`pkg/client/client_handshake_test.go`** (new): Unit tests for client handshake including version mismatch detection.
- **`specs/client-binary-protocol-upgrade.md`** (new): Design document for the binary protocol upgrade.
- **`review/TEMPLATE.md`** (new): Standardised review template.
- **CI (`ci-cd.yml`)**: Added `go vet` and race-detector test steps.
- **`Makefile`**: `test` target now runs with the race detector.

---

## What Looked Good

- **Clean separation of concerns**: Frame encoding/decoding is fully isolated in `frame.go`, keeping `websocket.go` and `message.go` easy to reason about.
- **Body exfiltration via `json:"-"`**: Stripping `Body` from JSON metadata and placing it in the binary frame section is the right design — it avoids base64 overhead for large payloads.
- **Defensive validation on both ends of the handshake**: Version is checked both in `runSession` (initial handshake) and in `readLoop` (re-ack), covering reconnect scenarios.
- **Human-readable error messages**: Handshake errors now suggest actionable remediation ("Update your client or server") rather than raw protocol codes.
- **Test coverage is thorough**: Frame round-trips, edge cases, websocket integration, and handshake version mismatch are all covered.
- **Race detector enabled in CI and Makefile**: Good proactive move given the concurrent nature of the tunnel.

---

## Issues

### Issue 1 — `MsgRegisterAck` validation requires `ProtocolVersion` but server not updated (blocking)

> `pkg/tunnel/message.go:51`

`Validate()` now returns an error if `MsgRegisterAck` is missing `ProtocolVersion`. If the server has not yet been updated to send the field, every client connection will fail at decode time with a validation error rather than falling through to the friendlier "unsupported protocol version" path. The client and server must be deployed atomically, or a grace-period check (`ProtocolVersion == 0` treated as "old server") should be added until the server is updated.

### Issue 2 — `MsgRegister` validation requires `ProtocolVersion` (blocking — symmetric concern)

> `pkg/tunnel/message.go:48`

Same issue from the server's perspective: a server receiving a `MsgRegister` from an old client will fail validation and return a generic decode error instead of a structured `MsgError` with "unsupported protocol version". Consider letting the server accept `ProtocolVersion == 0` as "legacy" and respond with a clear `MsgError` before closing the connection.

### Issue 3 — `maxFrameSectionLength` check is redundant on 64-bit platforms (minor)

> `pkg/tunnel/frame.go:12`

`maxFrameSectionLength` is set to `uint32` max, but `len()` on a `[]byte` returns `int` (signed 64-bit on amd64). The comparison `uint64(len(f.Metadata)) > maxFrameSectionLength` can never be true in practice because Go's allocator limits slices well below 4 GiB. The check is harmless but misleading; a comment explaining the intent (cap at `uint32` to fit the 4-byte header field) would clarify it.

### Issue 4 — New WebSocket message types defined but not validated/handled server-side (observation)

> `pkg/tunnel/message.go:23-26`

`MsgWSOpen`, `MsgWSFrame`, `MsgWSClose`, and `MsgKeyExchange` are defined and pass client-side validation, but there is no server-side handling. This is fine as a forward-looking stub, but the spec doc should note they are not yet functional to avoid confusion during code review or integration testing.

---

## Test Results

```
ok  	github.com/antoinecorbel7/plex-tunnel/pkg/tunnel	0.008s
ok  	github.com/antoinecorbel7/plex-tunnel/pkg/client	5.134s
```

All tests pass. No race detector failures.

---

## Acceptance Criteria Checklist

- [x] Binary framing replaces JSON text frames
- [x] `ProtocolVersion` negotiated at handshake
- [x] `Body` transmitted as raw bytes (not base64)
- [x] New message types stubbed for future WebSocket proxying
- [x] Frame encode/decode unit tests added
- [x] WebSocket integration test added
- [x] Client handshake version-mismatch test added
- [x] Race detector enabled in CI
- [ ] Server updated to send/validate `ProtocolVersion` (required before production deploy)

---

## Verdict

**Changes Requested.** The binary framing and version-negotiation design are solid, but the `Validate()` changes make the new client incompatible with any unpatched server (issues 1 & 2) — this must be resolved (either by a coordinated deploy plan or a backward-compatibility grace period) before merging.
