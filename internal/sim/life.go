package sim

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
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
	// posted counts the events and tasks recorded, by type.
	posted map[string]int
	ctxs   map[string]*simulator.Context
	// apart is who reads a task's state: never the one running it
	apart simulator.Context

	// VMAdded and VMRemoved are told a VM number a cluster gained or lost,
	// so the caller can record it where a rebuild will find it.
	VMAdded, VMRemoved func(cluster string, number int)
	// Now is the clock a finished task's age is counted on; the real one
	// unless the owner of the simulator keeps another.
	Now func() time.Time
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
		l := &Life{s: s, reg: s.model.Service.Context.Map, vms: map[string]map[string]*lifeVM{}, off: map[string]map[string]bool{}, posted: map[string]int{}, ctxs: map[string]*simulator.Context{}}
		for _, refs := range s.Clusters {
			idx := map[string]*lifeVM{}
			for i, vm := range refs.Cluster.VMs {
				idx[vm.Name] = &lifeVM{ref: refs.VMs[i], vm: vm}
			}
			l.vms[refs.Cluster.Key] = idx
			l.off[refs.Cluster.Key] = map[string]bool{}
		}
		l.reg.Put(&clusterLoad{ManagedObjectReference: loadRef})
		// vcsim's event catalogue has no words for some of what a real
		// vCenter records; these are a real vCenter 9.1.1's
		em := l.reg.EventManager()
		l.as("").WithLock(em, func() {
			have := map[string]bool{}
			for _, info := range em.Description.EventInfo {
				have[info.Key] = true
			}
			for _, d := range [][3]string{
				{"TaskEvent", "Task event", "Task: {{.Info.DescriptionId}}"},
				{"VmEmigratingEvent", "VM emigrating", "Migrating {{.Vm.Name}} off host {{.Host.Name}} in {{.Datacenter.Name}}"},
				{"VmBeingRelocatedEvent", "VM relocating", "Relocating {{.Vm.Name}} from {{.Host.Name}}, {{.Ds.Name}} in {{.Datacenter.Name}} to {{.DestHost.Name}}, {{.DestDatastore.Name}} in {{.DestDatacenter.Name}}"},
				{"HostDasDisabledEvent", "vSphere HA agent disabled on host", "vSphere HA agent on {{.Host.Name}} in cluster {{.ComputeResource.Name}} in {{.Datacenter.Name}} is disabled"},
				{"HostDasEnabledEvent", "vSphere HA agent enabled on host", "vSphere HA agent on {{.Host.Name}} in cluster {{.ComputeResource.Name}} in {{.Datacenter.Name}} is enabled"},
			} {
				if !have[d[0]] {
					em.Description.EventInfo = append(em.Description.EventInfo, types.EventDescriptionEventDetail{Key: d[0], Description: d[1], Category: "info", FullFormat: d[2]})
				}
			}
		})
		for _, spec := range activityHostAlarms {
			ctx := l.as(lifeUser)
			am := ctx.Map.AlarmManager()
			ctx.WithLock(am, func() {
				am.CreateAlarm(ctx, &types.CreateAlarm{This: am.Self, Entity: s.model.ServiceContent.RootFolder, Spec: &spec})
			})
		}
		s.life = l
	})
	return s.life
}

// as is a context that acts as a user: "" for what the system does itself.
func (l *Life) as(user string) *simulator.Context {
	// one context for each user, kept: vcsim's object locks remember who
	// held them last, so a new one for every operation would never be freed
	if c, ok := l.ctxs[user]; ok {
		return c
	}
	c := *l.s.model.Service.Context
	c.Session = &simulator.Session{UserSession: types.UserSession{UserName: user}, Registry: simulator.NewRegistry(), Map: c.Map}
	l.ctxs[user] = &c
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

// What a real vCenter 9.1.1 records for one operation, read from one:
// first a TaskEvent that carries the task as it was queued, and then the
// operation's own events, every one with the TaskEvent's key as its chain
// id, which the task itself carries too.

// post records events in a chain, as the user of ctx. Chain 0 starts one.
// vcsim gives every event its own chain, so the chain is set as soon as
// the event is posted, before anything else here can post another.
func (l *Life) post(ctx *simulator.Context, chain int32, events ...types.BaseEvent) {
	em := ctx.Map.EventManager()
	for _, ev := range events {
		ctx.WithLock(em, func() {
			em.PostEvent(ctx, &types.PostEvent{EventToPost: ev})
			if chain != 0 {
				ev.GetEvent().ChainId = chain
			}
		})
		l.posted[eventName(ev)]++
	}
}

// eventName is an event's type; for the events vCenter added later
// (EventEx), the last part of its id.
func eventName(ev types.BaseEvent) string {
	if ex, ok := ev.(*types.EventEx); ok {
		return ex.EventTypeId[strings.LastIndex(ex.EventTypeId, ".")+1:]
	}
	return reflect.TypeOf(ev).Elem().Name()
}

// head posts the TaskEvent that starts an operation: where it happens and
// the task as queued. Its key is the chain id of the operation.
func (l *Life) head(ctx *simulator.Context, at types.Event, info types.TaskInfo) (*types.TaskEvent, int32) {
	at.Ds = nil
	info.State = types.TaskInfoStateQueued
	info.StartTime, info.CompleteTime, info.Result, info.EventChainId = nil, nil, nil, 0
	info.Cancelable = true
	ev := &types.TaskEvent{Event: at, Info: info}
	l.post(ctx, 0, ev)
	return ev, ev.Key
}

// begin starts an operation: its task, named as a real vCenter names it
// and started by who or what starts it there, and the TaskEvent. It
// returns the chain id for the operation's events. Name is empty for a
// task vCenter starts by itself.
func (l *Life) begin(ctx *simulator.Context, at types.Event, entity types.ManagedObjectReference, entityName, name, description string, reason types.BaseTaskReason) int32 {
	t := simulator.CreateTask(entity, description, func(*simulator.Task) (types.AnyType, types.BaseMethodFault) { return nil, nil })
	t.Info.Name = name
	t.Info.DescriptionId = description
	t.Info.EntityName = entityName
	t.Info.Reason = reason
	t.Run(ctx)
	l.wait(t)
	var info types.TaskInfo
	l.reg.WithLock(&l.apart, t, func() { info = t.Info })
	_, chain := l.head(ctx, at, info)
	l.own(ctx, t, entity, chain, "", nil)
	return chain
}

// own gives a finished task the chain of its events and notes it, to be
// forgotten when it is old. A task vcsim made itself is also given the
// name a real vCenter gives it and told who started it.
func (l *Life) own(ctx *simulator.Context, t *simulator.Task, entity types.ManagedObjectReference, chain int32, name string, by types.BaseTaskReason) {
	ctx.WithLock(t, func() {
		// Re-announce the final state with its now-complete metadata. vcsim's
		// TaskHistoryCollector refreshes latestPage only on info.state updates.
		changes := []types.PropertyChange{{Name: "info.eventChainId", Val: chain}, {Name: "info.state", Val: t.Info.State}}
		if by != nil {
			changes = append(changes, types.PropertyChange{Name: "info.name", Val: name}, types.PropertyChange{Name: "info.reason", Val: by})
		}
		updateLife(ctx, t, changes)
	})
	l.posted["task "+t.Info.DescriptionId]++
	l.old = append(l.old, oldTask{ref: t.Self, entity: entity, at: l.now()})
}

// wait is vcsim's Task.Wait without its 10 ms naps: these tasks do
// nothing, so they are done almost at once, and a host's 25 VMs must not
// take a quarter of a second to leave it.
func (l *Life) wait(t *simulator.Task) {
	for {
		done := false
		l.reg.WithLock(&l.apart, t, func() {
			done = t.Info.State == types.TaskInfoStateSuccess || t.Info.State == types.TaskInfoStateError
		})
		if done {
			return
		}
		time.Sleep(50 * time.Microsecond)
	}
}

// forgetParts forgets, at once, the tasks vcsim made for the parts of a
// VM's removal (its files and disks): a real vCenter lists the removal only.
func (l *Life) forgetParts(ctx *simulator.Context, after types.ManagedObjectReference) {
	tm, ok := l.reg.Get(*l.s.model.ServiceContent.TaskManager).(*simulator.TaskManager)
	if !ok {
		return
	}
	var recent []types.ManagedObjectReference
	ctx.WithLock(tm, func() { recent = append(recent, tm.RecentTask...) })
	for _, ref := range recent {
		t, ok := l.reg.Get(ref).(*simulator.Task)
		if !ok || refNumber(ref) <= refNumber(after) {
			continue
		}
		var part bool
		var entity types.ManagedObjectReference
		l.reg.WithLock(&l.apart, t, func() {
			id := t.Info.DescriptionId
			part = strings.HasPrefix(id, "FileManager.delete") || strings.HasPrefix(id, "VirtualDiskManager.delete")
			if t.Info.Entity != nil {
				entity = *t.Info.Entity
			}
		})
		if part {
			l.wait(t)
			l.forget(ctx, tm, ref, entity)
		}
	}
}

// adopt puts the events vcsim itself posted about a VM, since the
// TaskEvent of an operation, into that operation's chain.
func (l *Life) adopt(ctx *simulator.Context, chain int32, vm types.ManagedObjectReference) []types.BaseEvent {
	em := ctx.Map.EventManager()
	var page, out []types.BaseEvent
	ctx.WithLock(em, func() {
		if body, ok := em.QueryEvents(ctx, &types.QueryEvents{This: em.Self, Filter: types.EventFilterSpec{MaxCount: 20}}).(*methods.QueryEventsBody); ok && body.Res != nil {
			page = body.Res.Returnval
		}
		for _, ev := range page {
			if e := ev.GetEvent(); e.Key > chain && e.Vm != nil && e.Vm.Vm == vm {
				e.ChainId = chain
				e.UserName = ctx.Session.UserName
				out = append(out, ev)
			}
		}
	})
	for _, ev := range out {
		l.posted[eventName(ev)]++
	}
	return out
}

func (l *Life) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Posted is how many events and tasks have been recorded since the vCenter
// was built, by event type and by task ("task " and its description id).
func (l *Life) Posted() map[string]int {
	out := make(map[string]int, len(l.posted))
	for k, v := range l.posted {
		out[k] = v
	}
	return out
}

func byUser() types.BaseTaskReason   { return &types.TaskReasonUser{UserName: lifeUser} }
func bySystem() types.BaseTaskReason { return &types.TaskReasonSystem{} }

// taskAge is how long a finished task stays in the recent task lists, as
// on a real vCenter.
const taskAge = 10 * time.Minute

// Sweep forgets the tasks of what was done more than ten minutes ago, so
// they do not pile up for as long as the simulator runs.
func (l *Life) Sweep(now time.Time) {
	ctx := l.as("")
	tm, _ := l.reg.Get(*l.s.model.ServiceContent.TaskManager).(*simulator.TaskManager)
	n := 0
	for _, o := range l.old {
		if now.Sub(o.at) < taskAge {
			break
		}
		n++
		l.forget(ctx, tm, o.ref, o.entity)
	}
	if n > 0 {
		// copied, so the tasks already forgotten do not stay in memory behind the rest
		l.old = append([]oldTask(nil), l.old[n:]...)
	}
}

// forget takes a task out of vCenter's list, its entity's and vCenter.
func (l *Life) forget(ctx *simulator.Context, tm *simulator.TaskManager, ref, entity types.ManagedObjectReference) {
	if tm != nil {
		ctx.WithLock(tm, func() {
			updateLife(ctx, tm, []types.PropertyChange{{Name: "recentTask", Val: without(tm.RecentTask, ref)}})
		})
	}
	if e, ok := l.reg.Get(entity).(mo.Entity); ok {
		ctx.WithLock(e, func() {
			updateLife(ctx, e, []types.PropertyChange{{Name: "recentTask", Val: without(e.Entity().RecentTask, ref)}})
		})
	}
	l.reg.Remove(ctx, ref)
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

// move takes a VM to another host of its cluster: the VM's host and the two
// hosts' VM lists, with what a real vCenter records. A running VM moves by
// vMotion, which DRS starts by itself in a chain of its own. A VM that is
// off is relocated, within the chain of the operation that moves it
// (a host going into maintenance mode) or, with within 0, in its own.
func (l *Life) move(refs *ClusterRefs, vm *lifeVM, to types.ManagedObjectReference, within int32) (life.Move, bool) {
	sys := l.as("")
	from := l.hostOf(sys, vm)
	src, dst := l.host(from), l.host(to)
	obj, ok := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
	if !ok || src == nil || dst == nil || from == to {
		return life.Move{}, false
	}
	begin := l.vmEvent(refs, vm, from)
	dest := types.HostEventArgument{EntityEventArgument: types.EntityEventArgument{Name: dst.Name}, Host: to}
	source := types.HostEventArgument{EntityEventArgument: types.EntityEventArgument{Name: src.Name}, Host: from}
	place := func(ctx *simulator.Context) {
		ctx.WithLock(src, func() { updateLife(ctx, src, []types.PropertyChange{{Name: "vm", Val: without(src.Vm, vm.ref)}}) })
		ctx.WithLock(dst, func() {
			updateLife(ctx, dst, []types.PropertyChange{{Name: "vm", Val: append(without(dst.Vm, vm.ref), vm.ref)}})
		})
		ctx.WithLock(obj, func() {
			updateLife(ctx, obj, []types.PropertyChange{{Name: "runtime.host", Val: to}, {Name: "summary.runtime.host", Val: to}})
		})
	}
	if !l.off[refs.Cluster.Key][vm.vm.Name] {
		chain := l.begin(sys, begin.Event, vm.ref, vm.vm.Name, "", "Drm.ExecuteVMotionLRO", bySystem())
		leaving := begin
		leaving.Ds = nil
		l.post(sys, chain, hotMigrating(begin, dst.Name), &types.VmEmigratingEvent{VmEvent: leaving})
		place(sys)
		l.post(sys, chain, &types.DrsVmMigratedEvent{VmMigratedEvent: types.VmMigratedEvent{VmEvent: l.vmEvent(refs, vm, to), SourceHost: source, SourceDatacenter: begin.Datacenter, SourceDatastore: begin.Ds}})
	} else {
		ctx := l.as(lifeUser)
		chain := within
		if chain == 0 {
			chain = l.begin(ctx, begin.Event, vm.ref, vm.vm.Name, "RelocateVM_Task", "VirtualMachine.relocate", byUser())
		}
		l.post(ctx, chain, &types.VmBeingRelocatedEvent{VmRelocateSpecEvent: types.VmRelocateSpecEvent{VmEvent: begin}, DestHost: dest, DestDatacenter: begin.Datacenter, DestDatastore: begin.Ds})
		place(ctx)
		l.post(ctx, chain, &types.VmRelocatedEvent{VmRelocateSpecEvent: types.VmRelocateSpecEvent{VmEvent: l.vmEvent(refs, vm, to)}, SourceHost: source, SourceDatacenter: begin.Datacenter, SourceDatastore: begin.Ds})
	}
	return life.Move{VM: vm.vm.Name, From: src.Name, To: dst.Name}, true
}

// hotMigrating is the event a vCenter of this age starts a vMotion with.
func hotMigrating(at types.VmEvent, destHost string) *types.EventEx {
	e := at.Event
	e.FullFormattedMessage = fmt.Sprintf("Hot migrating %s from %s, %s in %s to %s, %s in %s with encryption", e.Vm.Name, e.Host.Name, e.Ds.Name, e.Datacenter.Name, destHost, e.Ds.Name, e.Datacenter.Name)
	return &types.EventEx{Event: e, EventTypeId: "com.vmware.vc.vm.VmHotMigratingWithEncryptionEvent", Severity: "info",
		Arguments: []types.KeyAnyValue{{Key: "destHost", Value: destHost}, {Key: "destDatastore", Value: e.Ds.Name}, {Key: "destDatacenter", Value: e.Datacenter.Name}},
		ObjectId:  e.Vm.Vm.Value, ObjectType: "VirtualMachine", ObjectName: e.Vm.Name}
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
		if m, ok := l.move(refs, vm, to.ref, 0); ok {
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
		updateLife(ctx, obj, []types.PropertyChange{
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
	if _, maint, down := l.hostState(ctx, l.hostOf(ctx, vm)); maint || down {
		return false
	}
	ev := l.vmEvent(refs, vm, l.hostOf(ctx, vm))
	ev.Ds = nil
	if on {
		chain := l.begin(ctx, ev.Event, vm.ref, name, "PowerOnVM_Task", "VirtualMachine.powerOn", byUser())
		l.post(ctx, chain, &types.VmStartingEvent{VmEvent: ev})
		l.setPower(ctx, obj, vm.vm, true)
		delete(l.off[cluster], name)
		l.post(ctx, chain, &types.VmPoweredOnEvent{VmEvent: ev})
	} else {
		chain := l.begin(ctx, ev.Event, vm.ref, name, "PowerOffVM_Task", "VirtualMachine.powerOff", byUser())
		l.post(ctx, chain, &types.VmStoppingEvent{VmEvent: ev})
		l.setPower(ctx, obj, vm.vm, false)
		l.off[cluster][name] = true
		l.post(ctx, chain, &types.VmPoweredOffEvent{VmEvent: ev})
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
func (l *Life) PowerOn(cluster, vm string) error {
	v := l.vms[cluster][vm]
	if v == nil {
		return fmt.Errorf("VM no longer exists")
	}
	if !l.off[cluster][vm] {
		return nil
	}
	ctx := l.as(lifeUser)
	if _, maint, down := l.hostState(ctx, l.hostOf(ctx, v)); maint || down {
		return life.ErrHostUnavailable
	}
	if !l.power(cluster, vm, true) {
		return fmt.Errorf("VM could not be powered on")
	}
	return nil
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
		updateLife(ctx, obj, []types.PropertyChange{{Name: "summary", Val: obj.Summary}})
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
	// the builder would put this number on built; it is made on the host
	// with the fewest VMs instead, and built is put back afterwards so that
	// a snapshot records where the VM really is
	built := vm.Host
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
	// the creation starts as on a real vCenter, with the event of its task;
	// vcsim then makes the task and posts the rest, which join the chain
	folder := l.s.dcs[cl.Datacenter].vmFolder
	queued := time.Now()
	te, chain := l.head(ctx, types.Event{Datacenter: l.where(refs, to.ref).Datacenter}, types.TaskInfo{Name: "CreateVM_Task", DescriptionId: "Folder.createVm", Entity: &folder.Self, EntityName: folder.Name, Reason: byUser(), QueueTime: queued})
	l.s.tasks = newTaskWaiter(ctx)
	err := l.s.createVM(ctx, refs, vm)
	vm.Host = built
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
	l.publishMembership(ctx, obj, true)
	// the builder shaped the VM after vcsim announced it; say what it is now
	ctx.WithLock(obj, func() {
		updateLife(ctx, obj, []types.PropertyChange{{Name: "config", Val: obj.Config}, {Name: "runtime", Val: obj.Runtime}, {Name: "summary", Val: obj.Summary}, {Name: "guest", Val: obj.Guest}})
	})
	var made types.ManagedObjectReference
	ctx.WithLock(folder, func() {
		if n := len(folder.RecentTask); n > 0 {
			made = folder.RecentTask[n-1]
		}
	})
	if t, ok := l.reg.Get(made).(*simulator.Task); ok {
		te.Info.Key, te.Info.Task = t.Self.Value, t.Self
		l.own(ctx, t, folder.Self, chain, "CreateVM_Task", byUser())
	}
	// vcsim announced the VM before the builder gave it its own identities
	for _, e := range l.adopt(ctx, chain, ref) {
		switch e := e.(type) {
		case *types.VmUuidAssignedEvent:
			e.FullFormattedMessage = strings.ReplaceAll(e.FullFormattedMessage, e.Uuid, vm.UUID)
			e.Uuid = vm.UUID
		case *types.VmInstanceUuidAssignedEvent:
			e.FullFormattedMessage = strings.ReplaceAll(e.FullFormattedMessage, e.InstanceUuid, vm.InstanceUUID)
			e.InstanceUuid = vm.InstanceUUID
		}
	}
	ev := l.vmEvent(refs, lv, to.ref)
	ev.Ds = nil
	on := l.begin(ctx, ev.Event, ref, vm.Name, "PowerOnVM_Task", "VirtualMachine.powerOn", byUser())
	l.post(ctx, on, &types.VmStartingEvent{VmEvent: ev}, &types.VmPoweredOnEvent{VmEvent: ev})
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
	names := l.names(cluster, func(vm *lifeVM) bool {
		ctx := l.as("")
		_, maint, down := l.hostState(ctx, l.hostOf(ctx, vm))
		if maint || down {
			return false
		}
		obj, _ := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
		if obj == nil {
			return false
		}
		var backup bool
		ctx.WithLock(obj, func() { backup = findBackup(obj) != nil })
		return !backup
	})
	if refs == nil || len(names) == 0 {
		return "", nil
	}
	name := names[pick%uint64(len(names))]
	vm := l.vms[cluster][name]
	obj, ok := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
	if !ok {
		return "", fmt.Errorf("%s is not there", name)
	}
	wasOn := !l.off[cluster][name]
	if wasOn {
		l.power(cluster, name, false)
	}
	kept := func(err error) (string, error) {
		if wasOn {
			l.power(cluster, name, true)
		}
		return "", err
	}
	ctx := l.as(lifeUser)
	te, chain := l.head(ctx, l.vmEvent(refs, vm, l.hostOf(ctx, vm)).Event, types.TaskInfo{Name: "Destroy_Task", DescriptionId: "VirtualMachine.destroy", Entity: &vm.ref, EntityName: name, Reason: byUser(), QueueTime: time.Now()})
	// vcsim deletes the VM's files under the datastore's address; see AddVM
	ds, _ := l.reg.Get(refs.Datastore).(*simulator.Datastore)
	var address string
	if ds != nil {
		ctx.WithLock(ds, func() { address, ds.Summary.Url = ds.Summary.Url, ds.Info.GetDatastoreInfo().Url })
	}
	body := obj.DestroyTask(ctx, &types.Destroy_Task{This: vm.ref})
	if f := body.Fault(); f != nil {
		if ds != nil {
			ctx.WithLock(ds, func() { ds.Summary.Url = address })
		}
		return kept(fmt.Errorf("remove %s: %s", name, f.String))
	}
	t, ok := l.reg.Get(taskOfDestroy(body)).(*simulator.Task)
	if ok {
		l.wait(t)
		// the tasks that delete the files run by themselves: they are waited
		// for while the datastore still points at the files
		l.forgetParts(ctx, t.Self)
	}
	if ds != nil {
		ctx.WithLock(ds, func() { ds.Summary.Url = address })
	}
	if ok {
		te.Info.Key, te.Info.Task = t.Self.Value, t.Self
		l.own(ctx, t, vm.ref, chain, "Destroy_Task", byUser())
		if t.Info.Error != nil {
			return kept(fmt.Errorf("remove %s: %s", name, t.Info.Error.LocalizedMessage))
		}
	}
	l.adopt(ctx, chain, vm.ref)
	l.publishMembership(ctx, obj, false)
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

// ha says whether the cluster has vSphere HA on, which a host leaves and
// joins as it goes into and out of maintenance mode.
func (l *Life) ha(refs *ClusterRefs) bool {
	ccr, ok := l.reg.Get(refs.Ref).(*simulator.ClusterComputeResource)
	if !ok {
		return false
	}
	cfg, ok := ccr.ConfigurationEx.(*types.ClusterConfigInfoEx)
	return ok && cfg.DasConfig.Enabled != nil && *cfg.DasConfig.Enabled
}

func (l *Life) setMaintenance(ctx *simulator.Context, hs *simulator.HostSystem, in bool) {
	ctx.WithLock(hs, func() {
		hs.Runtime.InMaintenanceMode = in
		if hs.Summary.Runtime != nil {
			hs.Summary.Runtime.InMaintenanceMode = in
		}
		updateLife(ctx, hs, []types.PropertyChange{{Name: "runtime", Val: hs.Runtime}, {Name: "summary", Val: hs.Summary}})
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
	hs := l.host(ref)
	chain := l.begin(ctx, l.where(refs, ref), ref, hs.Name, "EnterMaintenanceMode_Task", "HostSystem.enterMaintenanceMode", byUser())
	l.post(ctx, chain, &types.EnteringMaintenanceModeEvent{HostEvent: types.HostEvent{Event: l.where(refs, ref)}})
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
		if m, ok := l.move(refs, vm, to.ref, chain); ok {
			out = append(out, m)
			to.vms = append(to.vms, vm)
		}
	}
	if l.ha(refs) {
		l.post(ctx, chain, &types.HostDasDisabledEvent{HostEvent: types.HostEvent{Event: l.where(refs, ref)}})
	}
	l.setMaintenance(ctx, hs, true)
	l.post(ctx, chain, &types.EnteredMaintenanceModeEvent{HostEvent: types.HostEvent{Event: l.where(refs, ref)}})
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
	chain := l.begin(ctx, l.where(refs, ref), ref, hs.Name, "ExitMaintenanceMode_Task", "HostSystem.exitMaintenanceMode", byUser())
	l.setMaintenance(ctx, hs, false)
	l.post(ctx, chain, &types.ExitMaintenanceModeEvent{HostEvent: types.HostEvent{Event: l.where(refs, ref)}})
	if l.ha(refs) {
		l.post(ctx, chain, &types.HostDasEnabledEvent{HostEvent: types.HostEvent{Event: l.where(refs, ref)}})
	}

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
		if m, ok := l.move(refs, vm, ref, 0); ok {
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
		updateLife(ctx, ds, []types.PropertyChange{{Name: "summary", Val: ds.Summary}, {Name: "info", Val: ds.Info}})
	})
	return used, capacity
}

// A datastore is never filled past nine tenths. A cleanup frees only what
// activity filled: what the VMs' own disks use stays used.
const fullest = 0.9

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
	target := used + int64(share*float64(capacity))
	if share > 0 {
		target = min(target, max(used, int64(fullest*float64(capacity))))
	} else {
		target = max(target, used-max(0, cl.UsedDelta()))
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
	old, exists := next[refs.Ref]
	next[refs.Ref] = [2]float64{cpu, memory}
	load.m.Store(&next)
	if !exists {
		old = [2]float64{1, 1}
	}
	if old == next[refs.Ref] {
		return
	}
	l.publishLoad(l.as(""), refs)
}

func (l *Life) publishLoad(ctx *simulator.Context, refs *ClusterRefs) {
	for _, ref := range append(append([]types.ManagedObjectReference(nil), refs.Hosts...), refs.VMs...) {
		obj := l.reg.Get(ref)
		switch o := obj.(type) {
		case *simulator.HostSystem:
			ctx.WithLock(o, func() {
				updateLife(ctx, o, []types.PropertyChange{{Name: "summary.quickStats", Val: o.Summary.QuickStats}})
			})
		case *simulator.VirtualMachine:
			ctx.WithLock(o, func() {
				updateLife(ctx, o, []types.PropertyChange{{Name: "summary.quickStats", Val: o.Summary.QuickStats}})
			})
		}
	}
}

// ---------------------------------------------------------------------------
// what a rebuild or a restart puts back

// Live is what activity has changed about the inventory that the scenario
// does not say: where VMs are, which are off, which hosts are in
// maintenance mode and how much the datastores have filled.
type Live struct {
	Down     map[string]bool            `json:"down,omitempty"`
	Products map[string]types.AboutInfo `json:"products,omitempty"`
	Backups  map[string]time.Time       `json:"backups,omitempty"`
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
	out := Live{Hosts: map[string]string{}, Filled: map[string]int64{}, Down: map[string]bool{}, Products: map[string]types.AboutInfo{}, Backups: map[string]time.Time{}}
	for _, refs := range l.s.Clusters {
		cl := refs.Cluster
		name := map[types.ManagedObjectReference]string{}
		for i, ref := range refs.Hosts {
			name[ref] = cl.Hosts[i].Name
			hs := l.host(ref)
			ctx.WithLock(hs, func() {
				out.Down[cl.Hosts[i].Key] = hs.Runtime.ConnectionState != types.HostSystemConnectionStateConnected
				if hs.Config != nil {
					out.Products[cl.Hosts[i].Key] = hs.Config.Product
				}
			})
			if _, maint, _ := l.hostState(ctx, ref); maint {
				out.Maintenance = append(out.Maintenance, cl.Hosts[i].Key)
			}
		}
		for _, vm := range l.vms[cl.Key] {
			obj := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
			ctx.WithLock(obj, func() {
				if ref := findBackup(obj); ref != nil {
					out.Backups[vm.vm.Key] = l.now()
					for _, n := range obj.Snapshot.RootSnapshotList {
						if n.Snapshot == *ref {
							out.Backups[vm.vm.Key] = n.CreateTime
						}
					}
				}
			})
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
			byName[shortName(h.Name)] = refs.Hosts[i]
			hs := l.host(refs.Hosts[i])
			if p, ok := live.Products[h.Key]; ok && hs.Config != nil && p.Version == hs.Config.Product.Version {
				ctx.WithLock(hs, func() {
					hs.Config.Product = p
					hs.Summary.Config.Product = &hs.Config.Product
					updateLife(ctx, hs, []types.PropertyChange{{Name: "config.product", Val: &hs.Config.Product}, {Name: "summary.config", Val: hs.Summary.Config}})
				})
			}
			if live.Down[h.Key] {
				l.connection(ctx, hs, true)
				if !maint[h.Key] {
					l.hostAlarm(refs, hs, "Host connection and power state", true, false)
					l.hostAlarm(refs, hs, "vSphere HA host status", true, false)
				}
			}
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
			to, ok := byName[shortName(live.Hosts[vm.vm.Key])]
			if !ok {
				to = refs.Hosts[vm.vm.Host%len(refs.Hosts)]
			}
			if hs := l.host(to); hs == nil || hs.Runtime.InMaintenanceMode {
				if free := lightest(l.loads(ctx, refs, types.ManagedObjectReference{}), nil); free != nil {
					to = free.ref
				}
			}
			l.placeVM(ctx, vm.ref, to)
			obj := l.reg.Get(vm.ref).(*simulator.VirtualMachine)
			if hs := l.host(to); hs != nil && hs.Runtime.ConnectionState != types.HostSystemConnectionStateConnected {
				ctx.WithLock(obj, func() {
					updateLife(ctx, obj, []types.PropertyChange{{Name: "runtime.connectionState", Val: types.VirtualMachineConnectionStateDisconnected}, {Name: "summary.runtime.connectionState", Val: types.VirtualMachineConnectionStateDisconnected}})
				})
			}
			if at, ok := live.Backups[vm.vm.Key]; ok {
				ctx.WithLock(obj, func() { l.restoreBackup(ctx, obj, at) })
			}
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
