package sim

import (
	"strings"

	"github.com/vmware/govmomi/vim25/types"
)

// Host and VM power. vcsim's vCenter counter catalog carries six of the nine
// power counters a vCenter 9 serves (power.power, power.powerCap,
// power.energy and power.capacity.usable, usage and usagePct, at their real
// levels); powerCounters adds the three it lacks. Names, units, rollups and
// levels follow the vSphere 9.0 API reference (power counters).
//
// The model is a 2-socket server: an idle floor, a little hypervisor and
// vSAN work, and the VMs, each with a static share per running vCPU plus a
// share that follows the CPU it uses. Host idle + system + VM equals the
// host's usage, and the VMs' power adds up to the host's VM power until the
// host reaches its usable power: it cannot draw more, so the VM share is cut.
const (
	powerIdleWatts        = 150 // a powered-on host with nothing running
	powerSystemWatts      = 12  // hypervisor and vSAN work besides the VMs
	powerVCPUWatts        = 2   // static share of each running vCPU
	powerMilliWattsPerMHz = 8   // 8 W per GHz of CPU used
	powerUsableWatts      = 750 // floor of what the power supplies allow; no cap is enforced
	// A host in maintenance mode is on and runs no VMs. Its idle floor is
	// lower than a serving host's, as power management settles an empty host.
	powerMaintenanceIdleWatts = 100
)

// powerUnavailable is true for a host that is not connected or not powered on
// (a crashed host). It has no power or energy samples; query answers -1, the
// value vSphere uses for a sample it does not have, rather than 0 W.
func powerUnavailable(name string, n *workloadNode) bool {
	return strings.HasPrefix(name, "power.") && n.ref.Type == "HostSystem" && n.unreachable
}

// powerUsable is what the host's supplies allow: the floor, or more for a
// large host (every thread at full draw, plus the idle floor and system work).
// It depends on the host's size only, so it does not move with load.
func powerUsable(n *workloadNode) int64 {
	size := powerIdleWatts + powerSystemWatts + n.cpu*powerMilliWattsPerMHz/1000 + powerVCPUWatts*int64(n.cpuInstances)
	return max(powerUsableWatts, roundUp(size, 50))
}

func roundUp(v, step int64) int64 { return (v + step - 1) / step * step }

// powerCounters returns the power counters the catalog lacks, keyed after
// its highest key so that no key is shared and every start gives the same.
func powerCounters(catalog []types.PerfCounterInfo) []types.PerfCounterInfo {
	desc := func(key, label, summary string) *types.ElementDescription {
		return &types.ElementDescription{Key: key, Description: types.Description{Label: label, Summary: summary}}
	}
	watt := desc("watt", "W", "Watt")
	specs := []struct {
		name, label, summary string
		level, perDevice     int32
	}{
		{"capacity.usageIdle", "Host Power Capacity Usage - Idle", "Power usage due to host idleness.", 2, 3},
		{"capacity.usageSystem", "Host Power Capacity Usage - System", "Power usage due to non-VM activities.", 2, 3},
		{"capacity.usageVm", "Host Power Capacity Usage - VM", "Power usage due to VM workloads.", 2, 3},
	}
	have := map[string]bool{}
	next := int32(0)
	for _, c := range catalog {
		have[counterName(c)] = true
		next = max(next, c.Key)
	}
	var out []types.PerfCounterInfo
	for _, s := range specs {
		if have["power."+s.name] {
			continue
		}
		next++
		out = append(out, types.PerfCounterInfo{
			Key:            next,
			NameInfo:       desc(s.name, s.label, s.summary),
			GroupInfo:      desc("power", "Power", "Power"),
			UnitInfo:       watt,
			RollupType:     types.PerfSummaryTypeAverage,
			StatsType:      types.PerfStatsTypeAbsolute,
			Level:          s.level,
			PerDeviceLevel: s.perDevice,
		})
	}
	return out
}

// powerValue answers the power counters for hosts and VMs. Clusters,
// resource pools, datacenters and datastores have none, as on a real vCenter.
func powerValue(name string, n *workloadNode, w workload) (int64, bool) {
	vm := powerVCPUWatts*w.vcpus + w.cpu*powerMilliWattsPerMHz/1000
	switch n.ref.Type {
	case "VirtualMachine":
		switch name {
		case "power.power":
			return vm, true
		case "power.energy":
			return vm * 20, true // joules in one 20-second sample
		}
		return 0, false
	case "HostSystem":
	default:
		return 0, false
	}
	idle := int64(powerIdleWatts)
	if n.maintenance {
		idle = powerMaintenanceIdleWatts
	}
	usable := powerUsable(n)
	system := powerSystemWatts + vm/20
	hostVM := vm
	if idle+system+hostVM > usable {
		hostVM = (usable - idle - powerSystemWatts) * 20 / 21
		system = usable - idle - hostVM
	}
	usage := idle + system + hostVM
	switch name {
	case "power.power", "power.capacity.usage":
		return usage, true
	case "power.energy":
		return usage * 20, true
	case "power.powerCap":
		return 0, true // 0 W: no power cap is enforced
	case "power.capacity.usable":
		return usable, true
	case "power.capacity.usagePct":
		return min(10000, usage*10000/usable), true // hundredths of a percent
	case "power.capacity.usageIdle":
		return idle, true
	case "power.capacity.usageSystem":
		return system, true
	case "power.capacity.usageVm":
		return hostVM, true
	}
	return 0, false
}
