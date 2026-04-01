# TASK: Fix brand logo colors in client UI

## Overview

The client UI in `cmd/client/ui.go` uses old/incorrect colors for the Portless brand
logo. Fix the favicon SVG, inline logo SVG, and text color to match brand guidelines.

---

## Change 1: Fix favicon inline SVG

### Where to work
- `cmd/client/ui.go` — line 144 (the `<link rel="icon"...>` tag)

### Current behavior
The data URI SVG uses:
- Background: `%231a1a2e` (hex #1a1a2e)
- Font-weight: `700`

### Desired behavior
- Background: `%231C1917` (hex #1C1917 — Portless brand dark)
- Font-weight: `600`
- Font-family should include `'Inter'` if possible, but since this is a data URI
  favicon, `system-ui,sans-serif` is acceptable.

---

## Change 2: Fix inline logo SVG

### Where to work
- `cmd/client/ui.go` — lines 302-305 (the `<svg>` element with the P logo)

### Current behavior
```html
<rect width="36" height="36" rx="8" fill="#1a1a2e"/>
<text ... font-weight="700" ... fill="#D97706">P</text>
```

### Desired behavior
```html
<rect width="36" height="36" rx="8" fill="#1C1917"/>
<text ... font-weight="600" ... fill="#D97706">P</text>
```
Change background to `#1C1917` and font-weight to `600`. P color stays `#D97706`.

---

## Change 3: Fix --text CSS variable

### Where to work
- `cmd/client/ui.go` — line 151

### Current behavior
```css
--text: #1a1a1a;
```

### Desired behavior
```css
--text: #1C1917;
```
Use the Portless brand dark color.

---

## Constraints
- DO NOT delete or modify any existing code that is not explicitly mentioned in this task.
- DO NOT change any layout, structure, or functionality.
- Keep all existing tests passing.

## Verification
```bash
cd /home/dev/github/plex-tunnel
go build ./...
go test ./...
```

## When Done
Commit all changes with a descriptive commit message. Do NOT modify TASK.md.
