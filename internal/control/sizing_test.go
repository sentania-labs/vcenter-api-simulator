package control

import (
	"encoding/json"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryPlanning(t *testing.T) {
	small := &Projection{Hosts: 4, VMs: 4}
	large := &Projection{Hosts: 215, VMs: 5009, NVMe: 2580, LocalDisks: 215}
	if MemoryRequired(small) != 256*mib || MemoryRequired(large) != 2048*mib {
		t.Fatalf("budgets %d %d", MemoryRequired(small), MemoryRequired(large))
	}
	if !strings.Contains(memoryLine(large, 512*mib).T, "will not fit") || !strings.Contains(memoryLine(large, 512*mib).T, "2048 MiB") {
		t.Fatal(memoryLine(large, 512*mib))
	}
	if strings.Contains(memoryLine(large, 0).T, "limit") {
		t.Fatal("unknown limit must not claim one")
	}
	root := t.TempDir()
	leaf := filepath.Join(root, "slice", "scope")
	if err := os.MkdirAll(leaf, 0700); err != nil {
		t.Fatal(err)
	}
	for p, v := range map[string]string{root: "max", filepath.Dir(leaf): "536870912", leaf: "max"} {
		if err := os.WriteFile(filepath.Join(p, "memory.max"), []byte(v), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if n := limitUnder(root, "/slice/scope"); n != 512*mib {
		t.Fatal(n)
	}
}
func TestRegistrySaveFailureKeepsServing(t *testing.T) {
	sc := scenario.Phase1("full-volume")
	dir := t.TempDir()
	// A full device gives the same ENOSPC write failure as an exhausted volume.
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("requires /dev/full")
	}
	if err := os.Symlink("/dev/full", filepath.Join(dir, "registry.json.tmp")); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Scenario: sc, DataDir: dir, AnyLogin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.unsaved == "" {
		t.Fatal("unsaved state not reported")
	}
	w := httptest.NewRecorder()
	c.ServeHTTP(w, httptest.NewRequest("GET", "/sdk/vimServiceVersions.xml", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	next := clone(c.applied().sc)
	next.Defaults.Hosts++
	c.mu.Lock()
	_, err = c.applyScenario(next, false)
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if c.applied().sc.Defaults.Hosts != next.Defaults.Hosts {
		t.Fatal("change did not take effect")
	}
	if err := os.Remove(filepath.Join(dir, "registry.json.tmp")); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.save()
	c.mu.Unlock()
	if c.unsaved != "" {
		t.Fatal(c.unsaved)
	}
	if _, err := os.Stat(filepath.Join(dir, "registry.json")); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryDoesNotWaitForRebuildLock(t *testing.T) {
	c := newCtl(t, small(t), "", "health")
	c.mu.Lock() // applyScenario holds this while building the replacement.
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		c.ServeHTTP(w, httptest.NewRequest("GET", "/sdk/vimServiceVersions.xml", nil))
		done <- w.Code
	}()
	select {
	case code := <-done:
		if code != 200 {
			t.Error(code)
		}
	case <-time.After(time.Second):
		t.Error("SDK discovery blocked behind inventory build")
	}
	c.mu.Unlock()
}

func TestClusterPreviewShowsMemoryBeforeApply(t *testing.T) {
	c := newCtl(t, small(t), "", "preview")
	p, err := c.TaskPreview("add-cluster", json.RawMessage(`{"dc":"dc-01","hosts":61,"vms":23,"vsan":"ESA","count":3}`), Caller{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range p.Sum {
		if strings.Contains(item.T, "1792 MiB") && strings.Contains(item.T, "4,213 VMs") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing sizing in preview: %+v", p.Sum)
	}
	if len(c.applied().topo.Clusters()) != 1 {
		t.Fatal("preview applied the change")
	}
}
