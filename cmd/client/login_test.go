package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func newTestLoginHandler(auth authConfig, store *sessionStore, limiter *loginRateLimiter) http.Handler {
	return newUIHandler(newTestUIController(), zerolog.Nop(), auth, store, limiter, "127.0.0.1:9090")
}

func newLoginRequest(t *testing.T, target string, form url.Values) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:9090")
	return req
}

func sessionCookieFromResponse(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()

	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			return cookie
		}
	}
	return nil
}

func TestLogin_PasswordOnly_Success(t *testing.T) {
	store := newTestSessionStore()
	handler := newTestLoginHandler(authConfig{Password: "pw"}, store, newLoginRateLimiter())

	req := newLoginRequest(t, "/login", url.Values{"password": {"pw"}})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/" {
		t.Fatalf("location = %q, want %q", got, "/")
	}

	cookie := sessionCookieFromResponse(t, rec)
	if cookie == nil {
		t.Fatalf("missing session cookie")
	}
	if cookie.Name != sessionCookieName {
		t.Fatalf("cookie name = %q, want %q", cookie.Name, sessionCookieName)
	}
	if cookie.Value == "" {
		t.Fatal("cookie value is empty")
	}
	if !cookie.HttpOnly {
		t.Fatal("cookie HttpOnly = false, want true")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie SameSite = %v, want %v", cookie.SameSite, http.SameSiteStrictMode)
	}
	if cookie.Path != "/" {
		t.Fatalf("cookie Path = %q, want %q", cookie.Path, "/")
	}
}

func TestLogin_UsernameAndPassword_Success(t *testing.T) {
	store := newTestSessionStore()
	handler := newTestLoginHandler(authConfig{Username: "u", Password: "pw"}, store, newLoginRateLimiter())

	req := newLoginRequest(t, "/login", url.Values{
		"username": {"u"},
		"password": {"pw"},
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if cookie := sessionCookieFromResponse(t, rec); cookie == nil {
		t.Fatal("missing session cookie")
	}
}

func TestLogin_UsernameSubmittedWhenNotConfigured_Reject(t *testing.T) {
	store := newTestSessionStore()
	handler := newTestLoginHandler(authConfig{Password: "pw"}, store, newLoginRateLimiter())

	req := newLoginRequest(t, "/login", url.Values{
		"username": {"admin"},
		"password": {"pw"},
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/login?error=invalid" {
		t.Fatalf("location = %q, want %q", got, "/login?error=invalid")
	}
	if cookie := sessionCookieFromResponse(t, rec); cookie != nil {
		t.Fatalf("unexpected session cookie: %+v", cookie)
	}
}

func TestLogin_WrongPassword_Reject(t *testing.T) {
	store := newTestSessionStore()
	handler := newTestLoginHandler(authConfig{Password: "pw"}, store, newLoginRateLimiter())

	req := newLoginRequest(t, "/login", url.Values{"password": {"nope"}})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/login?error=invalid" {
		t.Fatalf("location = %q, want %q", got, "/login?error=invalid")
	}
	if cookie := sessionCookieFromResponse(t, rec); cookie != nil {
		t.Fatalf("unexpected session cookie: %+v", cookie)
	}
}

func TestLogin_NoPassword_Reject(t *testing.T) {
	store := newTestSessionStore()
	handler := newTestLoginHandler(authConfig{Password: "pw"}, store, newLoginRateLimiter())

	req := newLoginRequest(t, "/login", url.Values{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/login?error=invalid" {
		t.Fatalf("location = %q, want %q", got, "/login?error=invalid")
	}
	if cookie := sessionCookieFromResponse(t, rec); cookie != nil {
		t.Fatalf("unexpected session cookie: %+v", cookie)
	}
}

func TestLogin_RateLimit_429OnEleventh(t *testing.T) {
	store := newTestSessionStore()
	handler := newTestLoginHandler(authConfig{Password: "pw"}, store, newLoginRateLimiter())

	for i := range 11 {
		req := newLoginRequest(t, "/login", url.Values{"password": {"nope"}})
		req.RemoteAddr = "1.2.3.4:5555"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		wantStatus := http.StatusSeeOther
		if i == 10 {
			wantStatus = http.StatusTooManyRequests
		}
		if rec.Code != wantStatus {
			t.Fatalf("attempt %d status = %d, want %d", i+1, rec.Code, wantStatus)
		}
	}
}

func TestLogin_RateLimitWindowExpires(t *testing.T) {
	limiter := newLoginRateLimiter()
	ip := "1.2.3.4"
	t0 := time.Now()

	for i := 0; i < 10; i++ {
		if !limiter.Allow(ip, t0) {
			t.Fatalf("attempt %d unexpectedly blocked", i+1)
		}
	}
	if limiter.Allow(ip, t0) {
		t.Fatal("11th attempt unexpectedly allowed")
	}
	if !limiter.Allow(ip, t0.Add(6*time.Minute)) {
		t.Fatal("attempt after block expiry was not allowed")
	}
}

func TestSession_MissingCookie_RedirectsToLogin(t *testing.T) {
	handler := newTestLoginHandler(authConfig{Password: "pw"}, newTestSessionStore(), newLoginRateLimiter())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/login?next=%2F" {
		t.Fatalf("location = %q, want %q", got, "/login?next=%2F")
	}
}

func TestSession_ExpiredCookie_RedirectsToLogin(t *testing.T) {
	store := newSessionStore(time.Millisecond)
	handler := newTestLoginHandler(authConfig{Password: "pw"}, store, newLoginRateLimiter())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	addAuthenticatedSession(t, req, store)
	time.Sleep(5 * time.Millisecond)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/login?next=%2F" {
		t.Fatalf("location = %q, want %q", got, "/login?next=%2F")
	}
}

func TestSession_TamperedCookie_Reject(t *testing.T) {
	handler := newTestLoginHandler(authConfig{Password: "pw"}, newTestSessionStore(), newLoginRateLimiter())

	tests := []string{
		"not-a-real-token",
		"!!!invalid base64!!!",
	}

	for _, token := range tests {
		t.Run(token, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
			}
			if got := rec.Header().Get("Location"); got != "/login?next=%2F" {
				t.Fatalf("location = %q, want %q", got, "/login?next=%2F")
			}
		})
	}
}

func TestLogout_ClearsCookieAndRedirects(t *testing.T) {
	store := newTestSessionStore()
	handler := newTestLoginHandler(authConfig{Password: "pw"}, store, newLoginRateLimiter())

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9090")
	addAuthenticatedSession(t, req, store)
	originalCookie := req.Cookies()[0]

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/login" {
		t.Fatalf("location = %q, want %q", got, "/login")
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, sessionCookieName+"=") {
		t.Fatalf("Set-Cookie missing session name: %q", setCookie)
	}
	if !strings.Contains(setCookie, "Max-Age=0") {
		t.Fatalf("Set-Cookie missing Max-Age=0: %q", setCookie)
	}

	followUpReq := httptest.NewRequest(http.MethodGet, "/", nil)
	followUpReq.AddCookie(originalCookie)
	followUpRec := httptest.NewRecorder()
	handler.ServeHTTP(followUpRec, followUpReq)

	if followUpRec.Code != http.StatusSeeOther {
		t.Fatalf("follow-up status = %d, want %d", followUpRec.Code, http.StatusSeeOther)
	}
	if got := followUpRec.Header().Get("Location"); got != "/login?next=%2F" {
		t.Fatalf("follow-up location = %q, want %q", got, "/login?next=%2F")
	}
}

func TestMetrics_NoAuthRequired(t *testing.T) {
	handler := newTestLoginHandler(authConfig{Password: "pw"}, newTestSessionStore(), newLoginRateLimiter())

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestSession_ConcurrentCreate_RaceFree(t *testing.T) {
	store := newTestSessionStore()

	tokens := make([]string, 100)
	var wg sync.WaitGroup

	for i := range len(tokens) {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			token, err := store.Create()
			if err != nil {
				t.Errorf("Create() error: %v", err)
				return
			}
			tokens[idx] = token
		}(i)
	}

	wg.Wait()

	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if token == "" {
			t.Fatal("empty token returned")
		}
		if _, ok := seen[token]; ok {
			t.Fatalf("duplicate token generated: %q", token)
		}
		seen[token] = struct{}{}
		if !store.Validate(token) {
			t.Fatalf("Validate(%q) = false, want true", token)
		}
	}
}

func TestLogin_NextParamPreserved(t *testing.T) {
	store := newTestSessionStore()
	handler := newTestLoginHandler(authConfig{Password: "pw"}, store, newLoginRateLimiter())

	getReq := httptest.NewRequest(http.MethodGet, "/login?next=/settings", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", getRec.Code, http.StatusOK)
	}
	body := getRec.Body.String()
	if !strings.Contains(body, `name="next" value="/settings"`) {
		t.Fatalf("GET body missing next field: %q", body)
	}

	postReq := newLoginRequest(t, "/login?next=/settings", url.Values{"password": {"pw"}})
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST status = %d, want %d", postRec.Code, http.StatusSeeOther)
	}
	if got := postRec.Header().Get("Location"); got != "/settings" {
		t.Fatalf("location = %q, want %q", got, "/settings")
	}

	evilReq := newLoginRequest(t, "/login?next=//evil.com/x", url.Values{"password": {"pw"}})
	evilRec := httptest.NewRecorder()
	handler.ServeHTTP(evilRec, evilReq)

	if evilRec.Code != http.StatusSeeOther {
		t.Fatalf("evil status = %d, want %d", evilRec.Code, http.StatusSeeOther)
	}
	if got := evilRec.Header().Get("Location"); got != "/" {
		t.Fatalf("evil location = %q, want %q", got, "/")
	}
}

func TestLogin_CSRF_RejectBadOrigin(t *testing.T) {
	handler := newTestLoginHandler(authConfig{Password: "pw"}, newTestSessionStore(), newLoginRateLimiter())

	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"password": {"pw"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}
