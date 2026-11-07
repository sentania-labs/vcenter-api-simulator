package sim

import (
	"fmt"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

const (
	dvsHostPorts int32 = 128
	vsanNicType        = "vsan"
)

// shapeNetwork replaces vcsim's default host networking (vSwitch0, vmnic0,
// vmk0) with the slice's: the vSAN uplinks and the vSAN vmkernel NIC, both on
// the cluster's vDS, with the names the /vsanHealth entities carry
// (topology.Host.Pnics, VsanVmknic, Stack).
func shapeNetwork(hs *simulator.HostSystem, dvs *simulator.DistributedVirtualSwitch, pg *simulator.DistributedVirtualPortgroup, h *topology.Host) {
	net := hs.Config.Network

	net.Pnic = nil
	var pnicKeys []string
	var pnicSpecs []types.DistributedVirtualSwitchHostMemberPnicSpec
	for i, name := range h.Pnics {
		key := "key-vim.host.PhysicalNic-" + name
		pnicKeys = append(pnicKeys, key)
		pnicSpecs = append(pnicSpecs, types.DistributedVirtualSwitchHostMemberPnicSpec{PnicDevice: name})
		net.Pnic = append(net.Pnic, types.PhysicalNic{
			Key:                    key,
			Device:                 name,
			Pci:                    fmt.Sprintf("0000:%02x:00.0", i+1),
			Driver:                 "nvmxnet3",
			LinkSpeed:              &types.PhysicalNicLinkInfo{SpeedMb: 10000, Duplex: true},
			ValidLinkSpecification: []types.PhysicalNicLinkInfo{{SpeedMb: 10000, Duplex: true}},
			Spec:                   types.PhysicalNicSpec{Ip: &types.HostIpConfig{}},
			Mac:                    fmt.Sprintf("00:50:56:%02x:%02x:00", h.Index+1, i+1),
		})
	}
	net.Vswitch = nil
	net.Portgroup = nil

	conn := &types.DistributedVirtualSwitchPortConnection{SwitchUuid: dvs.Uuid, PortgroupKey: pg.Key}
	vnicKey := "key-vim.host.VirtualNic-" + h.VsanVmknic
	vnic := types.HostVirtualNic{
		Device: h.VsanVmknic,
		Key:    vnicKey,
		Spec: types.HostVirtualNicSpec{
			Ip: &types.HostIpConfig{
				IpAddress:  fmt.Sprintf("192.168.100.%d", h.Index+11),
				SubnetMask: "255.255.255.0",
			},
			Mac:                    fmt.Sprintf("00:50:56:%02x:00:aa", h.Index+1),
			DistributedVirtualPort: conn,
			Mtu:                    1500,
			TsoEnabled:             types.NewBool(true),
			NetStackInstanceKey:    h.Stack,
		},
	}
	net.Vnic = []types.HostVirtualNic{vnic}

	net.NetStackInstance = []types.HostNetStackInstance{{Key: h.Stack, Name: h.Stack}}
	net.ProxySwitch = []types.HostProxySwitch{{
		DvsUuid:           dvs.Uuid,
		DvsName:           dvs.Name,
		Key:               dvs.Uuid,
		NumPorts:          dvsHostPorts,
		ConfigNumPorts:    dvsHostPorts,
		NumPortsAvailable: dvsHostPorts - 1,
		Pnic:              pnicKeys,
		Spec: types.HostProxySwitchSpec{
			Backing: &types.DistributedVirtualSwitchHostMemberPnicBacking{PnicSpec: pnicSpecs},
		},
	}}

	hs.Config.VirtualNicManagerInfo.NetConfig = []types.VirtualNicManagerNetConfig{{
		NicType:            vsanNicType,
		MultiSelectAllowed: true,
		CandidateVnic:      []types.HostVirtualNic{{Device: vnic.Device, Key: vsanNicType + "." + vnicKey, Spec: vnic.Spec}},
		SelectedVnic:       []string{vsanNicType + "." + vnicKey},
	}}
}
