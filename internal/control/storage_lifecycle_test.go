package control

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
)

func storageLifecycleScenario(t *testing.T) *scenario.Scenario {
	t.Helper()
	s := scenario.Phase1("storage-lifecycle")
	s.Auth.Users = []scenario.User{{Name: "administrator@vsphere.local", Password: "first-password"}}
	s.Datacenters[0].Clusters = 2
	s.Datacenters[0].NFS = []scenario.NFSDatastore{
		{Name: "scoped", RemoteHost: "nfs.example.test", RemotePath: "/scoped", Capacity: 4096, FreeSpace: 2048, Clusters: []string{"dc-01/cluster-02"}, InaccessibleHosts: []string{"dc-01/cluster-02/esx04"}},
		{Name: "shared", RemoteHost: "nfs.example.test", RemotePath: "/shared", Capacity: 4096, FreeSpace: 2048, Clusters: []string{"dc-01/cluster-01", "dc-01/cluster-02"}, InaccessibleHosts: []string{"dc-01/cluster-01/esx04", "dc-01/cluster-02/esx04"}},
	}
	for i, key := range []string{"dc-01/cluster-01", "dc-01/cluster-02"} {
		s.Datacenters[0].ISCSI = append(s.Datacenters[0].ISCSI, scenario.ISCSIStorage{Cluster: key, Adapters: []scenario.ISCSIAdapter{{Name: "adapter-1", Kind: "software"}}, Targets: []scenario.ISCSITarget{{Name: "array", Address: "array.example.test", IQN: "iqn.2026-01.test.example:array"}}, LUNs: []scenario.ISCSILUN{{Name: []string{"vmfs-one", "vmfs-two"}[i], Target: "array", Number: 1, Capacity: 1048576, FreeSpace: 1024, Paths: []scenario.ISCSIPath{{Adapter: "adapter-1", State: "active"}, {Adapter: "adapter-1", Host: key + "/esx04", State: "dead"}}}}})
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStorageReferencesOnClusterRemovalAndRestart(t *testing.T) {
	sc := storageLifecycleScenario(t)
	dir := t.TempDir()
	c := newCtl(t, sc, dir, "same")
	if _, err := c.RemoveCluster("dc-01/cluster-02"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	restarted := newCtl(t, sc, dir, "same")
	dc := restarted.applied().sc.Datacenters[0]
	if len(dc.ISCSI) != 1 || dc.ISCSI[0].Cluster != "dc-01/cluster-01" {
		t.Fatalf("iSCSI after removal: %+v", dc.ISCSI)
	}
	if len(dc.NFS) != 1 || dc.NFS[0].Name != "shared" || len(dc.NFS[0].Clusters) != 1 || dc.NFS[0].Clusters[0] != "dc-01/cluster-01" || len(dc.NFS[0].InaccessibleHosts) != 1 {
		t.Fatalf("NFS scope widened or retained removed host: %+v", dc.NFS)
	}
}

// Removing a host by name removes the storage faults and paths that named
// it, and the result still starts.
func TestStorageReferencesOnHostRemovalAndRestart(t *testing.T) {
	sc := storageLifecycleScenario(t)
	dir := t.TempDir()
	c := newCtl(t, sc, dir, "same")
	must(t, c, "remove-hosts", map[string]string{"cluster": "dc-01/cluster-01"}, map[string]any{"pick": []string{"esx04"}})
	c.Close()
	restarted := newCtl(t, sc, dir, "same")
	dc := restarted.applied().sc.Datacenters[0]
	if len(dc.NFS[1].InaccessibleHosts) != 1 || !strings.HasPrefix(dc.NFS[1].InaccessibleHosts[0], "dc-01/cluster-02/") {
		t.Fatalf("NFS faults: %+v", dc.NFS[1].InaccessibleHosts)
	}
	if len(dc.ISCSI[0].LUNs[0].Paths) != 1 || len(dc.ISCSI[1].LUNs[0].Paths) != 2 {
		t.Fatalf("iSCSI paths after removal: %+v", dc.ISCSI)
	}
	// a host added later does not inherit the removed host's faults
	must(t, restarted, "add-hosts", map[string]string{"cluster": "dc-01/cluster-01"}, map[string]any{"n": 1})
	if item, _ := restarted.StorageItem("dc-01/vmfs-one"); len(item.Cut) != 0 {
		t.Errorf("a new host cannot reach the datastore: %v", item.Cut)
	}
}

// A prior release could persist dangling storage selectors. Recovery must
// repair only those references, record it, and persist a restartable state.
func TestRecoverSavedDanglingStorageReferences(t *testing.T) {
	baseline := storageLifecycleScenario(t)
	dir := t.TempDir()
	saved := clone(baseline)
	saved.Removed = []string{"dc-01/cluster-02"}
	override(saved, "dc-01/cluster-01").Hosts = 3
	body, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(persisted{Version: 2, Source: "same", Scenario: body})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "console.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	c, err := New(Options{Scenario: baseline, SourceHash: "same", DataDir: dir})
	if err != nil {
		t.Fatalf("recover state: %v", err)
	}
	if !strings.Contains(logs.String(), "removed") || !strings.Contains(logs.String(), "dangling storage references") {
		t.Fatalf("missing recovery log: %s", logs.String())
	}
	dc := c.applied().sc.Datacenters[0]
	if len(dc.ISCSI) != 1 || len(dc.ISCSI[0].LUNs[0].Paths) != 1 || len(dc.NFS) != 1 || len(dc.NFS[0].InaccessibleHosts) != 0 || len(dc.NFS[0].Clusters) != 1 {
		t.Fatalf("recovered storage: %+v", dc)
	}
	c.Close()
	c, err = New(Options{Scenario: baseline, SourceHash: "same", DataDir: dir})
	if err != nil {
		t.Fatalf("restart repaired state: %v", err)
	}
	defer c.Close()
}

// When the host a LUN's only path named is removed, the LUN goes with it
// instead of being left without a path.
func TestLastHostSpecificPathDoesNotLeaveInvalidLUN(t *testing.T) {
	sc := storageLifecycleScenario(t)
	sc.Datacenters[0].ISCSI[0].LUNs[0].Paths = []scenario.ISCSIPath{{Adapter: "adapter-1", Host: "dc-01/cluster-01/esx04", State: "dead"}}
	c := newCtl(t, sc, "", "same")
	must(t, c, "remove-hosts", map[string]string{"cluster": "dc-01/cluster-01"}, map[string]any{"pick": []string{"esx04"}})
	for _, st := range c.applied().sc.Datacenters[0].ISCSI {
		if st.Cluster == "dc-01/cluster-01" && len(st.LUNs) != 0 {
			t.Fatalf("unreachable LUN was not removed: %+v", st.LUNs)
		}
	}
}

func TestRecoveryDoesNotHideUnrelatedInvalidState(t *testing.T) {
	baseline := storageLifecycleScenario(t)
	dir := t.TempDir()
	saved := clone(baseline)
	saved.Datacenters[0].NFS[1].RemotePath = "relative"
	body, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(persisted{Version: 2, Source: "same", Scenario: body})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "console.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = New(Options{Scenario: baseline, SourceHash: "same", DataDir: dir}); err == nil {
		t.Fatal("unrelated invalid state was silently accepted")
	}
}

// The console says which hosts cannot reach a datastore, and the simulator
// serves each host's mount as accessible or not. Both follow the scenario's
// per-host path overrides, so they must name the same hosts.
func TestConsoleAndServedAPIAgreeOnHostSpecificPaths(t *testing.T) {
	sc := scenario.Phase1("iscsi-agreement")
	cluster := "dc-01/cluster-01"
	sc.Datacenters[0].ISCSI = []scenario.ISCSIStorage{{
		Cluster:  cluster,
		Adapters: []scenario.ISCSIAdapter{{Name: "software", Kind: "software"}, {Name: "hardware", Kind: "hardware"}},
		Targets:  []scenario.ISCSITarget{{Name: "array", Address: "array.example.test", IQN: "iqn.2026-01.test.example:array"}},
		LUNs: []scenario.ISCSILUN{{Name: "shared-vmfs", Target: "array", Number: 1, Capacity: 1 << 30, FreeSpace: 1 << 20, Paths: []scenario.ISCSIPath{
			{Adapter: "software", State: "active"},
			{Adapter: "hardware", State: "dead"},
			// esx02: both paths dead, so no working path
			{Adapter: "software", Host: cluster + "/esx02", State: "dead"},
			// esx03: software dead, but its own hardware override is standby
			{Adapter: "software", Host: cluster + "/esx03", State: "dead"},
			{Adapter: "hardware", Host: cluster + "/esx03", State: "standby"},
		}}},
	}}
	sc.Auth.AnyLogin = true
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	c := newCtl(t, sc, "", "t")
	item, err := c.StorageItem("dc-01/shared-vmfs")
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Cut) != 1 || item.Cut[0] != "esx02 in cluster-01" {
		t.Fatalf("console says these hosts cannot reach it: %v", item.Cut)
	}

	s, err := sim.New(sim.Options{Topology: c.applied().topo, Now: func() time.Time { return time.Unix(1_700_000_000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s.Handler)
	t.Cleanup(srv.Close)
	u, _ := soap.ParseURL(srv.URL + "/sdk")
	u.User = nil
	ctx := context.Background()
	client, err := vim25.NewClient(ctx, soap.NewClient(u, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.NewManager(client).Login(ctx, url.UserPassword("reader", "password")); err != nil {
		t.Fatal(err)
	}
	pc := property.DefaultCollector(client)
	names := map[string]string{}
	var store mo.Datastore
	for _, href := range s.Clusters[0].Hosts {
		var h mo.HostSystem
		if err := pc.RetrieveOne(ctx, href, []string{"name", "datastore"}, &h); err != nil {
			t.Fatal(err)
		}
		names[href.Value] = h.Name
		for _, dref := range h.Datastore {
			var ds mo.Datastore
			if err := pc.RetrieveOne(ctx, dref, []string{"name", "host"}, &ds); err != nil {
				t.Fatal(err)
			}
			if ds.Name == "shared-vmfs" {
				store = ds
			}
		}
	}
	var down []string
	for _, m := range store.Host {
		if m.MountInfo.Accessible != nil && !*m.MountInfo.Accessible {
			down = append(down, names[m.Key.Value])
		}
	}
	if len(down) != 1 || down[0] != "esx02.cluster-01.dc-01.sim.invalid" {
		t.Errorf("served API reports inaccessible mounts on %v, console reports %v", down, item.Cut)
	}
}
