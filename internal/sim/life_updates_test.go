package sim_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/types"
)

// Operations 9.1.1 PropertyRetriever requests leaf paths with partialUpdates=false.
// A fresh read and the incremental filter must both report the maintenance flag.
func TestActivityMaintenanceUpdates(t *testing.T) {
	e := start(t, "activity-updates")
	ctx := context.Background()
	pc := e.client.ServiceContent.PropertyCollector
	host := e.cl().Hosts[0]
	spec := types.PropertyFilterSpec{PropSet: []types.PropertySpec{{Type: "HostSystem", PathSet: []string{"runtime.inMaintenanceMode"}}}, ObjectSet: []types.ObjectSpec{{Obj: host}}}
	if _, err := methods.CreateFilter(ctx, e.client, &types.CreateFilter{This: pc, Spec: spec, PartialUpdates: false}); err != nil {
		t.Fatal(err)
	}
	first, err := methods.WaitForUpdatesEx(ctx, e.client, &types.WaitForUpdatesEx{This: pc, Options: &types.WaitOptions{MaxWaitSeconds: types.NewInt32(0)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.sim.Life().EnterMaintenance(e.cl().Cluster.Key, e.cl().Cluster.Hosts[0].Name); err != nil {
		t.Fatal(err)
	}
	fresh, err := methods.RetrievePropertiesEx(ctx, e.client, &types.RetrievePropertiesEx{This: pc, SpecSet: []types.PropertyFilterSpec{spec}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fresh read: %+v", fresh.Returnval.Objects[0].PropSet)
	next, err := methods.WaitForUpdatesEx(ctx, e.client, &types.WaitForUpdatesEx{This: pc, Version: first.Returnval.Version, Options: &types.WaitOptions{MaxWaitSeconds: types.NewInt32(1)}})
	if err != nil {
		t.Fatal(err)
	}
	if next.Returnval != nil {
		for _, f := range next.Returnval.FilterSet {
			for _, o := range f.ObjectSet {
				t.Logf("decoded update: %s %s %+v", o.Obj, o.Kind, o.ChangeSet)
				for _, p := range o.ChangeSet {
					if o.Obj == host && p.Name == "runtime.inMaintenanceMode" && p.Val == true {
						return
					}
				}
			}
		}
	}
	t.Fatal("fresh read sees maintenance but waiting filter received no maintenance update")
}

// Every changed property in a fresh adapter-style inventory must be present in
// the incremental stream. This catches mutations that bypass publication, even
// when an Activity log and the fresh read both look correct.
func TestActivityReadAndUpdateAgreement(t *testing.T) {
	e := activityUpdateEnv(t)
	ctx := context.Background()
	vmView, err := view.NewManager(e.client).CreateContainerView(ctx, e.client.ServiceContent.RootFolder, []string{"HostSystem", "VirtualMachine", "Datastore", "ResourcePool", "Folder", "Network", "ClusterComputeResource", "Datacenter"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer vmView.Destroy(ctx)
	spec := types.PropertyFilterSpec{PropSet: []types.PropertySpec{
		{Type: "HostSystem", PathSet: []string{"runtime.inMaintenanceMode", "runtime.connectionState", "runtime.powerState", "summary.runtime.inMaintenanceMode", "summary.runtime.connectionState", "summary.runtime.powerState", "runtime.bootTime", "summary.runtime.bootTime", "summary.quickStats.overallCpuUsage", "summary.quickStats.overallMemoryUsage", "config.product.build", "config.product.patchLevel", "config.product.fullName", "summary.config.product.build", "summary.config.product.patchLevel", "summary.config.product.fullName", "triggeredAlarmState", "overallStatus", "vm"}},
		{Type: "VirtualMachine", PathSet: []string{"name", "runtime.host", "runtime.powerState", "runtime.connectionState", "runtime.faultToleranceState", "runtime.maxCpuUsage", "runtime.bootTime", "summary.runtime.powerState", "summary.runtime.connectionState", "summary.runtime.host", "resourcePool", "datastore", "snapshot", "snapshot.currentSnapshot", "snapshot.rootSnapshotList", "rootSnapshot", "layoutEx.file", "layoutEx.snapshot", "layoutEx.disk", "layoutEx.timestamp", "guest", "summary.config", "summary.guest", "summary.quickStats.overallCpuUsage", "summary.quickStats.hostMemoryUsage"}},
		{Type: "Datastore", PathSet: []string{"summary", "summary.capacity", "summary.freeSpace", "summary.uncommitted", "info", "info.freeSpace", "vm"}},
		{Type: "ResourcePool", PathSet: []string{"vm"}}, {Type: "Folder", PathSet: []string{"childEntity", "triggeredAlarmState", "overallStatus"}},
		{Type: "Network", PathSet: []string{"vm"}}, {Type: "ClusterComputeResource", PathSet: []string{"host", "resourcePool", "summary", "triggeredAlarmState", "overallStatus"}},
		{Type: "Datacenter", PathSet: []string{"triggeredAlarmState", "overallStatus"}},
	}, ObjectSet: []types.ObjectSpec{{Obj: vmView.Reference(), Skip: types.NewBool(true), SelectSet: []types.BaseSelectionSpec{&types.TraversalSpec{Type: "ContainerView", Path: "view"}}}}}
	events, err := methods.CreateCollectorForEvents(ctx, e.client, &types.CreateCollectorForEvents{This: *e.client.ServiceContent.EventManager})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := methods.CreateCollectorForTasks(ctx, e.client, &types.CreateCollectorForTasks{This: *e.client.ServiceContent.TaskManager})
	if err != nil {
		t.Fatal(err)
	}
	spec.PropSet = append(spec.PropSet, types.PropertySpec{Type: "EventHistoryCollector", PathSet: []string{"latestPage"}}, types.PropertySpec{Type: "TaskHistoryCollector", PathSet: []string{"latestPage"}})
	spec.ObjectSet = append(spec.ObjectSet, types.ObjectSpec{Obj: events.Returnval}, types.ObjectSpec{Obj: tasks.Returnval})
	pc := e.client.ServiceContent.PropertyCollector
	// Keep overlapping parents in a separate filter: govmomi selects the
	// first matching path within each PropertySpec. Both filters mirror the
	// same fresh-read specification and are decoded through the SOAP client.
	leaves, parents := spec, spec
	leaves.PropSet = nil
	parents.PropSet = nil
	for _, props := range spec.PropSet {
		leaf, parent := props, props
		leaf.PathSet = nil
		parent.PathSet = nil
		for _, path := range props.PathSet {
			hasChild := false
			for _, other := range props.PathSet {
				if strings.HasPrefix(other, path+".") {
					hasChild = true
				}
			}
			if hasChild {
				parent.PathSet = append(parent.PathSet, path)
			} else {
				leaf.PathSet = append(leaf.PathSet, path)
			}
		}
		if len(leaf.PathSet) > 0 {
			leaves.PropSet = append(leaves.PropSet, leaf)
		}
		if len(parent.PathSet) > 0 {
			parents.PropSet = append(parents.PropSet, parent)
		}
	}
	for _, one := range []types.PropertyFilterSpec{leaves, parents} {
		if _, err := methods.CreateFilter(ctx, e.client, &types.CreateFilter{This: pc, Spec: one, PartialUpdates: false}); err != nil {
			t.Fatal(err)
		}
	}
	version := ""
	drain := func() map[types.ManagedObjectReference]types.ObjectUpdate {
		t.Helper()
		out := map[types.ManagedObjectReference]types.ObjectUpdate{}
		for {
			r, err := methods.WaitForUpdatesEx(ctx, e.client, &types.WaitForUpdatesEx{This: pc, Version: version, Options: &types.WaitOptions{MaxWaitSeconds: types.NewInt32(0)}})
			if err != nil {
				t.Fatal(err)
			}
			if r.Returnval == nil {
				break
			}
			version = r.Returnval.Version
			for _, f := range r.Returnval.FilterSet {
				for _, o := range f.ObjectSet {
					prev, ok := out[o.Obj]
					if ok && o.Kind != types.ObjectUpdateKindLeave {
						o.ChangeSet = append(prev.ChangeSet, o.ChangeSet...)
						if prev.Kind == types.ObjectUpdateKindEnter {
							o.Kind = prev.Kind
						}
					}
					out[o.Obj] = o
				}
			}
			if r.Returnval.Truncated == nil || !*r.Returnval.Truncated {
				break
			}
		}
		return out
	}
	read := func() map[types.ManagedObjectReference]map[string]any {
		t.Helper()
		r, err := methods.RetrievePropertiesEx(ctx, e.client, &types.RetrievePropertiesEx{This: pc, SpecSet: []types.PropertyFilterSpec{spec}})
		if err != nil {
			t.Fatal(err)
		}
		out := map[types.ManagedObjectReference]map[string]any{}
		for _, o := range r.Returnval.Objects {
			p := map[string]any{}
			for _, v := range o.PropSet {
				p[v.Name] = v.Val
			}
			out[o.Obj] = p
		}
		return out
	}
	drain()
	l := e.sim.Life()
	key := e.cl().Cluster.Key
	host := e.cl().Cluster.Hosts[0].Name
	var off string
	var crashed []string
	var backupVM string
	for _, tc := range []struct {
		name string
		do   func()
	}{
		{"moves", func() {
			if len(l.Balance(key, 1, 1)) != 1 {
				t.Fatal("no move")
			}
		}},
		{"power-off", func() {
			off = l.PowerOff(key, 0)
			if off == "" {
				t.Fatal("no power off")
			}
		}},
		{"power-on", func() {
			if err := l.PowerOn(key, off); err != nil {
				t.Fatal("no power on")
			}
		}},
		{"maintenance-enter", func() {
			if _, err := l.EnterMaintenance(key, host); err != nil {
				t.Fatal(err)
			}
		}},
		{"maintenance-exit", func() { l.ExitMaintenance(key, host, 1) }},
		{"adds", func() {
			if _, _, err := l.AddVM(key, 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"removes", func() {
			if _, err := l.RemoveVM(key, 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"disk-growth", func() { l.Fill(key, 0.01) }},
		{"disk-shrink", func() { l.Fill(key, -0.005) }},
		{"cpu", func() { l.SetLoad(key, 2, 1) }},
		{"memory", func() { l.SetLoad(key, 2, 2) }},
		{"backup-create", func() {
			backupVM = l.VMNames(key)[0]
			if err := l.SnapshotVM(key, backupVM); err != nil {
				t.Fatal(err)
			}
		}},
		{"backup-remove", func() {
			if err := l.RemoveBackup(key, backupVM); err != nil {
				t.Fatal(err)
			}
		}},
		{"host-crash", func() {
			var err error
			crashed, err = l.CrashHost(key, host)
			if err != nil {
				t.Fatal(err)
			}
			if len(crashed) == 0 {
				t.Fatal("crash affected no VMs")
			}
		}},
		{"HA-restart", func() {
			moves, err := l.RestartVMs(key, host, crashed)
			if err != nil {
				t.Fatal(err)
			}
			if len(moves) != len(crashed) {
				t.Fatal("HA did not restart all VMs")
			}
		}},
		{"host-recover", func() {
			if err := l.RecoverHost(key, host); err != nil {
				t.Fatal(err)
			}
		}},
		{"patch-evacuate", func() {
			if _, err := l.EnterMaintenance(key, host); err != nil {
				t.Fatal(err)
			}
		}},
		{"patch-reboot", func() {
			if err := l.PatchHost(key, host); err != nil {
				t.Fatal(err)
			}
		}},
		{"patch-return", func() {
			if err := l.FinishReboot(key, host); err != nil {
				t.Fatal(err)
			}
		}},
		{"patch-exit", func() { l.ExitMaintenance(key, host, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := read()
			tc.do()
			after := read()
			updates := drain()
			changed := 0
			for ref, props := range after {
				if _, ok := before[ref]; !ok {
					if updates[ref].Kind != types.ObjectUpdateKindEnter {
						t.Errorf("missing enter %s", ref)
					}
					changed++
				}
				for name, value := range props {
					if strings.HasPrefix(name, "summary.quickStats.") && tc.name != "cpu" && tc.name != "memory" && tc.name != "host-crash" && tc.name != "HA-restart" && tc.name != "host-recover" {
						continue
					}
					if reflect.DeepEqual(before[ref][name], value) {
						continue
					}
					changed++
					found := false
					for _, c := range updates[ref].ChangeSet {
						if c.Name == name && reflect.DeepEqual(c.Val, value) {
							found = true
						}
					}
					if !found {
						t.Errorf("fresh read changed %s %s without matching leaf update", ref, name)
					}
				}
			}
			for ref, oldProps := range before {
				if newProps, exists := after[ref]; exists {
					for name := range oldProps {
						if _, exists := newProps[name]; exists {
							continue
						}
						changed++
						found := false
						for _, change := range updates[ref].ChangeSet {
							if change.Name == name && change.Op == types.PropertyChangeOpRemove {
								found = true
							}
						}
						if !found {
							t.Errorf("fresh read removed %s %s without matching remove update", ref, name)
						}
					}
				}
				if _, ok := after[ref]; !ok {
					if updates[ref].Kind != types.ObjectUpdateKindLeave {
						t.Errorf("missing leave %s", ref)
					}
					changed++
				}
			}
			if changed == 0 {
				t.Fatal("behaviour changed no subscribed state")
			}
			t.Logf("%s: %d fresh property changes verified against decoded SOAP updates", tc.name, changed)
		})
	}
}

func activityUpdateEnv(t *testing.T) *env {
	t.Helper()
	sc := scenario.Phase1("activity-updates")
	sc.Datacenters[0].Clusters = 1
	sc.Defaults.Hosts = 4
	sc.Defaults.VMs.PerHost = scenario.IntPtr(5)
	sc.Vary = scenario.Vary{}
	sc.Clusters = nil
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	reg := registry.InMemory("activity-updates")
	e := startWith(t, topology.Generate(sc, reg), sim.Options{Registry: reg})
	if err := e.login("user", "pass"); err != nil {
		t.Fatal(err)
	}
	return e
}
