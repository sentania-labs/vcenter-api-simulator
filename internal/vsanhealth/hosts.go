package vsanhealth

import (
	_ "embed"
	"encoding/json"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	vim "github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vsan/methods"
	vsan "github.com/vmware/govmomi/vsan/types"
)

func (s *DiskManagementSystem) QueryDiskMappings(ctx *simulator.Context, req *vsan.QueryDiskMappings) soap.HasFault {
	if h, _ := s.env.Host(req.Host); h == nil {
		return &methods.QueryDiskMappingsBody{Fault_: simulator.Fault("host not found", &vim.ManagedObjectNotFound{Obj: req.Host})}
	}
	return &methods.QueryDiskMappingsBody{Res: &vsan.QueryDiskMappingsResponse{}}
}

type repairTimerInfo struct {
	Max          int `xml:"maxTimeToRepair"`
	Min          int `xml:"minTimeToRepair"`
	Objects      int `xml:"objectCount"`
	TimedObjects int `xml:"objectCountWithRepairTimer"`
}
type runtimeStats struct {
	vsan.VsanHostRuntimeStats
	Repair repairTimerInfo `xml:"repairTimerInfo"`
}
type runtimeHost struct {
	Host  vim.ManagedObjectReference `xml:"host"`
	Stats runtimeStats               `xml:"stats"`
}
type runtimeResponse struct {
	Returnval []runtimeHost `xml:"returnval"`
}
type runtimeBody struct {
	Res    *runtimeResponse `xml:"urn:vsan VsanClusterGetRuntimeStatsResponse,omitempty"`
	Fault_ *soap.Fault      `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault,omitempty"`
}

func (b *runtimeBody) Fault() *soap.Fault { return b.Fault_ }

func (s *ClusterConfigSystem) VsanClusterGetRuntimeStats(ctx *simulator.Context, req *vsan.VsanClusterGetRuntimeStats) soap.HasFault {
	res := &runtimeResponse{}
	cl := s.env.Cluster(&req.Cluster)
	if cl == nil {
		return &runtimeBody{Fault_: simulator.Fault("cluster not found", &vim.ManagedObjectNotFound{Obj: req.Cluster})}
	}
	for _, h := range cl.Hosts {
		res.Returnval = append(res.Returnval, runtimeHost{Host: s.env.hostRefs[h], Stats: runtimeStats{VsanHostRuntimeStats: vsan.VsanHostRuntimeStats{ResyncIopsInfo: &vsan.ResyncIopsInfo{}, ConfigGeneration: &vsan.VsanConfigGeneration{VcUuid: s.env.Topo.InstanceUUID, GenNum: 1, GenTime: s.env.Now().UnixMilli()}, SupportedClusterSize: 64, ComponentLimitPerCluster: 1000, MaxWitnessClusters: 48}}})
	}
	return &runtimeBody{Res: res}
}

type StretchSystem struct {
	vim.ManagedObjectReference
	env *Env
}

func (s *StretchSystem) VSANVcIsWitnessHost(ctx *simulator.Context, req *vsan.VSANVcIsWitnessHost) soap.HasFault {
	if h, _ := s.env.Host(req.Host); h == nil {
		return &methods.VSANVcIsWitnessHostBody{Fault_: simulator.Fault("host not found", &vim.ManagedObjectNotFound{Obj: req.Host})}
	}
	return &methods.VSANVcIsWitnessHostBody{Res: &vsan.VSANVcIsWitnessHostResponse{Returnval: false}}
}
func (s *StretchSystem) VSANVcGetWitnessHosts(ctx *simulator.Context, req *vsan.VSANVcGetWitnessHosts) soap.HasFault {
	return &methods.VSANVcGetWitnessHostsBody{Res: &vsan.VSANVcGetWitnessHostsResponse{}}
}

//go:embed fixtures/capabilities.json
var capabilityJSON []byte

type CapabilitySystem struct {
	vim.ManagedObjectReference
	env *Env
}

func (s *CapabilitySystem) VsanGetCapabilities(ctx *simulator.Context, req *vsan.VsanGetCapabilities) soap.HasFault {
	var cap map[string][]string
	if err := json.Unmarshal(capabilityJSON, &cap); err != nil {
		return &methods.VsanGetCapabilitiesBody{Fault_: simulator.Fault(err.Error(), &vim.SystemError{})}
	}
	res := &vsan.VsanGetCapabilitiesResponse{}
	if len(req.Targets) == 0 {
		res.Returnval = append(res.Returnval, vsan.VsanCapability{Capabilities: cap["vc"]})
	} else {
		for _, target := range req.Targets {
			ref := target
			res.Returnval = append(res.Returnval, vsan.VsanCapability{Target: &ref, Capabilities: cap["cluster"]})
		}
	}
	return &methods.VsanGetCapabilitiesBody{Res: res}
}

type smartParam struct {
	Parameter string `xml:"parameter"`
	Value     int    `xml:"value"`
	Threshold *int   `xml:"threshold,omitempty"`
}
type smartDisk struct {
	Disk  string       `xml:"disk"`
	Stats []smartParam `xml:"stats"`
}
type smartHost struct {
	Hostname   string      `xml:"hostname"`
	SmartStats []smartDisk `xml:"smartStats"`
}
type smartResponse struct {
	Returnval []smartHost `xml:"returnval"`
}
type smartBody struct {
	Res    *smartResponse `xml:"urn:vsan VsanQueryVcClusterSmartStatsSummaryResponse,omitempty"`
	Fault_ *soap.Fault    `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault,omitempty"`
}

func (b *smartBody) Fault() *soap.Fault { return b.Fault_ }
func (s *ClusterHealthSystem) VsanQueryVcClusterSmartStatsSummary(ctx *simulator.Context, req *vsan.VsanQueryVcClusterSmartStatsSummary) soap.HasFault {
	cl := s.env.Cluster(&req.Cluster)
	if cl == nil {
		return &smartBody{Fault_: simulator.Fault("cluster not found", &vim.ManagedObjectNotFound{Obj: req.Cluster})}
	}
	res := &smartResponse{}
	limit, temperature := 100, -273
	for _, h := range cl.Hosts {
		host := smartHost{Hostname: h.Name}
		for _, d := range h.Devices {
			if d.Claimed {
				host.SmartStats = append(host.SmartStats, smartDisk{Disk: d.CanonicalName, Stats: []smartParam{{Parameter: "smarthealthstatus", Value: 0}, {Parameter: "smartpoweronhours", Value: 60}, {Parameter: "smartpowercyclecount", Value: 10}, {Parameter: "smartreallocatedsectorct", Value: 0, Threshold: &limit}, {Parameter: "smartdrivetemperature", Value: 30, Threshold: &temperature}, {Parameter: "smartwritesectorstotct", Value: 5000}, {Parameter: "smartreadsectorstotct", Value: 6000}}})
			}
		}
		res.Returnval = append(res.Returnval, host)
	}
	return &smartBody{Res: res}
}
