package sim_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// The trimmed inventory capture (fixtures/live/9.1.1/inventory, produced by
// tools/fixtures/trim_inventory.py) is compared with what the simulator's
// /sdk serves, by property path. A path is a dotted chain of the vSphere
// property names (the capture's JSON keys, which match govmomi's JSON tags);
// lists contribute the paths of all their elements, references are leaves,
// and null, "", [] and {} count as absent. Zero numbers and false are values.

const (
	inventoryFixtures = fixtures + "/inventory"
	maxPathDepth      = 6
)

// requiredPaths are the properties the vSAN management pack and the VCF
// Operations vCenter adapter read most. Each must be in the fixture (so a
// misspelt path is caught) and served by the simulator.
var requiredPaths = map[string][]string{
	"ClusterComputeResource": {
		"configurationEx.dasConfig.enabled",
		"configurationEx.drsConfig.enabled",
		"configurationEx.drsConfig.defaultVmBehavior",
		"configurationEx.vsanConfigInfo.enabled",
		"configurationEx.vsanConfigInfo.defaultConfig.uuid",
		"configurationEx.vsanConfigInfo.vsanEsaEnabled",
		"configurationEx.vsanHostConfig.clusterInfo.nodeUuid",
		"summary.numHosts",
		"summary.numCpuCores",
		"summary.totalMemory",
		"summary.effectiveCpu",
		"summary.usageSummary.totalVmCount",
	},
	"HostSystem": {
		"config.vsanHostConfig.clusterInfo.nodeUuid",
		"config.network.vnic.spec.ip.ipAddress",
		"config.network.pnic.device",
		"config.network.proxySwitch.dvsUuid",
		"config.network.netStackInstance.key",
		"config.network.dnsConfig.hostName",
		"config.storageDevice.scsiLun.canonicalName",
		"config.storageDevice.hostBusAdapter.driver",
		"config.fileSystemVolume.mountInfo.volume.name",
		"config.product.version",
		"config.dateTimeInfo.ntpConfig.server",
		"hardware.cpuInfo.numCpuThreads",
		"hardware.memorySize",
		"hardware.systemInfo.uuid",
		"summary.hardware.cpuMhz",
		"summary.quickStats.overallCpuUsage",
		"runtime.connectionState",
		"runtime.bootTime",
		"runtime.vsanRuntimeInfo.membershipList.nodeUuid",
	},
	"Datastore/vsan": {
		"info.membershipUuid",
		"info.freeSpace",
		"summary.capacity",
		"summary.freeSpace",
		"summary.url",
		"summary.type",
		"summary.multipleHostAccess",
		"host.mountInfo.path",
		"iormConfiguration.enabled",
		"capability.vsanSparseSupported",
	},
	"Datastore/VMFS": {
		"info.vmfs.uuid",
		"info.vmfs.extent.diskName",
		"info.vmfs.local",
		"info.vmfs.ssd",
		"info.vmfs.version",
		"summary.capacity",
	},
	"VirtualMachine": {
		"config.uuid",
		"config.instanceUuid",
		"config.hardware.numCPU",
		"config.hardware.memoryMB",
		"config.files.vmPathName",
		"guest.guestState",
		"guest.ipAddress",
		"guest.hostName",
		"guest.toolsStatus",
		"runtime.powerState",
		"runtime.host",
		"runtime.bootTime",
		"summary.quickStats.overallCpuUsage",
		"summary.storage.committed",
		"summary.config.numVirtualDisks",
	},
	"VmwareDistributedVirtualSwitch": {
		"config.uuid",
		"config.numPorts",
		"summary.hostMember",
		"summary.portgroupName",
		"uuid",
	},
	"DistributedVirtualPortgroup": {
		"config.key",
		"config.distributedVirtualSwitch",
		"key",
		"host",
	},
}

type fixtureRecord struct {
	MoRef string         `json:"_moref"`
	Props map[string]any `json:"props"`
}

func loadInventoryFixture(t *testing.T, typ string) []fixtureRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(inventoryFixtures, typ+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var recs []fixtureRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		t.Fatalf("%s: %v", typ, err)
	}
	if len(recs) == 0 {
		t.Fatalf("%s: no records", typ)
	}
	return recs
}

// collectPaths adds the dotted paths of every non-empty node under v.
func collectPaths(v any, prefix string, depth int, out map[string]bool) {
	switch x := v.(type) {
	case nil:
		return
	case string:
		if x != "" {
			out[prefix] = true
		}
	case []any:
		for _, el := range x {
			collectPaths(el, prefix, depth, out)
		}
	case map[string]any:
		if len(x) == 0 {
			return
		}
		if _, ok := x["_moref"]; ok { // capture reference
			out[prefix] = true
			return
		}
		if _, ok := x["type"]; ok && len(x) <= 3 {
			if _, ok := x["value"]; ok { // govmomi ManagedObjectReference (also walked below)
				out[prefix] = true
			}
		}
		if depth >= maxPathDepth {
			out[prefix] = true
			return
		}
		for k, el := range x {
			if strings.HasPrefix(k, "_") { // _type, _moref wrappers
				continue
			}
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			n := len(out)
			collectPaths(el, p, depth+1, out)
			if len(out) > n && prefix != "" {
				out[prefix] = true
			}
		}
	default: // numbers and booleans, zero or not
		if prefix != "" {
			out[prefix] = true
		}
	}
}

func fixturePathSet(t *testing.T, recs []fixtureRecord) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, r := range recs {
		collectPaths(map[string]any(r.Props), "", 0, out)
	}
	return out
}

// simPathSet marshals govmomi mo structs to JSON and collects their paths.
func simPathSet(t *testing.T, objs ...any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, o := range objs {
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		// managed-object bookkeeping, not vSphere properties
		delete(m, "self")
		delete(m, "Self")
		collectPaths(m, "", 0, out)
	}
	return out
}

func compareInventory(t *testing.T, kind string, fx, got map[string]bool) {
	t.Helper()
	for _, p := range requiredPaths[kind] {
		if !fx[p] {
			t.Errorf("%s: required path %q is not in the fixture (misspelt?)", kind, p)
		}
		if !got[p] {
			t.Errorf("%s: simulator lacks required path %q", kind, p)
		}
	}
	var missing, frontier []string
	for p := range fx {
		if got[p] {
			continue
		}
		missing = append(missing, p)
		if i := strings.LastIndexByte(p, '.'); i < 0 || got[p[:i]] {
			frontier = append(frontier, p) // the parent is served: the gap starts here
		}
	}
	sort.Strings(frontier)
	covered := len(fx) - len(missing)
	t.Logf("%s: simulator serves %d/%d fixture paths (%.1f%%); %d missing, %d where the gap starts:",
		kind, covered, len(fx), 100*float64(covered)/float64(len(fx)), len(missing), len(frontier))
	for _, p := range frontier {
		t.Logf("  %s: missing %s", kind, p)
	}
}

func TestInventoryFixture(t *testing.T) {
	e := start(t, "inventory")
	ctx := context.Background()
	pc := property.DefaultCollector(e.client)
	cl := e.cl()

	retrieveAll := func(kind string, dst any) {
		t.Helper()
		v, err := view.NewManager(e.client).CreateContainerView(ctx, e.client.ServiceContent.RootFolder, []string{kind}, true)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = v.Destroy(ctx) }()
		if err := v.Retrieve(ctx, []string{kind}, nil, dst); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("ClusterComputeResource", func(t *testing.T) {
		var c mo.ClusterComputeResource
		if err := pc.RetrieveOne(ctx, cl.Ref, nil, &c); err != nil {
			t.Fatal(err)
		}
		compareInventory(t, "ClusterComputeResource",
			fixturePathSet(t, loadInventoryFixture(t, "ClusterComputeResource")), simPathSet(t, c))
	})

	t.Run("HostSystem", func(t *testing.T) {
		var objs []any
		for _, ref := range cl.Hosts {
			var h mo.HostSystem
			if err := pc.RetrieveOne(ctx, ref, nil, &h); err != nil {
				t.Fatal(err)
			}
			objs = append(objs, h)
		}
		compareInventory(t, "HostSystem", fixturePathSet(t, loadInventoryFixture(t, "HostSystem")), simPathSet(t, objs...))
	})

	t.Run("Datastore", func(t *testing.T) {
		recs := loadInventoryFixture(t, "Datastore")
		var all []mo.Datastore
		retrieveAll("Datastore", &all)
		for _, typ := range []string{"vsan", "VMFS"} {
			var fx []fixtureRecord
			for _, r := range recs {
				if s, _ := r.Props["summary"].(map[string]any); s != nil && s["type"] == typ {
					fx = append(fx, r)
				}
			}
			if len(fx) == 0 {
				t.Fatalf("no %s datastore in the fixture", typ)
			}
			var objs []any
			for _, d := range all {
				if d.Summary.Type == typ {
					objs = append(objs, d)
				}
			}
			if len(objs) == 0 {
				t.Errorf("simulator serves no %s datastore", typ)
				continue
			}
			compareInventory(t, "Datastore/"+typ, fixturePathSet(t, fx), simPathSet(t, objs...))
		}
	})

	t.Run("VirtualMachine", func(t *testing.T) {
		var objs []any
		for _, ref := range cl.VMs {
			var vm mo.VirtualMachine
			if err := pc.RetrieveOne(ctx, ref, nil, &vm); err != nil {
				t.Fatal(err)
			}
			if vm.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn {
				objs = append(objs, vm)
			}
		}
		if len(objs) == 0 {
			t.Fatal("no powered-on VM in the first cluster")
		}
		compareInventory(t, "VirtualMachine", fixturePathSet(t, loadInventoryFixture(t, "VirtualMachine")), simPathSet(t, objs...))
	})

	t.Run("VmwareDistributedVirtualSwitch", func(t *testing.T) {
		// DistributedVirtualSwitch matches the VMware subtype too
		var all []mo.DistributedVirtualSwitch
		retrieveAll("DistributedVirtualSwitch", &all)
		if len(all) == 0 {
			t.Fatal("simulator serves no vDS")
		}
		var objs []any
		for _, d := range all {
			if d.Self.Type != "VmwareDistributedVirtualSwitch" {
				// the capture's vDS is a VmwareDistributedVirtualSwitch; a
				// client that asks for that type finds nothing here
				t.Logf("VmwareDistributedVirtualSwitch: simulator serves %s as %s", d.Self.Value, d.Self.Type)
			}
			objs = append(objs, d)
		}
		compareInventory(t, "VmwareDistributedVirtualSwitch",
			fixturePathSet(t, loadInventoryFixture(t, "VmwareDistributedVirtualSwitch")), simPathSet(t, objs...))
	})

	t.Run("DistributedVirtualPortgroup", func(t *testing.T) {
		var all []mo.DistributedVirtualPortgroup
		retrieveAll("DistributedVirtualPortgroup", &all)
		if len(all) == 0 {
			t.Fatal("simulator serves no portgroup")
		}
		var objs []any
		for _, d := range all {
			objs = append(objs, d)
		}
		compareInventory(t, "DistributedVirtualPortgroup",
			fixturePathSet(t, loadInventoryFixture(t, "DistributedVirtualPortgroup")), simPathSet(t, objs...))
	})
}
