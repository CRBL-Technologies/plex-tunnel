# Plan: Public Client Install

## Goal

Two supported install paths for end users:

1. **One-command compose** — download a compose file and `.env`, fill in credentials, `docker compose up`. No build step. Uses the pre-built GHCR image.
2. **Build from source** — clone the public repo, `docker build`, run. No account or registry access required beyond public GitHub.

The server repo stays private. The client and proto repos go public.

---

## What Already Works

- CI builds and pushes `ghcr.io/crbl-technologies/plex-tunnel:latest` on every push to `main` (`packages: write` permission is already set).
- `docker-compose.yml` already defaults to that image.
- Both Dockerfiles do a clean two-stage build with no private dependencies once the proto repo is public.
- No `GOPRIVATE` or credentials are needed once the proto repo is public — `go mod download` will resolve through `proxy.golang.org`.

---

## Steps

### 1. Make the proto repo public (GitHub settings)

The proto repo (`CRBL-Technologies/plex-tunnel-proto`) must be public before any of the build paths work without credentials. This is a one-click change in GitHub repo settings. **Do this first** — everything downstream depends on it.

### 2. Make the client repo public (GitHub settings)

Set `antoinecorbel7/plex-tunnel` (or the CRBL-Technologies fork, whichever is the canonical one) to public. Once public, `github.token` in Actions automatically has `packages: write` for GHCR — the `GHCR_TOKEN` secret is no longer required for publishing. The existing CI fallback `secrets.GHCR_TOKEN || github.token` handles this without any code change.

### 3. Make the GHCR package public (GitHub settings)

Go to **GitHub → Packages → plex-tunnel → Package settings → Change visibility → Public**. This is separate from repo visibility. Until this is done, `docker compose up` will fail with a 401 for anyone who hasn't run `docker login ghcr.io`. Once public, the image pulls anonymously.

### 4. Add `.env.example`

Create `.env.example` at the repo root documenting every variable the client container reads. This is what the join page tells users to copy and fill in.

```
# Required: token issued to you from the PlexTunnel dashboard
PLEXTUNNEL_TOKEN=

# Required: WebSocket URL of the PlexTunnel server
PLEXTUNNEL_SERVER_URL=wss://tunnel.example.com

# Optional: Plex Media Server address (default: http://127.0.0.1:32400)
PLEXTUNNEL_PLEX_TARGET=http://127.0.0.1:32400

# Optional: requested subdomain (server assigns one if blank)
PLEXTUNNEL_SUBDOMAIN=

# Optional: local address for the status UI (default: 127.0.0.1:9090)
PLEXTUNNEL_UI_LISTEN=127.0.0.1:9090
```

### 5. Add a client-only compose file

The existing `docker-compose.yml` bundles Plex Media Server alongside the client. That is the right default for users who want a full stack, but the join-page install flow should target users who **already have Plex running** and just need the tunnel client.

Add `docker-compose.client.yml` (client only, no bundled Plex):

```yaml
services:
  plextunnel-client:
    image: ${PLEXTUNNEL_CLIENT_IMAGE:-ghcr.io/crbl-technologies/plex-tunnel:latest}
    container_name: plextunnel-client
    env_file: .env
    restart: unless-stopped
    network_mode: host
```

This is the file linked from the join page. The join-page install instructions become:

```bash
curl -O https://raw.githubusercontent.com/CRBL-Technologies/plex-tunnel/main/docker-compose.client.yml
curl -O https://raw.githubusercontent.com/CRBL-Technologies/plex-tunnel/main/.env.example
cp .env.example .env
# fill in PLEXTUNNEL_TOKEN and PLEXTUNNEL_SERVER_URL in .env
docker compose -f docker-compose.client.yml up -d
```

### 6. Update README install section

Replace the current install section with two clearly separated paths:

**Path A — Pre-built image (recommended)**
Point users to the steps in §5 above.

**Path B — Build from source**
```bash
git clone https://github.com/CRBL-Technologies/plex-tunnel.git
cd plex-tunnel
cp .env.example .env
# fill in .env
docker build -f Dockerfile.client -t plextunnel-client .
docker run --env-file .env --network host plextunnel-client
```
Or with compose:
```bash
PLEXTUNNEL_CLIENT_IMAGE=plextunnel-client docker compose -f docker-compose.client.yml up -d
```

### 7. Verify CI secret dependency

With the client repo public, confirm no job fails due to missing secrets:

- `GHCR_TOKEN` — no longer needed for image push; `github.token` covers it. Secret can stay as an override but should not be required.
- `PORTAINER_CLIENT_STACK_WEBHOOK_URL` — already guarded with a warning if unset. No change needed.
- `PLEXTUNNEL_SERVER_REPO_TOKEN` — used only in the manual e2e job to pull the private server image. That job is already gated to `workflow_dispatch` only, so it will never run for external contributors. No change needed.

---

## What Does NOT Need to Change

- The server repo stays private. Nothing in the client build or runtime requires server source access.
- `Dockerfile.client` — correct as-is once proto is public.
- `go.mod` / `go.sum` — correct as-is.
- CI workflow permissions — `packages: write` already set.
- The existing `docker-compose.yml` (full stack with Plex) — keep it, it is useful for users who want everything in one compose.

---

## Checklist

- [ ] Proto repo set to public (GitHub settings)
- [ ] Client repo set to public (GitHub settings)
- [ ] GHCR `plex-tunnel` package set to public (GitHub package settings)
- [ ] `.env.example` added
- [ ] `docker-compose.client.yml` added
- [ ] README install section updated with both paths
- [ ] Verify CI push succeeds with `github.token` only (no `GHCR_TOKEN` secret) after repo goes public
