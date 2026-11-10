// Package vsanhealth serves the /vsanHealth managed objects the reference
// management pack's collection path calls, registered on the vcsim service so
// one session store covers /sdk and /vsanHealth:
//
//	VsanPerformanceManager      VsanPerfQueryNodeInformation, VsanPerfGetSupportedEntityTypes, VsanPerfQueryPerf
//	VsanVcClusterConfigSystem   VsanClusterGetConfig
//	VsanVcDiskManagementSystem  QueryVsanManagedDisks
//	VsanVcClusterHealthSystem   VsanQueryVcClusterHealthSummary (hclInfo)
//
// Nothing else is registered, so any other managed object on /vsanHealth
// (vcsim's stock stretched-cluster system included) is ManagedObjectNotFound.
// vcsim's own vsan/simulator package must stay unimported. Response shapes
// follow fixtures/live/9.1.1.
package vsanhealth

import (
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
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
	// Knobs are the scenario's fault and latency settings; nil means none.
	Knobs *Knobs

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

// clusterByValue resolves a cluster MoRef value.
func (e *Env) clusterByValue(value string) *topology.Cluster {
	return e.clusters[vim25types.ManagedObjectReference{Type: "ClusterComputeResource", Value: value}]
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
	PerfManagerRef      = vim25types.ManagedObjectReference{Type: "VsanPerformanceManager", Value: "vsan-performance-manager"}
	ConfigSystemRef     = vim25types.ManagedObjectReference{Type: "VsanVcClusterConfigSystem", Value: "vsan-cluster-config-system"}
	DiskMgmtRef         = vim25types.ManagedObjectReference{Type: "VimClusterVsanVcDiskManagementSystem", Value: "vsan-disk-management-system"}
	HealthSystemRef     = vim25types.ManagedObjectReference{Type: "VsanVcClusterHealthSystem", Value: "vsan-cluster-health-system"}
	StretchSystemRef    = vim25types.ManagedObjectReference{Type: "VimClusterVsanVcStretchedClusterSystem", Value: "vsan-stretched-cluster-system"}
	CapabilitySystemRef = vim25types.ManagedObjectReference{Type: "VsanCapabilitySystem", Value: "vsan-vc-capability-system"}
	SpaceSystemRef      = vim25types.ManagedObjectReference{Type: "VsanSpaceReportSystem", Value: "vsan-cluster-space-report-system"}
)

// Install registers the /vsanHealth endpoint on the service. It must run after
// Service.NewServer, which sets up the service's endpoint table.
func Install(s *simulator.Service, env *Env) {
	r := simulator.NewRegistry()
	r.Namespace = "vsan"
	r.Path = "/vsanHealth"
	r.Put(&PerfManager{ManagedObjectReference: PerfManagerRef, env: env})
	r.Put(&ClusterConfigSystem{ManagedObjectReference: ConfigSystemRef, env: env})
	r.Put(&DiskManagementSystem{ManagedObjectReference: DiskMgmtRef, env: env})
	r.Put(&ClusterHealthSystem{ManagedObjectReference: HealthSystemRef, env: env})
	r.Put(&SpaceReportSystem{ManagedObjectReference: SpaceSystemRef, env: env})
	r.Put(&StretchSystem{ManagedObjectReference: StretchSystemRef, env: env})
	r.Put(&CapabilitySystem{ManagedObjectReference: CapabilitySystemRef, env: env})
	s.RegisterSDK(r)
}

// methodFault returns the scenario's injected fault for a method on a
// cluster, if any is active.
func (e *Env) methodFault(method string, cl *topology.Cluster) *soap.Fault {
	if e.Knobs == nil || cl == nil {
		return nil
	}
	if inj := e.Knobs.MethodFault(method, cl.Key); inj != nil {
		return e.Knobs.Fault(inj, method)
	}
	return nil
}
