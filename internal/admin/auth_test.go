package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConsoleAuthClaimAndSessions(t *testing.T) {
	dir := t.TempDir()
	a, err := newConsoleAuth(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := a.claimed(); err != nil || claimed {
		t.Fatalf("fresh claim: %v %v", claimed, err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- a.claim("test-password") }()
	}
	wg.Wait()
	close(results)
	successes, refused := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, errClaimed) {
			refused++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || refused != 1 {
		t.Fatalf("claim results: %d %d", successes, refused)
	}
	info, err := os.Stat(a.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("password permissions: %v %v", info, err)
	}
	if _, err := a.login("incorrect"); !errors.Is(err, errCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	token, err := a.login("test-password")
	if err != nil || !a.valid(token) {
		t.Fatalf("login: %v", err)
	}
	restarted, err := newConsoleAuth(dir, "other-password")
	if err != nil {
		t.Fatal(err)
	}
	if restarted.valid(token) {
		t.Fatal("session survived restart")
	}
	if _, err := restarted.login("test-password"); err != nil {
		t.Fatalf("claim did not persist: %v", err)
	}
	now := time.Now()
	a.now = func() time.Time { return now.Add(31 * time.Minute) }
	if a.valid(token) {
		t.Fatal("idle session survived")
	}
}

func TestConsoleAuthSeedAndRateLimit(t *testing.T) {
	a, err := newConsoleAuth(t.TempDir(), "seed-password")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := a.claimed(); err != nil || !claimed {
		t.Fatalf("seed: %v %v", claimed, err)
	}
	for i := 0; i < 5; i++ {
		if _, err := a.login("wrong"); !errors.Is(err, errCredentials) {
			t.Fatal(err)
		}
	}
	if _, err := a.login("seed-password"); !errors.Is(err, errRateLimited) {
		t.Fatalf("rate limit: %v", err)
	}
	now := time.Now()
	a.now = func() time.Time { return now.Add(time.Minute) }
	token, err := a.login("seed-password")
	if err != nil {
		t.Fatal(err)
	}
	a.logout(token)
	if a.valid(token) {
		t.Fatal("logout did not revoke session")
	}
}

func TestConsoleAuthHTTP(t *testing.T) {
	a, err := newConsoleAuth(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	h := a.protectConsole(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://localhost"+path, strings.NewReader(body))
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/api/state", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unclaimed API: %d", w.Code)
	}
	w := request("POST", "/api/auth/claim", `{"password":"test-password"}`, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("claim: %d", w.Code)
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie flags: %+v", cookie)
	}
	if w := request("POST", "/api/auth/claim", `{"password":"test-password"}`, nil); w.Code != http.StatusConflict {
		t.Fatalf("second claim: %d", w.Code)
	}
	if w := request("GET", "/api/state", "", cookie); w.Code != http.StatusNoContent {
		t.Fatalf("authenticated API: %d", w.Code)
	}
	if w := request("POST", "/api/auth/logout", "", cookie); w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := request("GET", "/api/state", "", cookie); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session: %d", w.Code)
	}
}
