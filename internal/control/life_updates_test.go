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

	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
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

// Opt in: a real SOAP subscription across the 215-host, 5,009-VM lab topology,
// with the adapter's host/VM leaf paths, collected every simulated five minutes.
func TestActivityLargeDayPropertyStream(t *testing.T) {
	if os.Getenv("VCSIM_LARGE_UPDATES") == "" {
		t.Skip("set VCSIM_LARGE_UPDATES=1 for full-size SOAP activity proof")
	}
	clk := &clock{t: monday9}
	c := newCtlAt(t, largeSize(t), "", "h", clk.now)
	srv := httptest.NewServer(c)
	defer srv.Close()
	client := activityClient(t, srv.URL)
	ctx := context.Background()
	pc := client.ServiceContent.PropertyCollector
	kinds := []string{"HostSystem", "VirtualMachine", "Datastore"}
	if os.Getenv("VCSIM_LARGE_UPDATES") == "scoped" {
		kinds = []string{"HostSystem", "Datastore"}
	}
	cv, err := view.NewManager(client).CreateContainerView(ctx, client.ServiceContent.RootFolder, kinds, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cv.Destroy(ctx)
	spec := types.PropertyFilterSpec{PropSet: []types.PropertySpec{
		{Type: "HostSystem", PathSet: []string{"runtime.inMaintenanceMode", "runtime.connectionState", "runtime.powerState", "vm"}},
		{Type: "VirtualMachine", PathSet: []string{"name", "runtime.host", "runtime.powerState"}},
		{Type: "Datastore", PathSet: []string{"summary"}},
	}, ObjectSet: []types.ObjectSpec{{Obj: cv.Reference(), Skip: types.NewBool(true), SelectSet: []types.BaseSelectionSpec{&types.TraversalSpec{Type: "ContainerView", Path: "view"}}}}}
	if _, err := methods.CreateFilter(ctx, client, &types.CreateFilter{This: pc, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	version := ""
	objects := 0
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
	t.Logf("property stream object types: %v", kinds)
	snapshot := collect()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := c.ActivityStart(true); err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	started := time.Now()
	for minute := 1; minute <= 1440; minute++ {
		clk.add(time.Minute)
		c.lifeStep()
		if minute%5 == 0 {
			waits = append(waits, collect())
		}
		if minute%360 == 0 {
			t.Logf("%d hours: elapsed=%s decoded=%d", minute/60, time.Since(started), objects)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	median := func(v []time.Duration) time.Duration {
		v = append([]time.Duration(nil), v...)
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		return v[len(v)/2]
	}
	t.Logf("215 hosts/5009 VMs: initial snapshot=%s; day=%s; heap before=%d MiB after=%d MiB; wait median first quarter=%s last quarter=%s max=%s; decoded objects=%d", snapshot, time.Since(started), before.HeapInuse>>20, after.HeapInuse>>20, median(waits[:72]), median(waits[216:]), func() time.Duration {
		sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
		return waits[len(waits)-1]
	}(), objects)
	if after.HeapInuse > before.HeapInuse+128<<20 {
		t.Error("activity retained over 128 MiB after a day")
	}
}
