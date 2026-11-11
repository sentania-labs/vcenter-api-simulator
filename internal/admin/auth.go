package admin

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var errClaimed = errors.New("console already claimed")
var errCredentials = errors.New("incorrect password")
var errRateLimited = errors.New("too many attempts")

// consoleAuth keeps sessions in memory so a restart invalidates them. The
// password file is linked atomically to make even simultaneous claims safe.
type consoleAuth struct {
	mu       sync.Mutex
	path     string
	sessions map[string]time.Time
	failures []time.Time
	now      func() time.Time
}

func newConsoleAuth(dir, seed string) (*consoleAuth, error) {
	a := &consoleAuth{path: filepath.Join(dir, "console-password"), sessions: make(map[string]time.Time), now: time.Now}
	if seed != "" {
		if err := a.claim(seed); err != nil && !errors.Is(err, errClaimed) {
			return nil, err
		}
	}
	return a, nil
}

func (a *consoleAuth) claimed() (bool, error) {
	_, err := os.Stat(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (a *consoleAuth) claim(password string) error {
	if password == "" {
		return errCredentials
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(a.path), ".console-password-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(hash); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Link(f.Name(), a.path); errors.Is(err, os.ErrExist) {
		return errClaimed
	} else if err != nil {
		return err
	}
	return nil
}

func (a *consoleAuth) login(password string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	recent := a.failures[:0]
	for _, t := range a.failures {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	a.failures = recent
	if len(recent) >= 5 {
		return "", errRateLimited
	}
	hash, err := os.ReadFile(a.path)
	if err != nil {
		return "", err
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		a.failures = append(a.failures, now)
		return "", errCredentials
	}
	a.failures = nil
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	a.sessions[token] = now
	return token, nil
}

func (a *consoleAuth) valid(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.sessions[token]
	if !ok {
		return false
	}
	now := a.now()
	if now.Sub(t) >= 30*time.Minute {
		delete(a.sessions, token)
		return false
	}
	a.sessions[token] = now
	return true
}

func (a *consoleAuth) logout(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

// protectConsole exposes authentication operations and protects the console API.
// The host and origin guard must wrap this handler as well as its child.
func (a *consoleAuth) protectConsole(next http.Handler, secureProxy ...bool) http.Handler {
	forceSecure := len(secureProxy) > 0 && secureProxy[0]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		path := r.URL.Path
		if path == "/api/auth/status" && r.Method == http.MethodGet {
			claimed, err := a.claimed()
			if err != nil {
				http.Error(w, "console authentication unavailable", http.StatusInternalServerError)
				return
			}
			authenticated := false
			if cookie, err := r.Cookie("vcsim_console"); err == nil {
				authenticated = a.valid(cookie.Value)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]bool{"claimed": claimed, "authenticated": authenticated})
			return
		}
		if (path == "/api/auth/claim" || path == "/api/auth/login") && r.Method == http.MethodPost {
			var input struct {
				Password string `json:"password"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input); err != nil {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if path == "/api/auth/claim" {
				if err := a.claim(input.Password); err != nil {
					status := http.StatusInternalServerError
					if errors.Is(err, errClaimed) {
						status = http.StatusConflict
					} else if errors.Is(err, errCredentials) || errors.Is(err, bcrypt.ErrPasswordTooLong) {
						status = http.StatusBadRequest
					}
					http.Error(w, "unable to claim console", status)
					return
				}
			}
			token, err := a.login(input.Password)
			if err != nil {
				status := http.StatusUnauthorized
				if errors.Is(err, errRateLimited) {
					status = http.StatusTooManyRequests
				}
				http.Error(w, "unable to sign in", status)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "vcsim_console", Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil || forceSecure, SameSite: http.SameSiteStrictMode})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		cookie, err := r.Cookie("vcsim_console")
		if err != nil || !a.valid(cookie.Value) {
			http.Error(w, "sign in required", http.StatusUnauthorized)
			return
		}
		if path == "/api/auth/logout" && r.Method == http.MethodPost {
			a.logout(cookie.Value)
			http.SetCookie(w, &http.Cookie{Name: "vcsim_console", Path: "/", HttpOnly: true, Secure: r.TLS != nil || forceSecure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
