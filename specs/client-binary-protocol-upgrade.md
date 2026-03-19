# Spec: Client-Side Binary Tunnel Protocol Upgrade

## Context

The server has been upgraded from a JSON-over-WebSocket tunnel protocol to a binary framing protocol (TODO 3.1). The new wire format eliminates the ~33% bandwidth overhead of base64-encoding HTTP bodies and reduces CPU cost for media streaming.

The tunnel client (separate repository) must be updated to match. Until it is, the client cannot connect — the server rejects clients that do not send `protocol_version: 1` in their register message, and now expects WebSocket **binary** frames instead of text frames.

This spec describes the exact changes needed in the client codebase.

---

## Goals

1. **Wire compatibility** — The client must speak the same binary framing protocol the server now expects
2. **Version negotiation** — The client must send `protocol_version` during registration and validate the server's response
3. **Zero base64** — HTTP request/response bodies must be transmitted as raw bytes, not base64-encoded JSON strings
4. **Clear upgrade errors** — If the client connects to an old server (or vice versa), the error message must tell the user what happened and what to do

---

## Wire Format

The new protocol uses WebSocket **binary** messages. Each message is a single binary frame with this layout:

```
[1 byte]  message type (uint8)
[4 bytes] metadata length (big-endian uint32)
[4 bytes] body length (big-endian uint32)
[N bytes] JSON-encoded metadata
[M bytes] raw binary body
```

### Message types

| Value | Name             | Direction        |
|-------|------------------|------------------|
| 1     | `Register`       | client → server  |
| 2     | `RegisterAck`    | server → client  |
| 3     | `HTTPRequest`    | server → client  |
| 4     | `HTTPResponse`   | client → server  |
| 5     | `Ping`           | either           |
| 6     | `Pong`           | either           |
| 7     | `Error`          | server → client  |
| 8     | `WSOpen`         | server → client  |
| 9     | `WSFrame`        | either           |
| 10    | `WSClose`        | either           |
| 11    | `KeyExchange`    | reserved         |

`WSOpen`, `WSFrame`, `WSClose`, and `KeyExchange` are protocol-reserved/stubbed message types in this phase. They are defined for forward compatibility but are not yet fully functional end-to-end.

### Metadata (JSON header)

The metadata section is a JSON object containing all message fields **except** the body. Fields use `omitempty` — only non-zero fields are present.

```json
{
  "type": 1,
  "token": "abc123",
  "subdomain": "myplex",
  "protocol_version": 1
}
```

Key point: the `body` field is **never** present in the JSON metadata. The body is carried exclusively in the raw binary section of the frame.

### Body

The body section contains raw bytes (HTTP request/response bodies, WebSocket frame payloads). It may be empty (length 0) for control messages like Ping, Pong, Register, and RegisterAck.

---

## Protocol Version Constant

```
ProtocolVersion = 1
```

This is a `uint16`. The client must define this constant and include it in every `Register` message. The server echoes it back in `RegisterAck`.

---

## Required Changes

### 1. Frame encoding and decoding

Implement a `Frame` type (or equivalent) that handles binary serialization:

**Encoding (Send path):**
1. Serialize all message fields except `Body` to JSON → this is the metadata
2. Build the 9-byte header: `[type byte][metadata length as uint32 BE][body length as uint32 BE]`
3. Concatenate: header + metadata + body
4. Send as a single WebSocket **binary** message (`MessageBinary`, not `MessageText`)

**Decoding (Receive path):**
1. Read a WebSocket message; reject if it is not a binary frame
2. Parse the 9-byte header to get the message type, metadata length, and body length
3. Validate that `9 + metadata_length + body_length == payload_length`
4. JSON-decode the metadata section into the message struct
5. Attach the body section as raw `[]byte` to the message's `Body` field
6. **Copy** the metadata and body bytes out of the read buffer (do not retain sub-slices of the WebSocket library's buffer)

### 2. Registration handshake

**Current (old) flow:**
```
Client → Server:  {"type":1, "token":"...", "subdomain":"..."}     (text frame)
Server → Client:  {"type":2, "subdomain":"..."}                    (text frame)
```

**New flow:**
```
Client → Server:  [binary frame] type=1, metadata={"type":1,"token":"...","subdomain":"...","protocol_version":1}
Server → Client:  [binary frame] type=2, metadata={"type":2,"subdomain":"...","protocol_version":1}
```

The client must:
- Include `protocol_version: 1` in the `Register` message
- Verify the `RegisterAck` contains `protocol_version: 1`
- If the server responds with `MsgError` containing `"unsupported tunnel protocol version"`, surface a clear error: `"Server requires a different protocol version. Update your client or server."`

### 3. Message struct changes

The `Body` field on the message struct must:
- Be typed as raw bytes (`[]byte`)
- Be excluded from JSON serialization (e.g., `json:"-"` tag in Go)
- Be read from / written to the binary body section of the frame, not the JSON metadata

If the client currently base64-encodes bodies into a JSON string field, that code must be removed entirely.

### 4. WebSocket message type

All writes must use the WebSocket **binary** message type. All reads must verify the incoming frame is binary and reject text frames with a clear error.

### 5. Read buffer safety

When decoding a received frame, the metadata and body byte slices must be copied into new allocations. Do not retain sub-slices of the WebSocket library's internal read buffer, as it may be reused on the next read call.

---

## Error Handling

### Version mismatch

If the server rejects the client with a version error, the client should log:

```
ERROR: Server rejected connection: unsupported tunnel protocol version.
       Client protocol version: 1
       Upgrade your server or client to a matching version.
```

### Old server (pre-binary protocol)

If the client connects to a server that has not been upgraded:
- The server will likely close the connection or return an error when it receives a binary frame where it expects a text/JSON frame
- The client should detect unexpected disconnects during the handshake and suggest: `"Connection failed during handshake. The server may be running an older protocol version. Ensure both client and server are updated."`

### Malformed frames

If a received frame has:
- Less than 9 bytes → error: `"frame too short"`
- Length fields that don't match payload size → error: `"frame length mismatch"`
- Non-binary WebSocket message type → error: `"expected binary frame"`

---

## Validation Rules

The client should validate messages before sending and after receiving, matching the server's rules:

| Message Type    | Required Fields                            |
|----------------|--------------------------------------------|
| `Register`     | `token`, `protocol_version`                |
| `RegisterAck`  | `subdomain`, `protocol_version`            |
| `HTTPRequest`  | `id`, `method`, `path`                     |
| `HTTPResponse` | `id`, `status >= 0`                        |
| `Ping`/`Pong`  | (none)                                     |
| `Error`        | `error` (non-empty string)                 |
| `WSOpen`       | `id`                                       |
| `WSFrame`      | `id`                                       |
| `WSClose`      | `id`                                       |

---

## Testing Requirements

### Unit tests

- **Frame round-trip**: encode a message with metadata + body, decode it, verify all fields match
- **Frame with empty body**: encode/decode a Ping (no body), verify body is nil/empty
- **Frame with binary body**: encode/decode a message with non-UTF-8 bytes (`0x00, 0x7f, 0x80, 0xff`), verify exact byte equality
- **Reject text frames**: verify that receiving a WebSocket text frame produces a clear error
- **Buffer isolation**: after decoding a frame from a payload, mutate the original payload bytes and verify the decoded message is unaffected
- **Length mismatch**: craft a payload where declared lengths don't match actual payload size, verify error
- **Frame too short**: send fewer than 9 bytes, verify error
- **Version validation**: verify `Register` without `protocol_version` fails validation

### Integration / end-to-end tests

- **Handshake success**: client connects to a test server, sends `Register` with `protocol_version: 1`, receives `RegisterAck` with `protocol_version: 1`
- **Version rejection**: client sends `protocol_version: 99`, server rejects with a clear error message
- **HTTP round-trip over binary frames**: proxy an HTTP request through the tunnel and verify the response body arrives intact as raw bytes (no base64 artifacts)
- **Large body**: send a >1 MiB body through the tunnel and verify no corruption

### Benchmark

- Compare binary frame encode/decode vs the old JSON+base64 approach for a 256 KiB payload to confirm the expected throughput improvement

---

## Migration Notes

- This is a **breaking change**. The client and server must be upgraded together. There is no backward-compatible fallback.
- If the client codebase shares a `tunnel` package with the server (e.g., as a Go module dependency), it may be able to import the server's `Frame`, `NewFrame`, `UnmarshalFrame`, and `decodeMessagePayload` directly rather than reimplementing them.
- The `ProtocolVersion` constant should come from a single shared source if possible to avoid drift.

---

## Out of Scope

- Backward-compatible negotiation (server explicitly chose a breaking change with version gating)
- Compression of the metadata section (JSON is already compact for typical headers)
- Zero-copy body forwarding optimizations (deferred to a future iteration)
- End-to-end encryption (`Encrypted` field and `MsgKeyExchange` are reserved but not yet implemented)

---

## Reference

- Shared proto implementation: `github.com/CRBL-Technologies/plex-tunnel-proto/tunnel/frame.go` — `Frame`, `NewFrame`, `MarshalBinary`, `UnmarshalFrame`
- Shared proto wire format: `github.com/CRBL-Technologies/plex-tunnel-proto/tunnel/websocket.go` — `Send()` and `Receive()`
- Server handshake: `pkg/server/server.go` — `handleTunnel()`
- Protocol version constant: `github.com/CRBL-Technologies/plex-tunnel-proto/tunnel/message.go` — `ProtocolVersion = 1`
- Shared proto benchmark: `github.com/CRBL-Technologies/plex-tunnel-proto/tunnel/benchmark_test.go` — `BenchmarkBinaryFrameVsLegacyJSON`
