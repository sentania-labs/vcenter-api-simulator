// Package scenario is the operator's contract with the simulator: one YAML
// file describing the simulated vCenter (datacenters, clusters and their
// per-cluster cardinality), the identity and telemetry seeds, who may log
// in, which certificate to serve, and the failure and latency knobs.
//
// Everything structural is derived from the file plus the identity registry
// (package registry); nothing is edited through an API. Counts a cluster does
// not set explicitly are drawn from Defaults, and Vary lets each cluster draw
// its own counts from the file's distributions using the cluster's own seed,
// so ten generated clusters are ten different clusters, not ten copies.
package scenario

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/registry"
)

// Scenario is the parsed, validated file.
type Scenario struct {
	// Seed names the identity namespace. Every MoRef-independent identity
	// (vCenter instance uuid, cluster uuids, node uuids, disk uuids, VM
	// uuids) is derived from it and the registry's serials, so a scenario
	// with the same seed and registry reproduces the same identities.
	Seed string `yaml:"seed" json:"seed,omitempty"`
	// TelemetrySeed feeds metric values only; empty means Seed. Changing it
	// changes every value and no identity.
	TelemetrySeed string `yaml:"telemetrySeed" json:"telemetrySeed,omitempty"`

	Datacenters []Datacenter      `yaml:"datacenters" json:"datacenters,omitempty"`
	Defaults    ClusterSpec       `yaml:"defaults" json:"defaults,omitempty"`
	Vary        Vary              `yaml:"vary" json:"vary,omitempty"`
	Clusters    []ClusterOverride `yaml:"clusters" json:"clusters,omitempty"`
	// Removed lists cluster keys the datacenters would generate but the
	// scenario leaves out (the console's Delete cluster). The cluster
	// numbers of the others do not shift, and a removed cluster's
	// identities stay reserved in the registry for a later re-add.
	Removed []string `yaml:"removed" json:"removed,omitempty"`

	VCenter         Version            `yaml:"vcenterVersion,omitempty" json:"vcenterVersion,omitempty"`
	ESX             Version            `yaml:"esxVersion,omitempty" json:"esxVersion,omitempty"`
	HostVersions    map[string]Version `yaml:"hostVersions,omitempty" json:"hostVersions,omitempty"`
	ForceNewerHosts bool               `yaml:"forceNewerHosts,omitempty" json:"forceNewerHosts,omitempty"`
	Auth            Auth               `yaml:"auth" json:"auth,omitempty"`
	TLS             TLS                `yaml:"tls" json:"tls,omitempty"`
	Faults          []Fault            `yaml:"faults" json:"faults,omitempty"`
	Latency         Latency            `yaml:"latency" json:"latency,omitempty"`
	// Conditions are the slowness and faults the console switched on. The
	// console also turns Faults, Latency and statsPrimary: false into
	// conditions when it starts, so there is one list of what is on.
	Conditions []Condition `yaml:"conditions,omitempty" json:"conditions,omitempty"`
}

// Datacenter generates Clusters clusters named cluster-01, cluster-02, ...
// under one datacenter with one vDS.
type Datacenter struct {
	Name     string         `yaml:"name" json:"name,omitempty"`
	Clusters int            `yaml:"clusters" json:"clusters,omitempty"`
	NFS      []NFSDatastore `yaml:"nfs" json:"nfs,omitempty"`
	ISCSI    []ISCSIStorage `yaml:"iscsi,omitempty" json:"iscsi,omitempty"`
}

// ISCSIStorage describes one cluster's adapters and shared block storage.
type ISCSIStorage struct {
	Cluster  string         `yaml:"cluster" json:"cluster"`
	Adapters []ISCSIAdapter `yaml:"adapters" json:"adapters"`
	Targets  []ISCSITarget  `yaml:"targets" json:"targets"`
	LUNs     []ISCSILUN     `yaml:"luns" json:"luns"`
}

type ISCSIAdapter struct {
	Name string `yaml:"name" json:"name"`
	Kind string `yaml:"kind" json:"kind"` // software or hardware
}

type ISCSITarget struct {
	Name    string `yaml:"name" json:"name"`
	Address string `yaml:"address" json:"address"`
	IQN     string `yaml:"iqn" json:"iqn"`
	Port    int32  `yaml:"port,omitempty" json:"port,omitempty"`
}

type ISCSILUN struct {
	Name      string      `yaml:"name" json:"name"`
	Target    string      `yaml:"target" json:"target"`
	Number    int32       `yaml:"number" json:"number"`
	Capacity  int64       `yaml:"capacity" json:"capacity"`
	FreeSpace int64       `yaml:"freeSpace" json:"freeSpace"`
	Policy    string      `yaml:"policy,omitempty" json:"policy,omitempty"`
	Paths     []ISCSIPath `yaml:"paths" json:"paths"`
}

type ISCSIPath struct {
	Adapter string `yaml:"adapter" json:"adapter"`
	Host    string `yaml:"host,omitempty" json:"host,omitempty"`
	State   string `yaml:"state" json:"state"`
}

// PathState is the state of the LUN's path through an adapter as seen from a
// host: the host-specific path where one is listed, otherwise the path that
// names no host. It reports false when the host has no such path.
func (l ISCSILUN) PathState(adapter, host string) (string, bool) {
	state, found := "", false
	for _, p := range l.Paths {
		if p.Adapter == adapter && p.Host == "" {
			state, found = p.State, true
		}
	}
	for _, p := range l.Paths {
		if p.Adapter == adapter && p.Host == host {
			state, found = p.State, true
		}
	}
	return state, found
}

// Reachable reports whether the host has a working (active or standby) path
// to the LUN through any of the adapters.
func (l ISCSILUN) Reachable(adapters []ISCSIAdapter, host string) bool {
	for _, a := range adapters {
		if state, _ := l.PathState(a.Name, host); state == "active" || state == "standby" {
			return true
		}
	}
	return false
}

// NFSDatastore describes a shared NFS v3 mount in a datacenter.
type NFSDatastore struct {
	Name       string `yaml:"name" json:"name"`
	RemoteHost string `yaml:"remoteHost" json:"remoteHost"`
	RemotePath string `yaml:"remotePath" json:"remotePath"`
	Capacity   int64  `yaml:"capacity" json:"capacity"`
	FreeSpace  int64  `yaml:"freeSpace" json:"freeSpace"`
	// Clusters limits the mount to cluster keys in this datacenter. Empty
	// retains the original behavior of mounting on every cluster.
	Clusters []string `yaml:"clusters,omitempty" json:"clusters,omitempty"`
	// InaccessibleHosts identifies mounts by cluster key and host ordinal,
	// for example dc-01/cluster-01/esx02.
	InaccessibleHosts []string `yaml:"inaccessibleHosts,omitempty" json:"inaccessibleHosts,omitempty"`
}

// ClusterSpec is the per-cluster cardinality. An unset value means "use
// the built-in default" in Defaults and "inherit" in an override; the
// counts that may legitimately be zero are pointers so that zero can be
// set.
type ClusterSpec struct {
	Architecture string   `yaml:"architecture" json:"architecture,omitempty"`
	StatsPrimary *bool    `yaml:"statsPrimary" json:"statsPrimary,omitempty"`
	Hosts        int      `yaml:"hosts" json:"hosts,omitempty"`
	Dedup        *bool    `yaml:"dedup" json:"dedup,omitempty"`
	Compression  *bool    `yaml:"compression" json:"compression,omitempty"`
	Host         HostSpec `yaml:"host" json:"host,omitempty"`
	VMs          VMSpec   `yaml:"vms" json:"vms,omitempty"`
}

// HostSpec is the per-host cardinality.
type HostSpec struct {
	Threads        int  `yaml:"threads" json:"threads,omitempty"`               // host-cpu entities: cpu-0..N-1 plus the aggregate
	DomWorlds      *int `yaml:"domWorlds" json:"domWorlds,omitempty"`           // dom-world-cpu entities
	VsanPnics      int  `yaml:"vsanPnics" json:"vsanPnics,omitempty"`           // vmnic uplinks on the vDS, the vsan-pnic-net entities
	ClaimedDevices int  `yaml:"claimedDevices" json:"claimedDevices,omitempty"` // NVMe devices claimed by vSAN ESA
	LocalDevices   *int `yaml:"localDevices" json:"localDevices,omitempty"`     // NVMe devices left to local VMFS
}

// VMSpec is the VM population of a cluster.
type VMSpec struct {
	PerHost    *int `yaml:"perHost" json:"perHost,omitempty"`
	DisksPerVM int  `yaml:"disksPerVM" json:"disksPerVM,omitempty"`
}

// Int dereferences an optional count; nil is 0.
func Int(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// IntPtr makes an optional count.
func IntPtr(n int) *int { return &n }

// MinHosts is the smallest vSAN cluster the simulator builds: vSAN (and
// vcsim's vSAN datastore) needs three hosts before a VM can be placed.
const MinHosts = 3

// Vary gives each generated cluster an independent draw. A list is a choice
// (duplicates weight it); a Range draws uniformly. Explicit cluster
// overrides win over draws.
type Vary struct {
	Hosts          []int  `yaml:"hosts" json:"hosts,omitempty"`
	VMsPerHost     *Range `yaml:"vmsPerHost" json:"vmsPerHost,omitempty"`
	ClaimedDevices []int  `yaml:"claimedDevices" json:"claimedDevices,omitempty"`
	DomWorlds      *Range `yaml:"domWorlds" json:"domWorlds,omitempty"`
	Threads        []int  `yaml:"threads" json:"threads,omitempty"`
}

// DrawSource is the random source of a cluster's draws from Vary, seeded
// from the cluster's own sub-seed.
func DrawSource(sub uint64) *rand.Rand {
	return rand.New(rand.NewPCG(sub, sub^0x9E3779B97F4A7C15))
}

// DrawHosts is a cluster's host count, always the first draw from its
// source, or 0 when Vary leaves the host count alone.
func (v Vary) DrawHosts(rng *rand.Rand) int {
	u := rng.Uint64()
	if len(v.Hosts) == 0 {
		return 0
	}
	return v.Hosts[u%uint64(len(v.Hosts))]
}

// Range is an inclusive integer range.
type Range struct {
	Min int `yaml:"min" json:"min,omitempty"`
	Max int `yaml:"max" json:"max,omitempty"`
}

// ClusterOverride pins values for one cluster, keyed "<datacenter>/<cluster>".
type ClusterOverride struct {
	Key string `yaml:"key" json:"key,omitempty"`
	// SeedEpoch, when above zero, gives the cluster its own telemetry seed
	// (the console's Reset cluster seed): every synthetic sample and the
	// whole history of this cluster change, no identity does.
	SeedEpoch int     `yaml:"seedEpoch,omitempty" json:"seedEpoch,omitempty"`
	ESX       Version `yaml:"esxVersion,omitempty" json:"esxVersion,omitempty"`
	// RemovedHosts lists host numbers (1 is esx01) the cluster leaves out
	// (the console's Remove hosts). The numbers of the other hosts do not
	// shift, and a removed number is not given to a host added later.
	RemovedHosts []int `yaml:"removedHosts,omitempty" json:"removedHosts,omitempty"`
	ClusterSpec  `yaml:",inline" json:",omitempty"`
}

// Auth lists accepted users. AnyLogin explicitly permits arbitrary credentials.
type Auth struct {
	AnyLogin bool   `yaml:"anyLogin" json:"anyLogin,omitempty"`
	Users    []User `yaml:"users" json:"users,omitempty"`
}

// User is one account. Password may come from the environment instead of
// the file.
type User struct {
	Name        string `yaml:"name" json:"name,omitempty"`
	Password    string `yaml:"password" json:"password,omitempty"`
	PasswordEnv string `yaml:"passwordEnv" json:"passwordEnv,omitempty"`
	// Role is "administrator" (the default) or "read-only".
	Role string `yaml:"role,omitempty" json:"role,omitempty"`
}

// TLS selects the certificate: a provided pair, or a self-signed one
// created once in Dir and kept across restarts.
type TLS struct {
	Dir      string   `yaml:"dir" json:"dir,omitempty"`
	CertFile string   `yaml:"certFile" json:"certFile,omitempty"`
	KeyFile  string   `yaml:"keyFile" json:"keyFile,omitempty"`
	Names    []string `yaml:"names" json:"names,omitempty"`
}

// Fault injects a SOAP fault on /vsanHealth. Family selects VsanPerfQueryPerf
// wildcard queries of one entity family; Method selects any other served
// method by name (QueryVsanManagedDisks, VsanQueryVcClusterHealthSummary,
// VsanPerfQueryNodeInformation, VsanClusterGetConfig). Clusters narrows it
// to cluster keys; empty means every cluster. From/To bound it in time and
// snap to five-minute bucket boundaries, so a change takes effect at the
// next bucket as a scenario edit would.
type Fault struct {
	Family   string     `yaml:"family" json:"family,omitempty"`
	Method   string     `yaml:"method" json:"method,omitempty"`
	Clusters []string   `yaml:"clusters" json:"clusters,omitempty"`
	Fault    string     `yaml:"fault" json:"fault,omitempty"`
	From     *time.Time `yaml:"from" json:"from,omitempty"`
	To       *time.Time `yaml:"to" json:"to,omitempty"`
	// Cause is set on a fault the simulator made up from a Condition: the
	// condition's id. It is never read from or written to a file.
	Cause int `yaml:"-" json:"-"`
}

// Fault kinds.
const (
	FaultInvalidArgument = "invalid-argument" // the pack treats it as "feature absent"
	FaultNotSupported    = "not-supported"    // likewise
	FaultSystemError     = "system-error"     // the pack reports a collection problem
	FaultTimeout         = "timeout"          // no response within the pack's 120 s request timeout
)

// Latency delays responses. PerfQuery applies to every VsanPerfQueryPerf;
// Families overrides it per entity family; SDK applies to every /sdk call.
type Latency struct {
	PerfQuery *Delay           `yaml:"perfQuery" json:"perfQuery,omitempty"`
	Families  map[string]Delay `yaml:"families" json:"families,omitempty"`
	SDK       *Delay           `yaml:"sdk" json:"sdk,omitempty"`
}

// Delay is a fixed delay plus uniform jitter, in milliseconds.
type Delay struct {
	FixedMs  int `yaml:"fixedMs" json:"fixedMs,omitempty"`
	JitterMs int `yaml:"jitterMs" json:"jitterMs,omitempty"`
}

// Duration draws one delay; r in [0,1) selects the jitter.
func (d Delay) Duration(r float64) time.Duration {
	ms := float64(d.FixedMs) + r*float64(d.JitterMs)
	return time.Duration(ms * float64(time.Millisecond))
}

// Built-in defaults, the lab's measured per-host shape (recon section 5.2:
// 32 threads, 66 DOM worlds, 2 vSAN uplinks, one claimed NVMe device).
var builtin = ClusterSpec{
	Hosts:       4,
	Dedup:       ptr(true),
	Compression: ptr(true),
	Host:        HostSpec{Threads: 32, DomWorlds: IntPtr(66), VsanPnics: 2, ClaimedDevices: 1, LocalDevices: IntPtr(1)},
	VMs:         VMSpec{PerHost: IntPtr(2), DisksPerVM: 1},
}

func ptr(b bool) *bool { return &b }

// Load reads and validates a scenario file.
func Load(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse validates scenario YAML.
func Parse(b []byte) (*Scenario, error) {
	var s Scenario
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil && !errors.Is(err, os.ErrNotExist) {
		if err.Error() == "EOF" {
			return nil, errors.New("scenario: empty file")
		}
		return nil, fmt.Errorf("scenario: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Phase1 is the built-in scenario: one datacenter, one four-host ESA cluster
// with the Phase 1 shape (16 threads, 12 DOM worlds, one VM per host).
func Phase1(seed string) *Scenario {
	s := &Scenario{
		Seed:        seed,
		Datacenters: []Datacenter{{Name: "dc-01", Clusters: 1}},
		Defaults: ClusterSpec{
			Hosts: 4,
			Host:  HostSpec{Threads: 16, DomWorlds: IntPtr(12), VsanPnics: 2, ClaimedDevices: 1, LocalDevices: IntPtr(1)},
			VMs:   VMSpec{PerHost: IntPtr(1), DisksPerVM: 1},
		},
	}
	if err := s.Validate(); err != nil {
		panic(err)
	}
	return s
}

var (
	nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	keyRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?/[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	hostRe = regexp.MustCompile(`^esx[0-9]{2,}$`)
)

func validStorageHost(name string) bool {
	_, ok := StorageHostOrdinal(name)
	return ok
}

// StorageHostOrdinal parses a generated host's short name.
func StorageHostOrdinal(name string) (int, bool) {
	if !hostRe.MatchString(name) {
		return 0, false
	}
	n, err := strconv.Atoi(name[3:])
	return n, err == nil && n > 0 && fmt.Sprintf("esx%02d", n) == name
}

// StorageNumber gives an adapter or target a stable synthetic numeric identity.
func StorageNumber(name string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return h.Sum32()
}

// Validate checks the file and fills Defaults from the built-in defaults.
func (s *Scenario) Validate() error {
	if err := s.validateVersions(); err != nil {
		return err
	}
	if s.Seed == "" {
		return errors.New("scenario: seed is required")
	}
	if len(s.Datacenters) == 0 {
		return errors.New("scenario: at least one datacenter is required")
	}
	seen := map[string]bool{}
	for i, dc := range s.Datacenters {
		if !nameRe.MatchString(dc.Name) {
			return fmt.Errorf("scenario: datacenters[%d]: name %q must be lower-case letters, digits and hyphens", i, dc.Name)
		}
		if seen[dc.Name] {
			return fmt.Errorf("scenario: datacenter %q listed twice", dc.Name)
		}
		seen[dc.Name] = true
		nfsNames := map[string]bool{}
		for _, n := range dc.NFS {
			if !nameRe.MatchString(n.Name) || nfsNames[n.Name] || n.RemoteHost == "" || !strings.HasPrefix(n.RemotePath, "/") || n.Capacity <= 0 || n.FreeSpace < 0 || n.FreeSpace > n.Capacity {
				return fmt.Errorf("scenario: datacenter %q: invalid NFS datastore %q", dc.Name, n.Name)
			}
			nfsNames[n.Name] = true
		}
		if dc.Clusters < 0 {
			return fmt.Errorf("scenario: datacenter %q: clusters must not be negative", dc.Name)
		}
	}
	s.Defaults = merge(builtin, s.Defaults)
	if err := s.Defaults.check("defaults"); err != nil {
		return err
	}
	all := map[string]bool{}
	for _, dc := range s.Datacenters {
		for n := 1; n <= dc.Clusters; n++ {
			all[ClusterKey(dc.Name, n)] = true
		}
	}
	removed := map[string]bool{}
	for _, k := range s.Removed {
		if !all[k] {
			return fmt.Errorf("scenario: removed: %q is not a cluster the datacenters generate", k)
		}
		if removed[k] {
			return fmt.Errorf("scenario: removed: %q listed twice", k)
		}
		removed[k] = true
	}
	generated := map[string]bool{}
	for _, k := range s.ClusterKeys() {
		generated[k] = true
	}
	for _, dc := range s.Datacenters {
		for _, n := range dc.NFS {
			selected := map[string]bool{}
			for _, key := range n.Clusters {
				if !generated[key] || !strings.HasPrefix(key, dc.Name+"/") || selected[key] {
					return fmt.Errorf("scenario: datacenter %q: NFS datastore %q has invalid cluster %q", dc.Name, n.Name, key)
				}
				selected[key] = true
			}
			hosts := map[string]bool{}
			for _, key := range n.InaccessibleHosts {
				i := strings.LastIndexByte(key, '/')
				if i < 0 || !generated[key[:i]] || !strings.HasPrefix(key, dc.Name+"/") || (len(selected) > 0 && !selected[key[:i]]) || !validStorageHost(key[i+1:]) || hosts[key] {
					return fmt.Errorf("scenario: datacenter %q: NFS datastore %q has invalid host %q", dc.Name, n.Name, key)
				}
				hosts[key] = true
			}
		}
		clusterStorage := map[string]bool{}
		storeNames := map[string]bool{}
		for _, n := range dc.NFS {
			storeNames[n.Name] = true
		}
		for _, st := range dc.ISCSI {
			if !generated[st.Cluster] || !strings.HasPrefix(st.Cluster, dc.Name+"/") || clusterStorage[st.Cluster] {
				return fmt.Errorf("scenario: datacenter %q: invalid or duplicate iSCSI cluster %q", dc.Name, st.Cluster)
			}
			clusterStorage[st.Cluster] = true
			adapters := map[string]bool{}
			adapterIDs := map[uint32]bool{}
			for _, a := range st.Adapters {
				id := StorageNumber(a.Name) % 1000000
				if !nameRe.MatchString(a.Name) || adapters[a.Name] || adapterIDs[id] || (a.Kind != "software" && a.Kind != "hardware") {
					return fmt.Errorf("scenario: invalid iSCSI adapter %q", a.Name)
				}
				adapters[a.Name] = true
				adapterIDs[id] = true
			}
			targets := map[string]bool{}
			targetIDs := map[uint32]bool{}
			for _, t := range st.Targets {
				id := StorageNumber(t.Name) & 0x7fffffff
				if !nameRe.MatchString(t.Name) || targets[t.Name] || targetIDs[id] || t.Address == "" || !strings.HasPrefix(t.IQN, "iqn.") || t.Port < 0 || t.Port > 65535 {
					return fmt.Errorf("scenario: invalid iSCSI target %q", t.Name)
				}
				targets[t.Name] = true
				targetIDs[id] = true
			}
			lunNumbers := map[string]bool{}
			for _, lun := range st.LUNs {
				id := fmt.Sprintf("%s/%d", lun.Target, lun.Number)
				if !nameRe.MatchString(lun.Name) || storeNames[lun.Name] || !targets[lun.Target] || lun.Number < 0 || lunNumbers[id] || lun.Capacity <= 0 || lun.Capacity%512 != 0 || lun.FreeSpace < 0 || lun.FreeSpace > lun.Capacity || (lun.Policy != "" && lun.Policy != "VMW_PSP_RR" && lun.Policy != "VMW_PSP_FIXED" && lun.Policy != "VMW_PSP_MRU") {
					return fmt.Errorf("scenario: invalid iSCSI LUN %q", lun.Name)
				}
				storeNames[lun.Name], lunNumbers[id] = true, true
				paths := map[string]bool{}
				for _, p := range lun.Paths {
					key := p.Adapter + "/" + p.Host
					if !adapters[p.Adapter] || paths[key] || (p.Host != "" && (!strings.HasPrefix(p.Host, st.Cluster+"/") || !validStorageHost(strings.TrimPrefix(p.Host, st.Cluster+"/")))) || (p.State != "active" && p.State != "standby" && p.State != "dead" && p.State != "disabled") {
						return fmt.Errorf("scenario: invalid path for iSCSI LUN %q", lun.Name)
					}
					paths[key] = true
				}
				if len(lun.Paths) == 0 {
					return fmt.Errorf("scenario: iSCSI LUN %q needs a path", lun.Name)
				}
			}
		}
	}
	keys := map[string]bool{}
	for i, c := range s.Clusters {
		if !keyRe.MatchString(c.Key) {
			return fmt.Errorf("scenario: clusters[%d]: key %q must be <datacenter>/<cluster>", i, c.Key)
		}
		if !generated[c.Key] {
			return fmt.Errorf("scenario: clusters[%d]: %q is not a cluster the datacenters generate", i, c.Key)
		}
		if keys[c.Key] {
			return fmt.Errorf("scenario: cluster %q overridden twice", c.Key)
		}
		if c.SeedEpoch < 0 {
			return fmt.Errorf("scenario: clusters[%s]: seedEpoch must not be negative", c.Key)
		}
		keys[c.Key] = true
		if err := merge(s.Defaults, c.ClusterSpec).check("clusters[" + c.Key + "]"); err != nil {
			return err
		}
	}
	for _, c := range s.Clusters {
		if len(c.RemovedHosts) == 0 {
			continue
		}
		total := s.hostNumbers(c.Key)
		gone := map[int]bool{}
		for _, n := range c.RemovedHosts {
			if n < 1 || n > total || gone[n] {
				return fmt.Errorf("scenario: clusters[%s]: removedHosts: %d is not a host of the cluster, or is listed twice", c.Key, n)
			}
			gone[n] = true
		}
		if total-len(gone) < MinHosts {
			return fmt.Errorf("scenario: clusters[%s]: removedHosts leaves fewer than %d hosts (vSAN needs three hosts)", c.Key, MinHosts)
		}
	}
	for _, v := range s.Vary.Hosts {
		if v < MinHosts {
			return fmt.Errorf("scenario: vary.hosts values must be at least %d (vSAN needs three hosts)", MinHosts)
		}
	}
	for _, v := range s.Vary.ClaimedDevices {
		if v < 1 {
			return errors.New("scenario: vary.claimedDevices values must be at least 1")
		}
	}
	for _, v := range s.Vary.Threads {
		if v < 1 {
			return errors.New("scenario: vary.threads values must be at least 1")
		}
	}
	for name, r := range map[string]*Range{"vmsPerHost": s.Vary.VMsPerHost, "domWorlds": s.Vary.DomWorlds} {
		if r != nil && (r.Min < 0 || r.Max < r.Min) {
			return fmt.Errorf("scenario: vary.%s: need 0 <= min <= max", name)
		}
	}
	// Host faults must refer to a host the resolved topology will actually
	// create. The first draw is the host count, using the same seed as topology.
	hostExists := func(key string) bool {
		i := strings.LastIndexByte(key, '/')
		if i < 0 || !validStorageHost(key[i+1:]) {
			return false
		}
		ordinal, ok := StorageHostOrdinal(key[i+1:])
		if !ok {
			return false
		}
		cluster := key[:i]
		return ordinal <= s.hostNumbers(cluster) && !s.RemovedHosts(cluster)[ordinal]
	}
	for _, dc := range s.Datacenters {
		for _, n := range dc.NFS {
			for _, host := range n.InaccessibleHosts {
				if !hostExists(host) {
					return fmt.Errorf("scenario: NFS datastore %q references a host that does not exist: %q", n.Name, host)
				}
			}
		}
		for _, st := range dc.ISCSI {
			for _, lun := range st.LUNs {
				for _, path := range lun.Paths {
					if path.Host != "" && !hostExists(path.Host) {
						return fmt.Errorf("scenario: iSCSI LUN %q references a host that does not exist: %q", lun.Name, path.Host)
					}
				}
			}
		}
	}
	users := map[string]bool{}
	for i, u := range s.Auth.Users {
		if u.Name == "" {
			return fmt.Errorf("scenario: auth.users[%d]: name is required", i)
		}
		if users[u.Name] {
			return fmt.Errorf("scenario: auth.users[%d]: %q listed twice", i, u.Name)
		}
		users[u.Name] = true
		if u.Role != "" && u.Role != "administrator" && u.Role != "read-only" {
			return fmt.Errorf("scenario: auth.users[%d] (%s): role must be administrator or read-only", i, u.Name)
		}
		if (u.Password == "") == (u.PasswordEnv == "") {
			return fmt.Errorf("scenario: auth.users[%d] (%s): exactly one of password and passwordEnv is required", i, u.Name)
		}
	}
	if (s.TLS.CertFile == "") != (s.TLS.KeyFile == "") {
		return errors.New("scenario: tls: certFile and keyFile go together")
	}
	cat, err := catalog.Load()
	if err != nil {
		return err
	}
	for i, f := range s.Faults {
		if (f.Family == "") == (f.Method == "") {
			return fmt.Errorf("scenario: faults[%d]: exactly one of family and method is required", i)
		}
		if f.Family != "" {
			if _, ok := cat.Families[f.Family]; !ok {
				return fmt.Errorf("scenario: faults[%d]: %q is not an entity family the service advertises", i, f.Family)
			}
		}
		if f.Method != "" && !FaultMethods[f.Method] {
			return fmt.Errorf("scenario: faults[%d]: method %q takes no injected fault (one of %s)", i, f.Method, strings.Join(faultMethodNames(), ", "))
		}
		switch f.Fault {
		case FaultInvalidArgument, FaultNotSupported, FaultSystemError, FaultTimeout:
		default:
			return fmt.Errorf("scenario: faults[%d]: fault %q is not one of %s, %s, %s, %s", i, f.Fault,
				FaultInvalidArgument, FaultNotSupported, FaultSystemError, FaultTimeout)
		}
		for _, k := range f.Clusters {
			if !generated[k] {
				return fmt.Errorf("scenario: faults[%d]: %q is not a cluster the datacenters generate", i, k)
			}
		}
		if f.From != nil && f.To != nil && f.To.Before(*f.From) {
			return fmt.Errorf("scenario: faults[%d]: to is before from", i)
		}
	}
	if err := s.validateConditions(cat, generated); err != nil {
		return err
	}
	for name, d := range map[string]*Delay{"perfQuery": s.Latency.PerfQuery, "sdk": s.Latency.SDK} {
		if d != nil && (d.FixedMs < 0 || d.JitterMs < 0) {
			return fmt.Errorf("scenario: latency.%s: delays must not be negative", name)
		}
	}
	for fam, d := range s.Latency.Families {
		if _, ok := cat.Families[fam]; !ok {
			return fmt.Errorf("scenario: latency.families: %q is not an entity family the service advertises", fam)
		}
		if d.FixedMs < 0 || d.JitterMs < 0 {
			return fmt.Errorf("scenario: latency.families[%s]: delays must not be negative", fam)
		}
	}
	return nil
}

// FaultMethods are the /vsanHealth methods a Fault may select by name.
var FaultMethods = map[string]bool{
	"VsanPerfQueryNodeInformation":    true,
	"VsanClusterGetConfig":            true,
	"QueryVsanManagedDisks":           true,
	"VsanQueryVcClusterHealthSummary": true,
}

func faultMethodNames() []string {
	var out []string
	for m := range FaultMethods {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func (c ClusterSpec) check(where string) error {
	switch {
	case c.Architecture != "" && c.Architecture != "esa" && c.Architecture != "osa":
		return fmt.Errorf("scenario: %s: architecture must be esa or osa", where)
	case c.Hosts < MinHosts:
		return fmt.Errorf("scenario: %s: hosts must be at least %d (vSAN needs three hosts)", where, MinHosts)
	case c.Host.Threads < 1:
		return fmt.Errorf("scenario: %s: host.threads must be at least 1", where)
	case Int(c.Host.DomWorlds) < 0:
		return fmt.Errorf("scenario: %s: host.domWorlds must not be negative", where)
	case c.Host.VsanPnics < 1:
		return fmt.Errorf("scenario: %s: host.vsanPnics must be at least 1", where)
	case c.Host.ClaimedDevices < 1:
		return fmt.Errorf("scenario: %s: host.claimedDevices must be at least 1", where)
	case Int(c.Host.LocalDevices) < 0:
		return fmt.Errorf("scenario: %s: host.localDevices must not be negative", where)
	case Int(c.VMs.PerHost) < 0:
		return fmt.Errorf("scenario: %s: vms.perHost must not be negative", where)
	case c.VMs.DisksPerVM < 1:
		return fmt.Errorf("scenario: %s: vms.disksPerVM must be at least 1", where)
	}
	return nil
}

// merge overlays the non-zero fields of over onto base.
func merge(base, over ClusterSpec) ClusterSpec {
	out := base
	if over.Architecture != "" {
		out.Architecture = over.Architecture
	}
	if over.StatsPrimary != nil {
		out.StatsPrimary = over.StatsPrimary
	}
	if over.Hosts != 0 {
		out.Hosts = over.Hosts
	}
	if over.Dedup != nil {
		out.Dedup = over.Dedup
	}
	if over.Compression != nil {
		out.Compression = over.Compression
	}
	if over.Host.Threads != 0 {
		out.Host.Threads = over.Host.Threads
	}
	if over.Host.DomWorlds != nil {
		out.Host.DomWorlds = over.Host.DomWorlds
	}
	if over.Host.VsanPnics != 0 {
		out.Host.VsanPnics = over.Host.VsanPnics
	}
	if over.Host.ClaimedDevices != 0 {
		out.Host.ClaimedDevices = over.Host.ClaimedDevices
	}
	if over.Host.LocalDevices != nil {
		out.Host.LocalDevices = over.Host.LocalDevices
	}
	if over.VMs.PerHost != nil {
		out.VMs.PerHost = over.VMs.PerHost
	}
	if over.VMs.DisksPerVM != 0 {
		out.VMs.DisksPerVM = over.VMs.DisksPerVM
	}
	return out
}

// Override returns the explicit override for a cluster key, if any.
func (s *Scenario) Override(key string) (ClusterSpec, bool) {
	for _, c := range s.Clusters {
		if c.Key == key {
			return c.ClusterSpec, true
		}
	}
	return ClusterSpec{}, false
}

// Resolve returns the effective spec for a cluster: the defaults, then the
// cluster's draws from Vary (drawn by the caller and passed as drawn), then
// the explicit override.
func (s *Scenario) Resolve(key string, drawn ClusterSpec) ClusterSpec {
	out := merge(s.Defaults, drawn)
	if o, ok := s.Override(key); ok {
		out = merge(out, o)
	}
	return out
}

// Credentials resolves the configured users to name -> password, reading
// passwordEnv from the environment. The result never goes to a log.
func (s *Scenario) Credentials() (map[string]string, error) {
	out := map[string]string{}
	for _, u := range s.Auth.Users {
		p := u.Password
		if u.PasswordEnv != "" {
			p = os.Getenv(u.PasswordEnv)
			if p == "" {
				return nil, fmt.Errorf("scenario: auth user %s: environment variable %s is not set", u.Name, u.PasswordEnv)
			}
		}
		out[u.Name] = p
	}
	return out, nil
}

// hostNumbers is how many host numbers a cluster uses: its resolved host
// count, removed hosts included. The first draw from the cluster's own seed
// is the host count, as in package topology.
func (s *Scenario) hostNumbers(key string) int {
	r := registry.InMemory(s.Seed)
	return s.Resolve(key, ClusterSpec{Hosts: s.Vary.DrawHosts(DrawSource(r.Sub(key)))}).Hosts
}

// HostNumbers is the exported form of hostNumbers, for the console.
func (s *Scenario) HostNumbers(key string) int { return s.hostNumbers(key) }

// RemovedHosts is the set of host numbers a cluster leaves out.
func (s *Scenario) RemovedHosts(key string) map[int]bool {
	for _, c := range s.Clusters {
		if c.Key == key && len(c.RemovedHosts) > 0 {
			out := make(map[int]bool, len(c.RemovedHosts))
			for _, n := range c.RemovedHosts {
				out[n] = true
			}
			return out
		}
	}
	return nil
}

// ClusterKeys lists every cluster the scenario generates, in order.
func (s *Scenario) ClusterKeys() []string {
	var out []string
	for _, dc := range s.Datacenters {
		for n := 1; n <= dc.Clusters; n++ {
			if k := ClusterKey(dc.Name, n); !s.IsRemoved(k) {
				out = append(out, k)
			}
		}
	}
	return out
}

// ClusterKey names the n-th cluster of a datacenter.
func ClusterKey(datacenter string, n int) string {
	return fmt.Sprintf("%s/cluster-%02d", datacenter, n)
}

// IsRemoved reports whether the scenario leaves a generated cluster out.
func (s *Scenario) IsRemoved(key string) bool {
	for _, k := range s.Removed {
		if k == key {
			return true
		}
	}
	return false
}

// SeedEpoch is the cluster's telemetry seed epoch (0 until reset).
func (s *Scenario) SeedEpoch(key string) int {
	for _, c := range s.Clusters {
		if c.Key == key {
			return c.SeedEpoch
		}
	}
	return 0
}

// FamilyFaults lists the perf-family faults, sorted for deterministic
// evaluation.
func (s *Scenario) FamilyFaults() []Fault {
	var out []Fault
	for _, f := range s.Faults {
		if f.Family != "" {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Family < out[j].Family })
	return out
}
