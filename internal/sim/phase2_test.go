package sim_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vsan"
	vsanmethods "github.com/vmware/govmomi/vsan/methods"
	vsantypes "github.com/vmware/govmomi/vsan/types"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/sentania/vcenter-api-simulator/internal/vsanhealth"
)

// tenClusters is the acceptance scenario without its latency knobs.
func tenClusters(t *testing.T) *scenario.Scenario {
	t.Helper()
	s, err := scenario.Load("../../examples/scenarios/ten-clusters.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s.Latency = scenario.Latency{}
	return s
}

func startWith(t *testing.T, topo *topology.Topology, opts sim.Options) *env {
	t.Helper()
	opts.Topology = topo
	if opts.Now == nil {
		opts.Now = func() time.Time { return captureTime }
	}
	s, err := sim.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s.Handler)
	t.Cleanup(srv.Close)
	return &env{t: t, sim: s, srv: srv}
}

func (e *env) login(user, pass string) error {
	u, _ := soap.ParseURL(e.srv.URL + "/sdk")
	u.User = nil
	sc := soap.NewClient(u, true)
	c, err := vim25.NewClient(context.Background(), sc)
	if err != nil {
		return err
	}
	if err := session.NewManager(c).Login(context.Background(), url.UserPassword(user, pass)); err != nil {
		return err
	}
	e.client = c
	for _, ck := range sc.Jar.Cookies(u) {
		if ck.Name == soap.SessionCookieName {
			e.cookie = ck.Value
		}
	}
	return nil
}

// identities is everything the management pack and the native adapter key
// on, by logical key, read back through /sdk and /vsanHealth.
func identities(t *testing.T, e *env) map[string]string {
	t.Helper()
	ctx := context.Background()
	pc := property.DefaultCollector(e.client)
	out := map[string]string{"vcenter": e.client.ServiceContent.About.InstanceUuid}
	vc, err := vsan.NewClient(ctx, e.client)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range e.sim.Clusters {
		var cl mo.ClusterComputeResource
		if err := pc.RetrieveOne(ctx, c.Ref, []string{"name", "host", "configurationEx", "datastore"}, &cl); err != nil {
			t.Fatal(err)
		}
		out[c.Cluster.Key] = cl.Reference().Value + " " + cl.ConfigurationEx.(*types.ClusterConfigInfoEx).VsanConfigInfo.DefaultConfig.Uuid
		cfg, err := vc.VsanClusterGetConfig(ctx, c.Ref)
		if err != nil {
			t.Fatal(err)
		}
		out[c.Cluster.Key+"/vsanHealth"] = cfg.DefaultConfig.Uuid
		for _, href := range cl.Host {
			var hs mo.HostSystem
			if err := pc.RetrieveOne(ctx, href, []string{"name", "summary.hardware.uuid", "configManager.vsanSystem", "config.vsanHostConfig"}, &hs); err != nil {
				t.Fatal(err)
			}
			var vs mo.HostVsanSystem
			if err := pc.RetrieveOne(ctx, *hs.ConfigManager.VsanSystem, []string{"config"}, &vs); err != nil {
				t.Fatal(err)
			}
			out[hs.Name] = href.Value + " " + hs.Summary.Hardware.Uuid + " " + vs.Config.ClusterInfo.NodeUuid + " " + hs.Config.VsanHostConfig.ClusterInfo.NodeUuid
			dres, err := methods.QueryDisksForVsan(ctx, e.client, &types.QueryDisksForVsan{This: *hs.ConfigManager.VsanSystem})
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range dres.Returnval {
				out[hs.Name+"/"+d.Disk.CanonicalName] = d.Disk.Uuid + " " + d.VsanUuid
			}
		}
		for _, vref := range c.VMs {
			var vm mo.VirtualMachine
			if err := pc.RetrieveOne(ctx, vref, []string{"name", "config.uuid", "config.instanceUuid", "config.hardware.device"}, &vm); err != nil {
				t.Fatal(err)
			}
			var disks []string
			for _, dev := range vm.Config.Hardware.Device {
				if d, ok := dev.(*types.VirtualDisk); ok {
					disks = append(disks, d.Backing.(*types.VirtualDiskFlatVer2BackingInfo).FileName)
				}
			}
			out[vm.Name] = vref.Value + " " + vm.Config.Uuid + " " + vm.Config.InstanceUuid + " " + strings.Join(disks, ",")
		}
		// one metric value per family, cluster-scoped and host-scoped
		for _, fam := range []string{"cluster-domclient", "host-domclient"} {
			_, body := e.postPerf(c.Ref, fam)
			out[c.Cluster.Key+"/"+fam] = string(body)
		}
	}
	return out
}

func (e *env) postPerf(cluster types.ManagedObjectReference, family string) (int, []byte) {
	e.t.Helper()
	req := `<?xml version="1.0" encoding="UTF-8"?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body><VsanPerfQueryPerf xmlns="urn:vsan"><_this type="VsanPerformanceManager">vsan-performance-manager</_this><querySpecs><entityRefId>` + family + `:*</entityRefId><startTime>2026-11-06T23:23:22.340Z</startTime><endTime>2026-11-06T23:38:22.340Z</endTime></querySpecs><cluster type="ClusterComputeResource">` + cluster.Value + `</cluster></VsanPerfQueryPerf></soapenv:Body></soapenv:Envelope>`
	return e.post("/vsanHealth", req)
}

// Ten clusters with different host, VM and device counts: every cluster's
// identities hold on both surfaces, every entity family is sized by its
// own cluster, and a per-cluster override (dedup off) applies to that
// cluster alone.
func TestMultiClusterIdentityConsistency(t *testing.T) {
	sc := tenClusters(t)
	topo := topology.Generate(sc, registry.InMemory(sc.Seed))
	e := startWith(t, topo, sim.Options{})
	if err := e.login("administrator@vsphere.local", "VMware1!"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if len(e.sim.Clusters) != 10 {
		t.Fatalf("%d clusters", len(e.sim.Clusters))
	}
	v, err := view.NewManager(e.client).CreateContainerView(ctx, e.client.ServiceContent.RootFolder, []string{"ClusterComputeResource"}, true)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := v.Find(ctx, []string{"ClusterComputeResource"}, nil)
	if err != nil || len(refs) != 10 {
		t.Fatalf("container view: %d clusters, %v", len(refs), err)
	}
	hostCounts := map[int]bool{}
	vc, err := vsan.NewClient(ctx, e.client)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range e.sim.Clusters {
		cl := c.Cluster
		hostCounts[len(cl.Hosts)] = true
		var mcl mo.ClusterComputeResource
		if err := property.DefaultCollector(e.client).RetrieveOne(ctx, c.Ref, []string{"name", "host", "configurationEx", "datastore", "summary"}, &mcl); err != nil {
			t.Fatal(err)
		}
		cfg := mcl.ConfigurationEx.(*types.ClusterConfigInfoEx)
		if mcl.Name != cl.Name || len(mcl.Host) != len(cl.Hosts) || cfg.VsanConfigInfo.DefaultConfig.Uuid != cl.VsanUUID || !*cfg.VsanConfigInfo.Enabled {
			t.Errorf("%s: /sdk cluster %q hosts %d vsan %q enabled %v", cl.Key, mcl.Name, len(mcl.Host), cfg.VsanConfigInfo.DefaultConfig.Uuid, *cfg.VsanConfigInfo.Enabled)
		}
		if len(cfg.VsanHostConfig) != len(cl.Hosts) || !*cfg.DasConfig.Enabled || !*cfg.DrsConfig.Enabled {
			t.Errorf("%s: vsanHostConfig %d, HA %v, DRS %v", cl.Key, len(cfg.VsanHostConfig), *cfg.DasConfig.Enabled, *cfg.DrsConfig.Enabled)
		}
		sum := mcl.Summary.(*types.ClusterComputeResourceSummary)
		if sum.NumHosts != int32(len(cl.Hosts)) || sum.UsageSummary.TotalVmCount != int32(len(cl.VMs)) {
			t.Errorf("%s: summary hosts %d vms %d", cl.Key, sum.NumHosts, sum.UsageSummary.TotalVmCount)
		}
		vcfg, err := vc.VsanClusterGetConfig(ctx, c.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if vcfg.DefaultConfig.Uuid != cl.VsanUUID || vcfg.DataEfficiencyConfig.DedupEnabled != cl.Dedup {
			t.Errorf("%s: /vsanHealth uuid %q dedup %v", cl.Key, vcfg.DefaultConfig.Uuid, vcfg.DataEfficiencyConfig.DedupEnabled)
		}
		nodes, err := vsanmethods.VsanPerfQueryNodeInformation(ctx, vc.Client, &vsantypes.VsanPerfQueryNodeInformation{This: vsanhealth.PerfManagerRef, Cluster: &c.Ref})
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes.Returnval) != len(cl.Hosts) {
			t.Errorf("%s: %d nodes", cl.Key, len(nodes.Returnval))
		}
		// the cluster's own vSAN datastore, mounted on exactly its hosts
		var ds mo.Datastore
		if err := property.DefaultCollector(e.client).RetrieveOne(ctx, c.Datastore, []string{"name", "host", "summary", "info", "vm"}, &ds); err != nil {
			t.Fatal(err)
		}
		if ds.Name != cl.Datastore || len(ds.Host) != len(cl.Hosts) || ds.Summary.Type != "vsan" || len(ds.Vm) != len(cl.VMs) {
			t.Errorf("%s: datastore %q hosts %d type %q vms %d", cl.Key, ds.Name, len(ds.Host), ds.Summary.Type, len(ds.Vm))
		}
		if !strings.HasPrefix(ds.Summary.Url, "ds:///vmfs/volumes/vsan:"+cl.ContainerID) || ds.Summary.Capacity <= ds.Summary.FreeSpace || ds.Summary.FreeSpace <= 0 {
			t.Errorf("%s: datastore url %q capacity %d free %d", cl.Key, ds.Summary.Url, ds.Summary.Capacity, ds.Summary.FreeSpace)
		}
		// entity cardinality follows this cluster, not another
		want := map[string]int{
			"host-domclient":      len(cl.Hosts),
			"host-cpu":            len(cl.Hosts) * (cl.Spec.Host.Threads + 1),
			"dom-world-cpu":       len(cl.Hosts) * scenario.Int(cl.Spec.Host.DomWorlds),
			"vsan-pnic-net":       len(cl.Hosts) * cl.Spec.Host.VsanPnics,
			"vsan-esa-disk-layer": len(cl.Hosts) * cl.Spec.Host.ClaimedDevices,
			"virtual-machine":     len(cl.VMs),
			"vscsi":               len(cl.VMs) * cl.Spec.VMs.DisksPerVM,
		}
		for fam, n := range want {
			_, body := e.postPerf(c.Ref, fam)
			got := len(refRe.FindAllSubmatch(body, -1))
			if got != n {
				t.Errorf("%s: %s has %d entities, want %d", cl.Key, fam, got, n)
			}
		}
		_, body := e.postPerf(c.Ref, "cluster-esa-dedup-domclient-io")
		hasData := bytes.Contains(body, []byte("<values>"))
		if hasData != cl.Dedup {
			t.Errorf("%s: dedup family data %v, dedup %v", cl.Key, hasData, cl.Dedup)
		}
	}
	if len(hostCounts) < 3 {
		t.Errorf("host counts not varied: %v", hostCounts)
	}
	c3 := e.sim.Cluster("dc-01/cluster-03")
	if c3 == nil || len(c3.Cluster.Hosts) != 16 || c3.Cluster.Dedup {
		t.Errorf("override: %+v", c3)
	}
	// every VM in the inventory belongs to one cluster's datastore
	all, err := view.NewManager(e.client).CreateContainerView(ctx, e.client.ServiceContent.RootFolder, []string{"VirtualMachine"}, true)
	if err != nil {
		t.Fatal(err)
	}
	vms, _ := all.Find(ctx, []string{"VirtualMachine"}, nil)
	_, _, nvms, _ := topo.Counts()
	if len(vms) != nvms {
		t.Errorf("%d VMs in the inventory, topology has %d", len(vms), nvms)
	}
}

// A restart with the same registry reproduces every MoRef, uuid and metric
// value; a scenario edit (more clusters) keeps every existing identity.
func TestRestartKeepsIdentities(t *testing.T) {
	sc := tenClusters(t)
	path := filepath.Join(t.TempDir(), "registry.json")
	r1, err := registry.Open(path, sc.Seed, false)
	if err != nil {
		t.Fatal(err)
	}
	e1 := startWith(t, topology.Generate(sc, r1), sim.Options{})
	if err := r1.Save(); err != nil {
		t.Fatal(err)
	}
	if err := e1.login("administrator@vsphere.local", "VMware1!"); err != nil {
		t.Fatal(err)
	}
	before := identities(t, e1)
	e1.srv.Close()
	e1.sim.Close()

	r2, err := registry.Open(path, sc.Seed, false)
	if err != nil {
		t.Fatal(err)
	}
	e2 := startWith(t, topology.Generate(sc, r2), sim.Options{})
	if err := e2.login("administrator@vsphere.local", "VMware1!"); err != nil {
		t.Fatal(err)
	}
	after := identities(t, e2)
	if len(before) != len(after) || len(before) < 500 {
		t.Fatalf("%d identities before, %d after", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("restart changed %s:\n before %s\n after  %s", k, v, after[k])
		}
	}

	// edit: an eleventh cluster; everything that existed keeps its ids
	sc.Datacenters[0].Clusters = 11
	r3, err := registry.Open(path, sc.Seed, false)
	if err != nil {
		t.Fatal(err)
	}
	e3 := startWith(t, topology.Generate(sc, r3), sim.Options{})
	if err := e3.login("administrator@vsphere.local", "VMware1!"); err != nil {
		t.Fatal(err)
	}
	edited := identities(t, e3)
	for k, v := range before {
		if edited[k] != v {
			t.Errorf("edit changed %s:\n before %s\n after  %s", k, v, edited[k])
		}
	}
	if len(e3.sim.Clusters) != 11 {
		t.Errorf("%d clusters after the edit", len(e3.sim.Clusters))
	}
}

// Configured credentials are enforced on /sdk, and /vsanHealth shares the
// session: no session, no answer.
func TestAuth(t *testing.T) {
	topo := topology.Phase1("auth")
	e := startWith(t, topo, sim.Options{Credentials: map[string]string{"operator@vsphere.local": "s3cret"}})
	err := e.login("operator@vsphere.local", "wrong")
	if err == nil || !strings.Contains(err.Error(), "InvalidLogin") && !strings.Contains(err.Error(), "Login failure") {
		t.Fatalf("wrong password: %v", err)
	}
	if err := e.login("nobody@vsphere.local", "s3cret"); err == nil {
		t.Fatal("unknown user logged in")
	}
	if err := e.login("operator@vsphere.local", "s3cret"); err != nil {
		t.Fatal(err)
	}
	status, body := e.postPerf(e.cl().Ref, "cluster-domclient")
	if status != 200 || !bytes.Contains(body, []byte("<values>")) {
		t.Fatalf("logged in: status %d %s", status, body)
	}
	saved := e.cookie
	e.cookie = "not-a-session"
	status, body = e.postPerf(e.cl().Ref, "cluster-domclient")
	if status != 500 || !bytes.Contains(body, []byte("NotAuthenticated")) {
		t.Fatalf("bogus cookie: status %d %s", status, body)
	}
	e.cookie = saved

	// the default: any user name and password
	open := startWith(t, topology.Phase1("open"), sim.Options{})
	if err := open.login("anyone", "anything"); err != nil {
		t.Fatal(err)
	}
}

// Injected faults are the fault shapes the pack distinguishes, selected by
// family or method, by cluster, and by a bucket-aligned time window.
func TestFaultInjection(t *testing.T) {
	sc := tenClusters(t)
	from := captureTime.Add(-time.Hour)
	to := captureTime.Add(time.Hour)
	past := captureTime.Add(-2 * time.Hour)
	sc.Faults = []scenario.Fault{
		{Family: "vsan-pnic-net", Clusters: []string{"dc-01/cluster-02"}, Fault: scenario.FaultInvalidArgument},
		{Family: "host-domowner", Fault: scenario.FaultSystemError, From: &from, To: &to},
		{Family: "vsan-cpu", Fault: scenario.FaultNotSupported, From: &past, To: &from},
		{Method: "QueryVsanManagedDisks", Clusters: []string{"dc-01/cluster-04"}, Fault: scenario.FaultSystemError},
	}
	topo := topology.Generate(sc, registry.InMemory(sc.Seed))
	e := startWith(t, topo, sim.Options{Faults: sc.Faults})
	if err := e.login("administrator@vsphere.local", "VMware1!"); err != nil {
		t.Fatal(err)
	}
	c2, c4 := e.sim.Cluster("dc-01/cluster-02"), e.sim.Cluster("dc-01/cluster-04")
	check := func(name string, status int, body []byte, fault string) {
		t.Helper()
		got := parseShape(t, body)
		if got.fault != fault || (fault != "" && status != 500) || (fault == "" && status != 200) {
			t.Errorf("%s: status %d fault %q, want %q", name, status, got.fault, fault)
		}
	}
	s, b := e.postPerf(c2.Ref, "vsan-pnic-net")
	check("family fault on the selected cluster", s, b, "InvalidArgumentFault")
	s, b = e.postPerf(c4.Ref, "vsan-pnic-net")
	check("family fault spares other clusters", s, b, "")
	s, b = e.postPerf(c4.Ref, "host-domowner")
	check("system error inside its window", s, b, "SystemErrorFault")
	s, b = e.postPerf(c4.Ref, "vsan-cpu")
	check("fault whose window has passed", s, b, "")
	vc, _ := vsan.NewClient(context.Background(), e.client)
	for _, c := range []*sim.ClusterRefs{c2, c4} {
		_, err := vsanmethods.VsanQueryVcClusterHealthSummary(context.Background(), vc.Client, &vsantypes.VsanQueryVcClusterHealthSummary{
			This: vsanhealth.HealthSystemRef, Cluster: &c.Ref, Fields: []string{"hclInfo"},
		})
		if err != nil {
			t.Errorf("%s: health summary: %v", c.Cluster.Key, err)
		}
	}
	_, b = e.replayHost("manageddisks-"+capturedMoid+"-"+capturedHost+".req.xml", c4.Hosts[0])
	if !bytes.Contains(b, []byte("SystemErrorFault")) {
		t.Errorf("method fault on cluster-04: %s", b)
	}
	_, b = e.replayHost("manageddisks-"+capturedMoid+"-"+capturedHost+".req.xml", c2.Hosts[0])
	if bytes.Contains(b, []byte("Fault")) {
		t.Errorf("method fault leaked to cluster-02: %s", b)
	}
}

// Latency knobs delay the selected family and /sdk calls.
func TestLatency(t *testing.T) {
	topo := topology.Phase1("latency")
	e := startWith(t, topo, sim.Options{Latency: scenario.Latency{
		Families: map[string]scenario.Delay{"host-domclient": {FixedMs: 150}},
		SDK:      &scenario.Delay{FixedMs: 40},
	}})
	start := time.Now()
	if err := e.login("a", "b"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 80*time.Millisecond { // two /sdk calls at least
		t.Errorf("/sdk login took %s, the knob adds 40 ms per call", d)
	}
	start = time.Now()
	e.postPerf(e.cl().Ref, "host-domclient")
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("delayed family answered in %s", d)
	}
	start = time.Now()
	e.postPerf(e.cl().Ref, "cluster-domclient")
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("undelayed family took %s", d)
	}
}

// The inventory realism the native adapter reads: host config, datastores
// of both kinds with their mounts, VM guest and runtime.
func TestInventoryRealism(t *testing.T) {
	e := start(t, "inventory")
	ctx := context.Background()
	pc := property.DefaultCollector(e.client)
	cl := e.cl().Cluster
	for i, href := range e.cl().Hosts {
		h := cl.Hosts[i]
		var hs mo.HostSystem
		if err := pc.RetrieveOne(ctx, href, []string{"config", "hardware", "summary", "runtime", "datastore"}, &hs); err != nil {
			t.Fatal(err)
		}
		if hs.Config.VsanHostConfig == nil || hs.Config.VsanHostConfig.ClusterInfo.NodeUuid != h.NodeUUID || !*hs.Config.VsanHostConfig.VsanEsaEnabled {
			t.Errorf("%s: vsanHostConfig %+v", h.Name, hs.Config.VsanHostConfig)
		}
		if hs.Config.Product.Version != "9.1.1" || hs.Summary.Config.Product.ApiVersion != "9.1.1.0" {
			t.Errorf("%s: product %+v", h.Name, hs.Config.Product)
		}
		if hs.Hardware.CpuInfo.NumCpuThreads != int16(h.Threads) || hs.Summary.Hardware.NumCpuThreads != int16(h.Threads) || hs.Hardware.MemorySize == 0 {
			t.Errorf("%s: hardware threads %d/%d memory %d", h.Name, hs.Hardware.CpuInfo.NumCpuThreads, hs.Summary.Hardware.NumCpuThreads, hs.Hardware.MemorySize)
		}
		if hs.Runtime.ConnectionState != "connected" || hs.Runtime.PowerState != "poweredOn" || hs.Runtime.BootTime == nil || hs.Runtime.VsanRuntimeInfo == nil || len(hs.Runtime.VsanRuntimeInfo.MembershipList) != len(cl.Hosts) {
			t.Errorf("%s: runtime %+v", h.Name, hs.Runtime)
		}
		if hs.Config.FileSystemVolume == nil || len(hs.Config.FileSystemVolume.MountInfo) != 1+1 { // vsan + one local VMFS
			t.Errorf("%s: file system volumes %+v", h.Name, hs.Config.FileSystemVolume)
		}
		if len(hs.Datastore) != 2 {
			t.Errorf("%s: %d datastores", h.Name, len(hs.Datastore))
		}
		if hs.Config.DateTimeInfo == nil || hs.Config.Network.DnsConfig == nil || hs.Config.Network.DnsConfig.GetHostDnsConfig().HostName != strings.SplitN(h.Name, ".", 2)[0] {
			t.Errorf("%s: dateTime %v dns %v", h.Name, hs.Config.DateTimeInfo, hs.Config.Network.DnsConfig)
		}
		for _, dref := range hs.Datastore {
			var ds mo.Datastore
			if err := pc.RetrieveOne(ctx, dref, []string{"name", "summary", "info", "host", "iormConfiguration", "capability"}, &ds); err != nil {
				t.Fatal(err)
			}
			mounted := false
			for _, m := range ds.Host {
				if m.Key == href {
					mounted = true
					if m.MountInfo.Path == "" || !*m.MountInfo.Mounted {
						t.Errorf("%s: mount %+v", ds.Name, m.MountInfo)
					}
				}
			}
			if !mounted {
				t.Errorf("%s: %s not mounted on %s", ds.Name, dref, h.Name)
			}
			switch info := ds.Info.(type) {
			case *types.VsanDatastoreInfo:
				if info.MembershipUuid == "" || ds.Summary.Type != "vsan" || !*ds.Summary.MultipleHostAccess {
					t.Errorf("%s: vsan info %+v summary %+v", ds.Name, info, ds.Summary)
				}
			case *types.VmfsDatastoreInfo:
				if info.Vmfs == nil || info.Vmfs.Version != "6.82" || !*info.Vmfs.Local || len(info.Vmfs.Extent) != 1 || ds.Summary.Type != "VMFS" || len(ds.Host) != 1 {
					t.Errorf("%s: vmfs info %+v summary %+v hosts %d", ds.Name, info, ds.Summary, len(ds.Host))
				}
				ext := info.Vmfs.Extent[0].DiskName
				found := false
				for _, d := range h.Devices {
					found = found || (!d.Claimed && d.CanonicalName == ext)
				}
				if !found {
					t.Errorf("%s: extent %s is not one of the host's unclaimed devices", ds.Name, ext)
				}
			default:
				t.Errorf("%s: datastore info %T", ds.Name, ds.Info)
			}
			if ds.IormConfiguration == nil {
				t.Errorf("%s: no iormConfiguration", ds.Name)
			}
		}
	}
	for _, vref := range e.cl().VMs {
		var vm mo.VirtualMachine
		if err := pc.RetrieveOne(ctx, vref, []string{"name", "guest", "runtime", "summary", "config.version", "network"}, &vm); err != nil {
			t.Fatal(err)
		}
		if vm.Runtime.PowerState != "poweredOn" || vm.Guest.GuestState != "running" || vm.Guest.IpAddress == "" || vm.Guest.HostName == "" || vm.Summary.QuickStats.OverallCpuUsage == 0 {
			t.Errorf("%s: guest %+v runtime %+v", vm.Name, vm.Guest, vm.Runtime)
		}
		if vm.Config.Version != "vmx-21" || len(vm.Network) != 1 {
			t.Errorf("%s: version %q networks %d", vm.Name, vm.Config.Version, len(vm.Network))
		}
	}
}

// A scenario edit that adds hosts to a cluster (old VMs rebalance onto the
// new hosts, which are created last) and removes a host elsewhere restarts
// cleanly, and every uuid of an entity that still exists is unchanged.
func TestEditHostsThenRestart(t *testing.T) {
	sc, err := scenario.Parse([]byte(`
seed: edit-hosts
datacenters: [{name: dc-01, clusters: 2}]
defaults: {hosts: 3, vms: {perHost: 2}}
`))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	r1, _ := registry.Open(path, sc.Seed, false)
	e1 := startWith(t, topology.Generate(sc, r1), sim.Options{})
	if err := r1.Save(); err != nil {
		t.Fatal(err)
	}
	if err := e1.login("a", "b"); err != nil {
		t.Fatal(err)
	}
	before := identities(t, e1)
	e1.srv.Close()
	e1.sim.Close()

	sc.Clusters = []scenario.ClusterOverride{
		{Key: "dc-01/cluster-01", ClusterSpec: scenario.ClusterSpec{Hosts: 5}},
		{Key: "dc-01/cluster-02", ClusterSpec: scenario.ClusterSpec{Hosts: 3, Host: scenario.HostSpec{LocalDevices: scenario.IntPtr(0)}, VMs: scenario.VMSpec{PerHost: scenario.IntPtr(0)}}},
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	r2, _ := registry.Open(path, sc.Seed, false)
	topo := topology.Generate(sc, r2)
	if len(topo.Clusters()[0].Hosts) != 5 || len(topo.Clusters()[1].VMs) != 0 || len(topo.Clusters()[1].Hosts[0].Devices) != 1 {
		t.Fatalf("edit not applied: %+v", topo.Clusters()[0].Spec)
	}
	e2 := startWith(t, topo, sim.Options{})
	if err := e2.login("a", "b"); err != nil {
		t.Fatal(err)
	}
	after := identities(t, e2)
	uuidOf := func(v string) string { // drop the MoRef, keep the uuids
		parts := strings.Fields(v)
		if len(parts) > 1 {
			return strings.Join(parts[1:], " ")
		}
		return v
	}
	for k, v := range before {
		if strings.Contains(k, "/") && !strings.Contains(k, "vsanHealth") || strings.HasSuffix(k, "domclient") {
			continue // cluster lines carry hosts lists and metric bodies that legitimately change
		}
		nv, ok := after[k]
		if !ok {
			continue
		}
		if strings.HasPrefix(k, "cluster-01-vm-") {
			// an addition-only edit keeps a VM's MoRef as well as its uuids
			if nv != v {
				t.Errorf("edit changed VM %s (MoRef included):\n before %s\n after  %s", k, v, nv)
			}
			continue
		}
		if uuidOf(nv) != uuidOf(v) {
			t.Errorf("edit changed %s:\n before %s\n after  %s", k, v, nv)
		}
	}
	for _, vm := range topo.Clusters()[0].VMs {
		if vm.Host >= 3 { // placed on a new host
			if _, ok := after[vm.Name]; !ok {
				t.Errorf("%s on new host %d was not created", vm.Name, vm.Host)
			}
		}
	}
}

// A timeout fault scoped to one cluster and a family's latency never stall
// another cluster's query: holds run before dispatch, outside vcsim's lock
// on the one Performance Manager.
func TestHoldsDoNotSerializeClusters(t *testing.T) {
	sc := tenClusters(t)
	sc.Faults = []scenario.Fault{{Family: "cluster-domclient", Clusters: []string{"dc-01/cluster-04"}, Fault: scenario.FaultTimeout}}
	sc.Latency = scenario.Latency{Families: map[string]scenario.Delay{"host-domclient": {FixedMs: 800}}}
	saved := vsanhealth.TimeoutHold
	vsanhealth.TimeoutHold = 800 * time.Millisecond
	t.Cleanup(func() { vsanhealth.TimeoutHold = saved })
	topo := topology.Generate(sc, registry.InMemory(sc.Seed))
	e := startWith(t, topo, sim.Options{Faults: sc.Faults, Latency: sc.Latency})
	if err := e.login("administrator@vsphere.local", "VMware1!"); err != nil {
		t.Fatal(err)
	}
	c2, c4 := e.sim.Cluster("dc-01/cluster-02"), e.sim.Cluster("dc-01/cluster-04")
	type res struct {
		name string
		d    time.Duration
		body []byte
	}
	results := make(chan res, 3)
	run := func(name string, c *sim.ClusterRefs, family string) {
		start := time.Now()
		_, body := e.postPerf(c.Ref, family)
		results <- res{name, time.Since(start), body}
	}
	go run("timeout on cluster-04", c4, "cluster-domclient")
	go run("slow family on cluster-02", c2, "host-domclient")
	time.Sleep(50 * time.Millisecond) // let the held requests enter first
	go run("cluster-02 unaffected", c2, "cluster-domclient")
	for i := 0; i < 3; i++ {
		r := <-results
		switch r.name {
		case "cluster-02 unaffected":
			if r.d > 300*time.Millisecond || !bytes.Contains(r.body, []byte("<values>")) {
				t.Errorf("%s took %s: held by another cluster's knobs", r.name, r.d)
			}
		default:
			if r.d < 700*time.Millisecond || !bytes.Contains(r.body, []byte("<values>")) {
				t.Errorf("%s took %s, expected the hold and then a normal answer", r.name, r.d)
			}
		}
	}
}

// With users configured, the token and certificate logins vcsim accepts
// from anyone are refused on /sdk.
func TestTokenLoginRefusedWithUsers(t *testing.T) {
	e := startWith(t, topology.Phase1("token"), sim.Options{Credentials: map[string]string{"u": "p"}})
	req := `<?xml version="1.0" encoding="UTF-8"?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body><LoginByToken xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><locale>en</locale></LoginByToken></soapenv:Body></soapenv:Envelope>`
	status, body := e.post("/sdk", req)
	if status != 500 || !bytes.Contains(body, []byte("InvalidLogin")) {
		t.Fatalf("LoginByToken with users configured: status %d %s", status, body)
	}
	if err := e.login("u", "p"); err != nil {
		t.Fatal(err)
	}
	status, body = e.postPerf(e.cl().Ref, "cluster-domclient")
	if status != 200 || !bytes.Contains(body, []byte("<values>")) {
		t.Fatalf("a real login still works: %d %s", status, body)
	}
}

// Fault windows snap outward to bucket boundaries: a window inside one
// bucket is active for that bucket, and one ending mid-bucket runs to the
// bucket's end.
func TestFaultWindowRounding(t *testing.T) {
	bucket := captureTime.Truncate(5 * time.Minute) // 23:35; captureTime is 23:38:34
	inside := bucket.Add(time.Minute)               // 23:36
	alsoInside := bucket.Add(2 * time.Minute)       // 23:37, before captureTime
	sc := tenClusters(t)
	sc.Faults = []scenario.Fault{
		{Family: "vsan-cpu", Fault: scenario.FaultSystemError, From: &inside, To: &alsoInside},
	}
	e := startWith(t, topology.Generate(sc, registry.InMemory(sc.Seed)), sim.Options{Faults: sc.Faults})
	if err := e.login("administrator@vsphere.local", "VMware1!"); err != nil {
		t.Fatal(err)
	}
	status, body := e.postPerf(e.cl().Ref, "vsan-cpu")
	if status != 500 || !bytes.Contains(body, []byte("SystemErrorFault")) {
		t.Fatalf("a window inside the current bucket must be active: %d %s", status, body[:200])
	}
	past := bucket.Add(-time.Minute) // 23:34, previous bucket
	sc.Faults[0].From, sc.Faults[0].To = &past, &past
	e2 := startWith(t, topology.Generate(sc, registry.InMemory(sc.Seed)), sim.Options{Faults: sc.Faults})
	if err := e2.login("administrator@vsphere.local", "VMware1!"); err != nil {
		t.Fatal(err)
	}
	if status, _ := e2.postPerf(e2.cl().Ref, "vsan-cpu"); status != 200 {
		t.Fatalf("a window in the previous bucket must be over: %d", status)
	}
}

// Adding hosts to a cluster with several VMs per host keeps every existing
// VM's MoRef (VMs are created in their own order, then moved to the host
// the topology places them on), and the VMs that move land on the new host.
func TestAddingHostsKeepsVMMoRefs(t *testing.T) {
	sc, err := scenario.Parse([]byte(`
seed: grow-hosts
datacenters: [{name: dc-01, clusters: 1}]
defaults: {hosts: 4, vms: {perHost: 2}}
`))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	r1, _ := registry.Open(path, sc.Seed, false)
	e1 := startWith(t, topology.Generate(sc, r1), sim.Options{})
	if err := r1.Save(); err != nil {
		t.Fatal(err)
	}
	if err := e1.login("a", "b"); err != nil {
		t.Fatal(err)
	}
	before := identities(t, e1)
	e1.srv.Close()
	e1.sim.Close()

	sc.Defaults.Hosts = 5
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	r2, _ := registry.Open(path, sc.Seed, false)
	topo := topology.Generate(sc, r2)
	e2 := startWith(t, topo, sim.Options{})
	if err := e2.login("a", "b"); err != nil {
		t.Fatal(err)
	}
	after := identities(t, e2)
	cl := topo.Clusters()[0]
	if len(cl.Hosts) != 5 || len(cl.VMs) != 10 { // two VMs per host: the fifth host brings two new VMs
		t.Fatalf("hosts %d vms %d", len(cl.Hosts), len(cl.VMs))
	}
	for _, vm := range cl.VMs[:8] { // the eight VMs that existed before the edit
		if before[vm.Name] == "" || before[vm.Name] != after[vm.Name] {
			t.Errorf("%s changed:\n before %s\n after  %s", vm.Name, before[vm.Name], after[vm.Name])
		}
	}
	// VMs placed on the new host really are there, and the old hosts' MoRefs held
	refs := e2.cl()
	pc := property.DefaultCollector(e2.client)
	moved := 0
	for i, vref := range refs.VMs {
		var mvm mo.VirtualMachine
		if err := pc.RetrieveOne(context.Background(), vref, []string{"runtime.host"}, &mvm); err != nil {
			t.Fatal(err)
		}
		want := refs.Hosts[cl.VMs[i].Host]
		if mvm.Runtime.Host == nil || *mvm.Runtime.Host != want {
			t.Errorf("%s: runtime.host %v, want %s", cl.VMs[i].Name, mvm.Runtime.Host, want)
		}
		if cl.VMs[i].Host == 4 {
			moved++
			var hs mo.HostSystem
			if err := pc.RetrieveOne(context.Background(), want, []string{"vm"}, &hs); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, v := range hs.Vm {
				found = found || v == vref
			}
			if !found {
				t.Errorf("%s not in the new host's vm list", cl.VMs[i].Name)
			}
		}
	}
	if moved == 0 {
		t.Error("no VM was placed on the new host")
	}
	for i := 0; i < 4; i++ {
		if before[cl.Hosts[i].Name] != after[cl.Hosts[i].Name] {
			t.Errorf("host %s changed", cl.Hosts[i].Name)
		}
	}
}
