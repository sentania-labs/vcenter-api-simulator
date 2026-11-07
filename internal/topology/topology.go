// Package topology is the simulator's source of truth: the inventory it
// serves on /sdk and the entities it serves on /vsanHealth come from one
// Topology, so every identifier the management pack joins on (vCenter
// instance UUID, vSAN cluster UUID, host node UUIDs, disk vsanUuids, VM
// instanceUuids, VMDK namespace paths) is the same on both surfaces.
//
// Identities are derived, never stored: each one is a UUID v5 of
// (namespace, kind, serial), where the namespace comes from the seed. The
// same seed therefore reproduces the same identities after a restart, and a
// different seed changes every identifier without changing the structure.
// Names never feed identity, so a rename cannot churn an id.
//
// Phase 1 builds one fixed topology (Phase1): one datacenter, one vSAN ESA
// cluster of four hosts, two NVMe devices per host, one vDS, one vSAN
// vmkernel NIC per host, and four VMs on the vSAN datastore.
package topology

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Topology is one simulated vCenter.
type Topology struct {
	Seed         string
	InstanceUUID string // ServiceContent.about.instanceUuid
	Datacenter   string
	Cluster      Cluster
}

// Cluster is a vSAN ESA cluster.
type Cluster struct {
	Name        string
	VsanUUID    string // configurationEx.vsanConfigInfo.defaultConfig.uuid and every cluster-* entityRefId
	Dedup       bool   // ESA dedup/compression: switches the *-esa-dedup-* families on
	Compression bool
	VDS         string
	Portgroup   string
	Hosts       []*Host
	VMs         []*VM
}

// Host is one ESXi host.
type Host struct {
	Index        int
	Name         string
	HardwareUUID string // summary.hardware.uuid
	NodeUUID     string // vsanSystem.config.clusterInfo.nodeUuid and every host-scoped entityRefId
	Devices      []*Device
	Pnics        []string // vSAN uplinks, the vsan-pnic-net entities
	VsanVmknic   string   // the vsan-vnic-net / vnic-rdt entity
	Stack        string   // TCP/IP stack name
	Threads      int      // host-cpu: cpu-0..Threads-1 plus the aggregate
	Worlds       []World  // dom-world-cpu entities
}

// World is one DOM world on a host.
type World struct {
	Name string // VSAN_0x<hex>_<Type>
	ID   int64
}

// Device is one NVMe device behind its own PCIe HBA.
type Device struct {
	Index         int
	HBA           string // vmhbaN
	HBAModel      string
	Driver        string
	Model         string // 40-column model field, unpadded
	Vendor        string
	Revision      string // firmware
	Serial        string // 16 hex
	CanonicalName string // t10.NVMe____<model>____<serial>
	Key           string // key-vim.host.ScsiDisk-<uuid>
	UUID          string // ScsiLun.uuid (64 hex)
	CapacityBytes int64
	Claimed       bool   // claimed by vSAN ESA (storage pool member)
	VsanUUID      string // vsanDiskInfo.vsanUuid when Claimed
}

// VM is a virtual machine on the vSAN datastore.
type VM struct {
	Index         int
	Name          string
	UUID          string // config.uuid (SMBIOS)
	InstanceUUID  string // config.instanceUuid, the virtual-machine / vscsi identity
	NamespaceUUID string // the VM home namespace directory on the vSAN datastore
	Host          int    // index into Cluster.Hosts
	Disks         []*VDisk
}

// VDisk is one virtual disk of a VM.
type VDisk struct {
	Path       string // "<namespace>/<file>.vmdk", the virtual-disk identity (backing.fileName minus "[datastore] ")
	Device     string // "scsiN:M", the vscsi identity
	CapacityKB int64
}

// Datastore is the name of the cluster's vSAN datastore as vcsim creates it.
const Datastore = "vsanDatastore"

// ids derives identifiers from the seed.
type ids struct{ ns uuid.UUID }

func newIDs(seed string) ids {
	return ids{ns: uuid.NewSHA1(uuid.NameSpaceOID, []byte("vcenter-api-simulator:"+seed))}
}

// UUID returns the identity of (kind, serial) as a UUID string.
func (i ids) UUID(kind string, serial int) string {
	return uuid.NewSHA1(i.ns, []byte(fmt.Sprintf("%s:%d", kind, serial))).String()
}

// Hex returns n hex characters of identity for (kind, serial).
func (i ids) Hex(kind string, serial int, n int) string {
	sum := sha256.Sum256([]byte(i.ns.String() + "|" + kind + ":" + fmt.Sprint(serial)))
	return hex.EncodeToString(sum[:])[:n]
}

// T10Model encodes a model string the way an NVMe canonical name carries it:
// spaces become underscores, other non-alphanumerics their hex code, padded
// with underscores to 40 columns.
func T10Model(model string) string {
	var b strings.Builder
	for _, ch := range model {
		switch {
		case ch == ' ':
			b.WriteByte('_')
		case (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9'):
			b.WriteRune(ch)
		default:
			fmt.Fprintf(&b, "%02X", ch)
		}
	}
	for b.Len() < 40 {
		b.WriteByte('_')
	}
	return b.String()
}

// Phase1 returns the fixed Phase 1 topology for a seed.
func Phase1(seed string) *Topology {
	id := newIDs(seed)
	t := &Topology{
		Seed:         seed,
		InstanceUUID: id.UUID("vcenter", 0),
		Datacenter:   "dc-01",
		Cluster: Cluster{
			Name:        "cluster-01",
			VsanUUID:    id.UUID("vsan-cluster", 1),
			Dedup:       true,
			Compression: true,
			VDS:         "vds-01",
			Portgroup:   "pg-01",
		},
	}
	const hosts = 4
	for n := 1; n <= hosts; n++ {
		h := &Host{
			Index:        n - 1,
			Name:         fmt.Sprintf("esx%02d", n),
			HardwareUUID: id.UUID("host-hardware", n),
			NodeUUID:     id.UUID("vsan-node", n),
			Pnics:        []string{"vmnic1", "vmnic2"},
			VsanVmknic:   "vmk2",
			Stack:        "defaultTcpipStack",
			Threads:      16,
		}
		// Two devices: a local boot/VMFS device on vmhba0 and the device
		// claimed by vSAN ESA on vmhba1. Model strings are the placeholders
		// the masked fixtures use.
		devs := []struct {
			model, hbaModel string
			capacity        int64
			claimed         bool
		}{
			{"SIM NVMe 1TB Model C", "SIM NVMe Controller C", 1024209543168, false},
			{"SIM NVMe 4TB Model A", "SIM NVMe Controller A", 4000787030016, true},
		}
		for di, d := range devs {
			serial := n*10 + di
			dev := &Device{
				Index:         di,
				HBA:           fmt.Sprintf("vmhba%d", di),
				HBAModel:      d.hbaModel,
				Driver:        "nvme_pcie",
				Model:         d.model,
				Vendor:        "NVMe",
				Revision:      "SIMFW" + strings.ToUpper(id.Hex("firmware", di, 3)),
				Serial:        strings.ToUpper(id.Hex("nvme-serial", serial, 16)),
				UUID:          id.Hex("scsi-lun", serial, 64),
				CapacityBytes: d.capacity,
				Claimed:       d.claimed,
			}
			dev.CanonicalName = "t10.NVMe____" + T10Model(dev.Model) + dev.Serial
			dev.Key = "key-vim.host.ScsiDisk-" + dev.UUID
			if dev.Claimed {
				dev.VsanUUID = id.UUID("vsan-disk", serial)
			}
			h.Devices = append(h.Devices, dev)
		}
		// DOM worlds, a small fixed set per host with the four world types
		// seen live. Names carry a per-host hex address; ids are per host.
		types := []string{"Client", "Client", "Owner", "Owner", "OwnerHlpr0", "OwnerHlpr0",
			"CompServer", "CompServer", "CompServer", "CompServer", "CompServer", "CompServer"}
		for wi, wt := range types {
			serial := n*100 + wi
			h.Worlds = append(h.Worlds, World{
				Name: fmt.Sprintf("VSAN_0x%s_%s", id.Hex("dom-world-addr", serial, 12), wt),
				ID:   2098000 + int64(n*1000+wi),
			})
		}
		t.Cluster.Hosts = append(t.Cluster.Hosts, h)
	}
	const vms = 4
	for n := 1; n <= vms; n++ {
		vm := &VM{
			Index:         n - 1,
			Name:          fmt.Sprintf("vm-%02d", n),
			UUID:          id.UUID("vm-smbios", n),
			InstanceUUID:  id.UUID("vm-instance", n),
			NamespaceUUID: id.UUID("vm-namespace", n),
			Host:          (n - 1) % hosts,
		}
		// A VMDK path carries the VM name on a real vCenter too
		// ("[ds] <namespace>/<vm>.vmdk"); the pack joins it to the VM through
		// backing.fileName, never through the name, so this is the one
		// identity that is name-derived by nature.
		vm.Disks = append(vm.Disks, &VDisk{
			Path:       fmt.Sprintf("%s/%s.vmdk", vm.NamespaceUUID, vm.Name),
			Device:     "scsi0:0",
			CapacityKB: 40 * 1024 * 1024,
		})
		t.Cluster.VMs = append(t.Cluster.VMs, vm)
	}
	return t
}
