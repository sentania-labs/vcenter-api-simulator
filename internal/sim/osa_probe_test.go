package sim_test

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// TestOSAWalk runs the read-only probe against both OSA service states.
// It is opt-in because the probe requires pyVmomi and writes capture tarballs.
func TestOSAWalk(t *testing.T) {
	out := os.Getenv("OSA_PROBE_OUT")
	if out == "" {
		t.Skip("set OSA_PROBE_OUT to run the OSA probe")
	}
	for _, primary := range []bool{false, true} {
		name := "offline"
		if primary {
			name = "online"
		}
		t.Run(name, func(t *testing.T) {
			sc := scenario.Phase1("osa-walk")
			sc.Defaults.Architecture = "osa"
			sc.Defaults.Hosts = 3
			sc.Defaults.StatsPrimary = &primary
			sc.Defaults.Host.Threads = 5
			sc.Defaults.Host.DomWorlds = scenario.IntPtr(14)
			sc.Defaults.Host.VsanPnics = 1
			s, err := sim.New(sim.Options{Topology: topology.Generate(sc, registry.InMemory(sc.Seed))})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			srv := httptest.NewTLSServer(s.Handler)
			defer srv.Close()
			dest := filepath.Join(out, name)
			if err := os.MkdirAll(dest, 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("python3", "../../tools/probe/probe.py", srv.URL, "--out", dest)
			cmd.Env = append(os.Environ(), "VCENTER_USER=probe", "VCENTER_PASSWORD=probe")
			result, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("probe: %v\n%s", err, result)
			}
			t.Logf("%s", result)
		})
	}
}
