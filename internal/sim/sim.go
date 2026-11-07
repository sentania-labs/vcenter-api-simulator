// Package sim builds a govmomi vcsim instance from a topology and merges the
// out-of-tree /vsanHealth handlers into it. The result is one http.Handler
// serving /sdk and /vsanHealth from one session store, with the identities
// the topology defines on both surfaces.
package sim

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/simulator/vpx"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/types"
	_ "github.com/vmware/govmomi/vsan/simulator" // registers vcsim's stock /vsanHealth endpoint

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/sentania/vcenter-api-simulator/internal/vsanhealth"
)

// Options configure a Simulator.
type Options struct {
	Topology *topology.Topology
	// Now is the clock the Performance Service uses; nil means time.Now.
	Now func() time.Time
}

// Simulator is a running in-process vCenter.
type Simulator struct {
	Topology *topology.Topology
	Catalog  *catalog.Catalog
	Env      *vsanhealth.Env
	// Handler serves /sdk, /vsanHealth and the service version documents.
	Handler http.Handler
	// ClusterRef and HostRefs are the MoRefs vcsim assigned.
	ClusterRef types.ManagedObjectReference
	HostRefs   []types.ManagedObjectReference
	VMRefs     []types.ManagedObjectReference

	model  *simulator.Model
	server *simulator.Server
}

// New builds the inventory and starts the internal vcsim service.
func New(opts Options) (*Simulator, error) {
	if opts.Topology == nil {
		return nil, fmt.Errorf("sim: topology required")
	}
	cat, err := catalog.Load()
	if err != nil {
		return nil, err
	}
	topo := opts.Topology

	content := vpx.ServiceContent
	content.About = aboutInfo(cat, topo)
	m := &simulator.Model{
		ServiceContent: content,
		RootFolder:     vpx.RootFolder,
		Datacenter:     1,
		Cluster:        1,
		ClusterHost:    len(topo.Cluster.Hosts),
		Portgroup:      1,
	}
	if err := m.Create(); err != nil {
		return nil, fmt.Errorf("sim: create model: %w", err)
	}
	s := &Simulator{Topology: topo, Catalog: cat, model: m}
	s.Env = vsanhealth.NewEnv(topo, cat)
	if opts.Now != nil {
		s.Env.Now = opts.Now
	}
	if err := s.shape(); err != nil {
		m.Remove()
		return nil, err
	}

	m.Service.RegisterEndpoints = true
	m.Service.Listen = &url.URL{Host: "127.0.0.1:0"}
	s.server = m.Service.NewServer()
	vsanhealth.Install(m.Service, s.Env)
	s.Handler = &handler{mux: m.Service.ServeMux, cat: cat}
	return s, nil
}

// Close stops the internal server and removes vcsim's temporary files.
func (s *Simulator) Close() {
	if s.server != nil {
		s.server.Close()
	}
	if s.model != nil {
		s.model.Remove()
	}
}

func aboutInfo(cat *catalog.Catalog, topo *topology.Topology) types.AboutInfo {
	a := vpx.ServiceContent.About
	a.FullName = cat.About.FullName
	a.Version = cat.About.Version
	a.Build = cat.About.Build
	a.ApiVersion = cat.About.ApiVersion
	a.OsType = cat.About.OsType
	a.ProductLineId = cat.About.ProductLine
	a.LicenseProductName = cat.About.LicenseName
	a.LicenseProductVersion = cat.About.LicenseVer
	a.InstanceUuid = topo.InstanceUUID
	return a
}

// shape turns vcsim's generated inventory into the topology: names,
// identities, vSAN configuration, storage devices and VMs.
func (s *Simulator) shape() error {
	ctx := s.model.Service.Context
	topo := s.Topology
	cl := &topo.Cluster

	dc := ctx.Map.Any("Datacenter").(*simulator.Datacenter)
	dc.Name = topo.Datacenter
	ccr := ctx.Map.Any("ClusterComputeResource").(*simulator.ClusterComputeResource)
	ccr.Name = cl.Name
	s.ClusterRef = ccr.Self
	s.Env.RegisterCluster(ccr.Self, cl)
	if dvs, ok := ctx.Map.Any("DistributedVirtualSwitch").(*simulator.DistributedVirtualSwitch); ok {
		dvs.Name = cl.VDS
	}
	for _, e := range ctx.Map.All("DistributedVirtualPortgroup") {
		if pg, ok := e.(*simulator.DistributedVirtualPortgroup); ok && !strings.Contains(pg.Name, "DVUplinks") {
			pg.Name = cl.Portgroup
		}
	}

	if len(ccr.Host) != len(cl.Hosts) {
		return fmt.Errorf("sim: vcsim created %d hosts, topology has %d", len(ccr.Host), len(cl.Hosts))
	}
	esx := types.AboutInfo{
		Name:          "VMware ESXi",
		FullName:      s.Catalog.About.EsxiFullName,
		Vendor:        "VMware, Inc.",
		Version:       s.Catalog.About.EsxiVersion,
		Build:         s.Catalog.About.EsxiBuild,
		LocaleVersion: "INTL",
		LocaleBuild:   "000",
		OsType:        "vmnix-x86",
		ProductLineId: "embeddedEsx",
		ApiType:       "HostAgent",
		ApiVersion:    s.Catalog.About.ApiVersion,
	}
	for i, ref := range ccr.Host {
		h := cl.Hosts[i]
		hs := ctx.Map.Get(ref).(*simulator.HostSystem)
		hs.Name = h.Name
		hs.Summary.Config.Name = h.Name
		hs.Summary.Hardware.Uuid = h.HardwareUUID
		hs.Summary.Hardware.Vendor = "SIM Hardware Vendor"
		hs.Summary.Hardware.Model = "SIM-HOST"
		hs.Summary.Hardware.CpuModel = "SIM CPU 16-Core Processor"
		hs.Summary.Hardware.NumCpuCores = int16(h.Threads / 2)
		hs.Summary.Hardware.NumCpuThreads = int16(h.Threads)
		hs.Hardware.SystemInfo.Uuid = h.HardwareUUID
		hs.Hardware.SystemInfo.Vendor = "SIM Hardware Vendor"
		hs.Hardware.SystemInfo.Model = "SIM-HOST"
		hs.Hardware.SystemInfo.SerialNumber = "SIM" + strings.ToUpper(h.HardwareUUID[:8])
		hs.Config.Product = esx
		hs.Summary.Config.Product = &hs.Config.Product

		sd := storageDevice(h)
		hs.Config.StorageDevice = sd
		if ss, ok := ctx.Map.Get(*hs.ConfigManager.StorageSystem).(*simulator.HostStorageSystem); ok {
			ss.StorageDeviceInfo = sd
		}

		vs := newHostVsanSystem(ref, cl, h)
		ctx.Map.Put(vs)
		vref := vs.Self
		hs.ConfigManager.VsanSystem = &vref

		s.HostRefs = append(s.HostRefs, ref)
		s.Env.RegisterHost(ref, cl, h)
	}

	// vSAN on the cluster: our uuid first, then enable through the real
	// reconfigure path so vcsim creates the vSAN datastore on every host.
	ccr.ConfigurationEx.(*types.ClusterConfigInfoEx).VsanConfigInfo = &types.VsanClusterConfigInfo{
		Enabled:        types.NewBool(false),
		DefaultConfig:  &types.VsanClusterConfigInfoHostDefaultInfo{Uuid: cl.VsanUUID},
		VsanEsaEnabled: types.NewBool(true),
	}
	c, err := vim25.NewClient(ctx, s.model.Service)
	if err != nil {
		return err
	}
	spec := &types.ClusterConfigSpecEx{
		VsanConfig: &types.VsanClusterConfigInfo{Enabled: types.NewBool(true)},
	}
	res, err := methods.ReconfigureComputeResource_Task(ctx, c, &types.ReconfigureComputeResource_Task{
		This: ccr.Self, Spec: spec, Modify: true,
	})
	if err != nil {
		return fmt.Errorf("sim: enable vsan: %w", err)
	}
	if err := object.NewTask(c, res.Returnval).Wait(ctx); err != nil {
		return fmt.Errorf("sim: enable vsan: %w", err)
	}
	got := ccr.ConfigurationEx.(*types.ClusterConfigInfoEx).VsanConfigInfo.DefaultConfig.Uuid
	if got != cl.VsanUUID {
		return fmt.Errorf("sim: cluster vsan uuid %q, want %q", got, cl.VsanUUID)
	}

	var ds *simulator.Datastore
	for _, e := range ctx.Map.All("Datastore") {
		if d, ok := e.(*simulator.Datastore); ok && d.Name == topology.Datastore {
			ds = d
			break
		}
	}
	if ds == nil {
		return fmt.Errorf("sim: %s not created", topology.Datastore)
	}

	folders, err := object.NewDatacenter(c, dc.Self).Folders(ctx)
	if err != nil {
		return err
	}
	pool, err := object.NewClusterComputeResource(c, ccr.Self).ResourcePool(ctx)
	if err != nil {
		return err
	}
	for _, vm := range cl.VMs {
		ref, err := s.createVM(ctx, c, folders, pool, ds.Self, vm)
		if err != nil {
			return fmt.Errorf("sim: create %s: %w", vm.Name, err)
		}
		s.VMRefs = append(s.VMRefs, ref)
	}
	// vcsim's datastore URL is its temporary directory, which changes on
	// every start and is where it writes the VMs created above; once they
	// exist the summary gets a vSAN-shaped, seed-derived URL. Info.Url stays
	// the temp path because vcsim's file operations use it.
	ds.Summary.Url = fmt.Sprintf("ds:///vmfs/volumes/vsan:%s-%s/", strings.ReplaceAll(cl.VsanUUID[:18], "-", ""), strings.ReplaceAll(cl.VsanUUID[19:], "-", ""))
	return nil
}

func (s *Simulator) createVM(ctx context.Context, c *vim25.Client, folders *object.DatacenterFolders, pool *object.ResourcePool, ds types.ManagedObjectReference, vm *topology.VM) (types.ManagedObjectReference, error) {
	var none types.ManagedObjectReference
	hostRef := s.HostRefs[vm.Host]
	config := types.VirtualMachineConfigSpec{
		Name:    vm.Name,
		GuestId: string(types.VirtualMachineGuestOsIdentifierOtherGuest),
		Files: &types.VirtualMachineFileInfo{
			VmPathName: fmt.Sprintf("[%s] %s/%s.vmx", topology.Datastore, vm.NamespaceUUID, vm.Name),
		},
		NumCPUs:  2,
		MemoryMB: 4096,
	}
	var devices object.VirtualDeviceList
	scsi, err := devices.CreateSCSIController("pvscsi")
	if err != nil {
		return none, err
	}
	devices = append(devices, scsi)
	for _, d := range vm.Disks {
		disk := devices.CreateDisk(scsi.(types.BaseVirtualController), ds, fmt.Sprintf("[%s] %s", topology.Datastore, d.Path))
		disk.CapacityInKB = d.CapacityKB
		devices = append(devices, disk)
	}
	config.DeviceChange, err = devices.ConfigSpec(types.VirtualDeviceConfigSpecOperationAdd)
	if err != nil {
		return none, err
	}
	task, err := folders.VmFolder.CreateVM(ctx, config, pool, object.NewHostSystem(c, hostRef))
	if err != nil {
		return none, err
	}
	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return none, err
	}
	ref := info.Result.(types.ManagedObjectReference)
	obj := s.model.Service.Context.Map.Get(ref).(*simulator.VirtualMachine)
	obj.Config.Uuid = vm.UUID
	obj.Config.InstanceUuid = vm.InstanceUUID
	obj.Summary.Config.Uuid = vm.UUID
	obj.Summary.Config.InstanceUuid = vm.InstanceUUID
	return ref, nil
}

// handler serves the service version documents from the capture and
// everything else from vcsim.
type handler struct {
	mux *http.ServeMux
	cat *catalog.Catalog
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/sdk/vimServiceVersions.xml":
		w.Header().Set("Content-Type", "text/xml; charset=UTF-8")
		fmt.Fprint(w, h.cat.VimServiceVersions)
	case "/sdk/vsanServiceVersions.xml":
		w.Header().Set("Content-Type", "text/xml; charset=UTF-8")
		fmt.Fprint(w, h.cat.VsanServiceVersions)
	default:
		h.mux.ServeHTTP(w, r)
	}
}
