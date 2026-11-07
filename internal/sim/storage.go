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
	return sd
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
