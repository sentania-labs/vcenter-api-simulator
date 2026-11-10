package sim_test

import (
	"fmt"
	"path/filepath"
	"testing"

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
				for k, v := range afterAdd {
					if _, ok := previous[k]; !ok && before[k] == v {
						t.Errorf("removed identity reused for %s", k)
					}
				}
			})
		}
	}
}
