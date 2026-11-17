package sim

import (
	"encoding/json"
	"net/http"
)

// Appliance configuration belongs to this simulated appliance. These are
// invented defaults, not settings or account data copied from a vCenter.
func (s *restSessions) appliance(w http.ResponseWriter, r *http.Request) bool {
	var value any
	switch r.URL.Path {
	case "/api/appliance/local-accounts/root":
		value = map[string]any{"fullname": "Simulator appliance administrator", "enabled": true, "has_password": true, "roles": []string{"admin"}, "last_password_change": "2026-01-01T00:00:00Z", "warn_days_before_password_expiration": 7, "min_days_between_password_change": 0, "max_days_between_password_change": 90}
	case "/api/appliance/local-accounts/global-policy":
		value = map[string]any{"minimum_numerics_char_count": 1, "prior_password_remember_count": 5, "minimum_lowercase_char_count": 1, "min_days": 0, "managed_at_fleet": false, "length_of_lockout_period_in_seconds": 900, "minimum_length": 12, "minimum_special_char_count": 1, "minimum_uppercase_char_count": 1, "fail_interval_between_attempts": 0, "failed_attempt_count_before_account_lockout": 5, "warn_days": 7, "max_days": 90}
	case "/api/appliance/access/consolecli", "/api/appliance/access/ssh":
		value = false
	case "/api/appliance/recovery/backup/job", "/api/appliance/logging/forwarding", "/api/appliance/networking/firewall/inbound", "/api/vcenter/storage/policies", "/api/vcenter/namespaces/instances/v2":
		value = []any{}
	case "/api/appliance/recovery/backup/schedules":
		value = map[string]any{}
	case "/api/appliance/networking":
		value = map[string]any{"interfaces": map[string]any{"nic0": map[string]any{"name": "nic0", "mac": "00:50:56:00:00:01", "status": "up", "ipv4": map[string]any{"mode": "static", "address": "192.0.2.10", "prefix": 24, "default_gateway": "192.0.2.1", "configurable": true}}}, "dns": map[string]any{"mode": "is_static", "hostname": "vcenter.example.invalid", "servers": []string{"192.0.2.1"}}, "vcenter_base_url": "https://" + clientHost(r)}
	case "/api/appliance/timesync":
		value = "NTP"
	case "/api/appliance/tls/profiles/global":
		value = map[string]any{"profile": "COMPATIBLE"}
	case "/api/vcenter/topology/nodes":
		value = []any{map[string]any{"node": "vcenter.example.invalid", "replication_partners": []string{}, "type": "VCSA_EMBEDDED"}}
	case "/api/vcenter/namespace-management/supervisors/summaries":
		value = map[string]any{"items": []any{}}
	case "/api/vcenter/services/vsan-health":
		value = map[string]any{"name_key": "vsan-health", "description_key": "vSAN Health Service", "startup_type": "AUTOMATIC", "state": "STARTED", "health": "HEALTHY", "health_messages": []any{}}
	default:
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
	return true
}
