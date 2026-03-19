# PlexTunnel — Full System Specification

> **Scope:** Complete specification for both the **client** and **server** repositories. This document supersedes the original `PLEXTUNNEL_SPEC.md` (which described an early monorepo design) and serves as the single source of truth for the current split-repo architecture.

---

## Table of Contents

1. [System Overview](#1-system-overview)
2. [Architecture](#2-architecture)
3. [Repository Layout](#3-repository-layout)
4. [Wire Protocol](#4-wire-protocol)
5. [Client Specification](#5-client-specification)
6. [Server Specification](#6-server-specification)
7. [Deployment](#7-deployment)
8. [Development & Testing](#8-development--testing)
9. [Security Model](#9-security-model)
10. [Future Work](#10-future-work)

---

## 1. System Overview

PlexTunnel is a managed tunneling service that gives Plex users secure remote access to their media servers without opening ports, configuring firewalls, or dealing with CGNAT restrictions.

A lightweight **client** (agent) runs alongside the user's Plex server and establishes an outbound encrypted WebSocket connection to a **server** (relay). The server exposes a public HTTPS endpoint (e.g., `username.plextunnel.io`) that routes traffic back through the tunnel to the local Plex instance.

Since the connection is outbound-only from the client, it works behind CGNAT, double NAT, firewalls, and any other network restriction.

### Design Principles

1. **Transparent proxy.** Plex clients must not know they are going through a tunnel. All HTTP headers, cookies, and future WebSocket upgrades pass through unmodified.
2. **Streaming-first.** Video is the primary payload. Response bodies are streamed in chunks — never buffered entirely in memory. A 4K movie could be 50+ GB.
3. **Seamless reconnection.** The client handles every network disruption automatically with exponential backoff and jitter.
4. **Stateless server.** The only state on the server is an in-memory routing table of connected clients. If the server restarts, clients reconnect and re-register. No data loss.
5. **Never inspect traffic.** The server is a pass-through. It does not read, cache, log, or store any media content.
6. **Config over code.** Both binaries are fully configurable via environment variables. No config files required for basic usage.

---

## 2. Architecture

### Traffic Flow

```
Remote Plex Client (phone, TV, browser)
    │
    │  HTTPS request to myserver.plextunnel.io
    ▼
┌─────────────────────────────────────────┐
│  Caddy (TLS termination, wildcard cert) │   ← VPS
│  *.plextunnel.io                        │
└──────────┬──────────────────────────────┘
           │ HTTP (plain)
           ▼
┌─────────────────────────────────────────┐
│  PlexTunnel Server (Go binary)         │   ← VPS
│  • HTTP listener (:8080) — receives     │
│    proxied requests from Caddy          │
│  • Tunnel listener (:8081) — accepts    │
│    WebSocket connections from clients   │
│  • In-memory routing: subdomain → conn  │
└──────┬───────────────────▲──────────────┘
       │ MsgHTTPRequest    │ MsgHTTPResponse
       │ (binary frame)    │ (binary frame)
       ▼                   │
   ═══════════════════════════════════════
        Encrypted WebSocket Tunnel (wss://)
   ═══════════════════════════════════════
       │                   ▲
       ▼                   │
┌─────────────────────────────────────────┐
│  PlexTunnel Client (Go binary)         │   ← User's machine
│  • Connects outbound to server          │
│  • Proxies requests to local Plex       │
│  • Auto-reconnects on failure           │
│  • Optional web UI (:9090)              │
└──────────┬──────────────────────────────┘
           │ HTTP (plain)
           ▼
┌─────────────────────────────────────────┐
│  Plex Media Server (127.0.0.1:32400)   │   ← User's machine
└─────────────────────────────────────────┘
```

### Components

| Component | Runs on | Repository | Purpose |
|-----------|---------|------------|---------|
| **Client** | User's machine (alongside Plex) | `plex-tunnel` | Outbound tunnel endpoint, HTTP proxy to Plex |
| **Server** | VPS with public IP | `plex-tunnel-server` | Inbound tunnel endpoint, subdomain routing, HTTP proxy to clients |
| **Caddy** | VPS (in front of server) | N/A (deployed separately) | TLS termination, wildcard Let's Encrypt certs |

---

## 3. Repository Layout

### Client Repository (`plex-tunnel`)

```
plex-tunnel/
├── cmd/client/
│   ├── main.go                 # Entrypoint, config loading, signal handling
│   └── ui.go                   # Web UI handler (status + settings page)
├── pkg/
│   ├── client/
│   │   ├── client.go           # Main client: connect, register, proxy loop
│   │   ├── config.go           # Environment variable configuration
│   │   ├── reconnect.go        # Exponential backoff with jitter
│   │   └── status.go           # Runtime connection status struct
│   └── tunnel/
│       ├── transport.go        # Transport/Connection/Listener interfaces
│       ├── websocket.go        # WebSocket transport implementation
│       ├── message.go          # Message types, validation, protocol version
│       └── frame.go            # Binary frame codec (encode/decode)
├── scripts/
│   ├── e2e-debug.sh            # Local E2E test orchestration
│   └── install.sh              # Binary release installer
├── configs/
│   └── client.example.env
├── testdata/
│   └── tokens.debug.json       # Dev tokens for local testing
├── specs/                      # Specifications
├── Dockerfile.client           # Multi-stage Alpine build
├── docker-compose.yml          # Production: client + Plex
├── docker-compose.debug.yml    # Dev: client + server + mock-plex
├── Makefile
├── go.mod / go.sum
└── README.md
```

### Server Repository (`plex-tunnel-server`)

```
plex-tunnel-server/
├── cmd/server/
│   └── main.go                 # Entrypoint, config loading, signal handling
├── pkg/
│   ├── server/
│   │   ├── server.go           # HTTP + tunnel listeners, request routing
│   │   ├── router.go           # Subdomain → connection routing table
│   │   └── config.go           # Environment variable configuration
│   ├── auth/
│   │   └── token.go            # Token validation (JSON file, future: database)
│   ├── admin/
│   │   └── admin.go            # Admin API (optional)
│   └── tunnel/
│       ├── transport.go        # Transport/Connection/Listener interfaces
│       ├── websocket.go        # WebSocket transport implementation
│       ├── message.go          # Message types, validation, protocol version
│       └── frame.go            # Binary frame codec (encode/decode)
├── Dockerfile.server           # Multi-stage Alpine build
├── Makefile
├── go.mod / go.sum
└── README.md
```

> **Note:** The client now imports the shared protocol module `github.com/CRBL-Technologies/plex-tunnel-proto/tunnel`. The server still needs to complete the same migration (see [Section 10](#10-future-work)).

---

## 4. Wire Protocol

### 4.1 Transport

Communication between client and server uses a single **WebSocket** connection over TLS (`wss://`). The connection is initiated by the client (outbound only).

The transport layer is designed behind a pluggable interface to allow future protocols (e.g., QUIC):

```go
type Transport interface {
    Dial(ctx context.Context, url string) (Connection, error)
    Listen(addr string) (Listener, error)
}

type Connection interface {
    Send(msg Message) error
    Receive() (Message, error)
    Close() error
    RemoteAddr() string
}

type Listener interface {
    Accept() (Connection, error)
    Close() error
}
```

### 4.2 Binary Framing (Protocol Version 1)

All messages are sent as WebSocket **binary** frames. Text frames are rejected.

Each message is a single binary frame with this layout:

```
Offset  Size     Field
──────  ──────   ─────────────────────────────
0       1 byte   Message type (uint8)
1       4 bytes  Metadata length M (big-endian uint32)
5       4 bytes  Body length B (big-endian uint32)
9       M bytes  JSON-encoded metadata
9+M     B bytes  Raw binary body
```

**Total frame size:** `9 + M + B` bytes.

**Metadata section:** JSON object containing all message fields **except** `body`. Fields use `omitempty` — only non-zero fields are present.

**Body section:** Raw bytes (HTTP request/response bodies, future WebSocket frame payloads). Empty (length 0) for control messages like Ping, Pong, Register, RegisterAck.

**Why binary framing?**
- The previous JSON+base64 approach incurred ~33% bandwidth overhead on body payloads
- Binary framing transmits HTTP bodies as raw bytes with zero encoding overhead
- Significant throughput improvement for video streaming workloads

### 4.3 Message Types

```
Value  Name            Direction         Description
─────  ──────────────  ────────────────  ─────────────────────────────────
1      Register        client → server   Client registers with token
2      RegisterAck     server → client   Server confirms registration
3      HTTPRequest     server → client   Incoming HTTP request to proxy
4      HTTPResponse    client → server   HTTP response from local Plex
5      Ping            either            Keepalive heartbeat
6      Pong            either            Keepalive response
7      Error           server → client   Protocol or authentication error
8      WSOpen          (reserved)        Future: WebSocket upgrade
9      WSFrame         (reserved)        Future: WebSocket data frame
10     WSClose         (reserved)        Future: WebSocket close
11     KeyExchange     (reserved)        Future: end-to-end encryption
```

Types 8-11 are defined for forward compatibility. They pass validation but are not yet implemented end-to-end. Clients and servers must ignore unknown/unsupported message types gracefully.

### 4.4 Message Structure

```go
type Message struct {
    Type            MessageType         `json:"type"`
    ID              string              `json:"id,omitempty"`
    Token           string              `json:"token,omitempty"`
    Subdomain       string              `json:"subdomain,omitempty"`
    ProtocolVersion uint16              `json:"protocol_version,omitempty"`
    Method          string              `json:"method,omitempty"`
    Path            string              `json:"path,omitempty"`
    Headers         map[string][]string `json:"headers,omitempty"`
    Body            []byte              `json:"-"`
    Status          int                 `json:"status,omitempty"`
    EndStream       bool                `json:"end_stream,omitempty"`
    Error           string              `json:"error,omitempty"`
}
```

Key: `Body` is tagged `json:"-"` — it is **never** included in the JSON metadata section. It is carried exclusively in the binary body section of the frame.

### 4.5 Validation Rules

Messages are validated both before sending and after receiving.

| Message Type  | Required Fields                          |
|---------------|------------------------------------------|
| Register      | `token`, `protocol_version` (send only)  |
| RegisterAck   | `subdomain`, `protocol_version` (send only) |
| HTTPRequest   | `id`, `method`, `path`                   |
| HTTPResponse  | `id`, `status >= 0`                      |
| Ping / Pong   | (none)                                   |
| Error         | `error` (non-empty string)               |
| WSOpen/Frame/Close | `id`                                |
| KeyExchange   | (none)                                   |

**Two validation levels:**

- **`Validate()`** (lenient, used on receive): Allows `ProtocolVersion == 0` for backward compatibility with peers that haven't upgraded yet. This lets the application layer handle version mismatch with a clear error message.
- **`ValidateForSend()`** (strict, used on send): Requires `ProtocolVersion` on Register and RegisterAck. Prevents sending incomplete messages.

### 4.6 Protocol Version Negotiation

```
ProtocolVersion = 1  (uint16)
```

**Handshake flow:**

```
Client                                  Server
  │                                       │
  │─── Register ────────────────────────→ │
  │    token: "abc123"                    │
  │    subdomain: "myserver"              │
  │    protocol_version: 1                │
  │                                       │
  │                     ┌─ Validate token │
  │                     ├─ Check version  │
  │                     └─ Assign subdomain
  │                                       │
  │←── RegisterAck ──────────────────────│
  │    subdomain: "myserver"              │
  │    protocol_version: 1                │
  │                                       │
  │  (tunnel is now active)               │
```

**Version mismatch handling:**

| Scenario | Behavior |
|----------|----------|
| Client sends `protocol_version: 1`, server supports it | Normal handshake, both sides echo version 1 |
| Client sends version server doesn't support | Server responds with `MsgError` containing `"unsupported tunnel protocol version"` |
| Client receives `RegisterAck` with different version | Client disconnects with clear error: `"Server requires a different protocol version"` |
| Old client (no version field) connects to new server | Server receives `protocol_version: 0`, can respond with structured error before closing |
| New client connects to old server | Client detects non-binary frame or handshake failure, suggests updating both sides |

### 4.7 HTTP Proxying Flow

```
Plex App         Server (Relay)         Client (Agent)        Local Plex
   │                 │                      │                     │
   │─ GET /library →│                      │                     │
   │                 │                      │                     │
   │                 │── MsgHTTPRequest ──→ │                     │
   │                 │   id: "req-001"      │                     │
   │                 │   method: GET        │                     │
   │                 │   path: /library     │                     │
   │                 │   headers: {...}     │ ── GET /library ──→│
   │                 │                      │                     │
   │                 │                      │ ←── 200 OK ────────│
   │                 │                      │     (streaming)     │
   │                 │                      │                     │
   │                 │←─ MsgHTTPResponse ──│                     │
   │                 │   id: "req-001"      │                     │
   │                 │   status: 200        │                     │
   │                 │   headers: {...}     │                     │
   │                 │   body: [chunk 1]    │                     │
   │                 │                      │                     │
   │                 │←─ MsgHTTPResponse ──│                     │
   │                 │   id: "req-001"      │                     │
   │                 │   body: [chunk 2]    │                     │
   │                 │                      │                     │
   │                 │←─ MsgHTTPResponse ──│                     │
   │                 │   id: "req-001"      │                     │
   │                 │   end_stream: true   │                     │
   │                 │   body: []           │                     │
   │                 │                      │                     │
   │←── 200 OK ─────│                      │                     │
   │   (streamed)    │                      │                     │
```

**Key properties:**
- Multiple HTTP requests can be in-flight simultaneously over a single tunnel (multiplexed by message `id`)
- First response message includes `status` + `headers` + optional first body chunk
- Subsequent messages carry body chunks (same `id`, no headers)
- Final message has `end_stream: true` and an empty body
- Default chunk size: 64 KB (configurable via `PLEXTUNNEL_RESPONSE_CHUNK_SIZE`)

### 4.8 Keepalive

- Client sends `MsgPing` every 30 seconds (configurable via `PLEXTUNNEL_PING_INTERVAL`)
- Server responds with `MsgPong`
- If no Pong received within 10 seconds (configurable via `PLEXTUNNEL_PONG_TIMEOUT`), client considers the connection dead and reconnects
- Server removes client from routing table if no Ping received within 60 seconds

This keeps firewall/NAT mappings alive and provides fast detection of broken tunnels.

---

## 5. Client Specification

### 5.1 Lifecycle

```
main()
  ├─ Load config from environment
  ├─ Initialize zerolog logger
  ├─ Start web UI HTTP server (optional)
  ├─ Start clientController
  │    └─ Client.Run() loop:
  │         ├─ runSession()
  │         │    ├─ Dial WebSocket to server
  │         │    ├─ Send MsgRegister (token, subdomain, protocol_version)
  │         │    ├─ Receive MsgRegisterAck (validate version)
  │         │    ├─ Launch readLoop goroutine
  │         │    ├─ Launch pingLoop goroutine
  │         │    └─ Wait for error from either loop
  │         │
  │         ├─ On error: calculate backoff, update status, wait
  │         └─ Retry session
  │
  └─ Handle SIGINT/SIGTERM → graceful shutdown (5s timeout)
```

### 5.2 Configuration

All environment variables are prefixed with `PLEXTUNNEL_`.

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `TOKEN` | Yes | — | Authentication token (UUID from server admin) |
| `SERVER_URL` | Yes | — | WebSocket endpoint (`wss://tunnel.example.com/tunnel`) |
| `PLEX_TARGET` | No | `http://127.0.0.1:32400` | Local Plex server URL |
| `SUBDOMAIN` | No | — | Requested subdomain (server may assign one) |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn`, `error` |
| `PING_INTERVAL` | No | `30s` | Keepalive ping interval |
| `PONG_TIMEOUT` | No | `10s` | Max time to wait for pong reply |
| `MAX_RECONNECT_DELAY` | No | `60s` | Cap on exponential backoff |
| `RESPONSE_CHUNK_SIZE` | No | `65536` | HTTP response body chunk size in bytes |
| `UI_LISTEN` | No | `127.0.0.1:9090` | Web UI address (empty string to disable) |

### 5.3 Request Handling

When the client receives a `MsgHTTPRequest` from the server:

1. Construct target URL: resolve `path` against `PLEXTUNNEL_PLEX_TARGET`
2. Build `http.Request` with method, path, headers from the message
3. Strip the `Host` header (the relay provides the original external host)
4. Forward to local Plex via `http.Client` (30-second timeout for connection, no global timeout for streaming)
5. Read response in chunks (`RESPONSE_CHUNK_SIZE`, default 64 KB)
6. Send first `MsgHTTPResponse` with `status`, `headers`, and first body chunk
7. Send subsequent `MsgHTTPResponse` messages with body chunks (same `id`)
8. Send final `MsgHTTPResponse` with `end_stream: true` and empty body
9. On error (Plex unreachable, bad URL): send `MsgHTTPResponse` with status 502

### 5.4 Reconnection Logic

```
on disconnect:
  attempt = 0
  while not connected:
    delay = min(2^attempt * 1s, MAX_RECONNECT_DELAY) + jitter(0..500ms)
    wait(delay)
    try connect
    if success: break
    else: attempt++
```

Example backoff sequence (default 60s cap):
| Attempt | Base Delay | With Jitter |
|---------|-----------|-------------|
| 0 | 1s | 1.0–1.5s |
| 1 | 2s | 2.0–2.5s |
| 2 | 4s | 4.0–4.5s |
| 3 | 8s | 8.0–8.5s |
| 4 | 16s | 16.0–16.5s |
| 5 | 32s | 32.0–32.5s |
| 6+ | 60s | 60.0–60.5s |

### 5.5 Web UI

Optional local HTTP server (default: `127.0.0.1:9090`).

**Endpoints:**

| Method | Path | Description |
|--------|------|-------------|
| GET | `/` | HTML status page (auto-refreshes every 5s) |
| GET | `/api/status` | JSON connection status |
| POST | `/settings` | Update runtime config (restarts client session) |

**Status fields:** connected (bool), server address, subdomain, last error, reconnect attempt count, connected/disconnected timestamps.

**Settings form:** Token, server URL, subdomain, Plex target, log level. Validates inputs (ws/wss URLs, valid log levels) before applying.

### 5.6 Concurrency Model

```
Client.Run() goroutine
  ├── readLoop goroutine
  │    ├── Receives messages from tunnel
  │    ├── For each MsgHTTPRequest: spawns handleHTTPRequest goroutine
  │    └── Sends errors to shared channel
  ├── pingLoop goroutine
  │    ├── Sends MsgPing every PING_INTERVAL
  │    └── Monitors pong timeout
  └── Waits for first error from either loop
```

**Synchronization:**
- `WebSocketConnection.writeMu sync.Mutex` — serializes WebSocket writes (not concurrent-safe)
- `Client.stateMu sync.RWMutex` — protects `ConnectionStatus` reads/writes
- `clientController.mu sync.RWMutex` — protects runner lifecycle (start/stop/restart)

### 5.7 WebSocket Parameters

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| Read timeout | 70s | Must exceed ping interval + pong timeout to allow streaming |
| Write timeout | 60s | Prevents indefinitely hanging sends |
| Max message size | 8 MB | Prevents memory exhaustion from oversized frames |

---

## 6. Server Specification

### 6.1 Lifecycle

```
main()
  ├─ Load config from environment
  ├─ Load token file
  ├─ Initialize in-memory routing table
  ├─ Start tunnel listener (:8081)
  │    └─ For each WebSocket connection:
  │         ├─ Receive MsgRegister
  │         ├─ Validate token
  │         ├─ Validate protocol version
  │         ├─ Register subdomain → connection in routing table
  │         ├─ Send MsgRegisterAck
  │         └─ Enter message forwarding loop
  │
  ├─ Start HTTP listener (:8080)
  │    └─ For each HTTP request:
  │         ├─ Extract subdomain from Host header
  │         ├─ Look up connection in routing table
  │         ├─ Generate unique request ID
  │         ├─ Send MsgHTTPRequest to client via tunnel
  │         ├─ Wait for MsgHTTPResponse(s) with matching ID
  │         └─ Stream response back to original HTTP client
  │
  └─ Handle SIGINT/SIGTERM → graceful shutdown
```

### 6.2 Configuration

All environment variables are prefixed with `PLEXTUNNEL_SERVER_`.

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `LISTEN` | Yes | `:8080` | HTTP listen address (Caddy proxies to this) |
| `TUNNEL_LISTEN` | Yes | `:8081` | WebSocket tunnel listen address |
| `DOMAIN` | Yes | — | Base domain for subdomain extraction (e.g., `plextunnel.io`) |
| `TOKENS_FILE` | Yes | — | Path to JSON token file |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn`, `error` |

### 6.3 Token File Format

```json
{
  "tokens": [
    { "token": "550e8400-e29b-41d4-a716-446655440000", "subdomain": "myserver" },
    { "token": "6ba7b810-9dad-11d1-80b4-00c04fd430c8", "subdomain": "friendserver" }
  ]
}
```

Tokens are UUIDs. Each token maps to exactly one subdomain. The server loads this file at startup. Future phases replace this with a database + API.

### 6.4 Subdomain Routing

```
Incoming request: Host: myserver.plextunnel.io
  → Strip base domain → extract "myserver"
  → Look up "myserver" in routing table (map[string]*WebSocketConnection)
  → If found: forward request through tunnel
  → If not found: return HTTP 502 "Tunnel not connected"
```

**Routing table operations:**
- **Register:** Add `subdomain → connection` mapping on successful handshake
- **Unregister:** Remove mapping when connection closes or client disconnects
- **Lookup:** O(1) map lookup on every incoming HTTP request
- **Conflict:** If a subdomain is already registered, reject the new connection (or replace — TBD per policy)

### 6.5 Request Proxying (Server Side)

When the server receives an HTTP request from a Plex app (via Caddy):

1. Extract subdomain from `Host` header
2. Look up client connection in routing table
3. Generate a unique request ID (UUID)
4. Build `MsgHTTPRequest` with `id`, `method`, `path`, `headers`, `body`
5. Send to client via WebSocket tunnel
6. Create a response channel keyed by request ID
7. Wait for `MsgHTTPResponse` messages with matching `id`
8. First response: write status + headers to HTTP response writer
9. Subsequent responses: stream body chunks to HTTP response writer
10. Final response (`end_stream: true`): close the HTTP response
11. On timeout or client disconnect: return 502

### 6.6 Client Lifecycle Management

**Stale client detection:**
- Server tracks last ping time per client
- If no ping received within 60 seconds, consider client disconnected
- Remove from routing table
- Close WebSocket connection

**Graceful disconnect:**
- On WebSocket close frame: remove from routing table immediately
- Pending requests for that client: return 502 to HTTP callers

### 6.7 Concurrency Model

```
Server
  ├── HTTP listener goroutine
  │    └── Per-request goroutine (standard net/http)
  │         ├── Route to client connection
  │         ├── Send MsgHTTPRequest
  │         └── Wait on response channel
  │
  ├── Tunnel listener goroutine
  │    └── Per-client goroutine
  │         ├── Handshake (register + ack)
  │         └── Message forwarding loop
  │              ├── MsgHTTPResponse → route to response channel
  │              ├── MsgPing → send MsgPong
  │              └── MsgError → log
  │
  └── Cleanup goroutine
       └── Periodic stale client check
```

**Synchronization:**
- Routing table: `sync.RWMutex` for concurrent read (HTTP lookups) / exclusive write (register/unregister)
- Response channels: `map[string]chan Message` protected by mutex, keyed by request ID
- WebSocket write mutex: same as client side

---

## 7. Deployment

### 7.1 Client Deployment

#### Docker Compose (recommended)

```yaml
services:
  plex:
    image: plexinc/pms-docker:latest
    network_mode: host

  plextunnel-client:
    image: ghcr.io/antoinecorbel7/plex-tunnel:latest
    network_mode: host
    restart: unless-stopped
    environment:
      PLEXTUNNEL_TOKEN: ${PLEXTUNNEL_TOKEN}
      PLEXTUNNEL_SERVER_URL: ${PLEXTUNNEL_SERVER_URL}
      PLEXTUNNEL_PLEX_TARGET: http://127.0.0.1:32400
      PLEXTUNNEL_SUBDOMAIN: ${PLEXTUNNEL_SUBDOMAIN:-}
      PLEXTUNNEL_UI_LISTEN: 127.0.0.1:9090
```

#### Docker Networking Scenarios

| Scenario | `PLEXTUNNEL_PLEX_TARGET` | Notes |
|----------|--------------------------|-------|
| Compose (same network) | `http://plex:32400` | Docker DNS resolves container name |
| Host network | `http://127.0.0.1:32400` | Both on host, default works |
| User-defined network | `http://plex:32400` | Explicit docker network |

#### Standalone Binary

```bash
GOOS=linux GOARCH=amd64 go build -o bin/plextunnel-client ./cmd/client
```

Cross-compile targets: `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`.

#### Client Docker Image

- Registry: `ghcr.io/antoinecorbel7/plex-tunnel`
- Tags: `latest`, `sha-<commit>`
- Base: `alpine:3.19` with `ca-certificates`
- Binary: `/usr/local/bin/plextunnel-client`
- Build: `CGO_ENABLED=0` for static linking

### 7.2 Server Deployment

#### Server on VPS

The server runs on a VPS with a public IP, behind Caddy for TLS termination.

#### Caddy Configuration

```caddyfile
*.plextunnel.io {
    tls {
        dns cloudflare {env.CLOUDFLARE_API_TOKEN}
    }

    # Tunnel endpoint — clients connect here
    @tunnel path /tunnel
    handle @tunnel {
        reverse_proxy localhost:8081
    }

    # Everything else — proxy to server HTTP handler
    handle {
        reverse_proxy localhost:8080
    }
}
```

Requires:
- Cloudflare DNS plugin for ACME DNS challenge (needed for wildcard certs)
- Wildcard DNS record: `*.plextunnel.io → VPS IP`

#### Server Docker Image

- Registry: `ghcr.io/antoinecorbel7/plex-tunnel-server`
- Tags: `latest`, `sha-<commit>`
- Base: `alpine:3.19` with `ca-certificates`
- Binary: `/usr/local/bin/plextunnel-server`

### 7.3 CI/CD

#### Client CI (GitHub Actions)

```
Push/PR to main:
  1. Test       → go test ./... (with race detector)
  2. E2E        → docker-compose debug stack (non-blocking, see notes)
  3. Docker     → Build + push to GHCR (main only)
  4. Deploy     → Trigger Portainer webhook (main only, optional)
```

#### Server CI (GitHub Actions)

```
Push/PR to main:
  1. Test       → go test ./... (with race detector)
  2. Docker     → Build + push to GHCR (main only)
  3. Deploy     → Trigger Portainer webhook (main only, optional)
```

#### E2E Testing Note

The client repo's e2e job runs a full docker-compose stack (mock-plex + server + client). This requires a compatible server image. During protocol upgrades, e2e will fail until both sides are updated. This job should be non-blocking (informational) and eventually replaced by per-repo integration tests against a shared protocol module.

---

## 8. Development & Testing

### 8.1 Local Debug Stack

The client repo includes `docker-compose.debug.yml` for local end-to-end testing:

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│  mock-plex   │     │   server    │     │   client    │
│  (http-echo) │←────│  (:8080)    │←────│  (built     │
│  :32400      │     │  (:8081)    │     │   locally)  │
│  "mock-plex- │     │             │     │             │
│   ok"        │     │             │     │             │
└─────────────┘     └──────┬──────┘     └─────────────┘
                           │
                    localhost:18080
```

**Run:**
```bash
make debug-test     # Full automated test
make debug-up       # Start stack
make debug-logs     # Tail logs
make debug-down     # Tear down
```

**Server image sources (in priority order):**
1. `PLEXTUNNEL_SERVER_IMAGE` env var (pre-built image from GHCR)
2. Auto-clone from `github.com/antoinecorbel7/plex-tunnel-server` and build locally
3. Existing local checkout at `PLEXTUNNEL_SERVER_CONTEXT`

### 8.2 Unit Test Coverage

#### Client Tests

| Test File | Coverage |
|-----------|----------|
| `client_handshake_test.go` | Protocol version negotiation, version mismatch, old server detection |
| `reconnect_test.go` | Exponential backoff, jitter, max delay cap |
| `config_test.go` | Environment variable parsing |

#### Tunnel/Protocol Tests

| Test File | Coverage |
|-----------|----------|
| `frame_test.go` | Frame round-trip, empty body, binary payload, buffer isolation, length mismatch, truncation |
| `tunnel_test.go` | Message validation (all types), `Validate` vs `ValidateForSend`, header cloning |
| `websocket_test.go` | Binary frame send/receive over in-process WebSocket, text frame rejection |
| `benchmark_test.go` | Binary frame vs legacy JSON+base64 encode/decode throughput (256 KB payload) |

### 8.3 Running Tests

```bash
# Client repo
make test                    # go test -race ./...
go test ./pkg/client/...     # Client logic only

# Shared proto repo
go test ./...                # Contract tests and transport tests
go test -bench=. ./...       # Protocol benchmarks

# Server repo
make test                    # go test -race ./...
```

---

## 9. Security Model

### 9.1 Current (MVP)

| Layer | Mechanism |
|-------|-----------|
| **Authentication** | Pre-shared token (UUID) per client, validated at registration |
| **Transport encryption** | TLS via Caddy (HTTPS for HTTP traffic, WSS for tunnel) |
| **Authorization** | Token maps to subdomain; one token = one tunnel |
| **Traffic inspection** | None — server is a pass-through, never inspects media content |

### 9.2 Token Security

- Tokens are random UUIDs stored in a JSON file on the server
- Tokens are transmitted once during registration (over encrypted WebSocket)
- Invalid tokens receive `MsgError` and connection is closed
- No brute-force protection in MVP (rate limiting planned for Phase 2)

### 9.3 Network Security

- Client initiates all connections (outbound only) — no inbound ports needed
- Works behind CGNAT, double NAT, firewalls, VPNs
- WebSocket keepalives prevent NAT mapping expiry
- Server tunnel endpoint should not be publicly documented (security through obscurity is not relied upon, but reduces attack surface)

### 9.4 Future Security (Reserved)

- **End-to-end encryption:** `MsgKeyExchange` type is reserved for future Diffie-Hellman key exchange, allowing media traffic to be encrypted end-to-end (server cannot inspect)
- **Rate limiting:** Per-token connection rate limits and bandwidth caps (Phase 2)
- **Token rotation:** API for generating and rotating tokens (Phase 2)

---

## 10. Future Work

### 10.1 Shared Protocol Module Rollout

**Priority: High.**

The client has already migrated to `github.com/CRBL-Technologies/plex-tunnel-proto`, but the server still carries its local `pkg/tunnel/` copy. That partial rollout still leaves protocol coordination risk during upgrades. The remaining work is:

1. Migrate `plex-tunnel-server` to import `github.com/CRBL-Technologies/plex-tunnel-proto`
2. Delete the server-local `pkg/tunnel/` copy after parity is confirmed
3. Keep protocol changes versioned in the shared module only
4. Run protocol tests and benchmarks in the proto repo, with client/server testing only their integration points

### 10.2 QUIC Transport

The `Transport` interface is designed to be pluggable. A future `QUICTransport` would:
- Eliminate WebSocket framing overhead
- Provide native multiplexing (no head-of-line blocking)
- Reduce latency for concurrent streams
- Use `quic-go` library

### 10.3 WebSocket Proxying

Message types `WSOpen`, `WSFrame`, `WSClose` are reserved for proxying native WebSocket connections (Plex uses WebSockets for real-time updates). Currently, only HTTP request/response proxying is implemented.

### 10.4 End-to-End Encryption

`MsgKeyExchange` is reserved for a future DH key exchange that would allow client↔app encryption through the server, ensuring the server cannot inspect media traffic even in theory.

### 10.5 Dashboard & Control Plane

Phase 2 plans include:
- Web dashboard (Next.js): signup, login, API token generation, connection status, setup wizard
- Control plane API: user CRUD, token management, agent status
- PostgreSQL database (Supabase): users, tokens, connection logs, usage stats
- Multi-region relay support with automatic latency-based selection

### 10.6 Monetization

Phase 4 plans:
- Free tier: 10 Mbps, 1 stream
- Basic ($4.99/mo): higher bandwidth, multiple streams
- Pro ($9.99/mo): custom subdomains, custom domains, priority support
- Jellyfin / Emby support alongside Plex
