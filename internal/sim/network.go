package sim

import (
	"fmt"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

const (
	dvsHostPorts int32 = 6400
	vsanNicType        = "vsan"
)

// Stacks every host carries, as the lab's hosts do (capture
// HostSystem.config.network.netStackInstance): the default stack, the
// vMotion stack and the provisioning stack. The vSAN vmkernel NIC lives on
// the default stack, which is what the vsan-tcpip-stats and vsan-vnic-net
// entities name.
var stacks = []types.HostNetStackInstance{
	{Key: "defaultTcpipStack", Name: "defaultTcpipStack"},
	{Key: "vmotion", Name: "vmotion"},
	{Key: "vSphereProvisioning"},
}

// shapeNetwork replaces vcsim's default host networking (vSwitch0, vmnic0,
// vmk0) with the topology's: a management uplink vmnic0, the vSAN uplinks
// on the datacenter's vDS, and three vmkernel NICs (management, vMotion,
// vSAN) on the vDS portgroups, with the names the /vsanHealth entities
// carry (topology.Host.Pnics, VsanVmknic, Stack).
func shapeNetwork(hs *simulator.HostSystem, dvs *simulator.DistributedVirtualSwitch, pgs map[string]*simulator.DistributedVirtualPortgroup, h *topology.Host) {
	net := hs.Config.Network

	net.Pnic = nil
	var pnicKeys []string
	var pnicSpecs []types.DistributedVirtualSwitchHostMemberPnicSpec
	pnic := func(i int, name string, speed int32) types.PhysicalNic {
		return types.PhysicalNic{
			Key:                    "key-vim.host.PhysicalNic-" + name,
			Device:                 name,
			Pci:                    fmt.Sprintf("0000:%02x:00.0", i+1),
			Driver:                 "nvmxnet3",
			LinkSpeed:              &types.PhysicalNicLinkInfo{SpeedMb: speed, Duplex: true},
			ValidLinkSpecification: []types.PhysicalNicLinkInfo{{SpeedMb: speed, Duplex: true}},
			Spec:                   types.PhysicalNicSpec{Ip: &types.HostIpConfig{}},
			Mac:                    mac(h, i),
			AutoNegotiateSupported: types.NewBool(true),
			WakeOnLanSupported:     true,
		}
	}
	net.Pnic = append(net.Pnic, pnic(0, "vmnic0", 2500))
	for i, name := range h.Pnics {
		key := "key-vim.host.PhysicalNic-" + name
		pnicKeys = append(pnicKeys, key)
		pnicSpecs = append(pnicSpecs, types.DistributedVirtualSwitchHostMemberPnicSpec{
			PnicDevice: name, UplinkPortKey: fmt.Sprint(16 + i),
		})
		net.Pnic = append(net.Pnic, pnic(i+1, name, 10000))
	}
	net.Vswitch = nil
	net.Portgroup = nil

	vnic := func(device, pg, ip, stack string, i int) types.HostVirtualNic {
		return types.HostVirtualNic{
			Device: device,
			Key:    "key-vim.host.VirtualNic-" + device,
			Spec: types.HostVirtualNicSpec{
				Ip:                     &types.HostIpConfig{Dhcp: false, IpAddress: ip, SubnetMask: "255.255.255.0"},
				Mac:                    fmt.Sprintf("00:50:56:%02x:%02x:%02x", 0x60+(h.Serial>>8)&0x1f, h.Serial&0xff, i),
				DistributedVirtualPort: &types.DistributedVirtualSwitchPortConnection{SwitchUuid: dvs.Uuid, PortgroupKey: pgs[pg].Key, PortKey: fmt.Sprint(32*(h.Serial%64) + i)},
				Mtu:                    1500,
				TsoEnabled:             types.NewBool(true),
				NetStackInstanceKey:    stack,
				SystemOwned:            types.NewBool(false),
			},
		}
	}
	mgmt := vnic("vmk0", "pg-mgmt", h.MgmtIP, "defaultTcpipStack", 0)
	vmotion := vnic("vmk1", "pg-vmotion", h.VmotionIP, "vmotion", 1)
	vsan := vnic(h.VsanVmknic, "pg-vsan", h.VsanIP, h.Stack, 2)
	vsan.Spec.Mtu = 9000
	net.Vnic = []types.HostVirtualNic{mgmt, vmotion, vsan}

	net.NetStackInstance = stacks
	net.ProxySwitch = []types.HostProxySwitch{{
		DvsUuid:           dvs.Uuid,
		DvsName:           dvs.Name,
		Key:               "DvsPortset-0",
		NumPorts:          dvsHostPorts,
		ConfigNumPorts:    512,
		NumPortsAvailable: dvsHostPorts - 3 - int32(len(h.Pnics)),
		UplinkPort:        uplinkPorts(h.Pnics),
		Mtu:               9000,
		Pnic:              pnicKeys,
		Spec: types.HostProxySwitchSpec{
			Backing: &types.DistributedVirtualSwitchHostMemberPnicBacking{PnicSpec: pnicSpecs},
		},
	}}
	net.DnsConfig = &types.HostDnsConfig{
		Dhcp:         false,
		HostName:     shortName(h.Name),
		DomainName:   domainOf(h.Name),
		Address:      []string{"10.0.0.53", "10.0.0.54"},
		SearchDomain: []string{domainOf(h.Name)},
	}
	net.IpRouteConfig = &types.HostIpRouteConfig{DefaultGateway: gateway(h.MgmtIP)}
	net.IpV6Enabled = types.NewBool(false)
	net.AtBootIpV6Enabled = types.NewBool(false)

	// vmkernel NIC roles, as HostVirtualNicManager reports them
	role := func(nicType string, selected types.HostVirtualNic) types.VirtualNicManagerNetConfig {
		cfg := types.VirtualNicManagerNetConfig{NicType: nicType, MultiSelectAllowed: true}
		for _, v := range net.Vnic {
			cfg.CandidateVnic = append(cfg.CandidateVnic, types.HostVirtualNic{Device: v.Device, Key: nicType + "." + v.Key, Spec: v.Spec})
		}
		cfg.SelectedVnic = []string{nicType + "." + selected.Key}
		return cfg
	}
	hs.Config.VirtualNicManagerInfo.NetConfig = []types.VirtualNicManagerNetConfig{
		role("management", mgmt),
		role("vmotion", vmotion),
		role(vsanNicType, vsan),
	}
}

func uplinkPorts(pnics []string) []types.KeyValue {
	var out []types.KeyValue
	for i := range pnics {
		out = append(out, types.KeyValue{Key: fmt.Sprint(16 + i), Value: fmt.Sprintf("uplink%d", i+1)})
	}
	return out
}

func mac(h *topology.Host, i int) string {
	return fmt.Sprintf("00:50:56:%02x:%02x:%02x", 0x80+(h.Serial>>8)&0x1f, h.Serial&0xff, i)
}

func shortName(fqdn string) string {
	for i := 0; i < len(fqdn); i++ {
		if fqdn[i] == '.' {
			return fqdn[:i]
		}
	}
	return fqdn
}

func domainOf(fqdn string) string {
	for i := 0; i < len(fqdn); i++ {
		if fqdn[i] == '.' {
			return fqdn[i+1:]
		}
	}
	return ""
}

func gateway(ip string) string {
	var a, b, c, d int
	fmt.Sscanf(ip, "%d.%d.%d.%d", &a, &b, &c, &d)
	return fmt.Sprintf("%d.%d.%d.254", a, b, c)
}
