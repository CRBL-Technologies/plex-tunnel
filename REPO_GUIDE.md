# Repository Guide

This repository contains the PlexTunnel client.

## Main Files

- `README.md`: operator-facing setup and usage
- `pkg/client/`: client runtime, reconnect logic, config, and status handling
- `cmd/client/`: binary entrypoint and local UI wiring
- `specs/`: design and architecture notes
- `reviews/`: review template only
- `TODO.md`: current client task list

## Workflow

1. Put user-facing usage in `README.md`.
2. Put design or migration notes in `specs/`.
3. Track actionable follow-up work in `TODO.md`.
4. Use `reviews/TEMPLATE.md` for temporary review writeups, then remove the concrete review file once the work is merged or captured elsewhere.

## Shared Proto

This repo imports the shared transport module from:

- `github.com/CRBL-Technologies/plex-tunnel-proto/tunnel`

For local multi-repo work, run:

```bash
make workspace-setup
```

That creates a parent-level `go.work` linking `plex-tunnel`, `plex-tunnel-server`, and `plex-tunnel-proto`.
