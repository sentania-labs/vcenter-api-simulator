package admin

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/sentania/vcenter-api-simulator/internal/control"
)

var errClaimed = errors.New("console already claimed")
var errCredentials = errors.New("incorrect password")
var errRateLimited = errors.New("too many attempts")
var errShort = errors.New("password too short")

const (
	idleTimeout     = 30 * time.Minute
	sessionLifetime = 12 * time.Hour
	pollHeader      = "X-Console-Poll"
	sessionCookie   = "vcsim_console"
	// minPassword is the shortest console password a person may choose. A
	// password supplied by the deployment is taken as it is.
	minPassword = 8
)

type consoleSession struct{ started, active time.Time }

// consoleAuth keeps sessions in memory so a restart invalidates them. The
// password file is linked atomically to make even simultaneous claims safe.
type consoleAuth struct {
	mu       sync.Mutex
	path     string
	sessions map[string]consoleSession
	managed  bool
	failures []time.Time
	now      func() time.Time
	// accountNeeded reports whether first-run set-up still has a second
	// step: choosing the password tools sign in with.
	accountNeeded func() bool
}

func newConsoleAuth(dir, seed string) (*consoleAuth, error) {
	a := &consoleAuth{path: filepath.Join(dir, "console-password"), sessions: make(map[string]consoleSession), now: time.Now, managed: seed != ""}
	if seed != "" {
		err := a.claim(seed)
		if errors.Is(err, errClaimed) {
			stored, readErr := os.ReadFile(a.path)
			if readErr != nil {
				return nil, readErr
			}
			if bcrypt.CompareHashAndPassword(stored, []byte(seed)) != nil {
				if err := a.replace(seed); err != nil {
					return nil, err
				}
				log.Print("console password set from the deployment")
			}
		} else if err != nil {
			return nil, err
		}
	}
	return a, nil
}

func (a *consoleAuth) replace(password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
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
	return os.Rename(f.Name(), a.path)
}

// change replaces the console password with next once current is shown to
// be right. The session that asked stays signed in; every other session
// ends.
func (a *consoleAuth) change(current, next, keep string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.managed {
		return errors.New("the console password is set by the deployment")
	}
	if len([]rune(next)) < minPassword {
		return errShort
	}
	now := a.now()
	recent := a.failures[:0]
	for _, t := range a.failures {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	a.failures = recent
	if len(recent) >= 5 {
		return errRateLimited
	}
	hash, err := os.ReadFile(a.path)
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(current)) != nil {
		a.failures = append(a.failures, now)
		return control.ErrWrongConsolePassword
	}
	if err := a.replace(next); err != nil {
		return err
	}
	for token := range a.sessions {
		if token != keep {
			delete(a.sessions, token)
		}
	}
	return nil
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
	a.sessions[token] = consoleSession{started: now, active: now}
	return token, nil
}

func (a *consoleAuth) valid(token string, activity bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	session, ok := a.sessions[token]
	if !ok {
		return false
	}
	now := a.now()
	if now.Sub(session.active) >= idleTimeout || now.Sub(session.started) >= sessionLifetime {
		delete(a.sessions, token)
		return false
	}
	if activity {
		session.active = now
		a.sessions[token] = session
	}
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
		activity := r.Method != http.MethodGet || r.Header.Get(pollHeader) == ""
		if path == "/api/auth/status" && r.Method == http.MethodGet {
			claimed, err := a.claimed()
			if err != nil {
				http.Error(w, "console authentication unavailable", http.StatusInternalServerError)
				return
			}
			authenticated := false
			if cookie, err := r.Cookie(sessionCookie); err == nil {
				authenticated = a.valid(cookie.Value, activity)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]bool{"claimed": claimed, "authenticated": authenticated, "managed": a.managed,
				"accountNeeded": a.accountNeeded != nil && a.accountNeeded()})
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
				err := errShort
				if len([]rune(input.Password)) >= minPassword {
					err = a.claim(input.Password)
				}
				if err != nil {
					status := http.StatusInternalServerError
					if errors.Is(err, errClaimed) {
						status = http.StatusConflict
					} else if errors.Is(err, errCredentials) || errors.Is(err, errShort) || errors.Is(err, bcrypt.ErrPasswordTooLong) {
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
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil || forceSecure, SameSite: http.SameSiteStrictMode})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		cookie, err := r.Cookie(sessionCookie)
		authed := err == nil && a.valid(cookie.Value, activity)
		if r.Method == http.MethodGet && (path == "/auth.js" || path == "/app.css" || path == "/lattice/tokens.css" || path == "/lattice/lattice.css") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet && (path == "/claim" || path == "/login") {
			if authed {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			claimed, err := a.claimed()
			if err != nil {
				http.Error(w, "console authentication unavailable", http.StatusInternalServerError)
				return
			}
			if claimed && path == "/claim" {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			if !claimed && path == "/login" {
				http.Redirect(w, r, "/claim", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !authed {
			if strings.HasPrefix(path, "/api/") || r.Method != http.MethodGet {
				http.Error(w, "sign in required", http.StatusUnauthorized)
				return
			}
			claimed, err := a.claimed()
			if err != nil {
				http.Error(w, "console authentication unavailable", http.StatusInternalServerError)
				return
			}
			if claimed {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			} else {
				http.Redirect(w, r, "/claim", http.StatusSeeOther)
			}
			return
		}
		if path == "/api/auth/logout" && r.Method == http.MethodPost {
			a.logout(cookie.Value)
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", HttpOnly: true, Secure: r.TLS != nil || forceSecure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
