package sim

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// serveRPC implements the vAPI bindings' session and empty tagging flow.
// Licensing mutations are deliberately not part of this read-only service.
func (s *restSessions) serveRPC(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		restError(w, http.StatusMethodNotAllowed, "NOT_ALLOWED_IN_CURRENT_STATE")
		return true
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  struct {
			Service   string `json:"serviceId"`
			Operation string `json:"operationId"`
			Input     any    `json:"input"`
			Context   struct {
				Security map[string]json.RawMessage `json:"securityCtx"`
			} `json:"ctx"`
		} `json:"params"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLocalSOAPBody)).Decode(&req) != nil || req.Method != "invoke" || req.JSONRPC != "2.0" || len(req.ID) == 0 {
		restError(w, 400, "INVALID_ARGUMENT")
		return true
	}
	var output any
	errorType := ""
	readSecret := func(key string) string {
		raw := req.Params.Context.Security[key]
		var val string
		if json.Unmarshal(raw, &val) == nil {
			return val
		}
		var secret struct {
			Secret string `json:"SECRET"`
		}
		_ = json.Unmarshal(raw, &secret)
		return secret.Secret
	}
	if req.Params.Service == "com.vmware.cis.session" && req.Params.Operation == "create" {
		user, password := readSecret("userName"), readSecret("password")
		if user == "" {
			user, password, _ = r.BasicAuth()
		}
		authorized := s.authorized(user, password)
		if !authorized && s.bearer != nil {
			user, authorized = s.bearer(readSecret("samlToken"))
		}
		if !authorized {
			errorType = "unauthenticated"
		} else {
			var bytes [16]byte
			if _, err := rand.Read(bytes[:]); err != nil {
				restError(w, 500, "INTERNAL_SERVER_ERROR")
				return true
			}
			token := hex.EncodeToString(bytes[:])
			s.mu.Lock()
			s.tokens[token] = restSession{user: user, last: time.Now()}
			s.mu.Unlock()
			output = map[string]any{"SECRET": token}
		}
	} else {
		token := readSecret("sessionId")
		if token == "" {
			token = r.Header.Get("vmware-api-session-id")
		}
		valid := s.valid(token)
		user := ""
		if valid {
			s.mu.Lock()
			user = s.tokens[token].user
			s.mu.Unlock()
		} else if s.bearer != nil {
			user, valid = s.bearer(readSecret("samlToken"))
		}
		if !valid {
			errorType = "unauthenticated"
		} else {
			switch {
			case req.Params.Service == "com.vmware.cis.session" && req.Params.Operation == "delete":
				s.mu.Lock()
				delete(s.tokens, token)
				s.mu.Unlock()
			case req.Params.Service == "com.vmware.vcenter.authentication.token" && req.Params.Operation == "issue":
				output, errorType = s.exchange(user, vapiFields(vapiFields(req.Params.Input)["spec"]))
			case (req.Params.Service == "com.vmware.cis.tagging.tag" || req.Params.Service == "com.vmware.cis.tagging.category") && req.Params.Operation == "list":
				output = []string{}
			default:
				if req.Params.Service == "com.vmware.stats.provider.queries" || req.Params.Service == "com.vmware.stats.provider.stream" {
					var streamed bool
					output, errorType, streamed = s.statsRPC(w, r, req.ID, req.Params.Service, req.Params.Operation, user, req.Params.Input)
					if streamed {
						return true
					}
				} else {
					errorType = "operation_not_found"
				}
			}
		}
	}
	result := map[string]any{"output": output}
	if errorType != "" {
		result = map[string]any{"error": map[string]any{"ERROR": map[string]any{"com.vmware.vapi.std.errors." + errorType: map[string]any{"messages": []any{}}}}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	return true
}
func restError(w http.ResponseWriter, status int, kind string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error_type": kind, "messages": []any{map[string]any{"id": "com.vmware.vapi.std.errors." + strings.ToLower(kind), "default_message": http.StatusText(status), "args": []string{}}}})
}

func legacyError(w http.ResponseWriter, status int, kind string) {
	id := "com.vmware.vapi.std.errors." + strings.ToLower(kind)
	message := map[string]any{"id": id, "default_message": http.StatusText(status), "args": []string{}}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"majorErrorCode": status, "name": kind, "localizableMessages": []any{map[string]any{"id": id, "defaultMessage": http.StatusText(status), "args": []string{}}}, "type": id, "value": map[string]any{"messages": []any{message}}})
}
