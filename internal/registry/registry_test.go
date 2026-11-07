package registry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSerialsPersistAndNeverReuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r, err := Open(path, "seed-a", false)
	if err != nil {
		t.Fatal(err)
	}
	a, b := r.Serial("cluster/dc-01/cluster-01"), r.Serial("cluster/dc-01/cluster-02")
	if a != 1 || b != 2 || r.Serial("cluster/dc-01/cluster-01") != 1 {
		t.Fatalf("serials %d %d", a, b)
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	// reopen: same serials, a new key gets the next serial even though
	// nothing in between was "removed" from the file
	r2, err := Open(path, "seed-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Serial("cluster/dc-01/cluster-02") != 2 || r2.Serial("cluster/dc-01/cluster-01") != 1 {
		t.Fatal("serials changed across reopen")
	}
	if r2.Serial("cluster/dc-01/cluster-03") != 3 {
		t.Fatal("new key did not get the next serial")
	}
	if r.UUID("vsan-cluster", 1) != r2.UUID("vsan-cluster", 1) {
		t.Fatal("identity changed across reopen")
	}
	if r.UUID("vsan-cluster", 1) == r.UUID("vsan-cluster", 2) || r.UUID("vsan-cluster", 1) == r.UUID("vsan-node", 1) {
		t.Fatal("identities alias")
	}
}

func TestSeedChangeRefusedUnlessRegenerate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r, _ := Open(path, "seed-a", false)
	r.Serial("x")
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "seed-b", false); !errors.Is(err, ErrSeedChanged) {
		t.Fatalf("got %v", err)
	}
	r2, err := Open(path, "seed-b", true)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Has("x") || r2.Serial("x") != 1 {
		t.Fatal("regenerate did not start over")
	}
	if r.UUID("k", 1) == r2.UUID("k", 1) {
		t.Fatal("regenerate kept the namespace")
	}
	// regenerate under the same seed starts the serials over too
	r2.Serial("y")
	if err := r2.Save(); err != nil {
		t.Fatal(err)
	}
	r3, err := Open(path, "seed-b", true)
	if err != nil {
		t.Fatal(err)
	}
	if r3.Has("x") || r3.Has("y") || r3.Serial("z") != 1 {
		t.Fatal("regenerate with the same seed kept the registry")
	}
}

func TestSaveIsAtomicAndOnlyWhenDirty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "registry.json")
	r, _ := Open(path, "s", false)
	if !r.Dirty() {
		t.Fatal("a new registry is dirty")
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if r.Dirty() {
		t.Fatal("still dirty after save")
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("temp file left behind")
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	m := InMemory("s")
	m.Serial("y")
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestNamespaceIsSeedDerived(t *testing.T) {
	a, b := InMemory("seed"), InMemory("seed")
	if a.UUID("vcenter", 0) != b.UUID("vcenter", 0) || a.Sub("dc/cl") != b.Sub("dc/cl") {
		t.Fatal("same seed, different identities")
	}
	if a.Sub("dc/cl-01") == a.Sub("dc/cl-02") {
		t.Fatal("sub seeds alias")
	}
}

func TestCloneIsolatedAndRetireNeverReusesSerials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r, _ := Open(path, "seed", false)
	r.Serial("cluster/dc-01/cluster-01")
	r.Serial("host/dc-01/cluster-01/esx01")
	r.Serial("vm/dc-01/cluster-01/vm001")
	r.Serial("cluster/dc-01/cluster-02")
	r.Serial("host/dc-01/cluster-02/esx01")
	r.Serial("cluster/dc-01/cluster-10")

	c := r.Clone()
	if n := c.Retire("dc-01/cluster-01"); n != 3 {
		t.Errorf("retired %d keys, want the cluster, its host and its VM", n)
	}
	if !r.Has("host/dc-01/cluster-01/esx01") {
		t.Error("retiring on a clone changed the original")
	}
	if c.Has("cluster/dc-01/cluster-02") == false || c.Has("cluster/dc-01/cluster-10") == false {
		t.Error("retire matched another cluster (cluster-10 shares a prefix with cluster-1)")
	}
	next := c.Serial("cluster/dc-01/cluster-01")
	if next != 7 {
		t.Errorf("a retired key got serial %d, want a fresh 7", next)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	r2, _ := Open(path, "seed", false)
	if r2.Serial("cluster/dc-01/cluster-01") != 7 {
		t.Error("the clone's save did not become the registry")
	}
}
