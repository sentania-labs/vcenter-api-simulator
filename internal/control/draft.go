package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/perf"
	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// ErrNoDraft is returned when there is nothing pending.
var ErrNoDraft = errors.New("no pending draft")

// Invalid wraps a message the operator can fix (a 422, not a 500).
type Invalid struct{ Msg string }

func (e *Invalid) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &Invalid{fmt.Sprintf(format, a...)} }

// applied is the running scenario; callers hold c.mu or only read.
func (c *Controller) applied() *running { return c.cur.Load() }

// ensureDraft returns the draft, starting one from the applied scenario.
func (c *Controller) ensureDraft() *Draft {
	if c.scheduled != nil {
		// a scheduled apply owns the pending change until it lands or is cancelled
		return nil
	}
	if c.draft == nil {
		r := c.applied()
		c.draft = &Draft{Scenario: clone(r.sc), Scope: c.scope, BasedOn: r.version, Origin: "edit", Created: c.now()}
	}
	return c.draft
}

// settle drops a draft that no longer differs from what is applied.
func (c *Controller) settle() {
	d := c.draft
	if d == nil {
		return
	}
	if normal(d.Scenario) == normal(c.applied().sc) && len(d.Regenerate) == 0 && d.Scope == c.scope && d.Origin == "edit" {
		c.draft = nil
	}
}

// Pending reports what is waiting, for the header badge.
type Pending struct {
	Exists    bool   `json:"exists"`
	Scheduled bool   `json:"scheduled"`
	Topology  int    `json:"topologyChanges"`
	Telemetry int    `json:"telemetryChanges"`
	Origin    string `json:"origin,omitempty"`
	BasedOn   int    `json:"basedOn,omitempty"`
	Target    int    `json:"target,omitempty"`
	// At is when a scheduled draft lands.
	At *time.Time `json:"at,omitempty"`
}

func (c *Controller) pending() Pending {
	d, sched := c.draft, false
	var at *time.Time
	if c.scheduled != nil {
		d, sched = c.scheduled.Draft, true
		t := c.scheduled.At
		at = &t
	}
	if d == nil {
		return Pending{}
	}
	p := Pending{Exists: true, Scheduled: sched, Origin: d.Origin, BasedOn: d.BasedOn, Target: c.applied().version + 1, At: at}
	rev, err := c.review(d)
	if err == nil {
		p.Topology = rev.TopologyCount
		p.Telemetry = rev.TelemetryCount
		p.Target = rev.To
	}
	return p
}

// DraftView is the draft as the console edits it.
type DraftView struct {
	Pending    Pending            `json:"pending"`
	Scenario   *scenario.Scenario `json:"scenario"`
	Applied    *scenario.Scenario `json:"applied"`
	Scope      string             `json:"scope"`
	Regenerate []string           `json:"regenerate"`
	Discarded  string             `json:"discarded,omitempty"`
}

// DraftState returns the draft, or the applied scenario when there is none.
func (c *Controller) DraftState() DraftView {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.draftView()
}

func (c *Controller) draftView() DraftView {
	r := c.applied()
	v := DraftView{Pending: c.pending(), Applied: Strip(r.sc), Scenario: Strip(r.sc), Scope: c.scope, Regenerate: []string{}}
	d := c.draft
	if d == nil && c.scheduled != nil {
		d = c.scheduled.Draft
	}
	if d != nil {
		v.Scenario, v.Scope, v.Discarded = Strip(d.Scenario), d.Scope, d.Discarded
		if d.Regenerate != nil {
			v.Regenerate = d.Regenerate
		}
	}
	return v
}

// SetDraft replaces the draft's scenario with the operator's edit. raw is
// the editable scenario as JSON; scope is the metric scope ("" keeps it).
func (c *Controller) SetDraft(raw []byte, scope string) (DraftView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scheduled != nil {
		return DraftView{}, invalid("a change is scheduled for %s; cancel it to edit again", c.scheduled.At.UTC().Format("15:04:05Z"))
	}
	sc, err := fromJSON(raw, c.base)
	if err != nil {
		return DraftView{}, &Invalid{err.Error()}
	}
	d := c.ensureDraft()
	if scope != "" {
		if scope != ScopeAll && scope != ScopeDashboard {
			return DraftView{}, invalid("scope must be %q or %q", ScopeAll, ScopeDashboard)
		}
		d.Scope = scope
	}
	prev := d.Scenario
	d.Scenario = sc
	// a cluster that vanished from the scenario no longer needs regenerating
	keep := d.Regenerate[:0]
	gen := map[string]bool{}
	for _, k := range sc.ClusterKeys() {
		gen[k] = true
	}
	for _, k := range d.Regenerate {
		if gen[k] {
			keep = append(keep, k)
		}
	}
	d.Regenerate = keep
	_ = prev
	c.settle()
	return c.draftView(), nil
}

// DiscardDraft drops the pending draft (a scheduled one included).
func (c *Controller) DiscardDraft() DraftView {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scheduled != nil {
		c.scheduled.timer.Stop()
		c.scheduled = nil
	}
	c.draft = nil
	return c.draftView()
}

// CancelScheduled returns a scheduled apply to the draft.
func (c *Controller) CancelScheduled() (DraftView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scheduled == nil {
		return DraftView{}, ErrNoDraft
	}
	c.scheduled.timer.Stop()
	c.draft, c.scheduled = c.scheduled.Draft, nil
	return c.draftView(), nil
}

// Regenerate marks a cluster for new identities (same seed): applying the
// draft retires every identity of the cluster and mints fresh ones.
func (c *Controller) Regenerate(key string) (DraftView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.ensureDraft()
	if d == nil {
		return DraftView{}, invalid("a change is scheduled; cancel it first")
	}
	if !contains(d.Scenario.ClusterKeys(), key) {
		return DraftView{}, invalid("%q is not a cluster in the draft", key)
	}
	if !contains(d.Regenerate, key) {
		d.Regenerate = append(d.Regenerate, key)
		sort.Strings(d.Regenerate)
	}
	return c.draftView(), nil
}

// ResetSeed gives a cluster a new telemetry seed epoch: every sample of the
// cluster changes, no identity does.
func (c *Controller) ResetSeed(key string) (DraftView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.ensureDraft()
	if d == nil {
		return DraftView{}, invalid("a change is scheduled; cancel it first")
	}
	if !contains(d.Scenario.ClusterKeys(), key) {
		return DraftView{}, invalid("%q is not a cluster in the draft", key)
	}
	for i := range d.Scenario.Clusters {
		if d.Scenario.Clusters[i].Key == key {
			d.Scenario.Clusters[i].SeedEpoch++
			return c.draftView(), nil
		}
	}
	d.Scenario.Clusters = append(d.Scenario.Clusters, scenario.ClusterOverride{Key: key, SeedEpoch: 1})
	return c.draftView(), nil
}

// DeleteCluster leaves a cluster out of the draft. Its overrides and fault
// selectors go with it; the other clusters keep their names.
func (c *Controller) DeleteCluster(key string) (DraftView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.ensureDraft()
	if d == nil {
		return DraftView{}, invalid("a change is scheduled; cancel it first")
	}
	if !contains(d.Scenario.ClusterKeys(), key) {
		return DraftView{}, invalid("%q is not a cluster in the draft", key)
	}
	if len(d.Scenario.ClusterKeys()) == 1 {
		return DraftView{}, invalid("the last cluster cannot be deleted; a simulator needs at least one")
	}
	sc := d.Scenario
	sc.Removed = append(sc.Removed, key)
	sort.Strings(sc.Removed)
	var cl []scenario.ClusterOverride
	for _, o := range sc.Clusters {
		if o.Key != key {
			cl = append(cl, o)
		}
	}
	sc.Clusters = cl
	for i := range sc.Faults {
		sc.Faults[i].Clusters = without(sc.Faults[i].Clusters, key)
	}
	d.Regenerate = without(d.Regenerate, key)
	return c.draftView(), nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func without(l []string, s string) []string {
	var out []string
	for _, x := range l {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// review

// Change is one row of the review table.
type Change struct {
	Object string `json:"object"`
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
	Effect string `json:"effect"`
	Area   string `json:"area"`
}

// IdentityRow counts the identities one kind keeps, adds and removes.
type IdentityRow struct {
	Label   string `json:"label"`
	Kept    int    `json:"kept"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	// Renumbered counts kept objects that get a new MoRef (MoRef row only).
	Renumbered int `json:"renumbered,omitempty"`
}

// Warning is a note the operator should read before applying.
type Warning struct {
	Severity string `json:"severity"` // warn | info | ok
	Title    string `json:"title"`
	Text     string `json:"text"`
}

// Renumbered counts the retained objects that get a new MoRef: vcsim numbers
// MoRefs in creation order, which is registry serial order, so every object
// created after the first removed one is numbered again. UUIDs do not change.
type Renumbered struct {
	Clusters int `json:"clusters"`
	Hosts    int `json:"hosts"`
	VMs      int `json:"vms"`
}

// Total is every renumbered object.
func (r Renumbered) Total() int { return r.Clusters + r.Hosts + r.VMs }

// firstRemoved is the lowest registry serial among objects that o has and n
// does not (0 when nothing is removed): everything created after it is
// numbered again.
func firstRemoved(o, n *topology.Topology) int {
	type id struct {
		key    string
		serial int
	}
	have := map[id]bool{}
	for _, c := range n.Clusters() {
		have[id{c.Key, c.Serial}] = true
		for _, h := range c.Hosts {
			have[id{h.Key, h.Serial}] = true
			for _, d := range h.Devices {
				have[id{d.Key, d.Serial}] = true
			}
		}
		for _, v := range c.VMs {
			have[id{v.Key, v.Serial}] = true
		}
	}
	first := 0
	note := func(k id) {
		if !have[k] && (first == 0 || k.serial < first) {
			first = k.serial
		}
	}
	for _, c := range o.Clusters() {
		note(id{c.Key, c.Serial})
		for _, h := range c.Hosts {
			note(id{h.Key, h.Serial})
			for _, d := range h.Devices {
				note(id{d.Key, d.Serial})
			}
		}
		for _, v := range c.VMs {
			note(id{v.Key, v.Serial})
		}
	}
	return first
}

// renumbered compares the creation order of two topologies.
func renumbered(o, n *topology.Topology) Renumbered {
	type id struct {
		key    string
		serial int
	}
	oldIDs, newIDs := map[id]bool{}, map[id]bool{}
	first := 0
	collect := func(t *topology.Topology, into map[id]bool) {
		for _, c := range t.Clusters() {
			into[id{c.Key, c.Serial}] = true
			for _, h := range c.Hosts {
				into[id{h.Key, h.Serial}] = true
				for _, d := range h.Devices {
					into[id{d.Key, d.Serial}] = true
				}
			}
			for _, v := range c.VMs {
				into[id{v.Key, v.Serial}] = true
			}
		}
	}
	collect(o, oldIDs)
	collect(n, newIDs)
	for k := range oldIDs {
		if !newIDs[k] && (first == 0 || k.serial < first) {
			first = k.serial
		}
	}
	var r Renumbered
	if first == 0 {
		return r
	}
	for _, c := range n.Clusters() {
		if !oldIDs[id{c.Key, c.Serial}] {
			continue // new objects are created last
		}
		if c.Serial > first {
			r.Clusters++
		}
		for _, h := range c.Hosts {
			if oldIDs[id{h.Key, h.Serial}] && h.Serial > first {
				r.Hosts++
			}
		}
		for _, v := range c.VMs {
			if oldIDs[id{v.Key, v.Serial}] && v.Serial > first {
				r.VMs++
			}
		}
	}
	return r
}

// Review is what applying a draft would do.
type Review struct {
	From             int           `json:"from"`
	To               int           `json:"to"`
	Origin           string        `json:"origin"`
	Discarded        string        `json:"discarded,omitempty"`
	Changes          []Change      `json:"changes"`
	More             int           `json:"moreChanges"`
	TopologyCount    int           `json:"topologyChanges"`
	TelemetryCount   int           `json:"telemetryChanges"`
	RebuildsVCenter  bool          `json:"rebuildsVcenter"`
	Identities       []IdentityRow `json:"identities"`
	Churn            bool          `json:"identityChurn"`
	Renumbered       Renumbered    `json:"renumbered"`
	NotAffected      []string      `json:"notAffected"`
	Warnings         []Warning     `json:"warnings"`
	Scope            string        `json:"scope"`
	Before           Load          `json:"before"`
	After            Load          `json:"after"`
	HostsBefore      int           `json:"hostsBefore"`
	HostsAfter       int           `json:"hostsAfter"`
	Boundary         time.Time     `json:"boundary"`
	Now              time.Time     `json:"now"`
	SessionsDropped  bool          `json:"sessionsDropped"`
	RegisteredBefore int           `json:"registryBefore"`
}

// Review computes the review of the pending draft.
func (c *Controller) Review() (*Review, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.draft
	if d == nil && c.scheduled != nil {
		d = c.scheduled.Draft
	}
	if d == nil {
		return nil, ErrNoDraft
	}
	return c.review(d)
}

// previewTopology generates what the draft would build, against a copy of
// the registry so nothing is minted for real.
func (c *Controller) previewTopology(d *Draft) (*topology.Topology, *registry.Registry) {
	old := c.applied()
	var reg *registry.Registry
	if d.Scenario.Seed != old.sc.Seed {
		reg = registry.InMemory(d.Scenario.Seed)
	} else {
		reg = c.reg.Clone()
		for _, k := range d.Regenerate {
			reg.Retire(k)
		}
		for _, k := range d.Scenario.Removed {
			if !old.sc.IsRemoved(k) {
				reg.Retire(k)
			}
		}
	}
	return topology.Generate(d.Scenario, reg), reg
}

func (c *Controller) review(d *Draft) (*Review, error) {
	old := c.applied()
	now := c.now()
	nt, _ := c.previewTopology(d)
	rv := &Review{From: old.version, To: old.version + 1, Origin: d.Origin, Discarded: d.Discarded, Scope: d.Scope,
		Now: now.UTC(), Boundary: nextBoundary(now), Identities: []IdentityRow{}, Changes: []Change{},
		NotAffected: []string{}, Warnings: []Warning{}}
	topoChanged := normal(old.sc, "topology") != normal(d.Scenario, "topology") || len(d.Regenerate) > 0
	teleChanged := normal(old.sc, "telemetry") != normal(d.Scenario, "telemetry")
	rv.RebuildsVCenter = topoChanged
	if !topoChanged {
		rv.To = old.version
	}
	rv.SessionsDropped = topoChanged

	oldProj, newProj := Project(c.cat, old.topo), Project(c.cat, nt)
	rv.Before, rv.After = oldProj.Selected(d.Scope), newProj.Selected(d.Scope)
	rv.HostsBefore, rv.HostsAfter = oldProj.Hosts, newProj.Hosts
	rv.RegisteredBefore = c.reg.Len()

	if topoChanged {
		rv.Changes = append(rv.Changes, topologyChanges(old, nt, d, oldProj, newProj)...)
		rv.Identities = identityRows(old.topo, nt, oldProj, newProj)
		if old.sc.Seed == d.Scenario.Seed {
			rv.Renumbered = renumbered(old.topo, nt)
		}
		rv.Identities = withMoRefRow(rv.Identities, rv.Renumbered)
	}
	if teleChanged {
		rv.Changes = append(rv.Changes, telemetryChanges(old.sc, d.Scenario)...)
	}
	if d.Scope != c.scope {
		rv.Changes = append(rv.Changes, Change{Object: "Collector", Field: "Metric scope", Before: scopeName(c.scope), After: scopeName(d.Scope),
			Effect: "projections only", Area: "Telemetry"})
	}
	for _, ch := range rv.Changes {
		if ch.Area == "Topology" {
			rv.TopologyCount++
		} else {
			rv.TelemetryCount++
		}
	}
	if len(rv.Changes) > 120 {
		rv.More = len(rv.Changes) - 120
		rv.Changes = rv.Changes[:120]
	}
	for _, r := range rv.Identities {
		// churn is an identity that goes away: additions only grow the inventory
		if r.Removed > 0 && r.Label != "Performance entities" && r.Label != "Virtual disk identities" {
			rv.Churn = true
		}
	}
	if rv.Renumbered.Total() > 0 {
		rv.Churn = true
	}
	rv.NotAffected = notAffected(old, nt, d, topoChanged, teleChanged, rv.Renumbered)
	rv.Warnings = warnings(old, nt, d, rv, newProj)
	return rv, nil
}

func scopeName(s string) string {
	if s == ScopeDashboard {
		return "Dashboard-only"
	}
	return "All modeled kinds"
}

// nextBoundary is the next five-minute bucket boundary after t.
func nextBoundary(t time.Time) time.Time {
	return t.UTC().Truncate(perf.Interval).Add(perf.Interval)
}

func specFields(s scenario.ClusterSpec) [][2]string {
	b := func(p *bool) string {
		if p != nil && *p {
			return "on"
		}
		return "off"
	}
	return [][2]string{
		{"Hosts", fmt.Sprint(s.Hosts)},
		{"VMs per host", fmt.Sprint(scenario.Int(s.VMs.PerHost))},
		{"Virtual disks per VM", fmt.Sprint(s.VMs.DisksPerVM)},
		{"NVMe devices per host", fmt.Sprint(s.Host.ClaimedDevices)},
		{"Local devices per host", fmt.Sprint(scenario.Int(s.Host.LocalDevices))},
		{"Logical CPUs per host", fmt.Sprint(s.Host.Threads)},
		{"DOM worlds per host", fmt.Sprint(scenario.Int(s.Host.DomWorlds))},
		{"vSAN pNICs per host", fmt.Sprint(s.Host.VsanPnics)},
		{"Dedup", b(s.Dedup)},
		{"Compression", b(s.Compression)},
	}
}

func topologyChanges(old *running, nt *topology.Topology, d *Draft, op, np *Projection) []Change {
	var out []Change
	osc, nsc := old.sc, d.Scenario
	if osc.Seed != nsc.Seed {
		out = append(out, Change{Object: "Identity", Field: "Global seed", Before: osc.Seed, After: nsc.Seed,
			Effect: "replaces every identity", Area: "Topology"})
	}
	if osc.TelemetrySeed != nsc.TelemetrySeed {
		out = append(out, Change{Object: "Telemetry", Field: "Telemetry seed", Before: orDash(osc.TelemetrySeed), After: orDash(nsc.TelemetrySeed),
			Effect: "changes every sample", Area: "Topology"})
	}
	oc, nc := map[string]*topology.Cluster{}, map[string]*topology.Cluster{}
	for _, c := range old.topo.Clusters() {
		oc[c.Key] = c
	}
	for _, c := range nt.Clusters() {
		nc[c.Key] = c
	}
	regen := map[string]bool{}
	for _, k := range d.Regenerate {
		regen[k] = true
	}
	var keys []string
	for k := range oc {
		keys = append(keys, k)
	}
	for k := range nc {
		if _, ok := oc[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		o, n := oc[k], nc[k]
		switch {
		case o == nil:
			out = append(out, Change{Object: k, Field: "Cluster", Before: "—", After: fmt.Sprintf("%d hosts, %d VMs", len(n.Hosts), len(n.VMs)),
				Effect: "adds cluster", Area: "Topology"})
		case n == nil:
			out = append(out, Change{Object: k, Field: "Cluster", Before: fmt.Sprintf("%d hosts, %d VMs", len(o.Hosts), len(o.VMs)), After: "—",
				Effect: "removes cluster", Area: "Topology"})
		default:
			of, nf := specFields(o.Spec), specFields(n.Spec)
			for i := range of {
				if of[i][1] != nf[i][1] {
					eff := "changes cardinality"
					a, e1 := strconv.Atoi(of[i][1])
					b, e2 := strconv.Atoi(nf[i][1])
					if e1 == nil && e2 == nil && a != b {
						eff = "adds objects"
						if b < a {
							eff = "removes objects"
						}
					}
					out = append(out, Change{Object: k, Field: of[i][0], Before: of[i][1], After: nf[i][1], Effect: eff, Area: "Topology"})
				}
			}
			if regen[k] {
				out = append(out, Change{Object: k, Field: "Identity generation", Before: fmt.Sprintf("serial %d", o.Serial), After: fmt.Sprintf("serial %d", n.Serial),
					Effect: "new identities", Area: "Topology"})
			}
			if o.TelemetrySeed != n.TelemetrySeed {
				out = append(out, Change{Object: k, Field: "Cluster telemetry seed", Before: "epoch " + fmt.Sprint(old.sc.SeedEpoch(k)), After: "epoch " + fmt.Sprint(d.Scenario.SeedEpoch(k)),
					Effect: "changes samples and history", Area: "Topology"})
			}
		}
	}
	// datacenter and default edits that did not surface as a cluster change
	if len(out) == 0 {
		out = append(out, Change{Object: "Scenario", Field: "Defaults or datacenters", Before: "—", After: "edited", Effect: "rebuilds the vCenter", Area: "Topology"})
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "(seed)"
	}
	return s
}

func fmtDelay(d *scenario.Delay) string {
	if d == nil || (d.FixedMs == 0 && d.JitterMs == 0) {
		return "none"
	}
	return fmt.Sprintf("%d ms +0–%d", d.FixedMs, d.JitterMs)
}

func faultText(f scenario.Fault) string {
	t := f.Family
	if t == "" {
		t = f.Method
	}
	where := "all clusters"
	if len(f.Clusters) > 0 {
		where = strings.Join(f.Clusters, ", ")
	}
	return fmt.Sprintf("%s → %s on %s", t, f.Fault, where)
}

func telemetryChanges(o, n *scenario.Scenario) []Change {
	var out []Change
	add := func(obj, field, before, after, eff string) {
		out = append(out, Change{Object: obj, Field: field, Before: before, After: after, Effect: eff, Area: "Telemetry"})
	}
	if a, b := fmtDelay(o.Latency.PerfQuery), fmtDelay(n.Latency.PerfQuery); a != b {
		add("Latency", "VsanPerfQueryPerf", a, b, "history preserved")
	}
	if a, b := fmtDelay(o.Latency.SDK), fmtDelay(n.Latency.SDK); a != b {
		add("Latency", "/sdk", a, b, "history preserved")
	}
	fams := map[string]bool{}
	for f := range o.Latency.Families {
		fams[f] = true
	}
	for f := range n.Latency.Families {
		fams[f] = true
	}
	var names []string
	for f := range fams {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		od, oo := o.Latency.Families[f]
		nd, no := n.Latency.Families[f]
		var a, b *scenario.Delay
		if oo {
			a = &od
		}
		if no {
			b = &nd
		}
		if fmtDelay(a) != fmtDelay(b) {
			add("Latency", f, fmtDelay(a), fmtDelay(b), "history preserved")
		}
	}
	ofs, nfs := map[string]scenario.Fault{}, map[string]scenario.Fault{}
	enc := func(f scenario.Fault) string { b, _ := json.Marshal(f); return string(b) }
	for _, f := range o.Faults {
		ofs[enc(f)] = f
	}
	for _, f := range n.Faults {
		nfs[enc(f)] = f
	}
	for k, f := range ofs {
		if _, ok := nfs[k]; !ok {
			add("Fault rule", faultText(f), "active", "removed", "history preserved")
		}
	}
	for k, f := range nfs {
		if _, ok := ofs[k]; !ok {
			add("Fault rule", faultText(f), "—", "added", "history preserved")
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Object+out[i].Field < out[j].Object+out[j].Field })
	return out
}

// identityRows compares two topologies by (key, serial).
func identityRows(o, n *topology.Topology, op, np *Projection) []IdentityRow {
	type id struct {
		key    string
		serial int
	}
	set := func(t *topology.Topology, pick func(*topology.Cluster) []id) map[id]bool {
		m := map[id]bool{}
		for _, c := range t.Clusters() {
			for _, x := range pick(c) {
				m[x] = true
			}
		}
		return m
	}
	row := func(label string, pick func(*topology.Cluster) []id) IdentityRow {
		a, b := set(o, pick), set(n, pick)
		r := IdentityRow{Label: label}
		for k := range a {
			if b[k] {
				r.Kept++
			} else {
				r.Removed++
			}
		}
		for k := range b {
			if !a[k] {
				r.Added++
			}
		}
		return r
	}
	rows := []IdentityRow{
		row("Host UUIDs · vSAN node UUIDs", func(c *topology.Cluster) (out []id) {
			for _, h := range c.Hosts {
				out = append(out, id{h.Key, h.Serial})
			}
			return
		}),
		row("NVMe disk UUIDs", func(c *topology.Cluster) (out []id) {
			for _, h := range c.Hosts {
				for _, d := range h.Devices {
					if d.Claimed {
						out = append(out, id{d.Key, d.Serial})
					}
				}
			}
			return
		}),
		row("VM UUIDs · instanceUuid", func(c *topology.Cluster) (out []id) {
			for _, v := range c.VMs {
				out = append(out, id{v.Key, v.Serial})
			}
			return
		}),
		row("Virtual disk identities", func(c *topology.Cluster) (out []id) {
			for _, v := range c.VMs {
				for i := range v.Disks {
					out = append(out, id{fmt.Sprintf("%s#%d", v.Key, i), v.Serial})
				}
			}
			return
		}),
		row("Cluster UUIDs · vSAN datastores", func(c *topology.Cluster) []id { return []id{{c.Key, c.Serial}} }),
	}
	// performance entities: a cluster that keeps its identity keeps its
	// entities up to the smaller of the two counts
	pe := IdentityRow{Label: "Performance entities"}
	oc, nc := map[string]*topology.Cluster{}, map[string]*topology.Cluster{}
	for _, c := range o.Clusters() {
		oc[c.Key] = c
	}
	for _, c := range n.Clusters() {
		nc[c.Key] = c
	}
	for k, c := range oc {
		ol := op.PerCluster[k].Entities
		if x, ok := nc[k]; ok && x.Serial == c.Serial {
			nl := np.PerCluster[k].Entities
			pe.Kept += min(ol, nl)
			if nl > ol {
				pe.Added += nl - ol
			} else {
				pe.Removed += ol - nl
			}
		} else {
			pe.Removed += ol
		}
	}
	for k, c := range nc {
		if x, ok := oc[k]; !ok || x.Serial != c.Serial {
			pe.Added += np.PerCluster[k].Entities
		}
	}
	return append(rows, pe)
}

func notAffected(old *running, nt *topology.Topology, d *Draft, topoChanged, teleChanged bool, rn Renumbered) []string {
	var out []string
	if topoChanged {
		touched := map[string]bool{}
		oc := map[string]*topology.Cluster{}
		for _, c := range old.topo.Clusters() {
			oc[c.Key] = c
		}
		kept := 0
		for _, c := range nt.Clusters() {
			if o, ok := oc[c.Key]; ok && o.Serial == c.Serial && len(o.Hosts) == len(c.Hosts) && len(o.VMs) == len(c.VMs) &&
				fmt.Sprint(specFields(o.Spec)) == fmt.Sprint(specFields(c.Spec)) && o.TelemetrySeed == c.TelemetrySeed {
				kept++
			} else {
				touched[c.Key] = true
			}
		}
		if kept > 0 {
			if rn.Total() > 0 {
				out = append(out, fmt.Sprintf("UUIDs of %d other clusters", kept))
			} else {
				out = append(out, fmt.Sprintf("%d other clusters", kept))
			}
		}
		if old.sc.Seed == d.Scenario.Seed {
			out = append(out, "seed "+d.Scenario.Seed)
		}
	} else {
		out = append(out, "every cluster, host, VM and identity", "sessions and the running vCenter")
	}
	if !teleChanged {
		out = append(out, "latency and fault rules")
	}
	out = append(out, "24 h of telemetry history (a pure function of seed, entity and bucket)")
	return out
}

func warnings(old *running, nt *topology.Topology, d *Draft, rv *Review, np *Projection) []Warning {
	var w []Warning
	if rv.Churn && old.sc.Seed != d.Scenario.Seed {
		w = append(w, Warning{"warn", "Applying replaces every identity",
			"A new seed gives every cluster, host, disk and VM a new identity. VCF Operations will see new objects and stop receiving data for the old ones."})
	}
	if rn := rv.Renumbered; rn.Total() > 0 {
		w = append(w, Warning{"warn", "MoRefs are renumbered",
			fmt.Sprintf("%s clusters, %s hosts and %s VMs that already exist are created after something this change removes, so they get new MoRefs. Their UUIDs do not change. The VCF Operations vCenter adapter keys on MoRefs and will see them as new objects; the vSAN pack keys on UUIDs and will not.",
				num(rn.Clusters), num(rn.Hosts), num(rn.VMs))})
	}
	if rv.RebuildsVCenter && rv.After.Entities > 20000 {
		w = append(w, Warning{"warn", "Large fan-out",
			fmt.Sprintf("%s entities per cycle (projected, not measured). Past about 20,000 the collection cycle approaches the five-minute window on the reference pack.", num(rv.After.Entities))})
	}
	if rv.SessionsDropped {
		w = append(w, Warning{"info", "Sessions end at the swap",
			"The new vCenter starts with an empty session store. A client mid-cycle at the boundary logs in again, as it would after a vCenter restart."})
	}
	for _, k := range d.Regenerate {
		w = append(w, Warning{"warn", "Regenerate " + k, "Every host, disk and VM UUID in this cluster is replaced. VCF Operations treats them as new objects."})
	}
	if !rv.RebuildsVCenter && rv.TelemetryCount > 0 {
		w = append(w, Warning{"ok", "Telemetry only", "The running vCenter is not rebuilt. History and identities are untouched."})
	}
	if rv.Discarded != "" {
		w = append(w, Warning{"warn", "Replaces a pending draft", rv.Discarded})
	}
	if !rv.Churn && rv.RebuildsVCenter && len(rv.Identities) > 0 {
		w = append(w, Warning{"ok", "No identity churn", "Nothing is removed or renamed. Restarting the simulator after the apply keeps every identity."})
	}
	return w
}

func num(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// withMoRefRow inserts the MoRef row after the host row. MoRefs are a
// different thing from UUIDs: vcsim numbers them in creation order, so
// kept UUIDs do not mean kept MoRefs.
func withMoRefRow(rows []IdentityRow, rn Renumbered) []IdentityRow {
	var hostRow IdentityRow
	var vmRow IdentityRow
	var clRow IdentityRow
	for _, r := range rows {
		switch {
		case strings.HasPrefix(r.Label, "Host"):
			hostRow = r
		case strings.HasPrefix(r.Label, "VM"):
			vmRow = r
		case strings.HasPrefix(r.Label, "Cluster"):
			clRow = r
		}
	}
	row := IdentityRow{Label: "MoRefs · clusters, hosts, VMs",
		Added: clRow.Added + hostRow.Added + vmRow.Added, Removed: clRow.Removed + hostRow.Removed + vmRow.Removed,
		Renumbered: rn.Total()}
	row.Kept = clRow.Kept + hostRow.Kept + vmRow.Kept - row.Renumbered
	out := make([]IdentityRow, 0, len(rows)+1)
	for _, r := range rows {
		out = append(out, r)
		if strings.HasPrefix(r.Label, "Host") {
			out = append(out, row)
		}
	}
	return out
}
