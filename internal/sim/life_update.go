package sim

import (
	"strings"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// updateLife is the publication boundary for Activity state. Keep the parent
// assignment for aggregate subscribers and explicitly publish its observable
// children: govmomi promotes child updates to parents, but not the reverse.
// Call under the object's lock. New behaviours must publish their changes here.
func updateLife(ctx *simulator.Context, obj mo.Reference, changes []types.PropertyChange) {
	out := append([]types.PropertyChange(nil), changes...)
	add := func(name string, value any) { out = append(out, types.PropertyChange{Name: name, Val: value}) }
	hostRuntime := func(path string, r types.HostRuntimeInfo) {
		add(path+".inMaintenanceMode", r.InMaintenanceMode)
		add(path+".connectionState", r.ConnectionState)
		add(path+".powerState", r.PowerState)
		add(path+".bootTime", r.BootTime)
	}
	vmRuntime := func(path string, r types.VirtualMachineRuntimeInfo) {
		add(path+".host", r.Host)
		add(path+".powerState", r.PowerState)
		add(path+".connectionState", r.ConnectionState)
		add(path+".faultToleranceState", r.FaultToleranceState)
		add(path+".maxCpuUsage", r.MaxCpuUsage)
		add(path+".maxMemoryUsage", r.MaxMemoryUsage)
		add(path+".bootTime", r.BootTime)
	}
	vmQuick := func(path string, q types.VirtualMachineQuickStats) {
		add(path+".overallCpuUsage", q.OverallCpuUsage)
		add(path+".overallCpuDemand", q.OverallCpuDemand)
		add(path+".guestMemoryUsage", q.GuestMemoryUsage)
		add(path+".hostMemoryUsage", q.HostMemoryUsage)
		add(path+".guestHeartbeatStatus", q.GuestHeartbeatStatus)
		add(path+".uptimeSeconds", q.UptimeSeconds)
	}

	for _, c := range changes {
		switch v := c.Val.(type) {
		case types.HostRuntimeInfo:
			hostRuntime(c.Name, v)
		case types.HostListSummary:
			if v.Runtime != nil {
				add(c.Name+".runtime", v.Runtime)
				hostRuntime(c.Name+".runtime", *v.Runtime)
			}
		case types.VirtualMachineRuntimeInfo:
			vmRuntime(c.Name, v)
		case types.VirtualMachineSummary:
			add(c.Name+".runtime", v.Runtime)
			vmRuntime(c.Name+".runtime", v.Runtime)
			add(c.Name+".config", v.Config)
			add(c.Name+".guest", v.Guest)
			add(c.Name+".quickStats", v.QuickStats)
			vmQuick(c.Name+".quickStats", v.QuickStats)
			add(c.Name+".config.uuid", v.Config.Uuid)
			add(c.Name+".config.instanceUuid", v.Config.InstanceUuid)
			add(c.Name+".guest.toolsStatus", v.Guest.ToolsStatus)
			add(c.Name+".guest.toolsRunningStatus", v.Guest.ToolsRunningStatus)
			add(c.Name+".guest.hostName", v.Guest.HostName)
			add(c.Name+".guest.ipAddress", v.Guest.IpAddress)
			add(c.Name+".storage", v.Storage)
			add(c.Name+".overallStatus", v.OverallStatus)
		case *types.VirtualMachineConfigInfo:
			if v != nil {
				add(c.Name+".uuid", v.Uuid)
				add(c.Name+".instanceUuid", v.InstanceUuid)
				add(c.Name+".hardware", v.Hardware)
				add(c.Name+".guestId", v.GuestId)
				add(c.Name+".guestFullName", v.GuestFullName)
				add(c.Name+".version", v.Version)
			}
		case *types.GuestInfo:
			if v != nil {
				add(c.Name+".guestState", v.GuestState)
				add(c.Name+".toolsStatus", v.ToolsStatus)
				add(c.Name+".toolsRunningStatus", v.ToolsRunningStatus)
				add(c.Name+".toolsVersionStatus2", v.ToolsVersionStatus2)
				add(c.Name+".ipAddress", v.IpAddress)
				add(c.Name+".hostName", v.HostName)
				add(c.Name+".guestOperationsReady", v.GuestOperationsReady)
			}
		case types.VirtualMachineQuickStats:
			vmQuick(c.Name, v)
		case types.HostListSummaryQuickStats:
			add(c.Name+".overallCpuUsage", v.OverallCpuUsage)
			add(c.Name+".overallMemoryUsage", v.OverallMemoryUsage)
			add(c.Name+".uptime", v.Uptime)
		case types.DatastoreSummary:
			add(c.Name+".capacity", v.Capacity)
			add(c.Name+".freeSpace", v.FreeSpace)
			add(c.Name+".uncommitted", v.Uncommitted)
		case types.BaseDatastoreInfo:
			if v != nil {
				d := v.GetDatastoreInfo()
				add(c.Name+".freeSpace", d.FreeSpace)
				add(c.Name+".timestamp", d.Timestamp)
			}
		case *types.AboutInfo:
			if v != nil {
				add(c.Name+".version", v.Version)
				add(c.Name+".build", v.Build)
			}
		case *types.VirtualMachineSnapshotInfo:
			if v != nil {
				add(c.Name+".currentSnapshot", v.CurrentSnapshot)
				add(c.Name+".rootSnapshotList", v.RootSnapshotList)
			} else {
				add(c.Name+".currentSnapshot", nil)
				add(c.Name+".rootSnapshotList", nil)
				// Leaf clears allocate the optional parent in govmomi. Clear it last.
				add(c.Name, nil)
			}
		}
	}
	// Nil pointers must be untyped nil. ApplyPropertyChange otherwise allocates
	// the target before dereferencing the nil source, which panics in govmomi.
	for i := range out {
		switch v := out[i].Val.(type) {
		case *types.VirtualMachineSnapshotInfo:
			if v == nil {
				out[i].Val = nil
			}
		case *types.ManagedObjectReference:
			if v == nil {
				out[i].Val = nil
			} else {
				out[i].Val = *v
			}
		case *time.Time:
			if v == nil {
				out[i].Val = nil
			} else {
				out[i].Val = *v
			}
		case *int64:
			if v == nil {
				out[i].Val = nil
			} else {
				out[i].Val = *v
			}
		case *bool:
			if v == nil {
				out[i].Val = nil
			} else {
				out[i].Val = *v
			}
		}
		if out[i].Val == nil {
			out[i].Op = types.PropertyChangeOpRemove
		}
	}
	// govmomi indexes interface fields against the base DatastoreInfo type,
	// but applies those indexes to the concrete VsanDatastoreInfo. Publish the
	// base leaves against the base object while locked, retaining the concrete
	// info for readers and parent notifications.
	if ds, ok := obj.(*simulator.Datastore); ok {
		var base []types.PropertyChange
		rest := out[:0]
		for _, c := range out {
			if strings.HasPrefix(c.Name, "info.") {
				base = append(base, c)
			} else {
				rest = append(rest, c)
			}
		}
		if len(base) > 0 && ds.Info != nil {
			original := ds.Info
			ds.Info = original.GetDatastoreInfo()
			// Apply base leaves before the concrete parent, in one registry update.
			// Queuing two updates would make govmomi rescan the inventory twice.
			ctx.Update(obj, append(base, rest...))
			ds.Info = original
		} else {
			ctx.Update(obj, rest)
		}
		return
	}
	ctx.Update(obj, out)
}

// vcsim's creation/removal code sometimes mutates lists directly, or emits
// indexed removals (vm[key]) that its filters do not match to a vm subscriber.
// Publish the complete authoritative membership once the operation has finished.
func (l *Life) publishMembership(ctx *simulator.Context, vm *simulator.VirtualMachine, added bool) {
	refs := append(append([]types.ManagedObjectReference(nil), vm.Datastore...), vm.Network...)
	if vm.Runtime.Host != nil {
		refs = append(refs, *vm.Runtime.Host)
	}
	if vm.ResourcePool != nil {
		refs = append(refs, *vm.ResourcePool)
	}
	if vm.Parent != nil {
		refs = append(refs, *vm.Parent)
	}
	for _, ref := range refs {
		obj := l.reg.Get(ref)
		if obj == nil {
			continue
		}
		ctx.WithLock(obj, func() {
			var changes []types.PropertyChange
			switch o := obj.(type) {
			case *simulator.HostSystem:
				changes = []types.PropertyChange{{Name: "vm", Val: o.Vm}}
			case *simulator.Datastore:
				changes = []types.PropertyChange{{Name: "vm", Val: o.Vm}}
			case *simulator.ResourcePool:
				changes = []types.PropertyChange{{Name: "vm", Val: o.Vm}}
			case *simulator.Folder:
				changes = []types.PropertyChange{{Name: "childEntity", Val: o.ChildEntity}}
			case *simulator.DistributedVirtualPortgroup:
				members := without(o.Vm, vm.Self)
				if added {
					members = append(members, vm.Self)
				}
				changes = []types.PropertyChange{{Name: "vm", Val: members}}
			case *mo.Network:
				members := without(o.Vm, vm.Self)
				if added {
					members = append(members, vm.Self)
				}
				changes = []types.PropertyChange{{Name: "vm", Val: members}}
			}
			if len(changes) > 0 {
				updateLife(ctx, obj, changes)
			}
		})
	}
}
