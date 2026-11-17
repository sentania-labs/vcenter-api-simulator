package sim

import (
	"strings"
	"time"

	"github.com/vmware/govmomi/vim25/types"
)

func quickStatsProperty(prop types.DynamicProperty, n *workloadNode, w workload, now time.Time) types.DynamicProperty {
	uptime := int32(0)
	if n.active && !n.boot.IsZero() {
		uptime = int32(max(0, now.Sub(n.boot)/time.Second))
	}
	vm := func(q types.VirtualMachineQuickStats) types.VirtualMachineQuickStats {
		q.OverallCpuUsage = int32(w.cpu)
		q.OverallCpuDemand = int32(w.demand)
		q.GuestMemoryUsage = int32(w.activeMemory)
		q.HostMemoryUsage = int32(w.memory)
		q.UptimeSeconds = uptime
		return q
	}
	host := func(q types.HostListSummaryQuickStats) types.HostListSummaryQuickStats {
		q.OverallCpuUsage = int32(w.cpu)
		q.OverallMemoryUsage = int32(w.memory)
		q.Uptime = uptime
		return q
	}
	switch v := prop.Val.(type) {
	case types.VirtualMachineSummary:
		v.QuickStats = vm(v.QuickStats)
		prop.Val = v
	case *types.VirtualMachineSummary:
		if v != nil {
			c := *v
			c.QuickStats = vm(c.QuickStats)
			prop.Val = &c
		}
	case types.HostListSummary:
		v.QuickStats = host(v.QuickStats)
		prop.Val = v
	case *types.HostListSummary:
		if v != nil {
			c := *v
			c.QuickStats = host(c.QuickStats)
			prop.Val = &c
		}
	case types.VirtualMachineQuickStats:
		prop.Val = vm(v)
	case *types.VirtualMachineQuickStats:
		if v != nil {
			c := vm(*v)
			prop.Val = &c
		}
	case types.HostListSummaryQuickStats:
		prop.Val = host(v)
	case *types.HostListSummaryQuickStats:
		if v != nil {
			c := host(*v)
			prop.Val = &c
		}
	default:
		if strings.HasPrefix(prop.Name, "summary.quickStats.") {
			switch strings.TrimPrefix(prop.Name, "summary.quickStats.") {
			case "overallCpuUsage":
				prop.Val = int32(w.cpu)
			case "overallCpuDemand":
				prop.Val = int32(w.demand)
			case "guestMemoryUsage":
				prop.Val = int32(w.activeMemory)
			case "hostMemoryUsage", "overallMemoryUsage":
				prop.Val = int32(w.memory)
			case "uptime", "uptimeSeconds":
				prop.Val = uptime
			}
		}
	}
	return prop
}
