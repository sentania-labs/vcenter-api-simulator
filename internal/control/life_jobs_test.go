package control

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/life"
)

func TestActivityCalendarMonth(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 1, 17, 59, 0, 0, time.Local)}
	c := newCtlAt(t, lively(t), "", "jobs-month", clk.now)
	for b := range c.life.Settings.Global {
		if !life.IsLoad(b) {
			c.life.Settings.Global[b] = life.Dial{}
		}
	}
	c.life.Settings.Global[life.Patching] = life.Dial{Level: 1}
	c.life.Settings.Global[life.Maintenance] = life.Dial{Level: 1}
	c.life.Start(clk.now(), c.applied().sim.Life())
	play(c, clk, 31*24*time.Hour)
	posted := c.applied().sim.Life().Posted()
	// Two clusters, eight hosts each: both patched initially; the first is
	// due again 30 days later. The second is due just beyond this interval.
	if got := posted["task HostPatchManager.installV2"]; got != 24 {
		t.Fatalf("month patched %d hosts, want 24", got)
	}
	t.Logf("31 simulated days: %d host patches, %d reboots", posted["task HostPatchManager.installV2"], posted["task HostSystem.reboot"])
}

func TestActivityJobControls(t *testing.T) {
	clk := &clock{t: monday9}
	c := newCtlAt(t, lively(t), "", "job-controls", clk.now)
	v, _ := c.Activity("")
	for _, r := range v.Rows {
		if r.Key == life.Crash && r.Level != 0 {
			t.Fatal("crash defaults on")
		}
	}
	cluster := c.applied().topo.Clusters()[0].Key
	long := 90.0
	if _, err := c.ActivityDial(ActDial{Cluster: cluster, Behaviour: life.Patching, N: &long, Unit: life.UnitDays}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ActivityDial(ActDial{Cluster: cluster, Behaviour: life.Power, N: &long, Unit: life.UnitDays}); err == nil {
		t.Fatal("unrelated power interval limit changed")
	}
	n := 2.0
	recovery := 3
	if _, err := c.ActivityDial(ActDial{Cluster: cluster, Behaviour: life.Crash, N: &n, Unit: life.UnitDays, RecoveryMinutes: &recovery}); err != nil {
		t.Fatal(err)
	}
	d, own := c.life.Settings.Dial(cluster, life.Crash)
	if !own || life.Recovery(d) != 3 || d.Exact.N != 2 {
		t.Fatal("cluster crash controls not retained")
	}
	c.life.Start(clk.now(), c.applied().sim.Life())
	for _, b := range []string{life.Patching, life.Backups, life.Crash} {
		v, _ = c.Activity(cluster)
		found := false
		for _, r := range v.Rows {
			if r.Key == b {
				found = r.DoNow
			}
		}
		if !found {
			t.Fatalf("%s has no Do it now", b)
		}
		ask, err := c.ActivityDoNow(cluster, b, false)
		if err != nil || ask.Confirm == nil {
			t.Fatalf("%s confirmation: %v", b, err)
		}
	}
}

// Opt-in resource proof: 215 hosts and 5,009 VMs, a full backup night and
// a rolling patch of the largest cluster, plus a crash and its recovery.
func TestActivityJobsLarge(t *testing.T) {
	if os.Getenv("VCSIM_JOBS_LARGE") == "" {
		t.Skip("set VCSIM_JOBS_LARGE=1 for resource proof")
	}
	clk := &clock{t: time.Date(2026, 11, 17, 21, 59, 0, 0, time.Local)}
	c := newCtlAt(t, largeSize(t), "", "large-jobs", clk.now)
	for b := range c.life.Settings.Global {
		if !life.IsLoad(b) {
			c.life.Settings.Global[b] = life.Dial{}
		}
	}
	c.life.Settings.Global[life.Backups] = life.Dial{Level: 1}
	c.life.Start(clk.now(), c.applied().sim.Life())
	began := time.Now()
	play(c, clk, 6*time.Hour)
	posted := c.applied().sim.Life().Posted()
	if posted["task VirtualMachine.createSnapshot"] != 5009 || posted["task VirtualMachine.removeSnapshot"] != 5009 {
		t.Fatalf("night: snapshots taken %d, removed %d", posted["task VirtualMachine.createSnapshot"], posted["task VirtualMachine.removeSnapshot"])
	}
	c.life.Settings.Global[life.Backups] = life.Dial{}
	cluster := c.applied().topo.Clusters()[6].Key
	if !c.life.DoNow(clk.now(), c.applied().sim.Life(), cluster, life.Patching) {
		t.Fatal("patch did not start")
	}
	play(c, clk, 31*time.Hour)
	if c.life.State.Jobs.Patch != nil {
		t.Fatal("large cluster patch incomplete")
	}
	if !c.life.DoNow(clk.now(), c.applied().sim.Life(), cluster, life.Crash) {
		t.Fatal("crash did not start")
	}
	play(c, clk, 16*time.Minute)
	if len(c.life.State.Jobs.Crashes) != 0 {
		t.Fatal("crash did not recover")
	}
	posted = c.applied().sim.Life().Posted()
	if posted["task HostPatchManager.installV2"] != 61 {
		t.Fatalf("patched %d hosts", posted["task HostPatchManager.installV2"])
	}
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t.Logf("215 hosts, 5009 VMs: elapsed %s, heap in use %d MiB, system %d MiB; %d snapshots taken and removed, %d hosts patched, HA restarts %d", time.Since(began), m.HeapInuse>>20, m.Sys>>20, posted["task VirtualMachine.createSnapshot"], posted["task HostPatchManager.installV2"], posted["VmRestartedByHAEvent"])
}
