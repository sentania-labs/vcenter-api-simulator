package sim

import (
	"fmt"

	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/sentania/vcenter-api-simulator/internal/vsanhealth"
)

// storageDevice builds HostSystem.config.storageDevice for a host: one PCIe
// HBA per NVMe device, the disks as ScsiDisk entries, and the
// adapter -> target -> lun topology the management pack walks to join a disk
// to its controller driver (capture summary.json hosts[].hostBusAdapter,
// scsiLun, scsiTopology).
func storageDevice(h *topology.Host) *types.HostStorageDeviceInfo {
	sd := &types.HostStorageDeviceInfo{}
	for i, d := range h.Devices {
		hbaKey := "key-vim.host.PcieHba-" + d.HBA
		sd.HostBusAdapter = append(sd.HostBusAdapter, &types.HostPcieHba{
			HostHostBusAdapter: types.HostHostBusAdapter{
				Key:    hbaKey,
				Device: d.HBA,
				Bus:    int32(i + 1),
				Status: "unknown",
				Model:  d.HBAModel,
				Driver: d.Driver,
				Pci:    fmt.Sprintf("0000:%02x:00.0", i+1),
			},
		})
		disk := vsanhealth.ScsiDisk(d)
		sd.ScsiLun = append(sd.ScsiLun, &disk)
		sd.ScsiTopology = appendTopology(sd.ScsiTopology, hbaKey, d.ScsiKey)
	}
	populatePlugStore(sd)
	return sd
}

// populatePlugStore relates each SCSI topology path to its adapter and LUN.
func populatePlugStore(sd *types.HostStorageDeviceInfo) {
	p := &types.HostPlugStoreTopology{}
	devices := map[string]int{}
	if sd.ScsiTopology == nil {
		sd.PlugStoreTopology = p
		return
	}
	deviceNames := map[string]string{}
	for _, hba := range sd.HostBusAdapter {
		b := hba.GetHostHostBusAdapter()
		deviceNames[b.Key] = b.Device
	}
	for _, adapter := range sd.ScsiTopology.Adapter {
		devName, ok := deviceNames[adapter.Adapter]
		if !ok {
			devName = adapter.Adapter
		}
		a := types.HostPlugStoreTopologyAdapter{Key: "plug-" + adapter.Adapter, Adapter: adapter.Adapter}
		for _, target := range adapter.Target {
			for _, lun := range target.Lun {
				idx, ok := devices[lun.ScsiLun]
				if !ok {
					idx = len(p.Device)
					devices[lun.ScsiLun] = idx
					p.Device = append(p.Device, types.HostPlugStoreTopologyDevice{Key: "plug-" + lun.ScsiLun, Lun: lun.ScsiLun})
				}
				name := fmt.Sprintf("%s:C0:T%d:L%d", devName, target.Target, lun.Lun)
				if sd.MultipathInfo != nil {
					for _, m := range sd.MultipathInfo.Lun {
						if m.Lun == lun.ScsiLun {
							for _, path := range m.Path {
								if path.Adapter == adapter.Adapter {
									name = path.Name
								}
							}
						}
					}
				}
				key := "plug-path-" + name
				a.Path = append(a.Path, key)
				p.Device[idx].Path = append(p.Device[idx].Path, key)
				p.Path = append(p.Path, types.HostPlugStoreTopologyPath{Key: key, Name: name, Adapter: a.Key, Device: p.Device[idx].Key, TargetNumber: target.Target, LunNumber: lun.Lun})
			}
		}
		p.Adapter = append(p.Adapter, a)
	}
	sd.PlugStoreTopology = p
}

func appendTopology(t *types.HostScsiTopology, hbaKey, lunKey string) *types.HostScsiTopology {
	if t == nil {
		t = &types.HostScsiTopology{}
	}
	t.Adapter = append(t.Adapter, types.HostScsiTopologyInterface{
		Key:     "key-vim.host.ScsiTopology.Interface-" + hbaKey,
		Adapter: hbaKey,
		Target: []types.HostScsiTopologyTarget{{
			Key:    "key-vim.host.ScsiTopology.Target-" + hbaKey + "-0",
			Target: 0,
			Lun: []types.HostScsiTopologyLun{{
				Key:     "key-vim.host.ScsiTopology.Lun-" + lunKey,
				Lun:     0,
				ScsiLun: lunKey,
			}},
		}},
	})
	return t
}
