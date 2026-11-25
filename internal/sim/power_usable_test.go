package sim

import "testing"

func TestPowerUsableScalesWithHostSizeAndCapsUsage(t *testing.T) {
	small := &workloadNode{ref: perfRef{Type: "HostSystem", Value: "host-1"}, cpu: 16 * 2495, cpus: 16, cpuInstances: 32}
	if got := powerUsable(small); got != 750 {
		t.Fatalf("default host usable %d W, want the 750 W floor", got)
	}
	// 128 cores at 3 GHz, 256 threads: 150 + 12 + 3072 + 512 = 3746 W, in steps of 50
	big := &workloadNode{ref: perfRef{Type: "HostSystem", Value: "host-2"}, cpu: 128 * 3000, cpus: 128, cpuInstances: 256}
	if got := powerUsable(big); got != 3750 {
		t.Fatalf("large host usable %d W, want 3750 W", got)
	}

	// 200 vCPUs running 39,920 MHz: VMs would draw 400 + 319 = 719 W, system 47 W,
	// idle 150 W, so 916 W against 750 W usable. The host cannot draw more.
	busy := workload{vcpus: 200, cpu: 16 * 2495}
	for _, tc := range []struct {
		counter string
		want    int64
	}{
		{"power.capacity.usable", 750},
		{"power.capacity.usage", 750},
		{"power.power", 750},
		{"power.capacity.usagePct", 10000},
		{"power.capacity.usageIdle", 150},
	} {
		if got, _ := powerValue(tc.counter, small, busy); got != tc.want {
			t.Errorf("%s = %d, want %d", tc.counter, got, tc.want)
		}
	}
	idle, _ := powerValue("power.capacity.usageIdle", small, busy)
	system, _ := powerValue("power.capacity.usageSystem", small, busy)
	vm, _ := powerValue("power.capacity.usageVm", small, busy)
	if vm <= 0 || idle+system+vm != 750 {
		t.Fatalf("idle %d + system %d + vm %d W, want a positive vm share summing to 750 W", idle, system, vm)
	}

	// the same host with one VM: under the cap, usable stays at 750 W, usage follows load
	quiet := workload{vcpus: 4, cpu: 2000}
	if got, _ := powerValue("power.capacity.usable", small, quiet); got != 750 {
		t.Errorf("quiet host usable %d W, want 750 W", got)
	}
	if got, _ := powerValue("power.capacity.usage", small, quiet); got != 187 {
		t.Errorf("quiet host usage %d W, want 187 W (150 idle + 12 system + 25 VM)", got)
	}
}
