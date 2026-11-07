package topology

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestPhase1Shape(t *testing.T) {
	tp := Phase1("test")
	if len(tp.Datacenters) != 1 || len(tp.Datacenters[0].Clusters) != 1 {
		t.Fatalf("datacenters %d", len(tp.Datacenters))
	}
	cl := tp.Datacenters[0].Clusters[0]
	if len(cl.Hosts) != 4 || len(cl.VMs) != 4 {
		t.Fatalf("hosts=%d vms=%d", len(cl.Hosts), len(cl.VMs))
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
	add("cluster", cl.VsanUUID)
	for _, h := range cl.Hosts {
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
	for _, vm := range cl.VMs {
		add("vm", vm.UUID)
		add("vm-instance", vm.InstanceUUID)
		add("vm-namespace", vm.NamespaceUUID)
	}
}

// fingerprint lists every identity in traversal order (the topology has
// parent pointers, so it is not JSON-encodable as a whole).
func fingerprint(tp *Topology) string {
	var ids []string
	ids = append(ids, tp.InstanceUUID)
	for _, c := range tp.Clusters() {
		ids = append(ids, c.Key, c.VsanUUID, c.Datacenter.VDSUUID)
		for _, h := range c.Hosts {
			ids = append(ids, h.Name, h.NodeUUID, h.HardwareUUID)
			for _, d := range h.Devices {
				ids = append(ids, d.UUID, d.VsanUUID, d.VMFSUUID, d.CanonicalName)
			}
			for _, w := range h.Worlds {
				ids = append(ids, w.Name)
			}
		}
		for _, vm := range c.VMs {
			ids = append(ids, vm.Name, vm.UUID, vm.InstanceUUID, vm.Disks[0].Path)
		}
	}
	b, _ := json.Marshal(ids)
	return string(b)
}

func TestPhase1Deterministic(t *testing.T) {
	a := fingerprint(Phase1("seed-a"))
	b := fingerprint(Phase1("seed-a"))
	c := fingerprint(Phase1("seed-b"))
	if a != b {
		t.Fatal("same seed, different topology")
	}
	if a == c {
		t.Fatal("different seed, same identities")
	}
	if Phase1("seed-a").Clusters()[0].Hosts[0].Name != Phase1("seed-b").Clusters()[0].Hosts[0].Name {
		t.Fatal("seed must not change structure")
	}
}

func multi() *scenario.Scenario {
	s, err := scenario.Parse([]byte(`
seed: many
datacenters:
  - name: dc-01
    clusters: 10
defaults:
  hosts: 8
vary:
  hosts: [4, 6, 8, 12]
  vmsPerHost: {min: 2, max: 10}
  claimedDevices: [1, 2]
  domWorlds: {min: 40, max: 90}
clusters:
  - key: dc-01/cluster-03
    hosts: 16
    dedup: false
`))
	if err != nil {
		panic(err)
	}
	return s
}

// Ten generated clusters draw their own counts and are not copies of one
// template; explicit overrides win.
func TestGeneratedClustersVaryIndependently(t *testing.T) {
	tp := Generate(multi(), registry.InMemory("many"))
	cls := tp.Clusters()
	if len(cls) != 10 {
		t.Fatalf("%d clusters", len(cls))
	}
	hosts, vms, devs := map[int]bool{}, map[int]bool{}, map[int]bool{}
	for _, c := range cls {
		hosts[len(c.Hosts)] = true
		vms[len(c.VMs)/len(c.Hosts)] = true
		devs[len(c.Hosts[0].Devices)] = true
		for _, v := range []int{len(c.Hosts), scenario.Int(c.Spec.VMs.PerHost), c.Spec.Host.ClaimedDevices, scenario.Int(c.Spec.Host.DomWorlds)} {
			if v == 0 {
				t.Errorf("%s: a drawn count is zero: %+v", c.Key, c.Spec)
			}
		}
		if c.Datacenter.Name != "dc-01" || c.VDS() != "vds-01" {
			t.Errorf("%s: datacenter %q vds %q", c.Key, c.Datacenter.Name, c.VDS())
		}
	}
	if len(hosts) < 2 || len(vms) < 2 || len(devs) < 2 {
		t.Errorf("no variation: hosts %v vms/host %v devices %v", hosts, vms, devs)
	}
	c3 := cls[2]
	if c3.Key != "dc-01/cluster-03" || len(c3.Hosts) != 16 || c3.Dedup {
		t.Errorf("override lost: %s hosts=%d dedup=%v", c3.Key, len(c3.Hosts), c3.Dedup)
	}
	if cls[0].Datastore != "vsanDatastore" || cls[1].Datastore != "vsanDatastore (1)" {
		t.Errorf("datastore names %q %q", cls[0].Datastore, cls[1].Datastore)
	}
	// every identity unique across the whole vCenter
	seen := map[string]bool{}
	uniq := func(id string) {
		if seen[id] {
			t.Errorf("duplicate identity %s", id)
		}
		seen[id] = true
	}
	for _, c := range cls {
		uniq(c.VsanUUID)
		uniq(c.ContainerID)
		for _, h := range c.Hosts {
			uniq(h.NodeUUID)
			uniq(h.HardwareUUID)
			uniq(h.Name)
			uniq(h.MgmtIP)
			for _, d := range h.Devices {
				uniq(d.UUID)
				uniq(d.CanonicalName)
				if d.Claimed {
					uniq(d.VsanUUID)
				} else {
					uniq(d.VMFSUUID)
				}
			}
		}
		for _, vm := range c.VMs {
			uniq(vm.InstanceUUID)
			uniq(vm.UUID)
			uniq(vm.NamespaceUUID)
			uniq(vm.Name)
		}
	}
}

// Changing one cluster's size, or adding clusters, keeps every other
// identity; the registry hands the new entities new serials.
func TestEditsKeepExistingIdentities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r, err := registry.Open(path, "many", false)
	if err != nil {
		t.Fatal(err)
	}
	before := Generate(multi(), r)
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	ids := func(tp *Topology) map[string]string {
		m := map[string]string{}
		for _, c := range tp.Clusters() {
			m[c.Key] = c.VsanUUID
			for _, h := range c.Hosts {
				m[h.Key] = h.NodeUUID
				for _, d := range h.Devices {
					m[d.Key] = d.UUID
				}
			}
			for _, vm := range c.VMs {
				m[vm.Key] = vm.InstanceUUID
			}
		}
		return m
	}
	// restart: identical
	r2, _ := registry.Open(path, "many", false)
	if a, b := ids(before), ids(Generate(multi(), r2)); len(a) != len(b) {
		t.Fatal("restart changed the inventory")
	} else {
		for k, v := range a {
			if b[k] != v {
				t.Fatalf("restart changed %s", k)
			}
		}
	}
	// edit: cluster-03 shrinks to 4 hosts, cluster-11 appears
	s := multi()
	s.Datacenters[0].Clusters = 11
	s.Clusters[0].Hosts = 4
	r3, _ := registry.Open(path, "many", false)
	after := Generate(s, r3)
	a, b := ids(before), ids(after)
	for k, v := range b {
		if old, ok := a[k]; ok && old != v {
			t.Errorf("edit changed the identity of %s", k)
		}
	}
	if len(after.Clusters()) != 11 || len(after.Clusters()[2].Hosts) != 4 {
		t.Fatalf("edit not applied")
	}
	// a retired host's serial is not reused by the new cluster
	for k := range a {
		if _, still := b[k]; !still && r3.Has("host/"+k) {
			// retired hosts stay in the registry so their serials are never reused
			continue
		}
	}
	if !r3.Has("host/dc-01/cluster-03/esx16") {
		t.Error("retired host dropped from the registry")
	}
	// a different seed changes every identity (and, with vary draws, the
	// structure: the draws are seeded too)
	other := Generate(multi(), registry.InMemory("other"))
	for k, v := range ids(other) {
		if a[k] == v {
			t.Errorf("seed kept identity of %s", k)
		}
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
