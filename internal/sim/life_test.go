package sim

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// lifeScenario is two clusters of four hosts and twenty VMs.
func lifeScenario(t *testing.T) *scenario.Scenario {
	t.Helper()
	sc := scenario.Phase1("life")
	sc.Datacenters[0].Clusters = 2
	sc.Defaults.Hosts = 4
	sc.Defaults.VMs.PerHost = scenario.IntPtr(5)
	sc.Vary = scenario.Vary{}
	sc.Clusters = nil
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	return sc
}

func lifeSim(t *testing.T, sc *scenario.Scenario, reg *registry.Registry) *Simulator {
	t.Helper()
	s, err := New(Options{Topology: topology.Generate(sc, reg), Registry: reg, Now: func() time.Time { return time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// events returns what vCenter's event history holds with a key above
// after, oldest first. mark is the newest key there is: the events the
// inventory builder caused come before it.
func events(s *Simulator, after int32) []types.BaseEvent {
	ctx := s.Life().as("")
	em := ctx.Map.EventManager()
	ref := em.CreateCollectorForEvents(ctx, &types.CreateCollectorForEvents{This: em.Self}).(*methods.CreateCollectorForEventsBody).Res.Returnval
	c := ctx.Session.Get(ref).(*simulator.EventHistoryCollector)
	c.SetCollectorPageSize(ctx, &types.SetCollectorPageSize{This: ref, MaxCount: 1000})
	var out []types.BaseEvent
	for _, e := range c.GetLatestPage() {
		if e.GetEvent().Key > after {
			out = append(out, e)
		}
	}
	c.DestroyCollector(ctx, &types.DestroyCollector{This: ref})
	sort.Slice(out, func(i, j int) bool { return out[i].GetEvent().Key < out[j].GetEvent().Key })
	return out
}

func mark(s *Simulator) (key int32) {
	for _, e := range events(s, 0) {
		key = max(key, e.GetEvent().Key)
	}
	return
}

func kindOf(e types.BaseEvent) string { return eventName(e) }

// recentTasks is the tasks vCenter lists that were made after the task
// numbered after.
func recentTasks(l *Life, after int64) (out []types.TaskInfo) {
	tm := l.reg.Get(*l.s.model.ServiceContent.TaskManager).(*simulator.TaskManager)
	for _, ref := range tm.RecentTask {
		if t, ok := l.reg.Get(ref).(*simulator.Task); ok && refNumber(ref) > after {
			out = append(out, t.Info)
		}
	}
	return
}

// lastTask is the number of the newest task vCenter lists.
func lastTask(l *Life) (n int64) {
	tm := l.reg.Get(*l.s.model.ServiceContent.TaskManager).(*simulator.TaskManager)
	for _, ref := range tm.RecentTask {
		n = max(n, refNumber(ref))
	}
	return
}

// chainsOf groups events by the chain they carry, each chain's events in
// the order they were posted, and the chains in the order they began.
func chainsOf(evs []types.BaseEvent) (order []int32, by map[int32][]types.BaseEvent) {
	by = map[int32][]types.BaseEvent{}
	for _, e := range evs {
		c := e.GetEvent().ChainId
		if by[c] == nil {
			order = append(order, c)
		}
		by[c] = append(by[c], e)
	}
	return
}

// chainIs checks one operation's events against what a real vCenter
// records: a TaskEvent first, whose key every event carries as its chain
// id, with the task as queued, then the named events; and a finished task
// that carries the chain too.
func chainIs(t *testing.T, l *Life, evs []types.BaseEvent, description, user string, kinds ...string) {
	t.Helper()
	var got []string
	for _, e := range evs {
		got = append(got, kindOf(e))
	}
	want := append([]string{"TaskEvent"}, kinds...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: events %v, want %v", description, got, want)
		return
	}
	head := evs[0].(*types.TaskEvent)
	for _, e := range evs {
		if b := e.GetEvent(); b.ChainId != head.Key || b.UserName != user || b.FullFormattedMessage == "" {
			t.Errorf("%s: %s has chain %d (the TaskEvent is %d), user %q and message %q", description, kindOf(e), b.ChainId, head.Key, b.UserName, b.FullFormattedMessage)
		}
	}
	if i := head.Info; i.DescriptionId != description || i.State != types.TaskInfoStateQueued || i.Task.Value == "" || i.Key != i.Task.Value || i.EventChainId != 0 || i.Entity == nil || i.EntityName == "" || head.FullFormattedMessage != "Task: "+description {
		t.Errorf("%s: the TaskEvent carries %+v and says %q", description, i, head.FullFormattedMessage)
	}
	task, ok := l.reg.Get(head.Info.Task).(*simulator.Task)
	if !ok || task.Info.EventChainId != head.Key || task.Info.State != types.TaskInfoStateSuccess || task.Info.DescriptionId != description {
		t.Errorf("%s: no finished task carries chain %d", description, head.Key)
		return
	}
	switch r := task.Info.Reason.(type) {
	case *types.TaskReasonUser:
		if user == "" || r.UserName != user {
			t.Errorf("%s: started by user %q, want %q", description, r.UserName, user)
		}
	case *types.TaskReasonSystem:
		if user != "" {
			t.Errorf("%s: started by the system, want %q", description, user)
		}
	}
}

// placement checks that every VM is on exactly one host's list, and that it
// is the host the VM says it is on.
func placement(t *testing.T, s *Simulator) map[string]string {
	t.Helper()
	ctx := s.Life().as("")
	where := map[string]string{}
	for _, refs := range s.Clusters {
		for i, href := range refs.Hosts {
			hs := ctx.Map.Get(href).(*simulator.HostSystem)
			for _, v := range hs.Vm {
				vm := ctx.Map.Get(v).(*simulator.VirtualMachine)
				if where[vm.Name] != "" {
					t.Fatalf("%s is listed on two hosts", vm.Name)
				}
				if vm.Runtime.Host == nil || *vm.Runtime.Host != href || *vm.Summary.Runtime.Host != href {
					t.Fatalf("%s is listed on %s but says it is on %v", vm.Name, hs.Name, vm.Runtime.Host)
				}
				if hs.Runtime.InMaintenanceMode {
					t.Fatalf("%s is on %s, which is in maintenance mode", vm.Name, hs.Name)
				}
				where[vm.Name] = refs.Cluster.Hosts[i].Name
			}
		}
	}
	return where
}

func TestLifeBalanceMovesWithEventsAndTasks(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("life"))
	l := s.Life()
	key := s.Clusters[0].Cluster.Key
	before := placement(t, s)
	m0 := mark(s)
	moves := l.Balance(key, 3, 7)
	if len(moves) != 3 {
		t.Fatalf("moved %d VMs, want 3", len(moves))
	}
	after := placement(t, s)
	if len(after) != 40 {
		t.Fatalf("%d VMs are on a host after the moves, want 40", len(after))
	}
	seen, gave, took := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, m := range moves {
		if seen[m.VM] || before[m.VM] != m.From || after[m.VM] != m.To || m.From == m.To {
			t.Errorf("move %+v does not match where the VM was (%s) and is (%s), or the VM moved twice", m, before[m.VM], after[m.VM])
		}
		seen[m.VM], gave[m.From], took[m.To] = true, true, true
	}
	for h := range gave {
		if took[h] {
			t.Errorf("%s both gave and took a VM in one pass", h)
		}
	}
	// each move is a chain of its own, as DRS records a vMotion
	order, by := chainsOf(events(s, m0))
	if len(order) != 3 {
		t.Fatalf("%d chains of events, want one for each of the 3 moves", len(order))
	}
	for _, c := range order {
		chainIs(t, l, by[c], "Drm.ExecuteVMotionLRO", "", "VmHotMigratingWithEncryptionEvent", "VmEmigratingEvent", "DrsVmMigratedEvent")
		if len(by[c]) != 4 {
			continue
		}
		head, start, done := by[c][0].(*types.TaskEvent), by[c][1].(*types.EventEx), by[c][3].(*types.DrsVmMigratedEvent)
		if head.Info.Name != "" || *head.Info.Entity != done.Vm.Vm || head.Vm == nil || head.Host == nil || head.Ds != nil {
			t.Errorf("the TaskEvent of a vMotion: name %q, %+v", head.Info.Name, head.Event)
		}
		if done.SourceHost.Name == done.Host.Name || start.Host.Name != done.SourceHost.Name || len(start.Arguments) != 3 || start.Arguments[0].Value != done.Host.Name || start.ObjectId != done.Vm.Vm.Value || !strings.Contains(start.FullFormattedMessage, "to "+done.Host.Name) {
			t.Errorf("the vMotion from %s to %s starts with %+v", done.SourceHost.Name, done.Host.Name, start)
		}
	}
	// old tasks are forgotten
	t0 := lastTask(l) - 3
	if n := len(recentTasks(l, t0)); n != 3 {
		t.Fatalf("vCenter lists %d tasks for the 3 moves", n)
	}
	l.Sweep(time.Now().Add(11 * time.Minute))
	if n := len(recentTasks(l, t0)); len(l.old) != 0 || n != 0 {
		t.Errorf("%d tasks are still listed after ten minutes", n)
	}
}

func TestLifeMaintenanceEmptiesTheHostFirst(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("life"))
	l := s.Life()
	refs := s.Clusters[0]
	host := refs.Cluster.Hosts[1].Name
	m0 := mark(s)
	moves, err := l.EnterMaintenance(refs.Cluster.Key, host)
	if err != nil || len(moves) != 5 {
		t.Fatalf("EnterMaintenance moved %d VMs (%v), want the host's 5", len(moves), err)
	}
	where := placement(t, s)
	for vm, h := range where {
		if h == host {
			t.Errorf("%s is still on %s", vm, host)
		}
	}
	// the host's own chain, and one for each VM that DRS moved off it
	order, by := chainsOf(events(s, m0))
	if len(order) != 6 {
		t.Fatalf("%d chains of events, want the host's and one for each of its 5 VMs", len(order))
	}
	chainIs(t, l, by[order[0]], "HostSystem.enterMaintenanceMode", lifeUser, "EnteringMaintenanceModeEvent", "HostDasDisabledEvent", "EnteredMaintenanceModeEvent")
	last := by[order[0]][len(by[order[0]])-1].GetEvent().Key
	for _, c := range order[1:] {
		chainIs(t, l, by[c], "Drm.ExecuteVMotionLRO", "", "VmHotMigratingWithEncryptionEvent", "VmEmigratingEvent", "DrsVmMigratedEvent")
		if k := by[c][len(by[c])-1].GetEvent().Key; k > last {
			t.Errorf("a VM left the host (event %d) after the host said it was in maintenance mode (event %d)", k, last)
		}
	}
	cs := l.Clusters()
	if !cs[0].Hosts[1].Maintenance || cs[0].Hosts[1].VMs != 0 || cs[0].VMs != 20 {
		t.Errorf("the engine sees %+v", cs[0].Hosts[1])
	}
	// the performance model sees a host that carries nothing
	state := readPerformanceState(l.as(""))
	if n := state.nodes[refs.Hosts[1]]; n.active {
		t.Error("a host in maintenance mode still counts as carrying load")
	}
	m1 := mark(s)
	back := l.ExitMaintenance(refs.Cluster.Key, host, 3)
	if len(back) == 0 || len(back) > 4 {
		t.Errorf("%d VMs moved back onto the host, want a few", len(back))
	}
	order, by = chainsOf(events(s, m1))
	if len(order) != 1+len(back) {
		t.Fatalf("%d chains of events after the host came back, want its own and %d moves", len(order), len(back))
	}
	chainIs(t, l, by[order[0]], "HostSystem.exitMaintenanceMode", lifeUser, "ExitMaintenanceModeEvent", "HostDasEnabledEvent")
	placement(t, s)
}

func TestLifePowerShowsInStateAndPerformance(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("life"))
	l := s.Life()
	refs := s.Clusters[0]
	m0 := mark(s)
	name := l.PowerOff(refs.Cluster.Key, 4)
	if name == "" {
		t.Fatal("nothing was powered off")
	}
	vm := l.vms[refs.Cluster.Key][name]
	obj := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
	if obj.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOff || obj.Guest.GuestState != "notRunning" || obj.Summary.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOff || obj.GuestHeartbeatStatus != types.ManagedEntityStatusGray {
		t.Errorf("a powered-off VM says: power %s, guest %s, heartbeat %s", obj.Runtime.PowerState, obj.Guest.GuestState, obj.GuestHeartbeatStatus)
	}
	at := time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC)
	if w := readPerformanceState(l.as("")).sample("life", at)[vm.ref]; w.cpu != 0 || w.memory != 0 {
		t.Errorf("a powered-off VM still reports cpu %d and memory %d", w.cpu, w.memory)
	}
	chainIs(t, l, events(s, m0), "VirtualMachine.powerOff", lifeUser, "VmStoppingEvent", "VmPoweredOffEvent")
	if c := l.Clusters()[0]; c.Off != 1 {
		t.Errorf("the engine counts %d VMs off, want 1", c.Off)
	}
	m1 := mark(s)
	if !l.PowerOn(refs.Cluster.Key, name) || obj.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOn || obj.Guest.GuestState != "running" {
		t.Errorf("after power on: %s, guest %s", obj.Runtime.PowerState, obj.Guest.GuestState)
	}
	chainIs(t, l, events(s, m1), "VirtualMachine.powerOn", lifeUser, "VmStartingEvent", "VmPoweredOnEvent")
	if w := readPerformanceState(l.as("")).sample("life", at)[vm.ref]; w.cpu == 0 {
		t.Error("a VM that is on again reports no CPU")
	}
}

func TestLifeAddedVMKeepsItsIdentityAcrossARebuild(t *testing.T) {
	sc := lifeScenario(t)
	reg := registry.InMemory("life")
	s := lifeSim(t, sc, reg)
	l := s.Life()
	refs := s.Clusters[0]
	key := refs.Cluster.Key
	var added, removed []int
	l.VMAdded = func(_ string, n int) { added = append(added, n) }
	l.VMRemoved = func(_ string, n int) { removed = append(removed, n) }

	m0, t0 := mark(s), lastTask(l)
	name, host, err := l.AddVM(key, 1)
	if err != nil || name != "cluster-01-vm-021" || host == "" {
		t.Fatalf("AddVM = %q on %q, %v", name, host, err)
	}
	// created, then powered on: two operations, two chains
	order, by := chainsOf(events(s, m0))
	if len(order) != 2 {
		t.Fatalf("%d chains of events for a new VM, want its creation and its power-on", len(order))
	}
	chainIs(t, l, by[order[0]], "Folder.createVm", lifeUser, "VmBeingCreatedEvent", "VmInstanceUuidAssignedEvent", "VmUuidAssignedEvent", "VmCreatedEvent")
	chainIs(t, l, by[order[1]], "VirtualMachine.powerOn", lifeUser, "VmStartingEvent", "VmPoweredOnEvent")
	if made := l.vms[key][name].vm; len(by[order[0]]) == 5 {
		if u := by[order[0]][3].(*types.VmUuidAssignedEvent); u.Uuid != made.UUID || !strings.Contains(u.FullFormattedMessage, made.UUID) {
			t.Errorf("the event says the new VM's UUID is %s (%q); it is %s", u.Uuid, u.FullFormattedMessage, made.UUID)
		}
		if u := by[order[0]][2].(*types.VmInstanceUuidAssignedEvent); u.InstanceUuid != made.InstanceUUID {
			t.Errorf("the event says the new VM's instance UUID is %s; it is %s", u.InstanceUuid, made.InstanceUUID)
		}
	}
	m1 := mark(s)
	gone, err := l.RemoveVM(key, 2) // the third VM by name
	if err != nil || gone != "cluster-01-vm-003" {
		t.Fatalf("RemoveVM = %q, %v", gone, err)
	}
	// powered off, then destroyed
	order, by = chainsOf(events(s, m1))
	if len(order) != 2 {
		t.Fatalf("%d chains of events for a removed VM, want its power-off and its removal", len(order))
	}
	chainIs(t, l, by[order[0]], "VirtualMachine.powerOff", lifeUser, "VmStoppingEvent", "VmPoweredOffEvent")
	chainIs(t, l, by[order[1]], "VirtualMachine.destroy", lifeUser, "VmRemovedEvent")
	// vCenter lists the creation and the removal by the names a real one
	// gives them, and none of the tasks vcsim made for the VM's files
	names := map[string]string{}
	for _, info := range recentTasks(l, t0) {
		names[info.DescriptionId] = info.Name
		if info.State != types.TaskInfoStateSuccess {
			t.Errorf("task %s is %s", info.DescriptionId, info.State)
		}
	}
	if names["Folder.createVm"] != "CreateVM_Task" || names["VirtualMachine.destroy"] != "Destroy_Task" || len(names) != 4 {
		t.Errorf("the tasks left by a VM added and a VM removed: %v", names)
	}
	if !reflect.DeepEqual(added, []int{21}) || !reflect.DeepEqual(removed, []int{3}) {
		t.Fatalf("told of added %v and removed %v", added, removed)
	}
	vm := l.vms[key][name]
	obj := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
	if obj.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOn || obj.Config.Uuid != vm.vm.UUID {
		t.Errorf("the new VM is %s with uuid %s, want on and %s", obj.Runtime.PowerState, obj.Config.Uuid, vm.vm.UUID)
	}
	if n := len(refs.Cluster.LiveVMs()); n != 20 {
		t.Errorf("the cluster lists %d VMs, want 20 (one added, one removed)", n)
	}
	for _, v := range refs.Cluster.LiveVMs() {
		if v.Name == gone {
			t.Errorf("%s is still listed", gone)
		}
	}
	if l.reg.Get(l.as("").Map.Get(vm.ref).Reference()) == nil || len(placement(t, s)) != 40 {
		t.Error("the inventory does not hold 40 VMs")
	}
	sum := l.reg.Get(refs.Ref).(*simulator.ClusterComputeResource).Summary.(*types.ClusterComputeResourceSummary)
	if sum.UsageSummary.TotalVmCount != 20 {
		t.Errorf("the cluster says it has %d VMs", sum.UsageSummary.TotalVmCount)
	}

	// the scenario now records both, and a rebuild from it makes the same VMs
	sc.Clusters = append(sc.Clusters, scenario.ClusterOverride{Key: key, AddedVMs: added, RemovedVMs: removed})
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	again := lifeSim(t, sc, reg)
	want := map[string]types.ManagedObjectReference{}
	for n, v := range l.vms[key] {
		want[n] = v.ref
	}
	got := map[string]types.ManagedObjectReference{}
	for n, v := range again.Life().vms[key] {
		got[n] = v.ref
	}
	if !reflect.DeepEqual(got, want) {
		var names []string
		for n := range want {
			if got[n] != want[n] {
				names = append(names, n+": "+want[n].Value+" then "+got[n].Value)
			}
		}
		sort.Strings(names)
		t.Fatalf("a rebuild changed VM references: %v", names)
	}
	if o := again.Life().reg.Get(got[name]).(*simulator.VirtualMachine); o.Config.Uuid != vm.vm.UUID || o.Config.InstanceUuid != vm.vm.InstanceUUID {
		t.Error("a rebuild changed the added VM's uuids")
	}
	// and the next VM takes a number that was never used
	if next, _, err := again.Life().AddVM(key, 1); err != nil || next != "cluster-01-vm-022" {
		t.Errorf("the next VM is %q (%v), want cluster-01-vm-022", next, err)
	}
}

func TestLifeFillShowsInTheDatastoreAndPerformance(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("life"))
	l := s.Life()
	refs := s.Clusters[0]
	ds := l.reg.Get(refs.Datastore).(*simulator.Datastore)
	capacity, free := ds.Summary.Capacity, ds.Summary.FreeSpace
	stores := l.Fill(refs.Cluster.Key, 0.02)
	if d := free - ds.Summary.FreeSpace; len(stores) != 1 || stores[0].Bytes != d || d < capacity/50-1 || d > capacity/50+1 || ds.Info.GetDatastoreInfo().FreeSpace != ds.Summary.FreeSpace {
		t.Fatalf("filling 2%%: %+v, free %d then %d of %d", stores, free, ds.Summary.FreeSpace, capacity)
	}
	if used := readPerformanceState(l.as("")).nodes[refs.Datastore].used; used != (capacity-ds.Summary.FreeSpace)/1024 {
		t.Errorf("the performance model sees %d KiB used", used)
	}
	if got := topology.VsanUsed(refs.Cluster); got != capacity-ds.Summary.FreeSpace {
		t.Errorf("the vSAN capacity read says %d used, the datastore %d", got, capacity-ds.Summary.FreeSpace)
	}
	// never past nine tenths, and a cleanup frees no more than was filled
	l.Fill(refs.Cluster.Key, 5)
	if used := capacity - ds.Summary.FreeSpace; used != int64(0.9*float64(capacity)) {
		t.Errorf("filled to %d of %d", used, capacity)
	}
	l.Fill(refs.Cluster.Key, -5)
	if ds.Summary.FreeSpace != free {
		t.Errorf("a cleanup left %d free; before anything was filled it was %d", ds.Summary.FreeSpace, free)
	}
}

func TestLifeLoadDrivesCPUContentionAndMemory(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("life"))
	l := s.Life()
	a, b := s.Clusters[0], s.Clusters[1]
	at := time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC)
	base := readPerformanceState(l.as("")).sample("life", at)
	l.SetLoad(a.Cluster.Key, 2, 1.5)
	busy := readPerformanceState(l.as("")).sample("life", at)
	if busy[a.Ref].cpu < base[a.Ref].cpu*17/10 || busy[a.Ref].memory < base[a.Ref].memory*14/10 {
		t.Errorf("cluster one at twice the CPU and 1.5 times the memory: cpu %d then %d, memory %d then %d", base[a.Ref].cpu, busy[a.Ref].cpu, base[a.Ref].memory, busy[a.Ref].memory)
	}
	if busy[a.Ref].ready <= base[a.Ref].ready*2 {
		t.Errorf("a busier cluster should contend more than in proportion: ready %d then %d", base[a.Ref].ready, busy[a.Ref].ready)
	}
	if busy[b.Ref] != base[b.Ref] {
		t.Error("the other cluster changed too")
	}
	l.SetLoad(a.Cluster.Key, 0.5, 0.5)
	if quiet := readPerformanceState(l.as("")).sample("life", at); quiet[a.Ref].cpu > base[a.Ref].cpu*6/10 {
		t.Errorf("at half the load: cpu %d then %d", base[a.Ref].cpu, quiet[a.Ref].cpu)
	}
}

func TestLifeRestorePutsBackWhatASnapshotRead(t *testing.T) {
	sc := lifeScenario(t)
	reg := registry.InMemory("life")
	s := lifeSim(t, sc, reg)
	l := s.Life()
	key := s.Clusters[0].Cluster.Key
	l.Balance(key, 4, 11)
	off := l.PowerOff(key, 9)
	if _, err := l.EnterMaintenance(s.Clusters[1].Cluster.Key, s.Clusters[1].Cluster.Hosts[0].Name); err != nil {
		t.Fatal(err)
	}
	l.Fill(key, 0.05)
	snap := l.Snapshot()
	if len(snap.Hosts) < 9 || len(snap.Off) != 1 || len(snap.Maintenance) != 1 || snap.Filled[key] == 0 {
		t.Fatalf("snapshot: %d moved VMs, %v off, %v in maintenance mode, filled %v", len(snap.Hosts), snap.Off, snap.Maintenance, snap.Filled)
	}
	want := placement(t, s)

	again := lifeSim(t, sc, reg)
	al := again.Life()
	m0 := mark(again)
	al.Restore(snap)
	if got := placement(t, again); !reflect.DeepEqual(got, want) {
		t.Fatal("after Restore the VMs are not where they were")
	}
	if !reflect.DeepEqual(al.Snapshot(), snap) {
		t.Errorf("a snapshot of the restored inventory differs:\n%+v\n%+v", al.Snapshot(), snap)
	}
	if !al.off[key][off] || al.Clusters()[0].Off != 1 || !al.Clusters()[1].Hosts[0].Maintenance {
		t.Error("the powered-off VM or the host in maintenance mode did not come back")
	}
	if n := len(events(again, m0)); n != 0 {
		t.Errorf("Restore recorded %d events; it must record none", n)
	}
}
