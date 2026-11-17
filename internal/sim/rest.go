package sim

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/vmware/govmomi/simulator"
)

// legacySessionPath is the CIS REST spelling used by the Operations collector.
const legacySessionPath = "/rest/com/vmware/cis/session"

// saltPath is the appliance salt minion configuration, answered with made-up values.
const saltPath = "/api/appliance/salt/minion/configuration"

// restSessions holds local read-only API sessions. It shares the scenario's
// credential policy with SOAP but never stores a password in a session.
type restSession struct {
	user string
	last time.Time
}
type restSessions struct {
	mu          sync.Mutex
	tokens      map[string]restSession
	stats       map[string]*statsQuery
	issued      map[string]issuedBearer
	credentials map[string]string
	check       func(string, string) bool
	bearer      func(string) (string, bool)
	exchange    func(string, map[string]any) (any, string)
	access      *sync.RWMutex
	timeout     *atomic.Int64
	registry    *simulator.Registry
	catalog     *catalog.Catalog
}

func (s *restSessions) authorized(user, password string) bool {
	if user == "" || password == "" {
		return false
	}
	if s.check != nil {
		return s.check(user, password)
	}
	if len(s.credentials) == 0 {
		return true
	}
	expected, ok := s.credentials[user]
	return ok && subtle.ConstantTimeCompare([]byte(expected), []byte(password)) == 1
}

// valid reports whether a REST session token is a live session, and refreshes
// its idle timestamp, so a read served outside rest.serve keeps the session
// alive the way any other authenticated REST request does.
func (s *restSessions) valid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.tokens[token]
	if !ok {
		return false
	}
	if s.timeout != nil {
		if timeout := time.Duration(s.timeout.Load()); timeout > 0 && time.Since(session.last) >= timeout {
			delete(s.tokens, token)
			return false
		}
	}
	session.last = time.Now()
	s.tokens[token] = session
	return true
}

func (s *restSessions) serve(w http.ResponseWriter, r *http.Request) bool {
	legacy := r.URL.Path == legacySessionPath
	fail := func(status int, kind string) {
		if legacy {
			legacyError(w, status, kind)
		} else {
			restError(w, status, kind)
		}
	}
	if legacy {
		r = r.Clone(r.Context())
		u := *r.URL
		u.Path = "/api/session"
		r.URL = &u
	}
	if r.URL.Path == "/stats" {
		return s.serveRPC(w, r)
	}
	if r.URL.Path != "/api" && !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	if r.URL.Path == "/api" {
		return s.serveRPC(w, r)
	}
	if r.URL.Path == "/api/session" && r.Method == http.MethodPost {
		user, password, ok := r.BasicAuth()
		if !ok || !s.authorized(user, password) {
			fail(http.StatusUnauthorized, "UNAUTHENTICATED")
			return true
		}
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			fail(http.StatusInternalServerError, "INTERNAL_SERVER_ERROR")
			return true
		}
		token := hex.EncodeToString(b[:])
		s.mu.Lock()
		s.tokens[token] = restSession{user: user, last: time.Now()}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if legacy {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"value": token})
		} else {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(token)
		}
		return true
	}
	token := r.Header.Get("vmware-api-session-id")
	s.mu.Lock()
	session, valid := s.tokens[token]
	if valid && s.timeout != nil {
		if timeout := time.Duration(s.timeout.Load()); timeout > 0 && time.Since(session.last) >= timeout {
			delete(s.tokens, token)
			valid = false
		}
	}
	if valid {
		session.last = time.Now()
		s.tokens[token] = session
	}
	if valid && r.URL.Path == "/api/session" && r.Method == http.MethodDelete {
		delete(s.tokens, token)
	}
	s.mu.Unlock()
	if !valid {
		if r.URL.Path == saltPath {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error_type":"UNAUTHENTICATED","messages":[{"args":[],"default_message":"Authentication required.","id":"com.vmware.vapi.endpoint.method.authentication.required"}]}`))
			return true
		}
		fail(http.StatusUnauthorized, "UNAUTHENTICATED")
		return true
	}
	if r.URL.Path == "/api/session" && r.Method == http.MethodDelete {
		if legacy {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"value": nil})
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
		return true
	}
	if r.Method != http.MethodGet {
		fail(http.StatusMethodNotAllowed, "NOT_ALLOWED_IN_CURRENT_STATE")
		return true
	}
	path := r.URL.Path
	switch path {
	case "/api/cis/tagging/category", "/api/cis/tagging/tag", "/api/vcenter/namespace-management/clusters":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
		return true
	case saltPath:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"minion_service_enabled": true,
			"masters":                []string{"salt.example.invalid"},
			"memory_limit_mb":        -1.0,
			"minion_id":              "00000000-0000-4000-8000-000000000000",
			"cpu_limit_pct":          -1.0,
			"grains":                 []map[string]string{{"key": "role", "value": "simulator"}},
		})
		return true
	case "/api/appliance/system/version":
		w.Header().Set("Content-Type", "application/json")
		v := s.catalog.About
		release, hasPrefix := strings.CutPrefix(v.FullName, "VMware vCenter Server ")
		release, hasBuild := strings.CutSuffix(release, "."+v.Build)
		if !hasPrefix || !hasBuild {
			release = v.Version
		}
		// a patched appliance (9.1.1.0100) says "Patch for" and names the
		// release as major.minor; the others keep the full version number
		summary := "VMware vCenter Server " + v.Version
		if strings.Trim(v.PatchLevel, "0.") != "" {
			summary = "Patch for VMware vCenter Server " + v.LicenseVer
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"summary": summary, "product": "VMware vCenter Server", "build": v.Build, "name": release, "type": "vCenter Server with an embedded Platform Services Controller", "version": release})
		return true
	}
	if s.appliance(w, r) {
		return true
	}
	if s.inventory(w, r) {
		return true
	}
	status := http.StatusNotFound
	switch {
	case path == "/api/appliance/health/system", path == "/api/vcenter/system-config/deployment-type", path == "/api/content/library", path == "/api/content/local-library", path == "/api/vcenter/topology/nodes", strings.HasPrefix(path, "/api/esx/settings/"):
		status = http.StatusForbidden
	case path == "/api/vcenter/storage/policies", strings.HasPrefix(path, "/api/vcenter/vm/") && strings.HasSuffix(path, "/storage/policy"):
		status = http.StatusInternalServerError
	case path == "/api/vcenter/vcha/cluster/mode", path == "/api/vcenter/inventory/datastore":
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	kind := map[int]string{400: "INVALID_ARGUMENT", 403: "UNAUTHORIZED", 404: "NOT_FOUND", 500: "INTERNAL_SERVER_ERROR"}[status]
	fail(status, kind)
	return true
}
