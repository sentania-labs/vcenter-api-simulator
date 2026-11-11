package control

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

func storageLifecycleScenario(t *testing.T) *scenario.Scenario {
	t.Helper()
	s := scenario.Phase1("storage-lifecycle")
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
	if _, err := c.DeleteCluster("dc-01/cluster-02"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Review(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	restarted, err := New(Options{Scenario: sc, SourceHash: "same", DataDir: dir})
	if err != nil {
		t.Fatalf("restart after cluster deletion: %v", err)
	}
	defer restarted.Close()
	dc := restarted.applied().sc.Datacenters[0]
	if len(dc.ISCSI) != 1 || dc.ISCSI[0].Cluster != "dc-01/cluster-01" {
		t.Fatalf("iSCSI after removal: %+v", dc.ISCSI)
	}
	if len(dc.NFS) != 1 || dc.NFS[0].Name != "shared" || len(dc.NFS[0].Clusters) != 1 || dc.NFS[0].Clusters[0] != "dc-01/cluster-01" || len(dc.NFS[0].InaccessibleHosts) != 1 {
		t.Fatalf("NFS scope widened or retained removed host: %+v", dc.NFS)
	}
}

func TestStorageReferencesOnHostRemovalAndRestart(t *testing.T) {
	sc := storageLifecycleScenario(t)
	dir := t.TempDir()
	c := newCtl(t, sc, dir, "same")
	edited := editable(t, c)
	setHosts(edited, "dc-01/cluster-01", 3)
	b, err := json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetDraft(b, ""); err != nil {
		t.Fatalf("shrink host count: %v", err)
	}
	if _, err = c.Review(); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	restarted, err := New(Options{Scenario: sc, SourceHash: "same", DataDir: dir})
	if err != nil {
		t.Fatalf("restart after host removal: %v", err)
	}
	defer restarted.Close()
	dc := restarted.applied().sc.Datacenters[0]
	if len(dc.NFS[1].InaccessibleHosts) != 1 || !strings.HasPrefix(dc.NFS[1].InaccessibleHosts[0], "dc-01/cluster-02/") {
		t.Fatalf("NFS faults: %+v", dc.NFS[1].InaccessibleHosts)
	}
	if len(dc.ISCSI[0].LUNs[0].Paths) != 1 || len(dc.ISCSI[1].LUNs[0].Paths) != 2 {
		t.Fatalf("iSCSI paths after removal: %+v", dc.ISCSI)
	}
}

// A prior release could persist dangling storage selectors. Recovery must
// repair only those references, record it, and persist a restartable state.
func TestRecoverSavedDanglingStorageReferences(t *testing.T) {
	baseline := storageLifecycleScenario(t)
	dir := t.TempDir()
	saved := clone(baseline)
	saved.Removed = []string{"dc-01/cluster-02"}
	setHosts(saved, "dc-01/cluster-01", 3)
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

func TestReviewAndApplyRejectInvalidDraft(t *testing.T) {
	s := storageLifecycleScenario(t)
	c := newCtl(t, s, "", "same")
	bad := editable(t, c)
	bad.Datacenters[0].NFS[0].InaccessibleHosts = append(bad.Datacenters[0].NFS[0].InaccessibleHosts, "dc-01/cluster-02/esx99")
	raw, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetDraft(raw, ""); err == nil {
		t.Fatal("new dangling host accepted")
	}
	c.draft = &Draft{Scenario: bad, Scope: ScopeAll}
	if _, err = c.Review(); err == nil {
		t.Fatal("review accepted invalid scenario")
	}
	if _, err = c.Apply("boundary"); err == nil {
		t.Fatal("scheduled invalid scenario")
	}
	if _, err = c.Apply("now"); err == nil {
		t.Fatal("applied invalid scenario")
	}
	if c.applied().version != 1 {
		t.Fatal("invalid draft changed running topology")
	}
}

func TestLastHostSpecificPathDoesNotLeaveInvalidLUN(t *testing.T) {
	sc := storageLifecycleScenario(t)
	sc.Datacenters[0].ISCSI[0].LUNs[0].Paths = []scenario.ISCSIPath{{Adapter: "adapter-1", Host: "dc-01/cluster-01/esx04", State: "dead"}}
	c := newCtl(t, sc, "", "same")
	edit := editable(t, c)
	setHosts(edit, "dc-01/cluster-01", 3)
	put(t, c, edit)
	draft := c.DraftState().Scenario.Datacenters[0].ISCSI[0]
	if len(draft.LUNs) != 0 {
		t.Fatalf("unreachable LUN was not removed: %+v", draft.LUNs)
	}
	if _, err := c.Apply("now"); err != nil {
		t.Fatalf("apply after last path removed: %v", err)
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
