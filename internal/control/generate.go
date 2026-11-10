package control

import (
	"fmt"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// GenParams is the quick generator's form: a uniform start, to be
// overridden cluster by cluster in the editor. Zero values take the
// scenario defaults.
type GenParams struct {
	Seed           string                `json:"seed"`
	Architecture   string                `json:"architecture"`
	StatsPrimary   *bool                 `json:"statsPrimary"`
	Datacenters    []scenario.Datacenter `json:"datacenters"`
	Hosts          int                   `json:"hosts"`
	VMsPerHost     *int                  `json:"vmsPerHost"`
	DisksPerVM     int                   `json:"disksPerVM"`
	ClaimedDevices int                   `json:"claimedDevices"`
	LocalDevices   *int                  `json:"localDevices"`
	Threads        int                   `json:"threads"`
	DomWorlds      *int                  `json:"domWorlds"`
	VsanPnics      int                   `json:"vsanPnics"`
	Dedup          *bool                 `json:"dedup"`
	Compression    *bool                 `json:"compression"`
}

// generated builds the scenario a GenParams describes: its own datacenters
// and defaults, no overrides, the applied scenario's faults, latency, auth
// and TLS.
func (c *Controller) generated(p GenParams) (*scenario.Scenario, error) {
	base := c.applied().sc
	sc := clone(base)
	sc.Datacenters = p.Datacenters
	architecture := p.Architecture
	if architecture == "" {
		architecture = base.Defaults.Architecture
		clusters := c.applied().topo.Clusters()
		if len(clusters) > 0 {
			first := clusters[0].Spec.Architecture
			uniform := true
			for _, cl := range clusters[1:] {
				if cl.Spec.Architecture != first {
					uniform = false
					break
				}
			}
			if uniform {
				architecture = first
			}
		}
	}
	primary := p.StatsPrimary
	if primary == nil {
		primary = base.Defaults.StatsPrimary
		clusters := c.applied().topo.Clusters()
		if len(clusters) > 0 {
			first := clusters[0].Spec.StatsPrimary == nil || *clusters[0].Spec.StatsPrimary
			uniform := true
			for _, cl := range clusters[1:] {
				if (cl.Spec.StatsPrimary == nil || *cl.Spec.StatsPrimary) != first {
					uniform = false
					break
				}
			}
			if uniform {
				primary = &first
			}
		}
	}
	sc.Defaults = scenario.ClusterSpec{
		Architecture: architecture, StatsPrimary: primary, Hosts: p.Hosts, Dedup: p.Dedup, Compression: p.Compression,
		Host: scenario.HostSpec{Threads: p.Threads, DomWorlds: p.DomWorlds, VsanPnics: p.VsanPnics,
			ClaimedDevices: p.ClaimedDevices, LocalDevices: p.LocalDevices},
		VMs: scenario.VMSpec{PerHost: p.VMsPerHost, DisksPerVM: p.DisksPerVM},
	}
	sc.Vary, sc.Clusters, sc.Removed = scenario.Vary{}, nil, nil
	if p.Seed != "" {
		sc.Seed = p.Seed
	}
	// faults and latency name clusters that may not exist any more
	known := map[string]bool{}
	for _, k := range sc.ClusterKeys() {
		known[k] = true
	}
	sc.Faults = retainFaults(sc.Faults, func(k string) bool { return known[k] })
	if err := sc.Validate(); err != nil {
		return nil, &Invalid{err.Error()}
	}
	return sc, nil
}

// Preview is the generator's live projection.
type Preview struct {
	Projection *Projection `json:"projection"`
	Scope      string      `json:"scope"`
	Selected   Load        `json:"selected"`
	Other      Load        `json:"other"` // the other scope, for "Compare scopes"
	Identity   string      `json:"identity"`
	Warnings   []Warning   `json:"warnings"`
}

// PreviewGenerate projects a generator form without creating a draft.
func (c *Controller) PreviewGenerate(p GenParams) (*Preview, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sc, err := c.generated(p)
	if err != nil {
		return nil, err
	}
	topo := topology.Generate(sc, registry.InMemory(sc.Seed))
	proj := Project(c.cat, topo)
	pv := &Preview{Projection: proj, Scope: c.scope, Selected: proj.Selected(c.scope)}
	other := ScopeDashboard
	if c.scope == ScopeDashboard {
		other = ScopeAll
	}
	pv.Other = proj.Selected(other)
	if sc.Seed == c.applied().sc.Seed {
		pv.Identity = "same seed: clusters that keep their number and shape keep their identities"
	} else {
		pv.Identity = "new seed: every identity is replaced"
	}
	if pv.Selected.Entities > 20000 {
		pv.Warnings = append(pv.Warnings, Warning{"warn", "Large fan-out",
			fmt.Sprintf("%s entities per cycle (projected, not measured). This is a scale target, not yet a measured, supported profile.", num(pv.Selected.Entities))})
	}
	return pv, nil
}

// Generate replaces the pending draft with a generated topology.
func (c *Controller) Generate(p GenParams) (DraftView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scheduled != nil {
		return DraftView{}, invalid("a change is scheduled for %s; cancel it first", c.scheduled.At.UTC().Format("15:04:05Z"))
	}
	sc, err := c.generated(p)
	if err != nil {
		return DraftView{}, err
	}
	return c.replaceDraft(sc, "generator"), nil
}

// replaceDraft installs sc as the draft, noting what it replaced.
func (c *Controller) replaceDraft(sc *scenario.Scenario, origin string) DraftView {
	r := c.applied()
	note := ""
	if c.draft != nil {
		d := c.draft
		if rv, err := c.review(d); err == nil && len(rv.Changes) > 0 {
			first := rv.Changes[0]
			note = fmt.Sprintf("The pending draft (%s %s %s → %s, %d change(s)) is discarded.", first.Object, first.Field, first.Before, first.After, len(rv.Changes))
		}
	}
	scope := c.scope
	if c.draft != nil {
		scope = c.draft.Scope // the operator's pending scope choice survives a replacement
	}
	c.draft = &Draft{Scenario: sc, Scope: scope, BasedOn: r.version, Origin: origin, Discarded: note, Created: c.now()}
	return c.draftView()
}

// ---------------------------------------------------------------------------
// presets

// PresetInfo describes a scenario preset.
type PresetInfo struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Kind        string `json:"kind"` // topology | telemetry
	Effect      string `json:"effect"`
}

type preset struct {
	PresetInfo
	yaml string
	// telemetry presets keep the applied topology and replace faults and
	// latency from yaml
}

var presets = []preset{
	{PresetInfo{"osa", "Three-host OSA", "A three-host OSA cluster with its performance primary online.", "topology", "replaces every identity"}, `
seed: osa
datacenters: [{name: dc-01, clusters: 1}]
defaults:
  architecture: osa
  statsPrimary: true
  hosts: 3
  host: {threads: 5, domWorlds: 14, vsanPnics: 1, claimedDevices: 1, localDevices: 1}
  vms: {perHost: 1, disksPerVM: 1}
`},
	{PresetInfo{"phase1", "Phase 1 reference", "One datacenter, one four-host ESA cluster: the shape the reference pack was validated against.", "topology", "replaces every identity"}, `
seed: phase1
datacenters: [{name: dc-01, clusters: 1}]
defaults:
  hosts: 4
  host: {threads: 16, domWorlds: 12, vsanPnics: 2, claimedDevices: 1, localDevices: 1}
  vms: {perHost: 1, disksPerVM: 1}
`},
	{PresetInfo{"ten-clusters", "Ten varied clusters", "The Phase 2 acceptance scenario: ten clusters that each draw their own host, VM and device counts; two are pinned.", "topology", "replaces every identity"}, `
seed: ten-clusters
datacenters: [{name: dc-01, clusters: 10}]
defaults:
  hosts: 8
  host: {threads: 32, domWorlds: 66, vsanPnics: 2, claimedDevices: 1, localDevices: 1}
  vms: {perHost: 4, disksPerVM: 1}
vary:
  hosts: [4, 6, 8, 8, 12]
  vmsPerHost: {min: 2, max: 8}
  claimedDevices: [1, 1, 2]
  domWorlds: {min: 40, max: 90}
clusters:
  - {key: dc-01/cluster-01, hosts: 4}
  - {key: dc-01/cluster-03, hosts: 16, dedup: false, compression: false}
`},
	{PresetInfo{"scale-50", "Fifty clusters × 8 hosts", "Two datacenters, 50 uniform clusters of 8 hosts, 10 VMs per host. A scale target, not a measured profile.", "topology", "replaces every identity"}, `
seed: scale-50
datacenters: [{name: dc-east, clusters: 30}, {name: dc-west, clusters: 20}]
defaults:
  hosts: 8
  host: {threads: 64, domWorlds: 80, vsanPnics: 2, claimedDevices: 2, localDevices: 1}
  vms: {perHost: 10, disksPerVM: 2}
`},
	{PresetInfo{"clean", "Clean collector", "No faults and no added latency.", "telemetry", "telemetry only"}, `
seed: x
datacenters: [{name: dc-01, clusters: 1}]
`},
	{PresetInfo{"slow-collector", "Slow collector", "200 ms plus up to 50 ms of jitter on every performance query, and 5 ms plus up to 5 ms on /sdk.", "telemetry", "telemetry only"}, `
seed: x
datacenters: [{name: dc-01, clusters: 1}]
latency:
  perfQuery: {fixedMs: 200, jitterMs: 50}
  sdk: {fixedMs: 5, jitterMs: 5}
`},
	{PresetInfo{"family-fault", "One family fails", "vsan-pnic-net answers SystemError on every cluster: the pack reports a collection problem.", "telemetry", "telemetry only"}, `
seed: x
datacenters: [{name: dc-01, clusters: 1}]
faults:
  - {family: vsan-pnic-net, fault: system-error}
`},
	{PresetInfo{"feature-absent", "Kind not available", "vsan-pnic-net answers InvalidArgument on every cluster: the pack treats the kind as absent and skips it silently.", "telemetry", "telemetry only"}, `
seed: x
datacenters: [{name: dc-01, clusters: 1}]
faults:
  - {family: vsan-pnic-net, fault: invalid-argument}
`},
	{PresetInfo{"brown-out", "Collector brown-out", "host-domowner fails with SystemError and every query takes 300 ms plus up to 100 ms of jitter.", "telemetry", "telemetry only"}, `
seed: x
datacenters: [{name: dc-01, clusters: 1}]
latency:
  perfQuery: {fixedMs: 300, jitterMs: 100}
faults:
  - {family: host-domowner, fault: system-error}
`},
}

// Presets lists the scenario presets.
func (c *Controller) Presets() []PresetInfo {
	out := make([]PresetInfo, len(presets))
	for i, p := range presets {
		out[i] = p.PresetInfo
	}
	return out
}

// ApplyPreset replaces the draft with a preset. A topology preset brings
// its own datacenters, defaults and seed; a telemetry preset keeps the
// applied topology and replaces the faults and latency.
func (c *Controller) ApplyPreset(name string) (DraftView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scheduled != nil {
		return DraftView{}, invalid("a change is scheduled for %s; cancel it first", c.scheduled.At.UTC().Format("15:04:05Z"))
	}
	var p *preset
	for i := range presets {
		if presets[i].Name == name {
			p = &presets[i]
		}
	}
	if p == nil {
		return DraftView{}, invalid("unknown preset %q", name)
	}
	ps, err := scenario.Parse([]byte(p.yaml))
	if err != nil {
		return DraftView{}, err
	}
	var sc *scenario.Scenario
	// a preset builds on the pending draft when there is one, so loading it
	// never throws away edits the operator has not applied yet
	cur := c.draft
	base := c.applied().sc
	if cur != nil {
		base = cur.Scenario
	}
	if p.Kind == "topology" {
		sc = ps
		sc.Auth, sc.TLS = base.Auth, base.TLS
		sc.Faults, sc.Latency = base.Faults, base.Latency
		sc = clone(sc)
		// faults aimed at clusters the preset does not generate go with them
		known := map[string]bool{}
		for _, k := range sc.ClusterKeys() {
			known[k] = true
		}
		sc.Faults = retainFaults(sc.Faults, func(k string) bool { return known[k] })
	} else {
		sc = clone(base)
		sc.Faults, sc.Latency = ps.Faults, ps.Latency
	}
	if err := sc.Validate(); err != nil {
		return DraftView{}, &Invalid{err.Error()}
	}
	if cur != nil && p.Kind == "telemetry" {
		// pending topology edits, regeneration markers and the scope stay;
		// only the faults and latency change
		cur.Scenario = sc
		c.settle()
		return c.draftView(), nil
	}
	return c.replaceDraft(sc, "preset:"+name), nil
}

// retainFaults keeps the cluster selectors that known accepts. A rule whose
// selector names clusters and is left with none is dropped: an empty selector
// means every cluster, so keeping it would widen the fault.
func retainFaults(faults []scenario.Fault, known func(string) bool) []scenario.Fault {
	var out []scenario.Fault
	for _, f := range faults {
		var keep []string
		for _, k := range f.Clusters {
			if known(k) {
				keep = append(keep, k)
			}
		}
		if len(f.Clusters) > 0 && len(keep) == 0 {
			continue
		}
		f.Clusters = keep
		out = append(out, f)
	}
	return out
}
