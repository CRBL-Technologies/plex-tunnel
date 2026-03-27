# TASK: Rebrand client UI to Portless identity

## Overview
The client management UI still uses the old "PlexTunnel Client" branding with a dark theme and blue accents. Rebrand it to match the Portless brand identity used on the server dashboard: light theme, amber buttons, green status indicators, the Portless logo SVG, and "Portless" naming throughout.

---

## Change 1: Rebrand the HTML template in ui.go

### Where to work
- `cmd/client/ui.go` — the `statusPageTmpl` template string (lines 126–360)

### Current behavior
- Title: "PlexTunnel Client UI"
- Heading: "PlexTunnel Client"
- Dark theme with colors: `--bg: #0b0f14`, `--accent: #4dabf7` (blue), etc.
- Blue button styling, dark card backgrounds
- No logo or favicon

### Desired behavior
- Title: "Portless Client"
- Heading: replace `<h1>PlexTunnel Client</h1>` with the Portless SVG logo (centered) followed by `<h1>Portless Client</h1>`
- Light theme matching the server dashboard:
  - Body background: `#f8f9fa`
  - Text color: `#1a1a1a`
  - Card background: `#fff` with `border: 1px solid #dee2e6`, `border-radius: 12px`
  - Muted text: `#495057`
  - Amber buttons: `background: #e5a00d`, `color: #1a1a1a`, hover: `#c98a0b`
  - Status badge "CONNECTED": green — `background: #f0fdf4; border: 1px solid #b2f2bb; color: #2b8a3e`
  - Status badge "DISCONNECTED": red — `background: #fff5f5; border: 1px solid #ffc9c9; color: #c92a2a`
  - Info bubbles: border `#dee2e6`, color `#495057`, tooltip bg `#fff` with border `#dee2e6`
  - Input fields: `background: #f8f9fa; border: 1px solid #dee2e6; color: #1a1a1a`
  - Messages: success color `#2b8a3e`, error color `#c92a2a`
  - Font family: `system-ui, -apple-system, sans-serif` (body), `ui-monospace, SFMono-Regular, Menlo, Consolas, monospace` (values/inputs)
- Add the same Portless favicon as the server dashboard:
  ```
  <link rel="icon" type="image/svg+xml" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 64 64'%3E%3Ccircle cx='32' cy='32' r='28' fill='none' stroke='%231D9E75' stroke-width='3.5'/%3E%3Ccircle cx='32' cy='32' r='21' fill='%230F6E56' opacity='0.12'/%3E%3Cpath d='M25 19 L44 32 L25 45 Z' fill='%231D9E75'/%3E%3C/svg%3E">
  ```
- Add the Portless SVG logo above the h1 (same SVG as server dashboard):
  ```html
  <div style="text-align:center;margin-bottom:0.75rem;">
    <svg width="64" height="64" viewBox="0 0 64 64" xmlns="http://www.w3.org/2000/svg" aria-label="Portless">
      <circle cx="32" cy="32" r="28" fill="none" stroke="#1D9E75" stroke-width="3.5"/>
      <circle cx="32" cy="32" r="21" fill="#0F6E56" opacity="0.12"/>
      <circle cx="32" cy="32" r="15" fill="none" stroke="#1D9E75" stroke-width="1" opacity="0.2"/>
      <path d="M25 19 L44 32 L25 45 Z" fill="#1D9E75"/>
    </svg>
  </div>
  ```
- Keep the same layout structure (two panels: status + settings), info bubbles, form fields, and auto-refresh behavior
- Keep responsive breakpoint at 700px

### Notes
- The CSS `:root` variables need to be completely replaced
- The `body` background should change from `radial-gradient(...)` to flat `#f8f9fa`
- Button style changes from blue `var(--accent)` to amber `#e5a00d`
- The `.panel` class should match `.card` from server: white bg, light border, subtle shadow
- Section titles (`h2`) should use the `.field-label` style: uppercase, small, semi-bold, `#495057`
- `.value` font should switch from "IBM Plex Mono" to `ui-monospace, SFMono-Regular, Menlo, Consolas, monospace`

---

## Constraints
- DO NOT delete or modify any existing code that is not explicitly mentioned in this task.
- DO NOT change any Go logic, routes, handlers, or controller code — only the HTML/CSS template string.
- Keep all existing template variables and form fields intact.
- Keep all info bubble tooltips with the same text.
- Keep the auto-refresh meta tag.
- Keep all existing tests passing.

## Verification
```bash
go build ./...
go test ./...
```

## When Done
Commit all changes with a descriptive commit message. Do NOT modify TASK.md.
