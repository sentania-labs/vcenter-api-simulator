package topology

import (
	"encoding/json"
	"regexp"
	"testing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestPhase1Shape(t *testing.T) {
	tp := Phase1("test")
	if len(tp.Cluster.Hosts) != 4 || len(tp.Cluster.VMs) != 4 {
		t.Fatalf("hosts=%d vms=%d", len(tp.Cluster.Hosts), len(tp.Cluster.VMs))
	}
	seen := map[string]string{}
	add := func(kind, id string) {
		if !uuidRe.MatchString(id) {
			t.Errorf("%s %q is not a v5 uuid", kind, id)
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("%s %q collides with %s", kind, id, prev)
		}
		seen[id] = kind
	}
	add("instance", tp.InstanceUUID)
	add("cluster", tp.Cluster.VsanUUID)
	for _, h := range tp.Cluster.Hosts {
		add("node", h.NodeUUID)
		add("hardware", h.HardwareUUID)
		claimed := 0
		for _, d := range h.Devices {
			if len(d.CanonicalName) != len("t10.NVMe____")+40+16 {
				t.Errorf("canonical name width: %q", d.CanonicalName)
			}
			if d.Claimed {
				claimed++
				add("disk", d.VsanUUID)
			}
		}
		if claimed != 1 {
			t.Errorf("%s: %d claimed devices", h.Name, claimed)
		}
	}
	for _, vm := range tp.Cluster.VMs {
		add("vm", vm.UUID)
		add("vm-instance", vm.InstanceUUID)
		add("vm-namespace", vm.NamespaceUUID)
	}
}

func TestPhase1Deterministic(t *testing.T) {
	a, _ := json.Marshal(Phase1("seed-a"))
	b, _ := json.Marshal(Phase1("seed-a"))
	c, _ := json.Marshal(Phase1("seed-b"))
	if string(a) != string(b) {
		t.Fatal("same seed, different topology")
	}
	if string(a) == string(c) {
		t.Fatal("different seed, same identities")
	}
	if Phase1("seed-a").Cluster.Hosts[0].Name != Phase1("seed-b").Cluster.Hosts[0].Name {
		t.Fatal("seed must not change structure")
	}
}

func TestT10Model(t *testing.T) {
	if got := T10Model("SIM NVMe 4TB Model A"); got != "SIM_NVMe_4TB_Model_A____________________" {
		t.Fatalf("got %q", got)
	}
	if got := T10Model("X-1"); got[:4] != "X2D1" {
		t.Fatalf("hyphen encoding: %q", got)
	}
}
