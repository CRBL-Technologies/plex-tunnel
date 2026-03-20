# PlexTunnel Client

PlexTunnel Client runs next to your Plex server and opens an outbound encrypted tunnel to PlexTunnel Server, making your Plex accessible remotely without port forwarding.

## Install

### Option A — Docker Compose (recommended)

Download the compose file:

```bash
curl -O https://raw.githubusercontent.com/CRBL-Technologies/plex-tunnel/main/docker-compose.client.yml
```

Open `docker-compose.client.yml` and replace the two placeholder values:

- `your-token-here` → your token from the PlexTunnel dashboard
- `wss://tunnel.example.com` → the WebSocket URL of your PlexTunnel server

Then start the client:

```bash
docker compose -f docker-compose.client.yml up -d
```

All values in the compose file use `${VAR:-placeholder}` syntax — you can edit the placeholders directly in the file, or override any value by exporting the corresponding environment variable before running `docker compose`.

**Already running Plex in Docker?** The compose file has a commented-out Plex block at the top — paste your existing Plex service there to manage both containers together.

**Starting from scratch with Plex?** Use `docker-compose.yml` instead, which includes a full Plex + PlexTunnel stack. Replace the placeholder values for `PLEX_CLAIM`, `PLEXTUNNEL_TOKEN`, `PLEXTUNNEL_SERVER_URL`, and the volume paths:

```bash
curl -O https://raw.githubusercontent.com/CRBL-Technologies/plex-tunnel/main/docker-compose.yml
# edit placeholder values in the file
docker compose up -d
```

### Option B — Build from source

```bash
git clone https://github.com/CRBL-Technologies/plex-tunnel.git
cd plex-tunnel
docker build -f Dockerfile.client -t plextunnel-client .
docker run --network host \
  -e PLEXTUNNEL_TOKEN=your-token-here \
  -e PLEXTUNNEL_SERVER_URL=wss://tunnel.example.com \
  plextunnel-client
```

Or with compose after building:

```bash
PLEXTUNNEL_CLIENT_IMAGE=plextunnel-client docker compose -f docker-compose.client.yml up -d
```

## Configuration

All configuration is passed as environment variables:

| Variable | Required | Default | Description |
|---|---|---|---|
| `PLEXTUNNEL_TOKEN` | yes | — | Token from the PlexTunnel dashboard |
| `PLEXTUNNEL_SERVER_URL` | yes | — | WebSocket URL of the server, e.g. `wss://tunnel.example.com` |
| `PLEXTUNNEL_PLEX_TARGET` | no | `http://127.0.0.1:32400` | Address of your local Plex instance |
| `PLEXTUNNEL_SUBDOMAIN` | no | server-assigned | Fixed subdomain to request |
| `PLEXTUNNEL_LOG_LEVEL` | no | `info` | Log verbosity (`debug`, `info`, `warn`, `error`) |
| `PLEXTUNNEL_DEBUG_BANDWIDTH_LOGGING` | no | `false` | Emit per-chunk timing logs for Plex reads and tunnel writes; set `PLEXTUNNEL_LOG_LEVEL=debug` to see them |
| `PLEXTUNNEL_UI_LISTEN` | no | `127.0.0.1:9090` | Local status UI address; set empty to disable |

## Status UI

The client exposes a local status page at `http://127.0.0.1:9090/` showing connection state, assigned subdomain, and recent errors. Settings can be changed from the UI without restarting the container.

## CI/CD

On every push to `main`, GitHub Actions builds and pushes the image to GitHub Container Registry:

- `ghcr.io/crbl-technologies/plex-tunnel:latest`
- `ghcr.io/crbl-technologies/plex-tunnel:sha-<commit>`

The image is public — no login required to pull.

## Development

```bash
make test
```

Use the shared workspace helper when you want local client and proto changes to resolve together:

```bash
make workspace-setup
```

Expected layout:

```text
../go.work
../plex-tunnel
../plex-tunnel-proto
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
- Override server source path with: `PLEXTUNNEL_SERVER_CONTEXT=/path/to/plex-tunnel-server make debug-test`
- Or use a prebuilt image directly: `PLEXTUNNEL_SERVER_IMAGE=ghcr.io/crbl-technologies/plex-tunnel-server:latest make debug-test`

## Related

- Proto module: [github.com/CRBL-Technologies/plex-tunnel-proto](https://github.com/CRBL-Technologies/plex-tunnel-proto)
