package control

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/life"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// lively is two clusters of eight hosts and forty VMs each.
func lively(t *testing.T) *scenario.Scenario {
	t.Helper()
	s := scenario.Phase1("lively")
	s.Auth.Users = []scenario.User{{Name: "administrator@vsphere.local", Password: "first-password"}}
	s.Datacenters[0].Clusters = 2
	s.Defaults.Hosts = 8
	s.Defaults.VMs.PerHost = scenario.IntPtr(5)
	s.Vary, s.Clusters = scenario.Vary{}, nil
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	return s
}

// monday9 is 9 AM on a working day in the machine's own time zone, which
// is the clock activity keeps.
var monday9 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.Local)

func play(c *Controller, clk *clock, d time.Duration) {
	for end := clk.now().Add(d); clk.now().Before(end); {
		clk.add(time.Minute)
		c.lifeStep()
	}
}

func startActivity(t *testing.T, c *Controller) {
	t.Helper()
	ask, err := c.ActivityStart(false)
	if err != nil || ask.Confirm == nil || !strings.Contains(strings.Join(ask.Confirm.Body, " "), "into maintenance mode straight away") {
		t.Fatalf("starting must first say what will happen, hosts included: %+v, %v", ask, err)
	}
	if done, err := c.ActivityStart(true); err != nil || !strings.Contains(done.Done, "Started") {
		t.Fatalf("start: %+v, %v", done, err)
	}
}

func placements(c *Controller) map[string]string {
	out := map[string]string{}
	w := c.applied().sim.Life()
	snap := w.Snapshot()
	for k, v := range snap.Hosts {
		out[k] = v
	}
	for _, k := range snap.Off {
		out["off:"+k] = "off"
	}
	for _, k := range snap.Maintenance {
		out["maintenance:"+k] = "in"
	}
	return out
}

func TestActivityRunsADayAndComesBackAfterARestart(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: monday9}
	c := newCtlAt(t, lively(t), dir, "h", clk.now)
	if v, _ := c.Activity(""); v.SetUp || v.Running || len(v.Rows) != len(life.Order) || len(v.Rows[0].Levels) != 6 {
		t.Fatalf("before anything is set up: %+v", v)
	}
	if h := c.Home().Activity; h.State != "none" {
		t.Errorf("Home says activity is %q before it is set up", h.State)
	}
	startActivity(t, c)
	// busier than the baseline, so that a day of 80 VMs has something of everything
	for _, b := range []string{life.Moves, life.Power, life.Adds, life.Removes} {
		lv := 4.0
		if _, err := c.ActivityDial(ActDial{Behaviour: b, Level: &lv}); err != nil {
			t.Fatal(err)
		}
	}
	play(c, clk, 24*time.Hour)

	v, err := c.Activity("")
	if err != nil || !v.Running || v.Last == "" || v.Hour == "" || !strings.Contains(v.Today+v.Hour, "vMotion") {
		t.Fatalf("after a day: running %v, last %q, hour %q, today %q (%v)", v.Running, v.Last, v.Hour, v.Today, err)
	}
	log := c.ActivityLog("week", "all", 1)
	for _, want := range []string{"vMotions", "powered off", "added", "removed", "into maintenance mode", "GB freed"} {
		if !strings.Contains(log.Sum, want) {
			t.Errorf("a day's activity should include %q: %s", want, log.Sum)
		}
	}
	if log.Count < 30 || len(log.Lines) != 25 || log.Pages < 2 {
		t.Errorf("the list has %d lines on %d pages", log.Count, log.Pages)
	}
	if h := c.Home().Activity; h.State != "running" || !strings.Contains(h.Line, "Changing by itself") {
		t.Errorf("Home: %+v", h)
	}
	// the VMs that came and went are in the scenario, where a rebuild finds them
	var added, removed int
	for _, o := range c.applied().sc.Clusters {
		added += len(o.AddedVMs)
		removed += len(o.RemovedVMs)
	}
	_, _, vms, _ := c.applied().topo.Counts()
	if added == 0 || removed == 0 || vms != 80+added-removed {
		t.Fatalf("added %d and removed %d VMs, and the vCenter has %d", added, removed, vms)
	}
	if vms < v.MinVMs || vms > v.MaxVMs || v.MinVMs != 76 || v.MaxVMs != 84 {
		t.Errorf("%d VMs is outside %d to %d", vms, v.MinVMs, v.MaxVMs)
	}
	want := placements(c)
	refs := map[string]string{}
	for _, cl := range c.applied().sim.Clusters {
		for i, vm := range cl.Cluster.LiveVMs() {
			_ = i
			refs[vm.Key] = vm.UUID
		}
	}
	lines := len(c.life.Log)
	c.Close()

	// the same data directory, a day later: everything is as it was left
	clk.add(26 * time.Hour)
	again := newCtlAt(t, lively(t), dir, "h", clk.now)
	if got := placements(again); !reflect.DeepEqual(got, want) {
		t.Errorf("after a restart %d things differ from %d", len(got), len(want))
	}
	got := map[string]string{}
	for _, cl := range again.applied().sim.Clusters {
		for _, vm := range cl.Cluster.LiveVMs() {
			got[vm.Key] = vm.UUID
		}
	}
	if !reflect.DeepEqual(got, refs) {
		t.Error("after a restart the vCenter does not have the same VMs with the same identities")
	}
	if av, _ := again.Activity(""); !av.Running || !av.SetUp || av.Rows[0].Level != 4 {
		t.Errorf("after a restart: running %v, set up %v, load balancing at %v", av.Running, av.SetUp, av.Rows[0].Level)
	}
	// nothing was caught up for the day it was off
	again.lifeStep()
	if n := len(again.life.Log) - lines; n > 3 {
		t.Errorf("%d lines were written for the time the simulator was off", n)
	}
}

func TestActivityDialsForTheVCenterAndForOneCluster(t *testing.T) {
	clk := &clock{t: monday9}
	c := newCtlAt(t, lively(t), "", "h", clk.now)
	startActivity(t, c)
	key := c.applied().topo.Clusters()[1].Key
	v, _ := c.Activity(key)
	if !v.Rows[0].Follows || v.Rows[0].Own || !strings.Contains(v.Rows[0].Text, "about 10 vMotions a day") {
		t.Fatalf("a cluster follows the vCenter until its dial is moved: %+v", v.Rows[0])
	}
	four := 4.0
	if done, err := c.ActivityDial(ActDial{Cluster: key, Behaviour: life.Moves, Level: &four}); err != nil || !strings.Contains(done.Done, "about 40 vMotions a day") {
		t.Fatalf("cluster dial: %+v, %v", done, err)
	}
	if v, _ = c.Activity(key); !v.Rows[0].Own || v.Rows[0].Level != 4 {
		t.Errorf("the cluster's own dial: %+v", v.Rows[0])
	}
	if g, _ := c.Activity(""); g.Rows[0].Level != 1 || g.Clusters[1].Own != 1 {
		t.Errorf("the vCenter's dial moved too, or the cluster is not marked: %+v", g.Rows[0])
	}
	if _, err := c.ActivityDial(ActDial{Cluster: key, Behaviour: life.Moves, Follow: true}); err != nil {
		t.Fatal(err)
	}
	if v, _ = c.Activity(key); !v.Rows[0].Follows {
		t.Error("the cluster does not follow the vCenter again")
	}

	// his own numbers: a VM every 30 minutes, 5% of hosts
	n := 30.0
	if done, err := c.ActivityDial(ActDial{Behaviour: life.Adds, N: &n, Unit: life.UnitMinutes}); err != nil || !strings.Contains(done.Done, "A new VM is added every 30 minutes") {
		t.Fatalf("exact number: %+v, %v", done, err)
	}
	zero := 0.0
	if _, err := c.ActivityDial(ActDial{Behaviour: life.Adds, N: &zero, Unit: life.UnitMinutes}); err == nil {
		t.Error("an exact number of 0 was accepted")
	}

	// a change that moves hosts says so first
	lvl := 4.0
	ask, err := c.ActivityDial(ActDial{Behaviour: life.Maintenance, Level: &lvl})
	if err != nil || ask.Confirm == nil || !strings.Contains(strings.Join(ask.Confirm.Body, " "), "into maintenance mode straight away") {
		t.Fatalf("raising the share of hosts in maintenance mode must ask first: %+v, %v", ask, err)
	}
	before := c.Home().Activity.Line
	if _, err := c.ActivityDial(ActDial{Behaviour: life.Maintenance, Level: &lvl, Confirmed: true}); err != nil {
		t.Fatal(err)
	}
	if after := c.Home().Activity.Line; after == before || !strings.Contains(after, "hosts are in maintenance mode") {
		t.Errorf("Home's line after more hosts went in: %q", after)
	}

	// do it now says what it will do, then does it and says what it did
	ask, err = c.ActivityDoNow(key, life.Moves, false)
	if err != nil || ask.Confirm == nil {
		t.Fatalf("do it now must ask first: %+v, %v", ask, err)
	}
	if done, err := c.ActivityDoNow(key, life.Moves, true); err != nil || !strings.Contains(done.Done, "DRS moved") {
		t.Fatalf("do it now: %+v, %v", done, err)
	}

	// stop: says what it will do, settles, and nothing more happens
	if ask, _ := c.ActivityStop(false); ask.Confirm == nil || !strings.Contains(strings.Join(ask.Confirm.Body, " "), "out of maintenance mode") {
		t.Fatalf("stop must say hosts come out of maintenance mode: %+v", ask)
	}
	if _, err := c.ActivityStop(true); err != nil {
		t.Fatal(err)
	}
	if snap := c.applied().sim.Life().Snapshot(); len(snap.Maintenance) != 0 || len(snap.Off) != 0 {
		t.Errorf("after stop: %v in maintenance mode, %v off", snap.Maintenance, snap.Off)
	}
	lines := len(c.life.Log)
	play(c, clk, 3*time.Hour)
	if len(c.life.Log) != lines {
		t.Errorf("%d lines were written while stopped", len(c.life.Log)-lines)
	}
	if h := c.Home().Activity; h.State != "stopped" {
		t.Errorf("Home says %q", h.State)
	}
}

func TestActivitySurvivesAChangeThatRebuilds(t *testing.T) {
	clk := &clock{t: monday9}
	c := newCtlAt(t, lively(t), "", "h", clk.now)
	startActivity(t, c)
	four := 4.0
	for _, b := range []string{life.Moves, life.Power, life.Adds, life.Removes} {
		if _, err := c.ActivityDial(ActDial{Behaviour: b, Level: &four}); err != nil {
			t.Fatal(err)
		}
	}
	play(c, clk, 8*time.Hour)
	want := placements(c)
	old := c.applied().sim
	if len(want) < 20 {
		t.Fatalf("too little happened to test with: %d", len(want))
	}
	// an ordinary change the owner makes: one more cluster
	c.mu.Lock()
	next := clone(c.applied().sc)
	next.Datacenters[0].Clusters = 3
	rebuilt, err := c.applyScenario(next, false)
	c.mu.Unlock()
	if err != nil || !rebuilt || c.applied().sim == old {
		t.Fatalf("adding a cluster: rebuilt %v, %v", rebuilt, err)
	}
	if got := placements(c); !reflect.DeepEqual(got, want) {
		t.Errorf("after the rebuild %d things are placed, off or in maintenance mode; before, %d", len(got), len(want))
	}
	v, _ := c.Activity("")
	if v.VMs != v.Built || v.MaxVMs != 84+40 || len(v.Clusters) != 3 {
		t.Errorf("after one more cluster of 40 VMs: %d VMs, most %d, %d clusters", v.VMs, v.MaxVMs, len(v.Clusters))
	}
	// and activity carries on, on the new vCenter
	lines := len(c.life.Log)
	play(c, clk, 2*time.Hour)
	if len(c.life.Log) == lines {
		t.Error("nothing happened after the rebuild")
	}
}

func TestActivityKeepsAbbreviationsWhenASentenceIsJoined(t *testing.T) {
	for in, want := range map[string]string{"VMs removed": "VMs removed", "CPU load": "CPU load", "DRS moved 4 VMs in cluster-01 to balance the load.": "DRS moved 4 VMs in cluster-01 to balance the load.",
		"New VMs": "new VMs", "Load balancing (DRS)": "load balancing (DRS)", "A new VM is added every 30 minutes, day and night.": "a new VM is added every 30 minutes, day and night.", "": ""} {
		if got := lowerWord(in); got != want {
			t.Errorf("lowerWord(%q) = %q, want %q", in, got, want)
		}
	}
}

// hostsOfAdded is where the VMs Activity added to the lively scenario are,
// by name: the host the running vCenter has each on when that is not the
// host its number gives it, and that host otherwise.
func hostsOfAdded(c *Controller) map[string]string {
	out := map[string]string{}
	snap := c.applied().sim.Life().Snapshot()
	for _, refs := range c.applied().sim.Clusters {
		cl := refs.Cluster
		for _, vm := range cl.LiveVMs() {
			if vm.Index < 40 {
				continue
			}
			if out[vm.Name] = snap.Hosts[vm.Key]; out[vm.Name] == "" {
				out[vm.Name] = cl.Hosts[vm.Host%len(cl.Hosts)].Name
			}
		}
	}
	return out
}

// A VM Activity added is made on the emptiest host, not on the host its
// number would give it; a restart must find it where it was.
func TestActivityAddedVMsStayOnTheirHostsAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: monday9}
	c := newCtlAt(t, lively(t), dir, "h", clk.now)
	startActivity(t, c)
	key := c.applied().topo.Clusters()[0].Key
	for i := 0; i < 3; i++ {
		// a different VM leaves each time, so the emptiest host is a different one
		if _, err := c.ActivityDoNow(key, life.Removes, true); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ActivityDoNow(key, life.Adds, true); err != nil {
			t.Fatal(err)
		}
	}
	want := hostsOfAdded(c)
	if len(want) != 3 {
		t.Fatalf("%d added VMs are on a host, want 3: %v", len(want), want)
	}
	c.Close()
	again := newCtlAt(t, lively(t), dir, "h", clk.now)
	if got := hostsOfAdded(again); !reflect.DeepEqual(got, want) {
		t.Errorf("after a restart the added VMs are on %v; they were on %v", got, want)
	}
}

func TestActivityMovesNothingWhileTheSimulatorIsPaused(t *testing.T) {
	clk := &clock{t: monday9}
	c := newCtlAt(t, lively(t), "", "h", clk.now)
	c.SetPaused(true)
	if _, err := c.ActivityStart(true); err == nil {
		t.Error("activity started, and moved hosts, while the simulator was paused")
	}
	c.SetPaused(false)
	startActivity(t, c)
	before := placements(c)
	c.SetPaused(true)
	four := 4.0
	if _, err := c.ActivityDial(ActDial{Behaviour: life.Maintenance, Level: &four, Confirmed: true}); err == nil {
		t.Error("more hosts went into maintenance mode while the simulator was paused")
	}
	if _, err := c.ActivityStop(true); err == nil {
		t.Error("activity stopped, and moved hosts back, while the simulator was paused")
	}
	if _, err := c.ActivityDoNow(c.applied().topo.Clusters()[0].Key, life.Moves, true); err == nil {
		t.Error("VMs moved while the simulator was paused")
	}
	play(c, clk, time.Hour)
	if after := placements(c); !reflect.DeepEqual(after, before) {
		t.Error("something moved while the simulator was paused")
	}
}

// The scenario file wins when it changes, for what it says. It says nothing
// of the VMs Activity added and removed, so those are kept: a removed VM
// does not come back and an added one is not lost.
func TestActivityVMsSurviveAChangedScenarioFile(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: monday9}
	c := newCtlAt(t, lively(t), dir, "first", clk.now)
	startActivity(t, c)
	key := c.applied().topo.Clusters()[0].Key
	for _, b := range []string{life.Removes, life.Adds} {
		if _, err := c.ActivityDoNow(key, b, true); err != nil {
			t.Fatal(err)
		}
	}
	names := func(c *Controller) map[string]string {
		out := map[string]string{}
		for _, cl := range c.applied().sim.Clusters {
			for _, vm := range cl.Cluster.LiveVMs() {
				out[vm.Name] = vm.UUID
			}
		}
		return out
	}
	want := names(c)
	if _, ok := want["cluster-01-vm-041"]; !ok || len(want) != 80 {
		t.Fatalf("after one VM removed and one added: %d VMs, vm-041 there: %v", len(want), ok)
	}
	c.Close()

	// the same file with one more cluster
	bigger := lively(t)
	bigger.Datacenters[0].Clusters = 3
	if err := bigger.Validate(); err != nil {
		t.Fatal(err)
	}
	again := newCtlAt(t, bigger, dir, "second", clk.now)
	got := names(again)
	for n, id := range want {
		if got[n] != id {
			t.Errorf("%s: identity %q before the file changed, %q after", n, id, got[n])
		}
	}
	if len(got) != 120 {
		t.Errorf("%d VMs after the file added a cluster of 40, want 120: a removed VM came back or an added one was lost", len(got))
	}
	// the limits follow the size the file now gives
	if v, _ := again.Activity(""); v.VMs < v.MinVMs || v.VMs > v.MaxVMs {
		t.Errorf("%d VMs is outside the limits %d to %d", v.VMs, v.MinVMs, v.MaxVMs)
	}
}

// Removing hosts takes the highest-numbered VMs with them. The next VM
// Activity adds must not take one of their numbers.
func TestActivityNeverReusesANumberAfterHostsAreRemoved(t *testing.T) {
	clk := &clock{t: monday9}
	c := newCtlAt(t, lively(t), "", "h", clk.now)
	startActivity(t, c)
	key := c.applied().topo.Clusters()[0].Key
	c.mu.Lock()
	next := clone(c.applied().sc)
	override(next, key).RemovedHosts = []int{8}
	_, err := c.applyScenario(next, false)
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	cl := c.applied().topo.Clusters()[0]
	if len(cl.LiveVMs()) != 35 || cl.VMHigh != 40 {
		t.Fatalf("after a host was removed: %d VMs, highest number ever used %d; want 35 and 40", len(cl.LiveVMs()), cl.VMHigh)
	}
	done, err := c.ActivityDoNow(key, life.Adds, true)
	if err != nil || !strings.Contains(done.Done, "cluster-01-vm-041") {
		t.Errorf("the next VM: %+v, %v; want cluster-01-vm-041", done, err)
	}
}
