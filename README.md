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

Build local image (optional):

```bash
make docker-client
```

`docker-compose.yml` pulls `ghcr.io/antoinecorbel7/plex-tunnel:latest` by default.

```bash
docker login ghcr.io -u antoinecorbel7
docker compose --env-file .env pull
docker compose --env-file .env up -d plextunnel-client
```

Override image if needed:

```bash
PLEXTUNNEL_CLIENT_IMAGE=ghcr.io/antoinecorbel7/plex-tunnel:sha-<commit> docker compose --env-file .env up -d plextunnel-client
```

## Configuration

- `PLEXTUNNEL_TOKEN` (required)
- `PLEXTUNNEL_SERVER_URL` (required, example: `wss://server.example.com/tunnel`)
- `PLEXTUNNEL_PLEX_TARGET` (default: `http://127.0.0.1:32400`)
- `PLEXTUNNEL_SUBDOMAIN` (optional)
- `PLEXTUNNEL_LOG_LEVEL` (default: `info`)

## CI/CD

GitHub Actions runs tests on pull requests and on pushes to `main`.
On pushes to `main`, it builds and pushes `Dockerfile.client` to GitHub Container Registry:

- `ghcr.io/antoinecorbel7/plex-tunnel:sha-<commit>`
- `ghcr.io/antoinecorbel7/plex-tunnel:latest`

No extra Docker credentials are required in repo secrets for same-repo publishing.

## Development

```bash
make test
```

## Local Debug Environment

Run a full local stack (server + client + mock Plex) to validate end-to-end behavior:

```bash
make debug-test
```

Useful commands:

```bash
make debug-up
make debug-logs
make debug-down
```

Notes:

- The debug stack uses [docker-compose.debug.yml](docker-compose.debug.yml).
- Test token/subdomain live in [testdata/tokens.debug.json](testdata/tokens.debug.json).
- `make debug-test` auto-builds a local server image from `/tmp/plex-tunnel-server` by default.
- Override server source path with: `PLEXTUNNEL_SERVER_CONTEXT=/path/to/plex-tunnel-server make debug-test`
- Or use a prebuilt image directly: `PLEXTUNNEL_SERVER_IMAGE=ghcr.io/antoinecorbel7/plex-tunnel-server:latest make debug-test`
