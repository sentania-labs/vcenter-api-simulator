package control

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
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

// A registry write can exhaust storage while the smaller state file would
// still fit. A changed seed must never land on disk without its identities.
func TestRegistrySaveFailureKeepsPersistedScenarioConsistent(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("requires /dev/full")
	}
	for _, retry := range []bool{false, true} {
		name := "restart_previous_layout"
		if retry {
			name = "retry_then_restart_new_layout"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			opts := Options{Scenario: scenario.Phase1("previous-layout"), SourceHash: "seed-save", DataDir: dir, AnyLogin: true}
			c, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if c != nil {
					c.Close()
				}
			}()
			oldUUID := c.applied().topo.InstanceUUID
			saved := map[string][]byte{}
			for _, file := range []string{"registry.json", "console.json"} {
				saved[file], err = os.ReadFile(filepath.Join(dir, file))
				if err != nil {
					t.Fatal(err)
				}
			}
			fault := filepath.Join(dir, "registry.json.tmp")
			if err = os.Symlink("/dev/full", fault); err != nil {
				t.Fatal(err)
			}
			next := clone(c.applied().sc)
			next.Seed = "replacement-layout"
			c.mu.Lock()
			_, err = c.applyScenario(next, true)
			c.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if c.applied().sc.Seed != next.Seed || c.unsaved == "" {
				t.Fatal("replacement must serve in memory and report unsaved state")
			}
			w := httptest.NewRecorder()
			c.ServeHTTP(w, httptest.NewRequest("GET", "/sdk/vimServiceVersions.xml", nil))
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
			for file, want := range saved {
				got, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("%s changed although registry could not be saved", file)
				}
			}
			if err = os.Remove(fault); err != nil {
				t.Fatal(err)
			}
			wantSeed, wantUUID := opts.Scenario.Seed, oldUUID
			if retry {
				c.mu.Lock()
				c.save()
				c.mu.Unlock()
				if c.unsaved != "" {
					t.Fatal(c.unsaved)
				}
				wantSeed, wantUUID = next.Seed, c.applied().topo.InstanceUUID
			}
			c.Close()
			c = nil
			restored, err := New(opts)
			if err != nil {
				t.Fatalf("restart after storage recovery: %v", err)
			}
			defer restored.Close()
			if restored.applied().sc.Seed != wantSeed || restored.reg.Seed() != wantSeed || restored.applied().topo.InstanceUUID != wantUUID {
				t.Fatal("restarted scenario and identities disagree")
			}
		})
	}
}
