package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/perf"
	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/sentania/vcenter-api-simulator/internal/vsanhealth"
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
	sc, err := decodeScenarioJSON(raw, c.base)
	if err == nil {
		previous := c.applied().sc
		if c.draft != nil {
			previous = c.draft.Scenario
		}
		_, err = reconcileStorage(sc, previous)
	}
	if err == nil {
		err = sc.Validate()
	}
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
	previous := d.Scenario
	sc := clone(previous)
	sc.Removed = append(sc.Removed, key)
	sort.Strings(sc.Removed)
	var cl []scenario.ClusterOverride
	for _, o := range sc.Clusters {
		if o.Key != key {
			cl = append(cl, o)
		}
	}
	sc.Clusters = cl
	sc.Faults = retainFaults(sc.Faults, func(k string) bool { return k != key })
	if _, err := reconcileStorage(sc, previous); err != nil {
		return DraftView{}, &Invalid{err.Error()}
	}
	if err := sc.Validate(); err != nil {
		return DraftView{}, &Invalid{err.Error()}
	}
	d.Scenario = sc
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
	Label    string `json:"label"`
	Kept     int    `json:"kept"`
	Added    int    `json:"added"`
	Restored int    `json:"restored"`
	Removed  int    `json:"removed"`
}

// Warning is a note the operator should read before applying.
type Warning struct {
	Severity string `json:"severity"` // warn | info | ok
	Title    string `json:"title"`
	Text     string `json:"text"`
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
	}
	return topology.Generate(d.Scenario, reg), reg
}

func (c *Controller) review(d *Draft) (*Review, error) {
	if err := clone(d.Scenario).Validate(); err != nil {
		return nil, &Invalid{err.Error()}
	}
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
		var previous *registry.Registry
		if old.sc.Seed == d.Scenario.Seed {
			previous = c.reg
		}
		rv.Identities = identityRows(old.topo, nt, c.cat, previous)
		rv.Identities = withMoRefRow(rv.Identities)
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
	for _, r := range rv.Identities {
		// churn is an identity that goes away: additions only grow the inventory
		if r.Removed > 0 && r.Label != "Performance entities" && r.Label != "Virtual disk identities" {
			rv.Churn = true
		}
	}
	for _, change := range rv.Changes {
		if change.Field == "Architecture" {
			for _, row := range rv.Identities {
				if row.Label == "Performance entities" && row.Removed > 0 {
					rv.Churn = true
				}
			}
		}
	}
	if len(rv.Changes) > 120 {
		kept := make([]Change, 0, len(rv.Changes))
		for _, ch := range rv.Changes {
			if ch.Field == "NFS datastore" {
				kept = append(kept, ch)
			}
		}
		for _, ch := range rv.Changes {
			if ch.Field != "NFS datastore" {
				if len(kept) < 120 {
					kept = append(kept, ch)
				} else {
					rv.More++
				}
			}
		}
		rv.Changes = kept
	}
	rv.NotAffected = notAffected(old, nt, d, topoChanged, teleChanged)
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
	architecture := s.Architecture
	if architecture == "" {
		architecture = "esa"
	}
	primary := s.StatsPrimary == nil || *s.StatsPrimary
	return [][2]string{
		{"Architecture", strings.ToUpper(architecture)},
		{"Stats primary", map[bool]string{true: "online", false: "offline"}[primary]},
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
	out = append(out, nfsChanges(osc, nsc)...)
	out = append(out, iscsiChanges(osc, nsc)...)
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
					if of[i][0] == "Architecture" {
						eff = "changes performance identities"
					}
					if of[i][0] == "Stats primary" {
						eff = "changes sample availability"
					}
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

func iscsiChanges(old, next *scenario.Scenario) []Change {
	collect := func(sc *scenario.Scenario) map[string]scenario.ISCSIStorage {
		out := map[string]scenario.ISCSIStorage{}
		for _, dc := range sc.Datacenters {
			for _, st := range dc.ISCSI {
				out[st.Cluster] = st
			}
		}
		return out
	}
	a, b := collect(old), collect(next)
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	var changes []Change
	describe := func(st scenario.ISCSIStorage, ok bool) string {
		if !ok {
			return "—"
		}
		var adapters, targets []string
		for _, a := range st.Adapters {
			adapters = append(adapters, fmt.Sprintf("%s (%s)", a.Name, a.Kind))
		}
		for _, t := range st.Targets {
			port := t.Port
			if port == 0 {
				port = 3260
			}
			targets = append(targets, fmt.Sprintf("%s at %s:%d", t.Name, t.Address, port))
		}
		var stores []string
		for _, l := range st.LUNs {
			var paths []string
			for _, p := range l.Paths {
				host := shortStorageHost(p.Host)
				if host == "" {
					host = "all hosts"
				}
				paths = append(paths, fmt.Sprintf("%s on %s: %s", p.Adapter, host, p.State))
			}
			policy := map[string]string{"": "round robin", "VMW_PSP_RR": "round robin", "VMW_PSP_FIXED": "fixed", "VMW_PSP_MRU": "most recently used"}[l.Policy]
			stores = append(stores, fmt.Sprintf("%s uses %s, LUN %d. %s free of %s, %s policy. Paths: %s", l.Name, l.Target, l.Number, humanStorageBytes(l.FreeSpace), humanStorageBytes(l.Capacity), policy, strings.Join(paths, ", ")))
		}
		return fmt.Sprintf("Adapters per host: %s. Targets: %s. Datastores: %s", strings.Join(adapters, ", "), strings.Join(targets, ", "), strings.Join(stores, ", "))
	}
	for _, k := range ordered {
		before, had := a[k]
		after, has := b[k]
		if had && has && reflect.DeepEqual(before, after) {
			continue
		}
		changes = append(changes, Change{Object: k, Field: "iSCSI storage", Before: describe(before, had), After: describe(after, has), Effect: "updates shared datastores, adapters and paths", Area: "Topology"})
	}
	return changes
}

// nfsChanges compares every datacenter regardless of whether the draft came
// from the editor, a generator, or a replacement preset.
func nfsChanges(old, next *scenario.Scenario) []Change {
	type entry struct {
		dc   string
		spec scenario.NFSDatastore
	}
	collect := func(sc *scenario.Scenario) map[string]entry {
		out := map[string]entry{}
		for _, dc := range sc.Datacenters {
			for _, n := range dc.NFS {
				out[dc.Name+"/"+n.Name] = entry{dc.Name, n}
			}
		}
		return out
	}
	before, after := collect(old), collect(next)
	keys := make([]string, 0, len(before)+len(after))
	for key := range before {
		keys = append(keys, key)
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	describe := func(n scenario.NFSDatastore) string {
		scope := "Available to all clusters"
		if len(n.Clusters) > 0 {
			names := make([]string, 0, len(n.Clusters))
			for _, k := range n.Clusters {
				names = append(names, shortStorageHost(k))
			}
			scope = "Available to " + strings.Join(names, ", ")
		}
		var faults []string
		for _, h := range n.InaccessibleHosts {
			faults = append(faults, shortStorageHost(h))
		}
		state := "All hosts can reach it"
		if len(faults) > 0 {
			state = "Cannot be reached by " + strings.Join(faults, ", ")
		}
		return fmt.Sprintf("%s:%s. %s free of %s. %s. %s.", n.RemoteHost, n.RemotePath, humanStorageBytes(n.FreeSpace), humanStorageBytes(n.Capacity), scope, state)
	}
	var changes []Change
	for _, key := range keys {
		a, had := before[key]
		b, has := after[key]
		switch {
		case !had:
			changes = append(changes, Change{Object: key, Field: "NFS datastore", Before: "—", After: describe(b.spec), Effect: "adds datastore and mounts on datacenter hosts", Area: "Topology"})
		case !has:
			changes = append(changes, Change{Object: key, Field: "NFS datastore", Before: describe(a.spec), After: "—", Effect: "removes datastore and host mounts", Area: "Topology"})
		case !reflect.DeepEqual(a.spec, b.spec):
			changes = append(changes, Change{Object: key, Field: "NFS datastore", Before: describe(a.spec), After: describe(b.spec), Effect: "updates datastore and host mounts", Area: "Topology"})
		}
	}
	return changes
}

func shortStorageHost(key string) string {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[i+1:]
	}
	return key
}
func humanStorageBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
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
func identityRows(o, n *topology.Topology, cat *catalog.Catalog, previous *registry.Registry) []IdentityRow {
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
	row := func(label, prefix string, pick func(*topology.Cluster) []id) IdentityRow {
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
				key := prefix + k.key
				if prefix != "" && previous != nil && previous.Has(key) && previous.Serial(key) == k.serial {
					r.Restored++
				} else {
					r.Added++
				}
			}
		}
		return r
	}
	nfsIDs := func(t *topology.Topology) map[id]bool {
		out := map[id]bool{}
		for _, dc := range t.Datacenters {
			for _, store := range dc.NFS {
				out[id{"nfs/" + dc.Name + "/" + store.Spec.Name, store.Serial}] = true
			}
		}
		return out
	}
	nfsBefore, nfsAfter := nfsIDs(o), nfsIDs(n)
	nfsRow := IdentityRow{Label: "NFS datastore identities"}
	for id := range nfsBefore {
		if nfsAfter[id] {
			nfsRow.Kept++
		} else {
			nfsRow.Removed++
		}
	}
	for id := range nfsAfter {
		if !nfsBefore[id] {
			if previous != nil && previous.Has(id.key) && previous.Serial(id.key) == id.serial {
				nfsRow.Restored++
			} else {
				nfsRow.Added++
			}
		}
	}
	iscsiIDs := func(t *topology.Topology) map[id]bool {
		out := map[id]bool{}
		for _, dc := range t.Datacenters {
			for _, st := range dc.ISCSI {
				for _, lun := range st.LUNs {
					out[id{"iscsi/" + st.Spec.Cluster + "/" + lun.Spec.Name, lun.Serial}] = true
				}
			}
		}
		return out
	}
	iscsiBefore, iscsiAfter := iscsiIDs(o), iscsiIDs(n)
	iscsiRow := IdentityRow{Label: "iSCSI datastore and LUN identities"}
	for id := range iscsiBefore {
		if iscsiAfter[id] {
			iscsiRow.Kept++
		} else {
			iscsiRow.Removed++
		}
	}
	for id := range iscsiAfter {
		if !iscsiBefore[id] {
			if previous != nil && previous.Has(id.key) && previous.Serial(id.key) == id.serial {
				iscsiRow.Restored++
			} else {
				iscsiRow.Added++
			}
		}
	}
	rows := []IdentityRow{
		nfsRow,
		iscsiRow,
		row("Host UUIDs · vSAN node UUIDs", "host/", func(c *topology.Cluster) (out []id) {
			for _, h := range c.Hosts {
				out = append(out, id{h.Key, h.Serial})
			}
			return
		}),
		row("NVMe disk UUIDs", "device/", func(c *topology.Cluster) (out []id) {
			for _, h := range c.Hosts {
				for _, d := range h.Devices {
					if d.Claimed {
						out = append(out, id{d.Key, d.Serial})
					}
				}
			}
			return
		}),
		row("VM UUIDs · instanceUuid", "vm/", func(c *topology.Cluster) (out []id) {
			for _, v := range c.VMs {
				out = append(out, id{v.Key, v.Serial})
			}
			return
		}),
		row("Virtual disk identities", "", func(c *topology.Cluster) (out []id) {
			for _, v := range c.VMs {
				for i := range v.Disks {
					out = append(out, id{fmt.Sprintf("%s#%d", v.Key, i), v.Serial})
				}
			}
			return
		}),
		row("Cluster UUIDs · vSAN datastores", "cluster/", func(c *topology.Cluster) []id { return []id{{c.Key, c.Serial}} }),
	}
	// Compare actual family/ref pairs for clusters that changed, so
	// architecture changes account for replaced disk and performance
	// entities. Clusters that did not change keep every entity.
	pe := IdentityRow{Label: "Performance entities"}
	oc := map[string]*topology.Cluster{}
	for _, c := range o.Clusters() {
		oc[c.Key] = c
	}
	perfSet := func(cl *topology.Cluster) map[string]bool {
		out := map[string]bool{}
		// A missing stats primary hides samples, but does not destroy
		// the underlying performance identities.
		visible := *cl
		visible.Spec.StatsPrimary = nil
		for _, family := range vsanhealth.PerfFamilyNames(cat, &visible) {
			refs, _ := vsanhealth.PerfEntities(cat, &visible, family)
			for _, ref := range refs {
				out[ref] = true
			}
		}
		return out
	}
	seen := map[string]bool{}
	for _, c := range n.Clusters() {
		seen[c.Key] = true
		old, ok := oc[c.Key]
		if ok && sameCluster(old, c) {
			visible := *c
			visible.Spec.StatsPrimary = nil
			for _, family := range vsanhealth.PerfFamilyNames(cat, &visible) {
				k, _ := vsanhealth.PerfEntityCount(cat, &visible, family)
				pe.Kept += k
			}
			continue
		}
		after := perfSet(c)
		var before map[string]bool
		if ok {
			before = perfSet(old)
		}
		for ref := range before {
			if after[ref] {
				pe.Kept++
			} else {
				pe.Removed++
			}
		}
		for ref := range after {
			if !before[ref] {
				pe.Added++
			}
		}
	}
	for key, old := range oc {
		if !seen[key] {
			pe.Removed += len(perfSet(old))
		}
	}
	return append(rows, pe)
}

func sameCluster(o, c *topology.Cluster) bool {
	return o.Serial == c.Serial && len(o.Hosts) == len(c.Hosts) && len(o.VMs) == len(c.VMs) &&
		fmt.Sprint(specFields(o.Spec)) == fmt.Sprint(specFields(c.Spec)) && o.TelemetrySeed == c.TelemetrySeed
}

func notAffected(old *running, nt *topology.Topology, d *Draft, topoChanged, teleChanged bool) []string {
	var out []string
	if topoChanged {
		touched := map[string]bool{}
		oc := map[string]*topology.Cluster{}
		for _, c := range old.topo.Clusters() {
			oc[c.Key] = c
		}
		affectedDC := map[string]bool{}
		for _, change := range nfsChanges(old.sc, d.Scenario) {
			affectedDC[strings.SplitN(change.Object, "/", 2)[0]] = true
		}
		for _, change := range iscsiChanges(old.sc, d.Scenario) {
			touched[change.Object] = true
		}
		kept := 0
		for _, c := range nt.Clusters() {
			if o, ok := oc[c.Key]; ok && sameCluster(o, c) && !affectedDC[c.Datacenter.Name] && !touched[c.Key] {
				kept++
			} else {
				touched[c.Key] = true
			}
		}
		if kept > 0 {
			out = append(out, fmt.Sprintf("%d other clusters", kept))
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

// withMoRefRow inserts the MoRef row after the host row.
func withMoRefRow(rows []IdentityRow) []IdentityRow {
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
		Kept:  clRow.Kept + hostRow.Kept + vmRow.Kept,
		Added: clRow.Added + hostRow.Added + vmRow.Added, Restored: clRow.Restored + hostRow.Restored + vmRow.Restored, Removed: clRow.Removed + hostRow.Removed + vmRow.Removed}
	out := make([]IdentityRow, 0, len(rows)+1)
	for _, r := range rows {
		out = append(out, r)
		if strings.HasPrefix(r.Label, "Host") {
			out = append(out, row)
		}
	}
	return out
}
