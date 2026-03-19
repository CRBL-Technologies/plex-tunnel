# PlexTunnel Client

PlexTunnel Client runs next to your Plex server and opens an outbound encrypted tunnel to PlexTunnel Server.

## Related Repository

- Server runtime: [github.com/CRBL-Technologies/plex-tunnel-server](https://github.com/CRBL-Technologies/plex-tunnel-server)

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

`docker-compose.yml` pulls `ghcr.io/crbl-technologies/plex-tunnel:latest` by default.

```bash
docker login ghcr.io -u CRBL-Technologies
docker compose --env-file .env pull
docker compose --env-file .env up -d plextunnel-client
```

Override image if needed:

```bash
PLEXTUNNEL_CLIENT_IMAGE=ghcr.io/crbl-technologies/plex-tunnel:sha-<commit> docker compose --env-file .env up -d plextunnel-client
```

## Configuration

- `PLEXTUNNEL_TOKEN` (required)
- `PLEXTUNNEL_SERVER_URL` (required, example: `wss://server.example.com/tunnel`)
- `PLEXTUNNEL_PLEX_TARGET` (default: `http://127.0.0.1:32400`)
- `PLEXTUNNEL_SUBDOMAIN` (optional)
- `PLEXTUNNEL_LOG_LEVEL` (default: `info`)
- `PLEXTUNNEL_UI_LISTEN` (default: `127.0.0.1:9090`, set empty to disable UI)

## Web UI

Client now exposes a small local status/settings page by default:

- URL: `http://127.0.0.1:9090/`
- Status: connected/disconnected, subdomain, last errors, reconnect attempts
- Settings: token, server URL, subdomain, Plex target, log level

Applying settings from the UI restarts the client runtime immediately.

## CI/CD

GitHub Actions runs tests on pull requests and on pushes to `main`.
On pushes to `main`, it builds and pushes `Dockerfile.client` to GitHub Container Registry:

- `ghcr.io/crbl-technologies/plex-tunnel:sha-<commit>`
- `ghcr.io/crbl-technologies/plex-tunnel:latest`

No extra Docker credentials are required in repo secrets for same-repo publishing.

## Development

```bash
make test
```

Use the shared workspace helper when you want local client/server/proto changes to resolve together:

```bash
make workspace-setup
```

Expected layout:

```text
../go.work
../plex-tunnel
../plex-tunnel-proto
../plex-tunnel-server   # optional, only if you are changing server + client together
```

`go.work` stays local-only and is not committed.

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
- `make debug-test` auto-clones (or pulls) the server repo into `/tmp/plex-tunnel-server` by default, then builds a local server image.
- If `PLEXTUNNEL_SERVER_IMAGE` is set but cannot be pulled, the script automatically falls back to source clone/build.
- Override server source path with: `PLEXTUNNEL_SERVER_CONTEXT=/path/to/plex-tunnel-server make debug-test`
- Override server repo/ref with:
  - `PLEXTUNNEL_SERVER_REPO_URL=git@github.com:<org>/<repo>.git` (or HTTPS URL with credentials/token)
  - `PLEXTUNNEL_SERVER_REF=<branch-or-tag>`
- For private server repos over HTTPS, provide a token:
  - `PLEXTUNNEL_SERVER_REPO_TOKEN=<github_token_or_pat>`
- Or use a prebuilt image directly: `PLEXTUNNEL_SERVER_IMAGE=ghcr.io/crbl-technologies/plex-tunnel-server:latest make debug-test`
