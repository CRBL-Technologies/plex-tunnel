# Changelog

## Unreleased

### Breaking changes

- The client status UI no longer accepts HTTP Basic Auth. The previous hardcoded
  `admin` username is removed. Users must now log in via a form at `/login` and
  receive a session cookie. Existing scripts that POST to `/api/status` with
  HTTP Basic Auth will need to be updated to obtain and send the
  `plextunnel_session` cookie instead.

### Added

- `PLEXTUNNEL_UI_USERNAME` env var (optional). When set, the login form requires
  both username and password. When unset, the form is password-only — users do not
  have to guess a username.
- `PLEXTUNNEL_UI_SESSION_TTL` env var (optional, default `168h`). Sets the session
  cookie lifetime.
- Logout button in the status UI.
- Per-IP login rate limiting: max 10 POST `/login` attempts per minute per source IP;
  exceeded IPs are blocked for 5 minutes.
