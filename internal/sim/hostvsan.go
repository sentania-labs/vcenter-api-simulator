package sim

import (
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/sentania/vcenter-api-simulator/internal/vsanhealth"
)

// HostVsanSystem is the per-host HostVsanSystem vcsim lacks: the management
// pack reads config.clusterInfo.nodeUuid through the property collector and
// calls QueryDisksForVsan for disk hardware (recon risk 2).
type HostVsanSystem struct {
	mo.HostVsanSystem

	host    *topology.Host
	cluster *topology.Cluster
}

func newHostVsanSystem(hostRef types.ManagedObjectReference, cl *topology.Cluster, h *topology.Host) *HostVsanSystem {
	s := &HostVsanSystem{host: h, cluster: cl}
	s.Self = types.ManagedObjectReference{Type: "HostVsanSystem", Value: "vsanSystem-" + hostRef.Value}
	s.Config = types.VsanHostConfigInfo{
		Enabled:    types.NewBool(true),
		HostSystem: &hostRef,
		ClusterInfo: &types.VsanHostConfigInfoClusterInfo{
			Uuid:     cl.VsanUUID,
			NodeUuid: h.NodeUUID,
		},
		VsanEsaEnabled: types.NewBool(true),
	}
	return s
}

// QueryDisksForVsan reports every local disk. On the live ESA cluster all
// disks were "ineligible": the claimed one carries its vsanUuid and a
// VsanFault, the others DiskHasPartitions (capture summary.json,
// hosts[].QueryDisksForVsan).
func (s *HostVsanSystem) QueryDisksForVsan(ctx *simulator.Context, req *types.QueryDisksForVsan) soap.HasFault {
	res := &types.QueryDisksForVsanResponse{}
	for _, d := range s.host.Devices {
		r := types.VsanHostDiskResult{
			Disk:  vsanhealth.ScsiDisk(d),
			State: string(types.VsanHostDiskResultStateIneligible),
		}
		if d.Claimed {
			r.VsanUuid = d.VsanUUID
			r.Error = &types.LocalizedMethodFault{Fault: &types.VsanFault{}}
		} else {
			r.Error = &types.LocalizedMethodFault{Fault: &types.DiskHasPartitions{}}
		}
		res.Returnval = append(res.Returnval, r)
	}
	return &methods.QueryDisksForVsanBody{Res: res}
}
