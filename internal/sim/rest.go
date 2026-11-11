package sim

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/vmware/govmomi/simulator"
)

// restSessions holds local read-only API sessions. It shares the scenario's
// credential policy with SOAP but never stores a password in a session.
type restSessions struct {
	mu          sync.Mutex
	tokens      map[string]string
	credentials map[string]string
	check       func(string, string) bool
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
		s.tokens[token] = user
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(token)
		return true
	}
	token := r.Header.Get("vmware-api-session-id")
	s.mu.Lock()
	_, valid := s.tokens[token]
	if valid && r.URL.Path == "/api/session" && r.Method == http.MethodDelete {
		delete(s.tokens, token)
	}
	s.mu.Unlock()
	if !valid {
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
	case "/api/appliance/system/version":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"summary": "Patch for VMware vCenter Server 9.1", "install_time": "2026-10-23T16:41:17.074Z", "product": "VMware vCenter Server", "build": s.catalog.About.Build, "name": s.catalog.About.Version + ".0100", "releasedate": "October 08, 2026", "type": "vCenter Server with an embedded Platform Services Controller", "version": s.catalog.About.Version + ".0100"})
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
