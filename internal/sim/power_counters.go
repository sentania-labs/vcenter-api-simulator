package sim

import "github.com/vmware/govmomi/vim25/types"

// Host and VM power. vcsim's vCenter counter catalog carries six of the nine
// power counters a vCenter 9 serves (power.power, power.powerCap,
// power.energy and power.capacity.usable, usage and usagePct, at their real
// levels); powerCounters adds the three it lacks. Names, units, rollups and
// levels follow the vSphere 9.0 API reference (power counters).
//
// The model is a 2-socket server: an idle floor, a little hypervisor and
// vSAN work, and the VMs, each with a static share per running vCPU plus a
// share that follows the CPU it uses. Host idle + system + VM equals the
// host's usage, and the VMs' power adds up to the host's VM power.
const (
	powerIdleWatts        = 150 // a powered-on host with nothing running
	powerSystemWatts      = 12  // hypervisor and vSAN work besides the VMs
	powerVCPUWatts        = 2   // static share of each running vCPU
	powerMilliWattsPerMHz = 8   // 8 W per GHz of CPU used
	powerUsableWatts      = 750 // what the power supplies allow; no cap is enforced
)

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
	system := powerSystemWatts + vm/20
	usage := powerIdleWatts + system + vm
	switch name {
	case "power.power", "power.capacity.usage":
		return usage, true
	case "power.energy":
		return usage * 20, true
	case "power.powerCap":
		return 0, true // 0 W: no power cap is enforced
	case "power.capacity.usable":
		return powerUsableWatts, true
	case "power.capacity.usagePct":
		return min(10000, usage*10000/powerUsableWatts), true // hundredths of a percent
	case "power.capacity.usageIdle":
		return powerIdleWatts, true
	case "power.capacity.usageSystem":
		return system, true
	case "power.capacity.usageVm":
		return vm, true
	}
	return 0, false
}
