// Package admin serves the control console: a JSON API over package control
// and the embedded Lattice UI. It is a separate plane from the simulated
// vCenter: it listens on its own address (loopback by default), never on the
// API listeners, and shares nothing with the simulator's login.
package admin

import (
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/control"
)

//go:embed ui
var uiFiles embed.FS

// done is the answer to an action that happens at once: the words for the
// green line, and whether they are good news (ok) or a warning (warn).
type done struct {
	Done string `json:"done"`
	Tone string `json:"tone"`
}

func ok(text string, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return done{text, "ok"}, nil
}

// Handler returns the admin plane's handler. Requests are served only for the
// Host names localhost, 127.0.0.1 and ::1 (any port) and for allowedHosts.
func Handler(c *control.Controller, allowedHosts []string) http.Handler {
	a := &api{c: c}
	mux := http.NewServeMux()
	get := func(p string, h func(*http.Request) (any, error)) { mux.HandleFunc("GET "+p, a.wrap(h)) }
	send := func(m, p string, h func(*http.Request) (any, error)) { mux.HandleFunc(m+" "+p, a.wrap(h)) }
	cluster := func(r *http.Request) string { return r.PathValue("dc") + "/" + r.PathValue("name") }

	// Home and the shell around every screen
	get("/api/home", func(r *http.Request) (any, error) { return c.Home(), nil })
	get("/api/history", func(r *http.Request) (any, error) { return c.History(), nil })
	send("POST", "/api/check-address", func(r *http.Request) (any, error) {
		works, text := c.CheckAddress()
		return map[string]any{"ok": works, "text": text}, nil
	})
	send("POST", "/api/pause", func(r *http.Request) (any, error) {
		var req struct {
			Paused bool `json:"paused"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		c.SetPaused(req.Paused)
		return map[string]bool{"paused": c.Paused()}, nil
	})
	send("POST", "/api/failure/dismiss", func(r *http.Request) (any, error) {
		c.DismissFailure()
		return map[string]bool{"ok": true}, nil
	})
	send("POST", "/api/setup/account", func(r *http.Request) (any, error) {
		var req struct {
			Password string `json:"password"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, c.SetupAccount(req.Password)
	})

	// tasks: every change the console makes
	get("/api/task/{kind}", func(r *http.Request) (any, error) {
		q := map[string]string{}
		for k, v := range r.URL.Query() {
			if len(v) > 0 {
				q[k] = v[0]
			}
		}
		return c.TaskForm(r.PathValue("kind"), q, caller(r))
	})
	send("POST", "/api/task/{kind}/preview", func(r *http.Request) (any, error) {
		var req struct {
			Params json.RawMessage `json:"params"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return c.TaskPreview(r.PathValue("kind"), req.Params, caller(r))
	})
	send("POST", "/api/task/{kind}", func(r *http.Request) (any, error) {
		var req struct {
			Params    json.RawMessage `json:"params"`
			At        *time.Time      `json:"at"`
			Confirmed bool            `json:"confirmed"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return c.TaskRun(r.PathValue("kind"), req.Params, req.At, req.Confirmed, caller(r))
	})
	send("POST", "/api/scheduled/{id}/now", func(r *http.Request) (any, error) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		return c.ScheduledNow(id)
	})
	send("DELETE", "/api/scheduled/{id}", func(r *http.Request) (any, error) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		return ok(c.ScheduledCancel(id))
	})

	// clusters, storage, versions
	get("/api/clusters", func(r *http.Request) (any, error) { return c.Clusters(), nil })
	get("/api/clusters/{dc}/{name}", func(r *http.Request) (any, error) { return c.Cluster(cluster(r)) })
	send("DELETE", "/api/clusters/{dc}/{name}", func(r *http.Request) (any, error) { return ok(c.RemoveCluster(cluster(r))) })
	get("/api/storage", func(r *http.Request) (any, error) { return c.Storage(), nil })
	get("/api/storage/{dc}/{name}", func(r *http.Request) (any, error) { return c.StorageItem(cluster(r)) })
	send("DELETE", "/api/storage/{dc}/{name}", func(r *http.Request) (any, error) { return ok(c.RemoveStorage(cluster(r))) })
	send("POST", "/api/storage/{dc}/{name}/reconnect", func(r *http.Request) (any, error) { return ok(c.ReconnectStorage(cluster(r))) })
	get("/api/versions", func(r *http.Request) (any, error) { return c.Versions(), nil })

	// users and security
	get("/api/users", func(r *http.Request) (any, error) { return c.Users(), nil })
	send("DELETE", "/api/users", func(r *http.Request) (any, error) {
		var req struct {
			Name string `json:"name"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		text, tone, err := c.RemoveUser(req.Name)
		if err != nil {
			return nil, err
		}
		return done{text, tone}, nil
	})
	send("PUT", "/api/users/any-login", func(r *http.Request) (any, error) {
		var req struct {
			On bool `json:"on"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		text, tone, err := c.SetAnyLogin(req.On)
		if err != nil {
			return nil, err
		}
		return done{text, tone}, nil
	})
	get("/api/security", func(r *http.Request) (any, error) { return c.Security(), nil })

	// slowness and faults
	get("/api/faults", func(r *http.Request) (any, error) { return c.Faults(), nil })
	send("DELETE", "/api/faults/{id}", func(r *http.Request) (any, error) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil || id < 0 {
			return nil, &control.Invalid{Msg: "That is not something that can be turned off."}
		}
		return ok(c.ConditionOff(id))
	})

	// requests
	get("/api/requests", func(r *http.Request) (any, error) { return c.Requests(), nil })
	get("/api/requests/log", func(r *http.Request) (any, error) { return c.RequestLog(), nil })
	get("/api/requests/call/{seq}", func(r *http.Request) (any, error) {
		seq, _ := strconv.ParseUint(r.PathValue("seq"), 10, 64)
		return c.RequestBody(seq)
	})
	get("/api/requests/supported", func(r *http.Request) (any, error) {
		rows, vcenter := c.Supported()
		return map[string]any{"rows": rows, "vcenter": vcenter}, nil
	})
	mux.HandleFunc("GET /api/requests/log.csv", func(w http.ResponseWriter, r *http.Request) {
		download(w, "text/csv; charset=utf-8", "vcsim-requests", "csv")
		out := csv.NewWriter(w)
		_ = out.Write([]string{"Time", "Asked by", "What was asked for", "Name tools use", "Cluster", "What happened", "Took (ms)"})
		for _, row := range c.RequestLog() {
			_ = out.Write([]string{row.Time.Format(time.RFC3339), row.Who, row.T, row.Tech, row.Cluster, row.Label, strconv.FormatFloat(row.Ms, 'f', 1, 64)})
		}
		out.Flush()
	})
	send("PUT", "/api/capture", func(r *http.Request) (any, error) {
		var req struct {
			Kind string `json:"kind"`
			On   bool   `json:"on"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return ok(c.SetCapture(req.Kind, req.On))
	})
	send("DELETE", "/api/capture", func(r *http.Request) (any, error) { return ok(c.ClearCapture()) })
	for _, kind := range []string{"all", "unanswered"} {
		kind := kind
		mux.HandleFunc("GET /api/capture/"+kind, func(w http.ResponseWriter, r *http.Request) {
			if c.Capture == nil {
				http.NotFound(w, r)
				return
			}
			// one request per line; a plain text file is what the project's
			// issue page takes as an attachment
			download(w, "text/plain; charset=utf-8", "vcsim-"+kind+"-requests", "txt")
			_ = c.Capture.WriteJSONL(w, kind == "unanswered")
		})
	}

	for _, path := range []string{"/claim", "/login"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			content, _ := uiFiles.ReadFile("ui/auth.html")
			_, _ = w.Write(content)
		})
	}
	sub, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET /", noCache(http.FileServerFS(sub)))
	return guard(mux, allowedHosts)
}

// download sets the headers of a file the browser should save, named with
// today's date.
func download(w http.ResponseWriter, contentType, name, ext string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	// The console names the saved file itself, with the viewer's own date;
	// the name here is for anything else that fetches the address.
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.%s", name, ext))
}

// caller says who is using the console: its session, and the name the
// console was opened with.
func caller(r *http.Request) control.Caller {
	who := control.Caller{Host: r.Host}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		who.Session = cookie.Value
	}
	return who
}

// ProtectedHandler requires a console session and keeps host and origin checks
// on the authentication routes as well as the rest of the console.
func ProtectedHandler(c *control.Controller, hosts []string, dir, seed string, secureCookie bool) (http.Handler, error) {
	a, err := newConsoleAuth(dir, seed)
	if err != nil {
		return nil, err
	}
	c.ConsolePassword = func(current, next, session string) error {
		switch err := a.change(current, next, session); {
		case errors.Is(err, errRateLimited):
			return &control.Invalid{Msg: "Too many wrong passwords. Wait a minute and try again."}
		case errors.Is(err, errShort):
			return &control.Invalid{Msg: "Use at least 8 characters."}
		default:
			return err
		}
	}
	c.ConsoleManaged = func() bool { return a.managed }
	a.accountNeeded = c.AccountNeeded
	return guard(a.protectConsole(Handler(c, hosts), secureCookie), hosts), nil
}

type api struct{ c *control.Controller }

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	if err := dec.Decode(v); err != nil {
		return &control.Invalid{Msg: "The console sent something the simulator could not read. Reload the page and try again."}
	}
	return nil
}

func (a *api) wrap(h func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Until first-run set-up has chosen the tools' password the console
		// shows that step and nothing else, so nothing else is served.
		if a.c.AccountNeeded() && r.URL.Path != "/api/home" && r.URL.Path != "/api/setup/account" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "Choose the password your tools will sign in with first."})
			return
		}
		v, err := h(r)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			var inv *control.Invalid
			code := http.StatusInternalServerError
			msg := "The simulator could not do that: " + err.Error()
			if errors.As(err, &inv) {
				code, msg = http.StatusUnprocessableEntity, inv.Msg
			} else if errors.Is(err, control.ErrWrongConsolePassword) {
				code, msg = http.StatusUnprocessableEntity, "That is not the current password."
			}
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(map[string]string{"error": msg})
			return
		}
		json.NewEncoder(w).Encode(v)
	}
}

// hostName is the name part of a Host header value, lower-cased, without port
// or brackets.
func hostName(hostport string) string {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = strings.Trim(hostport, "[]")
	}
	return strings.ToLower(h)
}

// guard refuses requests that were not addressed to the console by a name the
// operator chose (a DNS-rebinding page arrives under its own name), and
// cross-site writes: a state-changing request must be JSON (which a browser
// only sends cross-origin after a preflight this server never grants) and,
// when it names an Origin, must come from the page's own host.
func guard(next http.Handler, allowedHosts []string) http.Handler {
	allowed := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	for _, h := range allowedHosts {
		if h = hostName(strings.TrimSpace(h)); h != "" {
			allowed[h] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		if !allowed[hostName(r.Host)] {
			http.Error(w, `{"error":"host not allowed; add it to -admin-allowed-hosts"}`, http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, `{"error":"cross-origin request refused"}`, http.StatusForbidden)
					return
				}
			}
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") && r.ContentLength != 0 {
				http.Error(w, `{"error":"content type must be application/json"}`, http.StatusUnsupportedMediaType)
				return
			}
			if r.ContentLength == 0 && r.Header.Get("X-Console") == "" {
				http.Error(w, `{"error":"missing X-Console header"}`, http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}
