package vsanhealth

import (
	_ "embed"
	"encoding/json"

	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/simulator"
	vim "github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vsan/methods"
	vsan "github.com/vmware/govmomi/vsan/types"
)

// The fixture retains only public check identifiers and labels from the OSA walk.
//
//go:embed fixtures/osa-health-groups.json
var osaHealthGroups []byte

func (s *ClusterHealthSystem) osaHealthDetail(ctx *simulator.Context, sum *vsan.VsanClusterHealthSummary, cl *topology.Cluster, ref *vim.ManagedObjectReference) {
	sum.Cluster = ref
	sum.OverallHealth = "yellow"
	sum.OverallHealthDescription = "Hardware compatibility"
	healthy := vsan.VsanObjectHealth{Health: "healthy", ObjUuids: []string{cl.StatsUUID}}
	for _, vm := range cl.VMs {
		healthy.ObjUuids = append(healthy.ObjUuids, vm.NamespaceUUID)
	}
	healthy.NumObjects = int32(len(healthy.ObjUuids))
	sum.ObjectHealth = &vsan.VsanObjectOverallHealth{ObjectVersionCompliance: vim.NewBool(true), ObjectHealthDetail: []vsan.VsanObjectHealth{healthy}}
	for _, state := range []string{"inaccessible", "reducedavailabilitywithnorebuild", "reducedavailabilitywithnorebuilddelaytimer", "reducedavailabilitywithactiverebuild", "datamove", "nonavailabilityrelatedreconfig", "nonavailabilityrelatedincompliance"} {
		sum.ObjectHealth.ObjectHealthDetail = append(sum.ObjectHealth.ObjectHealthDetail, vsan.VsanObjectHealth{Health: state})
	}
	sum.NetworkHealth = &vsan.VsanClusterNetworkHealthResult{IssueFound: vim.NewBool(false), VsanVmknicPresent: vim.NewBool(true), MatchingMulticastConfig: vim.NewBool(true), PingTestSuccess: vim.NewBool(true), LargePingTestSuccess: vim.NewBool(true), HostLatencyCheckSuccess: vim.NewBool(true), PotentialMulticastIssue: vim.NewBool(false), ClusterInUnicastMode: vim.NewBool(true)}
	sum.LimitHealth = &vsan.VsanClusterLimitHealthResult{ComponentLimitHealth: "green", DiskFreeSpaceHealth: "green", RcFreeReservationHealth: "green"}
	sum.DiskBalance = &vsan.VsanClusterBalanceSummary{VarianceThreshold: 30}
	sum.ClomdLiveness = &vsan.VsanClusterClomdLivenessResult{}
	sum.EncryptionHealth = &vsan.VsanClusterEncryptionHealthSummary{OverallHealth: "green", ConfigHealth: "green", KmsHealth: "green", AesniHealth: "green", VcKmsResult: &vsan.VsanVcKmipServersHealth{Health: "green"}}
	perf := &PerfManager{env: s.env}
	primary := cl.Spec.StatsPrimary == nil || *cl.Spec.StatsPrimary
	sum.PerfsvcHealth = &vsan.VsanPerfsvcHealthResult{}
	if primary {
		sum.PerfsvcHealth.StatsObjectConsistent = vim.NewBool(true)
		sum.PerfsvcHealth.StatsObjectPolicyConsistent = vim.NewBool(true)
		sum.PerfsvcHealth.DatastoreCompatible = vim.NewBool(true)
		sum.PerfsvcHealth.EnoughFreeSpace = vim.NewBool(true)
		sum.PerfsvcHealth.RemediateAction = "no_action"
		sum.PerfsvcHealth.VerboseModeStatus = vim.NewBool(false)
	}
	if b, ok := perf.VsanPerfQueryStatsObjectInformation(ctx, &vsan.VsanPerfQueryStatsObjectInformation{Cluster: ref}).(*methods.VsanPerfQueryStatsObjectInformationBody); ok && b.Res != nil {
		sum.PerfsvcHealth.StatsObjectInfo = &b.Res.Returnval
	}
	if b, ok := perf.VsanPerfQueryNodeInformation(ctx, &vsan.VsanPerfQueryNodeInformation{Cluster: ref}).(*methods.VsanPerfQueryNodeInformationBody); ok && b.Res != nil {
		sum.PerfsvcHealth.HostResults = b.Res.Returnval
	}
	sum.VsanConfig = &vsan.VsanConfigCheckResult{VsanEnabled: true}
	sum.DitEncryptionHealth = &vsan.VsanClusterDitEncryptionHealthSummary{OverallHealth: "green", Enabled: vim.NewBool(false)}
	for _, n := range []string{"LSOM.lsomSlowDeviceUnmount", "LSOM.VSANDeviceMonitoring"} {
		sum.AdvCfgSync = append(sum.AdvCfgSync, vsan.VsanClusterAdvCfgSyncResult{Name: n, InSync: true})
	}
	partition := vsan.VsanClusterNetworkPartitionInfo{PartitionUnknown: vim.NewBool(false)}
	for _, h := range cl.Hosts {
		partition.Hosts = append(partition.Hosts, h.Name)
		sum.EncryptionHealth.HostResults = append(sum.EncryptionHealth.HostResults, vsan.VsanEncryptionHealthSummary{Hostname: h.Name, EncryptionInfo: &vsan.VsanHostEncryptionInfo{Enabled: vim.NewBool(false)}, OverallKmsHealth: "green"})
		ref := s.env.hostRefs[h]
		netHost := vsan.VsanNetworkHealthResult{Host: &ref, Hostname: h.Name, VsanVmknicPresent: vim.NewBool(true), IssueFound: vim.NewBool(false), InUnicast: vim.NewBool(true), IpSubnets: []string{h.VsanIP + "/24"}}
		for _, peer := range cl.Hosts {
			if peer == h {
				continue
			}
			netHost.PeerHealth = append(netHost.PeerHealth, vsan.VsanNetworkPeerHealthResult{Peer: peer.VsanIP, PeerHostname: peer.Name, PeerVmknicName: peer.VsanVmknic, SourceVmknicName: h.VsanVmknic, SmallPingTestSuccessPct: 100, LargePingTestSuccessPct: 100, MaxLatencyUs: 100, OnSameIpSubnet: vim.NewBool(true)})
			netHost.VMotionHealth = append(netHost.VMotionHealth, vsan.VsanNetworkPeerHealthResult{Peer: peer.VmotionIP, PeerHostname: peer.Name, PeerVmknicName: "vmk1", SourceVmknicName: "vmk1", SmallPingTestSuccessPct: 100, LargePingTestSuccessPct: 100, MaxLatencyUs: 100, OnSameIpSubnet: vim.NewBool(true)})
		}
		sum.NetworkHealth.HostResults = append(sum.NetworkHealth.HostResults, netHost)
		sum.ClomdLiveness.ClomdLivenessResult = append(sum.ClomdLiveness.ClomdLivenessResult, vsan.VsanHostClomdLivenessResult{Hostname: h.Name, ClomdStat: "alive"})
		var capacity int64
		var used int64
		if len(cl.Hosts) > 0 {
			used = topology.VsanUsed(cl) / int64(len(cl.Hosts))
		}
		diskHealth := vsan.VsanPhysicalDiskHealthSummary{Hostname: h.Name, OverallHealth: "green"}
		for _, d := range h.Devices {
			if !d.Claimed {
				continue
			}
			capacity += d.CapacityBytes
			disk := ScsiDisk(d)
			diskHealth.Disks = append(diskHealth.Disks, vsan.VsanPhysicalDiskHealth{Name: d.CanonicalName, Uuid: d.VsanUUID, InCmmds: true, InVsi: true, FormatVersion: disk.VsanDiskInfo.FormatVersion, CongestionArea: "none", CongestionHealth: "green", MetadataHealth: "green", OperationalHealthDescription: "OK", OperationalHealth: "green", DedupUsageHealth: "green", CapacityHealth: "green", SummaryHealth: "green", Capacity: d.CapacityBytes, ScsiDisk: &disk, EncryptionEnabled: vim.NewBool(false)})
			sum.DiskBalance.Disks = append(sum.DiskBalance.Disks, vsan.VsanClusterBalancePerDiskInfo{Uuid: d.VsanUUID})
		}
		sum.PhysicalDisksHealth = append(sum.PhysicalDisksHealth, diskHealth)
		freePct := int32(0)
		if capacity > 0 {
			freePct = int32(100 * (capacity - used) / capacity)
			if freePct < 0 {
				freePct = 0
			}
		}
		sum.LimitHealth.HostResults = append(sum.LimitHealth.HostResults, vsan.VsanLimitHealthResult{Hostname: h.Name, MaxComponents: 9000, FreeComponents: 9000, ComponentLimitHealth: "green", LowestFreeDiskSpacePct: freePct, UsedDiskSpaceB: used, TotalDiskSpaceB: capacity, DiskFreeSpaceHealth: "green", RcFreeReservationHealth: "green"})
	}
	sum.NetworkHealth.Partitions = []vsan.VsanClusterNetworkPartitionInfo{partition}
	var groups []capturedHealthGroup
	if json.Unmarshal(osaHealthGroups, &groups) == nil {
		for _, g := range groups {
			group := vsan.VsanClusterHealthGroup{GroupId: g.GroupID, GroupName: g.GroupName, GroupHealth: "green"}
			for _, t := range g.Tests {
				group.GroupTests = append(group.GroupTests, vsan.VsanClusterHealthTest{TestId: t.ID, TestName: t.Name, TestHealth: "green"})
			}
			if g.GroupID == "com.vmware.vsan.health.test.hcl" {
				group.GroupHealth = "yellow"
				for i := range group.GroupTests {
					if group.GroupTests[i].TestId == "com.vmware.vsan.health.test.controllerdiskmode" {
						group.GroupTests[i].TestHealth = "yellow"
					}
				}
			}
			if g.GroupID == "com.vmware.vsan.health.test.cloudhealth" {
				group.GroupHealth = "info"
				for i := range group.GroupTests {
					group.GroupTests[i].TestHealth = "info"
				}
			}
			if g.GroupID == "com.vmware.vsan.health.test.perfsvc" && cl.Spec.StatsPrimary != nil && !*cl.Spec.StatsPrimary {
				group.GroupHealth = "yellow"
				for i := range group.GroupTests {
					if group.GroupTests[i].TestId == "com.vmware.vsan.health.test.masterexist" {
						group.GroupTests[i].TestHealth = "yellow"
					}
				}
			}
			sum.Groups = append(sum.Groups, group)
		}
	}
}
