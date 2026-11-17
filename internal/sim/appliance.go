package sim

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
)

// Appliance configuration belongs to this simulated appliance. Account defaults
// are invented; networking comes from its configured identity and local runtime.
func (s *restSessions) appliance(w http.ResponseWriter, r *http.Request) bool {
	host := clientHost(r)
	if s.publicAddress != "" {
		if u, err := url.Parse(s.publicAddress); err == nil && u.Host != "" {
			host = u.Host
		}
	}
	hostname := host
	if u, err := url.Parse("https://" + host); err == nil {
		hostname = u.Hostname()
	}
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
		ipv4 := map[string]any{"mode": "static", "configurable": false}
		if s.network.IPv4 != "" {
			ip, subnet, _ := net.ParseCIDR(s.network.IPv4)
			prefix, _ := subnet.Mask.Size()
			ipv4["address"], ipv4["prefix"] = ip.String(), prefix
		}
		value = map[string]any{"interfaces": map[string]any{"nic0": map[string]any{"name": "nic0", "status": "up", "ipv4": ipv4}}, "dns": map[string]any{"mode": "is_static", "hostname": hostname, "servers": s.network.DNSServers}, "vcenter_base_url": "https://" + host}
	case "/api/appliance/networking/dns/hostname":
		value = hostname
	case "/api/appliance/networking/dns/servers":
		value = map[string]any{"mode": "is_static", "servers": s.network.DNSServers}
	case "/api/appliance/timesync":
		value = "NTP"
	case "/api/appliance/tls/profiles/global":
		value = map[string]any{"profile": "COMPATIBLE"}
	case "/api/vcenter/topology/nodes":
		value = []any{map[string]any{"node": hostname, "replication_partners": []string{}, "type": "VCSA_EMBEDDED"}}
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
