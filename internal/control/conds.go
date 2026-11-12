package control

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// Slowness and faults. Everything that makes the simulator misbehave on
// purpose is a scenario.Condition, so there is one list of what is on, one
// way to turn a thing off, and one sentence that describes it wherever it
// is shown.

// EndedCondition is a condition that is no longer on. It is kept for a
// while so that the requests it failed can still say why they failed.
type EndedCondition struct {
	ID   int       `json:"id"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

func shortCluster(key string) string {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[i+1:]
	}
	return key
}

func datacenterOf(key string) string {
	if i := strings.IndexByte(key, '/'); i >= 0 {
		return key[:i]
	}
	return ""
}

// clusterLabel names a cluster as a person reads it: its name alone, or
// with its datacenter when the scenario has more than one (cluster names
// repeat across datacenters).
func clusterLabel(sc *scenario.Scenario, key string) string {
	if len(sc.Datacenters) > 1 {
		return shortCluster(key) + " in " + datacenterOf(key)
	}
	return shortCluster(key)
}

func clusterLabels(sc *scenario.Scenario, keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, clusterLabel(sc, k))
	}
	sort.Strings(out)
	return out
}

// lift turns the scenario file's own fault rules, latency and
// statsPrimary: false into conditions, and gives every condition an id.
// After it the scenario's Faults and Latency are empty and no cluster has
// its performance service switched off in its spec.
func (c *Controller) lift(sc *scenario.Scenario) {
	for _, f := range sc.Faults {
		cond := scenario.Condition{Clusters: f.Clusters, From: f.From, To: f.To, Snap: true}
		switch f.Fault {
		case scenario.FaultSystemError:
			cond.Kind = scenario.CondError
		case scenario.FaultTimeout:
			cond.Kind = scenario.CondTimeout
		default:
			cond.Kind, cond.Fault = scenario.CondMissing, f.Fault
		}
		if f.Family != "" {
			cond.Which, cond.Family = scenario.WhichOne, f.Family
		} else {
			cond.Which, cond.Method = scenario.WhichMethod, f.Method
		}
		sc.Conditions = append(sc.Conditions, cond)
	}
	sc.Faults = nil
	l := sc.Latency
	var base scenario.Delay
	if d := l.PerfQuery; d != nil && (d.FixedMs > 0 || d.JitterMs > 0) {
		base = *d
		sc.Conditions = append(sc.Conditions, scenario.Condition{Kind: scenario.CondSlow, Which: scenario.WhichPerf, FixedMs: d.FixedMs, JitterMs: d.JitterMs, Base: true})
	}
	var fams []string
	for f := range l.Families {
		fams = append(fams, f)
	}
	sort.Strings(fams)
	for _, f := range fams {
		d := l.Families[f]
		if d.FixedMs > 0 || d.JitterMs > 0 || base.FixedMs > 0 || base.JitterMs > 0 {
			sc.Conditions = append(sc.Conditions, scenario.Condition{Kind: scenario.CondSlow, Which: scenario.WhichOne, Family: f, FixedMs: d.FixedMs, JitterMs: d.JitterMs, Base: true})
		}
	}
	if d := l.SDK; d != nil && (d.FixedMs > 0 || d.JitterMs > 0) {
		sc.Conditions = append(sc.Conditions, scenario.Condition{Kind: scenario.CondSlow, Which: scenario.WhichInv, FixedMs: d.FixedMs, JitterMs: d.JitterMs})
	}
	sc.Latency = scenario.Latency{}

	off := func(p *bool) bool { return p != nil && !*p }
	var down []string
	keys := sc.ClusterKeys()
	for _, key := range keys {
		spec := sc.Defaults.StatsPrimary
		if o, ok := sc.Override(key); ok && o.StatsPrimary != nil {
			spec = o.StatsPrimary
		}
		if off(spec) {
			down = append(down, key)
		}
	}
	sc.Defaults.StatsPrimary = nil
	for i := range sc.Clusters {
		sc.Clusters[i].StatsPrimary = nil
	}
	if len(down) > 0 {
		cond := scenario.Condition{Kind: scenario.CondPerfDown, Clusters: down}
		if len(down) == len(keys) {
			cond.Clusters = nil
		}
		sc.Conditions = append(sc.Conditions, cond)
	}
	for i := range sc.Conditions {
		if sc.Conditions[i].ID > c.seq {
			c.seq = sc.Conditions[i].ID
		}
	}
	for i := range sc.Conditions {
		if sc.Conditions[i].ID == 0 {
			c.seq++
			sc.Conditions[i].ID = c.seq
		}
	}
}

// syncConditions sets each cluster's performance service on or off as the
// conditions say right now.
func (c *Controller) syncConditions(r *running) {
	now := c.now()
	for _, cl := range r.topo.Clusters() {
		_, off := perfOffCause(r.sc, cl.Key, now)
		cl.SetPerfOff(off)
	}
}

// armExpiry sets the timer for the next moment a condition turns itself off
// or on. c.mu is held, or the caller is New.
func (c *Controller) armExpiry() {
	if c.expiry != nil {
		c.expiry.Stop()
		c.expiry = nil
	}
	now := c.now()
	var next time.Time
	consider := func(t time.Time) {
		if t.After(now) && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	for _, cond := range c.applied().sc.Conditions {
		if cond.To != nil {
			// a snapped window ends on the bucket boundary after To
			consider(cond.To.UTC().Add(5 * time.Minute).Truncate(5 * time.Minute))
			consider(cond.To.UTC())
		}
		if cond.From != nil {
			consider(cond.From.UTC())
		}
	}
	if next.IsZero() {
		return
	}
	c.expiry = time.AfterFunc(next.Sub(now)+50*time.Millisecond, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.sweep()
	})
}

// sweep moves conditions that have turned themselves off out of the list,
// says so in the history, and brings the performance service flags up to
// date. c.mu is held.
func (c *Controller) sweep() {
	now := c.now()
	r := c.applied()
	var keep []scenario.Condition
	var gone []scenario.Condition
	for _, cond := range r.sc.Conditions {
		if cond.Ended(now) {
			gone = append(gone, cond)
		} else {
			keep = append(keep, cond)
		}
	}
	if len(gone) == 0 {
		c.syncConditions(r)
		c.armExpiry()
		c.pruneEnded()
		return
	}
	next := clone(r.sc)
	next.Conditions = keep
	if _, err := c.applyScenario(next, false); err != nil {
		// nothing was cleared; the next look tries again
		log.Printf("control: a fault that had run out could not be cleared: %v", err)
		return
	}
	for _, cond := range gone {
		at := now
		if cond.To != nil {
			at = cond.To.UTC()
		}
		c.ended = append(c.ended, EndedCondition{ID: cond.ID, Kind: cond.Kind, Text: c.condText(r.sc, cond), At: at})
		what := "A fault"
		if cond.Kind == scenario.CondSlow {
			what = "Slowness"
		}
		c.addHistory(fmt.Sprintf("%s turned itself off. %s", what, offText(r.sc, cond)))
	}
	c.save()
	c.pruneEnded()
}

// pruneEnded forgets ended conditions older than the hour of requests they
// can still explain.
func (c *Controller) pruneEnded() {
	cut := c.now().Add(-2 * time.Hour)
	keep := c.ended[:0]
	for _, e := range c.ended {
		if e.At.After(cut) {
			keep = append(keep, e)
		}
	}
	c.ended = keep
}

// ---------------------------------------------------------------------------
// the words

func condWhat(cond scenario.Condition) string {
	switch cond.Which {
	case scenario.WhichInv:
		return "lists of clusters, hosts and VMs"
	case scenario.WhichOne:
		return "statistics about " + lower1(familyName(cond.Family))
	case scenario.WhichMethod:
		if n, ok := faultMethodNames[cond.Method]; ok {
			return n
		}
		return `what tools call "` + cond.Method + `"`
	}
	return "all vSAN performance data"
}

func condWhere(sc *scenario.Scenario, cond scenario.Condition) string {
	if cond.Kind == scenario.CondSlow && cond.Which == scenario.WhichInv {
		return ""
	}
	if len(cond.Clusters) > 0 {
		return ", on " + listAnd(clusterLabels(sc, cond.Clusters))
	}
	return ", on every cluster"
}

// condText is the one sentence that says what a condition does.
func (c *Controller) condText(sc *scenario.Scenario, cond scenario.Condition) string {
	w, wh := condWhat(cond), condWhere(sc, cond)
	switch cond.Kind {
	case scenario.CondSlow:
		vary := ""
		if cond.JitterMs > 0 {
			vary = fmt.Sprintf(" (give or take %s ms)", num(cond.JitterMs))
		}
		if cond.Base && cond.Which == scenario.WhichOne {
			return c.baseFamilyText(sc, cond, w, wh)
		}
		if cond.Base {
			for _, o := range sc.Conditions {
				if o.Base && o.Which == scenario.WhichOne {
					return fmt.Sprintf("Requests for %s take about %s ms longer%s%s, except for data the scenario file gives its own delay.", w, num(cond.FixedMs), vary, wh)
				}
			}
		}
		return fmt.Sprintf("Requests for %s take about %s ms longer%s%s.", w, num(cond.FixedMs), vary, wh)
	case scenario.CondError:
		return fmt.Sprintf("Requests for %s come back with an error%s.", w, wh)
	case scenario.CondMissing:
		verb := "is"
		if cond.Which == scenario.WhichOne {
			verb = "are"
		}
		return fmt.Sprintf("The simulator says %s %s not available%s.", w, verb, wh)
	case scenario.CondTimeout:
		return fmt.Sprintf("Requests for %s are never answered%s.", w, wh)
	}
	return fmt.Sprintf("The vSAN performance service is switched off%s.", wh)
}

// baseFamilyText says what a scenario file's own delay for one kind of data
// comes to, including slowness added since, worked out by the function that
// applies it.
func (c *Controller) baseFamilyText(sc *scenario.Scenario, cond scenario.Condition, w, wh string) string {
	total := cond.FixedMs
	vary := cond.JitterMs
	if now := c.now(); cond.ActiveAt(now) {
		all := scenario.SlowFor(sc.Conditions, cond.Family, "", now)
		total, vary = all.FixedMs, all.JitterMs
	}
	give := ""
	if vary > 0 {
		give = fmt.Sprintf(" (give or take %s ms)", num(vary))
	}
	if total != cond.FixedMs || vary != cond.JitterMs {
		return fmt.Sprintf("Requests for %s take about %s ms%s in all: %s ms from the scenario file plus %s ms added here%s.", w, num(total), give, num(cond.FixedMs), num(total-cond.FixedMs), wh)
	}
	if total == 0 && vary == 0 {
		return fmt.Sprintf("Requests for %s are not slowed down, whatever the scenario file's general delay is%s.", w, wh)
	}
	return fmt.Sprintf("Requests for %s take about %s ms%s, set by the scenario file in place of its general delay%s.", w, num(total), give, wh)
}

// offText says what is back to normal once a condition is off.
func offText(sc *scenario.Scenario, cond scenario.Condition) string {
	switch cond.Kind {
	case scenario.CondSlow:
		return fmt.Sprintf("Requests for %s are back to normal speed.", condWhat(cond))
	case scenario.CondPerfDown:
		return "The vSAN performance service is back on."
	}
	return fmt.Sprintf("Requests for %s are answered again.", condWhat(cond))
}

// toolAdvice is what a well-behaved tool should do about each kind.
var toolAdvice = map[string]string{
	scenario.CondSlow:     "A well-behaved tool still collects everything, just more slowly.",
	scenario.CondError:    "A well-behaved tool reports a collection problem for that data and carries on with the rest.",
	scenario.CondMissing:  "A well-behaved tool quietly skips that data and collects the rest.",
	scenario.CondTimeout:  "A well-behaved tool gives up after its own time limit and reports that vCenter did not answer.",
	scenario.CondPerfDown: "A well-behaved tool reports that the performance service is not available for that cluster.",
}

// ---------------------------------------------------------------------------
// the screen

// ConditionView is one thing that is switched on.
type ConditionView struct {
	ID   int    `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	// Since is when it was switched on, when the simulator knows.
	Since *time.Time `json:"since,omitempty"`
	// Until is when it turns itself off; nil means until it is turned off.
	Until *time.Time `json:"until,omitempty"`
	// Starts is set for a condition whose time has not come yet.
	Starts *time.Time `json:"starts,omitempty"`
}

// ReadyMade is one of the ready-made set-ups.
type ReadyMade struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	What string `json:"what"`
	Tool string `json:"tool"`
	// On is the id of a condition that is this set-up, when one is on.
	On int `json:"on,omitempty"`

	kind, which, family string
}

var readyMades = []ReadyMade{
	{Key: "slow", Name: "Slow answers", What: "Every request for vSAN performance data takes about 200 ms longer.", kind: scenario.CondSlow, which: scenario.WhichPerf},
	{Key: "error", Name: "One kind of data fails", What: "Requests for statistics about host network cards come back with an error.", kind: scenario.CondError, which: scenario.WhichOne, family: "vsan-pnic-net"},
	{Key: "missing", Name: "One kind of data does not exist", What: "The simulator says statistics about host network cards are not available.", kind: scenario.CondMissing, which: scenario.WhichOne, family: "vsan-pnic-net"},
	{Key: "timeout", Name: "No answer at all", What: "Requests for vSAN performance data are never answered.", kind: scenario.CondTimeout, which: scenario.WhichPerf},
	{Key: "perfdown", Name: "Performance service switched off", What: "One cluster behaves as if its vSAN performance service was never turned on.", kind: scenario.CondPerfDown},
}

// FaultsView is the Slowness and faults screen.
type FaultsView struct {
	On    []ConditionView `json:"on"`
	Ready []ReadyMade     `json:"ready"`
}

func (c *Controller) conditionViews() []ConditionView {
	sc := c.applied().sc
	now := c.now()
	out := []ConditionView{}
	for _, cond := range sc.Conditions {
		if cond.Ended(now) {
			continue
		}
		v := ConditionView{ID: cond.ID, Kind: cond.Kind, Text: c.condText(sc, cond), Since: cond.From, Until: cond.To}
		if !cond.Started(now) {
			v.Starts, v.Since = cond.From, nil
		}
		out = append(out, v)
	}
	return out
}

// Faults returns what is switched on and the ready-made set-ups.
func (c *Controller) Faults() FaultsView {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep()
	v := FaultsView{On: c.conditionViews()}
	now := c.now()
	for _, r := range readyMades {
		r.Tool = toolAdvice[r.kind]
		for _, cond := range c.applied().sc.Conditions {
			if cond.Kind == r.kind && cond.Which == r.which && cond.Family == r.family && !cond.Ended(now) {
				r.On = cond.ID
				break
			}
		}
		v.Ready = append(v.Ready, r)
	}
	return v
}

// ConditionOff turns one condition off at once (id), or every one (id 0),
// and returns the sentence that says what stopped.
func (c *Controller) ConditionOff(id int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep()
	r := c.applied()
	var keep, gone []scenario.Condition
	for _, cond := range r.sc.Conditions {
		if id == 0 || cond.ID == id {
			gone = append(gone, cond)
		} else {
			keep = append(keep, cond)
		}
	}
	if len(gone) == 0 {
		return "", invalid("That is not switched on any more. It may have turned itself off.")
	}
	next := clone(r.sc)
	next.Conditions = keep
	if _, err := c.applyScenario(next, false); err != nil {
		return "", err
	}
	now := c.now()
	for _, cond := range gone {
		c.ended = append(c.ended, EndedCondition{ID: cond.ID, Kind: cond.Kind, Text: c.condText(r.sc, cond), At: now})
		what := "A fault"
		if cond.Kind == scenario.CondSlow {
			what = "Slowness"
		}
		c.addHistory(fmt.Sprintf("%s was turned off. %s", what, offText(r.sc, cond)))
	}
	c.save()
	if id == 0 {
		return "**All turned off.** No slowness or faults are switched on now.", nil
	}
	rest := "No slowness or faults are switched on now."
	if n := len(keep); n > 0 {
		rest = bold(plural(n, "other thing is", "other things are") + " still switched on.")
	}
	return fmt.Sprintf("**Turned off.** %s %s", offText(r.sc, gone[0]), rest), nil
}
