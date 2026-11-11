package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every shipped example parses and validates.
func TestExamplesParse(t *testing.T) {
	files, _ := filepath.Glob("../../examples/scenarios/*.yaml")
	if len(files) < 3 {
		t.Fatalf("%d example scenarios", len(files))
	}
	for _, f := range files {
		s, err := Load(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if len(s.ClusterKeys()) == 0 {
			t.Errorf("%s: no clusters", f)
		}
	}
	ten, err := Load("../../examples/scenarios/ten-clusters.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(ten.ClusterKeys()) != 10 || ten.ClusterKeys()[2] != "dc-01/cluster-03" {
		t.Errorf("cluster keys %v", ten.ClusterKeys())
	}
	if len(ten.Faults) != 0 || ten.Latency.PerfQuery != nil {
		t.Error("the acceptance scenario must carry no faults or latency")
	}
	knobs, err := Load("../../examples/scenarios/faults-and-latency.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(knobs.FamilyFaults()) != 1 || knobs.Latency.Families["host-domowner"].FixedMs != 300 || knobs.Latency.SDK == nil {
		t.Errorf("knobs: faults %+v latency %+v", knobs.Faults, knobs.Latency)
	}
	if knobs.Seed != ten.Seed {
		t.Error("the knobs example must share the acceptance scenario's seed")
	}
}

func TestDefaultsAndOverrides(t *testing.T) {
	s, err := Parse([]byte(`
seed: s
datacenters: [{name: dc-01, clusters: 2}]
defaults:
  hosts: 6
  host: {threads: 64}
clusters:
  - key: dc-01/cluster-02
    hosts: 3
    dedup: false
    vms: {perHost: 9}
`))
	if err != nil {
		t.Fatal(err)
	}
	// unset defaults come from the built-in defaults
	if s.Defaults.Hosts != 6 || s.Defaults.Host.Threads != 64 || Int(s.Defaults.Host.DomWorlds) != 66 || s.Defaults.VMs.DisksPerVM != 1 || !*s.Defaults.Dedup {
		t.Errorf("defaults %+v", s.Defaults)
	}
	drawn := ClusterSpec{Hosts: 12, Host: HostSpec{ClaimedDevices: 2}}
	// zero is a value, not "inherit"
	z, err := Parse([]byte("seed: s\ndatacenters: [{name: dc-01, clusters: 1}]\ndefaults: {host: {localDevices: 0, domWorlds: 0}, vms: {perHost: 0}}"))
	if err != nil {
		t.Fatal(err)
	}
	if Int(z.Defaults.Host.LocalDevices) != 0 || Int(z.Defaults.Host.DomWorlds) != 0 || Int(z.Defaults.VMs.PerHost) != 0 {
		t.Errorf("zero counts lost: %+v", z.Defaults)
	}
	c1 := s.Resolve("dc-01/cluster-01", drawn)
	if c1.Hosts != 12 || c1.Host.ClaimedDevices != 2 || c1.Host.Threads != 64 || !*c1.Dedup {
		t.Errorf("draws over defaults: %+v", c1)
	}
	c2 := s.Resolve("dc-01/cluster-02", drawn)
	if c2.Hosts != 3 || *c2.Dedup || Int(c2.VMs.PerHost) != 9 || c2.Host.ClaimedDevices != 2 || c2.Host.Threads != 64 {
		t.Errorf("override over draws: %+v", c2)
	}
	if _, ok := s.Override("dc-01/cluster-01"); ok {
		t.Error("cluster-01 has no override")
	}
}

func TestValidationRejects(t *testing.T) {
	cases := map[string]string{
		"no seed":                     "datacenters: [{name: dc, clusters: 1}]",
		"no datacenters":              "seed: s",
		"bad dc name":                 "seed: s\ndatacenters: [{name: DC_1, clusters: 1}]",
		"duplicate dc":                "seed: s\ndatacenters: [{name: dc, clusters: 1}, {name: dc, clusters: 1}]",
		"unknown field":               "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nhosts: 4",
		"zero hosts":                  "seed: s\ndatacenters: [{name: dc, clusters: 1}]\ndefaults: {hosts: 0}\nclusters: [{key: dc/cluster-01, hosts: -1}]",
		"two hosts":                   "seed: s\ndatacenters: [{name: dc, clusters: 1}]\ndefaults: {hosts: 2}",
		"vary hosts two":              "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nvary: {hosts: [2, 4]}",
		"override not generated":      "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nclusters: [{key: dc/cluster-42}]",
		"user twice":                  "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nauth: {users: [{name: u, password: x}, {name: u, password: y}]}",
		"fault unknown family":        "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nfaults: [{family: no-such-family, fault: timeout}]",
		"fault unknown method":        "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nfaults: [{method: VsanPerfQueryPerf, fault: timeout}]",
		"fault cluster not generated": "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nfaults: [{family: host-domclient, fault: timeout, clusters: [dc/cluster-77]}]",
		"latency unknown family":      "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nlatency: {families: {typo-family: {fixedMs: 1}}}",
		"bad override key":            "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nclusters: [{key: cluster-01}]",
		"override unknown dc":         "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nclusters: [{key: other/cluster-01}]",
		"override twice":              "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nclusters: [{key: dc/cluster-01}, {key: dc/cluster-01}]",
		"vary hosts zero":             "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nvary: {hosts: [0, 4]}",
		"vary range inverted":         "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nvary: {vmsPerHost: {min: 5, max: 2}}",
		"user without name":           "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nauth: {users: [{password: x}]}",
		"user two passwords":          "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nauth: {users: [{name: u, password: x, passwordEnv: Y}]}",
		"user no password":            "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nauth: {users: [{name: u}]}",
		"tls half pair":               "seed: s\ndatacenters: [{name: dc, clusters: 1}]\ntls: {certFile: a.pem}",
		"fault without target":        "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nfaults: [{fault: timeout}]",
		"fault both targets":          "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nfaults: [{family: a, method: b, fault: timeout}]",
		"fault bad kind":              "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nfaults: [{family: a, fault: explode}]",
		"fault bad cluster":           "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nfaults: [{family: host-domclient, fault: timeout, clusters: [x]}]",
		"fault to before from":        "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nfaults: [{family: host-domclient, fault: timeout, from: 2026-11-07T15:00:00Z, to: 2026-11-07T14:00:00Z}]",
		"negative latency":            "seed: s\ndatacenters: [{name: dc, clusters: 1}]\nlatency: {sdk: {fixedMs: -1}}",
		"empty":                       "",
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse([]byte("seed: s\ndatacenters: [{name: dc, clusters: 0}]")); err != nil {
		t.Errorf("a datacenter with no clusters is allowed: %v", err)
	}
}

func TestCredentials(t *testing.T) {
	s, err := Parse([]byte("seed: s\ndatacenters: [{name: dc, clusters: 1}]\nauth: {users: [{name: a, password: pa}, {name: b, passwordEnv: SCENARIO_TEST_PW}]}"))
	if err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("SCENARIO_TEST_PW")
	if _, err := s.Credentials(); err == nil || !strings.Contains(err.Error(), "SCENARIO_TEST_PW") {
		t.Errorf("unset env: %v", err)
	}
	t.Setenv("SCENARIO_TEST_PW", "pb")
	c, err := s.Credentials()
	if err != nil || c["a"] != "pa" || c["b"] != "pb" || len(c) != 2 {
		t.Errorf("credentials %v %v", c, err)
	}
	open := Phase1("x")
	if c, err := open.Credentials(); err != nil || len(c) != 0 {
		t.Errorf("no users: %v %v", c, err)
	}
}

func TestDelayDuration(t *testing.T) {
	d := Delay{FixedMs: 100, JitterMs: 50}
	if d.Duration(0) != 100*time.Millisecond || d.Duration(1) != 150*time.Millisecond {
		t.Errorf("%v %v", d.Duration(0), d.Duration(1))
	}
}

func TestVersionPresetsAndNewerHostValidation(t *testing.T) {
	s := Phase1("version-test")
	s.VCenter = Version{Preset: "8.0u3"}
	s.ESX = Version{Preset: "9.0"}
	if err := s.Validate(); err == nil {
		t.Fatal("newer ESX was accepted without an override")
	}
	s.ForceNewerHosts = true
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	vc, err := s.VCenterVersion()
	if err != nil || vc.Build != "24022515" {
		t.Fatalf("vCenter preset: %+v %v", vc, err)
	}
	host, err := s.HostVersion("dc-01/cluster-01", "dc-01/cluster-01/esx01")
	if err != nil || host.Build != "24755229" {
		t.Fatalf("ESX preset: %+v %v", host, err)
	}
	s.HostVersions = map[string]Version{"dc-01/cluster-01/esx01": {Preset: "invalid"}}
	if err := s.Validate(); err == nil {
		t.Fatal("unknown preset accepted")
	}
}
