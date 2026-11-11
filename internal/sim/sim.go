// Package sim builds a govmomi vcsim instance from a topology and merges the
// out-of-tree /vsanHealth handlers into it. The result is one http.Handler
// serving /sdk and /vsanHealth from one session store, with the identities
// the topology defines on both surfaces.
//
// The inventory is created through vcsim's own method handlers
// (CreateDatacenter, CreateClusterEx, AddHost_Task, CreateVM_Task) in
// registry serial order. Each creation step (including each local datastore)
// reserves its counter interval in the identity registry, so removals leave
// gaps and additions take fresh space instead of shifting survivors.
package sim

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/simulator/vpx"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/sentania/vcenter-api-simulator/internal/vsanhealth"
)

//go:embed fixtures/vimService.wsdl
var vimServiceWSDL string

// Options configure a Simulator.
type Options struct {
	Topology *topology.Topology
	Registry *registry.Registry
	// Now is the clock the Performance Service uses; nil means time.Now.
	Now func() time.Time
	// Username and Password, when Username is set, add one more accepted
	// login to Credentials. A convenience for callers with a single user.
	Username, Password string
	// Credentials are the accepted logins (name -> password). Empty means
	// any non-empty user name and password log in, as vcsim does.
	Credentials map[string]string
	// SessionTimeout is the idle timeout of a session; zero means never.
	SessionTimeout time.Duration
	// Faults and Latency are the scenario's failure and latency knobs.
	Faults  []scenario.Fault
	Latency scenario.Latency
}

// ClusterRefs are the MoRefs vcsim assigned to one cluster's objects.
type ClusterRefs struct {
	Cluster   *topology.Cluster
	Ref       types.ManagedObjectReference
	Datastore types.ManagedObjectReference
	Hosts     []types.ManagedObjectReference
	VMs       []types.ManagedObjectReference
}

// Simulator is a running in-process vCenter.
type Simulator struct {
	Topology *topology.Topology
	Catalog  *catalog.Catalog
	Env      *vsanhealth.Env
	// Handler serves /sdk, /vsanHealth and the service version documents.
	Handler http.Handler
	// Clusters are the MoRefs vcsim assigned, in topology order.
	Clusters []*ClusterRefs

	model    *simulator.Model
	server   *simulator.Server
	home     string // datastore directories
	tasks    *taskWaiter
	registry *registry.Registry

	dcs   map[*topology.Datacenter]*dcRefs
	hosts map[*topology.Host]*simulator.HostSystem
}

type dcRefs struct {
	ref                                       types.ManagedObjectReference
	dc                                        *simulator.Datacenter
	hostFolder, vmFolder, dsFolder, netFolder *simulator.Folder
	dvs                                       *simulator.DistributedVirtualSwitch
	pgs                                       map[string]*simulator.DistributedVirtualPortgroup
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
	m := &simulator.Model{ServiceContent: content, RootFolder: vpx.RootFolder}
	if err := m.Create(); err != nil {
		return nil, fmt.Errorf("sim: create model: %w", err)
	}
	s := &Simulator{Topology: topo, Catalog: cat, model: m, registry: opts.Registry, dcs: map[*topology.Datacenter]*dcRefs{}, hosts: map[*topology.Host]*simulator.HostSystem{}}
	s.home, err = os.MkdirTemp("", "vcsimulator-")
	if err != nil {
		m.Remove()
		return nil, err
	}
	s.Env = vsanhealth.NewEnv(topo, cat)
	if opts.Now != nil {
		s.Env.Now = opts.Now
	}
	s.Env.Knobs = vsanhealth.NewKnobs(opts.Faults, opts.Latency, s.Env.Now)

	m.Service.RegisterEndpoints = true
	m.Service.Listen = &url.URL{Host: "127.0.0.1:0"}
	creds := opts.Credentials
	if opts.Username != "" {
		// Fold the single user into a copy of the map (the caller's stays
		// untouched); installAuth turns the map into vcsim's login check.
		creds = map[string]string{opts.Username: opts.Password}
		for n, p := range opts.Credentials {
			creds[n] = p
		}
	}
	installAuth(m, creds, opts.SessionTimeout)
	if err := s.build(); err != nil {
		s.Close()
		return nil, err
	}
	s.server = m.Service.NewServer()
	vsanhealth.Install(m.Service, s.Env)
	s.Handler = &handler{mux: m.Service.ServeMux, cat: cat, env: s.Env, strictLogin: len(creds) > 0, rest: restSessions{tokens: map[string]bool{}, credentials: creds, registry: m.Service.Context.Map, catalog: cat}}
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
	if s.home != "" {
		os.RemoveAll(s.home)
	}
}

// Cluster returns the refs of a cluster by key.
func (s *Simulator) Cluster(key string) *ClusterRefs {
	for _, c := range s.Clusters {
		if c.Cluster.Key == key {
			return c
		}
	}
	return nil
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

// step is one inventory creation, ordered by registry serial.
type step struct {
	serial int
	order  int
	run    func() error
}

// build creates the inventory in serial order.
func (s *Simulator) build() error {
	ctx := s.model.Service.Context
	s.tasks = newTaskWaiter(ctx)
	var steps []step
	// Registries predating reservations created every local datastore during
	// its host step, even when its device serial was allocated much later.
	legacyOrder := s.registry == nil || s.registry.MorefHigh() == 0
	nfsRefs := map[string]*simulator.Datastore{}
	iscsiRefs := map[string]*simulator.Datastore{}
	for _, dc := range s.Topology.Datacenters {
		dc := dc
		steps = append(steps, step{dc.Serial, dc.Serial, func() error { return s.createDatacenter(ctx, dc) }})
		for _, n := range dc.NFS {
			n := n
			steps = append(steps, step{n.Serial, n.Serial, func() error {
				ds, err := s.createNFS(ctx, dc, n)
				if err == nil {
					nfsRefs[n.UUID] = ds
				}
				return err
			}})
		}
		for _, st := range dc.ISCSI {
			for _, lun := range st.LUNs {
				lun := lun
				steps = append(steps, step{lun.Serial, lun.Serial, func() error {
					ds, err := s.createISCSIDatastore(ctx, dc, lun)
					if err == nil {
						iscsiRefs[lun.UUID] = ds
					}
					return err
				}})
			}
		}
		for _, cl := range dc.Clusters {
			cl := cl
			refs := &ClusterRefs{Cluster: cl}
			s.Clusters = append(s.Clusters, refs)
			steps = append(steps, step{cl.Serial, cl.Serial, func() error { return s.createCluster(ctx, refs) }})
			for _, h := range cl.Hosts {
				h := h
				steps = append(steps, step{h.Serial, h.Serial, func() error { return s.createHost(ctx, refs, h) }})
				for _, d := range h.Devices {
					d := d
					if d.Claimed {
						continue
					}
					order := d.Serial
					if legacyOrder {
						order = h.Serial
					}
					steps = append(steps, step{d.Serial, order, func() error { return s.createLocalDatastore(ctx, refs, h, d) }})
				}
			}
			for _, vm := range cl.VMs {
				vm := vm
				// VMs are created in their own serial order on whichever of
				// the cluster's hosts exist at that point, so adding hosts
				// never reorders VM creation (and so never changes a VM's
				// MoRef); finishCluster then moves each VM to the host the
				// topology places it on
				steps = append(steps, step{vm.Serial, vm.Serial, func() error { return s.createVM(ctx, refs, vm) }})
			}
		}
	}
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].order < steps[j].order })
	for _, st := range steps {
		if s.registry == nil {
			if err := st.run(); err != nil {
				return err
			}
			continue
		}
		reserved, known := s.registry.MorefRange(st.serial)
		start, err := counter(ctx)
		if err != nil {
			return err
		}
		target := start
		if known {
			target = reserved.Start
		} else if high := s.registry.MorefHigh(); high > target {
			target = high
		}
		if err := setCounter(ctx, target); err != nil {
			return err
		}
		if err := st.run(); err != nil {
			return err
		}
		end, err := counter(ctx)
		if err != nil {
			return err
		}
		if known && end > reserved.End {
			return fmt.Errorf("sim: inventory step %d exceeds its reserved identity range", st.serial)
		}
		if !known {
			s.registry.SetMorefRange(st.serial, registry.MorefRange{Start: target, End: end})
		}
	}
	for _, refs := range s.Clusters {
		// Creation follows serial order; a re-added object can have a later
		// serial than objects that follow it in topology order.
		hosts := make(map[string]types.ManagedObjectReference)
		for _, ref := range refs.Hosts {
			hosts[ctx.Map.Get(ref).(*simulator.HostSystem).Name] = ref
		}
		for i, h := range refs.Cluster.Hosts {
			refs.Hosts[i] = hosts[h.Name]
		}
		vms := make(map[string]types.ManagedObjectReference)
		for _, ref := range refs.VMs {
			vms[ctx.Map.Get(ref).(*simulator.VirtualMachine).Name] = ref
		}
		for i, vm := range refs.Cluster.VMs {
			refs.VMs[i] = vms[vm.Name]
		}
		s.finishCluster(ctx, refs)
	}
	for _, dc := range s.Topology.Datacenters {
		for _, nfs := range dc.NFS {
			s.mountNFS(ctx, dc, nfs, nfsRefs[nfs.UUID])
		}
		for _, st := range dc.ISCSI {
			s.mountISCSI(ctx, st, iscsiRefs)
		}
	}
	for key, ds := range nfsRefs {
		iscsiRefs[key] = ds
	}
	s.installStoragePerformance(iscsiRefs)
	return nil
}

// counter probes the next generated reference and restores the counter. The
// probe is removed before the counter is restored.
func counter(ctx *simulator.Context) (int64, error) {
	probe := &mo.ManagedEntity{}
	ctx.Map.Put(probe)
	n, err := strconv.ParseInt(strings.TrimPrefix(probe.Self.Value, "managedentity-"), 10, 64)
	ctx.Map.Remove(ctx, probe.Self)
	if err != nil {
		return 0, fmt.Errorf("sim: counter probe: %w", err)
	}
	if err := setCounter(ctx, n-1); err != nil {
		return 0, err
	}
	return n - 1, nil
}

// vcsim does not expose its allocation counter. AlignCounter cannot restore
// it because the static model contains fixed references above that counter.
// Validate the upstream layout before accessing the counter, and only do this
// during the single-threaded inventory build after each task has completed.
func setCounter(ctx *simulator.Context, n int64) error {
	field, ok := reflect.TypeOf(ctx.Map).Elem().FieldByName("counter")
	if !ok || field.Type.Kind() != reflect.Int64 || field.Offset != 0 {
		return fmt.Errorf("sim: unsupported vcsim registry counter layout")
	}
	atomic.StoreInt64((*int64)(unsafe.Pointer(ctx.Map)), n)
	return nil
}

func (s *Simulator) createDatacenter(ctx *simulator.Context, dc *topology.Datacenter) error {
	root := ctx.Map.Get(s.model.ServiceContent.RootFolder).(*simulator.Folder)
	body := root.CreateDatacenter(ctx, &types.CreateDatacenter{This: root.Self, Name: dc.Name})
	if f := body.Fault(); f != nil {
		return fmt.Errorf("sim: datacenter %s: %s", dc.Name, f.String)
	}
	ref := body.(*methods.CreateDatacenterBody).Res.Returnval
	d := ctx.Map.Get(ref).(*simulator.Datacenter)
	refs := &dcRefs{
		ref:        ref,
		dc:         d,
		hostFolder: ctx.Map.Get(d.HostFolder).(*simulator.Folder),
		vmFolder:   ctx.Map.Get(d.VmFolder).(*simulator.Folder),
		dsFolder:   ctx.Map.Get(d.DatastoreFolder).(*simulator.Folder),
		netFolder:  ctx.Map.Get(d.NetworkFolder).(*simulator.Folder),
		pgs:        map[string]*simulator.DistributedVirtualPortgroup{},
	}
	// one vDS per datacenter, with the portgroups every host attaches to
	spec := types.DVSCreateSpec{ConfigSpec: &types.VMwareDVSConfigSpec{}}
	spec.ConfigSpec.GetDVSConfigSpec().Name = dc.VDS
	tb := refs.netFolder.CreateDVSTask(ctx, &types.CreateDVS_Task{This: refs.netFolder.Self, Spec: spec})
	res, err := s.tasks.result(ctx, tb, taskOf(tb))
	if err != nil {
		return fmt.Errorf("sim: vds %s: %w", dc.VDS, err)
	}
	dvs := ctx.Map.Get(res.(types.ManagedObjectReference)).(*simulator.DistributedVirtualSwitch)
	// vcsim's CreateDVS adds the uplink portgroup through a nested task it
	// does not wait for; wait for it here so the uplink portgroup takes the
	// next MoRef every time instead of racing the objects created after it
	for i := 0; ; i++ {
		var n int
		ctx.Map.WithLock(&simulator.Context{}, dvs, func() { n = len(dvs.Portgroup) })
		if n > 0 {
			break
		}
		if i > 50000 {
			return fmt.Errorf("sim: vds %s: uplink portgroup never appeared", dc.VDS)
		}
		time.Sleep(20 * time.Microsecond)
	}
	dvs.Uuid = dc.VDSUUID
	dvs.Summary.Uuid = dc.VDSUUID
	if cfg, ok := dvs.Config.(*types.VMwareDVSConfigInfo); ok {
		cfg.Uuid = dc.VDSUUID
	}
	var pgSpecs []types.DVPortgroupConfigSpec
	for _, pg := range topology.Portgroups {
		pgSpecs = append(pgSpecs, types.DVPortgroupConfigSpec{
			Name: pg, Type: string(types.DistributedVirtualPortgroupPortgroupTypeEarlyBinding), NumPorts: 128,
		})
	}
	tb = dvs.AddDVPortgroupTask(ctx, &types.AddDVPortgroup_Task{This: dvs.Self, Spec: pgSpecs})
	if _, err := s.tasks.result(ctx, tb, taskOf(tb)); err != nil {
		return fmt.Errorf("sim: portgroups: %w", err)
	}
	refs.dvs = dvs
	for _, ref := range dvs.Portgroup {
		pg := ctx.Map.Get(ref).(*simulator.DistributedVirtualPortgroup)
		refs.pgs[pg.Name] = pg
	}
	for _, name := range topology.Portgroups {
		if refs.pgs[name] == nil {
			return fmt.Errorf("sim: portgroup %s not created", name)
		}
	}
	s.dcs[dc] = refs
	return nil
}

// The vDS is served as vcsim's base managed object type,
// DistributedVirtualSwitch, not VmwareDistributedVirtualSwitch as a real
// vCenter reports it: vcsim applies property updates to the switch by the
// field layout of the registered type, so re-registering the object under
// the VMware type breaks every later update to it. Container views by the
// base type (govc find -type DistributedVirtualSwitch) see it; a client
// asking for the VMware type by name does not. A known gap.

// taskOf extracts the task reference from a *_Task method body.
func taskOf(body soap.HasFault) types.ManagedObjectReference {
	switch b := body.(type) {
	case *methods.CreateDVS_TaskBody:
		if b.Res != nil {
			return b.Res.Returnval
		}
	case *methods.AddDVPortgroup_TaskBody:
		if b.Res != nil {
			return b.Res.Returnval
		}
	case *methods.ReconfigureDvs_TaskBody:
		if b.Res != nil {
			return b.Res.Returnval
		}
	case *methods.AddHost_TaskBody:
		if b.Res != nil {
			return b.Res.Returnval
		}
	case *methods.CreateVM_TaskBody:
		if b.Res != nil {
			return b.Res.Returnval
		}
	}
	return types.ManagedObjectReference{}
}

func (s *Simulator) createCluster(ctx *simulator.Context, refs *ClusterRefs) error {
	cl := refs.Cluster
	dc := s.dcs[cl.Datacenter]
	body := dc.hostFolder.CreateClusterEx(ctx, &types.CreateClusterEx{This: dc.hostFolder.Self, Name: cl.Name, Spec: types.ClusterConfigSpecEx{}})
	if f := body.Fault(); f != nil {
		return fmt.Errorf("sim: cluster %s: %s", cl.Key, f.String)
	}
	refs.Ref = body.(*methods.CreateClusterExBody).Res.Returnval
	obj := ctx.Map.Get(refs.Ref).(*simulator.ClusterComputeResource)
	cfg := obj.ConfigurationEx.(*types.ClusterConfigInfoEx)
	// vSAN stays disabled in vcsim's eyes while hosts are added so vcsim
	// does not create and share its own "vsanDatastore"; the cluster's own
	// datastore is created here and mounted on each host, and the enabled
	// flag is set in finishCluster.
	cfg.VsanConfigInfo = &types.VsanClusterConfigInfo{
		Enabled:        types.NewBool(false),
		DefaultConfig:  &types.VsanClusterConfigInfoHostDefaultInfo{Uuid: cl.VsanUUID, AutoClaimStorage: types.NewBool(false)},
		VsanEsaEnabled: types.NewBool(cl.Spec.Architecture != "osa"),
	}
	shapeClusterConfig(cfg, cl)
	s.Env.RegisterCluster(refs.Ref, cl)

	ds, err := s.createDatastore(ctx, dc, cl.Datastore, vsanDatastoreInfo(cl), string(types.HostFileSystemVolumeFileSystemTypeVsan))
	if err != nil {
		return err
	}
	ds.Capability = vsanDatastoreCapability
	refs.Datastore = ds.Self
	ctx.Map.AddReference(ctx, obj, &obj.Datastore, ds.Self)
	return nil
}

// createDatastore makes a datastore entity in the datacenter's datastore
// folder with a backing directory for vcsim's file operations.
func (s *Simulator) createDatastore(ctx *simulator.Context, dc *dcRefs, name string, info types.BaseDatastoreInfo, kind string) (*simulator.Datastore, error) {
	dir := filepath.Join(s.home, strings.NewReplacer("/", "_", " ", "_", "(", "", ")", "").Replace(dc.ref.Value+"-"+name))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ds := &simulator.Datastore{}
	ds.Name = name
	base := info.GetDatastoreInfo()
	base.Name = name
	base.Url = dir
	ds.Info = info
	ds.Summary.Type = kind
	ds.Summary.Name = name
	ds.Summary.Url = dir
	ds.Summary.MaintenanceMode = string(types.DatastoreSummaryMaintenanceModeStateNormal)
	ds.Summary.Accessible = true
	ds.Summary.MultipleHostAccess = types.NewBool(kind == string(types.HostFileSystemVolumeFileSystemTypeVsan))
	folder := dc.dsFolder
	ctx.Map.PutEntity(folder, ds)
	ds.Summary.Datastore = &ds.Self
	ctx.Map.AppendReference(ctx, folder, &folder.ChildEntity, ds.Self)
	// the datacenter's datastore list, which vcsim's VM creation resolves
	// "[name] path" against
	ctx.Map.AddReference(ctx, dc.dc, &dc.dc.Datastore, ds.Self)
	return ds, nil
}

// mount attaches a datastore to a host the way vcsim's HostDatastoreSystem
// does, with the mount path a real host reports.
func mount(ctx *simulator.Context, hs *simulator.HostSystem, ds *simulator.Datastore, path string) {
	ds.Host = append(ds.Host, types.DatastoreHostMount{
		Key: hs.Self,
		MountInfo: types.HostMountInfo{
			Path:       path,
			AccessMode: string(types.HostMountModeReadWrite),
			Mounted:    types.NewBool(true),
			Accessible: types.NewBool(true),
		},
	})
	dss := ctx.Map.Get(*hs.ConfigManager.DatastoreSystem).(*simulator.HostDatastoreSystem)
	dss.Datastore = append(dss.Datastore, ds.Self)
	hs.Datastore = dss.Datastore
}

func (s *Simulator) createHost(ctx *simulator.Context, refs *ClusterRefs, h *topology.Host) error {
	cl := refs.Cluster
	dc := s.dcs[cl.Datacenter]
	ccr := ctx.Map.Get(refs.Ref).(*simulator.ClusterComputeResource)
	tb := ccr.AddHostTask(ctx, &types.AddHost_Task{This: refs.Ref, Spec: types.HostConnectSpec{HostName: h.Name}, AsConnected: true})
	res, err := s.tasks.result(ctx, tb, taskOf(tb))
	if err != nil {
		return fmt.Errorf("sim: host %s: %w", h.Name, err)
	}
	ref := res.(types.ManagedObjectReference)
	hs := ctx.Map.Get(ref).(*simulator.HostSystem)
	hs.Name = h.Name
	hs.Summary.Config.Name = h.Name
	hs.Summary.Hardware.Uuid = h.HardwareUUID
	hs.Hardware.SystemInfo.Uuid = h.HardwareUUID
	shapeHost(hs, cl, h, s.Catalog)

	// vDS membership, through the real reconfigure path so the vDS and its
	// portgroups list the host
	tb = dc.dvs.ReconfigureDvsTask(ctx, &types.ReconfigureDvs_Task{This: dc.dvs.Self, Spec: &types.DVSConfigSpec{
		Host: []types.DistributedVirtualSwitchHostMemberConfigSpec{{Operation: string(types.ConfigSpecOperationAdd), Host: ref}},
	}})
	if _, err := s.tasks.result(ctx, tb, taskOf(tb)); err != nil {
		return fmt.Errorf("sim: host %s vds: %w", h.Name, err)
	}
	shapeNetwork(hs, dc.dvs, dc.pgs, h)

	sd := storageDevice(h)
	hs.Config.StorageDevice = sd
	if ss, ok := ctx.Map.Get(*hs.ConfigManager.StorageSystem).(*simulator.HostStorageSystem); ok {
		ss.StorageDeviceInfo = sd
	}

	vs := newHostVsanSystem(ref, cl, h)
	ctx.Map.Put(vs)
	vref := vs.Self
	hs.ConfigManager.VsanSystem = &vref

	// datastores: vcsim mounted its default local datastore on the host
	// (named after the host); replace it with the cluster's vSAN datastore
	// and one local VMFS datastore per unclaimed device
	dss := ctx.Map.Get(*hs.ConfigManager.DatastoreSystem).(*simulator.HostDatastoreSystem)
	for _, old := range dss.Datastore {
		if ds, ok := ctx.Map.Get(old).(*simulator.Datastore); ok {
			removeDatastore(ctx, dc, hs, ds)
		}
	}
	dss.Datastore = nil
	hs.Datastore = nil
	vsan := ctx.Map.Get(refs.Datastore).(*simulator.Datastore)
	mount(ctx, hs, vsan, "/vmfs/volumes/vsan:"+cl.ContainerID)
	hs.Config.FileSystemVolume = &types.HostFileSystemVolumeInfo{
		VolumeTypeList: []string{"VMFS", "NFS", "NFS41", "vsan", "VVOL", "VFFS", "OTHER", "PMEM"},
		MountInfo:      []types.HostFileSystemMountInfo{vsanMount(cl)},
	}

	s.hosts[h] = hs
	refs.Hosts = append(refs.Hosts, ref)
	s.Env.RegisterHost(ref, cl, h)
	return nil
}

// createLocalDatastore makes the local VMFS datastore of one unclaimed
// device and mounts it on the device's host. It is its own inventory step so
// a device added later takes MoRefs from fresh space instead of growing the
// host's reserved range.
func (s *Simulator) createLocalDatastore(ctx *simulator.Context, refs *ClusterRefs, h *topology.Host, d *topology.Device) error {
	cl := refs.Cluster
	dc := s.dcs[cl.Datacenter]
	hs := s.hosts[h]
	ccr := ctx.Map.Get(refs.Ref).(*simulator.ClusterComputeResource)
	name := fmt.Sprintf("local-%s-%s", cl.Name, strings.SplitN(h.Name, ".", 2)[0])
	if d.Index > 0 {
		name = fmt.Sprintf("%s-%d", name, d.Index)
	}
	ds, err := s.createDatastore(ctx, dc, name, vmfsDatastoreInfo(name, d), string(types.HostFileSystemVolumeFileSystemTypeVMFS))
	if err != nil {
		return err
	}
	ds.Capability = vmfsDatastoreCapability
	ds.IormConfiguration = iorm
	shapeVMFSDatastoreSummary(ds, d)
	mount(ctx, hs, ds, "/vmfs/volumes/"+d.VMFSUUID)
	ctx.Map.AddReference(ctx, ccr, &ccr.Datastore, ds.Self)
	fsv := hs.Config.FileSystemVolume
	fsv.MountInfo = append(fsv.MountInfo, vmfsMount(name, d))
	return nil
}

// removeDatastore detaches and deletes a datastore vcsim created for a host.
func removeDatastore(ctx *simulator.Context, dc *dcRefs, hs *simulator.HostSystem, ds *simulator.Datastore) {
	var hosts []types.DatastoreHostMount
	for _, m := range ds.Host {
		if m.Key != hs.Self {
			hosts = append(hosts, m)
		}
	}
	ds.Host = hosts
	if len(hosts) == 0 {
		ctx.Map.RemoveReference(ctx, dc.dsFolder, &dc.dsFolder.ChildEntity, ds.Self)
		ctx.Map.RemoveReference(ctx, dc.dc, &dc.dc.Datastore, ds.Self)
		if parent := hs.Parent; parent != nil {
			if cr, ok := ctx.Map.Get(*parent).(*simulator.ClusterComputeResource); ok {
				ctx.Map.RemoveReference(ctx, cr, &cr.Datastore, ds.Self)
			}
		}
		ctx.Map.Remove(ctx, ds.Self)
	}
}

func (s *Simulator) createVM(ctx *simulator.Context, refs *ClusterRefs, vm *topology.VM) error {
	cl := refs.Cluster
	dc := s.dcs[cl.Datacenter]
	ccr := ctx.Map.Get(refs.Ref).(*simulator.ClusterComputeResource)
	pool := *ccr.ResourcePool
	if len(refs.Hosts) == 0 {
		return fmt.Errorf("sim: create %s: cluster %s has no host yet", vm.Name, cl.Key)
	}
	// provisional placement on a host that already exists; the final host
	// is set in finishCluster
	hostRef := refs.Hosts[vm.Host%len(refs.Hosts)]
	config := types.VirtualMachineConfigSpec{
		Name:    vm.Name,
		GuestId: string(types.VirtualMachineGuestOsIdentifierOtherGuest),
		Files: &types.VirtualMachineFileInfo{
			VmPathName: fmt.Sprintf("[%s] %s/%s.vmx", cl.Datastore, vm.NamespaceUUID, vm.Name),
		},
		NumCPUs:  2,
		MemoryMB: 4096,
		Version:  "vmx-21",
	}
	var devices object.VirtualDeviceList
	scsi, err := devices.CreateSCSIController("pvscsi")
	if err != nil {
		return err
	}
	devices = append(devices, scsi)
	for _, d := range vm.Disks {
		disk := devices.CreateDisk(scsi.(types.BaseVirtualController), refs.Datastore, fmt.Sprintf("[%s] %s", cl.Datastore, d.Path))
		disk.CapacityInKB = d.CapacityKB
		devices = append(devices, disk)
	}
	pg := dc.pgs["pg-vm"]
	backing := &types.VirtualEthernetCardDistributedVirtualPortBackingInfo{
		Port: types.DistributedVirtualSwitchPortConnection{SwitchUuid: dc.dvs.Uuid, PortgroupKey: pg.Key},
	}
	nic, err := devices.CreateEthernetCard("vmxnet3", backing)
	if err != nil {
		return err
	}
	devices = append(devices, nic)
	config.DeviceChange, err = devices.ConfigSpec(types.VirtualDeviceConfigSpecOperationAdd)
	if err != nil {
		return err
	}
	tb := dc.vmFolder.CreateVMTask(ctx, &types.CreateVM_Task{This: dc.vmFolder.Self, Config: config, Pool: pool, Host: &hostRef})
	res, err := s.tasks.result(ctx, tb, taskOf(tb))
	if err != nil {
		return fmt.Errorf("sim: create %s: %w", vm.Name, err)
	}
	ref := res.(types.ManagedObjectReference)
	obj := ctx.Map.Get(ref).(*simulator.VirtualMachine)
	obj.Config.Uuid = vm.UUID
	obj.Config.InstanceUuid = vm.InstanceUUID
	obj.Summary.Config.Uuid = vm.UUID
	obj.Summary.Config.InstanceUuid = vm.InstanceUUID
	shapeVM(obj, cl, vm, s.Env.Now)
	refs.VMs = append(refs.VMs, ref)
	return nil
}

// finishCluster turns vSAN on once the hosts and VMs exist, fills the
// per-host vSAN config list and the cluster summary, and gives the
// datastore summary its vSAN-shaped URL. Info.Url stays the backing
// directory because vcsim's file operations use it.
func (s *Simulator) finishCluster(ctx *simulator.Context, refs *ClusterRefs) {
	cl := refs.Cluster
	obj := ctx.Map.Get(refs.Ref).(*simulator.ClusterComputeResource)
	cfg := obj.ConfigurationEx.(*types.ClusterConfigInfoEx)
	cfg.VsanConfigInfo.Enabled = types.NewBool(true)
	cfg.VsanHostConfig = nil
	for i, ref := range refs.Hosts {
		h := cl.Hosts[i]
		hs := ctx.Map.Get(ref).(*simulator.HostSystem)
		hc := vsanHostConfig(ref, cl, h)
		cfg.VsanHostConfig = append(cfg.VsanHostConfig, hc)
		hs.Config.VsanHostConfig = &hc
		shapeHostProduct(hs, s.Catalog)
		shapeHostRuntime(hs, cl, h, s.Env.Now)
	}
	for i, vref := range refs.VMs {
		relocateVM(ctx, vref, refs.Hosts[cl.VMs[i].Host])
	}
	shapeClusterSummary(obj, cl, refs)
	ds := ctx.Map.Get(refs.Datastore).(*simulator.Datastore)
	ds.Summary.Url = fmt.Sprintf("ds:///vmfs/volumes/vsan:%s/", cl.ContainerID)
	shapeDatastoreSummary(ds, cl, refs)
}

// relocateVM moves a VM to its final host: runtime.host and the two hosts'
// vm lists. Creation order, and so the VM's MoRef, is unaffected.
func relocateVM(ctx *simulator.Context, vref, hostRef types.ManagedObjectReference) {
	vm := ctx.Map.Get(vref).(*simulator.VirtualMachine)
	if vm.Runtime.Host != nil && *vm.Runtime.Host == hostRef {
		return
	}
	if vm.Runtime.Host != nil {
		if old, ok := ctx.Map.Get(*vm.Runtime.Host).(*simulator.HostSystem); ok {
			ctx.Map.RemoveReference(ctx, old, &old.Vm, vref)
		}
	}
	host := ctx.Map.Get(hostRef).(*simulator.HostSystem)
	ctx.Map.AppendReference(ctx, host, &host.Vm, vref)
	ref := hostRef
	vm.Runtime.Host = &ref
	vm.Summary.Runtime.Host = &ref
}

// handler serves the service version documents from the capture and
// everything else from vcsim. Before dispatch it applies the scenario's
// /sdk latency and the /vsanHealth latency and timeout holds (outside
// vcsim's per-object lock, so one cluster's hold never stalls another),
// and with configured users it refuses the token and certificate logins
// vcsim would otherwise accept from anyone.
type handler struct {
	mux         *http.ServeMux
	cat         *catalog.Catalog
	env         *vsanhealth.Env
	strictLogin bool
	rest        restSessions
}

// otherLogins are vcsim's alternative login methods: LoginByToken takes
// any unsigned SAML assertion and LoginExtensionByCertificate any client
// certificate the listener would present, neither of which a vCenter with
// real users accepts from a stranger.
var otherLogins = regexp.MustCompile(`<(?:[A-Za-z0-9_]+:)?(LoginByToken|LoginBySSPI|LoginExtensionByCertificate|LoginExtensionBySubjectName)`)

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.rest.serve(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/sdk", "/sdk/":
			http.NotFound(w, r)
			return
		case "/vsanHealth", "/vsanHealth/", "/vsanHealth/vsanServiceVersions.xml":
			w.WriteHeader(http.StatusNotImplemented)
			return
		case "/sdk/vimService.wsdl":
			w.Header().Set("Content-Type", "text/xml; charset=UTF-8")
			_, _ = fmt.Fprint(w, vimServiceWSDL)
			return
		}
	}
	switch r.URL.Path {
	case "/sdk/vimServiceVersions.xml":
		w.Header().Set("Content-Type", "text/xml; charset=UTF-8")
		fmt.Fprint(w, h.cat.VimServiceVersions)
	case "/sdk/vsanServiceVersions.xml":
		w.Header().Set("Content-Type", "text/xml; charset=UTF-8")
		fmt.Fprint(w, h.cat.VsanServiceVersions)
	default:
		if r.Method == http.MethodPost && (strings.HasPrefix(r.URL.Path, "/sdk") || strings.HasPrefix(r.URL.Path, "/vsanHealth")) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			switch {
			case strings.HasPrefix(r.URL.Path, "/sdk"):
				if h.strictLogin && otherLogins.Match(body) {
					invalidLogin(w)
					return
				}
				if d := h.env.Knobs.SDKDelay(); d != nil {
					time.Sleep(d.Duration(rand.Float64()))
				}
			default:
				if hold := h.env.Hold(body); hold > 0 {
					time.Sleep(hold)
				}
			}
		}
		h.mux.ServeHTTP(w, r)
	}
}

// invalidLogin answers a refused login the way vcsim's Login does.
func invalidLogin(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><soapenv:Body><soapenv:Fault><faultcode>ServerFaultCode</faultcode><faultstring>Login failure</faultstring><detail><InvalidLoginFault xmlns="urn:vim25" xsi:type="InvalidLogin"></InvalidLoginFault></detail></soapenv:Fault></soapenv:Body></soapenv:Envelope>`)
}
