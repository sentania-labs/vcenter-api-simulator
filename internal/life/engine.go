package life

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Entry is one line of "What has happened": a cause together with what it
// set off. Counts say what it added up to (VM moves, VMs powered off, and
// so on) and Details name each VM it touched.
type Entry struct {
	ID      int64          `json:"id"`
	At      time.Time      `json:"at"`
	Kind    string         `json:"kind"`
	Cluster string         `json:"cluster,omitempty"`
	Text    string         `json:"text"`
	Why     string         `json:"why"` // the behaviour, or "you" for a line that only says what the owner did
	You     bool           `json:"you,omitempty"`
	Counts  map[string]int `json:"n,omitempty"`
	Details []string       `json:"details,omitempty"`
}

// Kinds of entry.
const (
	KindMove   = "move"   // a load-balancing burst
	KindOff    = "off"    // a VM powered off
	KindOn     = "on"     // a VM powered on again
	KindAdd    = "add"    // a VM added
	KindRemove = "remove" // a VM removed
	KindIn     = "min"    // a host into maintenance mode, with its evacuation
	KindOut    = "mout"   // a host out of maintenance mode, with the moves back
	KindDisk   = "disk"   // a day of datastore growth and the cleanup
	KindWait   = "wait"   // something had to wait at a limit
	KindFail   = "fail"   // something failed
	KindNote   = "note"   // what the owner did; counts nothing
)

// LogSize is how many lines are remembered.
const LogSize = 1000

// OffVM is a VM the engine powered off, and the minute it is due back on.
type OffVM struct {
	Cluster string `json:"cluster"`
	VM      string `json:"vm"`
	On      int64  `json:"on"`
}

// MaintHost is a host the engine put in maintenance mode, since when, and
// the minute another host takes its place.
type MaintHost struct {
	Cluster string `json:"cluster"`
	Host    string `json:"host"`
	Since   int64  `json:"since"`
	Until   int64  `json:"until"`
}

// State is what the engine has to remember between steps and across a
// restart. Minutes are Unix minutes.
type State struct {
	At     int64              `json:"at"`               // the last minute stepped
	Acc    map[string]float64 `json:"acc,omitempty"`    // how far each cluster's behaviour is towards its next occurrence
	K      map[string]uint64  `json:"k,omitempty"`      // how many occurrences each has had, which numbers its picks
	Off    []OffVM            `json:"off,omitempty"`    // VMs powered off
	Maint  []MaintHost        `json:"maint,omitempty"`  // hosts in maintenance mode
	Rested map[string]string  `json:"rested,omitempty"` // by cluster, the host that last came out, which is not the next to go in
	Waited map[string]int64   `json:"waited,omitempty"` // by behaviour, when a "waiting at the limit" line was last written
	Grown  map[string]int64   `json:"grown,omitempty"`  // by cluster, bytes its datastores filled since the last daily line
	Fails  map[string]int     `json:"fails,omitempty"`  // by behaviour, failures in a row
	Held   map[string]int64   `json:"held,omitempty"`   // by behaviour, when it stopped itself after failing three times
	Seq    int64              `json:"seq"`              // the last log id
}

// Engine decides what happens. It is not safe for concurrent use: its
// caller steps it under one lock.
type Engine struct {
	Seed     string         `json:"-"`
	Loc      *time.Location `json:"-"`
	Settings Settings       `json:"settings"`
	State    State          `json:"state"`
	Log      []Entry        `json:"log"`

	you bool // what is being done now follows from a button the owner pressed
}

// New makes an engine with nothing running.
func New(seed string, loc *time.Location, s Settings) *Engine {
	if loc == nil {
		loc = time.Local
	}
	return &Engine{Seed: seed, Loc: loc, Settings: s}
}

// catchUp is how many missed minutes a step still plays. Anything longer
// (the simulator was off, or paused) is not caught up: the engine carries
// on from now.
const catchUp = 3

// Step plays every minute up to now that has not been played yet.
func (e *Engine) Step(now time.Time, w World) {
	m := now.Unix() / 60
	if e.State.At == 0 || m-e.State.At > catchUp {
		e.State.At = m - 1
	}
	for t := e.State.At + 1; t <= m; t++ {
		e.minute(t, w)
		e.State.At = t
	}
}

// Skip moves the engine's clock to now without playing anything, for the
// time the simulator spent paused.
func (e *Engine) Skip(now time.Time) { e.State.At = now.Unix() / 60 }

func (e *Engine) minute(t int64, w World) {
	if !e.Settings.Running {
		return
	}
	cs := clusters(w)
	// VMs that are due back on
	var still []OffVM
	for _, o := range e.State.Off {
		if o.On > t {
			still = append(still, o)
			continue
		}
		if w.PowerOn(o.Cluster, o.VM) {
			e.add(t, Entry{Kind: KindOn, Cluster: o.Cluster, Why: Power, Text: fmt.Sprintf("%s was powered on again.", o.VM), Counts: map[string]int{"on": 1}})
		}
	}
	e.State.Off = still
	e.limits(t, cs)
	for _, b := range Counted {
		if e.State.Held[b] != 0 {
			continue
		}
		g := e.Settings.Global[b]
		if g.Exact != nil {
			// the vCenter's own exact number is one stream for the whole vCenter:
			// "a VM every 30 minutes" is one VM, somewhere, every 30 minutes
			if c, ok := e.follower(cs, b); ok && e.due(t, "*", b, g.Exact.perDay(), b != Moves) {
				e.fire(t, w, c, b)
			}
		}
		for _, c := range cs {
			d, own := e.Settings.Dial(c.Key, b)
			if !own && g.Exact != nil {
				continue
			}
			if e.due(t, c.Key, b, e.PerDay(cs, c, b), d.Exact != nil && b != Moves) {
				e.fire(t, w, c, b)
			}
		}
	}
	e.maintenance(t, w, 1)
	e.disks(t, w, cs)
}

func clusters(w World) []Cluster {
	cs := w.Clusters()
	sort.Slice(cs, func(i, j int) bool { return cs[i].Key < cs[j].Key })
	return cs
}

func totalVMs(cs []Cluster) (vms, off int) {
	for _, c := range cs {
		vms += c.VMs
		off += c.Off
	}
	return
}

// PerDay is how many times a counted behaviour happens in a cluster on a
// working day: VM moves for load balancing, VMs for the others.
func (e *Engine) PerDay(cs []Cluster, c Cluster, behaviour string) float64 {
	d, own := e.Settings.Dial(c.Key, behaviour)
	if d.Exact == nil {
		return Baseline.Per100[behaviour] * float64(c.VMs) / 100 * d.Level
	}
	if own {
		return d.Exact.perDay()
	}
	// the vCenter's own exact number is shared among the clusters that follow it
	var all int
	for _, o := range cs {
		if _, has := e.Settings.Clusters[o.Key][behaviour]; !has {
			all += o.VMs
		}
	}
	if all == 0 {
		return 0
	}
	return d.Exact.perDay() * float64(c.VMs) / float64(all)
}

// burst is how many VMs a load-balancing burst moves on average.
const burst = 4

// follower picks the cluster the next occurrence of the vCenter's own exact
// number falls in: one of the clusters that follow the vCenter's dial, in
// proportion to their VMs.
func (e *Engine) follower(cs []Cluster, b string) (Cluster, bool) {
	var all []Cluster
	total := 0
	for _, c := range cs {
		if _, own := e.Settings.Clusters[c.Key][b]; !own && c.VMs > 0 {
			all = append(all, c)
			total += c.VMs
		}
	}
	if total == 0 {
		return Cluster{}, false
	}
	n := int(pick(e.Seed, "*", b+"/where", e.State.K["*|"+b]) % uint64(total))
	for _, c := range all {
		if n < c.VMs {
			return c, true
		}
		n -= c.VMs
	}
	return all[len(all)-1], true
}

// due advances one stream (a cluster's behaviour, or "*" for the vCenter's
// own exact number) by one minute and reports whether its next occurrence
// has come. per is how many a working day; steady means the same pace day
// and night.
func (e *Engine) due(t int64, stream, b string, per float64, steady bool) bool {
	if per <= 0 {
		return false
	}
	at := time.Unix(t*60, 0).In(e.Loc)
	var step, spread float64
	switch {
	case steady: // his own rhythm
		step, spread = per/1440, 0.1
	case b == Moves:
		step, spread = per/burst*balanceShape(at)/60, 0.3
	default:
		step, spread = per*workShape(at)/60, 0.3
	}
	key := stream + "|" + b
	if e.State.Acc == nil {
		e.State.Acc = map[string]float64{}
	}
	if e.State.K == nil {
		e.State.K = map[string]uint64{}
	}
	acc := e.State.Acc[key] + step
	// each occurrence comes a little early or late, so nothing runs like clockwork
	need := 1 - spread + 2*spread*float64(pick(e.Seed, stream, b+"/when", e.State.K[key])%1000)/1000
	if acc < need {
		e.State.Acc[key] = acc
		return false
	}
	e.State.Acc[key] = 0
	if stream == "*" {
		e.State.K[key]++
	}
	return true
}

func (e *Engine) next(cluster, b string) uint64 {
	key := cluster + "|" + b
	if e.State.K == nil {
		e.State.K = map[string]uint64{}
	}
	e.State.K[key]++
	return pick(e.Seed, cluster, b, e.State.K[key])
}

// fire makes one occurrence of a counted behaviour happen in a cluster.
func (e *Engine) fire(t int64, w World, c Cluster, b string) bool {
	p := e.next(c.Key, b)
	switch b {
	case Moves:
		moves := w.Balance(c.Key, 2+int(p%5), p)
		if len(moves) == 0 {
			return false
		}
		e.add(t, Entry{Kind: KindMove, Cluster: c.Key, Why: Moves, Text: fmt.Sprintf("DRS moved %s in %s to balance the load.", count(len(moves), "VM"), c.Name),
			Counts: map[string]int{"move": len(moves)}, Details: moveLines(moves)})
	case Power:
		vms, off := totalVMs(clusters(w))
		if (off+1)*10 > vms { // no more than 1 VM in 10 off at once
			return false
		}
		vm := w.PowerOff(c.Key, p)
		if vm == "" {
			return false
		}
		back := int64(5 + p>>8%26)
		if p>>4%4 == 0 {
			back = int64(30 + p>>8%91)
		}
		e.State.Off = append(e.State.Off, OffVM{Cluster: c.Key, VM: vm, On: t + back})
		e.add(t, Entry{Kind: KindOff, Cluster: c.Key, Why: Power, Text: fmt.Sprintf("%s was powered off.", vm), Counts: map[string]int{"off": 1}})
	case Adds:
		if vms, _ := totalVMs(clusters(w)); vms >= e.Settings.MaxVMs {
			return false
		}
		vm, host, err := w.AddVM(c.Key, p)
		if err != nil {
			e.failed(t, b, c, fmt.Sprintf("Failed: no VM was added to %s. %s", c.Name, reason(err)))
			return false
		}
		delete(e.State.Fails, b)
		e.add(t, Entry{Kind: KindAdd, Cluster: c.Key, Why: Adds, Text: fmt.Sprintf("%s was added to %s, on %s.", vm, c.Name, host), Counts: map[string]int{"add": 1}})
	case Removes:
		if vms, _ := totalVMs(clusters(w)); vms <= e.Settings.MinVMs {
			return false
		}
		vm, err := w.RemoveVM(c.Key, p)
		if err != nil {
			e.failed(t, b, c, fmt.Sprintf("Failed: no VM was removed from %s. %s", c.Name, reason(err)))
			return false
		}
		if vm == "" {
			return false
		}
		delete(e.State.Fails, b)
		var still []OffVM
		for _, o := range e.State.Off {
			if o.Cluster != c.Key || o.VM != vm {
				still = append(still, o)
			}
		}
		e.State.Off = still
		e.add(t, Entry{Kind: KindRemove, Cluster: c.Key, Why: Removes, Text: fmt.Sprintf("%s was removed from %s.", vm, c.Name), Counts: map[string]int{"remove": 1}})
	}
	return true
}

func reason(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "FileFault") || strings.Contains(msg, "no space") || strings.Contains(msg, "quota") {
		return "The machine that runs the simulator is out of disk space."
	}
	return strings.ToUpper(msg[:1]) + strings.TrimSuffix(msg[1:], ".") + "."
}

// failed records a failure. Three in a row stop that one behaviour, and
// nothing else, until the owner switches it on again.
func (e *Engine) failed(t int64, b string, c Cluster, text string) {
	if e.State.Fails == nil {
		e.State.Fails, e.State.Held = map[string]int{}, map[string]int64{}
	}
	e.State.Fails[b]++
	e.add(t, Entry{Kind: KindFail, Cluster: c.Key, Why: b, Text: text, Counts: map[string]int{"fail": 1}})
	if e.State.Fails[b] >= 3 {
		e.State.Held[b] = t
		e.add(t, Entry{Kind: KindFail, Why: b, Text: fmt.Sprintf("%s stopped itself after failing 3 times in a row. Everything else carries on.", Names[b])})
	}
}

// Resume lets a behaviour that stopped itself happen again.
func (e *Engine) Resume(b string) {
	delete(e.State.Held, b)
	delete(e.State.Fails, b)
}

// limits writes one line, and one a day while it lasts, when adding or
// removing VMs is waiting at a limit.
func (e *Engine) limits(t int64, cs []Cluster) {
	vms, _ := totalVMs(cs)
	said := func(b, text string) {
		if e.State.Waited == nil {
			e.State.Waited = map[string]int64{}
		}
		if last, ok := e.State.Waited[b]; ok && t-last < 1440 {
			return
		}
		e.State.Waited[b] = t
		e.add(t, Entry{Kind: KindWait, Why: b, Text: text, Counts: map[string]int{"wait": 1}})
	}
	if e.active(cs, Adds) && vms >= e.Settings.MaxVMs {
		said(Adds, fmt.Sprintf("Adding VMs is waiting: the environment has the most VMs it may have (%s). A VM is added only when one has been removed.", num(e.Settings.MaxVMs)))
	}
	if e.active(cs, Removes) && vms <= e.Settings.MinVMs {
		said(Removes, fmt.Sprintf("Removing VMs is waiting: the environment has the fewest VMs it may have (%s). A VM is removed only when one has been added.", num(e.Settings.MinVMs)))
	}
}

// active reports whether a counted behaviour happens anywhere.
func (e *Engine) active(cs []Cluster, b string) bool {
	if e.State.Held[b] != 0 {
		return false
	}
	for _, c := range cs {
		if e.PerDay(cs, c, b) > 0 {
			return true
		}
	}
	return false
}

// AtLimit reports whether adding (or removing) VMs is waiting.
func (e *Engine) AtLimit(cs []Cluster, b string) bool {
	vms, _ := totalVMs(cs)
	return (b == Adds && vms >= e.Settings.MaxVMs) || (b == Removes && vms <= e.Settings.MinVMs)
}

// ---------------------------------------------------------------------------
// hosts in maintenance mode

// maintPercent is the share of a cluster's hosts its dial asks for, and how
// long a host stays in before another takes its place.
func maintWant(d Dial) (percent float64, swap int64) {
	swap = int64(Baseline.MaintSwap / time.Minute)
	if d.Exact != nil {
		return d.Exact.N, swap
	}
	if d.Level <= 0 {
		return 0, swap
	}
	// a busier dial means more hosts out, changed more often
	return Baseline.MaintPercent * d.Level, int64(float64(swap) / math.Max(0.25, d.Level))
}

// room is how many of a cluster's hosts may be out of action at once: a
// quarter, and none in a cluster of fewer than four.
func room(c Cluster) int { return len(c.Hosts) / 4 }

// MaintTargets is how many hosts of each cluster should be in maintenance
// mode. The clusters that follow the vCenter's dial share its number in
// proportion to their hosts, taking turns for the odd ones; a cluster with
// its own dial has its own number.
func (e *Engine) MaintTargets(t int64, cs []Cluster) map[string]int {
	out := map[string]int{}
	var followers []Cluster
	var hosts int
	for _, c := range cs {
		d, own := e.Settings.Dial(c.Key, Maintenance)
		if !own {
			followers = append(followers, c)
			hosts += len(c.Hosts)
			continue
		}
		pct, _ := maintWant(d)
		n := int(math.Round(pct / 100 * float64(len(c.Hosts))))
		if pct > 0 && n == 0 {
			n = 1
		}
		out[c.Key] = min(n, room(c))
	}
	pct, swap := maintWant(e.Settings.Global[Maintenance])
	if pct <= 0 || hosts == 0 {
		return out
	}
	total := max(1, int(math.Round(pct/100*float64(hosts))))
	type share struct {
		c    Cluster
		frac float64
		turn int
	}
	epoch := int(t / max(1, swap))
	var rest []share
	for i, c := range followers {
		q := float64(total) * float64(len(c.Hosts)) / float64(hosts)
		n := min(int(q), room(c))
		out[c.Key] = n
		total -= n
		rest = append(rest, share{c, q - math.Floor(q), (i + epoch) % len(followers)})
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].frac != rest[j].frac {
			return rest[i].frac > rest[j].frac
		}
		return rest[i].turn < rest[j].turn
	})
	for round := 0; total > 0 && round < 4; round++ {
		for _, s := range rest {
			if total > 0 && out[s.c.Key] < room(s.c) {
				out[s.c.Key]++
				total--
			}
		}
	}
	return out
}

// maintenance brings each cluster towards its number: at most `rounds`
// hosts come out and go in for each cluster in one call, so a change to
// many hosts is spread over minutes.
func (e *Engine) maintenance(t int64, w World, rounds int) {
	for r := 0; r < rounds; r++ {
		cs := clusters(w)
		targets := e.MaintTargets(t, cs)
		changed := false
		for _, c := range cs {
			if e.maintainCluster(t, w, c, targets[c.Key]) {
				changed = true
			}
		}
		if !changed {
			return
		}
	}
}

func (e *Engine) maintainCluster(t int64, w World, c Cluster, target int) bool {
	live := map[string]Host{}
	out := 0
	for _, h := range c.Hosts {
		live[h.Name] = h
		if !h.InService() {
			out++
		}
	}
	// forget hosts that are no longer there, or that something else took out of maintenance mode
	var mine, others []MaintHost
	for _, m := range e.State.Maint {
		switch {
		case m.Cluster != c.Key:
			others = append(others, m)
		case live[m.Host].Maintenance:
			mine = append(mine, m)
		}
	}
	e.State.Maint = append(others, mine...)
	leave := -1
	for i, m := range mine {
		if m.Until <= t || len(mine) > target {
			leave = i
			break
		}
	}
	if leave >= 0 {
		m := mine[leave]
		moves := w.ExitMaintenance(c.Key, m.Host, e.next(c.Key, Maintenance+"/out"))
		e.dropMaint(c.Key, m.Host)
		if e.State.Rested == nil {
			e.State.Rested = map[string]string{}
		}
		e.State.Rested[c.Key] = m.Host
		e.add(t, Entry{Kind: KindOut, Cluster: c.Key, Why: Maintenance, Text: fmt.Sprintf("%s in %s came out of maintenance mode. %s moved back onto it.", short(m.Host), c.Name, count(len(moves), "VM")),
			Counts: map[string]int{"mout": 1, "move": len(moves)}, Details: moveLines(moves)})
		return true
	}
	if len(mine) >= target || out >= room(c) {
		return false
	}
	var free []string
	for _, h := range c.Hosts {
		if h.InService() && h.Name != e.State.Rested[c.Key] {
			free = append(free, h.Name)
		}
	}
	if len(free) == 0 {
		return false
	}
	sort.Strings(free)
	host := free[e.next(c.Key, Maintenance+"/in")%uint64(len(free))]
	moves, err := w.EnterMaintenance(c.Key, host)
	if err != nil {
		e.add(t, Entry{Kind: KindFail, Cluster: c.Key, Why: Maintenance, Text: fmt.Sprintf("Failed: %s in %s did not go into maintenance mode. %s", short(host), c.Name, reason(err)), Counts: map[string]int{"fail": 1}})
		return false
	}
	_, swap := maintWant(dialOf(&e.Settings, c.Key, Maintenance))
	e.State.Maint = append(e.State.Maint, MaintHost{Cluster: c.Key, Host: host, Since: t, Until: t + swap})
	e.add(t, Entry{Kind: KindIn, Cluster: c.Key, Why: Maintenance, Text: fmt.Sprintf("%s in %s went into maintenance mode. Its %s moved to other hosts first.", short(host), c.Name, count(len(moves), "VM")),
		Counts: map[string]int{"min": 1, "move": len(moves)}, Details: moveLines(moves)})
	return true
}

func dialOf(s *Settings, cluster, b string) Dial { d, _ := s.Dial(cluster, b); return d }

func (e *Engine) dropMaint(cluster, host string) {
	var keep []MaintHost
	for _, m := range e.State.Maint {
		if m.Cluster != cluster || m.Host != host {
			keep = append(keep, m)
		}
	}
	e.State.Maint = keep
}

// ---------------------------------------------------------------------------
// datastores

// diskEvery is how often datastores fill a little, in minutes.
const diskEvery = 30

func diskPercent(d Dial, base float64) float64 {
	if d.Exact != nil {
		return d.Exact.N
	}
	return base * d.Level
}

// disks fills each cluster's datastores a little every half hour, and at
// two in the morning cleans them up and writes the day's one line.
func (e *Engine) disks(t int64, w World, cs []Cluster) {
	at := time.Unix(t*60, 0).In(e.Loc)
	if at.Minute()%diskEvery != 0 {
		return
	}
	cleanup := at.Hour() == 2 && at.Minute() == 0
	for _, c := range cs {
		grow := diskPercent(dialOf(&e.Settings, c.Key, DiskGrowth), Baseline.GrowPercent)
		if grow > 0 {
			var bytes int64
			for _, s := range w.Fill(c.Key, grow/100*diskEvery/1440) {
				bytes += s.Bytes
			}
			if e.State.Grown == nil {
				e.State.Grown = map[string]int64{}
			}
			e.State.Grown[c.Key] += bytes
		}
		if !cleanup {
			continue
		}
		shrink := diskPercent(dialOf(&e.Settings, c.Key, DiskShrink), Baseline.ShrinkPct)
		var freed int64
		var stores []Store
		if shrink > 0 {
			stores = w.Fill(c.Key, -shrink/100)
			for _, s := range stores {
				freed -= s.Bytes
			}
		}
		grown := e.State.Grown[c.Key]
		delete(e.State.Grown, c.Key)
		if grown == 0 && freed == 0 {
			continue
		}
		text := fmt.Sprintf("The datastores of %s filled by %s over the last day", c.Name, size(grown))
		if freed > 0 {
			text += fmt.Sprintf(", and the nightly cleanup freed %s", size(freed))
		}
		text += "."
		var details []string
		for _, s := range stores {
			details = append(details, fmt.Sprintf("%s is now %d%% full", s.Name, s.UsedPercent))
		}
		if len(stores) > 0 {
			text += fmt.Sprintf(" %s is now %d%% full.", stores[0].Name, stores[0].UsedPercent)
		}
		e.add(t, Entry{Kind: KindDisk, Cluster: c.Key, Why: DiskGrowth, Text: text,
			Counts: map[string]int{"grownGB": int(grown >> 30), "freedGB": int(freed >> 30)}, Details: details})
	}
}

// ---------------------------------------------------------------------------
// what the owner does

// Start makes things happen again and brings hosts into maintenance mode as
// the dials ask.
func (e *Engine) Start(now time.Time, w World) {
	e.you = true
	defer func() { e.you = false }()
	t := now.Unix() / 60
	e.Settings.Running = true
	e.State.At = t
	e.add(t, Entry{Kind: KindNote, Why: "you", Text: "Activity was started."})
	e.maintenance(t, w, 8)
	e.Load(w)
}

// Stop ends everything the engine started, so the environment is left
// still and healthy: hosts come out of maintenance mode and VMs that are
// off are powered on. VMs that were added or removed stay that way.
func (e *Engine) Stop(now time.Time, w World) {
	e.you = true
	defer func() { e.you = false }()
	t := now.Unix() / 60
	e.add(t, Entry{Kind: KindNote, Why: "you", Text: "Activity was stopped. Nothing happens by itself until it is started again."})
	names := map[string]string{}
	for _, c := range clusters(w) {
		names[c.Key] = c.Name
	}
	for _, m := range e.State.Maint {
		moves := w.ExitMaintenance(m.Cluster, m.Host, e.next(m.Cluster, Maintenance+"/out"))
		e.add(t, Entry{Kind: KindOut, Cluster: m.Cluster, Why: Maintenance, Text: fmt.Sprintf("%s in %s came out of maintenance mode. %s moved back onto it.", short(m.Host), names[m.Cluster], count(len(moves), "VM")),
			Counts: map[string]int{"mout": 1, "move": len(moves)}, Details: moveLines(moves)})
	}
	e.State.Maint = nil
	for _, o := range e.State.Off {
		if w.PowerOn(o.Cluster, o.VM) {
			e.add(t, Entry{Kind: KindOn, Cluster: o.Cluster, Why: Power, Text: fmt.Sprintf("%s was powered on again, early, because activity was stopped.", o.VM), Counts: map[string]int{"on": 1}})
		}
	}
	e.State.Off = nil
	e.Settings.Running = false
}

// Apply takes new settings into use. A change to the hosts kept in
// maintenance mode takes effect at once; everything else from the next
// minute.
func (e *Engine) Apply(now time.Time, s Settings, w World) {
	e.you = true
	defer func() { e.you = false }()
	e.Settings = s
	if s.Running {
		e.maintenance(now.Unix()/60, w, 8)
	}
	e.Load(w)
}

// Load tells the world how loaded each cluster should look.
func (e *Engine) Load(w World) {
	for _, c := range clusters(w) {
		w.SetLoad(c.Key, dialOf(&e.Settings, c.Key, CPU).Level, dialOf(&e.Settings, c.Key, Memory).Level)
	}
}

// DoNow makes one occurrence of a counted behaviour happen in a cluster
// straight away, on top of what happens by itself. It reports whether
// anything was done.
func (e *Engine) DoNow(now time.Time, w World, cluster, b string) bool {
	e.you = true
	defer func() { e.you = false }()
	for _, c := range clusters(w) {
		if c.Key == cluster {
			return e.fire(now.Unix()/60, w, c, b)
		}
	}
	return false
}

// Note writes a line that only says what the owner did.
func (e *Engine) Note(now time.Time, text string) {
	e.add(now.Unix()/60, Entry{Kind: KindNote, Why: "you", Text: text, You: true})
}

// Reconcile drops what the engine remembers about hosts and VMs that are
// no longer there, after the inventory was rebuilt.
func (e *Engine) Reconcile(w World) {
	hosts := map[string]bool{}
	known := map[string]bool{}
	for _, c := range clusters(w) {
		known[c.Key] = true
		for _, h := range c.Hosts {
			if h.Maintenance {
				hosts[c.Key+"|"+h.Name] = true
			}
		}
	}
	var maint []MaintHost
	for _, m := range e.State.Maint {
		if hosts[m.Cluster+"|"+m.Host] {
			maint = append(maint, m)
		}
	}
	e.State.Maint = maint
	var off []OffVM
	for _, o := range e.State.Off {
		if known[o.Cluster] {
			off = append(off, o)
		}
	}
	e.State.Off = off
	for key := range e.Settings.Clusters {
		if !known[key] {
			delete(e.Settings.Clusters, key)
		}
	}
}

func (e *Engine) add(t int64, en Entry) {
	e.State.Seq++
	en.ID = e.State.Seq
	en.At = time.Unix(t*60, 0).UTC()
	en.You = en.You || e.you
	e.Log = append(e.Log, en)
	if len(e.Log) > LogSize {
		e.Log = append([]Entry(nil), e.Log[len(e.Log)-LogSize:]...)
	}
}

// Names are the behaviours in the console's words.
var Names = map[string]string{
	Moves: "Load balancing (DRS)", Power: "VMs powered off and on", Adds: "New VMs", Removes: "VMs removed",
	Maintenance: "Hosts in maintenance mode", DiskGrowth: "Datastores filling", DiskShrink: "Datastore cleanup",
	CPU: "CPU load", Memory: "Memory load",
}

func moveLines(moves []Move) []string {
	out := make([]string, len(moves))
	for i, m := range moves {
		out[i] = fmt.Sprintf("%s from %s to %s", m.VM, short(m.From), short(m.To))
	}
	return out
}

// short is a host's name without its domain.
func short(host string) string {
	if i := strings.IndexByte(host, '.'); i > 0 {
		return host[:i]
	}
	return host
}

func count(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return num(n) + " " + one + "s"
}

// num writes a number with thousands separators.
func num(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// size writes a number of bytes as GB or TB.
func size(b int64) string {
	gb := float64(b) / (1 << 30)
	if gb >= 1024 {
		return fmt.Sprintf("%.1f TB", gb/1024)
	}
	if gb >= 10 {
		return fmt.Sprintf("%.0f GB", gb)
	}
	return fmt.Sprintf("%.1f GB", gb)
}
