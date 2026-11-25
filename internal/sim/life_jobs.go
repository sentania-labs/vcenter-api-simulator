package sim

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/life"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

var _ life.JobWorld = (*Life)(nil)

func (l *Life) VMNames(cluster string) []string { return l.names(cluster, nil) }

const backupName = "Activity nightly backup"

func (l *Life) restoreBackup(ctx *simulator.Context, vm *simulator.VirtualMachine, topo *topology.VM, at time.Time) {
	snap := &simulator.VirtualMachineSnapshot{}
	snap.Vm = vm.Self
	l.reg.Put(snap)
	vm.Snapshot = &types.VirtualMachineSnapshotInfo{CurrentSnapshot: &snap.Self, RootSnapshotList: []types.VirtualMachineSnapshotTree{{Snapshot: snap.Self, Vm: vm.Self, Name: backupName, Description: "Temporary snapshot for the simulated nightly backup", Id: 1, CreateTime: at, State: types.VirtualMachinePowerStatePoweredOff}}}
	vm.RootSnapshot = []types.ManagedObjectReference{snap.Self}
	if vm.LayoutEx != nil {
		var key int32
		for _, f := range vm.LayoutEx.File {
			key = max(key, f.Key+1)
		}
		vm.LayoutEx.File = append(vm.LayoutEx.File, types.VirtualMachineFileLayoutExFileInfo{
			Key: key, Name: "[" + topo.Cluster.Datastore + "] " + topo.NamespaceUUID + "/" + topo.Name + "-Snapshot1.vmsn",
			Type: string(types.VirtualMachineFileLayoutExFileTypeSnapshotData), Accessible: types.NewBool(true),
		})
		vm.LayoutEx.Snapshot = append(vm.LayoutEx.Snapshot, types.VirtualMachineFileLayoutExSnapshotLayout{Key: snap.Self, DataKey: key, MemoryKey: -1})
	}
	l.publishBackup(ctx, vm, topo)
}

// SnapshotVM uses the actual snapshot implementation, including the managed
// snapshot object and task result. No snapshot event type is invented.
func (l *Life) SnapshotVM(cluster, name string) error {
	refs := l.refs(cluster)
	vm := l.vms[cluster][name]
	if refs == nil || vm == nil {
		return fmt.Errorf("VM no longer exists")
	}
	ctx := l.as(lifeUser)
	obj := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
	var existing bool
	ctx.WithLock(obj, func() { existing = findBackup(obj) != nil })
	if existing {
		return nil
	}
	host := l.hostOf(ctx, vm)
	_, maint, down := l.hostState(ctx, host)
	if maint || down {
		return life.ErrHostUnavailable
	}
	te, chain := l.head(ctx, l.vmEvent(refs, vm, host).Event, types.TaskInfo{Name: "CreateSnapshot_Task", DescriptionId: "VirtualMachine.createSnapshot", Entity: &vm.ref, EntityName: name, Reason: byUser(), QueueTime: l.now()})
	// vcsim writes a tiny snapshot file; keep its real backing directory available.
	ds, _ := l.reg.Get(refs.Datastore).(*simulator.Datastore)
	var address string
	if ds != nil {
		ctx.WithLock(ds, func() { address, ds.Summary.Url = ds.Summary.Url, ds.Info.GetDatastoreInfo().Url })
	}
	body := obj.CreateSnapshotTask(ctx, &types.CreateSnapshot_Task{This: vm.ref, Name: backupName, Description: "Temporary snapshot for the simulated nightly backup", Memory: false, Quiesce: false})
	if body.Fault() != nil {
		if ds != nil {
			ctx.WithLock(ds, func() { ds.Summary.Url = address })
		}
		return fmt.Errorf("snapshot failed: %s", body.Fault().String)
	}
	task := l.reg.Get(body.(*methods.CreateSnapshot_TaskBody).Res.Returnval).(*simulator.Task)
	l.wait(task)
	if ds != nil {
		ctx.WithLock(ds, func() { ds.Summary.Url = address })
	}
	ctx.WithLock(task, func() { task.Info.DescriptionId = "VirtualMachine.createSnapshot" })
	te.Info.Key, te.Info.Task = task.Self.Value, task.Self
	l.own(ctx, task, vm.ref, chain, "CreateSnapshot_Task", byUser())
	if task.Info.Error != nil {
		return fmt.Errorf("snapshot failed: %s", task.Info.Error.LocalizedMessage)
	}
	ctx.WithLock(obj, func() { l.publishBackup(ctx, obj, vm.vm) })
	l.space(ctx, refs)
	return nil
}

// publishBackup calls with the VM locked. Snapshot files change layoutEx and
// vcsim recalculates storage from it, so put the modelled figures back before
// publishing either.
func (l *Life) publishBackup(ctx *simulator.Context, obj *simulator.VirtualMachine, topo *topology.VM) {
	var snapshot int64
	if findBackup(obj) != nil {
		snapshot = topology.SnapshotSize(topo)
	}
	topo.SetSnapshotBytes(snapshot)
	shapeVMStorage(obj, topo.Cluster, topo, l.s.Env.Now())
	changes := []types.PropertyChange{
		{Name: "snapshot", Val: obj.Snapshot}, {Name: "rootSnapshot", Val: obj.RootSnapshot},
		{Name: "summary", Val: obj.Summary},
	}
	updateLife(ctx, obj, append(changes, storageChanges(obj)...))
}

func storageChanges(obj *simulator.VirtualMachine) []types.PropertyChange {
	changes := []types.PropertyChange{{Name: "storage", Val: obj.Storage}}
	if obj.LayoutEx != nil {
		changes = append(changes, types.PropertyChange{Name: "layoutEx", Val: obj.LayoutEx})
	}
	return changes
}

func findBackup(vm *simulator.VirtualMachine) *types.ManagedObjectReference {
	if vm.Snapshot == nil {
		return nil
	}
	var find func([]types.VirtualMachineSnapshotTree) *types.ManagedObjectReference
	find = func(nodes []types.VirtualMachineSnapshotTree) *types.ManagedObjectReference {
		for _, n := range nodes {
			if n.Name == backupName {
				r := n.Snapshot
				return &r
			}
			if r := find(n.ChildSnapshotList); r != nil {
				return r
			}
		}
		return nil
	}
	return find(vm.Snapshot.RootSnapshotList)
}

func (l *Life) RemoveBackup(cluster, name string) error {
	refs := l.refs(cluster)
	vm := l.vms[cluster][name]
	if refs == nil || vm == nil {
		return nil
	}
	ctx := l.as(lifeUser)
	obj := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
	var ref *types.ManagedObjectReference
	ctx.WithLock(obj, func() { ref = findBackup(obj) })
	if ref == nil {
		return nil
	}
	snap, ok := l.reg.Get(*ref).(*simulator.VirtualMachineSnapshot)
	if !ok {
		return fmt.Errorf("backup snapshot is missing")
	}
	defer l.space(ctx, refs)
	te, chain := l.head(ctx, l.vmEvent(refs, vm, l.hostOf(ctx, vm)).Event, types.TaskInfo{Name: "RemoveSnapshot_Task", DescriptionId: "VirtualMachine.removeSnapshot", Entity: &vm.ref, EntityName: name, Reason: byUser(), QueueTime: l.now()})
	// Use the backing path for file removal too. vcsim keeps old file entries
	// in LayoutEx after removing a snapshot; prune only this snapshot's entries.
	keys := map[int32]bool{}
	ctx.WithLock(obj, func() {
		if obj.LayoutEx != nil {
			for _, layout := range obj.LayoutEx.Snapshot {
				if layout.Key == *ref {
					keys[layout.DataKey] = true
					keys[layout.MemoryKey] = true
				}
			}
		}
	})
	ds, _ := l.reg.Get(refs.Datastore).(*simulator.Datastore)
	if ds != nil {
		var address string
		ctx.WithLock(ds, func() { address, ds.Summary.Url = ds.Summary.Url, ds.Info.GetDatastoreInfo().Url })
		defer ctx.WithLock(ds, func() { ds.Summary.Url = address })
	}
	body := snap.RemoveSnapshotTask(ctx, &types.RemoveSnapshot_Task{This: *ref, RemoveChildren: false, Consolidate: types.NewBool(true)})
	if body.Fault() != nil {
		return fmt.Errorf("snapshot removal failed: %s", body.Fault().String)
	}
	task := l.reg.Get(body.(*methods.RemoveSnapshot_TaskBody).Res.Returnval).(*simulator.Task)
	l.wait(task)
	te.Info.Key, te.Info.Task = task.Self.Value, task.Self
	l.own(ctx, task, vm.ref, chain, "RemoveSnapshot_Task", byUser())
	if task.Info.Error != nil {
		return fmt.Errorf("snapshot removal failed: %s", task.Info.Error.LocalizedMessage)
	}
	ctx.WithLock(obj, func() {
		if obj.LayoutEx != nil {
			kept := obj.LayoutEx.File[:0]
			for _, f := range obj.LayoutEx.File {
				if !keys[f.Key] {
					kept = append(kept, f)
				}
			}
			obj.LayoutEx.File = kept
		}
		l.publishBackup(ctx, obj, vm.vm)
	})
	return nil
}

func (l *Life) jobHost(cluster, name string) (*ClusterRefs, *simulator.HostSystem, error) {
	refs := l.refs(cluster)
	if refs == nil {
		return nil, nil, fmt.Errorf("cluster no longer exists")
	}
	ref, ok := l.hostRef(refs, name)
	if !ok {
		return nil, nil, fmt.Errorf("host no longer exists")
	}
	return refs, l.host(ref), nil
}

func (l *Life) connection(ctx *simulator.Context, hs *simulator.HostSystem, down bool) {
	ctx.WithLock(hs, func() {
		hs.Runtime.ConnectionState = types.HostSystemConnectionStateConnected
		hs.Runtime.PowerState = types.HostSystemPowerStatePoweredOn
		if down {
			hs.Runtime.ConnectionState = types.HostSystemConnectionStateNotResponding
			hs.Runtime.PowerState = types.HostSystemPowerStateUnknown
		}
		if hs.Summary.Runtime != nil {
			*hs.Summary.Runtime = hs.Runtime
		}
		if down {
			hs.Summary.QuickStats.OverallCpuUsage = 0
			hs.Summary.QuickStats.OverallMemoryUsage = 0
		}
		updateLife(ctx, hs, []types.PropertyChange{{Name: "runtime", Val: hs.Runtime}, {Name: "summary", Val: hs.Summary}})
	})
}

func (l *Life) PatchHost(cluster, host string) error {
	refs, hs, err := l.jobHost(cluster, host)
	if err != nil {
		return err
	}
	ctx := l.as(lifeUser)
	_, maint, down := l.hostState(ctx, hs.Self)
	if !maint || down {
		return fmt.Errorf("host must be in maintenance mode and connected before patching")
	}
	l.begin(ctx, l.where(refs, hs.Self), hs.Self, hs.Name, "InstallHostPatchV2_Task", "HostPatchManager.installV2", byUser())
	ctx.WithLock(hs, func() {
		if hs.Config != nil {
			p := &hs.Config.Product
			build, _ := strconv.Atoi(p.Build)
			p.Build = strconv.Itoa(build + 1)
			patch, _ := strconv.Atoi(p.PatchLevel)
			p.PatchLevel = fmt.Sprintf("%04d", patch+1)
			p.FullName = fmt.Sprintf("VMware ESXi %s build-%s", p.Version, p.Build)
			{
				hs.Summary.Config.Product = p
			}
			updateLife(ctx, hs, []types.PropertyChange{{Name: "config.product", Val: p}, {Name: "summary.config", Val: hs.Summary.Config}})
		}
	})
	chain := l.begin(ctx, l.where(refs, hs.Self), hs.Self, hs.Name, "RebootHost_Task", "HostSystem.reboot", byUser())
	l.post(ctx, chain, &types.HostShutdownEvent{HostEvent: types.HostEvent{Event: l.where(refs, hs.Self)}})
	l.connection(ctx, hs, true)
	l.publishLoad(ctx, refs)
	return nil
}

func (l *Life) FinishReboot(cluster, host string) error {
	refs, hs, err := l.jobHost(cluster, host)
	if err != nil {
		return err
	}
	ctx := l.as("")
	l.connection(ctx, hs, false)
	ctx.WithLock(hs, func() {
		now := l.now()
		hs.Runtime.BootTime = &now
		if hs.Summary.Runtime != nil {
			hs.Summary.Runtime.BootTime = &now
		}
		updateLife(ctx, hs, []types.PropertyChange{{Name: "runtime", Val: hs.Runtime}, {Name: "summary.runtime", Val: hs.Summary.Runtime}})
	})
	l.post(ctx, 0, &types.HostConnectedEvent{HostEvent: types.HostEvent{Event: l.where(refs, hs.Self)}})
	return nil
}

func (l *Life) haEvent(refs *ClusterRefs, hs *simulator.HostSystem, id string) {
	event := l.where(refs, hs.Self)
	severity := "info"
	event.FullFormattedMessage = "vSphere HA agent on " + hs.Name + " is healthy."
	if id == "com.vmware.vc.HA.DasHostFailedEvent" {
		severity = "error"
		event.FullFormattedMessage = "vSphere HA detected a host failure on " + hs.Name + "."
	}
	l.post(l.as(""), 0, &types.EventEx{Event: event, EventTypeId: id, ObjectType: "HostSystem", ObjectId: hs.Self.Value, ObjectName: hs.Name, Severity: severity, Message: event.FullFormattedMessage})
}

func (l *Life) CrashHost(cluster, host string) ([]string, error) {
	refs, hs, err := l.jobHost(cluster, host)
	if err != nil {
		return nil, err
	}
	if !l.ha(refs) {
		return nil, fmt.Errorf("vSphere HA is not enabled in this cluster")
	}
	ctx := l.as("")
	_, maint, down := l.hostState(ctx, hs.Self)
	if maint || down {
		return nil, fmt.Errorf("host is already out of service")
	}
	if len(l.loads(ctx, refs, hs.Self)) == 0 {
		return nil, fmt.Errorf("no other host is available")
	}
	var vms []string
	for name, vm := range l.vms[cluster] {
		if l.hostOf(ctx, vm) != hs.Self {
			continue
		}
		obj := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
		ctx.WithLock(obj, func() {
			updateLife(ctx, obj, []types.PropertyChange{{Name: "runtime.connectionState", Val: types.VirtualMachineConnectionStateDisconnected}, {Name: "summary.runtime.connectionState", Val: types.VirtualMachineConnectionStateDisconnected}})
		})
		if !l.off[cluster][name] {
			vms = append(vms, name)
		}
	}
	sort.Strings(vms)
	l.connection(ctx, hs, true)
	l.post(ctx, 0, &types.HostConnectionLostEvent{HostEvent: types.HostEvent{Event: l.where(refs, hs.Self)}})
	l.hostAlarm(refs, hs, "Host connection and power state", true, true)
	l.haEvent(refs, hs, "com.vmware.vc.HA.DasHostFailedEvent")
	l.hostAlarm(refs, hs, "vSphere HA host status", true, true)
	l.publishLoad(ctx, refs)
	return vms, nil
}

func (l *Life) RestartVMs(cluster, host string, names []string) ([]life.Move, error) {
	refs, hs, err := l.jobHost(cluster, host)
	if err != nil {
		return nil, err
	}
	ctx := l.as("")
	hosts := l.loads(ctx, refs, hs.Self)
	if len(hosts) == 0 {
		return nil, fmt.Errorf("HA is waiting for an available host")
	}
	var moves []life.Move
	for _, name := range names {
		vm := l.vms[cluster][name]
		if vm == nil {
			continue
		}
		to := lightest(hosts, nil)
		obj := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
		if l.hostOf(ctx, vm) != hs.Self {
			continue
		}
		chain := l.begin(ctx, l.vmEvent(refs, vm, to.ref).Event, vm.ref, name, "", "VirtualMachine.powerOn", bySystem())
		l.placeVM(ctx, vm.ref, to.ref)
		ctx.WithLock(obj, func() {
			updateLife(ctx, obj, []types.PropertyChange{{Name: "runtime.connectionState", Val: types.VirtualMachineConnectionStateConnected}, {Name: "summary.runtime.connectionState", Val: types.VirtualMachineConnectionStateConnected}})
		})
		l.setPower(ctx, obj, vm.vm, true)
		delete(l.off[cluster], name)
		ev := l.vmEvent(refs, vm, to.ref)
		ev.FullFormattedMessage = "vSphere HA restarted virtual machine " + name + " on host " + l.host(to.ref).Name + "."
		l.post(ctx, chain, &types.VmPoweredOnEvent{VmEvent: ev}, &types.EventEx{Event: ev.Event, EventTypeId: "com.vmware.vc.ha.VmRestartedByHAEvent", ObjectType: "VirtualMachine", ObjectId: vm.ref.Value, Severity: "info"})
		to.vms = append(to.vms, vm)
		moves = append(moves, life.Move{VM: name, From: host, To: l.host(to.ref).Name})
	}
	l.counts(ctx, refs)
	l.publishLoad(ctx, refs)
	return moves, nil
}

func (l *Life) RecoverHost(cluster, host string) error {
	refs, hs, err := l.jobHost(cluster, host)
	if err != nil {
		return err
	}
	ctx := l.as("")
	l.connection(ctx, hs, false)
	for _, vm := range l.vms[cluster] {
		if l.hostOf(ctx, vm) == hs.Self {
			obj := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
			ctx.WithLock(obj, func() {
				updateLife(ctx, obj, []types.PropertyChange{{Name: "runtime.connectionState", Val: types.VirtualMachineConnectionStateConnected}, {Name: "summary.runtime.connectionState", Val: types.VirtualMachineConnectionStateConnected}})
			})
		}
	}
	l.post(ctx, 0, &types.HostConnectedEvent{HostEvent: types.HostEvent{Event: l.where(refs, hs.Self)}})
	l.hostAlarm(refs, hs, "Host connection and power state", false, true)
	l.haEvent(refs, hs, "com.vmware.vc.HA.HostDasAgentHealthyEvent")
	l.hostAlarm(refs, hs, "vSphere HA host status", false, true)
	l.publishLoad(ctx, refs)
	return nil
}

// Install the captured default definitions. The state alarm is evaluated here
// because vcsim only evaluates EventEx expressions. Propagation matches vCenter:
// the host's alarm is also visible from ancestors' triggeredAlarmState.
func (l *Life) hostAlarm(refs *ClusterRefs, hs *simulator.HostSystem, name string, raised, emit bool) {
	ctx := l.as("")
	var alarm *simulator.Alarm
	for _, ref := range ctx.Map.AlarmManager().GetAlarmResponse.Returnval {
		a := ctx.Map.Get(ref).(*simulator.Alarm)
		if a.Info.Name == name {
			alarm = a
			break
		}
	}
	if alarm == nil {
		return
	}
	status := types.ManagedEntityStatusGreen
	from := "red"
	if raised {
		status = types.ManagedEntityStatusRed
		from = "green"
	}
	for obj := mo.Entity(hs); obj != nil; {
		var parent *types.ManagedObjectReference
		ctx.WithLock(obj, func() {
			me := obj.Entity()
			parent = me.Parent
			keep := make([]types.AlarmState, 0, len(me.TriggeredAlarmState)+1)
			for _, s := range me.TriggeredAlarmState {
				if s.Alarm != alarm.Self || s.Entity != hs.Self {
					keep = append(keep, s)
				}
			}
			if raised {
				keep = append(keep, types.AlarmState{Key: alarm.Self.Value + "." + hs.Self.Value, Alarm: alarm.Self, Entity: hs.Self, OverallStatus: status, Time: l.now(), Acknowledged: types.NewBool(false)})
			}
			me.TriggeredAlarmState = keep
			me.OverallStatus = types.ManagedEntityStatusGreen
			for _, s := range keep {
				if s.OverallStatus == types.ManagedEntityStatusRed {
					me.OverallStatus = types.ManagedEntityStatusRed
					break
				}
			}
			updateLife(ctx, obj, []types.PropertyChange{{Name: "triggeredAlarmState", Val: keep}, {Name: "overallStatus", Val: me.OverallStatus}})
		})
		if parent == nil {
			break
		}
		next, ok := ctx.Map.Get(*parent).(mo.Entity)
		if !ok {
			break
		}
		obj = next
	}
	if emit {
		l.post(ctx, 0, &types.AlarmStatusChangedEvent{AlarmEvent: types.AlarmEvent{Event: l.where(refs, hs.Self), Alarm: types.AlarmEventArgument{EntityEventArgument: types.EntityEventArgument{Name: name}, Alarm: alarm.Self}}, Source: types.ManagedEntityEventArgument{EntityEventArgument: types.EntityEventArgument{Name: hs.Name}, Entity: hs.Self}, Entity: types.ManagedEntityEventArgument{EntityEventArgument: types.EntityEventArgument{Name: "Datacenters"}, Entity: l.s.model.ServiceContent.RootFolder}, From: from, To: string(status)})
	}
}
