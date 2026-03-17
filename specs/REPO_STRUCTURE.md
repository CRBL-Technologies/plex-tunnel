# PlexTunnel — Repository Structure & Development Environment Specification

> **Purpose:** Define the target repository structure, shared protocol module, development environment, CI/CD strategy, and migration plan for the PlexTunnel project.

---

## Table of Contents

1. [Problem Statement](#1-problem-statement)
2. [Target Repository Structure](#2-target-repository-structure)
3. [Shared Protocol Module](#3-shared-protocol-module)
4. [Local Development Environment](#4-local-development-environment)
5. [Staging Environment](#5-staging-environment)
6. [CI/CD Strategy](#6-cicd-strategy)
7. [Migration Plan](#7-migration-plan)

---

## 1. Problem Statement

### Current state

Two repositories with duplicated protocol code and a fragile cross-repo CI dependency:

```
plex-tunnel (client)          plex-tunnel-server
├── pkg/tunnel/               ├── pkg/tunnel/         ← duplicated
│   ├── message.go            │   ├── message.go
│   ├── frame.go              │   ├── frame.go
│   ├── websocket.go          │   ├── websocket.go
│   └── transport.go          │   └── transport.go
├── pkg/client/               ├── pkg/server/
└── .github/workflows/        └── .github/workflows/
     └── e2e pulls server          └── (independent)
         image from GHCR
```

### Problems

1. **Protocol drift.** Two independent copies of `pkg/tunnel/` must be kept in sync manually. A change in one repo without the other creates incompatible builds.
2. **Chicken-and-egg CI.** The client's e2e job pulls `server:latest` from GHCR. Protocol changes break e2e until both repos are updated, but CI gates merges on e2e passing — deadlock.
3. **No contract testing.** Nothing validates that the client and server agree on the wire format without running a full docker-compose stack.
4. **No staging environment.** Changes go directly from local dev to production. No pre-production validation step.
5. **Slow feedback loop.** The e2e job clones a repo, builds Docker images, spins up compose, and polls for 45 seconds. This is slow and flaky for a merge gate.

### Target state

Three repositories with a shared protocol module, contract tests, local dev environment with Go workspace, and a staging environment for pre-production validation.

---

## 2. Target Repository Structure

### Three repositories

```
github.com/antoinecorbel7/
├── plex-tunnel-proto      ← NEW: shared protocol module
├── plex-tunnel            ← client (exists, refactored)
└── plex-tunnel-server     ← server (exists, refactored)
```

### Why three repos (not monorepo)

| Factor | Three repos | Monorepo |
|--------|-------------|----------|
| **Independent release cadence** | Client and server version independently. A server hotfix doesn't rebuild the client image. | Every push rebuilds and redeploys both. |
| **Access control** | Server repo can be private (infrastructure secrets in CI) while client and proto are public. | Single visibility setting for everything. |
| **CI isolation** | Client CI runs client tests only. Fast. | CI must determine what changed and conditionally run subsets, or run everything. |
| **Dependency clarity** | `go.mod` explicitly declares protocol version. `go mod tidy` catches drift. | Implicit coupling through shared directory. Easy to break accidentally. |
| **Scalability** | Adding a dashboard, CLI tool, or mobile SDK = new repo importing proto. Clean. | Monorepo grows unbounded. Every new component shares CI, permissions, and release lifecycle. |

The shared protocol module (`plex-tunnel-proto`) is the keystone. It is small, stable, and versioned with semantic versioning. Both client and server depend on it explicitly.

### Repository contents (target)

#### `plex-tunnel-proto`

```
plex-tunnel-proto/
├── go.mod                  # module github.com/antoinecorbel7/plex-tunnel-proto
├── message.go              # MessageType, Message, ProtocolVersion, Validate(), ValidateForSend()
├── message_test.go
├── frame.go                # Frame, NewFrame, MarshalBinary, UnmarshalFrame, encode/decode helpers
├── frame_test.go
├── transport.go            # Transport, Connection, Listener interfaces
├── websocket.go            # WebSocketTransport, WebSocketConnection, DialWebSocket, AcceptWebSocket
├── websocket_test.go
├── benchmark_test.go
├── CHANGELOG.md            # Semantic versioning changelog
└── README.md
```

- **Module path:** `github.com/antoinecorbel7/plex-tunnel-proto`
- **Versioning:** Semantic versioning (`v1.0.0`, `v1.1.0`, `v2.0.0`). Breaking protocol changes = major version bump.
- **Dependencies:** Only `nhooyr.io/websocket` and stdlib. No application-level dependencies.
- **Release process:** Tag a version → both client and server update their `go.mod` to the new version.

#### `plex-tunnel` (client)

```
plex-tunnel/
├── go.mod                  # requires github.com/antoinecorbel7/plex-tunnel-proto v1.x.x
├── cmd/client/
│   ├── main.go
│   └── ui.go
├── pkg/client/
│   ├── client.go           # imports proto for tunnel.Message, tunnel.DialWebSocket, etc.
│   ├── config.go
│   ├── reconnect.go
│   └── status.go
├── pkg/tunnel/             ← REMOVED (imported from proto)
├── scripts/
├── testdata/
├── configs/
├── specs/
├── Dockerfile.client
├── docker-compose.yml
├── docker-compose.debug.yml
├── Makefile
└── README.md
```

- `pkg/tunnel/` is deleted. All tunnel types are imported from proto.
- `go.mod` declares an explicit dependency on a pinned proto version.

#### `plex-tunnel-server` (server)

```
plex-tunnel-server/
├── go.mod                  # requires github.com/antoinecorbel7/plex-tunnel-proto v1.x.x
├── cmd/server/
│   └── main.go
├── pkg/server/
│   ├── server.go           # imports proto for tunnel.Message, tunnel.AcceptWebSocket, etc.
│   ├── router.go
│   └── config.go
├── pkg/auth/
│   └── token.go
├── pkg/admin/
│   └── admin.go
├── pkg/tunnel/             ← REMOVED (imported from proto)
├── Dockerfile.server
├── Makefile
└── README.md
```

---

## 3. Shared Protocol Module

### What goes in proto

Everything that **both** client and server need to agree on:

| File | Contents |
|------|----------|
| `message.go` | `MessageType` constants, `Message` struct, `ProtocolVersion`, `Validate()`, `ValidateForSend()`, `CloneHeaders()` |
| `frame.go` | `Frame` struct, `NewFrame()`, `MarshalBinary()`, `UnmarshalFrame()`, `encodeMessagePayload()`, `decodeMessagePayload()` |
| `transport.go` | `Transport`, `Connection`, `Listener` interfaces |
| `websocket.go` | `WebSocketTransport`, `WebSocketConnection`, `DialWebSocket()`, `AcceptWebSocket()`, timeouts, read limits |

### What stays in client/server repos

| Repo | Keeps |
|------|-------|
| Client | `pkg/client/` (connection lifecycle, reconnect, config, status, request handling, web UI) |
| Server | `pkg/server/` (HTTP listener, routing table, request forwarding), `pkg/auth/` (token validation), `pkg/admin/` |

### Versioning policy

| Change type | Version bump | Example |
|-------------|-------------|---------|
| New optional message field | Patch (`v1.0.1`) | Add `Compressed bool` to Message |
| New message type (backward compatible) | Minor (`v1.1.0`) | Add `MsgWSOpen` implementation |
| Wire format change | Major (`v2.0.0`) | Change frame header layout |
| Bug fix in validation | Patch (`v1.0.1`) | Fix edge case in `Validate()` |

**Rule:** A major version bump in proto means client and server must be updated together. Minor and patch versions are backward-compatible — either side can upgrade independently.

### API surface

The proto module exports only what both sides need:

```go
// Types
type MessageType uint8
type Message struct { ... }
type Frame struct { ... }

// Constants
const ProtocolVersion uint16 = 1
const MsgRegister MessageType = 1
// ... all message type constants

// Frame codec
func NewFrame(msg Message) (Frame, error)
func (f Frame) MarshalBinary() ([]byte, error)
func UnmarshalFrame(payload []byte) (Frame, error)

// Transport interfaces
type Transport interface { ... }
type Connection interface { ... }
type Listener interface { ... }

// WebSocket implementation
type WebSocketTransport struct { ... }
type WebSocketConnection struct { ... }
func DialWebSocket(ctx context.Context, url string, opts ...DialOption) (*WebSocketConnection, error)
func AcceptWebSocket(w http.ResponseWriter, r *http.Request, opts ...AcceptOption) (*WebSocketConnection, error)
```

Internal helpers (`encodeMessagePayload`, `decodeMessagePayload`) stay unexported — they are used by `WebSocketConnection.Send()` and `Receive()` internally.

---

## 4. Local Development Environment

### Go workspace

A Go workspace (`go.work`) lets you develop all three repos locally with local source references, so changes to proto are immediately visible in client and server without publishing a version.

#### Directory layout

```
~/dev/plextunnel/               ← workspace root
├── go.work                     # Go workspace file
├── plex-tunnel-proto/          # git clone
├── plex-tunnel/                # git clone
└── plex-tunnel-server/         # git clone
```

#### `go.work`

```go
go 1.22

use (
    ./plex-tunnel-proto
    ./plex-tunnel
    ./plex-tunnel-server
)
```

**How it works:**
- `go build`, `go test`, etc. in any of the three directories will use the local source for all three modules.
- Changes to `plex-tunnel-proto/message.go` are immediately reflected when running `go test ./...` in the client or server directory.
- `go.work` is **not** checked into any repo. It lives in the workspace root directory only. Each developer creates their own.
- CI does **not** use `go.work`. CI uses the published proto version from `go.mod`.

#### Setup script

Each repo's Makefile should include a `workspace-setup` target:

```makefile
# In plex-tunnel/Makefile and plex-tunnel-server/Makefile
workspace-setup:
	@if [ ! -f ../go.work ]; then \
		echo 'go 1.22\n\nuse (\n\t./plex-tunnel-proto\n\t./plex-tunnel\n\t./plex-tunnel-server\n)' > ../go.work; \
		echo "Created ../go.work"; \
	fi
	@if [ ! -d ../plex-tunnel-proto ]; then \
		git clone git@github.com:antoinecorbel7/plex-tunnel-proto.git ../plex-tunnel-proto; \
	fi
```

### Local debug stack

The existing `docker-compose.debug.yml` in the client repo continues to work for full end-to-end testing. With the Go workspace, the typical workflow is:

1. **Protocol change:** Edit proto → run `go test ./...` in proto dir → run `go test ./...` in client and server dirs (workspace resolves locally)
2. **Client change:** Edit client → run `go test ./...` in client dir
3. **Full integration:** Run `make debug-up` in client dir (builds client Docker image, pulls or builds server image)

For faster iteration without Docker:

```bash
# Terminal 1: run server locally
cd plex-tunnel-server && go run ./cmd/server

# Terminal 2: run client locally
cd plex-tunnel && go run ./cmd/client

# Terminal 3: mock Plex (or use real Plex on :32400)
docker run --rm -p 32400:32400 hashicorp/http-echo:1.0.0 -listen=:32400 -text=mock-plex-ok
```

---

## 5. Staging Environment

### Purpose

A persistent environment that mirrors production for pre-release validation. Used to:

- Validate protocol changes end-to-end before production deploy
- Test client upgrades against the staging server (and vice versa)
- Run longer-duration tests (sustained streaming, reconnect under load)
- Provide a safe target for external testers or demo purposes

### Requirements

| Requirement | Details |
|-------------|---------|
| **VPS** | A single small VPS (separate from production) or a separate process/port on the production VPS |
| **Domain** | Staging subdomain, e.g., `*.staging.plextunnel.io` or `*.stg.plextunnel.io` |
| **DNS** | Wildcard A record pointing to the staging VPS IP |
| **TLS** | Caddy with wildcard cert for the staging domain (same setup as production) |
| **Isolation** | Separate token file, separate routing table, separate Docker containers. No shared state with production. |
| **Persistence** | Staging can be ephemeral. Data loss on restart is acceptable. |
| **Access** | Can be public (for testing from remote networks) or restricted by IP/VPN |

### Staging stack

```yaml
# docker-compose.staging.yml (on staging VPS)
services:
  caddy:
    image: caddy:2-alpine
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile.staging:/etc/caddy/Caddyfile
      - caddy_data:/data
    environment:
      CLOUDFLARE_API_TOKEN: ${CLOUDFLARE_API_TOKEN}

  server:
    image: ghcr.io/antoinecorbel7/plex-tunnel-server:staging
    environment:
      PLEXTUNNEL_SERVER_LISTEN: :8080
      PLEXTUNNEL_SERVER_TUNNEL_LISTEN: :8081
      PLEXTUNNEL_SERVER_DOMAIN: staging.plextunnel.io
      PLEXTUNNEL_SERVER_TOKENS_FILE: /etc/plextunnel/tokens.json
      PLEXTUNNEL_SERVER_LOG_LEVEL: debug
    volumes:
      - ./tokens.staging.json:/etc/plextunnel/tokens.json:ro

volumes:
  caddy_data:
```

### Deployment workflow

```
Proto change:
  1. Tag proto release (e.g., v1.2.0)
  2. Update server go.mod → v1.2.0, push to server staging branch
  3. Server CI builds and pushes server:staging image
  4. Staging VPS pulls new server image (via webhook or manual)
  5. Update client go.mod → v1.2.0, push to client staging branch
  6. Test client against staging server
  7. Once validated: merge both to main, deploy to production

Client-only change:
  1. Push to client staging branch
  2. Run client locally against staging server
  3. Once validated: merge to main

Server-only change:
  1. Push to server staging branch
  2. Server CI builds server:staging image
  3. Staging VPS pulls new image
  4. Test existing client against staging server
  5. Once validated: merge to main
```

---

## 6. CI/CD Strategy

### 6.1 Proto CI (`plex-tunnel-proto`)

```yaml
# .github/workflows/ci.yml
on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go vet ./...
      - run: go test -race ./...
      - run: go test -bench=. ./...
```

No Docker build. No deployment. Proto is a library — it's consumed via `go get`, not as a binary or image.

**Release process:** Tag a version on main → `go get github.com/antoinecorbel7/plex-tunnel-proto@v1.x.x` becomes available.

### 6.2 Client CI (`plex-tunnel`)

```yaml
on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go vet ./...
      - run: go test -race ./...

  contract-test:
    runs-on: ubuntu-latest
    needs: test
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go test -race -tags=contract ./pkg/client/...

  docker:
    runs-on: ubuntu-latest
    needs: [test, contract-test]
    if: github.event_name == 'push'
    steps:
      # Build and push to GHCR (same as today)

  deploy:
    runs-on: ubuntu-latest
    needs: docker
    if: github.event_name == 'push'
    steps:
      # Portainer webhook (same as today)
```

**Key change:** The `e2e-connectivity` job is replaced by `contract-test` (see 6.4 below). No Docker compose, no server image pull, no 45-second polling loop.

### 6.3 Server CI (`plex-tunnel-server`)

```yaml
on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go vet ./...
      - run: go test -race ./...

  contract-test:
    runs-on: ubuntu-latest
    needs: test
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go test -race -tags=contract ./pkg/server/...

  docker:
    runs-on: ubuntu-latest
    needs: [test, contract-test]
    if: github.event_name == 'push'
    steps:
      # Build and push to GHCR
```

### 6.4 Contract Tests

Contract tests validate that each side correctly implements the shared protocol **without** running the other side. They use the proto module directly.

#### How they work

Both client and server include a `contract_test.go` file (build-tagged `//go:build contract`) that:

1. Starts an in-process WebSocket server using the proto module's `AcceptWebSocket`
2. Connects using `DialWebSocket`
3. Runs the actual handshake and message exchange logic from the application code
4. Validates that messages are correctly formed, versioned, and decodable

#### Client contract tests (`pkg/client/contract_test.go`)

```go
//go:build contract

package client_test

// Test: client sends Register with correct protocol_version
// - Start mock WebSocket server (using proto.AcceptWebSocket)
// - Mock server receives Register, validates fields
// - Mock server sends RegisterAck with matching protocol_version
// - Verify client enters connected state

// Test: client handles version mismatch
// - Mock server sends RegisterAck with protocol_version: 99
// - Verify client disconnects with clear error

// Test: client sends chunked HTTPResponse
// - Mock server sends MsgHTTPRequest
// - Client proxies to local HTTP server (httptest)
// - Mock server receives MsgHTTPResponse stream
// - Validate: first message has status + headers, subsequent have body, last has end_stream

// Test: client handles unknown message types gracefully
// - Mock server sends message with type 99
// - Verify client logs and ignores (doesn't crash)

// Test: round-trip binary frame integrity
// - Send a message with non-UTF8 body bytes through the full client encode path
// - Receive on mock server via proto.Receive()
// - Verify byte-for-byte equality
```

#### Server contract tests (`pkg/server/contract_test.go`)

```go
//go:build contract

package server_test

// Test: server validates Register and sends RegisterAck
// - Connect mock client (using proto.DialWebSocket)
// - Send Register with valid token and protocol_version
// - Receive RegisterAck, validate protocol_version matches

// Test: server rejects invalid token
// - Send Register with bad token
// - Receive MsgError

// Test: server rejects unsupported protocol version
// - Send Register with protocol_version: 0
// - Receive MsgError with "unsupported" message

// Test: server routes HTTP request to correct client
// - Register mock client with subdomain "test"
// - Send HTTP request with Host: test.<domain>
// - Mock client receives MsgHTTPRequest, validates fields
// - Mock client sends MsgHTTPResponse
// - Validate HTTP response received by caller

// Test: server handles client disconnect gracefully
// - Register mock client
// - Close client WebSocket
// - Send HTTP request to that subdomain
// - Verify 502 response
```

#### Why contract tests > e2e in CI

| Aspect | Contract tests | Full e2e (docker-compose) |
|--------|---------------|--------------------------|
| **Speed** | ~1 second (in-process) | ~60 seconds (Docker build + compose + polling) |
| **Dependencies** | None (proto module only) | Docker, GHCR access, server image, network |
| **Flakiness** | Deterministic | Registry rate limits, Docker daemon, SSH keys |
| **Protocol coverage** | Tests actual encode/decode paths | Tests the same thing plus Docker networking |
| **Cross-repo coupling** | None (uses proto version from go.mod) | Pulls server:latest (moving target) |
| **Failure diagnosis** | Go test output with line numbers | Docker logs from three containers |

---

## 7. Migration Plan

### Step 1: Create `plex-tunnel-proto` repository

**Effort:** Small. **Risk:** Low. **Dependency:** None.

1. Create `github.com/antoinecorbel7/plex-tunnel-proto` repository
2. Initialize `go.mod` with module path `github.com/antoinecorbel7/plex-tunnel-proto`
3. Copy from current client `pkg/tunnel/`:
   - `message.go` → `message.go`
   - `message_test.go` → `message_test.go` (tunnel_test.go validation tests)
   - `frame.go` → `frame.go`
   - `frame_test.go` → `frame_test.go`
   - `transport.go` → `transport.go`
   - `websocket.go` → `websocket.go`
   - `websocket_test.go` → `websocket_test.go`
   - `benchmark_test.go` → `benchmark_test.go`
4. Update package declaration from `package tunnel` to `package proto` (or keep as `tunnel` — choose one name)
5. Add CI workflow (test + vet)
6. Tag `v1.0.0`
7. Verify: `go get github.com/antoinecorbel7/plex-tunnel-proto@v1.0.0` works

**Decision: package name.** Recommend keeping `package tunnel` so import paths read naturally:

```go
import "github.com/antoinecorbel7/plex-tunnel-proto/tunnel"
// Usage: tunnel.Message, tunnel.DialWebSocket, etc.
```

Or use a flat module with `package proto`:

```go
import proto "github.com/antoinecorbel7/plex-tunnel-proto"
// Usage: proto.Message, proto.DialWebSocket, etc.
```

Either works. The flat module is simpler (no subdirectory). Keep `package tunnel` in a `tunnel/` subdirectory if you want the import to read `tunnel.Message`.

### Step 2: Migrate client to use proto

**Effort:** Medium. **Risk:** Medium (must not break existing behavior). **Dependency:** Step 1.

1. In `plex-tunnel/go.mod`: `go get github.com/antoinecorbel7/plex-tunnel-proto@v1.0.0`
2. Update all imports in `pkg/client/` from `github.com/antoinecorbel7/plex-tunnel/pkg/tunnel` to the proto module path
3. Delete `pkg/tunnel/` entirely from the client repo
4. Run `go test ./...` — fix any compilation errors (likely just import paths)
5. Run `go test -race ./...` — ensure all tests pass
6. Move tunnel-level tests (frame_test.go, etc.) that are now in proto to be excluded from client repo
7. Commit and push

### Step 3: Migrate server to use proto

**Effort:** Medium. **Risk:** Medium. **Dependency:** Step 1.

Same process as step 2, but for `plex-tunnel-server`. Update imports, delete `pkg/tunnel/`, verify tests pass.

### Step 4: Add contract tests to client

**Effort:** Medium. **Risk:** Low. **Dependency:** Step 2.

1. Create `pkg/client/contract_test.go` with `//go:build contract` tag
2. Implement contract tests as described in section 6.4
3. Add `contract-test` job to client CI
4. Verify contract tests pass in CI

### Step 5: Add contract tests to server

**Effort:** Medium. **Risk:** Low. **Dependency:** Step 3.

Same as step 4, but for server repo.

### Step 6: Remove e2e CI job from client

**Effort:** Small. **Risk:** Low. **Dependency:** Step 4 (contract tests must be in place first).

1. Remove the `e2e-connectivity` job from `.github/workflows/ci-cd.yml`
2. Update `docker` job's `needs` to remove `e2e-connectivity` dependency
3. Keep `docker-compose.debug.yml` and `scripts/e2e-debug.sh` for local use — just remove them from CI
4. Update README to reflect that e2e is local-only

### Step 7: Set up Go workspace for local dev

**Effort:** Small. **Risk:** None.

1. Add `workspace-setup` Makefile target to both client and server repos
2. Add `go.work` to `.gitignore` in all three repos (workspace file is local-only)
3. Document the workspace setup in each repo's README

### Step 8: Set up staging environment

**Effort:** Medium. **Risk:** Low. **Dependency:** Independent of steps 1-7 (can be done in parallel).

1. Provision staging VPS (or allocate ports on existing VPS)
2. Set up staging domain and wildcard DNS
3. Deploy Caddy with staging Caddyfile
4. Create `docker-compose.staging.yml` in server repo
5. Create staging token file
6. Add `:staging` tag to server CI (push to staging branch → build staging image)
7. Deploy staging server
8. Test client against staging server
9. Document staging workflow in specs

### Step order and parallelism

```
Step 1 (create proto) ──────┬──→ Step 2 (migrate client) ──→ Step 4 (client contract tests) ──→ Step 6 (remove e2e CI)
                            │
                            └──→ Step 3 (migrate server) ──→ Step 5 (server contract tests)

Step 7 (go workspace) ── can be done after steps 2+3

Step 8 (staging) ── independent, can start any time
```

**Critical path:** Steps 1 → 2 → 4 → 6. This is the sequence that unblocks the client CI deadlock. Prioritize this.
