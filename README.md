# PlexTunnel Client

PlexTunnel Client runs next to your Plex server and opens an outbound encrypted tunnel to PlexTunnel Server.

## Related Repository

- Server runtime: [github.com/antoinecorbel7/plex-tunnel-server](https://github.com/antoinecorbel7/plex-tunnel-server)

## Quick Start

### 1. Build

```bash
make build
```

### 2. Configure

```bash
cp configs/client.example.env .env.client
# edit values
```

### 3. Run

```bash
set -a; source .env.client; set +a
./bin/plextunnel-client
```

## Docker

```bash
make docker-client
```

`docker-compose.yml` includes a local Plex container and `plextunnel-client` in host networking mode.

## Configuration

- `PLEXTUNNEL_TOKEN` (required)
- `PLEXTUNNEL_SERVER_URL` (required, example: `wss://server.example.com/tunnel`)
- `PLEXTUNNEL_PLEX_TARGET` (default: `http://127.0.0.1:32400`)
- `PLEXTUNNEL_SUBDOMAIN` (optional)
- `PLEXTUNNEL_LOG_LEVEL` (default: `info`)

`PLEXTUNNEL_RELAY_URL` is still accepted as a fallback for backward compatibility.

## Development

```bash
make test
```
