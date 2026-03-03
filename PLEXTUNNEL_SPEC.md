# PlexTunnel — Technical Specification

> **Purpose:** This document is the single source of truth for building PlexTunnel. Hand this to Claude Code to start building the MVP.

---

## Project Overview

PlexTunnel is a managed tunneling service that gives Plex users secure remote access to their media servers without opening ports, configuring firewalls, or dealing with CGNAT restrictions.

**How it works:** A lightweight agent runs alongside the user's Plex server and establishes an outbound encrypted WebSocket connection to a relay server. The relay exposes a public HTTPS endpoint (e.g., `username.plextunnel.io`) that routes traffic back through the tunnel to the local Plex instance.

Since the connection is outbound-only from the agent, it works behind CGNAT, double NAT, firewalls, and any other network restriction.

---

## Architecture

```
Remote Plex client (phone, TV, laptop)
    → HTTPS request to username.plextunnel.io
    → Caddy (TLS termination, wildcard cert)
    → PlexTunnel Relay (Go binary, routes by subdomain)
    → Encrypted WebSocket tunnel
    → PlexTunnel Agent (Go binary / Docker container)
    → Local Plex server (default: 127.0.0.1:32400)
```

### Three Components

1. **Agent** — runs on the user's machine alongside Plex. Connects outbound to the relay.
2. **Relay** — runs on a VPS with a public IP. Accepts agent connections, routes incoming HTTP to the right agent.
3. **Caddy** — sits in front of the relay on the VPS. Handles TLS termination with automatic Let's Encrypt wildcard certs.

---

## Tech Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Language | Go | Single binary, excellent cross-compilation, great networking stdlib, low resource usage |
| Repo structure | Monorepo | One Go module: `cmd/agent`, `cmd/relay`, shared `pkg/` |
| Tunnel protocol (MVP) | WebSocket over TLS | Works through any firewall/proxy, simple, battle-tested |
| Future protocol | QUIC | Lower latency, no head-of-line blocking — design a pluggable Transport interface now |
| TLS termination | Caddy | Automatic Let's Encrypt, wildcard cert support, simple config |
| Agent deployment | Docker container (primary), standalone binary (secondary) | Most Plex users run Docker |
| Auth (MVP) | Pre-shared token per agent | No dashboard yet, just config. Tokens are random UUIDs. |
| DNS | Cloudflare | Wildcard DNS: `*.yourdomain.tld` → relay server IP |

---

## Repo Structure

```
plextunnel/
├── go.mod
├── go.sum
├── README.md
├── Makefile
├── Dockerfile.agent
├── Dockerfile.relay
├── docker-compose.yml          # Example: agent + Plex together
├── docker-compose.relay.yml    # Relay deployment
├── Caddyfile                   # Caddy config for the relay VPS
│
├── cmd/
│   ├── agent/
│   │   └── main.go             # Agent entrypoint
│   └── relay/
│       └── main.go             # Relay entrypoint
│
├── pkg/
│   ├── tunnel/
│   │   ├── transport.go        # Transport interface (pluggable: WebSocket now, QUIC later)
│   │   ├── websocket.go        # WebSocket transport implementation
│   │   ├── message.go          # Tunnel message protocol (framing, types)
│   │   └── tunnel_test.go
│   │
│   ├── agent/
│   │   ├── agent.go            # Agent logic: connect to relay, proxy to Plex
│   │   ├── reconnect.go        # Auto-reconnect with exponential backoff
│   │   └── config.go           # Agent configuration
│   │
│   ├── relay/
│   │   ├── relay.go            # Relay logic: accept agents, route HTTP requests
│   │   ├── router.go           # Subdomain → agent routing
│   │   └── config.go           # Relay configuration
│   │
│   └── auth/
│       └── token.go            # Simple token validation (MVP: pre-shared tokens)
│
├── configs/
│   ├── agent.example.env       # Example agent env vars
│   └── relay.example.env       # Example relay env vars
│
└── scripts/
    ├── install.sh              # One-liner install script for agent binary
    └── generate-token.sh       # Generate a new agent token
```

---

## Agent Specification

### What It Does

1. Reads config (env vars or config file)
2. Connects outbound to the relay via WebSocket over TLS
3. Sends a registration message with its token and desired subdomain
4. Waits for incoming proxy requests from the relay
5. For each request: forwards it to the local Plex server, reads the response, sends it back through the tunnel
6. If disconnected: auto-reconnects with exponential backoff (1s, 2s, 4s, 8s... max 60s)
7. Reports basic health metrics via logs

### Configuration (env vars)

```env
# Required
PLEXTUNNEL_TOKEN=uuid-token-here
PLEXTUNNEL_RELAY_URL=wss://relay.yourdomain.tld/tunnel

# Optional (with defaults)
PLEXTUNNEL_PLEX_TARGET=http://127.0.0.1:32400    # Where Plex is reachable from the agent
PLEXTUNNEL_SUBDOMAIN=myserver                      # Desired subdomain (default: derived from token)
PLEXTUNNEL_LOG_LEVEL=info                          # debug, info, warn, error
```

### Docker Networking Considerations

The agent needs to reach the local Plex server. Three scenarios to support:

**Scenario A — Docker Compose (recommended default):**
Agent and Plex in the same Compose file and network. Agent reaches Plex at `plex:32400`.

```yaml
# docker-compose.yml (shipped with the project)
services:
  plex:
    image: plexinc/pms-docker
    network_mode: host  # or bridge with ports
    # ... user's existing Plex config

  plextunnel-agent:
    image: plextunnel/agent:latest
    environment:
      - PLEXTUNNEL_TOKEN=xxx
      - PLEXTUNNEL_RELAY_URL=wss://relay.yourdomain.tld/tunnel
      - PLEXTUNNEL_PLEX_TARGET=http://plex:32400  # resolves via Docker DNS
    depends_on:
      - plex
```

**Scenario B — Agent with `--network=host`:**
Agent runs directly on host network, reaches Plex at `127.0.0.1:32400`.

```bash
docker run -d --network=host \
  -e PLEXTUNNEL_TOKEN=xxx \
  -e PLEXTUNNEL_RELAY_URL=wss://relay.yourdomain.tld/tunnel \
  plextunnel/agent:latest
```

**Scenario C — Separate containers, shared user-defined network:**
User creates a network and attaches both containers.

```bash
docker network create plextunnel
docker run -d --network=plextunnel --name=plex plexinc/pms-docker
docker run -d --network=plextunnel \
  -e PLEXTUNNEL_PLEX_TARGET=http://plex:32400 \
  -e PLEXTUNNEL_TOKEN=xxx \
  -e PLEXTUNNEL_RELAY_URL=wss://relay.yourdomain.tld/tunnel \
  plextunnel/agent:latest
```

**Default value for `PLEXTUNNEL_PLEX_TARGET`:** `http://127.0.0.1:32400` (works for host networking and bare metal). Docs should clearly explain how to set it for each scenario.

### Agent Reconnection Logic

```
on disconnect:
  attempt = 0
  while not connected:
    delay = min(2^attempt * 1 second, 60 seconds) + random jitter (0-500ms)
    wait(delay)
    try connect
    if success: attempt = 0, break
    else: attempt++
    log each attempt at info level
```

---

## Relay Specification

### What It Does

1. Listens for incoming agent WebSocket connections on a private endpoint (e.g., `/tunnel`)
2. Validates the agent's token
3. Registers the agent with its subdomain in a routing table (in-memory map)
4. Listens for incoming HTTP requests from Plex clients
5. Extracts the subdomain from the `Host` header (e.g., `myserver.yourdomain.tld`)
6. Looks up the corresponding agent in the routing table
7. Forwards the full HTTP request through the WebSocket tunnel to the agent
8. Streams the response back to the Plex client
9. Handles agent disconnection: removes from routing table, returns 502 to clients

### Configuration (env vars)

```env
# Required
PLEXTUNNEL_RELAY_LISTEN=:8080                      # HTTP listen address (Caddy proxies to this)
PLEXTUNNEL_RELAY_TUNNEL_LISTEN=:8081               # WebSocket tunnel listen address
PLEXTUNNEL_RELAY_DOMAIN=yourdomain.tld             # Base domain for subdomain extraction
PLEXTUNNEL_RELAY_TOKENS_FILE=/etc/plextunnel/tokens.json  # MVP: JSON file of valid tokens

# Optional
PLEXTUNNEL_RELAY_LOG_LEVEL=info
```

### Token File (MVP)

Simple JSON file mapping tokens to subdomains:

```json
{
  "tokens": [
    { "token": "uuid-1", "subdomain": "myserver" },
    { "token": "uuid-2", "subdomain": "friendserver" }
  ]
}
```

Later phases replace this with a database + API.

### Routing

```
incoming request: Host: myserver.yourdomain.tld
  → extract "myserver" from Host header
  → look up "myserver" in agents map
  → if found: proxy request through tunnel to that agent
  → if not found: return 502 "Tunnel not connected"
```

---

## Tunnel Protocol (Message Framing)

Communication between agent and relay happens over WebSocket using a simple binary or JSON message protocol.

### Message Types

```go
type MessageType uint8

const (
    MsgRegister    MessageType = 1  // Agent → Relay: register with token + subdomain
    MsgRegisterAck MessageType = 2  // Relay → Agent: registration confirmed
    MsgHTTPRequest MessageType = 3  // Relay → Agent: incoming HTTP request to proxy
    MsgHTTPResponse MessageType = 4 // Agent → Relay: HTTP response from Plex
    MsgPing        MessageType = 5  // Bidirectional: keepalive
    MsgPong        MessageType = 6  // Bidirectional: keepalive response
    MsgError       MessageType = 7  // Bidirectional: error message
)
```

### Request/Response Flow

```
1. Plex client → HTTPS → Caddy → Relay (HTTP request)
2. Relay generates a unique request ID
3. Relay sends MsgHTTPRequest to agent via WebSocket:
   {
     "id": "req-uuid",
     "method": "GET",
     "path": "/library/sections",
     "headers": { ... },
     "body": <bytes>  // if POST/PUT
   }
4. Agent receives request, forwards to local Plex at PLEXTUNNEL_PLEX_TARGET
5. Agent sends MsgHTTPResponse back:
   {
     "id": "req-uuid",
     "status": 200,
     "headers": { ... },
     "body": <bytes>
   }
6. Relay matches response to original HTTP request by ID, writes response to Plex client
```

### Streaming Consideration

For large media responses (video streaming), the body should be streamed in chunks rather than buffered entirely in memory. Use chunked transfer within the WebSocket:

```go
// For large responses, send multiple MsgHTTPResponse messages with the same ID
// First message: headers + status + first chunk
// Subsequent messages: continuation chunks with same ID
// Final message: empty body, marks end of stream
```

### Keepalive

- Agent sends MsgPing every 30 seconds
- Relay responds with MsgPong
- If no Pong received within 10 seconds, agent considers connection dead and reconnects
- Relay removes agent from routing table if no Ping received in 60 seconds

---

## Transport Interface (Pluggable)

Design the transport layer as an interface so QUIC can be added later without rewriting core logic:

```go
// pkg/tunnel/transport.go

type Transport interface {
    // Client-side (agent)
    Dial(ctx context.Context, url string) (Connection, error)

    // Server-side (relay)
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

MVP implementation: `WebSocketTransport` that wraps `gorilla/websocket` or `nhooyr.io/websocket`.

Future: `QUICTransport` that wraps `quic-go`.

---

## Caddy Configuration (Relay VPS)

```caddyfile
# Caddyfile for the relay VPS
# Handles TLS termination with automatic Let's Encrypt wildcard cert
# Requires Cloudflare DNS plugin for ACME DNS challenge (needed for wildcard certs)

*.yourdomain.tld {
    tls {
        dns cloudflare {env.CLOUDFLARE_API_TOKEN}
    }

    # Tunnel endpoint — agents connect here
    @tunnel path /tunnel
    handle @tunnel {
        reverse_proxy localhost:8081
    }

    # Everything else — proxy to relay HTTP handler
    handle {
        reverse_proxy localhost:8080
    }
}
```

---

## Docker Build

### Dockerfile.agent

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /plextunnel-agent ./cmd/agent

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /plextunnel-agent /usr/local/bin/plextunnel-agent
ENTRYPOINT ["plextunnel-agent"]
```

### Dockerfile.relay

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /plextunnel-relay ./cmd/relay

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /plextunnel-relay /usr/local/bin/plextunnel-relay
ENTRYPOINT ["plextunnel-relay"]
```

---

## Makefile

```makefile
.PHONY: build-agent build-relay build test docker-agent docker-relay clean

build-agent:
	go build -o bin/plextunnel-agent ./cmd/agent

build-relay:
	go build -o bin/plextunnel-relay ./cmd/relay

build: build-agent build-relay

test:
	go test ./...

docker-agent:
	docker build -f Dockerfile.agent -t plextunnel/agent:latest .

docker-relay:
	docker build -f Dockerfile.relay -t plextunnel/agent:latest .

clean:
	rm -rf bin/

# Cross-compile agent for all platforms
release-agent:
	GOOS=linux GOARCH=amd64 go build -o bin/plextunnel-agent-linux-amd64 ./cmd/agent
	GOOS=linux GOARCH=arm64 go build -o bin/plextunnel-agent-linux-arm64 ./cmd/agent
	GOOS=darwin GOARCH=amd64 go build -o bin/plextunnel-agent-darwin-amd64 ./cmd/agent
	GOOS=darwin GOARCH=arm64 go build -o bin/plextunnel-agent-darwin-arm64 ./cmd/agent
	GOOS=windows GOARCH=amd64 go build -o bin/plextunnel-agent-windows-amd64.exe ./cmd/agent
```

---

## Phase 0 Milestones (Solo Dogfooding)

Build in this exact order:

### Milestone 1: Tunnel proof-of-concept (Day 1-2)
- [ ] Go module initialized, repo structure created
- [ ] `Transport` interface defined in `pkg/tunnel/transport.go`
- [ ] `WebSocketTransport` implemented using `nhooyr.io/websocket`
- [ ] Message types and framing defined in `pkg/tunnel/message.go`
- [ ] Basic agent: connects to relay, sends register, handles ping/pong
- [ ] Basic relay: accepts agent connection, validates token (hardcoded), adds to routing map
- [ ] Test: agent and relay running locally, relay forwards a curl request through the tunnel to a local HTTP server

### Milestone 2: HTTP proxying (Day 3-4)
- [ ] Relay: accepts incoming HTTP requests, extracts subdomain, looks up agent, sends `MsgHTTPRequest`
- [ ] Agent: receives `MsgHTTPRequest`, forwards to Plex target, sends `MsgHTTPResponse`
- [ ] Relay: receives response, writes back to original HTTP client
- [ ] Streaming support: chunked transfer for large responses (video)
- [ ] Test: Plex web UI loads through the tunnel, can browse library

### Milestone 3: Playback (Day 5-6)
- [ ] Test actual video playback through the tunnel (1080p, then 4K)
- [ ] Fix buffering / streaming issues (ensure chunks flow without excessive buffering)
- [ ] Measure latency overhead
- [ ] Handle WebSocket connection limits (concurrent requests over a single tunnel)
- [ ] Multiplexing: multiple HTTP requests in-flight over one WebSocket connection

### Milestone 4: Reliability (Day 7-8)
- [ ] Agent auto-reconnect with exponential backoff + jitter
- [ ] Keepalive ping/pong (30s interval, 10s timeout)
- [ ] Relay cleans up stale agents (60s no-ping timeout)
- [ ] Graceful shutdown on SIGTERM/SIGINT
- [ ] Proper logging (structured, leveled: debug/info/warn/error)

### Milestone 5: Docker & deployment (Day 9-10)
- [ ] Dockerfile.agent builds and runs
- [ ] Dockerfile.relay builds and runs
- [ ] docker-compose.yml for agent + Plex (Scenario A)
- [ ] Caddyfile for relay VPS
- [ ] docker-compose.relay.yml for relay + Caddy on VPS
- [ ] Deploy relay to Hetzner, agent at home, test end-to-end
- [ ] Wildcard DNS configured on Cloudflare
- [ ] Full test: access Plex from phone on cellular via `yourname.yourdomain.tld`

### Milestone 6: Harden (Day 11-14)
- [ ] Token auth from JSON file (not hardcoded)
- [ ] Token generation script
- [ ] Agent config via env vars with sensible defaults
- [ ] Relay config via env vars
- [ ] Test: kill agent, verify it reconnects. Kill relay, verify agent retries. Switch WiFi, verify recovery.
- [ ] Test from multiple network conditions: CGNAT home, coffee shop, VPN, tethered phone
- [ ] Test with real Plex clients: Apple TV, iOS app, web browser, macOS Plex app
- [ ] Fix any bugs found during real-world testing

---

## Phase 1 Milestones (Friends & Family, Weeks 4-7)

- [ ] Multi-user routing: relay handles N agents simultaneously, each with their own subdomain
- [ ] Token management: CLI tool to add/remove tokens and subdomains from the JSON file
- [ ] README with clear install instructions for Docker Compose, host networking, and standalone binary
- [ ] Install script: `curl -sSL https://yourdomain.tld/install.sh | bash`
- [ ] Invite 5-10 friends, onboard via shared doc
- [ ] Discord/Telegram group for feedback
- [ ] Health check endpoint: `https://sub.yourdomain.tld/plextunnel/health` returns tunnel status
- [ ] Basic bandwidth logging per agent
- [ ] Fix top issues from friend feedback

---

## Phase 2 Milestones (Closed Beta, Weeks 8-14)

- [ ] Web dashboard (Next.js): signup, login, API token generation, connection status, setup wizard
- [ ] Control plane API (Fastify or Go): user CRUD, token management, agent status
- [ ] PostgreSQL database (Supabase): users, tokens, connection logs, usage stats
- [ ] Multi-region relays: US-East, US-West, EU-West
- [ ] Agent auto-selects lowest latency relay
- [ ] Relay failover
- [ ] Bandwidth tracking and per-user rate limiting
- [ ] Prometheus + Grafana monitoring
- [ ] Windows agent binary + installer
- [ ] Launch to 50-100 beta testers from Reddit

---

## Phase 3 Milestones (Public Launch, Weeks 15-20)

- [ ] Landing page
- [ ] Documentation site
- [ ] SEO blog posts (Plex remote access, CGNAT, etc.)
- [ ] Unraid Community Applications plugin
- [ ] Synology / TrueNAS packages (if demand)
- [ ] Homebrew formula for macOS
- [ ] Public launch: Reddit, HN, Product Hunt
- [ ] Outreach to homelab YouTubers

---

## Phase 4 Milestones (Monetization, Month 6+)

- [ ] Stripe integration: Basic ($4.99/mo) and Pro ($9.99/mo) tiers
- [ ] Free tier: 10 Mbps, 1 stream
- [ ] Bandwidth enforcement per tier
- [ ] Custom subdomains (paid) and custom domains (Pro)
- [ ] Annual billing option (20% discount)
- [ ] Jellyfin / Emby support
- [ ] QUIC transport option
- [ ] Referral program

---

## Key Dependencies (Go)

```
nhooyr.io/websocket           # WebSocket implementation (lighter than gorilla)
github.com/caddyserver/caddy  # TLS termination (deployed separately, not a Go dep)
github.com/rs/zerolog          # Structured logging
github.com/google/uuid         # Token generation
github.com/kelseyhightower/envconfig  # Env var config parsing
```

---

## Important Design Principles

1. **The tunnel must be transparent.** Plex clients should not know they're going through a tunnel. All HTTP headers, cookies, WebSocket upgrades (Plex uses these for real-time updates) must pass through unmodified.

2. **Streaming must work.** Video is the primary payload. The proxy must stream response bodies in chunks, not buffer entire responses in memory. A 4K movie file could be 50+ GB.

3. **Reconnection must be seamless.** Users will not tolerate "restart the agent" as a fix. The agent must handle every network disruption automatically.

4. **Keep the relay stateless (almost).** The only state on the relay is the in-memory routing table of connected agents. If the relay restarts, agents reconnect and re-register. No data loss.

5. **Security: never inspect traffic.** The relay is a pass-through. It does not read, cache, log, or store any media content. This is critical for legal protection and user trust.

6. **Config over code.** The agent and relay should be usable with just env vars. No config files required for basic usage. Config files are optional for advanced setups.
