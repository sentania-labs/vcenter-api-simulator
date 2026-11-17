package sim_test

import (
	"context"
	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// Operations resets the version on the same collector and filter at full sync.
// Its cache marks all prior objects absent, removing any not returned again.
func TestRepeatedEmptyVersionSnapshot(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "WaitForUpdatesEx", true: "WaitForUpdates"}[legacy], func(t *testing.T) {
			e := start(t, "resync")
			ctx := context.Background()
			pc, err := methods.CreatePropertyCollector(ctx, e.client, &types.CreatePropertyCollector{This: e.client.ServiceContent.PropertyCollector})
			if err != nil {
				t.Fatal(err)
			}
			ref := pc.Returnval
			filter, err := methods.CreateFilter(ctx, e.client, &types.CreateFilter{This: ref, Spec: types.PropertyFilterSpec{PropSet: []types.PropertySpec{{Type: "ClusterComputeResource", PathSet: []string{"name", "configuration"}}}, ObjectSet: []types.ObjectSpec{{Obj: e.cl().Ref}}}})
			if err != nil {
				t.Fatal(err)
			}
			var first map[types.ManagedObjectReference][]types.PropertyChange
			for sync := 0; sync < 3; sync++ {
				var update *types.UpdateSet
				if legacy {
					res, err := methods.WaitForUpdates(ctx, e.client, &types.WaitForUpdates{This: ref})
					if err != nil {
						t.Fatal(err)
					}
					update = &res.Returnval
				} else {
					res, err := methods.WaitForUpdatesEx(ctx, e.client, &types.WaitForUpdatesEx{This: ref, Options: &types.WaitOptions{MaxWaitSeconds: types.NewInt32(1)}})
					if err != nil {
						t.Fatal(err)
					}
					update = res.Returnval
				}
				objects := map[types.ManagedObjectReference][]types.PropertyChange{}
				for _, f := range update.FilterSet {
					if f.Filter != filter.Returnval {
						t.Fatal("filter identity changed")
					}
					for _, o := range f.ObjectSet {
						if o.Kind != types.ObjectUpdateKindEnter || len(o.ChangeSet) != 2 {
							t.Fatalf("incomplete enter: %+v", o)
						}
						objects[o.Obj] = o.ChangeSet
					}
				}
				if len(objects) != 1 {
					t.Fatalf("sync %d: got %d objects", sync, len(objects))
				}
				if sync == 0 {
					first = objects
				} else if !reflect.DeepEqual(first, objects) {
					t.Fatal("repeated snapshot changed")
				}
			}
			if _, err := methods.DestroyPropertyFilter(ctx, e.client, &types.DestroyPropertyFilter{This: filter.Returnval}); err != nil {
				t.Fatal(err)
			}
			// The destroyed filter must disappear from property reads and from
			// subsequent snapshots, even after its delegate was replaced.
			contents, err := methods.RetrievePropertiesEx(ctx, e.client, &types.RetrievePropertiesEx{This: ref, SpecSet: []types.PropertyFilterSpec{{PropSet: []types.PropertySpec{{Type: "PropertyCollector", PathSet: []string{"filter"}}}, ObjectSet: []types.ObjectSpec{{Obj: ref}}}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, obj := range contents.Returnval.Objects {
				for _, prop := range obj.PropSet {
					if refs, ok := prop.Val.(types.ArrayOfManagedObjectReference); ok && len(refs.ManagedObjectReference) != 0 {
						t.Fatal("destroyed filter still visible")
					}
				}
			}
			next, err := methods.WaitForUpdatesEx(ctx, e.client, &types.WaitForUpdatesEx{This: ref, Options: &types.WaitOptions{MaxWaitSeconds: types.NewInt32(0)}})
			if err != nil || next.Returnval == nil || len(next.Returnval.FilterSet) != 0 {
				t.Fatalf("destroyed filter replayed: %+v %v", next, err)
			}
			if _, err := methods.DestroyPropertyCollector(ctx, e.client, &types.DestroyPropertyCollector{This: ref}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSnapshotRestartAndIncremental(t *testing.T) {
	e := start(t, "resync-pages")
	ctx := context.Background()
	pc := e.client.ServiceContent.PropertyCollector
	spec := types.PropertyFilterSpec{PropSet: []types.PropertySpec{{Type: "VirtualMachine", PathSet: []string{"name", "runtime.powerState"}}}}
	for _, ref := range e.cl().VMs {
		spec.ObjectSet = append(spec.ObjectSet, types.ObjectSpec{Obj: ref})
	}
	var filters []types.ManagedObjectReference
	for i := 0; i < 2; i++ {
		f, err := methods.CreateFilter(ctx, e.client, &types.CreateFilter{This: pc, Spec: spec})
		if err != nil {
			t.Fatal(err)
		}
		filters = append(filters, f.Returnval)
	}
	read := func(version string) *types.UpdateSet {
		t.Helper()
		r, err := methods.WaitForUpdatesEx(ctx, e.client, &types.WaitForUpdatesEx{This: pc, Version: version, Options: &types.WaitOptions{MaxWaitSeconds: types.NewInt32(0), MaxObjectUpdates: 3}})
		if err != nil {
			t.Fatal(err)
		}
		return r.Returnval
	}
	if first := read(""); first.Truncated == nil || !*first.Truncated {
		t.Fatal("expected truncated snapshot")
	}
	// Restart before draining the first snapshot. Every object must reappear,
	// once per filter, with no tail from the abandoned snapshot.
	for sync := 0; sync < 3; sync++ {
		seen := map[types.ManagedObjectReference]map[types.ManagedObjectReference]bool{}
		version := ""
		for pages := 0; ; pages++ {
			if pages > 100 {
				t.Fatal("pagination did not finish")
			}
			u := read(version)
			if u == nil {
				t.Fatal("missing snapshot")
			}
			count := 0
			for _, f := range u.FilterSet {
				if seen[f.Filter] == nil {
					seen[f.Filter] = map[types.ManagedObjectReference]bool{}
				}
				for _, o := range f.ObjectSet {
					count++
					if seen[f.Filter][o.Obj] {
						t.Fatal("duplicate object")
					}
					seen[f.Filter][o.Obj] = true
					if o.Kind != types.ObjectUpdateKindEnter || len(o.ChangeSet) != 2 {
						t.Fatal("incomplete enter")
					}
				}
			}
			if count > 3 {
				t.Fatal("page exceeds maxObjectUpdates")
			}
			version = u.Version
			if u.Truncated == nil || !*u.Truncated {
				break
			}
		}
		for _, f := range filters {
			if len(seen[f]) != len(e.cl().VMs) {
				t.Fatalf("sync %d missing objects: %v", sync, seen)
			}
		}
		if u := read(version); u != nil {
			t.Fatalf("unexpected incremental replay: %+v", u)
		}
	}
	// A non-empty version still delivers a real property modification.
	_, err := methods.Rename_Task(ctx, e.client, &types.Rename_Task{This: e.cl().VMs[0], NewName: "resync-renamed"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for attempt := 0; attempt < 100; attempt++ {
		u := read("-")
		if u == nil {
			continue
		}
		for _, f := range u.FilterSet {
			for _, o := range f.ObjectSet {
				if o.Obj == e.cl().VMs[0] && o.Kind == types.ObjectUpdateKindModify {
					for _, p := range o.ChangeSet {
						if p.Name == "name" && p.Val == "resync-renamed" {
							found = true
						}
					}
				}
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("incremental name update missing")
	}
}

// Match the deployed lab cardinality, including cross-filter pagination.
func TestLargeInventoryResync(t *testing.T) {
	sc := scenario.Phase1("large-resync")
	sc.Datacenters[0].Clusters = 7
	sc.Defaults.Hosts = 32
	sc.Defaults.VMs.PerHost = scenario.IntPtr(23)
	sc.Clusters = []scenario.ClusterOverride{
		{Key: "dc-01/cluster-01", ClusterSpec: scenario.ClusterSpec{VMs: scenario.VMSpec{PerHost: scenario.IntPtr(24)}}},
		{Key: "dc-01/cluster-02", ClusterSpec: scenario.ClusterSpec{VMs: scenario.VMSpec{PerHost: scenario.IntPtr(24)}}},
		{Key: "dc-01/cluster-07", ClusterSpec: scenario.ClusterSpec{Hosts: 23}},
	}
	e := startTopology(t, topology.Generate(sc, registry.InMemory(sc.Seed)))
	ctx := context.Background()
	pc := e.client.ServiceContent.PropertyCollector
	spec := types.PropertyFilterSpec{PropSet: []types.PropertySpec{{Type: "ClusterComputeResource", PathSet: []string{"name", "configuration"}}, {Type: "HostSystem", PathSet: []string{"name", "parent"}}, {Type: "VirtualMachine", PathSet: []string{"name", "runtime", "config"}}}}
	hosts, vms := 0, 0
	for _, cl := range e.sim.Clusters {
		spec.ObjectSet = append(spec.ObjectSet, types.ObjectSpec{Obj: cl.Ref})
		for _, ref := range cl.Hosts {
			hosts++
			spec.ObjectSet = append(spec.ObjectSet, types.ObjectSpec{Obj: ref})
		}
		for _, ref := range cl.VMs {
			vms++
			spec.ObjectSet = append(spec.ObjectSet, types.ObjectSpec{Obj: ref})
		}
	}
	if hosts != 215 || vms != 5009 {
		t.Fatalf("wrong topology: %d hosts %d VMs", hosts, vms)
	}
	f, err := methods.CreateFilter(ctx, e.client, &types.CreateFilter{This: pc, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	var baseline map[types.ManagedObjectReference]bool
	for sync := 0; sync < 3; sync++ {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		started := time.Now()
		seen := map[types.ManagedObjectReference]bool{}
		version := ""
		pages := 0
		for {
			r, err := methods.WaitForUpdatesEx(ctx, e.client, &types.WaitForUpdatesEx{This: pc, Version: version, Options: &types.WaitOptions{MaxWaitSeconds: types.NewInt32(0), MaxObjectUpdates: 73}})
			if err != nil {
				t.Fatal(err)
			}
			u := r.Returnval
			if u == nil {
				t.Fatal("missing snapshot")
			}
			pages++
			count := 0
			for _, filter := range u.FilterSet {
				if filter.Filter != f.Returnval {
					t.Fatal("filter identity changed")
				}
				for _, o := range filter.ObjectSet {
					count++
					if seen[o.Obj] {
						t.Fatal("duplicate object")
					}
					seen[o.Obj] = true
					if o.Kind != types.ObjectUpdateKindEnter || len(o.ChangeSet) < 2 {
						t.Fatal("incomplete snapshot object")
					}
				}
			}
			if count > 73 {
				t.Fatal("oversized page")
			}
			version = u.Version
			if u.Truncated == nil || !*u.Truncated {
				break
			}
		}
		runtime.ReadMemStats(&after)
		t.Logf("full sync %d: objects=%d pages=%d elapsed=%s allocated_bytes=%d heap_bytes=%d", sync+1, len(seen), pages, time.Since(started), after.TotalAlloc-before.TotalAlloc, after.HeapAlloc)
		if len(seen) != 5231 {
			t.Fatalf("missing objects: %d", len(seen))
		}
		if sync == 0 {
			baseline = seen
		} else if !reflect.DeepEqual(baseline, seen) {
			t.Fatal("resync would remove objects")
		}
	}
}

// A full sync must not wait behind an earlier incremental long poll. The old
// request has no server timeout, as on a disconnected/orphaned adapter call.
func TestSnapshotInterruptsIncrementalWait(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "WaitForUpdatesEx", true: "WaitForUpdates"}[legacy], func(t *testing.T) {
			e := start(t, "resync-interrupt")
			ctx := context.Background()
			pc := e.client.ServiceContent.PropertyCollector
			f, err := methods.CreateFilter(ctx, e.client, &types.CreateFilter{This: pc, Spec: types.PropertyFilterSpec{PropSet: []types.PropertySpec{{Type: "ClusterComputeResource", PathSet: []string{"name", "configuration"}}}, ObjectSet: []types.ObjectSpec{{Obj: e.cl().Ref}}}})
			if err != nil {
				t.Fatal(err)
			}
			read := func(ctx context.Context, version string) (*types.UpdateSet, error) {
				if legacy {
					r, err := methods.WaitForUpdates(ctx, e.client, &types.WaitForUpdates{This: pc, Version: version})
					if err != nil {
						return nil, err
					}
					return &r.Returnval, nil
				}
				r, err := methods.WaitForUpdatesEx(ctx, e.client, &types.WaitForUpdatesEx{This: pc, Version: version})
				if err != nil {
					return nil, err
				}
				return r.Returnval, nil
			}
			first, err := read(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			oldDone := make(chan error, 1)
			go func() { _, err := read(ctx, first.Version); oldDone <- err }()
			// Always release the no-timeout server wait, including on the pre-fix
			// failure path, so a failed assertion cannot wedge server cleanup.
			defer func() { _, _ = methods.CancelWaitForUpdates(ctx, e.client, &types.CancelWaitForUpdates{This: pc}) }()
			// Allow the HTTP request to enter its long poll; it must remain blocked
			// until the new full-sync request interrupts it.
			time.Sleep(100 * time.Millisecond)
			select {
			case err := <-oldDone:
				t.Fatalf("incremental wait returned early: %v", err)
			default:
			}
			type result struct {
				update *types.UpdateSet
				err    error
			}
			snapshotDone := make(chan result, 1)
			resyncCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			go func() { u, err := read(resyncCtx, ""); snapshotDone <- result{u, err} }()
			select {
			case snapshot := <-snapshotDone:
				if snapshot.err != nil {
					t.Fatal(snapshot.err)
				}
				u := snapshot.update
				if u == nil || len(u.FilterSet) != 1 || u.FilterSet[0].Filter != f.Returnval || len(u.FilterSet[0].ObjectSet) != 1 {
					t.Fatalf("incomplete restarted snapshot: %+v", u)
				}
				object := u.FilterSet[0].ObjectSet[0]
				if object.Obj != e.cl().Ref || object.Kind != types.ObjectUpdateKindEnter || len(object.ChangeSet) != 2 {
					t.Fatalf("incomplete enter: %+v", object)
				}
			case <-time.After(time.Second):
				t.Fatal("empty-version resync blocked behind an in-flight incremental wait")
			}
			select {
			case err := <-oldDone:
				if !soap.IsSoapFault(err) {
					t.Fatalf("old wait was not canceled: %v", err)
				}
				if _, ok := soap.ToSoapFault(err).VimFault().(types.RequestCanceled); !ok {
					t.Fatalf("wrong old-wait fault: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("old incremental wait was not canceled")
			}
		})
	}
}
