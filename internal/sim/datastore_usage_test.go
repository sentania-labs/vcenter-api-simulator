package sim

import (
	"strings"
	"testing"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
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
			if len(lv.vm.Disks) == 1 && lv.vm.Disks[0].CapacityKB == 596<<20 && lv.vm.SnapshotBytes() == 0 {
				const gib298 = 298 << 30
				if u.Committed != gib298 || u.Uncommitted != gib298 || u.Unshared != gib298 {
					t.Errorf("%s: %s with one 596 GiB disk has %+v, want 298 GiB committed, uncommitted and unshared", stage, lv.vm.Name, u)
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
			t.Errorf("%s: no VM with a single 596 GiB disk to check against the fixed figure", stage)
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

// A VM's nightly backup snapshot is space on the datastore while it exists:
// the VM's committed and unshared space and its datastore's used space grow by
// the snapshot's size, and shrink back when the snapshot is removed.
func TestBackupSnapshotCountsAsUsedSpace(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("ds-snapshot"))
	l := s.Life()
	refs := s.Clusters[0]
	cl := refs.Cluster
	ds := l.reg.Get(refs.Datastore).(*simulator.Datastore)
	var name string
	for n := range l.vms[cl.Key] {
		name = n
		break
	}
	lv := l.vms[cl.Key][name]
	obj := l.reg.Get(lv.ref).(*simulator.VirtualMachine)
	used := func() int64 { return ds.Summary.Capacity - ds.Summary.FreeSpace }
	before, committed := used(), obj.Storage.PerDatastoreUsage[0].Committed

	if err := l.SnapshotVM(cl.Key, name); err != nil {
		t.Fatal(err)
	}
	// A 596 GiB disk commits 298 GiB; a snapshot takes a twentieth of that.
	const snapshot = 298 << 30 / 20
	if got := used() - before; got != snapshot {
		t.Errorf("snapshot added %d to the datastore, want %d", got, snapshot)
	}
	u := obj.Storage.PerDatastoreUsage[0]
	if u.Committed != committed+snapshot || u.Unshared != committed+snapshot || obj.Summary.Storage.Committed != u.Committed {
		t.Errorf("VM storage with snapshot: %+v, summary %+v", u, obj.Summary.Storage)
	}
	var files, snapshotFile int64
	for _, f := range obj.LayoutEx.File {
		files += f.Size
		if f.Type == "snapshotData" {
			snapshotFile = f.Size
		}
	}
	if files != u.Committed || snapshotFile != snapshot {
		t.Errorf("layoutEx files add up to %d with a snapshot file of %d, want %d and %d", files, snapshotFile, u.Committed, snapshot)
	}

	if err := l.RemoveBackup(cl.Key, name); err != nil {
		t.Fatal(err)
	}
	if got := used(); got != before {
		t.Errorf("removing the snapshot left %d used, was %d", got, before)
	}
	if u := obj.Storage.PerDatastoreUsage[0]; u.Committed != committed {
		t.Errorf("VM committed %d after removal, want %d", u.Committed, committed)
	}
}

// A client may destroy a VM that Activity manages. The state saved afterwards
// must leave that VM out and keep describing the rest, not crash.
func TestSnapshotSurvivesAClientDestroyingAVM(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("ds-client-destroy"))
	l := s.Life()
	cl := s.Clusters[0].Cluster
	var gone string
	for n := range l.vms[cl.Key] {
		gone = n
		break
	}
	l.reg.Remove(l.as(""), l.vms[cl.Key][gone].ref)
	l.off[cl.Key][gone] = true
	if snap := l.Snapshot(); len(snap.Off) != 0 {
		t.Errorf("snapshot after a client destroy lists %v off, want none", snap.Off)
	}
}

// Every datastore of a two-datacenter inventory has its own name, so the
// console never shows an "(n)" suffix: the vSAN and local VMFS datastores
// carry their datacenter, and the local ones use space like the others.
func TestDatastoreNamesDistinctAcrossDatacenters(t *testing.T) {
	sc := lifeScenario(t)
	sc.Datacenters = append(sc.Datacenters, scenario.Datacenter{Name: "dc-02", Clusters: 2})
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	s := lifeSim(t, sc, registry.InMemory("ds-names"))
	seen := map[string]bool{}
	var vsan, local int
	for _, e := range s.Life().reg.All("Datastore") {
		ds := e.(*simulator.Datastore)
		if seen[ds.Name] {
			t.Errorf("datastore name %q appears twice", ds.Name)
		}
		seen[ds.Name] = true
		switch {
		case strings.HasSuffix(ds.Name, "-vsanDatastore"):
			vsan++
		case strings.HasPrefix(ds.Name, "local-"):
			local++
			if used := ds.Summary.Capacity - ds.Summary.FreeSpace; used != ds.Summary.Capacity/10 {
				t.Errorf("%s uses %d of %d, want a tenth", ds.Name, used, ds.Summary.Capacity)
			}
		}
	}
	if vsan != 4 || local != 16 || !seen["dc-02-cluster-02-vsanDatastore"] {
		t.Errorf("%d vSAN and %d local datastores, names %v", vsan, local, seen)
	}
}
