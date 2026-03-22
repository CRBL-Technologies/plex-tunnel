# TASK: Upgrade Go from 1.22 to 1.25 + Switch CI to OIDC

## Overview

Upgrade Go toolchain to fix known stdlib vulnerabilities (03-infra F1) and switch CI Docker login from stored PAT secrets to OIDC (03-infra F5).

---

## Change 1: Update go.mod

### Where to work
- `go.mod` — line 3

### Current behavior
```
go 1.22
```

### Desired behavior
```
go 1.25
```

Run `go mod tidy` after updating.

---

## Change 2: Update Dockerfile

### Where to work
- `Dockerfile.client` — line 1

### Current behavior
```
FROM golang:1.22-alpine AS builder
```

### Desired behavior
```
FROM golang:1.25-alpine AS builder
```

---

## Change 3: Switch CI Docker login to OIDC

### Where to work
- `.github/workflows/ci-cd.yml` — lines 54-55 (e2e-connectivity job) and lines 81-82 (docker job)

### Current behavior
Both `Log in to GHCR` steps use:
```yaml
          username: ${{ secrets.GHCR_USERNAME || github.actor }}
          password: ${{ secrets.GHCR_TOKEN || github.token }}
```

### Desired behavior
Change both to:
```yaml
          username: ${{ github.actor }}
          password: ${{ github.token }}
```

Also on line 45, the `PLEXTUNNEL_SERVER_REPO_TOKEN` env var uses:
```yaml
      PLEXTUNNEL_SERVER_REPO_TOKEN: ${{ secrets.PLEXTUNNEL_SERVER_REPO_TOKEN || secrets.GHCR_TOKEN || github.token }}
```

Change to:
```yaml
      PLEXTUNNEL_SERVER_REPO_TOKEN: ${{ secrets.PLEXTUNNEL_SERVER_REPO_TOKEN || github.token }}
```

Remove only the `secrets.GHCR_TOKEN` fallback — keep `secrets.PLEXTUNNEL_SERVER_REPO_TOKEN` since that's a cross-repo token that may need different permissions.

---

## Constraints
- DO NOT delete or modify any existing code that is not explicitly mentioned in this task.
- Keep all existing tests passing.
- Run `go mod tidy` after changing go.mod.

## Verification
```bash
go build ./...
go test ./...
```

## When Done
Commit all changes with a descriptive commit message. Do NOT modify TASK.md.
