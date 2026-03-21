# Task: Rebrand to Portless + switch to Elastic License 2.0

## 1. Replace LICENSE file

Replace the entire contents of `LICENSE` with the Elastic License 2.0 text. Copy it exactly from `/home/dev/github/plex-tunnel-server/LICENSE`.

## 2. Rebrand user-facing docs

In these files, replace "PlexTunnel" with "Portless" in **user-facing text only**:

- `README.md` — update title, description, and any mentions of "PlexTunnel" (keep env var names like `PLEXTUNNEL_*` and Docker image refs unchanged)
- `REPO_GUIDE.md` — update description
- `TODO.md` — update title

**DO NOT modify:**
- Go source code, imports, or module paths
- Environment variable names (`PLEXTUNNEL_*`)
- Docker image names/refs (`ghcr.io/crbl-technologies/plex-tunnel`)
- Files in `specs/` directory (these are historical design docs)
- Any `.go` files

## Verification

```bash
# Confirm LICENSE was replaced:
head -1 LICENSE
# Should output: ## Elastic License 2.0 (ELv2)

# Confirm README title:
head -1 README.md
# Should output: # Portless Client

# Confirm no Go files were modified:
git diff --name-only -- '*.go'
# Should output nothing
```
