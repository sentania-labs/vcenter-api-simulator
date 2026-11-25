package sim

import (
	"testing"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// The datastore's summary, both info free-space fields, the VM's storage
// properties and the performance counters must all be one figure, and that
// figure must follow the VMs as Activity adds, removes and fills them.
func TestDatastoreUsageFollowsVMStorage(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("ds-usage"))
	l := s.Life()
	refs := s.Clusters[0]
	cl := refs.Cluster
	ds := l.reg.Get(refs.Datastore).(*simulator.Datastore)

	if cl.Datastore != "cluster-01-vsanDatastore" || ds.Name != cl.Datastore {
		t.Fatalf("vSAN datastore %q, cluster name %q", ds.Name, cl.Datastore)
	}

	check := func(stage string) {
		t.Helper()
		var committed, uncommitted, unshared, forty int64
		for _, lv := range l.vms[cl.Key] {
			obj := l.reg.Get(lv.ref).(*simulator.VirtualMachine)
			if len(obj.Storage.PerDatastoreUsage) != 1 {
				t.Fatalf("%s: %s has %d datastore usages", stage, lv.vm.Name, len(obj.Storage.PerDatastoreUsage))
			}
			u := obj.Storage.PerDatastoreUsage[0]
			wantC, wantU, wantS := topology.VMStorage(lv.vm)
			if u.Datastore != refs.Datastore || u.Committed != wantC || u.Uncommitted != wantU || u.Unshared != wantS {
				t.Errorf("%s: %s perDatastoreUsage %+v, want %d/%d/%d on %s", stage, lv.vm.Name, u, wantC, wantU, wantS, refs.Datastore)
			}
			var files int64
			for _, f := range obj.LayoutEx.File {
				files += f.Size
			}
			if files != u.Committed {
				t.Errorf("%s: %s layoutEx files add up to %d, committed %d", stage, lv.vm.Name, files, u.Committed)
			}
			if len(lv.vm.Disks) == 1 && lv.vm.Disks[0].CapacityKB == 40<<20 {
				const gib20 = 20 << 30
				if u.Committed != gib20 || u.Uncommitted != gib20 || u.Unshared != gib20 {
					t.Errorf("%s: %s with one 40 GiB disk has %+v, want 20 GiB committed, uncommitted and unshared", stage, lv.vm.Name, u)
				}
				forty++
			}
			sum := obj.Summary.Storage
			if sum == nil || sum.Committed != u.Committed || sum.Uncommitted != u.Uncommitted || sum.Unshared != u.Unshared {
				t.Errorf("%s: %s summary.storage %+v, perDatastoreUsage %+v", stage, lv.vm.Name, sum, u)
			}
			committed += u.Committed
			uncommitted += u.Uncommitted
			unshared += u.Unshared
		}
		if forty == 0 {
			t.Errorf("%s: no VM with a single 40 GiB disk to check against the fixed figure", stage)
		}
		used := ds.Summary.Capacity - ds.Summary.FreeSpace
		wantUsed := min(ds.Summary.Capacity, committed+max(0, cl.UsedDelta()))
		if used != wantUsed || unshared != committed {
			t.Errorf("%s: used %d of %d, want %d; unshared %d committed %d", stage, used, ds.Summary.Capacity, wantUsed, unshared, committed)
		}
		if ds.Summary.Uncommitted != uncommitted {
			t.Errorf("%s: datastore uncommitted %d, VMs %d", stage, ds.Summary.Uncommitted, uncommitted)
		}
		if ds.Info.GetDatastoreInfo().FreeSpace != ds.Summary.FreeSpace {
			t.Errorf("%s: info free %d, summary free %d", stage, ds.Info.GetDatastoreInfo().FreeSpace, ds.Summary.FreeSpace)
		}
		n := readPerformanceState(l.as("")).nodes[refs.Datastore]
		if n == nil || n.used != used/1024 || n.capacity != ds.Summary.Capacity/1024 || n.provisioned != (used+uncommitted)/1024 {
			t.Errorf("%s: datastore counters %+v, used %d capacity %d provisioned %d", stage, n, used, ds.Summary.Capacity, used+uncommitted)
		}
	}

	check("built")
	// A plain client refresh must leave the modelled figures as they are.
	for _, ref := range refs.VMs {
		l.reg.Get(ref).(*simulator.VirtualMachine).RefreshStorageInfo(l.as(""), &types.RefreshStorageInfo{This: ref})
	}
	check("refreshed")

	before := ds.Summary.Capacity - ds.Summary.FreeSpace
	if _, _, err := l.AddVM(cl.Key, 0); err != nil {
		t.Fatal(err)
	}
	check("added")
	if after := ds.Summary.Capacity - ds.Summary.FreeSpace; after <= before {
		t.Errorf("adding a VM left %d used, was %d", after, before)
	}
	before = ds.Summary.Capacity - ds.Summary.FreeSpace
	if _, err := l.RemoveVM(cl.Key, 0); err != nil {
		t.Fatal(err)
	}
	check("removed")
	if after := ds.Summary.Capacity - ds.Summary.FreeSpace; after >= before {
		t.Errorf("removing a VM left %d used, was %d", after, before)
	}
	before = ds.Summary.Capacity - ds.Summary.FreeSpace
	l.Fill(cl.Key, 0.01)
	check("filled")
	if after := ds.Summary.Capacity - ds.Summary.FreeSpace; after <= before {
		t.Errorf("filling left %d used, was %d", after, before)
	}
}
