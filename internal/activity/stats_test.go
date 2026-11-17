package activity

import (
	"testing"
	"time"
)

func call(surface, method, reason string, ms float64) Call {
	return Call{Time: time.Unix(1000, 0), Surface: surface, Method: method, Reason: reason, DurationMs: ms}
}

func TestServiceOf(t *testing.T) {
	for _, tc := range []struct {
		c    Call
		want string
	}{
		{call(SurfaceSDK, "RetrieveServiceContent", ReasonAnswered, 1), ServiceVSphere},
		{call(SurfaceSDK, "QueryAssignedLicenses", ReasonAnswered, 1), ServiceLicense},
		{call(SurfaceVsan, "VsanPerfQueryPerf", ReasonAnswered, 1), ServiceVsan},
		{call(SurfaceREST, "GET /api/vcenter/cluster", ReasonAnswered, 1), ServiceREST},
		{call(SurfaceOther, "POST /lookupservice/sdk", ReasonAnswered, 1), ServiceLookup},
		{call(SurfaceOther, "POST /sts/STSService/vsphere.local", ReasonAnswered, 1), ServiceSignOn},
		{call(SurfaceOther, "POST /sso-adminserver/sdk/vsphere.local", ReasonAnswered, 1), ServiceSignOn},
		{call(SurfaceOther, "POST /ls/sdk", ReasonAnswered, 1), ServiceLicense},
		{call(SurfaceOther, "GET /favicon.ico", ReasonUnknown, 1), ServiceOther},
	} {
		if got := ServiceOf(tc.c); got != tc.want {
			t.Errorf("%s %s: service %s, want %s", tc.c.Surface, tc.c.Method, got, tc.want)
		}
	}
}

func TestOutcomeOf(t *testing.T) {
	for reason, want := range map[string]string{
		ReasonAnswered: OutcomeAnswered, ReasonEmpty: OutcomeAnswered, ReasonRefused: OutcomeAnswered,
		ReasonDenied: OutcomeTurnedAway, ReasonPaused: OutcomeTurnedAway,
		ReasonUnknown: OutcomeNoAnswer, ReasonFault: OutcomeFailed,
	} {
		if got := OutcomeOf(reason); got != want {
			t.Errorf("%s: %s, want %s", reason, got, want)
		}
	}
}

func TestTimingBuckets(t *testing.T) {
	var tm Timing
	if tm.Percentile(0.5) != 0 {
		t.Fatal("no responses should report 0")
	}
	// 90 quick responses, 10 slow ones
	for i := 0; i < 90; i++ {
		tm.add(3) // bucket ≤5
	}
	for i := 0; i < 10; i++ {
		tm.add(700) // bucket ≤1000
	}
	if got := tm.Percentile(0.5); got != 5 {
		t.Errorf("median %v, want 5", got)
	}
	if got := tm.Percentile(0.95); got != 1000 {
		t.Errorf("slow end %v, want 1000", got)
	}
	// edges belong to the lower bucket; anything past the last edge is in the open bucket
	var edge Timing
	edge.add(5)
	edge.add(60000)
	if edge.Buckets[0] != 1 || edge.Buckets[len(bounds)] != 1 {
		t.Errorf("edge buckets %v", edge.Buckets)
	}
	if got := edge.Percentile(1); got != bounds[len(bounds)-1] {
		t.Errorf("open bucket reports %v", got)
	}
}

func TestLifetimeSurvivesTheRingAndTheWindowDoesNot(t *testing.T) {
	now := time.Unix(10_000, 0)
	l := New(4, now)
	add := func(at time.Time, c Call) { c.Time = at; l.Add(c) }
	for i := 0; i < 10; i++ { // ten answered, long ago; the ring keeps four
		add(now.Add(-2*time.Hour), call(SurfaceSDK, "RetrieveServiceContent", ReasonAnswered, 2))
	}
	add(now, call(SurfaceVsan, "VsanPerfQueryPerf", ReasonUnknown, 300))
	add(now, call(SurfaceREST, "POST /api/session", ReasonDenied, 8))
	add(now, call(SurfaceVsan, "VsanPerfQueryPerf", ReasonFault, 4000))
	life := l.Lifetime()
	if life.Total() != 13 || life.Answered != 10 || life.NoAnswer != 1 || life.TurnedAway != 1 || life.Failed != 1 {
		t.Errorf("lifetime %+v", life.Counts)
	}
	if got := life.ByService[ServiceVsan]; got.NoAnswer != 1 || got.Failed != 1 || got.Total() != 2 {
		t.Errorf("vsan %+v", got)
	}
	win := l.Window(now.Add(-time.Hour))
	if win.Total() != 3 || win.Answered != 0 || win.NoAnswer != 1 || win.TurnedAway != 1 || win.Failed != 1 {
		t.Errorf("window %+v", win.Counts)
	}
	if win.SlowMs != 5000 || win.MedianMs != 500 {
		t.Errorf("window timing median %v slow %v", win.MedianMs, win.SlowMs)
	}
	if l.Lifetime().Total() != l.Totals().Requests {
		t.Error("lifetime and the log's own total disagree")
	}
}
