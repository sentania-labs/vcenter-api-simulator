// Package admin serves the control console: a JSON API over package control
// and the embedded Lattice UI. It is a separate plane from the simulated
// vCenter: it listens on its own address (loopback by default), never on the
// API listeners, and shares nothing with the simulator's login.
package admin

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sentania/vcenter-api-simulator/internal/activity"
	"github.com/sentania/vcenter-api-simulator/internal/control"
	"github.com/sentania/vcenter-api-simulator/internal/querylog"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

//go:embed ui
var uiFiles embed.FS

// Handler returns the admin plane's handler. Requests are served only for the
// Host names localhost, 127.0.0.1 and ::1 (any port) and for allowedHosts.
func Handler(c *control.Controller, allowedHosts []string, captures ...*querylog.Capture) http.Handler {
	a := &api{c: c}
	mux := http.NewServeMux()
	get := func(p string, h func(*http.Request) (any, error)) { mux.HandleFunc("GET "+p, a.wrap(h)) }
	send := func(m, p string, h func(*http.Request) (any, error)) { mux.HandleFunc(m+" "+p, a.wrap(h)) }

	get("/api/state", func(r *http.Request) (any, error) { return c.State(), nil })
	get("/api/settings", func(r *http.Request) (any, error) { return c.RuntimeSettings(), nil })
	get("/api/certificate", func(r *http.Request) (any, error) { return c.CertificateInfo() })
	send("POST", "/api/certificate/regenerate", func(r *http.Request) (any, error) {
		var req struct {
			Names []string `json:"names"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return c.RegenerateCertificate(req.Names)
	})
	send("PUT", "/api/certificate", func(r *http.Request) (any, error) {
		var req struct {
			Certificate string `json:"certificate"`
			PrivateKey  string `json:"privateKey"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return c.UploadCertificate(req.Certificate, req.PrivateKey)
	})
	send("PUT", "/api/settings", func(r *http.Request) (any, error) {
		var req control.RuntimeSettings
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return c.SetRuntimeSettings(req)
	})
	get("/api/credentials", func(r *http.Request) (any, error) { return c.CredentialsView(), nil })
	send("PUT", "/api/credentials/user", func(r *http.Request) (any, error) {
		var req struct {
			Name     string `json:"name"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return c.PutUser(req.Name, req.Password, req.Role)
	})
	send("DELETE", "/api/credentials/user", func(r *http.Request) (any, error) {
		var req struct {
			Name string `json:"name"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return c.DeleteUser(req.Name)
	})
	send("PUT", "/api/credentials/mode", func(r *http.Request) (any, error) {
		var req struct {
			AnyLogin bool `json:"anyLogin"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return c.SetAnyLogin(req.AnyLogin)
	})
	get("/api/tree", func(r *http.Request) (any, error) { return c.Tree(), nil })
	get("/api/clusters/{dc}/{name}", func(r *http.Request) (any, error) {
		return c.Cluster(r.PathValue("dc") + "/" + r.PathValue("name"))
	})
	get("/api/draft", func(r *http.Request) (any, error) { return c.DraftState(), nil })
	send("PUT", "/api/draft", func(r *http.Request) (any, error) {
		var req struct {
			Scenario json.RawMessage `json:"scenario"`
			Scope    string          `json:"scope"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return c.SetDraft(req.Scenario, req.Scope)
	})
	send("DELETE", "/api/draft", func(r *http.Request) (any, error) { return c.DiscardDraft(), nil })
	send("POST", "/api/draft/generate", func(r *http.Request) (any, error) {
		var p control.GenParams
		if err := decode(r, &p); err != nil {
			return nil, err
		}
		return c.Generate(p)
	})
	send("POST", "/api/preview/generate", func(r *http.Request) (any, error) {
		var p control.GenParams
		if err := decode(r, &p); err != nil {
			return nil, err
		}
		return c.PreviewGenerate(p)
	})
	get("/api/presets", func(r *http.Request) (any, error) { return c.Presets(), nil })
	get("/api/version-presets", func(r *http.Request) (any, error) {
		vcenter, esx := scenario.VersionPresets()
		return map[string]any{"vcenter": vcenter, "esx": esx}, nil
	})
	send("POST", "/api/draft/preset", func(r *http.Request) (any, error) {
		var req struct {
			Name string `json:"name"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return c.ApplyPreset(req.Name)
	})
	for path, act := range map[string]func(string) (control.DraftView, error){
		"regenerate": c.Regenerate, "reset-seed": c.ResetSeed, "delete": c.DeleteCluster,
	} {
		act := act
		send("POST", "/api/draft/"+path, func(r *http.Request) (any, error) {
			var req struct {
				Cluster string `json:"cluster"`
			}
			if err := decode(r, &req); err != nil {
				return nil, err
			}
			return act(req.Cluster)
		})
	}
	get("/api/review", func(r *http.Request) (any, error) { return c.Review() })
	send("POST", "/api/apply", func(r *http.Request) (any, error) {
		var req struct {
			When string `json:"when"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return c.Apply(req.When)
	})
	send("POST", "/api/error/dismiss", func(r *http.Request) (any, error) { c.DismissError(); return map[string]bool{"ok": true}, nil })
	send("POST", "/api/apply/cancel", func(r *http.Request) (any, error) { return c.CancelScheduled() })
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
	get("/api/timeline", func(r *http.Request) (any, error) { return c.Timeline(), nil })
	get("/api/families", func(r *http.Request) (any, error) { return c.Families(), nil })
	get("/api/compat", func(r *http.Request) (any, error) { return c.Compatibility(), nil })
	get("/api/activity", func(r *http.Request) (any, error) {
		q := r.URL.Query()
		after, _ := strconv.ParseUint(q.Get("after"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		calls := c.Log.Query(activity.Filter{Surface: q.Get("surface"), Method: q.Get("method"), Family: q.Get("family"),
			Cluster: q.Get("cluster"), Result: q.Get("result"), AfterSeq: after, Limit: limit})
		if calls == nil {
			calls = []activity.Call{}
		}
		return map[string]any{"calls": calls, "totals": c.Log.Totals()}, nil
	})
	get("/api/activity/{seq}", func(r *http.Request) (any, error) {
		seq, _ := strconv.ParseUint(r.PathValue("seq"), 10, 64)
		call, ok := c.Log.Get(seq)
		if !ok {
			return nil, &control.Invalid{Msg: "that request has left the log"}
		}
		return call, nil
	})

	if len(captures) > 0 && captures[0] != nil {
		capture := captures[0]
		get("/api/capture", func(r *http.Request) (any, error) { return capture.State(), nil })
		send("PUT", "/api/capture", func(r *http.Request) (any, error) {
			var settings querylog.Settings
			if err := decode(r, &settings); err != nil {
				return nil, err
			}
			if err := capture.Set(settings); err != nil {
				return nil, err
			}
			return capture.State(), nil
		})
		send("DELETE", "/api/capture", func(r *http.Request) (any, error) {
			if err := capture.Clear(); err != nil {
				return nil, err
			}
			return capture.State(), nil
		})
		get("/api/capture/unanswered", func(r *http.Request) (any, error) { return capture.Records(true), nil })
		get("/api/capture/full", func(r *http.Request) (any, error) { return capture.Records(false), nil })
		for _, kind := range []string{"full", "unanswered"} {
			kind := kind
			mux.HandleFunc("GET /api/capture/"+kind+".jsonl", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Content-Disposition", "attachment; filename="+kind+"-requests.jsonl")
				_ = capture.WriteJSONL(w, kind == "unanswered")
			})
		}
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

// ProtectedHandler requires a console session and keeps host and origin checks
// on the authentication routes as well as the rest of the console.
func ProtectedHandler(c *control.Controller, hosts []string, dir, seed string, secureCookie bool, capture *querylog.Capture) (http.Handler, error) {
	a, err := newConsoleAuth(dir, seed)
	if err != nil {
		return nil, err
	}
	return guard(a.protectConsole(Handler(c, hosts, capture), secureCookie), hosts), nil
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
		return &control.Invalid{Msg: "bad request body: " + err.Error()}
	}
	return nil
}

func (a *api) wrap(h func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := h(r)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			var inv *control.Invalid
			code := http.StatusInternalServerError
			switch {
			case errors.As(err, &inv):
				code = http.StatusUnprocessableEntity
			case errors.Is(err, control.ErrNoDraft):
				code = http.StatusConflict
			}
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
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
