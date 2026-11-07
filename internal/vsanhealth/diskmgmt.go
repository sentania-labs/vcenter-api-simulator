package vsanhealth

import (
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	vim25types "github.com/vmware/govmomi/vim25/types"
	vsantypes "github.com/vmware/govmomi/vsan/types"

	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// DiskManagementSystem is VimClusterVsanVcDiskManagementSystem:vsan-disk-management-system.
type DiskManagementSystem struct {
	vim25types.ManagedObjectReference
	env *Env
}

// govmomi v0.56.2's VimVsanHostVsanManagedDisksInfo predates ESA storage
// pools, so the response is declared here with the storagePools member the
// capture shows (manageddisks-domain-c6004-host-*.resp.xml) and pyVmomi 8.0.3
// defines (vim.vsan.host.StoragePoolDiskInfo).
type storagePoolDiskInfo struct {
	Disk        vim25types.HostScsiDisk `xml:"disk"`
	VsanUuid    string                  `xml:"vsanUuid"`
	IsMounted   bool                    `xml:"isMounted"`
	IsEncrypted bool                    `xml:"isEncrypted"`
	DekId       string                  `xml:"dekId"`
	DiskType    string                  `xml:"diskType"`
}

type storagePoolInfo struct {
	StoragePoolDisks []storagePoolDiskInfo `xml:"storagePoolDisks,omitempty"`
}

type managedDisksInfo struct {
	VSANDirectDisks []vsantypes.VimVsanHostVsanDirectStorage `xml:"vSANDirectDisks,omitempty"`
	VSANDiskMapInfo []vsantypes.VimVsanHostDiskMapInfoEx     `xml:"vSANDiskMapInfo,omitempty"`
	StoragePools    []storagePoolInfo                        `xml:"storagePools,omitempty"`
}

type queryManagedDisksResponse struct {
	Returnval *managedDisksInfo `xml:"returnval,omitempty"`
}

type queryManagedDisksBody struct {
	Res    *queryManagedDisksResponse `xml:"urn:vsan QueryVsanManagedDisksResponse,omitempty"`
	Fault_ *soap.Fault                `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault,omitempty"`
}

func (b *queryManagedDisksBody) Fault() *soap.Fault { return b.Fault_ }

// QueryVsanManagedDisks returns the host's ESA storage pool: one
// single-tier entry per claimed device.
func (s *DiskManagementSystem) QueryVsanManagedDisks(ctx *simulator.Context, req *vsantypes.QueryVsanManagedDisks) soap.HasFault {
	h, cl := s.env.Host(req.Host)
	if h == nil {
		return &queryManagedDisksBody{Fault_: simulator.Fault("", &vim25types.ManagedObjectNotFound{Obj: req.Host})}
	}
	if fault := s.env.methodFault("QueryVsanManagedDisks", cl); fault != nil {
		return &queryManagedDisksBody{Fault_: fault}
	}
	pool := storagePoolInfo{}
	for _, d := range h.Devices {
		if !d.Claimed {
			continue
		}
		pool.StoragePoolDisks = append(pool.StoragePoolDisks, storagePoolDiskInfo{
			Disk:      ScsiDisk(d),
			VsanUuid:  d.VsanUUID,
			IsMounted: true,
			DiskType:  "singleTier",
		})
	}
	info := &managedDisksInfo{}
	if len(pool.StoragePoolDisks) > 0 {
		info.StoragePools = []storagePoolInfo{pool}
	}
	return &queryManagedDisksBody{Res: &queryManagedDisksResponse{Returnval: info}}
}

// ScsiDisk renders a device as the HostScsiDisk the capture shows on both
// surfaces (QueryVsanManagedDisks and HostVsanSystem.QueryDisksForVsan).
func ScsiDisk(d *topology.Device) vim25types.HostScsiDisk {
	disk := vim25types.HostScsiDisk{
		ScsiLun: vim25types.ScsiLun{
			HostDevice: vim25types.HostDevice{
				DeviceName: "/vmfs/devices/disks/" + d.CanonicalName,
				DeviceType: "disk",
			},
			Key:                    d.ScsiKey,
			Uuid:                   d.UUID,
			CanonicalName:          d.CanonicalName,
			DisplayName:            "Local NVMe Disk (" + d.CanonicalName + ")",
			LunType:                "disk",
			Vendor:                 d.Vendor,
			Model:                  padModel(d.Model),
			Revision:               d.Revision,
			ScsiLevel:              0,
			SerialNumber:           "NVMe____SIM_SERIAL" + d.SerialNumber,
			QueueDepth:             2046,
			OperationalState:       []string{"ok"},
			Capabilities:           &vim25types.ScsiLunCapabilities{UpdateDisplayNameSupported: true},
			VStorageSupport:        "vStorageUnsupported",
			ProtocolEndpoint:       vim25types.NewBool(false),
			PerenniallyReserved:    vim25types.NewBool(false),
			ClusteredVmdkSupported: vim25types.NewBool(false),
		},
		Capacity:              vim25types.HostDiskDimensionsLba{BlockSize: 512, Block: d.CapacityBytes / 512},
		DevicePath:            "/vmfs/devices/disks/" + d.CanonicalName,
		Ssd:                   vim25types.NewBool(true),
		LocalDisk:             vim25types.NewBool(true),
		EmulatedDIXDIFEnabled: vim25types.NewBool(false),
		ScsiDiskType:          "emulated512",
	}
	if d.Claimed {
		disk.VsanDiskInfo = &vim25types.VsanHostVsanDiskInfo{VsanUuid: d.VsanUUID, FormatVersion: 24}
	}
	return disk
}

// padModel right-pads a model string to the 40-column SCSI model field.
func padModel(m string) string {
	for len(m) < 40 {
		m += " "
	}
	return m
}
