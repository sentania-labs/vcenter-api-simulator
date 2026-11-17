package control

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// labSize is the lab's everyday environment: four clusters of eight hosts
// and 200 VMs.
func labSize(t *testing.T) *scenario.Scenario {
	t.Helper()
	s := scenario.Phase1("lab")
	s.Auth.Users = []scenario.User{{Name: "administrator@vsphere.local", Password: "first-password"}}
	s.Datacenters[0].Clusters = 4
	s.Defaults.Hosts = 8
	s.Defaults.VMs.PerHost = scenario.IntPtr(25)
	s.Vary, s.Clusters = scenario.Vary{}, nil
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	return s
}

// largeSize is the large lab topology of docs/sizing.md: four clusters of 8
// hosts and 200 VMs and three of 61 hosts and 1,403 VMs, 215 hosts and
// 5,009 VMs in all, with 12 claimed and one local device on every host.
func largeSize(t *testing.T) *scenario.Scenario {
	t.Helper()
	s := labSize(t)
	s.Datacenters[0].Clusters = 7
	s.Defaults.Host.ClaimedDevices = 12
	s.Defaults.Host.LocalDevices = scenario.IntPtr(1)
	for n := 5; n <= 7; n++ {
		s.Clusters = append(s.Clusters, scenario.ClusterOverride{Key: scenario.ClusterKey("dc-01", n), ClusterSpec: scenario.ClusterSpec{Hosts: 61, VMs: scenario.VMSpec{PerHost: scenario.IntPtr(23)}}})
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	return s
}

// aDay starts activity at midnight on a working day with every dial at
// its baseline, plays 24 hours and reports what happened.
func aDay(t *testing.T, sc *scenario.Scenario) (lines map[string]int, counts map[string]int, posted map[string]int, took time.Duration, c *Controller, clk *clock) {
	t.Helper()
	clk = &clock{t: time.Date(2026, 11, 16, 0, 0, 0, 0, time.Local)}
	c = newCtlAt(t, sc, "", "h", clk.now)
	if _, err := c.ActivityStart(true); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	play(c, clk, 24*time.Hour)
	took = time.Since(start)
	lines, counts = map[string]int{}, map[string]int{}
	for _, en := range c.life.Log {
		lines[en.Kind]++
		for k, v := range en.Counts {
			counts[k] += v
		}
	}
	return lines, counts, c.applied().sim.Life().Posted(), took, c, clk
}

func table(m map[string]int) string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "    %-34s %6d\n", k, m[k])
	}
	return b.String()
}

// A working day at the lab's size, every dial at its baseline: about 200
// vMotions by load balancing, 16 VMs powered off and on, 8 in and 8 out,
// 2 hosts into maintenance mode with their 25 VMs each, and every one of
// them recorded in vCenter as the events and the task of what it was.
func TestActivityADayAtLabSize(t *testing.T) {
	lines, counts, posted, took, c, _ := aDay(t, labSize(t))
	t.Logf("a working day at 32 hosts and 800 VMs, played in %s\n  lines in \"What has happened\", by kind:\n%s  what they add up to:\n%s  events and tasks recorded in vCenter:\n%s",
		took.Round(time.Millisecond), table(lines), table(counts), table(posted))
	near := func(what string, got int, want, slack float64) {
		t.Helper()
		if float64(got) < want*(1-slack) || float64(got) > want*(1+slack) {
			t.Errorf("%s: %d, want about %.0f", what, got, want)
		}
	}
	near("vMotions (200 by load balancing and 50 off the two hosts)", counts["move"], 250, 0.2)
	near("VMs powered off", counts["off"], 16, 0.4)
	near("VMs added", counts["add"], 8, 0.5)
	near("VMs removed", counts["remove"], 8, 0.5)
	if counts["min"] != 2 {
		t.Errorf("%d hosts went into maintenance mode, want 2 (5%% of 32)", counts["min"])
	}
	// every vMotion is two events and one task; every host into maintenance mode two events and one task
	moves := counts["move"]
	if posted["VmBeingHotMigratedEvent"]+posted["VmBeingMigratedEvent"] != moves || posted["DrsVmMigratedEvent"] != moves || posted["task Drm.ExecuteVMotionLRO"] != moves {
		t.Errorf("%d vMotions but %d start events, %d DRS events and %d tasks", moves, posted["VmBeingHotMigratedEvent"]+posted["VmBeingMigratedEvent"], posted["DrsVmMigratedEvent"], posted["task Drm.ExecuteVMotionLRO"])
	}
	if posted["EnteringMaintenanceModeEvent"] != 2 || posted["EnteredMaintenanceModeEvent"] != 2 || posted["task HostSystem.enterMaintenanceMode"] != 2 {
		t.Errorf("maintenance mode events and tasks: %v", posted)
	}
	// removing a VM powers it off first, so power-offs are the day's own and the removed VMs'
	if posted["VmPoweredOffEvent"] != counts["off"]+counts["remove"] || posted["VmPoweredOnEvent"] != counts["on"]+counts["add"] {
		t.Errorf("power events %d off and %d on; lines say %d off, %d removed, %d on, %d added", posted["VmPoweredOffEvent"], posted["VmPoweredOnEvent"], counts["off"], counts["remove"], counts["on"], counts["add"])
	}
	if v, _ := c.Activity(""); v.VMs != 800+counts["add"]-counts["remove"] {
		t.Errorf("the vCenter has %d VMs after %d were added and %d removed", v.VMs, counts["add"], counts["remove"])
	}
}

// The same day at the large lab topology. It builds 5,009 VMs, so it runs
// only when asked for: VCSIM_LARGE=1 go test ./internal/control -run Large -v
// A larger number plays that many days and reports the memory in use after
// each, to show it does not grow with time.
func TestActivityADayAtTheLargeSize(t *testing.T) {
	if os.Getenv("VCSIM_LARGE") == "" {
		t.Skip("set VCSIM_LARGE=1 to play a day at 215 hosts and 5,009 VMs")
	}
	lines, counts, posted, took, c, clk := aDay(t, largeSize(t))
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	v, _ := c.Activity("")
	t.Logf("a working day at %d hosts and %d VMs, played in %s (%.1f ms for each minute); heap in use afterwards %d MiB\n  lines in \"What has happened\", by kind:\n%s  what they add up to:\n%s  events and tasks recorded in vCenter:\n%s",
		v.Hosts, v.VMs, took.Round(time.Millisecond), float64(took.Milliseconds())/1440, m.HeapInuse>>20, table(lines), table(counts), table(posted))
	if v.Hosts != 215 || counts["move"] < 1200 || counts["min"] != 11 {
		t.Errorf("%d hosts, %d vMotions, %d hosts into maintenance mode: want 215 hosts, about 1,250 vMotions by load balancing plus the evacuations, and 11 hosts (5%% of 215)", v.Hosts, counts["move"], counts["min"])
	}
	days, _ := strconv.Atoi(os.Getenv("VCSIM_LARGE"))
	for d := 2; d <= days; d++ {
		play(c, clk, 24*time.Hour)
		runtime.GC()
		runtime.ReadMemStats(&m)
		v, _ = c.Activity("")
		t.Logf("after day %d: %d VMs, %d lines remembered, heap in use %d MiB", d, v.VMs, len(c.life.Log), m.HeapInuse>>20)
	}
}
