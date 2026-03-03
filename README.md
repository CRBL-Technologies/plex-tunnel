# PlexTunnel

PlexTunnel is a managed tunneling service that gives Plex users secure remote access without opening inbound ports.

## Components

- `plextunnel-agent`: runs near Plex, opens outbound tunnel to relay
- `plextunnel-relay`: receives public traffic and routes by subdomain
- `Caddy`: TLS termination and wildcard cert handling

## Quick Start

### 1. Build binaries

```bash
make build
```

### 2. Configure relay

```bash
cp configs/relay.example.env .env.relay
# edit values
```

### 3. Configure agent

```bash
cp configs/agent.example.env .env.agent
# edit values
```

### 4. Run relay

```bash
set -a; source .env.relay; set +a
./bin/plextunnel-relay
```

### 5. Run agent

```bash
set -a; source .env.agent; set +a
./bin/plextunnel-agent
```

## Environment Variables

### Agent

- `PLEXTUNNEL_TOKEN` (required)
- `PLEXTUNNEL_RELAY_URL` (required, example: `wss://relay.example.com/tunnel`)
- `PLEXTUNNEL_PLEX_TARGET` (default: `http://127.0.0.1:32400`)
- `PLEXTUNNEL_SUBDOMAIN` (optional)
- `PLEXTUNNEL_LOG_LEVEL` (default: `info`)

### Relay

- `PLEXTUNNEL_RELAY_LISTEN` (default: `:8080`)
- `PLEXTUNNEL_RELAY_TUNNEL_LISTEN` (default: `:8081`)
- `PLEXTUNNEL_RELAY_DOMAIN` (required)
- `PLEXTUNNEL_RELAY_TOKENS_FILE` (required)
- `PLEXTUNNEL_RELAY_LOG_LEVEL` (default: `info`)

## Development

```bash
make test
```
