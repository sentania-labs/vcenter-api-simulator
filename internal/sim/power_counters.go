package sim

import "github.com/vmware/govmomi/vim25/types"

// Host and VM power. govmomi's catalog carries power.power, power.powerCap
// and power.energy (with level 0); vCenter 8 and 9 also serve the six
// power.capacity counters below. Names, units, rollups and levels follow the
// vSphere 9.0 API reference (power counters).
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

// powerLevels sets the collection levels of the counters govmomi ships at
// level 0, which QueryPerfCounterByLevel would otherwise never return.
var powerLevels = map[string][2]int32{
	"power.power":    {2, 3},
	"power.energy":   {3, 3},
	"power.powerCap": {3, 3},
}

func powerCounters() []types.PerfCounterInfo {
	desc := func(key, label, summary string) *types.ElementDescription {
		return &types.ElementDescription{Key: key, Description: types.Description{Label: label, Summary: summary}}
	}
	watt := desc("watt", "W", "Watt")
	percent := desc("percent", "%", "Percentage")
	specs := []struct {
		name, label, summary string
		unit                 *types.ElementDescription
		level, perDevice     int32
	}{
		{"capacity.usable", "Host Power Capacity Usable", "Current maximum allowed power usage.", watt, 4, 4},
		{"capacity.usage", "Host Power Capacity Usage", "Current power usage.", watt, 4, 4},
		{"capacity.usagePct", "Host Power Capacity Provisioned", "Current power usage as a percentage of maximum allowed power.", percent, 4, 4},
		{"capacity.usageIdle", "Host Power Capacity Usage - Idle", "Power usage due to host idleness.", watt, 2, 3},
		{"capacity.usageSystem", "Host Power Capacity Usage - System", "Power usage due to non-VM activities.", watt, 2, 3},
		{"capacity.usageVm", "Host Power Capacity Usage - VM", "Power usage due to VM workloads.", watt, 2, 3},
	}
	out := make([]types.PerfCounterInfo, 0, len(specs))
	for i, s := range specs {
		out = append(out, types.PerfCounterInfo{
			// The keys after govmomi's power counters (720896-720898) are free.
			Key:            int32(720899 + i),
			NameInfo:       desc(s.name, s.label, s.summary),
			GroupInfo:      desc("power", "Power", "Power"),
			UnitInfo:       s.unit,
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
