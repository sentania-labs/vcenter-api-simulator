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
	credentials map[string]string
	check       func(string, string) bool
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
func (s *restSessions) serve(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/api" && !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	if r.URL.Path == "/api" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return true
	}
	if r.URL.Path == "/api/session" && r.Method == http.MethodPost {
		user, password, ok := r.BasicAuth()
		if !ok || !s.authorized(user, password) {
			w.WriteHeader(http.StatusUnauthorized)
			return true
		}
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		token := hex.EncodeToString(b[:])
		s.mu.Lock()
		s.tokens[token] = restSession{user: user, last: time.Now()}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(token)
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
		w.WriteHeader(http.StatusUnauthorized)
		return true
	}
	if r.URL.Path == "/api/session" && r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
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
		_ = json.NewEncoder(w).Encode(map[string]any{"summary": "VMware vCenter Server " + v.Version, "product": "VMware vCenter Server", "build": v.Build, "name": release, "type": "vCenter Server with an embedded Platform Services Controller", "version": release})
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
	w.WriteHeader(status)
	if status == http.StatusForbidden {
		_, _ = w.Write([]byte(`{"error_type":"UNAUTHORIZED","messages":[{"id":"com.vmware.vapi.authorization.permission.denied","default_message":"Permission denied"}]}`))
	}
	return true
}
