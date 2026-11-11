package sim

import (
	"fmt"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"
)

func iscsiVolume(l topology.ISCSILUN) *types.HostVmfsVolume {
	return &types.HostVmfsVolume{HostFileSystemVolume: types.HostFileSystemVolume{Type: "VMFS", Name: l.Spec.Name, Capacity: l.Spec.Capacity}, BlockSizeMb: 1, BlockSize: 1024, MajorVersion: 6, Version: "6.82", Uuid: l.UUID, Extent: []types.HostScsiDiskPartition{{DiskName: l.CanonicalName, Partition: 1}}, Local: types.NewBool(false)}
}

func (s *Simulator) createISCSIDatastore(ctx *simulator.Context, dc *topology.Datacenter, lun topology.ISCSILUN) (*simulator.Datastore, error) {
	info := &types.VmfsDatastoreInfo{DatastoreInfo: types.DatastoreInfo{FreeSpace: lun.Spec.FreeSpace}, Vmfs: iscsiVolume(lun)}
	ds, err := s.createDatastore(ctx, s.dcs[dc], lun.Spec.Name, info, "VMFS")
	if err != nil {
		return nil, err
	}
	ds.Summary.Capacity = lun.Spec.Capacity
	ds.Summary.FreeSpace = lun.Spec.FreeSpace
	ds.Summary.MultipleHostAccess = types.NewBool(true)
	ds.Summary.Url = "ds:///vmfs/volumes/" + lun.UUID + "/"
	ds.Capability = vmfsDatastoreCapability
	ds.IormConfiguration = iorm
	return ds, nil
}

func iscsiDevice(name string) string {
	return fmt.Sprintf("vmhba%d", 64+scenario.StorageNumber(name)%1000000)
}

func iscsiTargetNumber(name string) int32 {
	return int32(scenario.StorageNumber(name) & 0x7fffffff)
}

func (s *Simulator) mountISCSI(ctx *simulator.Context, st topology.ISCSIStorage, stores map[string]*simulator.Datastore) {
	var refs *ClusterRefs
	for _, c := range s.Clusters {
		if c.Cluster.Key == st.Spec.Cluster {
			refs = c
			break
		}
	}
	if refs == nil {
		return
	}
	ccr := ctx.Map.Get(refs.Ref).(*simulator.ClusterComputeResource)
	targets := map[string]scenario.ISCSITarget{}
	for _, target := range st.Spec.Targets {
		targets[target.Name] = target
	}
	for _, l := range st.LUNs {
		ds := stores[l.UUID]
		ds.Summary.Accessible = false
		ctx.Map.AddReference(ctx, ccr, &ccr.Datastore, ds.Self)
	}
	for i, href := range refs.Hosts {
		h := refs.Cluster.Hosts[i]
		hs := ctx.Map.Get(href).(*simulator.HostSystem)
		sd := hs.Config.StorageDevice
		if sd.MultipathInfo == nil {
			sd.MultipathInfo = &types.HostMultipathInfo{}
		}
		if hs.Config.MultipathState == nil {
			hs.Config.MultipathState = &types.HostMultipathStateInfo{}
		}
		for _, a := range st.Spec.Adapters {
			dev := iscsiDevice(a.Name)
			hbaKey := "key-vim.host.InternetScsiHba-" + dev
			hba := &types.HostInternetScsiHba{HostHostBusAdapter: types.HostHostBusAdapter{Key: hbaKey, Device: dev, Status: "online", Model: "Simulated iSCSI adapter", Driver: "iscsi_vmk"}, IsSoftwareBased: a.Kind == "software", IScsiName: fmt.Sprintf("iqn.2026-01.test.example:host-%d-%s", h.Serial, a.Name)}
			if hba.IsSoftwareBased {
				sd.SoftwareInternetScsiEnabled = true
			}
			for _, target := range st.Spec.Targets {
				port := target.Port
				if port == 0 {
					port = 3260
				}
				hba.ConfiguredSendTarget = append(hba.ConfiguredSendTarget, types.HostInternetScsiHbaSendTarget{Address: target.Address, Port: port})
				hba.ConfiguredStaticTarget = append(hba.ConfiguredStaticTarget, types.HostInternetScsiHbaStaticTarget{Address: target.Address, Port: port, IScsiName: target.IQN, DiscoveryMethod: "sendTargets"})
			}
			sd.HostBusAdapter = append(sd.HostBusAdapter, hba)
			iface := types.HostScsiTopologyInterface{Key: "key-vim.host.ScsiTopology.Interface-" + dev, Adapter: hbaKey}
			for _, target := range st.Spec.Targets {
				number := iscsiTargetNumber(target.Name)
				t := types.HostScsiTopologyTarget{Key: fmt.Sprintf("key-vim.host.ScsiTopology.Target-%s-%d", dev, number), Target: number, Transport: &types.HostInternetScsiTargetTransport{IScsiName: target.IQN, Address: []string{target.Address}}}
				for _, lun := range st.LUNs {
					if lun.Spec.Target != target.Name {
						continue
					}
					if _, ok := lun.Spec.PathState(a.Name, h.Key); !ok {
						continue
					}
					key := "key-vim.host.ScsiDisk-" + lun.CanonicalName
					t.Lun = append(t.Lun, types.HostScsiTopologyLun{Key: fmt.Sprintf("key-vim.host.ScsiTopology.Lun-%s-%d", dev, lun.Serial), Lun: lun.Spec.Number, ScsiLun: key})
				}
				if len(t.Lun) > 0 {
					iface.Target = append(iface.Target, t)
				}
			}
			sd.ScsiTopology.Adapter = append(sd.ScsiTopology.Adapter, iface)
		}
		for _, lun := range st.LUNs {
			ds := stores[lun.UUID]
			key := "key-vim.host.ScsiDisk-" + lun.CanonicalName
			active := lun.Spec.Reachable(st.Spec.Adapters, h.Key)
			policy := lun.Spec.Policy
			if policy == "" {
				policy = "VMW_PSP_RR"
			}
			mp := types.HostMultipathInfoLogicalUnit{Key: "key-vim.host.MultipathInfo.LogicalUnit-" + lun.CanonicalName, Id: lun.CanonicalName, Lun: key, Policy: &types.HostMultipathInfoLogicalUnitPolicy{Policy: policy}}
			target := targets[lun.Spec.Target]
			for _, a := range st.Spec.Adapters {
				state, ok := lun.Spec.PathState(a.Name, h.Key)
				if !ok {
					continue
				}
				dev := iscsiDevice(a.Name)
				path := fmt.Sprintf("%s:C0:T%d:L%d", dev, iscsiTargetNumber(target.Name), lun.Spec.Number)
				mp.Path = append(mp.Path, types.HostMultipathInfoPath{Key: path, Name: path, PathState: state, State: state, IsWorkingPath: types.NewBool(state == "active"), Adapter: "key-vim.host.InternetScsiHba-" + dev, Lun: key, Transport: &types.HostInternetScsiTargetTransport{IScsiName: target.IQN, Address: []string{target.Address}}})
				hs.Config.MultipathState.Path = append(hs.Config.MultipathState.Path, types.HostMultipathStateInfoPath{Name: path, PathState: state})
			}
			operational := "ok"
			if !active {
				operational = "lostCommunication"
			}
			disk := &types.HostScsiDisk{ScsiLun: types.ScsiLun{HostDevice: types.HostDevice{DeviceName: "/vmfs/devices/disks/" + lun.CanonicalName, DeviceType: "disk"}, Key: key, Uuid: lun.CanonicalName, CanonicalName: lun.CanonicalName, DisplayName: "iSCSI Disk (" + lun.CanonicalName + ")", LunType: "disk", Vendor: "SIMULATR", Model: "iSCSI LUN", OperationalState: []string{operational}}, Capacity: types.HostDiskDimensionsLba{BlockSize: 512, Block: lun.Spec.Capacity / 512}, DevicePath: "/vmfs/devices/disks/" + lun.CanonicalName, LocalDisk: types.NewBool(false)}
			sd.ScsiLun = append(sd.ScsiLun, disk)
			sd.MultipathInfo.Lun = append(sd.MultipathInfo.Lun, mp)
			path := "/vmfs/volumes/" + lun.UUID
			mount(ctx, hs, ds, path)
			ds.Host[len(ds.Host)-1].MountInfo.Accessible = types.NewBool(active)
			if active {
				ds.Summary.Accessible = true
			}
			hs.Config.FileSystemVolume.MountInfo = append(hs.Config.FileSystemVolume.MountInfo, types.HostFileSystemMountInfo{MountInfo: types.HostMountInfo{Path: path, AccessMode: "readWrite", Mounted: types.NewBool(true), Accessible: types.NewBool(active)}, Volume: iscsiVolume(lun), VStorageSupport: "vStorageUnsupported"})
		}
		populatePlugStore(sd)
		if ss, ok := ctx.Map.Get(*hs.ConfigManager.StorageSystem).(*simulator.HostStorageSystem); ok {
			ss.StorageDeviceInfo = sd
			ss.FileSystemVolumeInfo = *hs.Config.FileSystemVolume
			ss.MultipathStateInfo = hs.Config.MultipathState
		}
	}
}
