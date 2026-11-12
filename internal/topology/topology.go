// Package topology is the simulator's source of truth: the inventory it
// serves on /sdk and the entities it serves on /vsanHealth come from one
// Topology, so every identifier the management pack joins on (vCenter
// instance UUID, vSAN cluster UUIDs, host node UUIDs, disk vsanUuids, VM
// instanceUuids, VMDK namespace paths) is the same on both surfaces.
//
// A Topology is generated from a scenario (package scenario) and the
// identity registry (package registry). Identities are derived, never
// stored: each one is a UUID v5 of (namespace, kind, serial), where the
// namespace comes from the scenario's seed and the serial from the registry,
// so a restart reproduces every identity, an edit keeps the identities of
// the entities it did not touch, and only a seed change (an explicit
// regenerate) changes every identifier. Names never feed identity.
//
// High-fanout children (CPUs, DOM worlds, uplinks, vmkernel NICs, virtual
// disks) are derived from their parent and an index and are not registered.
package topology

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// Topology is one simulated vCenter.
type Topology struct {
	VCenterVersion scenario.Version
	Seed           string
	TelemetrySeed  string // feeds metric values; identities never depend on it
	InstanceUUID   string // ServiceContent.about.instanceUuid
	Datacenters    []*Datacenter
}

// Datacenter holds clusters and the one vDS their hosts share.
type Datacenter struct {
	Name     string
	Key      string // registry key tail: the name
	Serial   int
	VDS      string
	VDSUUID  string // the vDS uuid on /sdk (summary.uuid, proxy switch dvsUuid, vmkernel NIC switchUuid)
	Clusters []*Cluster
	NFS      []NFSDatastore
	ISCSI    []ISCSIStorage
}

type NFSDatastore struct {
	Spec   scenario.NFSDatastore
	Serial int
	UUID   string
}

type ISCSIStorage struct {
	Spec scenario.ISCSIStorage
	LUNs []ISCSILUN
}

type ISCSILUN struct {
	Spec          scenario.ISCSILUN
	Serial        int
	UUID          string
	CanonicalName string
}

// Portgroups every datacenter's vDS carries, in creation order. Host
// vmkernel NICs attach to the first three; VMs to the last.
var Portgroups = []string{"pg-mgmt", "pg-vmotion", "pg-vsan", "pg-vm"}

// Cluster is a vSAN ESA or OSA cluster.
type Cluster struct {
	Name       string
	Key        string // "<datacenter>/<cluster>", the scenario's override and fault selector
	Serial     int
	Datacenter *Datacenter
	VsanUUID   string // configurationEx.vsanConfigInfo.defaultConfig.uuid and every cluster-* entityRefId
	StatsUUID  string // vSAN performance statistics object
	PolicyUUID string // default storage policy
	// TelemetrySeed feeds this cluster's metric values: the scenario's
	// telemetry seed, narrowed by the cluster's seed epoch once it was reset.
	TelemetrySeed string
	Dedup         bool // ESA dedup/compression: switches the *-esa-dedup-* families on
	Compression   bool
	Datastore     string // the cluster's vSAN datastore name
	ContainerID   string // the vSAN datastore container id (url ds:///vmfs/volumes/vsan:<id>/)
	Hosts         []*Host
	VMs           []*VM
	Spec          scenario.ClusterSpec // the effective cardinality after defaults, draws and overrides

	// perfOff is the console's "performance service switched off" switch.
	// It is a pointer so that a copy of the cluster shares it, and atomic
	// because the console flips it while requests are being answered.
	perfOff *atomic.Bool
}

// PrimaryOnline reports whether the cluster's vSAN performance service
// answers: the scenario's statsPrimary, unless the console switched the
// service off.
func (c *Cluster) PrimaryOnline() bool {
	if c.perfOff != nil && c.perfOff.Load() {
		return false
	}
	return c.Spec.StatsPrimary == nil || *c.Spec.StatsPrimary
}

// SetPerfOff switches the cluster's performance service off (true) or back
// on while the simulator runs. It changes no identity and no history.
func (c *Cluster) SetPerfOff(off bool) {
	if c.perfOff != nil {
		c.perfOff.Store(off)
	}
}

// VsanCapacity and VsanUsed are shared by datastore and vSAN capacity reads.
func VsanCapacity(cl *Cluster) int64 {
	var total int64
	for _, h := range cl.Hosts {
		for _, d := range h.Devices {
			if d.Claimed {
				total += d.CapacityBytes
			}
		}
	}
	return total
}

func VsanUsed(cl *Cluster) int64 {
	var used int64
	for _, vm := range cl.VMs {
		for _, d := range vm.Disks {
			used += d.CapacityKB << 10 / 2
		}
	}
	return used
}

// VDS is the datacenter's vDS name; VDSUUID its uuid; Portgroup the VM
// portgroup. Kept as methods so the /vsanHealth and network code reads one
// place.
func (c *Cluster) VDS() string     { return c.Datacenter.VDS }
func (c *Cluster) VDSUUID() string { return c.Datacenter.VDSUUID }

// Host is one ESXi host.
type Host struct {
	Version       scenario.Version
	Index         int // within the cluster
	Name          string
	Key           string
	Serial        int
	Cluster       *Cluster
	HardwareUUID  string // summary.hardware.uuid
	NodeUUID      string // vsanSystem.config.clusterInfo.nodeUuid and every host-scoped entityRefId
	DiskGroupUUID string // OSA cache-disk and disk-group performance identity
	Devices       []*Device
	Pnics         []string // vSAN uplinks, the vsan-pnic-net entities
	VsanVmknic    string   // the vsan-vnic-net / vnic-rdt entity
	Stack         string   // TCP/IP stack name
	Threads       int      // host-cpu: cpu-0..Threads-1 plus the aggregate
	Worlds        []World  // dom-world-cpu entities
	// Addresses are derived from the host serial: a /24 per 254 hosts.
	MgmtIP    string
	VmotionIP string
	VsanIP    string
}

// World is one DOM world on a host.
type World struct {
	Name string // VSAN_0x<hex>_<Type>
	ID   int64
}

// Device is one NVMe device behind its own PCIe HBA.
type Device struct {
	Index         int
	Key           string
	Serial        int
	HBA           string // vmhbaN
	HBAModel      string
	Driver        string
	Model         string // 40-column model field, unpadded
	Vendor        string
	Revision      string // firmware
	SerialNumber  string // 16 hex
	CanonicalName string // t10.NVMe____<model>____<serial>
	ScsiKey       string // key-vim.host.ScsiDisk-<uuid>
	UUID          string // ScsiLun.uuid (64 hex)
	CapacityBytes int64
	Claimed       bool   // claimed by vSAN ESA (storage pool member)
	VsanUUID      string // vsanDiskInfo.vsanUuid when Claimed
	VMFSUUID      string // the local VMFS volume uuid when not Claimed
}

// VM is a virtual machine on the cluster's vSAN datastore.
type VM struct {
	Index         int
	Name          string
	Key           string
	Serial        int
	Cluster       *Cluster
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

// Domain is the DNS domain simulated host names live in (an IANA-reserved
// name that cannot resolve).
const Domain = "sim.invalid"

// Clusters lists every cluster in datacenter order.
func (t *Topology) Clusters() []*Cluster {
	var out []*Cluster
	for _, dc := range t.Datacenters {
		out = append(out, dc.Clusters...)
	}
	return out
}

// Counts summarises cardinality for logs and reports.
func (t *Topology) Counts() (clusters, hosts, vms, devices int) {
	for _, c := range t.Clusters() {
		clusters++
		hosts += len(c.Hosts)
		vms += len(c.VMs)
		for _, h := range c.Hosts {
			devices += len(h.Devices)
		}
	}
	return
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

// Phase1 returns the single-cluster Phase 1 topology for a seed, with an
// in-memory registry.
func Phase1(seed string) *Topology {
	return Generate(scenario.Phase1(seed), registry.InMemory(seed))
}

// Device model placeholders: the masked fixtures' strings, so the simulator
// never names real hardware.
var (
	claimedModels = []struct {
		model, hba string
		capacity   int64
	}{
		{"SIM NVMe 4TB Model A", "SIM NVMe Controller A", 4000787030016},
		{"SIM NVMe 2TB Model D", "SIM NVMe Controller D", 2000398934016},
	}
	localModels = []struct {
		model, hba string
		capacity   int64
	}{
		{"SIM NVMe 1TB Model C", "SIM NVMe Controller C", 1024209543168},
		{"SIM NVMe 500GB Model B", "SIM NVMe Controller B", 500107862016},
	}
	worldTypes = []string{"Client", "Client", "Owner", "Owner", "OwnerHlpr0", "OwnerHlpr0",
		"CompServer", "CompServer", "CompServer", "CompServer", "CompServer", "CompServer"}
)

// Generate materializes a scenario against a registry. Every entity's serial
// is looked up or allocated in a fixed traversal order, so a new registry
// numbers the inventory the same way every time and an existing one keeps
// its numbers.
func Generate(s *scenario.Scenario, r *registry.Registry) *Topology {
	vc, _ := s.VCenterVersion()
	t := &Topology{Seed: s.Seed, TelemetrySeed: s.TelemetrySeed, InstanceUUID: r.UUID("vcenter", 0), VCenterVersion: vc}
	if t.TelemetrySeed == "" {
		t.TelemetrySeed = s.Seed
	}
	for _, d := range s.Datacenters {
		dc := &Datacenter{Name: d.Name, Key: d.Name, Serial: r.Serial("datacenter/" + d.Name), VDS: "vds-01"}
		dc.VDSUUID = vdsUUID(r.Hex("vds", r.Serial("vds/"+d.Name), 32))
		for n := 1; n <= d.Clusters; n++ {
			if s.IsRemoved(scenario.ClusterKey(d.Name, n)) {
				continue
			}
			dc.Clusters = append(dc.Clusters, generateCluster(s, r, dc, t.TelemetrySeed, n))
		}
		t.Datacenters = append(t.Datacenters, dc)
	}
	for i, d := range s.Datacenters {
		for _, n := range d.NFS {
			serial := r.Serial("nfs/" + d.Name + "/" + n.Name)
			t.Datacenters[i].NFS = append(t.Datacenters[i].NFS, NFSDatastore{Spec: n, Serial: serial, UUID: r.UUID("nfs", serial)})
		}
		for _, st := range d.ISCSI {
			store := ISCSIStorage{Spec: st}
			for _, lun := range st.LUNs {
				serial := r.Serial("iscsi/" + st.Cluster + "/" + lun.Name)
				store.LUNs = append(store.LUNs, ISCSILUN{Spec: lun, Serial: serial, UUID: r.UUID("iscsi-vmfs", serial), CanonicalName: "naa.6" + r.Hex("iscsi-naa", serial, 31)})
			}
			t.Datacenters[i].ISCSI = append(t.Datacenters[i].ISCSI, store)
		}
	}
	return t
}

// vdsUUID renders 32 hex characters in the vDS uuid shape
// ("50 03 9c a3 c3 b8 10 19-bd 21 c6 23 a9 76 7f 93").
func vdsUUID(h string) string {
	var parts []string
	for i := 0; i < 32; i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts[:8], " ") + "-" + strings.Join(parts[8:], " ")
}

func generateCluster(s *scenario.Scenario, r *registry.Registry, dc *Datacenter, telemetrySeed string, n int) *Cluster {
	name := fmt.Sprintf("cluster-%02d", n)
	key := dc.Name + "/" + name
	spec := s.Resolve(key, draw(s.Vary, r.Sub(key)))
	cl := &Cluster{
		Name: name, Key: key, Datacenter: dc,
		Serial:        r.Serial("cluster/" + key),
		TelemetrySeed: telemetrySeed,
		Dedup:         *spec.Dedup && spec.Architecture != "osa",
		Compression:   *spec.Compression && spec.Architecture != "osa",
		Spec:          spec,
		perfOff:       new(atomic.Bool),
	}
	if e := s.SeedEpoch(key); e > 0 {
		cl.TelemetrySeed = fmt.Sprintf("%s/%s#%d", telemetrySeed, key, e)
	}
	cl.VsanUUID = r.UUID("vsan-cluster", cl.Serial)
	cl.StatsUUID = r.UUID("vsan-stats-object", cl.Serial)
	cl.PolicyUUID = r.UUID("vsan-default-policy", cl.Serial)
	cl.ContainerID = r.Hex("vsan-container", cl.Serial, 16) + "-" + r.Hex("vsan-container-tail", cl.Serial, 16)
	// vCenter names the first vSAN datastore in a datacenter "vsanDatastore"
	// and the next ones "vsanDatastore (1)", "vsanDatastore (2)", ...
	if n == 1 {
		cl.Datastore = "vsanDatastore"
	} else {
		cl.Datastore = fmt.Sprintf("vsanDatastore (%d)", n-1)
	}
	removed := s.RemovedHosts(key)
	for hi := 1; hi <= spec.Hosts; hi++ {
		if removed[hi] {
			// a host the console removed by name: its number is not reused
			continue
		}
		host := generateHost(r, cl, hi)
		host.Index = len(cl.Hosts)
		host.Version, _ = s.HostVersion(cl.Key, host.Key)
		cl.Hosts = append(cl.Hosts, host)
	}
	nvms := len(cl.Hosts) * scenario.Int(spec.VMs.PerHost)
	for vi := 1; vi <= nvms; vi++ {
		cl.VMs = append(cl.VMs, generateVM(r, cl, vi))
	}
	return cl
}

// draw makes a cluster's independent draws from Vary with its own seed.
func draw(v scenario.Vary, sub uint64) scenario.ClusterSpec {
	rng := scenario.DrawSource(sub)
	var d scenario.ClusterSpec
	choice := func(opts []int) int {
		u := rng.Uint64()
		if len(opts) == 0 {
			return 0
		}
		return opts[u%uint64(len(opts))]
	}
	rng1 := func(rg *scenario.Range) int {
		u := rng.Uint64()
		if rg == nil {
			return 0
		}
		return rg.Min + int(u%uint64(rg.Max-rg.Min+1))
	}
	// one draw per knob, always, so adding a knob to the file later does not
	// re-roll the earlier ones
	d.Hosts = v.DrawHosts(rng)
	if n := rng1(v.VMsPerHost); v.VMsPerHost != nil {
		d.VMs.PerHost = scenario.IntPtr(n)
	}
	d.Host.ClaimedDevices = choice(v.ClaimedDevices)
	if n := rng1(v.DomWorlds); v.DomWorlds != nil {
		d.Host.DomWorlds = scenario.IntPtr(n)
	}
	d.Host.Threads = choice(v.Threads)
	return d
}

func generateHost(r *registry.Registry, cl *Cluster, n int) *Host {
	short := fmt.Sprintf("esx%02d", n)
	key := cl.Key + "/" + short
	h := &Host{
		Index:      n - 1,
		Name:       fmt.Sprintf("%s.%s.%s.%s", short, cl.Name, cl.Datacenter.Name, Domain),
		Key:        key,
		Serial:     r.Serial("host/" + key),
		Cluster:    cl,
		VsanVmknic: "vmk2",
		Stack:      "defaultTcpipStack",
		Threads:    cl.Spec.Host.Threads,
	}
	h.HardwareUUID = r.UUID("host-hardware", h.Serial)
	h.NodeUUID = r.UUID("vsan-node", h.Serial)
	h.DiskGroupUUID = r.UUID("vsan-disk-group", h.Serial)
	for i := 0; i < cl.Spec.Host.VsanPnics; i++ {
		h.Pnics = append(h.Pnics, fmt.Sprintf("vmnic%d", i+1))
	}
	h.MgmtIP, h.VmotionIP, h.VsanIP = address(10, h.Serial), address(11, h.Serial), address(12, h.Serial)
	// Devices: the local boot/VMFS devices first (vmhba0...), then the ESA
	// storage pool members, each behind its own PCIe HBA.
	di := 0
	add := func(claimed bool, ki int, model struct {
		model, hba string
		capacity   int64
	}) {
		kind := "local"
		if claimed {
			kind = "claimed"
		}
		dkey := fmt.Sprintf("%s/%s%d", key, kind, ki)
		d := &Device{
			Index:         di,
			Key:           dkey,
			Serial:        r.Serial("device/" + dkey),
			HBA:           fmt.Sprintf("vmhba%d", di),
			HBAModel:      model.hba,
			Driver:        "nvme_pcie",
			Model:         model.model,
			Vendor:        "NVMe",
			CapacityBytes: model.capacity,
			Claimed:       claimed,
		}
		d.Revision = "SIMFW" + strings.ToUpper(r.Hex("firmware", d.Serial, 3))
		d.SerialNumber = strings.ToUpper(r.Hex("nvme-serial", d.Serial, 16))
		d.UUID = r.Hex("scsi-lun", d.Serial, 64)
		d.CanonicalName = "t10.NVMe____" + T10Model(d.Model) + d.SerialNumber
		d.ScsiKey = "key-vim.host.ScsiDisk-" + d.UUID
		if claimed {
			d.VsanUUID = r.UUID("vsan-disk", d.Serial)
		} else {
			d.VMFSUUID = vmfsUUID(r.Hex("vmfs", d.Serial, 32))
		}
		h.Devices = append(h.Devices, d)
		di++
	}
	for i := 0; i < scenario.Int(cl.Spec.Host.LocalDevices); i++ {
		add(false, i, localModels[i%len(localModels)])
	}
	for i := 0; i < cl.Spec.Host.ClaimedDevices; i++ {
		add(true, i, claimedModels[i%len(claimedModels)])
	}
	// DOM worlds: the four world types seen live, cycled. Names carry a
	// per-host hex address; ids are per host.
	for wi := 0; wi < scenario.Int(cl.Spec.Host.DomWorlds); wi++ {
		h.Worlds = append(h.Worlds, World{
			Name: fmt.Sprintf("VSAN_0x%s_%s", hashHex(h.NodeUUID, "dom-world-addr", wi, 12), worldTypes[wi%len(worldTypes)]),
			ID:   2098000 + int64(h.Serial*1000+wi),
		})
	}
	return h
}

// vmfsUUID renders a VMFS volume uuid ("69fd165a-38473b59-7bc3-3805253936dc").
func vmfsUUID(h string) string {
	return h[0:8] + "-" + h[8:16] + "-" + h[16:20] + "-" + h[20:32]
}

// address gives host n a stable address in 10.<net>.x.y.
func address(net, n int) string {
	return fmt.Sprintf("10.%d.%d.%d", net, (n/254)%256, n%254+1)
}

func generateVM(r *registry.Registry, cl *Cluster, n int) *VM {
	name := fmt.Sprintf("%s-vm-%03d", cl.Name, n)
	key := cl.Key + "/" + name
	vm := &VM{
		Index:   n - 1,
		Name:    name,
		Key:     key,
		Serial:  r.Serial("vm/" + key),
		Cluster: cl,
		Host:    (n - 1) % len(cl.Hosts), // placement follows the current host count, as DRS would rebalance
	}
	vm.UUID = r.UUID("vm-smbios", vm.Serial)
	vm.InstanceUUID = r.UUID("vm-instance", vm.Serial)
	vm.NamespaceUUID = r.UUID("vm-namespace", vm.Serial)
	// A VMDK path carries the VM name on a real vCenter too
	// ("[ds] <namespace>/<vm>.vmdk"); the pack joins it to the VM through
	// backing.fileName, never through the name, so this is the one
	// identity that is name-derived by nature.
	for di := 0; di < cl.Spec.VMs.DisksPerVM; di++ {
		file := vm.Name
		if di > 0 {
			file = fmt.Sprintf("%s_%d", vm.Name, di)
		}
		vm.Disks = append(vm.Disks, &VDisk{
			Path:       fmt.Sprintf("%s/%s.vmdk", vm.NamespaceUUID, file),
			Device:     fmt.Sprintf("scsi0:%d", di),
			CapacityKB: 40 * 1024 * 1024,
		})
	}
	return vm
}

func hashHex(parent, kind string, i, n int) string {
	sum := sha256.Sum256([]byte(parent + "|" + kind + ":" + fmt.Sprint(i)))
	return hex.EncodeToString(sum[:])[:n]
}
