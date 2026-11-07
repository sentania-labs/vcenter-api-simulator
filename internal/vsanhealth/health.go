package vsanhealth

import (
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	vim25types "github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vsan/methods"
	vsantypes "github.com/vmware/govmomi/vsan/types"
)

// ClusterHealthSystem is VsanVcClusterHealthSystem:vsan-cluster-health-system.
type ClusterHealthSystem struct {
	vim25types.ManagedObjectReference
	env *Env
}

// hclDbLastUpdate is fixed; the capture's value was the HCL database date.
var hclDbLastUpdate = time.Date(2026, 11, 3, 9, 5, 2, 0, time.UTC)

// VsanQueryVcClusterHealthSummary serves the hclInfo section the management
// pack reads for controller driver versions (healthsummary-domain-c6004).
func (s *ClusterHealthSystem) VsanQueryVcClusterHealthSummary(ctx *simulator.Context, req *vsantypes.VsanQueryVcClusterHealthSummary) soap.HasFault {
	now := s.env.Now().UTC()
	sum := vsantypes.VsanClusterHealthSummary{
		Timestamp:                &now,
		OverallHealth:            "green",
		OverallHealthDescription: "",
	}
	cl := s.env.Cluster(req.Cluster)
	if cl != nil {
		hcl := &vsantypes.VsanClusterHclInfo{HclDbLastUpdate: &hclDbLastUpdate, HclDbAgeHealth: "green"}
		for _, h := range cl.Hosts {
			hr := vsantypes.VsanHostHclInfo{Hostname: h.Name, HclChecked: true, ReleaseName: "ESXi " + majorMinor(s.env.Catalog.About.EsxiVersion)}
			for _, d := range h.Devices {
				c := vsantypes.VsanHclControllerInfo{
					DeviceName:             d.HBA,
					DeviceDisplayName:      d.HBAModel,
					DriverName:             d.Driver,
					DriverVersion:          "1.4.0.8-2vmw.911.0.25714478",
					VendorId:               65535,
					DeviceId:               65535,
					SubVendorId:            65535,
					SubDeviceId:            65535,
					DeviceOnHcl:            vim25types.NewBool(false),
					ReleaseSupported:       vim25types.NewBool(false),
					DriverVersionSupported: vim25types.NewBool(false),
					FwVersionSupported:     vim25types.NewBool(false),
					CacheConfigSupported:   vim25types.NewBool(false),
					RaidConfigSupported:    vim25types.NewBool(false),
					FwVersion:              d.Revision,
					UsedByVsan:             vim25types.NewBool(d.Claimed),
					Disks: []vsantypes.VsanHclDiskInfo{{
						DeviceName: d.CanonicalName,
						Model:      padModel(d.Model),
						IsSsd:      vim25types.NewBool(true),
						VsanDisk:   d.Claimed,
					}},
					FwAuxVersion:        "N/A",
					QueueDepth:          2046,
					QueueDepthSupported: vim25types.NewBool(false),
					DiskMode:            "All Flash",
				}
				hr.Controllers = append(hr.Controllers, c)
			}
			hcl.HostResults = append(hcl.HostResults, hr)
		}
		sum.HclInfo = hcl
	}
	return &methods.VsanQueryVcClusterHealthSummaryBody{Res: &vsantypes.VsanQueryVcClusterHealthSummaryResponse{Returnval: sum}}
}

func majorMinor(v string) string {
	n := 0
	for i, ch := range v {
		if ch == '.' {
			n++
			if n == 2 {
				return v[:i]
			}
		}
	}
	return v
}
