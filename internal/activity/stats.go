package activity

import (
	"strings"
	"time"
)

// Services: which of the simulated vCenter's services a request was for. The
// console's stats are counted by these.
const (
	ServiceVSphere = "vsphere" // the vSphere API (/sdk)
	ServiceVsan    = "vsan"
	ServiceLookup  = "lookup"
	ServiceLicense = "license"
	ServiceSignOn  = "signon"
	ServiceREST    = "rest"
	ServiceOther   = "other"
)

// Services lists the services in the order the console shows them.
var Services = []string{ServiceVSphere, ServiceVsan, ServiceREST, ServiceLookup, ServiceLicense, ServiceSignOn, ServiceOther}

// ServiceOf says which service a request was for.
func ServiceOf(c Call) string {
	m := c.Method
	path := ""
	if i := strings.IndexByte(m, ' '); i > 0 && strings.HasPrefix(m[i+1:], "/") {
		path = m[i+1:]
	}
	switch {
	case strings.HasPrefix(path, "/lookupservice"):
		return ServiceLookup
	case strings.HasPrefix(path, "/sts"), strings.HasPrefix(path, "/sso-adminserver"):
		return ServiceSignOn
	case strings.HasPrefix(path, "/ls/"), path != "" && strings.Contains(path, "/license"):
		return ServiceLicense
	case c.Surface == SurfaceVsan:
		return ServiceVsan
	case c.Surface == SurfaceREST:
		return ServiceREST
	case c.Surface == SurfaceSDK && path == "" && strings.Contains(m, "License"):
		return ServiceLicense
	case c.Surface == SurfaceSDK:
		return ServiceVSphere
	}
	return ServiceOther
}

// Outcomes: what became of a request, in the four words the stats use.
const (
	OutcomeAnswered   = "answered"   // answered, answered "none", or refused as a real vCenter refuses
	OutcomeTurnedAway = "turnedAway" // sign-in refused, not signed in, or the simulator was paused
	OutcomeNoAnswer   = "noAnswer"   // the simulator has no answer for it
	OutcomeFailed     = "failed"     // failed on purpose, by a fault the operator switched on
)

// OutcomeOf folds a call's reason into one of the outcomes.
func OutcomeOf(reason string) string {
	switch reason {
	case ReasonDenied, ReasonPaused:
		return OutcomeTurnedAway
	case ReasonFault:
		return OutcomeFailed
	case ReasonAnswered, ReasonEmpty, ReasonRefused:
		return OutcomeAnswered
	}
	return OutcomeNoAnswer
}

// bounds are the upper edges, in milliseconds, of the timing buckets. A
// response slower than the last bound falls in a final open bucket.
var bounds = [...]float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000}

// Timing counts responses by how long they took.
type Timing struct {
	Buckets [len(bounds) + 1]uint64
	N       uint64
}

func (t *Timing) add(ms float64) {
	i := 0
	for i < len(bounds) && ms > bounds[i] {
		i++
	}
	t.Buckets[i]++
	t.N++
}

// Percentile returns the upper edge of the bucket the p-th fraction (0..1)
// of responses fell at or under, in ms; 0 with no responses. The open last
// bucket reports the last bound. It is a bucket, not an exact figure.
func (t Timing) Percentile(p float64) float64 {
	if t.N == 0 {
		return 0
	}
	want := uint64(float64(t.N)*p + 0.999999)
	if want < 1 {
		want = 1
	}
	var seen uint64
	for i, n := range t.Buckets {
		seen += n
		if seen >= want {
			return bounds[min(i, len(bounds)-1)]
		}
	}
	return bounds[len(bounds)-1]
}

// Counts is a tally of outcomes.
type Counts struct {
	Answered   uint64 `json:"answered"`
	TurnedAway uint64 `json:"turnedAway"`
	NoAnswer   uint64 `json:"noAnswer"`
	Failed     uint64 `json:"failed"`
}

// Total is every request counted.
func (c Counts) Total() uint64 { return c.Answered + c.TurnedAway + c.NoAnswer + c.Failed }

func (c *Counts) add(reason string) {
	switch OutcomeOf(reason) {
	case OutcomeAnswered:
		c.Answered++
	case OutcomeTurnedAway:
		c.TurnedAway++
	case OutcomeFailed:
		c.Failed++
	default:
		c.NoAnswer++
	}
}

// Tally is requests counted overall and by service, with how long they took.
type Tally struct {
	Counts
	// MedianMs and SlowMs are the typical (half were quicker) and slow-end
	// (nineteen in twenty were quicker) response times, as the upper edge of
	// the timing bucket they fall in.
	MedianMs  float64           `json:"medianMs"`
	SlowMs    float64           `json:"slowMs"`
	ByService map[string]Counts `json:"byService"`
	timing    Timing
}

func newTally() Tally { return Tally{ByService: map[string]Counts{}} }

func (t *Tally) add(c Call) {
	t.Counts.add(c.Reason)
	sv := ServiceOf(c)
	cs := t.ByService[sv]
	cs.add(c.Reason)
	t.ByService[sv] = cs
	t.timing.add(c.DurationMs)
}

func (t *Tally) finish() {
	t.MedianMs, t.SlowMs = t.timing.Percentile(0.5), t.timing.Percentile(0.95)
}

// TallyOf counts calls.
func TallyOf(calls []Call) Tally {
	t := newTally()
	for _, c := range calls {
		t.add(c)
	}
	t.finish()
	return t
}

// Lifetime returns what has been counted since the log started. Unlike the
// held calls it is not bounded by the ring.
func (l *Log) Lifetime() Tally {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := newTally()
	t.Counts = l.life.Counts
	t.timing = l.life.timing
	for k, v := range l.life.ByService {
		t.ByService[k] = v
	}
	t.finish()
	return t
}

// Window returns the tally of calls at or after t, and when the tally really
// begins: t, unless the ring is full and its oldest held call is newer than t,
// in which case calls from before then have been dropped and that call's time
// is returned.
func (l *Log) Window(t time.Time) (Tally, time.Time) {
	held := l.All()
	from := t
	if len(held) == len(l.ring) {
		oldest := held[0].Time
		for _, c := range held {
			if c.Time.Before(oldest) {
				oldest = c.Time
			}
		}
		if oldest.After(t) {
			from = oldest
		}
	}
	var in []Call
	for _, c := range held {
		if !c.Time.Before(t) {
			in = append(in, c)
		}
	}
	return TallyOf(in), from
}
