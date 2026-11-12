package admin

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPassword() string { return uuid.NewString() }

func TestConsoleAuthClaimAndSessions(t *testing.T) {
	password := testPassword()
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
		go func() { defer wg.Done(); results <- a.claim(password) }()
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
	token, err := a.login(password)
	if err != nil || !a.valid(token, true) {
		t.Fatalf("login: %v", err)
	}
	restarted, err := newConsoleAuth(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if restarted.valid(token, true) {
		t.Fatal("session survived restart")
	}
	if _, err := restarted.login(password); err != nil {
		t.Fatalf("claim did not persist: %v", err)
	}
	now := time.Now()
	a.now = func() time.Time { return now.Add(31 * time.Minute) }
	if a.valid(token, true) {
		t.Fatal("idle session survived")
	}
}

func TestConsoleAuthSeedAndRateLimit(t *testing.T) {
	password := testPassword()
	a, err := newConsoleAuth(t.TempDir(), password)
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
	if _, err := a.login(password); !errors.Is(err, errRateLimited) {
		t.Fatalf("rate limit: %v", err)
	}
	now := time.Now()
	a.now = func() time.Time { return now.Add(time.Minute) }
	token, err := a.login(password)
	if err != nil {
		t.Fatal(err)
	}
	a.logout(token)
	if a.valid(token, true) {
		t.Fatal("logout did not revoke session")
	}
}

func TestConsoleAuthHTTP(t *testing.T) {
	password := testPassword()
	data, _ := json.Marshal(map[string]string{"password": password})
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
	w := request("POST", "/api/auth/claim", string(data), nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("claim: %d", w.Code)
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("incorrect cookie security flags")
	}
	if w := request("POST", "/api/auth/claim", string(data), nil); w.Code != http.StatusConflict {
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

func TestConsoleAuthPollingIsNotActivityAndLifetimeIsAbsolute(t *testing.T) {
	password := testPassword()
	a, err := newConsoleAuth(t.TempDir(), password)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	a.now = func() time.Time { return start }
	token, err := a.login(password)
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return start.Add(20 * time.Minute) }
	if !a.valid(token, false) {
		t.Fatal("session expired early")
	}
	a.now = func() time.Time { return start.Add(31 * time.Minute) }
	if a.valid(token, true) {
		t.Fatal("background polling kept an idle session alive")
	}

	a.now = func() time.Time { return start }
	token, _ = a.login(password)
	for elapsed := 10 * time.Minute; elapsed < sessionLifetime; elapsed += 10 * time.Minute {
		at := start.Add(elapsed)
		a.now = func() time.Time { return at }
		if !a.valid(token, true) {
			t.Fatalf("active session ended after %v", elapsed)
		}
	}
	a.now = func() time.Time { return start.Add(sessionLifetime + time.Minute) }
	if a.valid(token, true) {
		t.Fatal("session outlived the absolute lifetime")
	}
}

func TestConsoleAuthDeploymentPasswordWinsOnRotation(t *testing.T) {
	dir := t.TempDir()
	oldPassword, newPassword := testPassword(), testPassword()
	first, err := newConsoleAuth(dir, oldPassword)
	if err != nil {
		t.Fatal(err)
	}
	token, err := first.login(oldPassword)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := newConsoleAuth(dir, newPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !rotated.managed {
		t.Fatal("deployment password not reported as managed")
	}
	if rotated.valid(token, true) {
		t.Fatal("session survived password rotation")
	}
	if _, err := rotated.login(oldPassword); !errors.Is(err, errCredentials) {
		t.Fatalf("old password still works: %v", err)
	}
	if _, err := rotated.login(newPassword); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
	unmanaged, err := newConsoleAuth(dir, "")
	if err != nil || unmanaged.managed {
		t.Fatalf("unmanaged restart: %v", err)
	}
	if _, err := unmanaged.login(newPassword); err != nil {
		t.Fatalf("rotated password not persisted: %v", err)
	}
}

func TestConsolePollRequestsDoNotRefreshIdleTimer(t *testing.T) {
	password := testPassword()
	a, err := newConsoleAuth(t.TempDir(), password)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	a.now = func() time.Time { return start }
	h := a.protectConsole(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	token, _ := a.login(password)
	get := func(at time.Duration, poll bool) int {
		a.now = func() time.Time { return start.Add(at) }
		r := httptest.NewRequest("GET", "https://localhost/api/state", nil)
		r.AddCookie(&http.Cookie{Name: "vcsim_console", Value: token})
		if poll {
			r.Header.Set(pollHeader, "1")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for at := 10 * time.Minute; at <= 25*time.Minute; at += 5 * time.Minute {
		if code := get(at, true); code != http.StatusNoContent {
			t.Fatalf("poll at %v: %d", at, code)
		}
	}
	if code := get(31*time.Minute, true); code != http.StatusUnauthorized {
		t.Fatalf("polling extended an idle session: %d", code)
	}
}
