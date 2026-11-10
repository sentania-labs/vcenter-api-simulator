package control

import (
	"sort"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/sentania/vcenter-api-simulator/internal/vsanhealth"
)

// Scopes: which entity kinds the management pack's adapter instance asks
// for. The simulator serves every kind either way; the scope only drives the
// projections and the mismatch warning (a product decision).
const (
	ScopeAll       = "all"
	ScopeDashboard = "dashboard"
)

// dashboardKinds are the kinds the pack's shipped views reference, from the
// recon report's list of the dashboard scope (the pack's metric_scope.py at
// the pinned commit), plus the ESA dedup kinds, which are a per-cluster
// switch. The list is carried here, not read from the pack at run time.
var dashboardKinds = map[string]bool{
	"cache-disk": true, "capacity-disk": true,
	"cluster-domclient": true, "cluster-domcompmgr": true, "cluster-domowner": true,
	"cluster-rdt-network-latency": true, "cluster-zdom-top-stats": true, "vsan-cluster-capacity": true,
	"cmmds-net": true, "host-domclient": true, "host-domcompmgr": true, "host-domowner": true,
	"host-zdom-top-stats": true, "rdt-network-latency": true, "vnic-rdt-network-latency": true,
	"vsan-cpu": true, "vsan-memory": true, "vsan-esa-disk-layer": true, "vsan-esa-disk-scsifw": true,
	"vsan-host-net": true, "vsan-tcpip-stats": true, "vsan-vnic-net": true, "vsan-pnic-net": true,
	"zdom-vtx": true, "virtual-machine": true, "virtual-disk": true, "vscsi": true,
	"cluster-esa-dedup-domclient-io": true, "cluster-esa-dedup-domcompmgr-io": true,
	"cluster-esa-dedup-domowner-io": true, "cluster-esa-dedup-store-chunksvc": true,
	"cluster-esa-dedup-store-hashsvc": true, "host-esa-dedup-domclient-io": true,
	"host-esa-dedup-domcompmgr-io": true, "host-esa-dedup-domowner-io": true,
	"host-esa-dedup-store-chunksvc": true, "host-esa-dedup-store-hashsvc": true,
}

// Estimated wire cost of one cycle's answers: a series' CSV element and its
// three samples, an entity's wrapper. Estimates, shown as such.
const (
	bytesPerSeries = 170
	bytesPerEntity = 130
)

// FamilyProj is one entity family's projected load for one collection cycle.
type FamilyProj struct {
	Family    string `json:"family"`
	Behaviour string `json:"behaviour"`
	Dashboard bool   `json:"dashboard"`
	Objects   int    `json:"objects"`
	Metrics   int    `json:"metrics"` // maximum labels per object across clusters
	Series    int    `json:"series"`
}

// Load is the projected cost of one cycle.
type Load struct {
	Kinds    int   `json:"kinds"`
	Entities int   `json:"entities"`
	Series   int   `json:"series"`
	Calls    int   `json:"calls"`
	Bytes    int64 `json:"bytes"`
}

// Projection is what a topology costs the collector, computed from the
// topology and the catalog and never measured.
type Projection struct {
	Clusters    int `json:"clusters"`
	OSAClusters int `json:"osaClusters"`
	Hosts       int `json:"hosts"`
	VMs         int `json:"vms"`
	VDisks      int `json:"vdisks"`
	NVMe        int `json:"nvme"`
	LocalDisks  int `json:"localDevices"`
	LogicalCPUs int `json:"logicalCpus"`
	DomWorlds   int `json:"domWorlds"`
	Datastores  int `json:"datastores"`

	Families []FamilyProj `json:"families"`
	All      Load         `json:"all"`
	Dash     Load         `json:"dashboard"`
	// EmptyKinds counts families with no projected entities on any cluster.
	EmptyKinds int `json:"emptyKinds"`
	// EmptyCalls counts per-cluster queries returning no samples or a fault.
	EmptyCalls int `json:"emptyCalls"`

	PerCluster map[string]ClusterLoad `json:"perCluster"`
}

// ClusterLoad is one cluster's share (all kinds).
type ClusterLoad struct {
	Entities int `json:"entities"`
	Series   int `json:"series"`
}

// Selected picks the load of a scope.
func (p *Projection) Selected(scope string) Load {
	if scope == ScopeDashboard {
		return p.Dash
	}
	return p.All
}

// Project computes the cost of a topology.
func Project(cat *catalog.Catalog, t *topology.Topology) *Projection {
	p := &Projection{PerCluster: map[string]ClusterLoad{}}
	namesSet := map[string]bool{}
	for _, cl := range t.Clusters() {
		for _, name := range vsanhealth.PerfFamilyNames(cat, cl) {
			namesSet[name] = true
		}
	}
	if len(t.Clusters()) == 0 {
		for name := range cat.Families {
			namesSet[name] = true
		}
	}
	names := make([]string, 0, len(namesSet))
	for name := range namesSet {
		names = append(names, name)
	}
	sort.Strings(names)
	fam := map[string]*FamilyProj{}
	for _, name := range names {
		fam[name] = &FamilyProj{Family: name, Dashboard: dashboardKinds[name]}
	}
	for _, cl := range t.Clusters() {
		p.Clusters++
		if cl.Spec.Architecture == "osa" {
			p.OSAClusters++
		}
		p.Hosts += len(cl.Hosts)
		p.VMs += len(cl.VMs)
		p.Datastores++ // the cluster's vSAN datastore
		var cEnt, cSer int
		for _, h := range cl.Hosts {
			p.LogicalCPUs += h.Threads
			p.DomWorlds += len(h.Worlds)
			for _, d := range h.Devices {
				if d.Claimed {
					p.NVMe++
				} else {
					p.LocalDisks++
					p.Datastores++ // the local VMFS
				}
			}
		}
		for _, vm := range cl.VMs {
			p.VDisks += len(vm.Disks)
		}
		for _, n := range names {
			f, known := vsanhealth.EffectiveFamily(cat, cl, n)
			// Unknown names are rejected by the service as InvalidArgument.
			if !known {
				f.Behaviour = catalog.InvalidArgument
			}
			row := fam[n]
			behaviour := string(f.Behaviour)
			if row.Behaviour == "" {
				row.Behaviour = behaviour
			} else if row.Behaviour != behaviour {
				row.Behaviour = "mixed"
			}
			if f.Behaviour != catalog.Data {
				p.EmptyCalls++
				continue
			}
			// A family the engine cannot enumerate answers with a fault,
			// which counts as an empty call.
			objs, _ := vsanhealth.PerfEntityCount(cat, cl, n)
			metrics := len(f.Labels)
			if objs == 0 {
				p.EmptyCalls++
			}
			if row.Metrics != 0 && row.Metrics != metrics {
				row.Behaviour = "mixed"
			}
			if metrics > row.Metrics {
				row.Metrics = metrics
			}
			row.Objects += objs
			row.Series += objs * metrics
			cEnt += objs
			cSer += objs * metrics
		}
		p.PerCluster[cl.Key] = ClusterLoad{Entities: cEnt, Series: cSer}
	}
	for _, n := range names {
		f := fam[n]
		if f.Objects == 0 {
			p.EmptyKinds++
		}
		p.Families = append(p.Families, *f)
		for _, l := range []*Load{&p.All, &p.Dash} {
			if l == &p.Dash && !f.Dashboard {
				continue
			}
			l.Kinds++
			l.Entities += f.Objects
			l.Series += f.Series
		}
	}
	for _, l := range []*Load{&p.All, &p.Dash} {
		l.Calls = l.Kinds * p.Clusters
		l.Bytes = int64(l.Series)*bytesPerSeries + int64(l.Entities)*bytesPerEntity
	}
	sort.SliceStable(p.Families, func(i, j int) bool { return p.Families[i].Series > p.Families[j].Series })
	return p
}
