package control

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// Starting Activity must change the maintenance stream Operations consumes.
func TestActivityStartMaintenancePropertyStream(t *testing.T) {
	clk := &clock{t: monday9}
	c := newCtlAt(t, lively(t), "", "h", clk.now)
	srv := httptest.NewServer(c)
	defer srv.Close()
	client := activityClient(t, srv.URL)
	pc := client.ServiceContent.PropertyCollector
	spec := types.PropertyFilterSpec{PropSet: []types.PropertySpec{{Type: "HostSystem", PathSet: []string{"runtime.inMaintenanceMode"}}}}
	for _, refs := range c.applied().sim.Clusters {
		for _, h := range refs.Hosts {
			spec.ObjectSet = append(spec.ObjectSet, types.ObjectSpec{Obj: h})
		}
	}
	if _, err := methods.CreateFilter(context.Background(), client, &types.CreateFilter{This: pc, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	first, err := methods.WaitForUpdatesEx(context.Background(), client, &types.WaitForUpdatesEx{This: pc, Options: &types.WaitOptions{MaxWaitSeconds: types.NewInt32(0)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ActivityStart(true); err != nil {
		t.Fatal(err)
	}
	next, err := methods.WaitForUpdatesEx(context.Background(), client, &types.WaitForUpdatesEx{This: pc, Version: first.Returnval.Version, Options: &types.WaitOptions{MaxWaitSeconds: types.NewInt32(1)}})
	if err != nil {
		t.Fatal(err)
	}
	if next.Returnval != nil {
		for _, f := range next.Returnval.FilterSet {
			for _, o := range f.ObjectSet {
				for _, p := range o.ChangeSet {
					if p.Name == "runtime.inMaintenanceMode" && p.Val == true {
						t.Logf("Start Activity: decoded %s %s=%v", o.Obj, p.Name, p.Val)
						return
					}
				}
			}
		}
	}
	t.Fatal("Start Activity did not deliver maintenance to the waiting filter")
}

func activityClient(t *testing.T, endpoint string) *vim25.Client {
	t.Helper()
	u, err := soap.ParseURL(endpoint + "/sdk")
	if err != nil {
		t.Fatal(err)
	}
	sc := soap.NewClient(u, true)
	client, err := vim25.NewClient(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.NewManager(client).Login(context.Background(), url.UserPassword("administrator@vsphere.local", "first-password")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.NewManager(client).Logout(context.Background()); sc.CloseIdleConnections() })
	return client
}

// adapterTraversal is the selection set the VCF Operations adapter attaches to
// each explicit root object (MiscUtils.buildFullTraversalV4).
func adapterTraversal() []types.BaseSelectionSpec {
	sel := func(names ...string) []types.BaseSelectionSpec {
		var out []types.BaseSelectionSpec
		for _, n := range names {
			out = append(out, &types.SelectionSpec{Name: n})
		}
		return out
	}
	ts := func(name, typ, path string, skip bool, next ...string) *types.TraversalSpec {
		return &types.TraversalSpec{SelectionSpec: types.SelectionSpec{Name: name}, Type: typ, Path: path, Skip: types.NewBool(skip), SelectSet: sel(next...)}
	}
	return []types.BaseSelectionSpec{
		ts("visitFolders", "Folder", "childEntity", false, "visitFolders", "dcToHf", "dcToVmf", "dcToDs", "dcToNetf", "crToH", "crToRp", "HToVm", "rpToVm"),
		ts("dcToDs", "Datacenter", "datastoreFolder", false, "visitFolders"),
		ts("dcToNetf", "Datacenter", "networkFolder", false, "visitFolders"),
		ts("vAppToRp", "VirtualApp", "resourcePool", false, "rpToRp", "vAppToRp"),
		ts("rpToRp", "ResourcePool", "resourcePool", false, "rpToRp", "rpToVm"),
		ts("rpToVm", "ResourcePool", "vm", false),
		ts("crToRp", "ComputeResource", "resourcePool", false, "rpToRp", "rpToVm"),
		ts("crToH", "ComputeResource", "host", false),
		ts("dcToHf", "Datacenter", "hostFolder", false, "visitFolders"),
		ts("dcToVmf", "Datacenter", "vmFolder", false, "visitFolders"),
		ts("HToVm", "HostSystem", "vm", false, "visitFolders"),
	}
}

// Opt in: a real SOAP subscription across the 215-host, 5,009-VM lab topology,
// shaped like the Operations adapter's (explicit datacenter roots with the full
// traversal, host/VM/datastore/cluster paths, partialUpdates=false), collected
// every simulated five minutes for two simulated hours at default Activity dials.
func TestActivityLargePropertyStream(t *testing.T) {
	if os.Getenv("VCSIM_LARGE_UPDATES") == "" {
		t.Skip("set VCSIM_LARGE_UPDATES=1 for full-size SOAP activity measurement")
	}
	clk := &clock{t: monday9}
	c := newCtlAt(t, largeSize(t), "", "h", clk.now)
	srv := httptest.NewServer(c)
	defer srv.Close()
	client := activityClient(t, srv.URL)
	ctx := context.Background()
	pc := client.ServiceContent.PropertyCollector
	var dcs []mo.Folder
	if err := property.DefaultCollector(client).Retrieve(ctx, []types.ManagedObjectReference{client.ServiceContent.RootFolder}, []string{"childEntity"}, &dcs); err != nil {
		t.Fatal(err)
	}
	spec := types.PropertyFilterSpec{PropSet: []types.PropertySpec{
		{Type: "HostSystem", PathSet: []string{"runtime.inMaintenanceMode", "runtime.connectionState", "runtime.powerState", "vm", "summary.quickStats", "summary.overallStatus"}},
		{Type: "VirtualMachine", PathSet: []string{"name", "runtime.host", "runtime.powerState", "runtime.connectionState", "summary.quickStats", "summary.runtime", "snapshot"}},
		{Type: "Datastore", PathSet: []string{"summary", "info"}},
		{Type: "ClusterComputeResource", PathSet: []string{"name", "host", "summary"}},
	}}
	for _, o := range dcs[0].ChildEntity {
		spec.ObjectSet = append(spec.ObjectSet, types.ObjectSpec{Obj: o, Skip: types.NewBool(false), SelectSet: adapterTraversal()})
	}
	if len(spec.ObjectSet) == 0 {
		t.Fatal("no datacenter roots")
	}
	if _, err := methods.CreateFilter(ctx, client, &types.CreateFilter{This: pc, Spec: spec, PartialUpdates: false}); err != nil {
		t.Fatal(err)
	}
	version := ""
	objects, changes := 0, 0
	collect := func() time.Duration {
		t.Helper()
		start := time.Now()
		for {
			res, err := methods.WaitForUpdatesEx(ctx, client, &types.WaitForUpdatesEx{This: pc, Version: version, Options: &types.WaitOptions{MaxWaitSeconds: types.NewInt32(0)}})
			if err != nil {
				t.Fatal(err)
			}
			if res.Returnval == nil {
				break
			}
			version = res.Returnval.Version
			for _, f := range res.Returnval.FilterSet {
				objects += len(f.ObjectSet)
				for _, o := range f.ObjectSet {
					changes += len(o.ChangeSet)
				}
			}
			if res.Returnval.Truncated == nil || !*res.Returnval.Truncated {
				break
			}
		}
		return time.Since(start)
	}
	hosts, vms := 0, 0
	for _, refs := range c.applied().sim.Clusters {
		hosts += len(refs.Hosts)
		vms += len(refs.VMs)
	}
	if hosts != 215 || vms != 5009 {
		t.Fatalf("wrong topology %d hosts/%d VMs", hosts, vms)
	}
	snapshot := collect()
	snapObjects := objects
	t.Logf("initial snapshot: %s, %d objects, %d property values", snapshot, objects, changes)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := c.ActivityStart(true); err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	started := time.Now()
	for minute := 1; minute <= 120; minute++ {
		clk.add(time.Minute)
		c.lifeStep()
		if minute%5 == 0 {
			prev := objects
			waits = append(waits, collect())
			t.Logf("minute %d: wait=%s updates=%d", minute, waits[len(waits)-1], objects-prev)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	t.Logf("215 hosts/5009 VMs, 2h: initial snapshot=%s (%d objects); elapsed=%s; heap before=%d MiB after=%d MiB; WaitForUpdatesEx median=%s max=%s over %d collections; decoded %d object updates, %d property changes", snapshot, snapObjects, time.Since(started), before.HeapInuse>>20, after.HeapInuse>>20, waits[len(waits)/2], waits[len(waits)-1], len(waits), objects-snapObjects, changes)
	if objects == snapObjects {
		t.Error("no activity updates were delivered")
	}
	if after.HeapInuse > before.HeapInuse+128<<20 {
		t.Error("activity retained over 128 MiB after two hours")
	}
}
