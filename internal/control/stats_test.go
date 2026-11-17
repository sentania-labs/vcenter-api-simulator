package control

import (
	"strings"
	"testing"
	"time"
)

// Home carries the build the process was started as and the vCenter it
// presents, taken from the options the build stamped, not from the console.
func TestHomeSaysWhichBuildIsRunning(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	c.opts.Version, c.opts.Commit = "v9.8.7", "0123456789abcdef0123456789abcdef01234567"
	h := c.Home()
	if h.Build.Version != "v9.8.7" || h.Build.Commit != c.opts.Commit {
		t.Errorf("build %+v", h.Build)
	}
	if h.Build.VCenter != h.VCenter || h.Build.VCenter == "" {
		t.Errorf("presented vCenter %q, home says %q", h.Build.VCenter, h.VCenter)
	}
}

// What tools ask moves the numbers, counted by who was asked and what became
// of it, and the totals agree with the Requests screen.
func TestStatsCountWhatToolsAsked(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	if st := c.Home().Stats; st.Total.Requests != 0 || len(st.Total.Services) != 0 {
		t.Fatalf("fresh simulator already counts %+v", st.Total)
	}
	tool := newTool(t, c, "stats-test")
	if tool.login("nobody@vsphere.local", "wrong") {
		t.Fatal("a wrong password signed in")
	}
	tool.content()
	tool.content()
	tool.post("/sdk", `<NoSuchOperation xmlns="urn:vim25"><_this type="ServiceInstance">ServiceInstance</_this></NoSuchOperation>`)
	st := c.Home().Stats
	if st.Total.Requests != 4 || st.LastHour.Requests != 4 {
		t.Fatalf("counted %d / %d requests, want 4", st.Total.Requests, st.LastHour.Requests)
	}
	if st.Total.Answered != 2 || st.Total.TurnedAway != 1 || st.Total.NoAnswer != 1 || st.Total.Failed != 0 {
		t.Errorf("outcomes %+v", st.Total.Counts)
	}
	if len(st.Total.Services) != 1 || st.Total.Services[0].Key != "vsphere" || st.Total.Services[0].Name != "vSphere API" || st.Total.Services[0].Requests != 4 {
		t.Errorf("services %+v", st.Total.Services)
	}
	if st.Total.SlowMs < st.Total.MedianMs || st.Total.MedianMs <= 0 {
		t.Errorf("timing median %v slow %v", st.Total.MedianMs, st.Total.SlowMs)
	}
	if got := c.Home().Unanswered; got != 1 {
		t.Errorf("the tab badge says %d not answered; the stats say %d", got, st.Total.NoAnswer)
	}
}

// The last-hour numbers forget old requests; the since-started ones do not.
func TestStatsLastHourFallsOffButTotalStays(t *testing.T) {
	clock := &clock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	c := newCtlAt(t, small(t), "", "t", clock.now)
	tool := newTool(t, c, "stats-test")
	tool.content()
	clock.add(90 * time.Minute)
	tool.content()
	st := c.Home().Stats
	if st.Total.Requests != 2 || st.LastHour.Requests != 1 {
		t.Errorf("total %d, last hour %d; want 2 and 1", st.Total.Requests, st.LastHour.Requests)
	}
	if st.UptimeSec != 90*60 {
		t.Errorf("up %d s, want %d", st.UptimeSec, 90*60)
	}
	if !st.StartedAt.Equal(clock.now().Add(-90 * time.Minute)) {
		t.Errorf("started at %v", st.StartedAt)
	}
	if strings.Contains(c.Home().Build.Version, " ") {
		t.Error("version has stray spaces")
	}
}
