package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/life"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// Activity: the simulated environment changes by itself. Package life
// decides what happens; package sim does it to the running vCenter; this
// file owns the settings, steps the engine on the clock, saves what has to
// survive a restart, and says it all to the console.
//
// Activity changes the vCenter in place, like slowness and faults: nothing
// is rebuilt and no tool is signed out. When something else does rebuild
// the vCenter, what activity has done to it (where VMs are, which are off,
// which hosts are in maintenance mode, how full the datastores are) is put
// back on the new one before any tool sees it.

// lifeEvery is how often the engine is stepped. It plays whole minutes, so
// this only bounds how late a minute starts.
const lifeEvery = 5 * time.Second

// lifeState is what life.json holds.
type lifeState struct {
	Engine *life.Engine `json:"engine"`
	Live   sim.Live     `json:"live"`
	// SetUp is false until activity has been started once.
	SetUp bool `json:"setUp"`
	// Seed is the environment all of this was done to.
	Seed string `json:"seed,omitempty"`
}

func (c *Controller) lifePath() string { return filepath.Join(c.opts.DataDir, "life.json") }

// initLife loads what activity was doing when the simulator last ran, puts
// it back on the vCenter that was just built, and starts the clock. Called
// from New before the vCenter serves anything.
func (c *Controller) initLife(r *running) {
	_, _, vms, _ := r.topo.Counts()
	c.life = life.New(r.sc.Seed, time.Local, life.Defaults(vms))
	var saved lifeState
	if b, err := os.ReadFile(c.lifePath()); err == nil {
		if err := json.Unmarshal(b, &saved); err != nil || saved.Engine == nil {
			log.Printf("control: %s could not be read, so activity starts with nothing set up: %v", c.lifePath(), err)
		} else {
			c.life.Settings, c.life.State, c.life.Log = saved.Engine.Settings, saved.Engine.State, saved.Engine.Log
			c.lifeSetUp = saved.SetUp
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("control: %s: %v", c.lifePath(), err)
	}
	if c.life.Settings.Global == nil {
		c.life.Settings = life.Defaults(vms)
	}
	switch s := &c.life.Settings; {
	case saved.Seed != "" && saved.Seed != r.sc.Seed:
		// another environment than the one this was done to: the dials are
		// kept, and nothing that was done is put on it
		saved.Live = sim.Live{}
		c.life.State = life.State{Seq: c.life.State.Seq}
		s.MinVMs, s.MaxVMs = life.Bounds(vms)
	case vms < s.MinVMs || vms > s.MaxVMs:
		// the scenario file changed the size of the environment
		s.MinVMs, s.MaxVMs = life.Bounds(vms)
		if c.lifeSetUp {
			c.life.Note(c.now(), fmt.Sprintf("The environment now has %s, outside the limits that were set, so the limits are %s to %s.", plural(vms, "VM"), num(s.MinVMs), num(s.MaxVMs)))
		}
	}
	c.wireLife(r, saved.Live)
	// nothing is caught up for the time the simulator was off
	c.life.Skip(c.now())
	c.lifeSeq = c.life.State.Seq
	stop := make(chan struct{})
	c.lifeStop = stop
	go func() {
		t := time.NewTicker(lifeEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				c.lifeStep()
			}
		}
	}()
}

// wireLife makes a freshly built vCenter the one activity acts on: what
// activity had done is put back, and the engine forgets hosts and VMs that
// are no longer there.
func (c *Controller) wireLife(r *running, live sim.Live) {
	w := r.sim.Life()
	w.Restore(live)
	w.VMAdded = func(cluster string, n int) { c.recordVM(cluster, n, true) }
	w.VMRemoved = func(cluster string, n int) { c.recordVM(cluster, n, false) }
	w.Now = c.now
	c.life.Seed = r.sc.Seed
	c.life.Reconcile(w)
	c.life.Load(w)
}

// carryLife moves activity from the vCenter being replaced to the new one.
// A new environment ("Replace everything") starts with nothing done to it:
// the settings are kept and the limits follow the new number of VMs.
func (c *Controller) carryLife(old, next *running, fresh bool) {
	if c.life == nil {
		return
	}
	var live sim.Live
	if !fresh {
		live = old.sim.Life().Snapshot()
	} else {
		_, _, vms, _ := next.topo.Counts()
		c.life.Settings.MinVMs, c.life.Settings.MaxVMs = life.Bounds(vms)
		c.life.State = life.State{At: c.life.State.At, Seq: c.life.State.Seq}
	}
	// a VM number a cluster has used stays used, even when the hosts that
	// gave it are gone, so Activity never adds a VM under an old number
	if !fresh && c.lifeSetUp {
		was := map[string]int{}
		for _, cl := range old.topo.Clusters() {
			was[cl.Key] = cl.VMHigh
		}
		for _, cl := range next.topo.Clusters() {
			if h := was[cl.Key]; h > cl.VMHigh {
				cl.VMHigh = h
				override(next.sc, cl.Key).VMHigh = h
			}
		}
	}
	// the owner's own change to clusters or hosts moves the limits by as many VMs as came or went
	if !fresh {
		_, _, before, _ := old.topo.Counts()
		_, _, after, _ := next.topo.Counts()
		c.life.Settings.MinVMs = max(0, c.life.Settings.MinVMs+after-before)
		c.life.Settings.MaxVMs = max(c.life.Settings.MinVMs, c.life.Settings.MaxVMs+after-before)
	}
	c.wireLife(next, live)
	c.lifeDirty = true
}

// keepActivityVMs carries the VMs Activity added and removed from the saved
// scenario into one read from a changed scenario file, for the clusters the
// file still has and says nothing about. The file wins for everything it
// says; what Activity did is not in it.
func keepActivityVMs(sc, saved *scenario.Scenario) {
	if sc.Seed != saved.Seed {
		return
	}
	has := map[string]bool{}
	for _, k := range sc.ClusterKeys() {
		has[k] = true
	}
	for _, was := range saved.Clusters {
		if !has[was.Key] || (len(was.AddedVMs) == 0 && len(was.RemovedVMs) == 0 && was.VMHigh == 0) {
			continue
		}
		if o := override(sc, was.Key); len(o.AddedVMs) == 0 && len(o.RemovedVMs) == 0 && o.VMHigh == 0 {
			o.AddedVMs = append([]int(nil), was.AddedVMs...)
			o.RemovedVMs = append([]int(nil), was.RemovedVMs...)
			o.VMHigh = was.VMHigh
		}
	}
}

// recordVM writes a VM that activity added or removed into the scenario,
// so that the next build makes the same VMs. The vCenter is not rebuilt:
// it already has the change. c.mu is held.
func (c *Controller) recordVM(cluster string, number int, added bool) {
	r := c.applied()
	sc := clone(r.sc)
	o := override(sc, cluster)
	if added {
		o.AddedVMs = append(o.AddedVMs, number)
		sort.Ints(o.AddedVMs)
	} else {
		o.RemovedVMs = append(o.RemovedVMs, number)
		sort.Ints(o.RemovedVMs)
	}
	c.cur.Store(&running{version: r.version, at: r.at, sc: sc, topo: r.topo, sim: r.sim})
	c.save()
}

// lifeStep plays the minutes that are due and saves when something
// happened.
func (c *Controller) lifeStep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.life == nil {
		return
	}
	r := c.applied()
	w := r.sim.Life()
	now := c.now()
	at := c.life.State.At
	if c.paused.Load() {
		// nothing happens while the simulator is paused, and nothing is caught up after
		c.life.Skip(now)
	} else {
		c.life.Step(now, w)
		// a minute that was played changes what must be saved even when it
		// writes no line: datastores fill, and every pace moves on
		if c.life.Settings.Running && c.life.State.At != at {
			c.lifeDirty = true
		}
	}
	w.Sweep(now)
	if c.life.State.Seq != c.lifeSeq {
		c.lifeSeq = c.life.State.Seq
		c.lifeDirty = true
	}
	c.saveLife(false)
}

// saveLife writes life.json when there is something new to write, at most
// every ten seconds unless forced. c.mu is held.
func (c *Controller) saveLife(force bool) {
	if c.life == nil || !c.lifeDirty || (!force && time.Since(c.lifeSaved) < 10*time.Second) {
		return
	}
	b, err := json.Marshal(lifeState{Engine: c.life, Live: c.applied().sim.Life().Snapshot(), SetUp: c.lifeSetUp, Seed: c.applied().sc.Seed})
	if err == nil {
		if err = os.MkdirAll(c.opts.DataDir, 0o700); err == nil {
			tmp := c.lifePath() + ".tmp"
			if err = os.WriteFile(tmp, append(b, '\n'), 0o600); err == nil {
				err = os.Rename(tmp, c.lifePath())
			}
		}
	}
	if err != nil {
		log.Printf("control: activity not saved; it carries on in memory: %v", err)
		return
	}
	c.lifeDirty, c.lifeSaved = false, time.Now()
}

// closeLife stops the clock and saves.
func (c *Controller) closeLife() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lifeStop != nil {
		close(c.lifeStop)
		c.lifeStop = nil
	}
	c.saveLife(true)
}

// ---------------------------------------------------------------------------
// what the console shows

// ActLevel is one position of a dial, with what it would mean.
type ActLevel struct {
	V     float64 `json:"v"`
	Label string  `json:"label"`
	Text  string  `json:"text"`
}

// ActRow is one behaviour on the Activity screen, for the vCenter or for
// one cluster.
type ActRow struct {
	Key   string      `json:"key"`
	Name  string      `json:"name"`
	What  string      `json:"what"`
	Text  string      `json:"text"`
	Level float64     `json:"level"`
	Exact *life.Exact `json:"exact,omitempty"`
	// Own: this cluster has its own setting. Follows: it follows the
	// vCenter. Neither is set for the vCenter's own row.
	Own     bool       `json:"own,omitempty"`
	Follows bool       `json:"follows,omitempty"`
	Levels  []ActLevel `json:"levels"`
	Units   []string   `json:"units,omitempty"`
	// Note says what stands in the way: waiting at a limit, or stopped
	// after failing.
	Note  string `json:"note,omitempty"`
	Held  bool   `json:"held,omitempty"`
	DoNow bool   `json:"doNow,omitempty"`
}

// ActCluster is one cluster in the "for which cluster" choice.
type ActCluster struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Hosts int    `json:"hosts"`
	VMs   int    `json:"vms"`
	Own   int    `json:"own"` // how many behaviours it has its own setting for
}

// ActLine is one line of "What has happened".
type ActLine struct {
	At      time.Time `json:"at"`
	Text    string    `json:"text"`
	Why     string    `json:"why"`
	You     bool      `json:"you,omitempty"`
	Bad     bool      `json:"bad,omitempty"`
	Details []string  `json:"details,omitempty"`
}

// ActivityView is the Activity screen.
type ActivityView struct {
	SetUp    bool         `json:"setUp"`
	Running  bool         `json:"running"`
	Paused   bool         `json:"paused"`
	Scope    string       `json:"scope"`
	Hosts    int          `json:"hosts"`
	VMs      int          `json:"vms"`
	Built    int          `json:"built"`
	MinVMs   int          `json:"minVMs"`
	MaxVMs   int          `json:"maxVMs"`
	Last     string       `json:"last,omitempty"`
	Now      string       `json:"now,omitempty"`
	Day      string       `json:"day"`
	Rows     []ActRow     `json:"rows"`
	Clusters []ActCluster `json:"clusters"`
	Hour     string       `json:"hour"`
	Today    string       `json:"today"`
	HourYou  int          `json:"hourYou"`
	Lines    []ActLine    `json:"lines"`
	More     int          `json:"more"`
}

var levelLabels = map[float64]string{0: "Off", 0.25: "Much quieter", 0.5: "Quieter", 1: "Baseline", 2: "Busier", 4: "Much busier"}
var loadLabels = map[float64]string{0.5: "Half", 0.75: "Lighter", 1: "As built", 1.25: "Heavier", 1.5: "Much heavier", 2: "Double"}

var lifeWhat = map[string]string{
	life.Moves:       "As DRS does: when the load shifts, a few VMs move to other hosts by vMotion.",
	life.Power:       "People shut VMs down and start them again, mostly while they are at work.",
	life.Adds:        "New VMs are provisioned, mostly while people are at work.",
	life.Removes:     "Old VMs are decommissioned, mostly while people are at work.",
	life.Maintenance: "A share of the hosts is in maintenance mode. A host's VMs move off it first, and other hosts take their turn.",
	life.DiskGrowth:  "The vSAN datastores fill a little every half hour.",
	life.DiskShrink:  "A cleanup at 2 AM frees space on the vSAN datastores.",
	life.CPU:         "How busy the VMs' CPUs look in the performance numbers, and how much they contend.",
	life.Memory:      "How much memory the VMs use in the performance numbers.",
}

// lifeScope is the numbers a sentence needs: the VMs, hosts and vSAN
// capacity a dial covers, and what share of the vCenter's own exact number
// falls to it.
type lifeScope struct {
	vms, hosts int
	capacity   int64
	share      float64
}

func every(n float64, unit string) string {
	one := map[string]string{life.UnitMinutes: "minute", life.UnitHours: "hour", life.UnitDays: "day"}[unit]
	if n == 1 {
		return "every " + one
	}
	return fmt.Sprintf("every %s %ss", num(int(n)), one)
}

func about(n float64) string {
	switch {
	case n >= 100:
		return num(int(math.Round(n/10) * 10))
	case n >= 10 || n == math.Trunc(n):
		return num(int(math.Round(n)))
	case n >= 1:
		return fmt.Sprintf("%.1f", n)
	}
	return "less than 1"
}

// plainNumber is a number to two decimal places without trailing zeros:
// 1.25, 1.5, 2.
func plainNumber(n float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", n), "0"), ".")
}

// lifeText says what a dial means for a scope, in one sentence.
func lifeText(b string, d life.Dial, s lifeScope) string {
	off := d.Exact == nil && d.Level <= 0
	perDay := func() float64 {
		if d.Exact != nil {
			return d.Exact.N * s.share
		}
		return life.Baseline.Per100[b] * float64(s.vms) / 100 * d.Level
	}
	rhythm := d.Exact != nil && d.Exact.Unit != life.UnitPerDay
	switch b {
	case life.Moves:
		if off {
			return "Off: no VM is moved to balance the load."
		}
		return fmt.Sprintf("DRS moves VMs between hosts to balance the load: about %s vMotions a day, in bursts.", about(perDay()))
	case life.Power:
		switch {
		case off:
			return "Off: no VM is powered off."
		case rhythm:
			return fmt.Sprintf("A VM is powered off %s, day and night, and powered on again a little later.", every(d.Exact.N, d.Exact.Unit))
		}
		return fmt.Sprintf("VMs are powered off and on again: about %s a day, mostly in working hours.", about(perDay()))
	case life.Adds:
		switch {
		case off:
			return "Off: no VM is added."
		case rhythm:
			return fmt.Sprintf("A new VM is added %s, day and night.", every(d.Exact.N, d.Exact.Unit))
		}
		return fmt.Sprintf("New VMs are added: about %s a day, mostly in working hours.", about(perDay()))
	case life.Removes:
		switch {
		case off:
			return "Off: no VM is removed."
		case rhythm:
			return fmt.Sprintf("A VM is removed %s, day and night.", every(d.Exact.N, d.Exact.Unit))
		}
		return fmt.Sprintf("VMs are removed: about %s a day, mostly in working hours.", about(perDay()))
	case life.Maintenance:
		if off {
			return "Off: no host is kept in maintenance mode."
		}
		pct, days := life.Baseline.MaintPercent*d.Level, 7/math.Max(0.25, d.Level)
		if d.Exact != nil {
			pct, days = d.Exact.N, 7
		}
		n := max(1, int(math.Round(pct/100*float64(s.hosts))))
		swap := fmt.Sprintf("about every %s days", about(days))
		if days <= 1.5 {
			swap = "about every day"
		}
		return fmt.Sprintf("%s%% of hosts are kept in maintenance mode (%s of %s), and others take their place %s.", about(pct), num(n), num(s.hosts), swap)
	case life.DiskGrowth, life.DiskShrink:
		base := life.Baseline.GrowPercent
		if b == life.DiskShrink {
			base = life.Baseline.ShrinkPct
		}
		pct := base * d.Level
		if d.Exact != nil {
			pct = d.Exact.N
		}
		gb := float64(s.capacity) * pct / 100 / (1 << 30)
		if b == life.DiskGrowth {
			if off {
				return "Off: the datastores do not fill."
			}
			return fmt.Sprintf("The vSAN datastores fill by %s%% of their capacity a day (about %s GB).", plainNumber(pct), about(gb))
		}
		if off {
			return "Off: nothing is cleaned up."
		}
		return fmt.Sprintf("A cleanup at 2 AM frees %s%% of vSAN datastore capacity (about %s GB).", plainNumber(pct), about(gb))
	case life.CPU:
		switch {
		case d.Level == 1:
			return "CPU use is as the VMs were built."
		case d.Level > 1:
			return fmt.Sprintf("CPU use is about %s times what the VMs were built with, and they contend for CPU more.", plainNumber(d.Level))
		}
		return fmt.Sprintf("CPU use is about %s%% of what the VMs were built with.", about(d.Level*100))
	default:
		switch {
		case d.Level == 1:
			return "Memory use is as the VMs were built."
		case d.Level > 1:
			return fmt.Sprintf("Memory use is about %s times what the VMs were built with, up to what each VM has.", plainNumber(d.Level))
		}
		return fmt.Sprintf("Memory use is about %s%% of what the VMs were built with.", about(d.Level*100))
	}
}

// lifeScopes works out the numbers of the vCenter and of each cluster.
func (c *Controller) lifeScopes(r *running) (all lifeScope, by map[string]lifeScope, cs []life.Cluster) {
	cs = r.sim.Life().Clusters()
	by = map[string]lifeScope{}
	for _, cl := range r.topo.Clusters() {
		by[cl.Key] = lifeScope{capacity: topologyCapacity(cl), share: 1}
	}
	for _, cl := range cs {
		s := by[cl.Key]
		s.vms, s.hosts = cl.VMs, len(cl.Hosts)
		by[cl.Key] = s
		all.vms += cl.VMs
		all.hosts += len(cl.Hosts)
		all.capacity += s.capacity
	}
	all.share = 1
	return
}

// Activity is the Activity screen for the vCenter (scope "") or a cluster.
func (c *Controller) Activity(scope string) (*ActivityView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.applied()
	all, by, cs := c.lifeScopes(r)
	e := c.life
	v := &ActivityView{SetUp: c.lifeSetUp, Running: e.Settings.Running, Paused: c.paused.Load(), Scope: scope, Hosts: all.hosts, VMs: all.vms,
		MinVMs: e.Settings.MinVMs, MaxVMs: e.Settings.MaxVMs, Lines: []ActLine{}, Clusters: []ActCluster{}}
	_, _, v.Built, _ = r.topo.Counts()
	sc := all
	if scope != "" {
		s, ok := by[scope]
		if !ok {
			return nil, &Invalid{"There is no cluster " + scope + "."}
		}
		sc = s
	}
	for _, cl := range cs {
		v.Clusters = append(v.Clusters, ActCluster{Key: cl.Key, Label: clusterLabel(r.sc, cl.Key), Hosts: len(cl.Hosts), VMs: cl.VMs, Own: len(e.Settings.Clusters[cl.Key])})
	}
	for _, b := range life.Order {
		d := e.Settings.Global[b]
		row := ActRow{Key: b, Name: life.Names[b], What: lifeWhat[b], Units: life.ExactUnits(b)}
		rowScope := sc
		if scope != "" {
			own, has := e.Settings.Clusters[scope][b]
			row.Own, row.Follows = has, !has
			if has {
				d = own
			} else if d.Exact != nil && d.Exact.Unit != life.UnitPercent {
				// the vCenter's own exact number: this cluster's share of it
				var follow int
				for _, cl := range cs {
					if _, own := e.Settings.Clusters[cl.Key][b]; !own {
						follow += cl.VMs
					}
				}
				if follow > 0 {
					rowScope.share = float64(sc.vms) / float64(follow)
				}
			}
		}
		row.Level, row.Exact = d.Level, d.Exact
		row.Text = lifeText(b, d, rowScope)
		if row.Exact != nil && rowScope.share != 1 && row.Exact.Unit != life.UnitPerDay {
			row.Text = "Follows the vCenter's own number: " + lowerWord(lifeText(b, e.Settings.Global[b], all))
		}
		levels, labels := life.Levels, levelLabels
		if life.IsLoad(b) {
			levels, labels = life.LoadLevels, loadLabels
		}
		for _, lv := range levels {
			row.Levels = append(row.Levels, ActLevel{V: lv, Label: labels[lv], Text: lifeText(b, life.Dial{Level: lv}, sc)})
		}
		counted := b == life.Moves || b == life.Power || b == life.Adds || b == life.Removes
		switch {
		case e.State.Held[b] != 0:
			row.Held = true
			row.Note = fmt.Sprintf("Stopped itself at %s after failing 3 times in a row. Everything else carries on.", timeToken(time.Unix(e.State.Held[b]*60, 0)))
		case (b == life.Adds || b == life.Removes) && e.Settings.Running && e.AtLimit(cs, b) && !(d.Exact == nil && d.Level == 0):
			if b == life.Adds {
				row.Note = fmt.Sprintf("Waiting: the environment has the most VMs it may have (%s). A VM is added only when one has been removed.", num(e.Settings.MaxVMs))
			} else {
				row.Note = fmt.Sprintf("Waiting: the environment has the fewest VMs it may have (%s). A VM is removed only when one has been added.", num(e.Settings.MinVMs))
			}
		}
		row.DoNow = counted && scope != "" && e.Settings.Running && !v.Paused && !row.Held
		v.Rows = append(v.Rows, row)
	}
	v.Day = c.lifeDay(cs, all)
	v.Now = lifeNow(cs)
	now := c.now()
	for i := len(e.Log) - 1; i >= 0; i-- {
		if e.Log[i].Kind != life.KindNote {
			v.Last = fmt.Sprintf("%s, %s", timeToken(e.Log[i].At), lowerWord(e.Log[i].Text))
			break
		}
	}
	var hour, today []life.Entry
	y, m, d := now.In(time.Local).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	for _, en := range e.Log {
		if en.At.After(now.Add(-time.Hour)) {
			hour = append(hour, en)
		}
		if !en.At.Before(midnight) {
			today = append(today, en)
		}
	}
	v.Hour, v.Today = lifeSum(hour), lifeSum(today)
	for _, en := range hour {
		if en.You && en.Kind != life.KindNote {
			v.HourYou++
		}
	}
	for i := len(e.Log) - 1; i >= 0 && len(v.Lines) < 8; i-- {
		v.Lines = append(v.Lines, lifeLine(e.Log[i]))
	}
	v.More = max(0, len(hour)-len(v.Lines))
	return v, nil
}

func lifeLine(en life.Entry) ActLine {
	why := life.Names[en.Why]
	if why == "" {
		why = "You"
	}
	return ActLine{At: en.At, Text: en.Text, Why: why, You: en.You, Bad: en.Kind == life.KindFail, Details: en.Details}
}

// lifeNow says what is true at the moment because of activity.
func lifeNow(cs []life.Cluster) string {
	var maint, off int
	for _, c := range cs {
		off += c.Off
		for _, h := range c.Hosts {
			if h.Maintenance {
				maint++
			}
		}
	}
	var a []string
	if maint > 0 {
		a = append(a, plural(maint, "host is", "hosts are")+" in maintenance mode")
	}
	if off > 0 {
		a = append(a, plural(off, "VM is", "VMs are")+" powered off")
	}
	if len(a) == 0 {
		return ""
	}
	return "At the moment " + listAnd(a) + "."
}

// lifeDay adds up what a working day comes to with the dials as they are.
func (c *Controller) lifeDay(cs []life.Cluster, all lifeScope) string {
	e := c.life
	var parts []string
	total := 0.0
	for _, b := range []string{life.Moves, life.Power, life.Adds, life.Removes} {
		var n float64
		if g := e.Settings.Global[b]; g.Exact != nil {
			n += g.Exact.N
			if g.Exact.Unit != life.UnitPerDay {
				n = map[string]float64{life.UnitMinutes: 1440, life.UnitHours: 24, life.UnitDays: 1}[g.Exact.Unit] / g.Exact.N
			}
		}
		for _, cl := range cs {
			if d, own := e.Settings.Dial(cl.Key, b); own || d.Exact == nil {
				n += e.PerDay(cs, cl, b)
			}
		}
		if n <= 0 {
			continue
		}
		total += n
		parts = append(parts, about(n)+" "+map[string]string{life.Moves: "vMotions", life.Power: "VMs powered off and on", life.Adds: "VMs added", life.Removes: "VMs removed"}[b])
	}
	if len(parts) == 0 {
		return "With the dials as they are, no VM moves, is powered off, added or removed by itself."
	}
	return fmt.Sprintf("On a working day at this size (%s and %s) that comes to about %s: about %s changes for a monitoring tool to see, each of them a few tasks and events in vCenter.",
		plural(all.hosts, "host"), plural(all.vms, "VM"), listAnd(parts), about(total))
}

// lifeSum says what a list of lines adds up to.
func lifeSum(list []life.Entry) string {
	n := map[string]int{}
	for _, en := range list {
		for k, v := range en.Counts {
			n[k] += v
		}
	}
	var a []string
	pair := func(x, y int, one, first, second, short string) {
		switch {
		case x > 0 && y > 0:
			a = append(a, fmt.Sprintf("%s %s, %s %s", plural(x, one), first, num(y), short))
		case x > 0:
			a = append(a, plural(x, one)+" "+first)
		case y > 0:
			a = append(a, plural(y, one)+" "+second)
		}
	}
	if n["move"] > 0 {
		a = append(a, plural(n["move"], "vMotion"))
	}
	pair(n["off"], n["on"], "VM", "powered off", "powered on again", "powered on again")
	pair(n["add"], n["remove"], "VM", "added", "removed", "removed")
	pair(n["min"], n["mout"], "host", "into maintenance mode", "out of maintenance mode", "out of it")
	if n["grownGB"] > 0 || n["freedGB"] > 0 {
		a = append(a, fmt.Sprintf("%s GB filled and %s GB freed on datastores", num(n["grownGB"]), num(n["freedGB"])))
	}
	if n["wait"] > 0 {
		a = append(a, plural(n["wait"], "time", "times")+" something had to wait")
	}
	if n["fail"] > 0 {
		a = append(a, plural(n["fail"], "failure"))
	}
	return strings.Join(a, ", ")
}

// ActivityLogView is the full list of what has happened.
type ActivityLogView struct {
	Range string    `json:"range"`
	Show  string    `json:"show"`
	Count int       `json:"count"`
	Sum   string    `json:"sum"`
	You   int       `json:"you"`
	Lines []ActLine `json:"lines"`
	Page  int       `json:"page"`
	Pages int       `json:"pages"`
	Full  bool      `json:"full"`
	First time.Time `json:"first"`
}

var lifeGroups = map[string][]string{
	"moves": {life.KindMove}, "power": {life.KindOff, life.KindOn}, "vms": {life.KindAdd, life.KindRemove},
	"hosts": {life.KindIn, life.KindOut}, "disks": {life.KindDisk}, "other": {life.KindWait, life.KindFail},
}

// ActivityLog lists what has happened: in the last hour, today, the last
// 24 hours or the last 7 days, all of it or one kind, 25 lines to a page.
func (c *Controller) ActivityLog(rng, show string, page int) *ActivityLogView {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	from := now.Add(-time.Hour)
	switch rng {
	case "today":
		y, m, d := now.In(time.Local).Date()
		from = time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	case "day":
		from = now.Add(-24 * time.Hour)
	case "week":
		from = now.Add(-7 * 24 * time.Hour)
	default:
		rng = "hour"
	}
	kinds := lifeGroups[show]
	if kinds == nil {
		show = "all"
	}
	var list []life.Entry
	for _, en := range c.life.Log {
		if !en.At.After(from) {
			continue
		}
		if kinds != nil && !strings.Contains(" "+strings.Join(kinds, " ")+" ", " "+en.Kind+" ") {
			continue
		}
		list = append(list, en)
	}
	v := &ActivityLogView{Range: rng, Show: show, Count: len(list), Sum: lifeSum(list), Full: len(c.life.Log) >= life.LogSize && rng == "week", Lines: []ActLine{}}
	if len(c.life.Log) > 0 {
		v.First = c.life.Log[0].At
	}
	for _, en := range list {
		if en.You && en.Kind != life.KindNote {
			v.You++
		}
	}
	const per = 25
	v.Pages = max(1, (len(list)+per-1)/per)
	v.Page = min(max(1, page), v.Pages)
	for i := len(list) - 1 - (v.Page-1)*per; i >= 0 && len(v.Lines) < per; i-- {
		v.Lines = append(v.Lines, lifeLine(list[i]))
	}
	return v
}

// HomeActivity is what Home and the tab badge say about activity.
type HomeActivity struct {
	State string `json:"state"` // none, running, stopped, waiting
	Line  string `json:"line"`
	Held  int    `json:"held"`
}

// homeActivity is called with c.mu held.
func (c *Controller) homeActivity(r *running) HomeActivity {
	e := c.life
	if e == nil || !c.lifeSetUp {
		return HomeActivity{State: "none", Line: "Nothing changes by itself."}
	}
	h := HomeActivity{Held: len(e.State.Held)}
	switch {
	case !e.Settings.Running:
		h.State, h.Line = "stopped", "Activity is stopped, so nothing changes by itself."
	case c.paused.Load():
		h.State, h.Line = "waiting", "Activity is waiting while the simulator is paused."
	default:
		h.State = "running"
		var hour []life.Entry
		for _, en := range e.Log {
			if en.At.After(c.now().Add(-time.Hour)) {
				hour = append(hour, en)
			}
		}
		sum := lifeSum(hour)
		if sum == "" {
			sum = "nothing happened"
		}
		h.Line = strings.TrimSpace(fmt.Sprintf("**Changing by itself.** In the last hour: %s. %s", sum, lifeNow(r.sim.Life().Clusters())))
	}
	return h
}

// ---------------------------------------------------------------------------
// what the owner does

// ActDone is what an activity change did, or the box to show before it is
// made.
type ActDone struct {
	Done    string   `json:"done,omitempty"`
	Tone    string   `json:"tone,omitempty"`
	Confirm *Confirm `json:"confirm,omitempty"`
}

func (c *Controller) lifeWorld() *sim.Life { return c.applied().sim.Life() }

func topologyCapacity(cl *topology.Cluster) int64 { return topology.VsanCapacity(cl) }

// maintChange says how many hosts would go into or come out of maintenance
// mode straight away if the settings were s.
func (c *Controller) maintChange(s life.Settings) (in, out int) {
	cs := c.lifeWorld().Clusters()
	probe := life.New(c.life.Seed, time.Local, s)
	targets := probe.MaintTargets(c.now().Unix()/60, cs)
	for _, cl := range cs {
		have := 0
		for _, h := range cl.Hosts {
			if h.Maintenance {
				have++
			}
		}
		if d := targets[cl.Key] - have; d > 0 {
			in += d
		} else {
			out -= d
		}
	}
	return
}

func cloneSettings(s life.Settings) life.Settings {
	out := s
	out.Global = map[string]life.Dial{}
	for k, v := range s.Global {
		out.Global[k] = v
	}
	out.Clusters = map[string]map[string]life.Dial{}
	for k, m := range s.Clusters {
		out.Clusters[k] = map[string]life.Dial{}
		for b, v := range m {
			out.Clusters[k][b] = v
		}
	}
	return out
}

func hostsMove(in, out, per int) string {
	var a []string
	if in > 0 {
		a = append(a, fmt.Sprintf("%s into maintenance mode straight away, and about %s VMs move off %s first", plural(in, "host goes", "hosts go"), num(per), map[bool]string{true: "it", false: "each one"}[in == 1]))
	}
	if out > 0 {
		a = append(a, fmt.Sprintf("%s out of maintenance mode straight away, and VMs move back onto %s", plural(out, "host comes", "hosts come"), map[bool]string{true: "it", false: "them"}[out == 1]))
	}
	return upper1(strings.Join(a, "; ")) + "."
}

// ActivityStart starts activity: the first time with every behaviour at
// its baseline, afterwards with the dials as they were left.
func (c *Controller) ActivityStart(confirmed bool) (*ActDone, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.life
	if e.Settings.Running {
		return &ActDone{Done: "Activity is already running.", Tone: "ok"}, nil
	}
	if c.paused.Load() {
		return nil, &Invalid{"The simulator is paused, so nothing can move. Resume it first."}
	}
	r := c.applied()
	all, _, cs := c.lifeScopes(r)
	if !confirmed {
		s := cloneSettings(e.Settings)
		s.Running = true
		in, _ := c.maintChange(s)
		title, lead := "Start activity?", "The environment starts changing by itself, in amounts that fit its size. Every dial starts at its baseline; you can turn each one up or down afterwards, for the whole vCenter or for one cluster."
		if c.lifeSetUp {
			title, lead = "Start activity again?", "Things start happening by themselves again, with the dials as you left them."
		}
		body := []string{lead, c.lifeDay(cs, all)}
		if in > 0 {
			body = append(body, hostsMove(in, 0, all.vms/max(1, all.hosts)))
		}
		body = append(body, fmt.Sprintf("The number of VMs stays between %s and %s. Tools stay connected. One button stops all of it.", num(e.Settings.MinVMs), num(e.Settings.MaxVMs)))
		return &ActDone{Confirm: &Confirm{Title: title, Body: body, OK: map[bool]string{true: "Start activity again", false: "Start activity"}[c.lifeSetUp]}}, nil
	}
	e.Start(c.now(), r.sim.Life())
	c.lifeSetUp, c.lifeDirty = true, true
	c.addHistory("Activity started.")
	c.save()
	c.saveLife(true)
	return &ActDone{Done: strings.TrimSpace("**Started.** The environment now changes by itself. " + lifeNow(r.sim.Life().Clusters())), Tone: "ok"}, nil
}

// ActivityStop stops activity and leaves the environment still and
// healthy.
func (c *Controller) ActivityStop(confirmed bool) (*ActDone, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.life
	if !e.Settings.Running {
		return &ActDone{Done: "Activity is already stopped.", Tone: "ok"}, nil
	}
	if c.paused.Load() {
		return nil, &Invalid{"The simulator is paused, so nothing can move. Resume it first."}
	}
	r := c.applied()
	if !confirmed {
		body := []string{"From the moment you press the button, nothing happens by itself."}
		if n := len(e.State.Maint); n > 0 {
			body = append(body, fmt.Sprintf("%s out of maintenance mode, and VMs move back onto %s.", plural(n, "host comes", "hosts come"), map[bool]string{true: "it", false: "them"}[n == 1]))
		}
		if n := len(e.State.Off); n > 0 {
			body = append(body, fmt.Sprintf("%s powered off %s powered on again.", plural(n, "VM that is", "VMs that are"), map[bool]string{true: "is", false: "are"}[n == 1]))
		}
		vms := 0
		for _, cl := range r.sim.Life().Clusters() {
			vms += cl.VMs
		}
		body = append(body, fmt.Sprintf("So the environment ends up still and healthy, with %s. VMs that were added or removed stay that way, and the datastores stay as full as they are: a removed VM cannot come back, because its name and identity are never used again.", plural(vms, "VM")),
			"The dials are kept as you set them, so you can start again with one button. Tools stay connected.")
		return &ActDone{Confirm: &Confirm{Title: "Stop all activity?", Body: body, OK: "Stop all activity"}}, nil
	}
	e.Stop(c.now(), r.sim.Life())
	c.lifeDirty = true
	c.addHistory("Activity stopped.")
	c.save()
	c.saveLife(true)
	return &ActDone{Done: "**Stopped.** Nothing happens by itself now. The environment is still, and the dials are kept as you set them.", Tone: "ok"}, nil
}

// ActDial is a change to one dial: a level, an exact number, or (for a
// cluster) back to following the vCenter.
type ActDial struct {
	Cluster   string   `json:"cluster"`
	Behaviour string   `json:"behaviour"`
	Level     *float64 `json:"level"`
	N         *float64 `json:"n"`
	Unit      string   `json:"unit"`
	Follow    bool     `json:"follow"`
	Confirmed bool     `json:"confirmed"`
}

// ActivityDial changes one dial. A change that moves hosts into or out of
// maintenance mode straight away says so first.
func (c *Controller) ActivityDial(req ActDial) (*ActDone, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.life
	r := c.applied()
	all, by, _ := c.lifeScopes(r)
	b := req.Behaviour
	if life.Names[b] == "" {
		return nil, &Invalid{"That is not something activity does."}
	}
	if b == life.Maintenance && e.Settings.Running && c.paused.Load() {
		return nil, &Invalid{"The simulator is paused, so no host can move. Resume it first."}
	}
	sc := all
	if req.Cluster != "" {
		s, ok := by[req.Cluster]
		if !ok {
			return nil, &Invalid{"There is no cluster " + req.Cluster + "."}
		}
		sc = s
	}
	next := cloneSettings(e.Settings)
	var d life.Dial
	switch {
	case req.Follow:
		if req.Cluster == "" {
			return nil, &Invalid{"Only a cluster can follow the vCenter."}
		}
		delete(next.Clusters[req.Cluster], b)
		if len(next.Clusters[req.Cluster]) == 0 {
			delete(next.Clusters, req.Cluster)
		}
		d = next.Global[b]
	case req.N != nil:
		d = life.Dial{Level: 1, Exact: &life.Exact{N: *req.N, Unit: req.Unit}}
	case req.Level != nil:
		d = life.Dial{Level: *req.Level}
	default:
		return nil, &Invalid{"Choose a level or type a number."}
	}
	if err := d.Check(b); err != nil {
		return nil, &Invalid{upper1(err.Error()) + "."}
	}
	if !req.Follow && b == life.Moves && d.Exact != nil && d.Exact.N > float64(sc.vms) {
		return nil, &Invalid{fmt.Sprintf("The most is %s vMotions a day: one for every VM.", num(sc.vms))}
	}
	if !req.Follow {
		if req.Cluster == "" {
			next.Global[b] = d
		} else {
			if next.Clusters[req.Cluster] == nil {
				next.Clusters[req.Cluster] = map[string]life.Dial{}
			}
			next.Clusters[req.Cluster][b] = d
		}
	}
	where := "the whole vCenter"
	if req.Cluster != "" {
		where = clusterLabel(r.sc, req.Cluster)
	}
	text := lifeText(b, d, sc)
	if req.Follow {
		text = "It follows the vCenter again. " + lifeText(b, d, sc)
	}
	if b == life.Maintenance && e.Settings.Running && !req.Confirmed {
		if in, out := c.maintChange(next); in+out > 0 {
			return &ActDone{Confirm: &Confirm{Title: "Change the hosts in maintenance mode?", Body: []string{bold(text), hostsMove(in, out, sc.vms/max(1, sc.hosts)), "Tools stay connected."}, OK: "Change it"}}, nil
		}
	}
	e.Apply(c.now(), next, r.sim.Life())
	e.Note(c.now(), fmt.Sprintf("You changed %s for %s. %s", lowerWord(life.Names[b]), where, text))
	c.lifeDirty = true
	c.addHistory(fmt.Sprintf("Activity: %s for %s. %s", lowerWord(life.Names[b]), where, text))
	c.save()
	c.saveLife(true)
	return &ActDone{Done: "**Done.** " + text, Tone: "ok"}, nil
}

// ActivityLimits changes the fewest and the most VMs the vCenter may have.
func (c *Controller) ActivityLimits(lo, hi int) (*ActDone, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	vms := 0
	for _, cl := range c.lifeWorld().Clusters() {
		vms += cl.VMs
	}
	switch {
	case lo < 0 || lo > vms:
		return nil, &Invalid{fmt.Sprintf("Fewest VMs: type a number from 0 to %s, the number there are now.", num(vms))}
	case hi < vms:
		return nil, &Invalid{fmt.Sprintf("Most VMs: type %s or more, the number there are now.", num(vms))}
	case hi > 100000:
		return nil, &Invalid{"Most VMs: the most the simulator allows here is 100,000."}
	}
	c.life.Settings.MinVMs, c.life.Settings.MaxVMs = lo, hi
	delete(c.life.State.Waited, life.Adds)
	delete(c.life.State.Waited, life.Removes)
	text := fmt.Sprintf("The number of VMs now stays between %s and %s.", num(lo), num(hi))
	c.life.Note(c.now(), "You changed the limits. "+text)
	c.lifeDirty = true
	c.addHistory("Activity: " + lowerWord(text))
	c.save()
	c.saveLife(true)
	return &ActDone{Done: "**Done.** " + text, Tone: "ok"}, nil
}

// ActivityDoNow makes one occurrence of a behaviour happen in a cluster
// straight away. It says what it will do first.
func (c *Controller) ActivityDoNow(cluster, b string, confirmed bool) (*ActDone, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.applied()
	if _, ok := map[string]bool{life.Moves: true, life.Power: true, life.Adds: true, life.Removes: true}[b]; !ok {
		return nil, &Invalid{"That cannot be done on its own."}
	}
	label := clusterLabel(r.sc, cluster)
	if r.sim.Cluster(cluster) == nil {
		return nil, &Invalid{"There is no cluster " + cluster + "."}
	}
	if !c.life.Settings.Running || c.paused.Load() {
		return nil, &Invalid{"Activity is not running, so nothing can be done now. Start it first."}
	}
	if !confirmed {
		what := map[string][2]string{
			life.Moves:   {"Move some VMs now?", "DRS balances the load in " + label + " straight away: 2 to 6 of its VMs move by vMotion from its fullest hosts to its emptiest."},
			life.Power:   {"Power a VM off now?", "One VM in " + label + " is powered off straight away, and powered on again between 5 minutes and 2 hours later."},
			life.Adds:    {"Add a VM now?", "One new VM is added to " + label + " straight away, on its emptiest host."},
			life.Removes: {"Remove a VM now?", "One VM in " + label + " is powered off and removed straight away. It is gone for good: its name and identity are never used again."},
		}[b]
		return &ActDone{Confirm: &Confirm{Title: what[0], Body: []string{what[1], "This is once, on top of what happens by itself."}, OK: "Do it now"}}, nil
	}
	before := c.life.State.Seq
	did := c.life.DoNow(c.now(), r.sim.Life(), cluster, b)
	c.lifeDirty = true
	c.saveLife(true)
	if !did || c.life.State.Seq == before {
		why := "There is nothing it can be done to at the moment."
		if cs := r.sim.Life().Clusters(); c.life.AtLimit(cs, b) {
			why = "The environment is at its limit on the number of VMs. Change the limits first."
		}
		return &ActDone{Done: "**Not done.** " + why, Tone: "warn"}, nil
	}
	last := c.life.Log[len(c.life.Log)-1]
	tone := "ok"
	if last.Kind == life.KindFail {
		tone = "warn"
	}
	return &ActDone{Done: "**Done, once, just now.** " + last.Text, Tone: tone}, nil
}

// ActivityResume lets a behaviour that stopped itself happen again.
func (c *Controller) ActivityResume(b string) (*ActDone, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if life.Names[b] == "" || c.life.State.Held[b] == 0 {
		return nil, &Invalid{"That has not stopped itself."}
	}
	c.life.Resume(b)
	c.life.Note(c.now(), fmt.Sprintf("You switched %s on again.", lowerWord(life.Names[b])))
	c.lifeDirty = true
	c.saveLife(true)
	return &ActDone{Done: fmt.Sprintf("**Switched on again.** %s happens again from now on.", life.Names[b]), Tone: "ok"}, nil
}

// lowerWord is lower1 for a name that may begin with an abbreviation: "New
// VMs" becomes "new VMs", and "VMs removed" and "CPU load" stay as they are.
func lowerWord(s string) string {
	if len(s) > 1 && s[1] >= 'A' && s[1] <= 'Z' {
		return s
	}
	return lower1(s)
}
