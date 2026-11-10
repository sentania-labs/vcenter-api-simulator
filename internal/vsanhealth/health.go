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

// The current bindings omit the build fields present in the 9.1 response.
type healthHostVersion struct {
	Hostname string `xml:"hostname"`
	Version  string `xml:"version"`
	Build    string `xml:"build"`
}
type healthVersions struct {
	Hosts           []healthHostVersion `xml:"hostResults"`
	VcVersion       string              `xml:"vcVersion"`
	IssueFound      bool                `xml:"issueFound"`
	UpgradePossible bool                `xml:"upgradePossible"`
	VcBuild         string              `xml:"vcBuild"`
}
type osaHealthSummary struct {
	vsantypes.VsanClusterHealthSummary
	Versions healthVersions `xml:"clusterVersions"`
}
type osaHealthResponse struct {
	Returnval osaHealthSummary `xml:"returnval"`
}
type osaHealthBody struct {
	Res *osaHealthResponse `xml:"urn:vsan VsanQueryVcClusterHealthSummaryResponse,omitempty"`
}

func (*osaHealthBody) Fault() *soap.Fault { return nil }

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
	if fault := s.env.methodFault("VsanQueryVcClusterHealthSummary", cl); fault != nil {
		return &methods.VsanQueryVcClusterHealthSummaryBody{Fault_: fault}
	}
	if cl != nil {
		sum.ClusterStatus = &vsantypes.VsanClusterHealthSystemStatusResult{Status: "green", GoalState: "installed"}
		sum.ClusterVersions = &vsantypes.VsanClusterHealthSystemVersionResult{VcVersion: s.env.Catalog.About.Version, UpgradePossible: vim25types.NewBool(false)}
		for _, h := range cl.Hosts {
			sum.ClusterStatus.TrackedHostsStatus = append(sum.ClusterStatus.TrackedHostsStatus, vsantypes.VsanHostHealthSystemStatusResult{Hostname: h.Name, Status: "green"})
			sum.ClusterVersions.HostResults = append(sum.ClusterVersions.HostResults, vsantypes.VsanHostHealthSystemVersionResult{Hostname: h.Name, Version: s.env.Catalog.About.EsxiVersion})
		}
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
	if cl != nil && cl.Spec.Architecture == "osa" {
		versions := healthVersions{VcVersion: s.env.Catalog.About.Version, VcBuild: s.env.Catalog.About.Build}
		for _, host := range cl.Hosts {
			versions.Hosts = append(versions.Hosts, healthHostVersion{Hostname: host.Name, Version: s.env.Catalog.About.EsxiVersion, Build: s.env.Catalog.About.EsxiBuild})
		}
		return &osaHealthBody{Res: &osaHealthResponse{Returnval: osaHealthSummary{VsanClusterHealthSummary: sum, Versions: versions}}}
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
