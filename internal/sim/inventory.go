package sim

import (
	"fmt"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// The /sdk inventory realism of Phase 2: the properties the management pack
// and the native vCenter adapter read most, shaped after the sanitized
// management-vCenter capture (fixtures/live/9.1.1/inventory): cluster
// configurationEx (HA, DRS, vSAN) and summary, host config (vSAN, file
// systems, hardware, summary, runtime) and datastore info and summary.
// Every value here is consistent with the topology; nothing is a count
// that could disagree with the entities the Performance Service serves.

// shapeClusterConfig fills configurationEx the way the lab cluster reports
// it: HA on with admission control, DRS fully automated, proactive DRS on,
// DPM off.
func shapeClusterConfig(cfg *types.ClusterConfigInfoEx, cl *topology.Cluster) {
	cfg.DasConfig = types.ClusterDasConfigInfo{
		Enabled:                 types.NewBool(true),
		VmMonitoring:            string(types.ClusterDasConfigInfoVmMonitoringStateVmMonitoringDisabled),
		HostMonitoring:          string(types.ClusterDasConfigInfoServiceStateEnabled),
		VmComponentProtecting:   string(types.ClusterDasConfigInfoServiceStateEnabled),
		FailoverLevel:           1,
		AdmissionControlEnabled: types.NewBool(true),
		AdmissionControlPolicy: &types.ClusterFailoverResourcesAdmissionControlPolicy{
			CpuFailoverResourcesPercent:    20,
			MemoryFailoverResourcesPercent: 20,
			FailoverLevel:                  1,
			AutoComputePercentages:         types.NewBool(true),
		},
		DefaultVmSettings: &types.ClusterDasVmSettings{
			RestartPriority:        string(types.ClusterDasVmSettingsRestartPriorityMedium),
			RestartPriorityTimeout: 600,
			IsolationResponse:      string(types.ClusterDasVmSettingsIsolationResponsePowerOff),
			VmToolsMonitoringSettings: &types.ClusterVmToolsMonitoringSettings{
				Enabled: types.NewBool(true), VmMonitoring: string(types.ClusterDasConfigInfoVmMonitoringStateVmMonitoringDisabled),
				ClusterSettings: types.NewBool(true), FailureInterval: 30, MinUpTime: 120, MaxFailures: 3, MaxFailureWindow: -1,
			},
			VmComponentProtectionSettings: &types.ClusterVmComponentProtectionSettings{
				VmStorageProtectionForAPD: "warning", EnableAPDTimeoutForHosts: types.NewBool(false),
				VmTerminateDelayForAPDSec: 180, VmReactionOnAPDCleared: "none", VmStorageProtectionForPDL: "warning",
			},
		},
		HeartbeatDatastore:         nil,
		HBDatastoreCandidatePolicy: string(types.ClusterDasConfigInfoHBDatastoreCandidateAllFeasibleDsWithUserPreference),
	}
	cfg.DrsConfig = types.ClusterDrsConfigInfo{
		Enabled:                   types.NewBool(true),
		EnableVmBehaviorOverrides: types.NewBool(false),
		DefaultVmBehavior:         types.DrsBehaviorFullyAutomated,
		VmotionRate:               2,
		ScaleDescendantsShares:    string(types.ResourceConfigSpecScaleSharesBehaviorDisabled),
		Option:                    []types.BaseOptionValue{&types.OptionValue{Key: "TryBalanceVmsPerHost", Value: "1"}},
	}
	cfg.ProactiveDrsConfig = &types.ClusterProactiveDrsConfigInfo{Enabled: types.NewBool(true)}
	cfg.DpmConfigInfo = &types.ClusterDpmConfigInfo{Enabled: types.NewBool(false), DefaultDpmBehavior: types.DpmBehaviorAutomated, HostPowerActionRate: 3}
	cfg.VmSwapPlacement = string(types.VirtualMachineConfigInfoSwapPlacementTypeVmDirectory)
	cfg.SpbmEnabled = types.NewBool(true)
	cfg.CryptoConfig = &types.ClusterCryptoConfigInfo{CryptoMode: string(types.ClusterCryptoConfigInfoCryptoModeOnDemand)}
	cfg.InfraUpdateHaConfig = &types.ClusterInfraUpdateHaConfigInfo{
		Enabled: types.NewBool(false), Behavior: string(types.ClusterInfraUpdateHaConfigInfoBehaviorTypeManual),
		ModerateRemediation: string(types.ClusterInfraUpdateHaConfigInfoRemediationTypeQuarantineMode),
		SevereRemediation:   string(types.ClusterInfraUpdateHaConfigInfoRemediationTypeQuarantineMode),
	}
	cfg.Orchestration = &types.ClusterOrchestrationInfo{DefaultVmReadiness: &types.ClusterVmReadiness{ReadyCondition: string(types.ClusterVmReadinessReadyConditionNone), PostReadyDelay: 0}}
	cfg.SystemVMsConfig = &types.ClusterSystemVMsConfigInfo{DeploymentMode: "ABSENT"}
}

// vsanHostConfig is one entry of configurationEx.vsanHostConfig and the
// host's own config.vsanHostConfig: the join the native adapter and the
// pack read for the node uuid.
func vsanHostConfig(ref types.ManagedObjectReference, cl *topology.Cluster, h *topology.Host) types.VsanHostConfigInfo {
	return types.VsanHostConfigInfo{
		Enabled:     types.NewBool(true),
		HostSystem:  &ref,
		ClusterInfo: &types.VsanHostConfigInfoClusterInfo{Uuid: cl.VsanUUID, NodeUuid: h.NodeUUID},
		StorageInfo: &types.VsanHostConfigInfoStorageInfo{AutoClaimStorage: types.NewBool(false)},
		NetworkInfo: &types.VsanHostConfigInfoNetworkInfo{Port: []types.VsanHostConfigInfoNetworkInfoPortConfig{{
			Device:   h.VsanVmknic,
			IpConfig: &types.VsanHostIpConfig{UpstreamIpAddress: "224.1.2.3", DownstreamIpAddress: "224.2.3.4"},
		}}},
		FaultDomainInfo: &types.VsanHostFaultDomainInfo{Name: ""},
		VsanEsaEnabled:  types.NewBool(cl.Spec.Architecture != "osa"),
	}
}

// Host hardware the lab reports per node (capture summary.hardware /
// hardware.cpuInfo), with the placeholder model strings of the masked
// fixtures. Memory is 256 GiB per host.
const (
	hostMemoryBytes = int64(256) << 30
	cpuHz           = int64(2495312328)
	cpuMhz          = int32(2495)
)

// shapeHost fills the host identity, product, hardware and summary.
func shapeHost(hs *simulator.HostSystem, cl *topology.Cluster, h *topology.Host, cat *catalog.Catalog) {
	cores := int16(h.Threads / 2)
	if cores < 1 {
		cores = 1
	}
	hs.Summary.Hardware.Vendor = "SIM Hardware Vendor"
	hs.Summary.Hardware.Model = "SIM-HOST"
	hs.Summary.Hardware.CpuModel = "SIM CPU 16-Core Processor"
	hs.Summary.Hardware.NumCpuPkgs = 1
	hs.Summary.Hardware.NumCpuCores = cores
	hs.Summary.Hardware.NumCpuThreads = int16(h.Threads)
	hs.Summary.Hardware.CpuMhz = cpuMhz
	hs.Summary.Hardware.MemorySize = hostMemoryBytes
	hs.Summary.Hardware.NumNics = int32(1 + len(h.Pnics))
	hs.Summary.Hardware.NumHBAs = int32(len(h.Devices))
	hs.Summary.ManagementServerIp = "10.0.0.10"
	hs.Summary.Config.Port = 443
	hs.Summary.Config.VmotionEnabled = true
	hs.Summary.Config.FaultToleranceEnabled = true
	hs.Summary.Config.SslThumbprint = ""
	hs.Hardware.SystemInfo.Vendor = "SIM Hardware Vendor"
	hs.Hardware.SystemInfo.Model = "SIM-HOST"
	hs.Hardware.SystemInfo.SerialNumber = "SIM" + h.HardwareUUID[:8]
	hs.Hardware.CpuInfo = types.HostCpuInfo{NumCpuPackages: 1, NumCpuCores: cores, NumCpuThreads: int16(h.Threads), Hz: cpuHz}
	threads := make([]int16, h.Threads)
	for i := range threads {
		threads[i] = int16(i)
	}
	hs.Hardware.CpuPkg = []types.HostCpuPackage{{Index: 0, Vendor: "sim", Hz: cpuHz, BusHz: cpuHz / 100, Description: "SIM CPU 16-Core Processor", ThreadId: threads}}
	hs.Hardware.MemorySize = hostMemoryBytes
	hs.Hardware.BiosInfo = &types.HostBIOSInfo{BiosVersion: "1.03", Vendor: "SIM BIOS Vendor", MajorRelease: 5, MinorRelease: 36, FirmwareType: "UEFI"}
	hs.Hardware.NumaInfo = &types.HostNumaInfo{Type: "NUMA", NumNodes: 1}
	hs.Hardware.SmcPresent = false
	hs.Config.LockdownMode = types.HostLockdownModeLockdownDisabled
	hs.Config.DateTimeInfo = &types.HostDateTimeInfo{
		TimeZone:            types.HostDateTimeSystemTimeZone{Key: "UTC", Name: "UTC", Description: "UTC", GmtOffset: 0},
		SystemClockProtocol: "ntp",
		NtpConfig:           &types.HostNtpConfig{Server: []string{"ntp1." + topology.Domain, "ntp2." + topology.Domain}},
	}
	hs.Config.HyperThread = &types.HostHyperThreadScheduleInfo{Available: true, Active: true, Config: true}
	hs.Config.SystemResources = &types.HostSystemResourceInfo{Key: "host"}
	hs.Config.AdminDisabled = types.NewBool(false)
	hs.Config.WakeOnLanCapable = types.NewBool(true)
	hs.Config.DatastorePrincipal = "vpxuser"
}

// shapeHostProduct sets the ESXi product once every host of the cluster
// exists: vcsim's environment browser parses the version of every cluster
// host on each AddHost_Task, and govmomi v0.56.2 does not know 9.x.
func shapeHostProduct(hs *simulator.HostSystem, cat *catalog.Catalog) {
	esx := types.AboutInfo{
		Name:                  "VMware ESXi",
		FullName:              cat.About.EsxiFullName,
		Vendor:                "VMware, Inc.",
		Version:               cat.About.EsxiVersion,
		Build:                 cat.About.EsxiBuild,
		LocaleVersion:         "INTL",
		LocaleBuild:           "000",
		OsType:                "vmnix-x86",
		ProductLineId:         "embeddedEsx",
		ApiType:               "HostAgent",
		ApiVersion:            cat.About.ApiVersion,
		LicenseProductName:    "VMware ESX Server",
		LicenseProductVersion: "9.1",
	}
	hs.Config.Product = esx
	hs.Summary.Config.Product = &hs.Config.Product
}

// shapeHostRuntime fills runtime and quick stats after the cluster exists:
// connected and powered on, HA slave or master, vSAN membership of every
// node in the cluster.
func shapeHostRuntime(hs *simulator.HostSystem, cl *topology.Cluster, h *topology.Host, now func() time.Time) {
	boot := now().Add(-time.Duration(7*24+h.Serial%24) * time.Hour).UTC()
	hs.Runtime.ConnectionState = types.HostSystemConnectionStateConnected
	hs.Runtime.PowerState = types.HostSystemPowerStatePoweredOn
	hs.Runtime.StandbyMode = string(types.HostStandbyModeNone)
	hs.Runtime.InMaintenanceMode = false
	hs.Runtime.InQuarantineMode = types.NewBool(false)
	hs.Runtime.BootTime = &boot
	state := string(types.ClusterDasFdmAvailabilityStateConnectedToMaster)
	if h.Index == 0 {
		state = string(types.ClusterDasFdmAvailabilityStateMaster)
	}
	hs.Runtime.DasHostState = &types.ClusterDasFdmHostState{State: state}
	hs.Runtime.HealthSystemRuntime = &types.HealthSystemRuntime{SystemHealthInfo: &types.HostSystemHealthInfo{}}
	var members []types.VsanHostMembershipInfo
	for _, m := range cl.Hosts {
		members = append(members, types.VsanHostMembershipInfo{NodeUuid: m.NodeUUID, Hostname: m.Name})
	}
	hs.Runtime.VsanRuntimeInfo = &types.VsanHostRuntimeInfo{MembershipList: members}
	hs.Runtime.HostMaxVirtualDiskCapacity = 68169720922112
	hs.Summary.Runtime = &hs.Runtime
	hs.Summary.QuickStats.Uptime = int32(now().Sub(boot) / time.Second)
	hs.Summary.QuickStats.OverallCpuUsage = int32(len(cl.VMs)/len(cl.Hosts))*900 + 1200
	hs.Summary.QuickStats.OverallMemoryUsage = int32(len(cl.VMs)/len(cl.Hosts))*4096 + 8192
	hs.Summary.OverallStatus = types.ManagedEntityStatusGreen
	hs.Summary.RebootRequired = false
}

// shapeClusterSummary derives the cluster summary from its hosts and VMs.
func shapeClusterSummary(obj *simulator.ClusterComputeResource, cl *topology.Cluster, refs *ClusterRefs) {
	sum := obj.Summary.(*types.ClusterComputeResourceSummary)
	var cores, threads int16
	var cpu int32
	for _, h := range cl.Hosts {
		cores += int16(h.Threads / 2)
		threads += int16(h.Threads)
		cpu += int32(h.Threads/2) * cpuMhz
	}
	n := int32(len(cl.Hosts))
	sum.NumHosts = n
	sum.NumEffectiveHosts = n
	sum.NumCpuCores = cores
	sum.NumCpuThreads = threads
	sum.TotalCpu = cpu
	sum.EffectiveCpu = cpu * 85 / 100
	sum.TotalMemory = hostMemoryBytes * int64(n)
	sum.EffectiveMemory = (hostMemoryBytes * int64(n)) >> 20 * 77 / 100
	sum.OverallStatus = types.ManagedEntityStatusGreen
	sum.CurrentFailoverLevel = 1
	sum.AdmissionControlInfo = &types.ClusterFailoverResourcesAdmissionControlInfo{CurrentCpuFailoverResourcesPercent: 95, CurrentMemoryFailoverResourcesPercent: 90}
	sum.DrsScore = 97
	sum.NumVmsPerDrsScoreBucket = []int32{0, 0, 0, 0, int32(len(cl.VMs))}
	sum.UsageSummary = &types.ClusterUsageSummary{
		TotalCpuCapacityMhz: sum.EffectiveCpu, TotalMemCapacityMB: int32(sum.EffectiveMemory),
		CpuDemandMhz: int32(len(cl.VMs)) * 900, MemDemandMB: int32(len(cl.VMs)) * 4096,
		TotalVmCount: int32(len(cl.VMs)), PoweredOffVmCount: 0,
	}
	sum.ClusterMaintenanceModeStatus = "notInMaintenanceMode"
	sum.VcsHealthStatus = "healthy"
	sum.DasData = &types.ClusterDasDataSummary{}
}

// Datastore shapes (capture Datastore.json): the vSAN datastore's info and
// summary and the local VMFS 6.82 datastore on an unclaimed NVMe device.
var (
	vsanDatastoreCapability = types.DatastoreCapability{
		DirectoryHierarchySupported:      true,
		RawDiskMappingsSupported:         true,
		PerFileThinProvisioningSupported: true,
		StorageIORMSupported:             false,
		NativeSnapshotSupported:          true,
		TopLevelDirectoryCreateSupported: types.NewBool(false),
		SeSparseSupported:                types.NewBool(false),
		VmfsSparseSupported:              types.NewBool(false),
		VsanSparseSupported:              types.NewBool(true),
		VmdkExpandSupported:              types.NewBool(true),
	}
	vmfsDatastoreCapability = types.DatastoreCapability{
		DirectoryHierarchySupported:      true,
		RawDiskMappingsSupported:         true,
		PerFileThinProvisioningSupported: true,
		StorageIORMSupported:             true,
		NativeSnapshotSupported:          false,
		TopLevelDirectoryCreateSupported: types.NewBool(true),
		SeSparseSupported:                types.NewBool(true),
		VmfsSparseSupported:              types.NewBool(false),
		VsanSparseSupported:              types.NewBool(false),
		VmdkExpandSupported:              types.NewBool(true),
		ClusteredVmdkSupported:           types.NewBool(true),
	}
	iorm = &types.StorageIORMInfo{Enabled: false, CongestionThresholdMode: "automatic", CongestionThreshold: 30, PercentOfPeakThroughput: 90, StatsCollectionEnabled: false, ReservationEnabled: true, StatsAggregationDisabled: types.NewBool(true)}
)

func vsanDatastoreInfo(cl *topology.Cluster) *types.VsanDatastoreInfo {
	return &types.VsanDatastoreInfo{
		DatastoreInfo: types.DatastoreInfo{
			MaxFileSize:            140737488355328,
			MaxVirtualDiskCapacity: 68169720922112,
			MaxMemoryFileSize:      9223372036854775807,
			SupportedVDiskFormats:  []string{"native_512", "native_4k"},
			LogicalSectorSize:      512,
			PhysicalSectorSize:     0,
		},
		// govmomi v0.56.2 predates the 9.x containerId and aliasOf members
		MembershipUuid: cl.VsanUUID,
		AccessGenNo:    int32(cl.Serial*97 + 1000),
	}
}

func vmfsDatastoreInfo(name string, d *topology.Device) *types.VmfsDatastoreInfo {
	return &types.VmfsDatastoreInfo{
		DatastoreInfo: types.DatastoreInfo{
			MaxFileSize:            70368744177664,
			MaxVirtualDiskCapacity: 68169720922112,
			MaxMemoryFileSize:      70368744177664,
			SupportedVDiskFormats:  []string{"native_512"},
			LogicalSectorSize:      512,
			PhysicalSectorSize:     4096,
		},
		MaxPhysicalRDMFileSize: 70368744177664,
		MaxVirtualRDMFileSize:  68169720922112,
		Vmfs:                   vmfsVolume(name, d),
	}
}

func vmfsVolume(name string, d *topology.Device) *types.HostVmfsVolume {
	capacity := d.CapacityBytes - 128<<30 // the OSDATA partition takes 128 GiB
	return &types.HostVmfsVolume{
		HostFileSystemVolume: types.HostFileSystemVolume{Type: "VMFS", Name: name, Capacity: capacity},
		BlockSizeMb:          1,
		BlockSize:            1024,
		UnmapGranularity:     1024,
		UnmapPriority:        "low",
		MaxBlocks:            int32(capacity >> 20),
		MajorVersion:         6,
		Version:              "6.82",
		Uuid:                 d.VMFSUUID,
		Extent:               []types.HostScsiDiskPartition{{DiskName: d.CanonicalName, Partition: 8}},
		VmfsUpgradable:       false,
		Ssd:                  types.NewBool(true),
		Local:                types.NewBool(true),
	}
}

// vsanMount is the host's fileSystemVolume entry for the vSAN datastore.
func vsanMount(cl *topology.Cluster) types.HostFileSystemMountInfo {
	return types.HostFileSystemMountInfo{
		MountInfo:       types.HostMountInfo{Path: "/vmfs/volumes/vsan:" + cl.ContainerID, AccessMode: "readWrite", Mounted: types.NewBool(true), Accessible: types.NewBool(true)},
		Volume:          &types.HostFileSystemVolume{Type: "vsan", Name: cl.Datastore, Capacity: vsanCapacity(cl)},
		VStorageSupport: "vStorageUnsupported",
	}
}

func vmfsMount(name string, d *topology.Device) types.HostFileSystemMountInfo {
	return types.HostFileSystemMountInfo{
		MountInfo:       types.HostMountInfo{Path: "/vmfs/volumes/" + d.VMFSUUID, AccessMode: "readWrite", Mounted: types.NewBool(true), Accessible: types.NewBool(true)},
		Volume:          vmfsVolume(name, d),
		VStorageSupport: "vStorageUnsupported",
	}
}

// vsanCapacity is the raw capacity of the cluster's claimed devices.
func vsanCapacity(cl *topology.Cluster) int64 {
	return topology.VsanCapacity(cl)
}

// shapeDatastoreSummary sets capacity from the claimed devices and usage
// from the VMs placed on the datastore.
func shapeDatastoreSummary(ds *simulator.Datastore, cl *topology.Cluster, refs *ClusterRefs) {
	capacity := vsanCapacity(cl)
	committed := topology.VsanUsed(cl)
	ds.Summary.Capacity = capacity
	ds.Summary.FreeSpace = capacity - committed
	ds.Summary.Uncommitted = committed
	info := ds.Info.(*types.VsanDatastoreInfo)
	info.FreeSpace = ds.Summary.FreeSpace
	ts := time.Now().UTC()
	info.Timestamp = &ts
	ds.IormConfiguration = iorm
	ds.Summary.MultipleHostAccess = types.NewBool(true)
}

// shapeVMFSDatastoreSummary gives a local VMFS datastore the URL and
// capacity a host reports for it; Info.Url stays the backing directory for
// vcsim's file operations.
func shapeVMFSDatastoreSummary(ds *simulator.Datastore, d *topology.Device) {
	vol := ds.Info.(*types.VmfsDatastoreInfo).Vmfs
	ds.Summary.Url = "ds:///vmfs/volumes/" + d.VMFSUUID + "/"
	ds.Summary.Capacity = vol.Capacity
	ds.Summary.FreeSpace = vol.Capacity * 9 / 10
	ds.Summary.Uncommitted = 0
	ds.Info.GetDatastoreInfo().FreeSpace = ds.Summary.FreeSpace
	ts := time.Now().UTC()
	ds.Info.GetDatastoreInfo().Timestamp = &ts
}

// shapeVM gives a created VM the guest, runtime and summary a powered-on
// VM with tools reports.
func shapeVM(obj *simulator.VirtualMachine, cl *topology.Cluster, vm *topology.VM, now func() time.Time) {
	boot := now().Add(-time.Duration(24+vm.Serial%240) * time.Hour).UTC()
	obj.Runtime.PowerState = types.VirtualMachinePowerStatePoweredOn
	obj.Runtime.ConnectionState = types.VirtualMachineConnectionStateConnected
	obj.Runtime.BootTime = &boot
	obj.Runtime.MaxCpuUsage = 2 * cpuMhz
	obj.Runtime.MaxMemoryUsage = 4096
	obj.Summary.Runtime = obj.Runtime
	obj.Guest.GuestState = "running"
	obj.Guest.GuestId = obj.Config.GuestId
	obj.Guest.GuestFullName = obj.Config.GuestFullName
	obj.Guest.ToolsStatus = types.VirtualMachineToolsStatusToolsOk
	obj.Guest.ToolsRunningStatus = string(types.VirtualMachineToolsRunningStatusGuestToolsRunning)
	obj.Guest.ToolsVersionStatus2 = string(types.VirtualMachineToolsVersionStatusGuestToolsCurrent)
	obj.Guest.HostName = vm.Name + "." + topology.Domain
	obj.Guest.IpAddress = fmt.Sprintf("10.20.%d.%d", (vm.Serial/254)%256, vm.Serial%254+1)
	obj.Guest.GuestOperationsReady = types.NewBool(true)
	obj.Summary.Guest.GuestId = obj.Guest.GuestId
	obj.Summary.Guest.GuestFullName = obj.Guest.GuestFullName
	obj.Summary.Guest.ToolsStatus = obj.Guest.ToolsStatus
	obj.Summary.Guest.ToolsRunningStatus = obj.Guest.ToolsRunningStatus
	obj.Summary.Guest.HostName = obj.Guest.HostName
	obj.Summary.Guest.IpAddress = obj.Guest.IpAddress
	obj.Summary.QuickStats.OverallCpuUsage = 400 + int32(vm.Serial%500)
	obj.Summary.QuickStats.OverallCpuDemand = 420 + int32(vm.Serial%500)
	obj.Summary.QuickStats.GuestMemoryUsage = 1024 + int32(vm.Serial%2048)
	obj.Summary.QuickStats.HostMemoryUsage = 3000 + int32(vm.Serial%1000)
	obj.Summary.QuickStats.GuestHeartbeatStatus = types.ManagedEntityStatusGreen
	obj.Summary.QuickStats.UptimeSeconds = int32(now().Sub(boot) / time.Second)
	obj.Summary.OverallStatus = types.ManagedEntityStatusGreen
	obj.GuestHeartbeatStatus = types.ManagedEntityStatusGreen
	var committed int64
	for _, d := range vm.Disks {
		committed += d.CapacityKB << 10 / 2
	}
	obj.Summary.Storage.Committed = committed
	obj.Summary.Storage.Uncommitted = committed
	obj.Summary.Storage.Unshared = committed
}
