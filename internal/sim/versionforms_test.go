package sim_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
)

// What each interface serves for a release, pinned apart from the preset
// table so the forms cannot drift. 9.1.1 (both products) and the 9.0 host are
// what real vCenters and hosts serve; the 8.0 U3 and 9.0 vCenter strings have
// no reachable authoritative source and are pinned as they were.
//
// VCF Operations builds the vCenter card's Version as
// version.patchLevel.build, so patchLevel is what makes it read
// 9.1.1.0100.25784172 instead of 9.1.1.0.25784172.
func TestVersionFormsServedPerInterface(t *testing.T) {
	type form struct{ version, patch, build, api, full string }
	cases := []struct {
		preset  string
		vc, esx form
		rest    map[string]string
	}{
		{"8.0u3",
			form{"8.0.3", "", "24022515", "8.0.3.0", "VMware vCenter Server 8.0.3.00000.24022515"},
			form{"8.0.3", "", "24022510", "8.0.3.0", "VMware ESXi 8.0.3 build-24022510"},
			map[string]string{"version": "8.0.3.00000", "build": "24022515", "summary": "VMware vCenter Server 8.0"}},
		{"9.0",
			form{"9.0.0", "", "24755230", "9.0.0.0", "VMware vCenter Server 9.0.0.0.24755230"},
			form{"9.0.0", "0.0", "24755229", "9.0.0.0", "VMware ESXi 9.0.0.0.24755229"},
			map[string]string{"version": "9.0.0.0", "build": "24755230", "summary": "VMware vCenter Server 9.0"}},
		{"9.1.1",
			form{"9.1.1", "0100", "25784172", "9.1.1.0", "VMware vCenter Server 9.1.1.0100.25784172"},
			form{"9.1.1", "1.0100", "25782009", "9.1.1.0", "VMware ESXi 9.1.1.0100.25782009"},
			map[string]string{"version": "9.1.1.0100", "build": "25784172", "summary": "Patch for VMware vCenter Server 9.1"}},
	}
	for _, c := range cases {
		t.Run(c.preset, func(t *testing.T) {
			sc := scenario.Phase1("forms-" + c.preset)
			sc.VCenter = scenario.Version{Preset: c.preset}
			if err := sc.Validate(); err != nil {
				t.Fatal(err)
			}
			e := startWith(t, topology.Generate(sc, registry.InMemory(sc.Seed)), sim.Options{})
			if err := e.login("reader", "password"); err != nil {
				t.Fatal(err)
			}
			a := e.client.ServiceContent.About
			if got := (form{a.Version, a.PatchLevel, a.Build, a.ApiVersion, a.FullName}); got != c.vc || a.Name != "VMware vCenter Server" {
				t.Errorf("about: %+v, want %+v", a, c.vc)
			}
			// the card's string: version.patchLevel.build
			if c.vc.patch != "" && a.FullName != "VMware vCenter Server "+a.Version+"."+a.PatchLevel+"."+a.Build {
				t.Errorf("fullName %q does not read version.patchLevel.build", a.FullName)
			}
			pc := property.DefaultCollector(e.client)
			for _, h := range e.sim.Clusters[0].Hosts {
				var hs mo.HostSystem
				if err := pc.RetrieveOne(context.Background(), h, []string{"config.product", "summary.config.product"}, &hs); err != nil {
					t.Fatal(err)
				}
				for where, p := range map[string]*struct{ v, pl, b, api, full, name string }{
					"config.product":         {hs.Config.Product.Version, hs.Config.Product.PatchLevel, hs.Config.Product.Build, hs.Config.Product.ApiVersion, hs.Config.Product.FullName, hs.Config.Product.Name},
					"summary.config.product": {hs.Summary.Config.Product.Version, hs.Summary.Config.Product.PatchLevel, hs.Summary.Config.Product.Build, hs.Summary.Config.Product.ApiVersion, hs.Summary.Config.Product.FullName, hs.Summary.Config.Product.Name},
				} {
					if (form{p.v, p.pl, p.b, p.api, p.full}) != c.esx || p.name != "VMware ESXi" {
						t.Errorf("host %s: %+v, want %+v", where, *p, c.esx)
					}
				}
			}
			login, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/session", nil)
			login.SetBasicAuth("reader", "password")
			res, err := http.DefaultClient.Do(login)
			if err != nil {
				t.Fatal(err)
			}
			var token string
			_ = json.NewDecoder(res.Body).Decode(&token)
			res.Body.Close()
			req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/appliance/system/version", nil)
			req.Header.Set("vmware-api-session-id", token)
			res, err = http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var got map[string]string
			if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			for k, want := range c.rest {
				if got[k] != want {
					t.Errorf("REST %s = %q, want %q (%v)", k, got[k], want, got)
				}
			}
		})
	}
}
