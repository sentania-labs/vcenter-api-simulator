package sim

import (
	"strings"

	"github.com/vmware/govmomi/vim25/types"
)

func counterName(c types.PerfCounterInfo) string {
	return c.GroupInfo.GetElementDescription().Key + "." + c.NameInfo.GetElementDescription().Key
}

// valueAt returns the native unit advertised in perfCounter. Summation values
// describe one 20-second sampling period; the query layer sums them on rollup.
// Unsupported counters are omitted, never manufactured as zero samples.
func valueAt(c types.PerfCounterInfo, n *workloadNode, w workload) (int64, bool) {
	name := counterName(c)
	pct := func(v, cap int64) int64 {
		if cap <= 0 {
			return 0
		}
		return min(10000, v*10000/cap)
	}
	latency := int64(0)
	if w.ios > 0 {
		latency = w.latency / w.ios
	}
	switch name {
	case "cpu.usage":
		return pct(w.cpu, w.cpuCap), true
	case "cpu.usagemhz", "cpu.capacity.usage":
		return w.cpu, true
	case "cpu.demand", "cpu.capacity.demand":
		return w.demand, true
	case "cpu.totalmhz", "cpu.capacity.provisioned", "cpu.capacity.entitlement", "cpu.cpuentitlement", "clusterServices.effectivecpu":
		return w.cpuCap, true
	case "cpu.corecount.provisioned":
		return int64(n.cpus), true
	case "cpu.corecount.usage":
		if w.cpuCap > 0 {
			return w.cpu * int64(n.cpus) / w.cpuCap, true
		}
		return 0, true
	case "cpu.capacity.contention", "cpu.corecount.contention":
		return pct(w.ready, 20000*w.vcpus), true
	case "cpu.ready":
		return w.ready, true
	case "cpu.wait":
		return w.wait, true
	case "cpu.idle":
		return w.wait * 95 / 100, true
	case "cpu.used":
		return w.usedCPU, true
	case "cpu.system":
		return w.usedCPU / 20, true
	case "mem.usage":
		return pct(w.memory, w.memCap), true
	case "mem.active":
		return w.activeMemory * 1024, true
	case "mem.consumed", "mem.granted", "mem.capacity.usage", "mem.capacity.usage.vm", "mem.consumed.vms":
		return w.memory * 1024, true
	case "mem.capacity.provisioned", "mem.capacity.entitlement", "mem.capacity.usable":
		return w.memCap * 1024, true
	case "mem.totalmb", "mem.mementitlement", "clusterServices.effectivemem":
		return w.memCap, true
	case "mem.shared":
		return w.memory * 1024 / 30, true
	case "mem.zero":
		return max(0, w.memCap-w.memory) * 1024 / 10, true
	case "mem.overhead", "mem.capacity.usage.vmOvrhd":
		return w.memory * 1024 / 40, true
	case "mem.swapused", "mem.swapped", "mem.swaptarget", "mem.swapinRate", "mem.swapoutRate", "mem.vmmemctl", "mem.compressed", "mem.compressionRate", "mem.decompressionRate", "mem.capacity.contention", "mem.state":
		return 0, true
	case "disk.used", "disk.provisioned", "disk.unshared", "disk.capacity.provisioned", "disk.capacity.usage":
		return n.used, true
	case "disk.capacity":
		return n.capacity, true
	case "disk.capacity.contention":
		return pct(n.used, n.capacity), true
	case "disk.read", "virtualDisk.read", "datastore.read":
		return w.read, true
	case "disk.write", "virtualDisk.write", "datastore.write":
		return w.write, true
	case "disk.usage", "disk.throughput.usage", "virtualDisk.throughput.usage", "datastore.throughput.usage":
		return w.read + w.write, true
	case "disk.numberRead", "disk.numberWrite":
		if strings.Contains(name, "Read") {
			return w.read * 20 / 32, true
		}
		return w.write * 20 / 32, true
	case "disk.numberReadAveraged", "virtualDisk.numberReadAveraged", "datastore.numberReadAveraged":
		return w.read / 32, true
	case "disk.numberWriteAveraged", "virtualDisk.numberWriteAveraged", "datastore.numberWriteAveraged":
		return w.write / 32, true
	case "datastore.datastoreIops":
		return (w.read + w.write) / 32, true
	case "disk.totalLatency", "disk.maxTotalLatency", "disk.throughput.contention", "virtualDisk.totalReadLatency", "virtualDisk.totalWriteLatency", "virtualDisk.throughput.cont", "datastore.totalReadLatency", "datastore.totalWriteLatency", "datastore.maxTotalLatency", "datastore.throughput.contention":
		return latency / 1000, true
	case "datastore.sizeNormalizedDatastoreLatency", "datastore.datastoreVMObservedLatency":
		return latency, true
	case "disk.commandsAborted", "disk.busResets", "virtualDisk.commandsAborted", "virtualDisk.busResets", "datastore.commandsAborted", "datastore.busResets":
		return 0, true
	case "net.received":
		return w.rx, true
	case "net.transmitted":
		return w.tx, true
	case "net.usage", "net.throughput.usage", "net.throughput.usage.vm":
		return w.rx + w.tx, true
	case "net.packetsRx":
		return w.rx * 1024 * 20 / 1500, true
	case "net.packetsTx":
		return w.tx * 1024 * 20 / 1500, true
	case "net.throughput.packetsPerSec":
		return (w.rx + w.tx) * 1024 / 1500, true
	case "net.droppedRx", "net.droppedTx", "net.errorsRx", "net.errorsTx", "net.throughput.contention":
		return 0, true
	}
	if strings.HasPrefix(name, "vmop.") {
		return 0, true
	} // No Activity operations are invented.
	if !strings.HasPrefix(name, "guest.") {
		return 0, false
	}
	switch strings.TrimPrefix(name, "guest.") {
	case "mem.total", "mem.physUsable":
		return w.memCap * 1024, true
	case "mem.free", "mem.available", "mem.availableToMm":
		return max(0, w.memCap-w.memory) * 1024, true
	case "mem.needed", "mem.neededReservation":
		return w.memory * 1024, true
	case "mem.cached", "mem.activeFileCache":
		return (w.memory - w.activeMemory) * 1024 / 2, true
	case "mem.buffers", "mem.slabReclaim", "mem.standby.normal", "mem.standby.reserve", "mem.standby.core", "mem.modifiedPages":
		return w.memory * 1024 / 50, true
	case "page.size":
		return 4, true
	case "hugePage.size":
		return 2048, true
	case "hugePage.total":
		return w.memory / 4, true
	case "swap.spaceRemaining":
		return w.memCap * 1024, true
	case "cpu.runQueue", "processor.queue":
		return max(0, w.ready*w.vcpus/20000), true
	case "disk.requestQueue", "disk.requestQueueAvg", "disk.queue", "disk.queueAvg":
		return w.ios * latency / 1000000, true
	case "page.inRate":
		return w.read / 4, true
	case "page.outRate":
		return w.write / 4, true
	case "contextSwapRate":
		return w.cpu * 2, true
	}
	return 0, false
}

func supportsCounter(c types.PerfCounterInfo, n *workloadNode) bool {
	group := c.GroupInfo.GetElementDescription().Key
	switch counterName(c) {
	case "disk.used", "disk.provisioned", "disk.unshared", "disk.capacity", "disk.capacity.provisioned", "disk.capacity.usage", "disk.capacity.contention":
		if n.ref.Type != "Datastore" {
			return false
		}
	}
	switch n.ref.Type {
	case "Datastore":
		if group != "disk" && group != "datastore" {
			return false
		}
	case "Datacenter", "ClusterComputeResource", "ResourcePool":
		if group == "guest" || group == "virtualDisk" {
			return false
		}
	case "HostSystem":
		if group == "guest" || group == "virtualDisk" || group == "vmop" {
			return false
		}
	case "VirtualMachine":
		if group == "vmop" || group == "clusterServices" {
			return false
		}
	default:
		return false
	}
	_, ok := valueAt(c, n, workload{})
	return ok
}
