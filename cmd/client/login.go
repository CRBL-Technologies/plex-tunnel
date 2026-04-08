package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName   = "plextunnel_session"
	loginWindow         = time.Minute
	loginBlockDuration  = 5 * time.Minute
	loginBucketMaxAge   = 10 * time.Minute
	loginGCSweepEvery   = time.Hour
	sessionGCSweepEvery = time.Hour
)

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time
	ttl      time.Duration
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{
		sessions: make(map[string]time.Time),
		ttl:      ttl,
	}
}

func (s *sessionStore) Create() (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}

	token := base64.RawURLEncoding.EncodeToString(tokenBytes)

	s.mu.Lock()
	s.sessions[token] = time.Now()
	s.mu.Unlock()

	return token, nil
}

func (s *sessionStore) Validate(token string) bool {
	if token == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	createdAt, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Since(createdAt) > s.ttl {
		delete(s.sessions, token)
		return false
	}

	return true
}

func (s *sessionStore) Delete(token string) {
	if token == "" {
		return
	}

	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

func (s *sessionStore) sweep() {
	now := time.Now()

	s.mu.Lock()
	for token, createdAt := range s.sessions {
		if now.Sub(createdAt) > s.ttl {
			delete(s.sessions, token)
		}
	}
	s.mu.Unlock()
}

func (s *sessionStore) StartGC(ctx context.Context) {
	ticker := time.NewTicker(sessionGCSweepEvery)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sweep()
			}
		}
	}()
}

type loginRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*loginBucket
}

type loginBucket struct {
	attempts     int
	windowStart  time.Time
	blockedUntil time.Time
}

func newLoginRateLimiter() *loginRateLimiter {
	return &loginRateLimiter{
		buckets: make(map[string]*loginBucket),
	}
}

func (l *loginRateLimiter) Allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, ok := l.buckets[ip]
	if !ok {
		bucket = &loginBucket{}
		l.buckets[ip] = bucket
	}

	if now.Before(bucket.blockedUntil) {
		return false
	}

	if bucket.windowStart.IsZero() || now.Sub(bucket.windowStart) > loginWindow {
		bucket.attempts = 1
		bucket.windowStart = now
		bucket.blockedUntil = time.Time{}
		return true
	}

	bucket.attempts++
	if bucket.attempts > 10 {
		bucket.blockedUntil = now.Add(loginBlockDuration)
		return false
	}

	return true
}

func (l *loginRateLimiter) sweep(now time.Time) {
	l.mu.Lock()
	for ip, bucket := range l.buckets {
		if now.Sub(bucket.windowStart) > loginBucketMaxAge && now.After(bucket.blockedUntil) {
			delete(l.buckets, ip)
		}
	}
	l.mu.Unlock()
}

func (l *loginRateLimiter) StartGC(ctx context.Context) {
	ticker := time.NewTicker(loginGCSweepEvery)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				l.sweep(time.Now())
			}
		}
	}()
}

type authConfig struct {
	Username string
	Password string
}

func (a authConfig) Verify(submittedUsername, submittedPassword string) bool {
	passwordOK := constantTimeMatch(a.Password, submittedPassword)

	if a.Username == "" {
		usernameOK := constantTimeMatch("", submittedUsername)
		return usernameOK && passwordOK
	}

	usernameOK := constantTimeMatch(a.Username, submittedUsername)
	return usernameOK && passwordOK
}

func constantTimeMatch(expected, actual string) bool {
	expectedBytes := []byte(expected)
	actualBytes := []byte(actual)

	maxLen := len(expectedBytes)
	if len(actualBytes) > maxLen {
		maxLen = len(actualBytes)
	}

	paddedExpected := make([]byte, maxLen)
	paddedActual := make([]byte, maxLen)
	copy(paddedExpected, expectedBytes)
	copy(paddedActual, actualBytes)

	lengthMatch := subtle.ConstantTimeEq(int32(len(expectedBytes)), int32(len(actualBytes)))
	valueMatch := subtle.ConstantTimeCompare(paddedExpected, paddedActual)

	return lengthMatch == 1 && valueMatch == 1
}

type loginPageData struct {
	UsernameField bool
	Error         string
	Next          string
}

var loginPageTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Portless Client Login</title>
  <style>
    :root {
      --bg: #FFFBF5;
      --card: #fff;
      --surface: #FFF7ED;
      --border: #E5DDD3;
      --text: #1C1917;
      --muted: #6B5E50;
      --accent: #D97706;
      --accent-hover: #B45309;
      --bad-text: #c92a2a;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 24px 16px;
      background: var(--bg);
      color: var(--text);
      font-family: system-ui, -apple-system, sans-serif;
    }
    .card {
      width: 100%;
      max-width: 360px;
      background: var(--card);
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 24px;
      box-shadow: 0 10px 30px rgba(15, 23, 42, 0.05);
    }
    h1 {
      margin: 0 0 8px;
      font-size: 1.4rem;
      text-align: center;
    }
    p {
      margin: 0 0 20px;
      color: var(--muted);
      text-align: center;
      line-height: 1.45;
    }
    form {
      display: grid;
      gap: 12px;
    }
    label {
      display: block;
      margin-bottom: 6px;
      color: var(--muted);
      font-size: .78rem;
      font-weight: 600;
      letter-spacing: .08em;
      text-transform: uppercase;
    }
    input {
      width: 100%;
      background: var(--surface);
      color: var(--text);
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 10px 12px;
      font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    }
    button {
      border: 0;
      border-radius: 8px;
      padding: 10px 14px;
      cursor: pointer;
      background: var(--accent);
      color: var(--text);
      font-weight: 700;
      transition: background-color .12s ease-in-out;
    }
    button:hover { background: var(--accent-hover); }
    .err {
      margin-bottom: 12px;
      color: var(--bad-text);
      font-size: .9rem;
      text-align: center;
    }
  </style>
</head>
<body>
  <div class="card">
    <h1>Portless Client Login</h1>
    <p>Sign in to view status and manage the local Portless client.</p>
    {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
    <form method="post" action="/login">
      <input type="hidden" name="next" value="{{.Next}}">
      {{if .UsernameField}}
      <div>
        <label for="username">Username</label>
        <input id="username" name="username" type="text" autocomplete="username" required>
      </div>
      {{end}}
      <div>
        <label for="password">Password</label>
        <input id="password" name="password" type="password" autocomplete="current-password" required>
      </div>
      <button type="submit">Sign in</button>
    </form>
  </div>
</body>
</html>`))

func (h *uiHandler) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data := loginPageData{
		UsernameField: h.auth.Username != "",
		Error:         loginErrorMessage(strings.TrimSpace(r.URL.Query().Get("error"))),
		Next:          strings.TrimSpace(r.URL.Query().Get("next")),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := loginPageTmpl.Execute(w, data); err != nil {
		h.logger.Error().Err(err).Msg("render login page")
	}
}

func (h *uiHandler) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.originAllowed(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !h.loginLimiter.Allow(remoteIPKey(r.RemoteAddr), time.Now()) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	if err := r.ParseForm(); err != nil {
		redirectLoginError(w, r, "invalid", "")
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	next := r.FormValue("next")

	if !h.auth.Verify(username, password) {
		redirectLoginError(w, r, "invalid", next)
		return
	}

	token, err := h.sessions.Create()
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, h.sessionCookie(token, r))
	http.Redirect(w, r, safeNextPath(next), http.StatusSeeOther)
}

func (h *uiHandler) handleLogoutPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.originAllowed(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		h.sessions.Delete(cookie.Value)
	}

	http.SetCookie(w, h.expiredSessionCookie(r))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *uiHandler) requireSession(next http.Handler) http.Handler {
	if h.auth.Password == "" {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || !h.sessions.Validate(cookie.Value) {
			if r.Method == http.MethodGet {
				values := url.Values{}
				values.Set("next", r.URL.Path)
				http.Redirect(w, r, "/login?"+values.Encode(), http.StatusSeeOther)
				return
			}

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (h *uiHandler) originAllowed(r *http.Request) bool {
	allowed := h.allowedOrigin
	if origin := r.Header.Get("Origin"); origin != "" {
		return origin == allowed
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		parsed, err := url.Parse(referer)
		if err != nil {
			return false
		}
		return parsed.Scheme+"://"+parsed.Host == allowed
	}
	return false
}

func (h *uiHandler) sessionCookie(token string, r *http.Request) *http.Cookie {
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	}
	if h.sessions != nil && h.sessions.ttl > 0 {
		cookie.Expires = time.Now().Add(h.sessions.ttl)
		cookie.MaxAge = int(h.sessions.ttl / time.Second)
	}
	return cookie
}

func (h *uiHandler) expiredSessionCookie(r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	}
}

func loginErrorMessage(code string) string {
	switch code {
	case "invalid":
		return "Invalid credentials"
	case "rate_limit":
		return "Too many attempts, try again later"
	default:
		return ""
	}
}

func redirectLoginError(w http.ResponseWriter, r *http.Request, errorCode, next string) {
	values := url.Values{}
	values.Set("error", errorCode)
	if strings.TrimSpace(next) != "" {
		values.Set("next", next)
	}
	http.Redirect(w, r, "/login?"+values.Encode(), http.StatusSeeOther)
}

func safeNextPath(next string) string {
	next = strings.TrimSpace(next)
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") {
		return next
	}
	return "/"
}

func remoteIPKey(remoteAddr string) string {
	host := strings.TrimSpace(remoteAddr)
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	return strings.Trim(host, "[]")
}
