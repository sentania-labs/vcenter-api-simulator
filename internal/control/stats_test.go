package control

import (
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/activity"
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
}

// A busy simulator's request log holds fewer calls than an hour of them. The
// last-hour figures then say how far back they reach instead of claiming the
// hour, and the since-started figures still count everything.
func TestStatsLastHourSaysWhenItCoversLess(t *testing.T) {
	clock := &clock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	c := newCtlAt(t, small(t), "", "t", clock.now)
	if st := c.Home().Stats; st.LastHour.CoveredSec != 0 {
		t.Errorf("a fresh simulator covers %d s, want it to say nothing", st.LastHour.CoveredSec)
	}
	const held = activity.DefaultSize
	for i := 0; i < held+10; i++ {
		clock.add(10 * time.Millisecond)
		c.Log.Add(activity.Call{Time: clock.now(), Surface: activity.SurfaceSDK, Method: "RetrieveServiceContent", Reason: activity.ReasonAnswered, DurationMs: 1})
	}
	clock.add(time.Second)
	st := c.Home().Stats
	if st.Total.Requests != held+10 {
		t.Errorf("since started counts %d, want %d", st.Total.Requests, held+10)
	}
	if st.LastHour.Requests != held {
		t.Errorf("last hour counts %d, want the %d the log holds", st.LastHour.Requests, held)
	}
	wantSec := int64((time.Duration(held-1)*10*time.Millisecond + time.Second).Seconds())
	if st.LastHour.CoveredSec != wantSec {
		t.Errorf("covered %d s, want %d", st.LastHour.CoveredSec, wantSec)
	}
	if st.Total.CoveredSec != 0 {
		t.Errorf("since started claims to cover only %d s", st.Total.CoveredSec)
	}
	clock.add(2 * time.Hour)
	if st := c.Home().Stats; st.LastHour.Requests != 0 || st.LastHour.CoveredSec != 0 {
		t.Errorf("an hour later: %d requests covering %d s, want an empty, whole hour", st.LastHour.Requests, st.LastHour.CoveredSec)
	}
}

// The view carries "over" through to the API answer.
func TestStatBlockCarriesOver(t *testing.T) {
	b := statBlock(activity.TallyOf([]activity.Call{{Reason: activity.ReasonFault, DurationMs: 125000}}))
	if !b.MedianOver || !b.SlowOver {
		t.Errorf("%+v", b)
	}
}
