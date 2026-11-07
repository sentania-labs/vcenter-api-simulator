// Package vsanhealth serves the /vsanHealth managed objects the reference
// management pack's collection path calls, merged into govmomi vcsim's stock
// /vsanHealth registry so one session store covers /sdk and /vsanHealth:
//
//	VsanPerformanceManager      VsanPerfQueryNodeInformation, VsanPerfGetSupportedEntityTypes, VsanPerfQueryPerf
//	VsanVcClusterConfigSystem   VsanClusterGetConfig (replaces vcsim's, which invents a second cluster uuid)
//	VsanVcDiskManagementSystem  QueryVsanManagedDisks
//	VsanVcClusterHealthSystem   VsanQueryVcClusterHealthSummary (hclInfo)
//
// Everything else on /vsanHealth keeps vcsim's behaviour (ManagedObjectNotFound
// or MethodNotFound). Response shapes follow fixtures/live/9.1.1.
package vsanhealth

import (
	"time"

	"github.com/vmware/govmomi/simulator"
	vim25types "github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// Env is what the handlers need: the topology, the catalog, a clock, and the
// MoRefs vcsim assigned to the topology's clusters and hosts.
type Env struct {
	Topo    *topology.Topology
	Catalog *catalog.Catalog
	Now     func() time.Time

	clusters map[vim25types.ManagedObjectReference]*topology.Cluster
	hosts    map[vim25types.ManagedObjectReference]hostEntry
	hostRefs map[*topology.Host]vim25types.ManagedObjectReference
}

type hostEntry struct {
	host    *topology.Host
	cluster *topology.Cluster
}

// NewEnv returns an Env with a real clock.
func NewEnv(t *topology.Topology, c *catalog.Catalog) *Env {
	return &Env{
		Topo:     t,
		Catalog:  c,
		Now:      time.Now,
		clusters: map[vim25types.ManagedObjectReference]*topology.Cluster{},
		hosts:    map[vim25types.ManagedObjectReference]hostEntry{},
		hostRefs: map[*topology.Host]vim25types.ManagedObjectReference{},
	}
}

// RegisterCluster records the MoRef vcsim assigned to a cluster.
func (e *Env) RegisterCluster(ref vim25types.ManagedObjectReference, c *topology.Cluster) {
	e.clusters[ref] = c
}

// RegisterHost records the MoRef vcsim assigned to a host.
func (e *Env) RegisterHost(ref vim25types.ManagedObjectReference, c *topology.Cluster, h *topology.Host) {
	e.hosts[ref] = hostEntry{host: h, cluster: c}
	e.hostRefs[h] = ref
}

// Cluster resolves a cluster MoRef.
func (e *Env) Cluster(ref *vim25types.ManagedObjectReference) *topology.Cluster {
	if ref == nil {
		return nil
	}
	return e.clusters[*ref]
}

// Host resolves a host MoRef.
func (e *Env) Host(ref vim25types.ManagedObjectReference) (*topology.Host, *topology.Cluster) {
	h := e.hosts[ref]
	return h.host, h.cluster
}

// Refs of the managed objects, as the management pack's bindings name them.
var (
	PerfManagerRef  = vim25types.ManagedObjectReference{Type: "VsanPerformanceManager", Value: "vsan-performance-manager"}
	ConfigSystemRef = vim25types.ManagedObjectReference{Type: "VsanVcClusterConfigSystem", Value: "vsan-cluster-config-system"}
	DiskMgmtRef     = vim25types.ManagedObjectReference{Type: "VimClusterVsanVcDiskManagementSystem", Value: "vsan-disk-management-system"}
	HealthSystemRef = vim25types.ManagedObjectReference{Type: "VsanVcClusterHealthSystem", Value: "vsan-cluster-health-system"}
)

// Install merges the handlers into the service's /vsanHealth registry. It must
// run after Service.NewServer, which is where vcsim registers the stock
// /vsanHealth endpoint (github.com/vmware/govmomi/vsan/simulator).
func Install(s *simulator.Service, env *Env) {
	r := simulator.NewRegistry()
	r.Namespace = "vsan"
	r.Path = "/vsanHealth"
	r.Put(&PerfManager{ManagedObjectReference: PerfManagerRef, env: env})
	r.Put(&ClusterConfigSystem{ManagedObjectReference: ConfigSystemRef, env: env})
	r.Put(&DiskManagementSystem{ManagedObjectReference: DiskMgmtRef, env: env})
	r.Put(&ClusterHealthSystem{ManagedObjectReference: HealthSystemRef, env: env})
	s.RegisterSDK(r)
}
