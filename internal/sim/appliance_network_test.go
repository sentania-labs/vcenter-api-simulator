package sim

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestApplianceNetworkingIdentity(t *testing.T) {
	n, err := (ApplianceNetwork{DNSServers: []string{"10.20.0.53", "2001:db8::53"}, IPv4: "10.20.0.10/20"}).resolve()
	if err != nil {
		t.Fatal(err)
	}
	s := restSessions{publicAddress: "https://sim.example.test:8443", network: n}
	read := func(path string) any {
		t.Helper()
		r := httptest.NewRequest("GET", "https://backend.invalid"+path, nil)
		w := httptest.NewRecorder()
		if !s.appliance(w, r) {
			t.Fatal("route missing")
		}
		var result any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	full := read("/api/appliance/networking").(map[string]any)
	dns := full["dns"].(map[string]any)
	if dns["hostname"] != "sim.example.test" || read("/api/appliance/networking/dns/hostname") != dns["hostname"] {
		t.Fatal("public hostname not used")
	}
	servers := read("/api/appliance/networking/dns/servers").(map[string]any)["servers"].([]any)
	if len(servers) != 2 || servers[0] != "10.20.0.53" {
		t.Fatal("DNS override lost")
	}
	ip := full["interfaces"].(map[string]any)["nic0"].(map[string]any)["ipv4"].(map[string]any)
	nic := full["interfaces"].(map[string]any)["nic0"].(map[string]any)
	if nic["mac"] != "00:50:56:14:00:0a" || ip["default_gateway"] != "10.20.0.1" || ip["configurable"] != true {
		t.Fatalf("interface fields missing: %+v", nic)
	}
	if ip["address"] != "10.20.0.10" || ip["prefix"] != float64(20) || full["vcenter_base_url"] != "https://sim.example.test:8443" {
		t.Fatalf("wrong networking: %+v", full)
	}
	s.publicAddress = ""
	if got := read("/api/appliance/networking/dns/hostname"); got != "backend.invalid" {
		t.Fatalf("request fallback: %v", got)
	}
}

func TestApplianceNetworkDefaultsAndValidation(t *testing.T) {
	n, err := (ApplianceNetwork{}).resolve()
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range n.DNSServers {
		if server == "192.0.2.1" {
			t.Fatal("placeholder resolver")
		}
	}
	for _, n := range []ApplianceNetwork{{DNSServers: []string{"invalid"}}, {IPv4: "invalid"}, {IPv4: "2001:db8::1/64"}} {
		if _, err := n.resolve(); err == nil {
			t.Fatalf("accepted invalid override: %+v", n)
		}
	}
}
