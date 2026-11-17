package sim

import "github.com/vmware/govmomi/vim25/types"

// Names and units were checked with QueryPerfCounterInt on vCenter 9.1.1.
// IDs are simulator-owned and stable, not tied to another vCenter's IDs.
func guestCounters() []types.PerfCounterInfo {
	names := []string{"hugePage.size", "mem.physUsable", "mem.free", "mem.activeFileCache", "swap.spaceRemaining", "hugePage.total", "page.inRate", "page.outRate", "contextSwapRate", "page.size", "mem.needed", "mem.neededReservation", "mem.available", "mem.slabReclaim", "mem.buffers", "mem.cached", "mem.total", "cpu.runQueue", "disk.requestQueue", "disk.requestQueueAvg", "processor.queue", "mem.availableToMm", "mem.standby.normal", "mem.standby.reserve", "mem.standby.core", "mem.modifiedPages", "disk.queue", "disk.queueAvg"}
	out := make([]types.PerfCounterInfo, 0, len(names))
	desc := func(key string) *types.ElementDescription {
		return &types.ElementDescription{Key: key, Description: types.Description{Label: key, Summary: key}}
	}
	for i, name := range names {
		unit := "kiloBytes"
		switch name {
		case "hugePage.total", "page.inRate", "page.outRate", "contextSwapRate", "cpu.runQueue", "disk.requestQueue", "disk.requestQueueAvg", "processor.queue", "disk.queue", "disk.queueAvg":
			unit = "number"
		}
		out = append(out, types.PerfCounterInfo{Key: int32(20001 + i), NameInfo: desc(name), GroupInfo: desc("guest"), UnitInfo: desc(unit), RollupType: types.PerfSummaryTypeLatest, StatsType: types.PerfStatsTypeAbsolute, Level: 1, PerDeviceLevel: 1})
	}
	return out
}
