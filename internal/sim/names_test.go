package sim

import (
	"strings"
	"testing"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// With several datacenters, clusters and VMs are named for their datacenter,
// as vSAN datastores are, so no two share a name in vCenter. Only the names
// change: keys, host names, disk files and every UUID stay what a single
// datacenter's naming gives them, so no identity moves when a second
// datacenter is added or this naming arrives.
func TestClusterAndVMNamesDistinctAcrossDatacenters(t *testing.T) {
	one := lifeScenario(t)
	two := lifeScenario(t)
	two.Datacenters = append(two.Datacenters, scenario.Datacenter{Name: "dc-02", Clusters: 2})
	if err := two.Validate(); err != nil {
		t.Fatal(err)
	}

	// a single datacenter keeps the names it always had
	single := topology.Generate(one, registry.InMemory("names"))
	if cl := single.Datacenters[0].Clusters[0]; cl.Name != "cluster-01" || cl.VMs[0].Name != "cluster-01-vm-001" {
		t.Fatalf("single datacenter: cluster %q, VM %q", cl.Name, cl.VMs[0].Name)
	}

	s := lifeSim(t, two, registry.InMemory("names"))
	ctx := s.Life().as("")
	seen := map[string]string{}
	for _, kind := range []string{"ClusterComputeResource", "VirtualMachine"} {
		for _, e := range s.Life().reg.All(kind) {
			name := e.(mo.Entity).Entity().Name
			if other, ok := seen[name]; ok {
				t.Errorf("%s and %s are both named %q", other, kind, name)
			}
			seen[name] = kind
		}
	}
	for _, want := range []string{"dc-01-cluster-01", "dc-02-cluster-01", "dc-02-cluster-02-vm-001"} {
		if seen[want] == "" {
			t.Errorf("no object named %q", want)
		}
	}

	// identities are those the one-datacenter naming gives: the same keys,
	// host names and disk files, so the same serials and UUIDs
	base := topology.Generate(one, registry.InMemory("names"))
	for _, refs := range s.Clusters {
		cl := refs.Cluster
		if cl.Datacenter.Name != "dc-01" {
			continue
		}
		var was *topology.Cluster
		for _, c := range base.Datacenters[0].Clusters {
			if c.Key == cl.Key {
				was = c
			}
		}
		if was == nil {
			t.Fatalf("no cluster %s in the one-datacenter build", cl.Key)
		}
		for i, h := range cl.Hosts {
			if h.Name != was.Hosts[i].Name || h.HardwareUUID != was.Hosts[i].HardwareUUID {
				t.Errorf("host %s became %s", was.Hosts[i].Name, h.Name)
			}
		}
		for i, vm := range cl.VMs {
			old := was.VMs[i]
			if vm.Key != old.Key || vm.UUID != old.UUID || vm.InstanceUUID != old.InstanceUUID || vm.Disks[0].Path != old.Disks[0].Path {
				t.Errorf("VM %s (was %s): key %s/%s, disk %s/%s", vm.Name, old.Name, vm.Key, old.Key, vm.Disks[0].Path, old.Disks[0].Path)
			}
			if !strings.HasPrefix(vm.Name, "dc-01-") || strings.TrimPrefix(vm.Name, "dc-01-") != old.Name {
				t.Errorf("VM %s was %s", vm.Name, old.Name)
			}
		}
		obj := s.Life().reg.Get(refs.Ref).(*simulator.ClusterComputeResource)
		var name string
		ctx.WithLock(obj, func() { name = obj.Name })
		if name != "dc-01-"+was.Name {
			t.Errorf("cluster %s is named %q in vCenter", cl.Key, name)
		}
	}
}
