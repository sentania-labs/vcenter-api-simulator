package catalog

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const fixtures = "../../fixtures/live/9.1.1"

// The embedded catalog must be what the generator produces from the fixtures.
func TestCatalogIsCurrent(t *testing.T) {
	out := filepath.Join(t.TempDir(), "catalog.json")
	cmd := exec.Command("go", "run", "./gen", "-fixtures", fixtures, "-out", out)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, raw) {
		t.Fatal("catalog.json is stale: run `go generate ./internal/catalog`")
	}
}

func TestCatalogContents(t *testing.T) {
	c := MustLoad()
	if len(c.EntityTypes) != 70 {
		t.Errorf("entity types: %d", len(c.EntityTypes))
	}
	counts := map[Behaviour]int{}
	for _, f := range c.Families {
		counts[f.Behaviour]++
	}
	if counts[Data] != 41 || counts[Empty] != 17 || counts[InvalidArgument] != 9 || counts[NotSupported] != 2 {
		t.Errorf("behaviours: %v", counts)
	}
	if c.About.ApiVersion != "9.1.1.0" || c.About.Build == "" {
		t.Errorf("about: %+v", c.About)
	}
	if !strings.Contains(c.VsanServiceVersions, "<version>9.1.1.0</version>") {
		t.Error("vsanServiceVersions.xml missing")
	}
	if got := c.Unit("cluster-domclient", "latencyAvgRead"); got != "time_ms" {
		t.Errorf("unit: %q", got)
	}
	if got := c.Unit("vsan-tcpip-stats", "tcpErrsActual"); got != "permille" {
		t.Errorf("twin unit: %q", got)
	}
	if got := c.Unit("vsan-tcpip-stats", "nonesuch"); got != "number" {
		t.Errorf("default unit: %q", got)
	}
	if len(c.Families["vsan-tcpip-stats"].Labels) != 32 {
		t.Errorf("observed labels exceed the advertised 16: %d", len(c.Families["vsan-tcpip-stats"].Labels))
	}
}

// The fixture set is a sanitized capture of a lab vCenter. Nothing that
// identifies the lab or its hardware may be in it.
func TestFixturesAreSanitized(t *testing.T) {
	// hardware vendors, not parts: the list must not itself name what the lab runs
	brands := regexp.MustCompile(`(?i)Samsung|Sabrent|Seagate|FireCuda|\bTEAM\b|hynix|phison|\bAMD\b|\bIntel\b|Realtek|Mellanox|Broadcom Inc|Micron|Kingston|Western Digital|\bWDC\b|Crucial|\bDell\b|\bHPE\b|Lenovo|Supermicro|\bASUS\b|Gigabyte|Minisforum|Beelink|Micro Computer|JetKVM|Toshiba|Kioxia|Solidigm|Nvidia|Qualcomm`)
	hosts := regexp.MustCompile(`\b[a-z0-9-]+\.(?:sentania|local|lan|home|internal|corp|int)\b`)
	nqn := regexp.MustCompile(`nqn\.\d{4}-\d{2}\.[a-z.]+`)
	// every file under the fixture tree, including the inventory/ subtree
	var files []string
	err := filepath.WalkDir(fixtures, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 180 {
		t.Fatalf("only %d fixture files", len(files))
	}
	inventory := 0
	for _, path := range files {
		name, _ := filepath.Rel(fixtures, path)
		if strings.HasPrefix(name, "inventory"+string(filepath.Separator)) {
			inventory++
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range []*regexp.Regexp{brands, hosts} {
			if m := re.Find(b); m != nil {
				t.Errorf("%s: %q", name, m)
			}
		}
		for _, m := range nqn.FindAll(b, -1) {
			if string(m) != "nqn.2000-01.invalid" {
				t.Errorf("%s: NVMe qualified name %q", name, m)
			}
		}
		if bytes.Contains(b, []byte("vmware_soap_session=")) && !bytes.Contains(b, []byte("<session-redacted>")) {
			t.Errorf("%s: session cookie", name)
		}
	}
	if inventory == 0 {
		t.Error("no files under inventory/ were scanned")
	}
}
