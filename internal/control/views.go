package control

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/activity"
	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/sentania/vcenter-api-simulator/internal/vsanhealth"
)

// State is the overview's data.
type State struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Endpoint string `json:"endpoint"`
	API      []string
	Admin    string `json:"admin"`

	Profile struct {
		FullName string `json:"fullName"`
		Version  string `json:"version"`
		Build    string `json:"build"`
		Source   string `json:"source"`
	} `json:"profile"`
	Clock struct {
		Now    time.Time `json:"now"`
		Bucket time.Time `json:"bucket"`
		Next   time.Time `json:"nextBoundary"`
	} `json:"clock"`
	Running bool      `json:"running"`
	Since   time.Time `json:"since"`

	Topology struct {
		Version       int       `json:"version"`
		AppliedAt     time.Time `json:"appliedAt"`
		Seed          string    `json:"seed"`
		TelemetrySeed string    `json:"telemetrySeed"`
		Config        string    `json:"config"` // short hash of the applied scenario
		Datacenters   int       `json:"datacenters"`
		BuildMs       int64     `json:"buildMs"`
	} `json:"topology"`
	Pending Pending `json:"pending"`

	Projection *Projection `json:"projection"`
	Scope      string      `json:"scope"`
	Selected   Load        `json:"selected"`
	Overridden int         `json:"overriddenClusters"`
	Top        []FamilyProj

	Scenario struct {
		FaultRules int    `json:"faultRules"`
		Latency    string `json:"latency"`
	} `json:"scenario"`
	Auth struct {
		Mode  string   `json:"mode"`
		Users []string `json:"users"`
	} `json:"auth"`
	TLS     string `json:"tls"`
	Process struct {
		RSSBytes   uint64  `json:"rssBytes"`
		Cores      float64 `json:"cores"`
		Goroutines int     `json:"goroutines"`
	} `json:"process"`
	Activity activity.Summary `json:"activity"`
	Events   []Event          `json:"events"`
	LastErr  string           `json:"lastError,omitempty"`
}

// Event is one line of the overview's recent events.
type Event struct {
	At     time.Time `json:"at"`
	Text   string    `json:"text"`
	Detail string    `json:"detail"`
}

var lastCPU struct {
	t   time.Time
	cpu time.Duration
}

func cpuCores(now time.Time) float64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	prev := lastCPU
	lastCPU.t, lastCPU.cpu = now, cpu
	if prev.t.IsZero() || !now.After(prev.t) {
		return 0
	}
	return float64(cpu-prev.cpu) / float64(now.Sub(prev.t))
}

func rss() uint64 {
	if b, err := os.ReadFile("/proc/self/statm"); err == nil {
		f := strings.Fields(string(b))
		if len(f) > 1 {
			if n, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				return n * uint64(os.Getpagesize())
			}
		}
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Sys
}

// State assembles the overview.
func (c *Controller) State() *State {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.applied()
	now := c.now()
	s := &State{Version: c.opts.Version, Commit: c.opts.Commit, Endpoint: c.opts.Endpoint, API: c.opts.APIAddrs, Admin: c.opts.AdminAddr}
	s.Profile.FullName, s.Profile.Version, s.Profile.Build = c.cat.About.FullName, c.cat.About.Version, c.cat.About.Build
	s.Profile.Source = c.cat.Source
	s.Clock.Now, s.Clock.Bucket, s.Clock.Next = now.UTC(), bucket(now), nextBoundary(now)
	s.Running = !c.paused.Load()
	s.Since = c.started
	if !c.pausedAt.IsZero() {
		s.Since = c.pausedAt
	}
	s.Topology.Version, s.Topology.AppliedAt = r.version, r.at.UTC()
	s.Topology.Seed, s.Topology.TelemetrySeed = r.sc.Seed, r.topo.TelemetrySeed
	sum := sha256.Sum256([]byte(normal(r.sc)))
	s.Topology.Config = "sha256:" + hex.EncodeToString(sum[:])[:8]
	s.Topology.Datacenters = len(r.topo.Datacenters)
	s.Topology.BuildMs = c.buildMs
	s.Pending = c.pending()
	s.Projection = Project(c.cat, r.topo)
	s.Scope = c.scope
	s.Selected = s.Projection.Selected(c.scope)
	s.Overridden = len(r.sc.Clusters)
	for i, f := range s.Projection.Families {
		if i == 8 {
			break
		}
		s.Top = append(s.Top, f)
	}
	s.Scenario.FaultRules = len(r.sc.Faults)
	s.Scenario.Latency = latencyLine(r.sc.Latency)
	s.Auth.Users = []string{}
	if !c.auth.Snapshot().AnyLogin {
		for _, user := range c.auth.Users() {
			s.Auth.Users = append(s.Auth.Users, user.Name)
		}
	}
	sort.Strings(s.Auth.Users)
	s.Auth.Mode = "any user name and password are accepted"
	if len(s.Auth.Users) > 0 {
		s.Auth.Mode = fmt.Sprintf("restricted to %d user(s)", len(s.Auth.Users))
	}
	s.TLS = "self-signed, kept in the data directory"
	if r.sc.TLS.CertFile != "" {
		s.TLS = "provided certificate"
	}
	s.Process.RSSBytes, s.Process.Cores, s.Process.Goroutines = rss(), cpuCores(now), runtime.NumGoroutine()
	s.Activity = c.Log.Summarize(now)
	s.LastErr = c.lastErr
	s.Events = c.events(s.Activity)
	return s
}

func latencyLine(l scenario.Latency) string {
	var parts []string
	if l.PerfQuery != nil && (l.PerfQuery.FixedMs > 0 || l.PerfQuery.JitterMs > 0) {
		parts = append(parts, fmtDelay(l.PerfQuery)+" on perf queries")
	}
	if len(l.Families) > 0 {
		parts = append(parts, fmt.Sprintf("%d family override(s)", len(l.Families)))
	}
	if l.SDK != nil && (l.SDK.FixedMs > 0 || l.SDK.JitterMs > 0) {
		parts = append(parts, fmtDelay(l.SDK)+" on /sdk")
	}
	if len(parts) == 0 {
		return "none added"
	}
	return strings.Join(parts, " · ")
}

func (c *Controller) events(a activity.Summary) []Event {
	var out []Event
	for i := len(c.timeline) - 1; i >= 0 && len(out) < 4; i-- {
		t := c.timeline[i]
		out = append(out, Event{At: t.At, Text: t.Text, Detail: fmt.Sprintf("%s · history %s · identities %s", t.Area, t.History, t.Identities)})
	}
	for _, f := range a.Cycle.Findings {
		if f.Severity == "warn" || f.Severity == "bad" || f.Severity == "info" {
			out = append(out, Event{At: a.Cycle.End, Text: f.Title, Detail: f.Severity})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// Timeline returns the applied changes, newest first.
func (c *Controller) Timeline() []TimelineEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]TimelineEntry, len(c.timeline))
	for i, t := range c.timeline {
		out[len(c.timeline)-1-i] = t
	}
	return out
}

// ---------------------------------------------------------------------------
// topology

// TreeCluster is one cluster row.
type TreeCluster struct {
	Key          string `json:"key"`
	Architecture string `json:"architecture"`
	Name         string `json:"name"`
	Datacenter   string `json:"datacenter"`
	Hosts        int    `json:"hosts"`
	HostsBefore  int    `json:"hostsBefore"` // -1 when the cluster is new
	VMsPerHost   int    `json:"vmsPerHost"`
	Devices      int    `json:"devices"` // NVMe per host
	State        string `json:"state"`   // "" | pending | new | removed
	Override     bool   `json:"override"`
	Regenerate   bool   `json:"regenerate"`
	SeedReset    bool   `json:"seedReset"`
	Warnings     int    `json:"warnings"`
	Entities     int    `json:"entities"`
}

// TreeDC groups clusters.
type TreeDC struct {
	Name     string        `json:"name"`
	Clusters []TreeCluster `json:"clusters"`
	Hosts    int           `json:"hosts"`
}

// Tree is the cluster tree: the draft's clusters when a draft is pending,
// each flagged against what is applied.
type Tree struct {
	Pending     Pending  `json:"pending"`
	Datacenters []TreeDC `json:"datacenters"`
	Clusters    int      `json:"clusters"`
	Hosts       int      `json:"hosts"`
	Overridden  int      `json:"overridden"`
	PendingN    int      `json:"pendingClusters"`
	WarningsN   int      `json:"warningClusters"`
	Version     int      `json:"version"`
	Seed        string   `json:"seed"`
}

func (c *Controller) viewDraft() *Draft {
	if c.draft != nil {
		return c.draft
	}
	if c.scheduled != nil {
		return c.scheduled.Draft
	}
	return nil
}

// Tree returns the cluster tree.
func (c *Controller) Tree() *Tree {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.applied()
	topo, sc := r.topo, r.sc
	d := c.viewDraft()
	var proj *Projection
	if d != nil {
		topo, _ = c.previewTopology(d)
		sc = d.Scenario
	}
	proj = Project(c.cat, topo)
	old := map[string]*topology.Cluster{}
	for _, cl := range r.topo.Clusters() {
		old[cl.Key] = cl
	}
	regen := map[string]bool{}
	if d != nil {
		for _, k := range d.Regenerate {
			regen[k] = true
		}
	}
	t := &Tree{Pending: c.pending(), Version: r.version, Seed: sc.Seed}
	seen := map[string]bool{}
	for _, dc := range topo.Datacenters {
		td := TreeDC{Name: dc.Name}
		for _, cl := range dc.Clusters {
			seen[cl.Key] = true
			tc := treeCluster(cl, old[cl.Key], sc, regen[cl.Key])
			tc.Entities = proj.PerCluster[cl.Key].Entities
			tc.Warnings = len(clusterChecks(cl, tc.Entities))
			td.Clusters = append(td.Clusters, tc)
			td.Hosts += tc.Hosts
		}
		t.Datacenters = append(t.Datacenters, td)
	}
	// removed clusters stay visible until applied
	if d != nil {
		for _, cl := range r.topo.Clusters() {
			if seen[cl.Key] {
				continue
			}
			for i := range t.Datacenters {
				if t.Datacenters[i].Name == cl.Datacenter.Name {
					t.Datacenters[i].Clusters = append(t.Datacenters[i].Clusters, TreeCluster{Key: cl.Key, Name: cl.Name, Datacenter: cl.Datacenter.Name,
						Hosts: 0, HostsBefore: len(cl.Hosts), State: "removed"})
					sort.Slice(t.Datacenters[i].Clusters, func(a, b int) bool { return t.Datacenters[i].Clusters[a].Name < t.Datacenters[i].Clusters[b].Name })
				}
			}
		}
	}
	for _, dc := range t.Datacenters {
		for _, cl := range dc.Clusters {
			if cl.State != "removed" {
				t.Clusters++
			}
			t.Hosts += cl.Hosts
			if cl.Override {
				t.Overridden++
			}
			if cl.State != "" {
				t.PendingN++
			}
			if cl.Warnings > 0 {
				t.WarningsN++
			}
		}
	}
	return t
}

func storageForCluster(cl *topology.Cluster) (nfs []scenario.NFSDatastore, iscsi []scenario.ISCSIStorage) {
	if cl == nil {
		return nil, nil
	}
	for _, n := range cl.Datacenter.NFS {
		if len(n.Spec.Clusters) > 0 && !slices.Contains(n.Spec.Clusters, cl.Key) {
			continue
		}
		x := n.Spec
		x.Clusters = nil
		x.InaccessibleHosts = nil
		for _, h := range n.Spec.InaccessibleHosts {
			if strings.HasPrefix(h, cl.Key+"/") {
				x.InaccessibleHosts = append(x.InaccessibleHosts, h)
			}
		}
		nfs = append(nfs, x)
	}
	for _, st := range cl.Datacenter.ISCSI {
		if st.Spec.Cluster == cl.Key {
			iscsi = append(iscsi, st.Spec)
		}
	}
	return
}
func clusterStorageChanged(a, b *topology.Cluster) bool {
	an, ai := storageForCluster(a)
	bn, bi := storageForCluster(b)
	return !reflect.DeepEqual(an, bn) || !reflect.DeepEqual(ai, bi)
}

func treeCluster(cl, old *topology.Cluster, sc *scenario.Scenario, regen bool) TreeCluster {
	tc := TreeCluster{Key: cl.Key, Name: cl.Name, Datacenter: cl.Datacenter.Name, Architecture: cl.Spec.Architecture, Hosts: len(cl.Hosts), HostsBefore: -1,
		VMsPerHost: scenario.Int(cl.Spec.VMs.PerHost), Devices: cl.Spec.Host.ClaimedDevices, Regenerate: regen}
	_, tc.Override = sc.Override(cl.Key)
	tc.SeedReset = sc.SeedEpoch(cl.Key) > 0
	switch {
	case old == nil:
		tc.State = "new"
	default:
		tc.HostsBefore = len(old.Hosts)
		if fmt.Sprint(specFields(old.Spec)) != fmt.Sprint(specFields(cl.Spec)) || old.Serial != cl.Serial || old.TelemetrySeed != cl.TelemetrySeed || clusterStorageChanged(old, cl) {
			tc.State = "pending"
		}
	}
	return tc
}

// clusterChecks are the consistency checks of one cluster.
func clusterChecks(cl *topology.Cluster, entities int) []Warning {
	var w []Warning
	if cl.Spec.Architecture != "osa" && !cl.Dedup {
		w = append(w, Warning{"warn", "Dedup is off", "The *-esa-dedup-* kinds answer empty for this cluster; the collector still queries them."})
	}
	if cl.Spec.StatsPrimary != nil && !*cl.Spec.StatsPrimary {
		w = append(w, Warning{"warn", "Stats primary is offline", "Performance queries return a missing-primary fault until it is online."})
	}
	if entities > 6000 {
		w = append(w, Warning{"warn", "Large cluster", fmt.Sprintf("%s performance entities from this cluster alone.", num(entities))})
	}
	return w
}

// HostRow is one host of a cluster.
type HostRow struct {
	Name     string `json:"name"`
	MoRef    string `json:"moref"`
	NodeUUID string `json:"nodeUuid"`
	NVMe     int    `json:"nvme"`
	LCPU     int    `json:"lcpu"`
	VMs      int    `json:"vms"`
	Identity string `json:"identity"` // unchanged | new | removed
}

// DatastoreRow is one datastore of a cluster.
const sharedStoragePerf = "queried: capacity and used space, I/O counters read zero"

type DatastoreRow struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Backing string `json:"backing"`
	Mounted int    `json:"mounted"`
	Perf    string `json:"perfService"`
	State   string `json:"state,omitempty"`
}

// FieldSource says where a cluster's value comes from.
type FieldSource struct {
	Field  string `json:"field"`
	Source string `json:"source"` // default | varied | override
}

// ClusterView is the editor's data for one cluster.
type ClusterView struct {
	Key        string                    `json:"key"`
	Name       string                    `json:"name"`
	Datacenter string                    `json:"datacenter"`
	State      string                    `json:"state"`
	Applied    *scenario.ClusterSpec     `json:"applied"` // nil for a new cluster
	Spec       scenario.ClusterSpec      `json:"spec"`    // effective, in the draft
	Override   *scenario.ClusterOverride `json:"override"`
	Sources    []FieldSource             `json:"sources"`
	MoRef      string                    `json:"moref"`
	VsanUUID   string                    `json:"vsanUuid"`
	Serial     int                       `json:"serial"`
	Hosts      []HostRow                 `json:"hosts"`
	HostsOld   int                       `json:"hostsApplied"`
	VMs        int                       `json:"vms"`
	Datastores []DatastoreRow            `json:"datastores"`
	Load       Load                      `json:"load"`    // this cluster, selected scope
	LoadOld    Load                      `json:"loadOld"` // applied
	Entities   int                       `json:"entities"`
	Checks     []Warning                 `json:"checks"`
	Regenerate bool                      `json:"regenerate"`
	SeedEpoch  int                       `json:"seedEpoch"`
	VDS        string                    `json:"vds"`
	PortGroups int                       `json:"portgroups"`
}

// Cluster returns the editor view of one cluster.
func (c *Controller) Cluster(key string) (*ClusterView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.applied()
	d := c.viewDraft()
	topo, sc := r.topo, r.sc
	if d != nil {
		topo, _ = c.previewTopology(d)
		sc = d.Scenario
	}
	var cl, old *topology.Cluster
	for _, x := range topo.Clusters() {
		if x.Key == key {
			cl = x
		}
	}
	for _, x := range r.topo.Clusters() {
		if x.Key == key {
			old = x
		}
	}
	if cl == nil && old == nil {
		return nil, invalid("unknown cluster %q", key)
	}
	v := &ClusterView{Key: key, Hosts: []HostRow{}, Datastores: []DatastoreRow{}, Checks: []Warning{}, Sources: []FieldSource{}}
	if cl == nil {
		// deleted in the draft: show it as it is applied
		cl, v.State = old, "removed"
	}
	v.Name, v.Datacenter, v.Spec, v.Serial, v.VsanUUID = cl.Name, cl.Datacenter.Name, cl.Spec, cl.Serial, ""
	if old != nil {
		spec := old.Spec
		v.Applied, v.HostsOld = &spec, len(old.Hosts)
		v.VsanUUID = old.VsanUUID
		for _, ref := range r.sim.Clusters {
			if ref.Cluster == old {
				v.MoRef = ref.Ref.Value
			}
		}
	}
	if v.State == "" {
		switch {
		case old == nil:
			v.State = "new"
		case fmt.Sprint(specFields(old.Spec)) != fmt.Sprint(specFields(cl.Spec)) || old.Serial != cl.Serial || old.TelemetrySeed != cl.TelemetrySeed || clusterStorageChanged(old, cl):
			v.State = "pending"
		}
	}
	if o, ok := sc.Override(key); ok {
		ov := scenario.ClusterOverride{Key: key, SeedEpoch: sc.SeedEpoch(key), ClusterSpec: o}
		v.Override = &ov
	}
	v.SeedEpoch = sc.SeedEpoch(key)
	if d != nil {
		v.Regenerate = contains(d.Regenerate, key)
	}
	v.Sources = sources(sc, key)
	oldSerial := map[string]int{}
	if old != nil {
		for _, h := range old.Hosts {
			oldSerial[h.Key] = h.Serial
		}
	}
	var hostRefs []string
	if old != nil {
		for _, ref := range r.sim.Clusters {
			if ref.Cluster == old {
				for _, h := range ref.Hosts {
					hostRefs = append(hostRefs, h.Value)
				}
			}
		}
	}
	vmsOn := map[int]int{}
	for _, vm := range cl.VMs {
		vmsOn[vm.Host]++
	}
	for _, h := range cl.Hosts {
		nv := 0
		for _, dv := range h.Devices {
			if dv.Claimed {
				nv++
			}
		}
		row := HostRow{Name: h.Name, NodeUUID: h.NodeUUID, NVMe: nv, LCPU: h.Threads, VMs: vmsOn[h.Index], Identity: "new"}
		if s, ok := oldSerial[h.Key]; ok && s == h.Serial {
			row.Identity = "unchanged"
			if h.Index < len(hostRefs) && old != nil {
				row.MoRef = hostRefs[h.Index]
			}
		}
		if v.State == "removed" {
			row.Identity = "removed"
		}
		v.Hosts = append(v.Hosts, row)
	}
	if old != nil && v.State != "removed" {
		for _, h := range old.Hosts {
			if _, ok := findHost(cl, h.Key, h.Serial); !ok {
				nv, vms := 0, 0
				for _, dv := range h.Devices {
					if dv.Claimed {
						nv++
					}
				}
				for _, vm := range old.VMs {
					if vm.Host == h.Index {
						vms++
					}
				}
				row := HostRow{Name: h.Name, NodeUUID: h.NodeUUID, Identity: "removed", LCPU: h.Threads, NVMe: nv, VMs: vms}
				if h.Index < len(hostRefs) {
					row.MoRef = hostRefs[h.Index]
				}
				v.Hosts = append(v.Hosts, row)
			}
		}
	}
	v.VMs = len(cl.VMs)
	proj := Project(c.cat, topo)
	if v.State == "removed" {
		proj = Project(c.cat, r.topo) // a deleted cluster is shown as it is applied
	}
	v.Entities = proj.PerCluster[key].Entities
	v.Load = clusterLoad(c.cat, cl, c.scope)
	if old != nil {
		v.LoadOld = clusterLoad(c.cat, old, c.scope)
	}
	local := 0
	for _, h := range cl.Hosts {
		for _, dv := range h.Devices {
			if !dv.Claimed {
				local++
			}
		}
	}
	claimed := cl.Spec.Host.ClaimedDevices
	vsanType := "vSAN ESA"
	if cl.Spec.Architecture == "osa" {
		vsanType = "vSAN OSA"
	}
	v.Datastores = append(v.Datastores, DatastoreRow{Name: cl.Datastore, Type: vsanType, Backing: fmt.Sprintf("%d NVMe / host", claimed),
		Mounted: len(cl.Hosts), Perf: "queried"})
	if local > 0 {
		v.Datastores = append(v.Datastores, DatastoreRow{Name: "local VMFS", Type: "VMFS · local", Backing: fmt.Sprintf("%d / host", scenario.Int(cl.Spec.Host.LocalDevices)),
			Mounted: local, Perf: "inventory only"})
	}
	for _, nfs := range cl.Datacenter.NFS {
		if len(nfs.Spec.Clusters) > 0 && !slices.Contains(nfs.Spec.Clusters, cl.Key) {
			continue
		}
		bad := 0
		for _, h := range cl.Hosts {
			if slices.Contains(nfs.Spec.InaccessibleHosts, h.Key) {
				bad++
			}
		}
		state := "Accessible"
		if bad > 0 {
			if bad == 1 {
				state = "1 host mount inaccessible"
			} else {
				state = fmt.Sprintf("%d host mounts inaccessible", bad)
			}
		}
		v.Datastores = append(v.Datastores, DatastoreRow{Name: nfs.Spec.Name, Type: "NFS", Backing: nfs.Spec.RemoteHost + ":" + nfs.Spec.RemotePath, Mounted: len(cl.Hosts), Perf: sharedStoragePerf, State: state})
	}
	for _, st := range cl.Datacenter.ISCSI {
		if st.Spec.Cluster != cl.Key {
			continue
		}
		for _, lun := range st.LUNs {
			bad, standby := 0, 0
			for _, h := range cl.Hosts {
				if !lun.Spec.Reachable(st.Spec.Adapters, h.Key) {
					bad++
				}
				for _, a := range st.Spec.Adapters {
					if state, _ := lun.Spec.PathState(a.Name, h.Key); state == "standby" {
						standby++
						break
					}
				}
			}
			state := "Accessible"
			if bad > 0 {
				if bad == 1 {
					state = "1 host has no working path"
				} else {
					state = fmt.Sprintf("%d hosts have no working path", bad)
				}
			} else if standby > 0 {
				if standby == 1 {
					state = "1 host has a standby path"
				} else {
					state = fmt.Sprintf("%d hosts have a standby path", standby)
				}
			}
			v.Datastores = append(v.Datastores, DatastoreRow{Name: lun.Spec.Name, Type: "VMFS · iSCSI", Backing: lun.Spec.Target, Mounted: len(cl.Hosts), Perf: sharedStoragePerf, State: state})
		}
	}
	v.VDS, v.PortGroups = cl.VDS(), len(topology.Portgroups)
	v.Checks = append(v.Checks,
		Warning{"ok", "uplinks", fmt.Sprintf("%s uplinks cover %d hosts × %d pNICs", cl.VDS(), len(cl.Hosts), cl.Spec.Host.VsanPnics)},
		Warning{"ok", "placement", fmt.Sprintf("%d VMs placed on %d hosts", len(cl.VMs), len(cl.Hosts))})
	v.Checks = append(v.Checks, clusterChecks(cl, v.Entities)...)
	return v, nil
}

func findHost(cl *topology.Cluster, key string, serial int) (*topology.Host, bool) {
	for _, h := range cl.Hosts {
		if h.Key == key && h.Serial == serial {
			return h, true
		}
	}
	return nil, false
}

func clusterLoad(cat *catalog.Catalog, cl *topology.Cluster, scope string) Load {
	t := &topology.Topology{Datacenters: []*topology.Datacenter{{Clusters: []*topology.Cluster{cl}}}}
	return Project(cat, t).Selected(scope)
}

// sources says, for each editable field, whether the value is the default,
// drawn from the scenario's vary section, or an explicit override.
func sources(sc *scenario.Scenario, key string) []FieldSource {
	o, has := sc.Override(key)
	src := func(field string, overridden, varied bool) FieldSource {
		switch {
		case has && overridden:
			return FieldSource{field, "override"}
		case varied:
			return FieldSource{field, "varied"}
		}
		return FieldSource{field, "default"}
	}
	v := sc.Vary
	return []FieldSource{
		src("architecture", o.Architecture != "", false),
		src("statsPrimary", o.StatsPrimary != nil, false),
		src("hosts", o.Hosts != 0, len(v.Hosts) > 0),
		src("vmsPerHost", o.VMs.PerHost != nil, v.VMsPerHost != nil),
		src("disksPerVM", o.VMs.DisksPerVM != 0, false),
		src("claimedDevices", o.Host.ClaimedDevices != 0, len(v.ClaimedDevices) > 0),
		src("localDevices", o.Host.LocalDevices != nil, false),
		src("threads", o.Host.Threads != 0, len(v.Threads) > 0),
		src("domWorlds", o.Host.DomWorlds != nil, v.DomWorlds != nil),
		src("vsanPnics", o.Host.VsanPnics != 0, false),
		src("dedup", o.Dedup != nil, false),
		src("compression", o.Compression != nil, false),
	}
}

// ---------------------------------------------------------------------------
// catalog and compatibility

// FamilyInfo describes one entity kind.
type FamilyInfo struct {
	Family    string `json:"family"`
	Behaviour string `json:"behaviour"`
	Dashboard bool   `json:"dashboard"`
	Labels    int    `json:"labels"`
}

// Families lists the entity kinds the service advertises.
func (c *Controller) Families() []FamilyInfo {
	var out []FamilyInfo
	for n, f := range c.cat.Families {
		out = append(out, FamilyInfo{n, string(f.Behaviour), dashboardKinds[n], len(f.Labels)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Family < out[j].Family })
	return out
}

// Method is one row of the compatibility matrix.
type Method struct {
	Surface string `json:"surface"`
	Object  string `json:"object"`
	Method  string `json:"method"`
	Basis   string `json:"basis"`  // how the answer is shaped
	Status  string `json:"status"` // verified | partial | unverified
	Note    string `json:"note"`
	Calls   int    `json:"callsSeen"`
	Faults  int    `json:"faultsSeen"`
}

// Compat is the compatibility screen's data.
type Compat struct {
	Methods  []Method       `json:"methods"`
	Unlisted []Method       `json:"unlisted"` // seen in traffic, not in the matrix
	Kinds    map[string]int `json:"kinds"`    // catalog behaviours
	OSAKinds map[string]int `json:"osaKinds"` // primary-online wildcard outcomes
	Profile  string         `json:"profile"`
	Verified string         `json:"verifiedBy"`
	Cycle    activity.Cycle `json:"lastCycle"`
	Pending  []string       `json:"unverified"`
}

var matrix = []Method{
	{"vsanHealth", "VsanPerformanceManager", "VsanPerfQueryPerf", "live capture", "verified", "138 captured queries and 20 edge cases replayed; values are synthetic inside each label's live envelope.", 0, 0},
	{"vsanHealth", "VsanPerformanceManager", "VsanPerfQueryNodeInformation", "live capture", "verified", "Captured request and response replayed.", 0, 0},
	{"vsanHealth", "VsanPerformanceManager", "VsanPerfGetSupportedEntityTypes", "live capture", "verified", "The advertised catalog is the captured one, verbatim.", 0, 0},
	{"vsanHealth", "VsanVcClusterConfigSystem", "VsanClusterGetConfig", "live capture", "verified", "Captured request and response replayed.", 0, 0},
	{"vsanHealth", "VimClusterVsanVcDiskManagementSystem", "QueryVsanManagedDisks", "live capture", "verified", "Ten captured per-host responses replayed.", 0, 0},
	{"vsanHealth", "VsanVcClusterHealthSystem", "VsanQueryVcClusterHealthSummary", "live capture", "partial", "ESA HCL information and OSA status, disk, object, network, limits and health-check groups are modeled; some detailed results remain incomplete.", 0, 0},
	{"api", "vCenter inventory", "GET inventory", "live capture", "partial", "Inventory lists and cluster and datastore detail share SOAP identities; some VM details remain incomplete.", 0, 0},
	{"sdk", "SessionManager", "Login", "vcsim", "partial", "Accepts any user, or only the scenario's users. Token and certificate logins are refused when users are configured.", 0, 0},
	{"sdk", "SessionManager", "Logout", "vcsim", "partial", "Faults after logout match the captured shape.", 0, 0},
	{"sdk", "ServiceInstance", "RetrieveServiceContent", "vcsim + capture", "verified", "About block is the captured 9.1.1 identity.", 0, 0},
	{"sdk", "ViewManager", "CreateContainerView", "vcsim", "partial", "Stock vcsim behaviour.", 0, 0},
	{"sdk", "PropertyCollector", "RetrievePropertiesEx", "vcsim + capture", "partial", "Cluster, host, datastore, VM and vDS properties are shaped after the trimmed management-vCenter capture.", 0, 0},
	{"sdk", "PropertyCollector", "ContinueRetrievePropertiesEx", "vcsim", "partial", "Stock vcsim behaviour.", 0, 0},
	{"sdk", "(document)", "GET sdk/vimServiceVersions.xml", "live capture", "verified", "Served verbatim from the capture.", 0, 0},
	{"sdk", "(document)", "GET sdk/vsanServiceVersions.xml", "live capture", "verified", "Served verbatim from the capture.", 0, 0},
}

// Compatibility returns the matrix with the counts the log has seen.
func (c *Controller) Compatibility() *Compat {
	sum := c.Log.Summarize(c.now())
	seen := map[string]activity.MethodStat{}
	for _, m := range sum.Methods {
		seen[m.Surface+"/"+m.Method] = m
	}
	cv := &Compat{Kinds: map[string]int{}, OSAKinds: map[string]int{}, Profile: c.cat.About.FullName, Verified: "fixtures/live/9.1.1, replayed by go test", Cycle: sum.Cycle,
		Methods: []Method{}, Unlisted: []Method{}, Pending: []string{
			"Behaviour on a real vCenter 9.1.1 other than the captured one",
			"The adapter's use of every /sdk property (verified against the trimmed capture only)",
			"vSAN OSA beyond a single-disk-group, three-host cluster: per-VM and virtual-disk performance, multiple disk groups per host, stretched clusters and OSA deduplication",
			"iSCSI inventory and path faults are plausible but not verified against a lab iSCSI host; Fibre Channel and file service are not emulated",
			"/sdk/vim.wsdl is not served: a complete sanitized capture is unavailable",
			"The VCF Operations ingestion path (Phase 3)",
		}}
	listed := map[string]bool{}
	for _, m := range matrix {
		name := m.Method
		if strings.HasPrefix(name, "GET ") {
			name = "GET " + strings.TrimPrefix(name, "GET ")
		}
		listed[m.Surface+"/"+name] = true
		if s, ok := seen[m.Surface+"/"+name]; ok {
			m.Calls, m.Faults = s.Calls, s.Injected+s.Unexpected
		}
		cv.Methods = append(cv.Methods, m)
	}
	for k, s := range seen {
		if !listed[k] {
			cv.Unlisted = append(cv.Unlisted, Method{Surface: s.Surface, Method: s.Method, Status: "unverified", Basis: "vcsim", Calls: s.Calls, Faults: s.Injected + s.Unexpected,
				Note: "Seen in traffic; not in the verified matrix."})
		}
	}
	sort.Slice(cv.Unlisted, func(i, j int) bool { return cv.Unlisted[i].Calls > cv.Unlisted[j].Calls })
	for _, f := range c.cat.Families {
		cv.Kinds[string(f.Behaviour)]++
	}
	osa := &topology.Cluster{Spec: scenario.ClusterSpec{Architecture: "osa"}}
	for _, name := range vsanhealth.PerfFamilyNames(c.cat, osa) {
		if f, ok := vsanhealth.EffectiveFamily(c.cat, osa, name); ok {
			cv.OSAKinds[string(f.Behaviour)]++
		}
	}
	return cv
}

// Banner is the start-up summary, one line per item.
func (c *Controller) Banner(took time.Duration) []string {
	r := c.applied()
	cl, h, v, d := r.topo.Counts()
	out := []string{
		fmt.Sprintf("vCenter %s instanceUuid=%s seed=%q registry=%d ids built in %s", c.cat.About.Version, r.topo.InstanceUUID, r.sc.Seed, c.reg.Len(), took.Round(time.Millisecond)),
		fmt.Sprintf("%d clusters, %d hosts, %d vms, %d devices", cl, h, v, d),
	}
	for _, ref := range r.sim.Clusters {
		k := ref.Cluster
		out = append(out, fmt.Sprintf("%s (%s) vsan uuid=%s hosts=%d vms=%d dedup=%v", k.Key, ref.Ref.Value, k.VsanUUID, len(k.Hosts), len(k.VMs), k.Dedup))
	}
	return out
}

// LoginUsers lists the accepted user names (empty: anyone).
func (c *Controller) LoginUsers() []string {
	if c.auth.Snapshot().AnyLogin {
		return nil
	}
	var out []string
	for _, user := range c.auth.Users() {
		out = append(out, user.Name)
	}
	return out
}
