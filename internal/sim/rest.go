package sim

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// restSessions holds local read-only API sessions. It shares the scenario's
// credential policy with SOAP but never stores a password in a session.
type restSessions struct {
	mu          sync.Mutex
	tokens      map[string]bool
	credentials map[string]string
}

func (s *restSessions) authorized(user, password string) bool {
	if user == "" || password == "" {
		return false
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
		s.tokens[token] = true
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(token)
		return true
	}
	token := r.Header.Get("vmware-api-session-id")
	s.mu.Lock()
	valid := s.tokens[token]
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
