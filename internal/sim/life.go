package sim

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/life"
	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// Life changes the running vCenter in place, the way package life asks:
// VMs move, are powered off and on, come and go; hosts go into and out of
// maintenance mode; the vSAN datastores fill and empty; and the VMs look
// more or less loaded. Every change is made through vcsim's registry, so a
// tool that is waiting for updates is told, and is recorded with the events
// and tasks a real vCenter records for it. Nothing here rebuilds anything:
// sessions, views and history stay as they are.
//
// One caller at a time uses a Life (the control plane, under its lock).
type Life struct {
	s   *Simulator
	reg *simulator.Registry

	vms map[string]map[string]*lifeVM // by cluster key, then VM name
	off map[string]map[string]bool    // by cluster key: VMs that are powered off
	old []oldTask                     // tasks to forget once they are old

	// VMAdded and VMRemoved are told a VM number a cluster gained or lost,
	// so the caller can record it where a rebuild will find it.
	VMAdded, VMRemoved func(cluster string, number int)
}

type lifeVM struct {
	ref types.ManagedObjectReference
	vm  *topology.VM
}

type oldTask struct {
	ref, entity types.ManagedObjectReference
	at          time.Time
}

// lifeUser is who the events and tasks a person would have started are
// recorded for. DRS moves carry no user, as on a real vCenter.
const lifeUser = `VSPHERE.LOCAL\Administrator`

// loadRef is where the performance model finds how loaded each cluster
// should look.
var loadRef = types.ManagedObjectReference{Type: "VcsimulatorLoad", Value: "load"}

// clusterLoad holds each cluster's load factors: 1 is as built.
type clusterLoad struct {
	types.ManagedObjectReference
	m atomic.Pointer[map[types.ManagedObjectReference][2]float64]
}

func (c *clusterLoad) Reference() types.ManagedObjectReference { return c.ManagedObjectReference }

// loadOf returns the CPU and memory load factors of a cluster.
func loadOf(ctx *simulator.Context, cluster types.ManagedObjectReference) (cpu, memory float64) {
	if l, ok := ctx.Map.Get(loadRef).(*clusterLoad); ok {
		if m := l.m.Load(); m != nil {
			if f, ok := (*m)[cluster]; ok {
				return f[0], f[1]
			}
		}
	}
	return 1, 1
}

// Life returns the simulator's in-place operations.
func (s *Simulator) Life() *Life {
	s.lifeOnce.Do(func() {
		l := &Life{s: s, reg: s.model.Service.Context.Map, vms: map[string]map[string]*lifeVM{}, off: map[string]map[string]bool{}}
		for _, refs := range s.Clusters {
			idx := map[string]*lifeVM{}
			for i, vm := range refs.Cluster.VMs {
				idx[vm.Name] = &lifeVM{ref: refs.VMs[i], vm: vm}
			}
			l.vms[refs.Cluster.Key] = idx
			l.off[refs.Cluster.Key] = map[string]bool{}
		}
		l.reg.Put(&clusterLoad{ManagedObjectReference: loadRef})
		s.life = l
	})
	return s.life
}

// as is a context that acts as a user: "" for what the system does itself.
func (l *Life) as(user string) *simulator.Context {
	c := *l.s.model.Service.Context
	c.Session = &simulator.Session{UserSession: types.UserSession{UserName: user}, Registry: simulator.NewRegistry(), Map: c.Map}
	return &c
}

func (l *Life) refs(cluster string) *ClusterRefs { return l.s.Cluster(cluster) }

func (l *Life) host(ref types.ManagedObjectReference) *simulator.HostSystem {
	h, _ := l.reg.Get(ref).(*simulator.HostSystem)
	return h
}

// hostState reads a host's VMs and whether it can run them.
func (l *Life) hostState(ctx *simulator.Context, ref types.ManagedObjectReference) (vms []types.ManagedObjectReference, maintenance, down bool) {
	hs := l.host(ref)
	ctx.WithLock(hs, func() {
		vms = append(vms, hs.Vm...)
		maintenance = hs.Runtime.InMaintenanceMode
		down = hs.Runtime.ConnectionState != types.HostSystemConnectionStateConnected
	})
	return
}

// Clusters implements life.World.
func (l *Life) Clusters() []life.Cluster {
	ctx := l.as("")
	out := make([]life.Cluster, 0, len(l.s.Clusters))
	for _, refs := range l.s.Clusters {
		cl := refs.Cluster
		c := life.Cluster{Key: cl.Key, Name: cl.Name, VMs: len(l.vms[cl.Key]), Off: len(l.off[cl.Key])}
		for i, ref := range refs.Hosts {
			vms, maint, down := l.hostState(ctx, ref)
			c.Hosts = append(c.Hosts, life.Host{Name: cl.Hosts[i].Name, VMs: len(vms), Maintenance: maint, Down: down})
		}
		out = append(out, c)
	}
	return out
}

// ---------------------------------------------------------------------------
// events and tasks

// post records events. The first one starts a chain when chain is 0, and
// the rest join it, as the events of one operation do on a real vCenter.
// vcsim gives every event its own chain, so the chain is set once posted.
func (l *Life) post(ctx *simulator.Context, chain *int32, events ...types.BaseEvent) {
	em := ctx.Map.EventManager()
	for _, ev := range events {
		ctx.WithLock(em, func() { em.PostEvent(ctx, &types.PostEvent{EventToPost: ev}) })
		e := ev.GetEvent()
		if *chain == 0 {
			*chain = e.Key
		}
		e.ChainId = *chain
	}
}

// record makes the task record of something that was done: its name as a
// real vCenter names it, who or what started it, and the chain of its
// events. It is complete as soon as it exists.
func (l *Life) record(ctx *simulator.Context, entity types.ManagedObjectReference, entityName, name, description string, reason types.BaseTaskReason, chain int32) {
	t := simulator.CreateTask(entity, name, func(*simulator.Task) (types.AnyType, types.BaseMethodFault) { return nil, nil })
	t.Info.Name = name
	t.Info.DescriptionId = description
	t.Info.EntityName = entityName
	t.Info.Reason = reason
	t.Info.EventChainId = chain
	ref := t.Run(ctx)
	t.Wait()
	l.old = append(l.old, oldTask{ref: ref, entity: entity, at: time.Now()})
}

func byUser() types.BaseTaskReason { return &types.TaskReasonUser{UserName: lifeUser} }
func bySystem() types.BaseTaskReason { return &types.TaskReasonSystem{} }

// taskAge is how long a finished task stays in the recent task lists, as
// on a real vCenter.
const taskAge = 10 * time.Minute

// Sweep forgets the tasks of what was done more than ten minutes ago, so
// they do not pile up for as long as the simulator runs.
func (l *Life) Sweep(now time.Time) {
	ctx := l.as("")
	n := 0
	for _, o := range l.old {
		if now.Sub(o.at) < taskAge {
			break
		}
		n++
		if tm, ok := l.reg.Get(*l.s.model.ServiceContent.TaskManager).(*simulator.TaskManager); ok {
			ctx.WithLock(tm, func() {
				ctx.Update(tm, []types.PropertyChange{{Name: "recentTask", Val: without(tm.RecentTask, o.ref)}})
			})
		}
		if e, ok := l.reg.Get(o.entity).(mo.Entity); ok {
			ctx.WithLock(e, func() {
				ctx.Update(e, []types.PropertyChange{{Name: "recentTask", Val: without(e.Entity().RecentTask, o.ref)}})
			})
		}
		l.reg.Remove(ctx, o.ref)
	}
	l.old = l.old[n:]
}

func without(list []types.ManagedObjectReference, ref types.ManagedObjectReference) []types.ManagedObjectReference {
	out := make([]types.ManagedObjectReference, 0, len(list))
	for _, r := range list {
		if r != ref {
			out = append(out, r)
		}
	}
	return out
}

// where is what an event says about where something happened.
func (l *Life) where(refs *ClusterRefs, host types.ManagedObjectReference) types.Event {
	cl := refs.Cluster
	dc := l.s.dcs[cl.Datacenter]
	e := types.Event{
		Datacenter:      &types.DatacenterEventArgument{EntityEventArgument: types.EntityEventArgument{Name: cl.Datacenter.Name}, Datacenter: dc.ref},
		ComputeResource: &types.ComputeResourceEventArgument{EntityEventArgument: types.EntityEventArgument{Name: cl.Name}, ComputeResource: refs.Ref},
	}
	if hs := l.host(host); hs != nil {
		e.Host = &types.HostEventArgument{EntityEventArgument: types.EntityEventArgument{Name: hs.Name}, Host: host}
	}
	return e
}

func (l *Life) vmEvent(refs *ClusterRefs, vm *lifeVM, host types.ManagedObjectReference) types.VmEvent {
	e := l.where(refs, host)
	e.Ds = &types.DatastoreEventArgument{EntityEventArgument: types.EntityEventArgument{Name: refs.Cluster.Datastore}, Datastore: refs.Datastore}
	e.Vm = &types.VmEventArgument{EntityEventArgument: types.EntityEventArgument{Name: vm.vm.Name}, Vm: vm.ref}
	return types.VmEvent{Event: e}
}

// ---------------------------------------------------------------------------
// moving VMs

// hostOf is the host a VM is on.
func (l *Life) hostOf(ctx *simulator.Context, vm *lifeVM) (ref types.ManagedObjectReference) {
	obj, ok := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
	if !ok {
		return
	}
	ctx.WithLock(obj, func() {
		if obj.Runtime.Host != nil {
			ref = *obj.Runtime.Host
		}
	})
	return
}

// move takes a VM to another host of its cluster: the VM's host, the two
// hosts' VM lists, and the two events and the task of a vMotion. drs says
// the move is one DRS made by itself.
func (l *Life) move(refs *ClusterRefs, vm *lifeVM, to types.ManagedObjectReference, drs bool) (life.Move, bool) {
	ctx := l.as("")
	from := l.hostOf(ctx, vm)
	src, dst := l.host(from), l.host(to)
	obj, ok := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
	if !ok || src == nil || dst == nil || from == to {
		return life.Move{}, false
	}
	hot := !l.off[refs.Cluster.Key][vm.vm.Name]
	var chain int32
	begin := l.vmEvent(refs, vm, from)
	dest := &types.HostEventArgument{EntityEventArgument: types.EntityEventArgument{Name: dst.Name}, Host: to}
	if hot {
		l.post(ctx, &chain, &types.VmBeingHotMigratedEvent{VmEvent: begin, DestHost: *dest, DestDatacenter: begin.Datacenter, DestDatastore: begin.Ds})
	} else {
		l.post(ctx, &chain, &types.VmBeingMigratedEvent{VmEvent: begin, DestHost: *dest, DestDatacenter: begin.Datacenter, DestDatastore: begin.Ds})
	}
	ctx.WithLock(src, func() { ctx.Update(src, []types.PropertyChange{{Name: "vm", Val: without(src.Vm, vm.ref)}}) })
	ctx.WithLock(dst, func() {
		ctx.Update(dst, []types.PropertyChange{{Name: "vm", Val: append(without(dst.Vm, vm.ref), vm.ref)}})
	})
	ctx.WithLock(obj, func() {
		ctx.Update(obj, []types.PropertyChange{{Name: "runtime.host", Val: to}, {Name: "summary.runtime.host", Val: to}})
	})
	done := types.VmMigratedEvent{VmEvent: l.vmEvent(refs, vm, to), SourceHost: types.HostEventArgument{EntityEventArgument: types.EntityEventArgument{Name: src.Name}, Host: from}, SourceDatacenter: begin.Datacenter, SourceDatastore: begin.Ds}
	if drs {
		l.post(ctx, &chain, &types.DrsVmMigratedEvent{VmMigratedEvent: done})
		l.record(ctx, vm.ref, vm.vm.Name, "Drm.ExecuteVMotionLRO", "Drm.ExecuteVMotionLRO", bySystem(), chain)
	} else {
		l.post(ctx, &chain, &done)
		l.record(ctx, vm.ref, vm.vm.Name, "RelocateVM_Task", "VirtualMachine.relocate", byUser(), chain)
	}
	return life.Move{VM: vm.vm.Name, From: src.Name, To: dst.Name}, true
}

// loads is the in-service hosts of a cluster and the VMs on each.
type hostLoad struct {
	ref types.ManagedObjectReference
	vms []*lifeVM
}

func (l *Life) loads(ctx *simulator.Context, refs *ClusterRefs, not types.ManagedObjectReference) []*hostLoad {
	byRef := map[types.ManagedObjectReference]*lifeVM{}
	for _, vm := range l.vms[refs.Cluster.Key] {
		byRef[vm.ref] = vm
	}
	var out []*hostLoad
	for _, ref := range refs.Hosts {
		vms, maint, down := l.hostState(ctx, ref)
		if maint || down || ref == not {
			continue
		}
		h := &hostLoad{ref: ref}
		for _, v := range vms {
			if vm := byRef[v]; vm != nil {
				h.vms = append(h.vms, vm)
			}
		}
		sort.Slice(h.vms, func(i, j int) bool { return h.vms[i].vm.Name < h.vms[j].vm.Name })
		out = append(out, h)
	}
	return out
}

// lightest is the host with the fewest VMs; the first of equals, so the
// same inventory gives the same answer.
func lightest(hosts []*hostLoad, skip map[types.ManagedObjectReference]bool) *hostLoad {
	var best *hostLoad
	for _, h := range hosts {
		if !skip[h.ref] && (best == nil || len(h.vms) < len(best.vms)) {
			best = h
		}
	}
	return best
}

func (h *hostLoad) take(i int) *lifeVM {
	vm := h.vms[i]
	h.vms = append(h.vms[:i:i], h.vms[i+1:]...)
	return vm
}

// Balance implements life.World: up to n running VMs go from the fullest
// hosts to the emptiest. No VM moves twice and no host both gives and
// takes, as in one pass of DRS.
func (l *Life) Balance(cluster string, n int, pick uint64) []life.Move {
	refs := l.refs(cluster)
	if refs == nil {
		return nil
	}
	ctx := l.as("")
	hosts := l.loads(ctx, refs, types.ManagedObjectReference{})
	gave, took := map[types.ManagedObjectReference]bool{}, map[types.ManagedObjectReference]bool{}
	off := l.off[cluster]
	var out []life.Move
	for i := 0; i < n; i++ {
		var from *hostLoad
		for _, h := range hosts {
			running := 0
			for _, vm := range h.vms {
				if !off[vm.vm.Name] {
					running++
				}
			}
			if !took[h.ref] && running > 0 && (from == nil || len(h.vms) > len(from.vms)) {
				from = h
			}
		}
		if from == nil {
			break
		}
		skip := map[types.ManagedObjectReference]bool{from.ref: true}
		for ref := range gave {
			skip[ref] = true
		}
		to := lightest(hosts, skip)
		if to == nil {
			break
		}
		var running []int
		for j, vm := range from.vms {
			if !off[vm.vm.Name] {
				running = append(running, j)
			}
		}
		vm := from.take(running[int((pick>>uint(8*(i%7)))%uint64(len(running)))])
		if m, ok := l.move(refs, vm, to.ref, true); ok {
			out = append(out, m)
			gave[from.ref], took[to.ref] = true, true
		}
		// the VM is counted on its new host but is not moved again in this pass
		to.vms = append(to.vms, &lifeVM{ref: vm.ref, vm: vm.vm})
		off = offPlus(off, vm.vm.Name)
	}
	return out
}

// offPlus is a set with one more name in it, leaving the original alone:
// Balance uses it to leave a VM it has moved out of the rest of the pass.
func offPlus(set map[string]bool, name string) map[string]bool {
	out := make(map[string]bool, len(set)+1)
	for k := range set {
		out[k] = true
	}
	out[name] = true
	return out
}

// ---------------------------------------------------------------------------
// power

func (l *Life) names(cluster string, keep func(*lifeVM) bool) []string {
	var out []string
	for name, vm := range l.vms[cluster] {
		if keep == nil || keep(vm) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// setPower gives a VM the state, guest and heartbeat of a VM that is on or
// off, and tells whoever is waiting.
func (l *Life) setPower(ctx *simulator.Context, obj *simulator.VirtualMachine, vm *topology.VM, on bool) {
	ctx.WithLock(obj, func() {
		if on {
			shapeVM(obj, vm.Cluster, vm, l.s.Env.Now)
			boot := l.s.Env.Now().UTC()
			obj.Runtime.BootTime = &boot
			obj.Summary.Runtime = obj.Runtime
			obj.Summary.QuickStats.UptimeSeconds = 0
		} else {
			obj.Runtime.PowerState = types.VirtualMachinePowerStatePoweredOff
			obj.Runtime.BootTime = nil
			obj.Summary.Runtime = obj.Runtime
			obj.Guest.GuestState = string(types.VirtualMachineGuestStateNotRunning)
			obj.Guest.ToolsStatus = types.VirtualMachineToolsStatusToolsNotRunning
			obj.Guest.ToolsRunningStatus = string(types.VirtualMachineToolsRunningStatusGuestToolsNotRunning)
			obj.Guest.IpAddress = ""
			obj.Guest.GuestOperationsReady = types.NewBool(false)
			obj.Summary.Guest.ToolsStatus = obj.Guest.ToolsStatus
			obj.Summary.Guest.ToolsRunningStatus = obj.Guest.ToolsRunningStatus
			obj.Summary.Guest.IpAddress = ""
			obj.Summary.QuickStats = types.VirtualMachineQuickStats{GuestHeartbeatStatus: types.ManagedEntityStatusGray}
			obj.GuestHeartbeatStatus = types.ManagedEntityStatusGray
		}
		ctx.Update(obj, []types.PropertyChange{
			{Name: "runtime", Val: obj.Runtime}, {Name: "summary", Val: obj.Summary}, {Name: "guest", Val: obj.Guest}, {Name: "guestHeartbeatStatus", Val: obj.GuestHeartbeatStatus},
		})
	})
}

func (l *Life) power(cluster, name string, on bool) bool {
	refs, vm := l.refs(cluster), l.vms[cluster][name]
	if refs == nil || vm == nil {
		return false
	}
	obj, ok := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
	if !ok {
		return false
	}
	ctx := l.as(lifeUser)
	ev := l.vmEvent(refs, vm, l.hostOf(ctx, vm))
	var chain int32
	if on {
		l.post(ctx, &chain, &types.VmStartingEvent{VmEvent: ev})
		l.setPower(ctx, obj, vm.vm, true)
		delete(l.off[cluster], name)
		l.post(ctx, &chain, &types.VmPoweredOnEvent{VmEvent: ev})
		l.record(ctx, vm.ref, name, "PowerOnVM_Task", "VirtualMachine.powerOn", byUser(), chain)
	} else {
		l.post(ctx, &chain, &types.VmStoppingEvent{VmEvent: ev})
		l.setPower(ctx, obj, vm.vm, false)
		l.off[cluster][name] = true
		l.post(ctx, &chain, &types.VmPoweredOffEvent{VmEvent: ev})
		l.record(ctx, vm.ref, name, "PowerOffVM_Task", "VirtualMachine.powerOff", byUser(), chain)
	}
	l.counts(ctx, refs)
	return true
}

// PowerOff implements life.World.
func (l *Life) PowerOff(cluster string, pick uint64) string {
	off := l.off[cluster]
	names := l.names(cluster, func(vm *lifeVM) bool { return !off[vm.vm.Name] })
	if len(names) == 0 {
		return ""
	}
	name := names[pick%uint64(len(names))]
	if !l.power(cluster, name, false) {
		return ""
	}
	return name
}

// PowerOn implements life.World.
func (l *Life) PowerOn(cluster, vm string) bool {
	if !l.off[cluster][vm] {
		return l.vms[cluster][vm] != nil
	}
	return l.power(cluster, vm, true)
}

// counts keeps the cluster's own count of VMs, and of those that are off,
// in step with what is there.
func (l *Life) counts(ctx *simulator.Context, refs *ClusterRefs) {
	obj, ok := l.reg.Get(refs.Ref).(*simulator.ClusterComputeResource)
	if !ok {
		return
	}
	key := refs.Cluster.Key
	ctx.WithLock(obj, func() {
		sum, ok := obj.Summary.(*types.ClusterComputeResourceSummary)
		if !ok || sum.UsageSummary == nil {
			return
		}
		sum.UsageSummary.TotalVmCount = int32(len(l.vms[key]))
		sum.UsageSummary.PoweredOffVmCount = int32(len(l.off[key]))
		sum.NumVmsPerDrsScoreBucket = []int32{0, 0, 0, 0, int32(len(l.vms[key]) - len(l.off[key]))}
		ctx.Update(obj, []types.PropertyChange{{Name: "summary", Val: obj.Summary}})
	})
}

// ---------------------------------------------------------------------------
// VMs that come and go

func refNumber(ref types.ManagedObjectReference) int64 {
	n, _ := strconv.ParseInt(ref.Value[strings.LastIndexByte(ref.Value, '-')+1:], 10, 64)
	return n
}

// AddVM implements life.World: a new VM with the next number the cluster
// has never used, on the host with the fewest VMs. It is made the way the
// inventory builder makes one, so a rebuild makes the same VM again, and
// its place in the identity registry is recorded so that it then gets the
// same reference too.
func (l *Life) AddVM(cluster string, pick uint64) (string, string, error) {
	refs := l.refs(cluster)
	if refs == nil || l.s.registry == nil {
		return "", "", fmt.Errorf("no such cluster")
	}
	cl := refs.Cluster
	ctx := l.as(lifeUser)
	hosts := l.loads(ctx, refs, types.ManagedObjectReference{})
	to := lightest(hosts, nil)
	if to == nil {
		return "", "", fmt.Errorf("no host of %s is in service", cl.Name)
	}
	number := cl.VMHigh + 1
	vm := topology.NewVM(l.s.registry, cl, number)
	for i, ref := range refs.Hosts {
		if ref == to.ref {
			vm.Host = i
		}
	}
	// vcsim writes the VM's files under the datastore's address, which the
	// builder points at the backing directory while it creates VMs and at
	// the vSAN address afterwards; point it back for this one creation
	ds, _ := l.reg.Get(refs.Datastore).(*simulator.Datastore)
	var address string
	if ds != nil {
		ctx.WithLock(ds, func() { address, ds.Summary.Url = ds.Summary.Url, ds.Info.GetDatastoreInfo().Url })
	}
	l.s.tasks = newTaskWaiter(ctx)
	err := l.s.createVM(ctx, refs, vm)
	ctx.Map.RemoveHandler(l.s.tasks)
	l.s.tasks = nil
	if ds != nil {
		ctx.WithLock(ds, func() { ds.Summary.Url = address })
	}
	if err != nil {
		return "", "", err
	}
	cl.VMHigh = number
	ref := refs.VMs[len(refs.VMs)-1]
	if l.s.vmRefSpan > 0 {
		start := refNumber(ref) - l.s.vmRefOffset
		l.s.registry.SetMorefRange(vm.Serial, registry.MorefRange{Start: start, End: start + l.s.vmRefSpan})
	}
	lv := &lifeVM{ref: ref, vm: vm}
	l.vms[cluster][vm.Name] = lv
	cl.SetLiveVMs(append(append([]*topology.VM(nil), cl.LiveVMs()...), vm))
	obj := l.reg.Get(ref).(*simulator.VirtualMachine)
	// the builder shaped the VM after vcsim announced it; say what it is now
	ctx.WithLock(obj, func() {
		ctx.Update(obj, []types.PropertyChange{{Name: "config", Val: obj.Config}, {Name: "runtime", Val: obj.Runtime}, {Name: "summary", Val: obj.Summary}, {Name: "guest", Val: obj.Guest}})
	})
	var chain int32
	ev := l.vmEvent(refs, lv, to.ref)
	l.post(ctx, &chain, &types.VmStartingEvent{VmEvent: ev}, &types.VmPoweredOnEvent{VmEvent: ev})
	l.record(ctx, ref, vm.Name, "PowerOnVM_Task", "VirtualMachine.powerOn", byUser(), chain)
	l.counts(ctx, refs)
	l.space(ctx, refs)
	if l.VMAdded != nil {
		l.VMAdded(cluster, number)
	}
	return vm.Name, l.host(to.ref).Name, nil
}

// RemoveVM implements life.World: a VM is powered off and destroyed. Its
// number and its identities are never used again.
func (l *Life) RemoveVM(cluster string, pick uint64) (string, error) {
	refs := l.refs(cluster)
	names := l.names(cluster, nil)
	if refs == nil || len(names) == 0 {
		return "", nil
	}
	name := names[pick%uint64(len(names))]
	vm := l.vms[cluster][name]
	obj, ok := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
	if !ok {
		return "", fmt.Errorf("%s is not there", name)
	}
	if !l.off[cluster][name] {
		l.power(cluster, name, false)
	}
	ctx := l.as(lifeUser)
	body := obj.DestroyTask(ctx, &types.Destroy_Task{This: vm.ref})
	if f := body.Fault(); f != nil {
		return "", fmt.Errorf("remove %s: %s", name, f.String)
	}
	if t, ok := l.reg.Get(taskOfDestroy(body)).(*simulator.Task); ok {
		t.Wait()
		l.old = append(l.old, oldTask{ref: t.Self, entity: vm.ref, at: time.Now()})
		if t.Info.Error != nil {
			return "", fmt.Errorf("remove %s: %s", name, t.Info.Error.LocalizedMessage)
		}
	}
	delete(l.vms[cluster], name)
	delete(l.off[cluster], name)
	cl := refs.Cluster
	live := make([]*topology.VM, 0, len(cl.LiveVMs()))
	for _, v := range cl.LiveVMs() {
		if v != vm.vm {
			live = append(live, v)
		}
	}
	cl.SetLiveVMs(live)
	l.counts(ctx, refs)
	l.space(ctx, refs)
	if l.VMRemoved != nil {
		l.VMRemoved(cluster, vm.vm.Index+1)
	}
	return name, nil
}

// ---------------------------------------------------------------------------
// maintenance mode

func (l *Life) hostRef(refs *ClusterRefs, name string) (types.ManagedObjectReference, bool) {
	for i, h := range refs.Cluster.Hosts {
		if h.Name == name {
			return refs.Hosts[i], true
		}
	}
	return types.ManagedObjectReference{}, false
}

func (l *Life) setMaintenance(ctx *simulator.Context, hs *simulator.HostSystem, in bool) {
	ctx.WithLock(hs, func() {
		hs.Runtime.InMaintenanceMode = in
		if hs.Summary.Runtime != nil {
			hs.Summary.Runtime.InMaintenanceMode = in
		}
		ctx.Update(hs, []types.PropertyChange{{Name: "runtime", Val: hs.Runtime}, {Name: "summary", Val: hs.Summary}})
	})
}

// EnterMaintenance implements life.World. Every VM on the host moves to the
// emptiest hosts in service first; only then does the host say it is in
// maintenance mode, so no tool ever sees VMs on a host that is.
func (l *Life) EnterMaintenance(cluster, host string) ([]life.Move, error) {
	refs := l.refs(cluster)
	if refs == nil {
		return nil, fmt.Errorf("no such cluster")
	}
	ref, ok := l.hostRef(refs, host)
	if !ok {
		return nil, fmt.Errorf("no such host")
	}
	ctx := l.as(lifeUser)
	hosts := l.loads(ctx, refs, ref)
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no other host of %s is in service", refs.Cluster.Name)
	}
	var chain int32
	hs := l.host(ref)
	l.post(ctx, &chain, &types.EnteringMaintenanceModeEvent{HostEvent: types.HostEvent{Event: l.where(refs, ref)}})
	here, _, _ := l.hostState(ctx, ref)
	byRef := map[types.ManagedObjectReference]*lifeVM{}
	for _, vm := range l.vms[cluster] {
		byRef[vm.ref] = vm
	}
	var vms []*lifeVM
	for _, v := range here {
		if vm := byRef[v]; vm != nil {
			vms = append(vms, vm)
		}
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].vm.Name < vms[j].vm.Name })
	var out []life.Move
	for _, vm := range vms {
		to := lightest(hosts, nil)
		if m, ok := l.move(refs, vm, to.ref, true); ok {
			out = append(out, m)
			to.vms = append(to.vms, vm)
		}
	}
	l.setMaintenance(ctx, hs, true)
	l.post(ctx, &chain, &types.EnteredMaintenanceModeEvent{HostEvent: types.HostEvent{Event: l.where(refs, ref)}})
	l.record(ctx, ref, hs.Name, "EnterMaintenanceMode_Task", "HostSystem.enterMaintenanceMode", byUser(), chain)
	return out, nil
}

// ExitMaintenance implements life.World: the host is in service again, and
// DRS moves between a third and two thirds of an even share of VMs onto it.
func (l *Life) ExitMaintenance(cluster, host string, pick uint64) []life.Move {
	refs := l.refs(cluster)
	if refs == nil {
		return nil
	}
	ref, ok := l.hostRef(refs, host)
	if !ok {
		return nil
	}
	ctx := l.as(lifeUser)
	hs := l.host(ref)
	var chain int32
	l.setMaintenance(ctx, hs, false)
	l.post(ctx, &chain, &types.ExitMaintenanceModeEvent{HostEvent: types.HostEvent{Event: l.where(refs, ref)}})
	l.record(ctx, ref, hs.Name, "ExitMaintenanceMode_Task", "HostSystem.exitMaintenanceMode", byUser(), chain)

	hosts := l.loads(ctx, refs, types.ManagedObjectReference{})
	var me *hostLoad
	total := 0
	for _, h := range hosts {
		total += len(h.vms)
		if h.ref == ref {
			me = h
		}
	}
	if me == nil || len(hosts) < 2 {
		return nil
	}
	want := total / len(hosts) * int(35+pick%31) / 100
	off := l.off[cluster]
	var out []life.Move
	for len(out) < want {
		var from *hostLoad
		for _, h := range hosts {
			if h != me && (from == nil || len(h.vms) > len(from.vms)) {
				from = h
			}
		}
		if from == nil || len(from.vms) <= len(me.vms)+1 {
			break
		}
		at := -1
		for j := range from.vms {
			if i := (j + int(pick%uint64(len(from.vms)))) % len(from.vms); !off[from.vms[i].vm.Name] {
				at = i
				break
			}
		}
		if at < 0 {
			break
		}
		vm := from.take(at)
		if m, ok := l.move(refs, vm, ref, true); ok {
			out = append(out, m)
		}
		me.vms = append(me.vms, vm)
	}
	return out
}

// ---------------------------------------------------------------------------
// datastores and load

// space makes the cluster's vSAN datastore say what is used: the VMs'
// disks and what activity has filled, as the vSAN capacity reads say it.
func (l *Life) space(ctx *simulator.Context, refs *ClusterRefs) (used, capacity int64) {
	ds, ok := l.reg.Get(refs.Datastore).(*simulator.Datastore)
	if !ok {
		return 0, 0
	}
	ctx.WithLock(ds, func() {
		capacity = ds.Summary.Capacity
		used = min(capacity, topology.VsanUsed(refs.Cluster))
		ds.Summary.FreeSpace = capacity - used
		ds.Info.GetDatastoreInfo().FreeSpace = ds.Summary.FreeSpace
		ts := time.Now().UTC()
		ds.Info.GetDatastoreInfo().Timestamp = &ts
		ctx.Update(ds, []types.PropertyChange{{Name: "summary", Val: ds.Summary}, {Name: "info", Val: ds.Info}})
	})
	return used, capacity
}

// A datastore is never filled past nine tenths, nor emptied below a tenth.
const fullest, emptiest = 0.9, 0.1

// Fill implements life.World: the cluster's vSAN datastore gains or loses
// a share of its capacity.
func (l *Life) Fill(cluster string, share float64) []life.Store {
	refs := l.refs(cluster)
	if refs == nil {
		return nil
	}
	ctx := l.as("")
	cl := refs.Cluster
	used, capacity := l.space(ctx, refs)
	if capacity <= 0 {
		return nil
	}
	// filling stops at nine tenths and emptying at a tenth; a datastore that is
	// already beyond either is left where it is
	target := used + int64(share*float64(capacity))
	if share > 0 {
		target = min(target, max(used, int64(fullest*float64(capacity))))
	} else {
		target = max(target, min(used, int64(emptiest*float64(capacity))))
	}
	cl.AddUsed(target - used)
	now, _ := l.space(ctx, refs)
	return []life.Store{{Name: cl.Datastore, Bytes: now - used, UsedPercent: int((now*100 + capacity/2) / capacity)}}
}

// SetLoad implements life.World.
func (l *Life) SetLoad(cluster string, cpu, memory float64) {
	refs := l.refs(cluster)
	load, ok := l.reg.Get(loadRef).(*clusterLoad)
	if refs == nil || !ok {
		return
	}
	next := map[types.ManagedObjectReference][2]float64{}
	if m := load.m.Load(); m != nil {
		for k, v := range *m {
			next[k] = v
		}
	}
	next[refs.Ref] = [2]float64{cpu, memory}
	load.m.Store(&next)
}

// ---------------------------------------------------------------------------
// what a rebuild or a restart puts back

// Live is what activity has changed about the inventory that the scenario
// does not say: where VMs are, which are off, which hosts are in
// maintenance mode and how much the datastores have filled.
type Live struct {
	// Hosts is the host of each VM that is not on the one the inventory
	// builder puts it on, by VM key.
	Hosts map[string]string `json:"hosts,omitempty"`
	// Off lists the VMs that are powered off, by key.
	Off []string `json:"off,omitempty"`
	// Maintenance lists the hosts in maintenance mode, by key.
	Maintenance []string `json:"maintenance,omitempty"`
	// Filled is how many bytes activity has added to each cluster's vSAN
	// datastore (or freed, when negative), by cluster key.
	Filled map[string]int64 `json:"filled,omitempty"`
}

// Snapshot reads what activity has changed.
func (l *Life) Snapshot() Live {
	ctx := l.as("")
	out := Live{Hosts: map[string]string{}, Filled: map[string]int64{}}
	for _, refs := range l.s.Clusters {
		cl := refs.Cluster
		name := map[types.ManagedObjectReference]string{}
		for i, ref := range refs.Hosts {
			name[ref] = cl.Hosts[i].Name
			if _, maint, _ := l.hostState(ctx, ref); maint {
				out.Maintenance = append(out.Maintenance, cl.Hosts[i].Key)
			}
		}
		for _, vm := range l.vms[cl.Key] {
			if h := name[l.hostOf(ctx, vm)]; h != "" && h != cl.Hosts[vm.vm.Host%len(cl.Hosts)].Name {
				out.Hosts[vm.vm.Key] = h
			}
			if l.off[cl.Key][vm.vm.Name] {
				out.Off = append(out.Off, vm.vm.Key)
			}
		}
		if d := cl.UsedDelta(); d != 0 {
			out.Filled[cl.Key] = d
		}
	}
	sort.Strings(out.Off)
	sort.Strings(out.Maintenance)
	return out
}

// Restore puts back what a Snapshot read, on an inventory that has just
// been built and that no tool is connected to yet. Nothing is recorded: no
// event, no task. What no longer fits (a VM or host that is gone) is left
// out.
func (l *Life) Restore(live Live) {
	ctx := l.as("")
	off := map[string]bool{}
	for _, k := range live.Off {
		off[k] = true
	}
	maint := map[string]bool{}
	for _, k := range live.Maintenance {
		maint[k] = true
	}
	for _, refs := range l.s.Clusters {
		cl := refs.Cluster
		byName := map[string]types.ManagedObjectReference{}
		for i, h := range cl.Hosts {
			byName[h.Name] = refs.Hosts[i]
			if maint[h.Key] {
				hs := l.host(refs.Hosts[i])
				hs.Runtime.InMaintenanceMode = true
				if hs.Summary.Runtime != nil {
					hs.Summary.Runtime.InMaintenanceMode = true
				}
			}
		}
		// a VM whose host is now in maintenance mode, or gone, goes to the emptiest host in service
		for _, name := range l.names(cl.Key, nil) {
			vm := l.vms[cl.Key][name]
			to, ok := byName[live.Hosts[vm.vm.Key]]
			if !ok {
				to = refs.Hosts[vm.vm.Host%len(refs.Hosts)]
			}
			if hs := l.host(to); hs == nil || hs.Runtime.InMaintenanceMode {
				if free := lightest(l.loads(ctx, refs, types.ManagedObjectReference{}), nil); free != nil {
					to = free.ref
				}
			}
			relocateVM(ctx, vm.ref, to)
			if off[vm.vm.Key] {
				if obj, ok := l.reg.Get(vm.ref).(*simulator.VirtualMachine); ok {
					l.setPower(ctx, obj, vm.vm, false)
					l.off[cl.Key][name] = true
				}
			}
		}
		if d := live.Filled[cl.Key]; d != 0 {
			cl.AddUsed(d)
		}
		l.counts(ctx, refs)
		l.space(ctx, refs)
	}
}
