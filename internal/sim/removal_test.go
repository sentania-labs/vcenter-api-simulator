package sim_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

func snapshot(s *sim.Simulator) map[string]string {
	out := map[string]string{}
	for _, c := range s.Clusters {
		out[c.Cluster.Key] = fmt.Sprint(c.Ref, c.Cluster.VsanUUID, c.Cluster.StatsUUID, c.Cluster.PolicyUUID)
		for i, h := range c.Cluster.Hosts {
			out[h.Key] = fmt.Sprint(c.Hosts[i], h.HardwareUUID, h.NodeUUID, h.DiskGroupUUID)
		}
		for i, v := range c.Cluster.VMs {
			out[v.Key] = fmt.Sprint(c.VMs[i], v.UUID, v.InstanceUUID, v.NamespaceUUID)
		}
	}
	return out
}

func TestRemovalIdentity(t *testing.T) {
	for _, kind := range []string{"cluster", "host", "vm"} {
		for _, index := range []int{0, 1, 2} {
			t.Run(fmt.Sprintf("%s/%d", kind, index), func(t *testing.T) {
				sc, err := scenario.Parse([]byte("seed: identity\ndatacenters: [{name: dc, clusters: 3}]\ndefaults: {hosts: 4, vms: {perHost: 1}}\n"))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "registry.json")
				r, err := registry.Open(path, sc.Seed, false)
				if err != nil {
					t.Fatal(err)
				}
				original := topology.Generate(sc, r)
				baseline, err := sim.New(sim.Options{Topology: original})
				if err != nil {
					t.Fatal(err)
				}
				expected := snapshot(baseline)
				baseline.Close()
				first, err := sim.New(sim.Options{Topology: original, Registry: r})
				if err != nil {
					t.Fatal(err)
				}
				before := snapshot(first)
				first.Close()
				for k, v := range expected {
					if before[k] != v {
						t.Fatalf("initial identity %s changed: %s != %s", k, before[k], v)
					}
				}
				if err := r.Save(); err != nil {
					t.Fatal(err)
				}
				edit := func(tp *topology.Topology) string {
					dc := tp.Datacenters[0]
					c := dc.Clusters[0]
					switch kind {
					case "cluster":
						key := dc.Clusters[index].Key
						dc.Clusters = append(dc.Clusters[:index], dc.Clusters[index+1:]...)
						return key
					case "host":
						key := c.Hosts[index].Key
						c.Hosts = append(c.Hosts[:index], c.Hosts[index+1:]...)
						for _, vm := range c.VMs {
							vm.Host %= len(c.Hosts)
						}
						return key
					default:
						key := c.VMs[index].Key
						c.VMs = append(c.VMs[:index], c.VMs[index+1:]...)
						return key
					}
				}
				var previous map[string]string
				for pass := 0; pass < 2; pass++ {
					r, err = registry.Open(path, sc.Seed, false)
					if err != nil {
						t.Fatal(err)
					}
					tp := topology.Generate(sc, r)
					removed := edit(tp)
					current, err := sim.New(sim.Options{Topology: tp, Registry: r})
					if err != nil {
						t.Fatal(err)
					}
					after := snapshot(current)
					current.Close()
					if _, ok := after[removed]; ok {
						t.Fatalf("removed %s remains", removed)
					}
					for k, v := range after {
						if before[k] != v {
							t.Errorf("%s changed: %s -> %s", k, before[k], v)
						}
						if pass > 0 && previous[k] != v {
							t.Errorf("%s changed after restart", k)
						}
					}
					previous = after
					if err := r.Save(); err != nil {
						t.Fatal(err)
					}
				}
				r, err = registry.Open(path, sc.Seed, false)
				if err != nil {
					t.Fatal(err)
				}
				added, err := sim.New(sim.Options{Topology: topology.Generate(sc, r), Registry: r})
				if err != nil {
					t.Fatal(err)
				}
				afterAdd := snapshot(added)
				added.Close()
				for k, v := range previous {
					if afterAdd[k] != v {
						t.Errorf("survivor %s changed on addition", k)
					}
				}
				for k, v := range before {
					if afterAdd[k] != v {
						t.Errorf("re-added %s did not get its previous identities back: %s -> %s", k, v, afterAdd[k])
					}
				}
			})
		}
	}
}

func datastoreRefs(t *testing.T, e *env) map[string]string {
	t.Helper()
	ctx := context.Background()
	v, err := view.NewManager(e.client).CreateContainerView(ctx, e.client.ServiceContent.RootFolder, []string{"Datastore"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v.Destroy(ctx) }()
	var dss []mo.Datastore
	if err := v.Retrieve(ctx, []string{"Datastore"}, []string{"name"}, &dss); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, ds := range dss {
		out[ds.Name] = ds.Self.Value
	}
	return out
}

func TestAddingDeviceKeepsExistingMoRefs(t *testing.T) {
	sc, err := scenario.Parse([]byte("seed: growth\ndatacenters: [{name: dc, clusters: 2}]\ndefaults: {hosts: 3, host: {localDevices: 1}, vms: {perHost: 1}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	run := func(localDevices int) (map[string]string, map[string]string) {
		t.Helper()
		next := *sc
		next.Defaults.Host.LocalDevices = scenario.IntPtr(localDevices)
		r, err := registry.Open(path, next.Seed, false)
		if err != nil {
			t.Fatal(err)
		}
		e := startWith(t, topology.Generate(&next, r), sim.Options{Registry: r})
		if err := e.login("administrator@vsphere.local", "VMware1!"); err != nil {
			t.Fatal(err)
		}
		if err := r.Save(); err != nil {
			t.Fatal(err)
		}
		return snapshot(e.sim), datastoreRefs(t, e)
	}
	before, beforeDS := run(1)
	after, afterDS := run(2)
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed when a device was added: %s -> %s", k, v, after[k])
		}
	}
	for name, ref := range beforeDS {
		if afterDS[name] != ref {
			t.Errorf("datastore %s moved from %s to %s", name, ref, afterDS[name])
		}
	}
	if added := len(afterDS) - len(beforeDS); added != 6 {
		t.Errorf("%d datastores added, want one per host (6)", added)
	}
	seen := map[string]string{}
	for name, ref := range afterDS {
		if other, dup := seen[ref]; dup {
			t.Errorf("datastores %s and %s share %s", name, other, ref)
		}
		seen[ref] = name
	}
	again, againDS := run(2)
	for k, v := range after {
		if again[k] != v {
			t.Errorf("%s changed on restart: %s -> %s", k, v, again[k])
		}
	}
	for name, ref := range afterDS {
		if againDS[name] != ref {
			t.Errorf("datastore %s changed on restart: %s -> %s", name, ref, againDS[name])
		}
	}
}
