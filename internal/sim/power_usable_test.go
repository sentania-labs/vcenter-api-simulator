package sim

import "testing"

func TestPowerUsableScalesWithHostSizeAndNeverTrailsUsage(t *testing.T) {
	small := &workloadNode{ref: perfRef{Type: "HostSystem", Value: "host-1"}, cpu: 16 * 2495, cpus: 16, cpuInstances: 32}
	if got := powerUsable(small, 200); got != 750 {
		t.Fatalf("default host usable %d W, want the 750 W floor", got)
	}
	big := &workloadNode{ref: perfRef{Type: "HostSystem", Value: "host-2"}, cpu: 128 * 3000, cpus: 128, cpuInstances: 256}
	if got := powerUsable(big, 200); got <= 750 {
		t.Fatalf("large host usable %d W, want above the floor", got)
	}
	// a host full of VMs: usage above even the size estimate still fits
	w := workload{vcpus: 600, cpu: 16 * 2495}
	usage, _ := powerValue("power.capacity.usage", small, w)
	usable, _ := powerValue("power.capacity.usable", small, w)
	pct, _ := powerValue("power.capacity.usagePct", small, w)
	if usage <= 750 || usable < usage || pct != usage*10000/usable {
		t.Fatalf("usage %d W, usable %d W, %d%%/100", usage, usable, pct)
	}
}
