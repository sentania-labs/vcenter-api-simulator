package sim

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// With credentials configured, only that pair logs in; without them, any
// non-empty pair does (vcsim's default, which the Phase 1 harness relies on).
func TestLoginCredentials(t *testing.T) {
	cases := []struct {
		name         string
		user, pass   string
		tryUser, try string
		ok           bool
	}{
		{"open: any pair", "", "", "anyone", "anything", true},
		{"restricted: right pair", "operator", "s3cret", "operator", "s3cret", true},
		{"restricted: wrong password", "operator", "s3cret", "operator", "nope", false},
		{"restricted: wrong user", "operator", "s3cret", "someone", "s3cret", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(Options{Topology: topology.Phase1("login"), Username: tc.user, Password: tc.pass})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			srv := httptest.NewTLSServer(s.Handler)
			defer srv.Close()

			u, _ := url.Parse(srv.URL + "/sdk")
			sc := soap.NewClient(u, true)
			vc, err := vim25.NewClient(context.Background(), sc)
			if err != nil {
				t.Fatal(err)
			}
			err = session.NewManager(vc).Login(context.Background(), url.UserPassword(tc.tryUser, tc.try))
			if (err == nil) != tc.ok {
				t.Fatalf("login as %s: err=%v, want ok=%v", tc.tryUser, err, tc.ok)
			}
		})
	}
}

// A read-only account keeps its session alive (CurrentTime, SessionIsActive)
// but cannot mutate; the vCenter about info carries the selected release's
// licenseProductVersion rather than the 9.1.1 capture's.
func TestReadOnlyUserSessionHousekeepingAndLicenseVersion(t *testing.T) {
	topo := topology.Phase1("readonly")
	topo.VCenterVersion = scenario.Version{Version: "8.0.3", Build: "24022515", APIVersion: "8.0.3.0", FullName: "VMware vCenter Server 8.0.3.00000.24022515"}
	s, err := New(Options{Topology: topo, Username: "viewer", Password: "pw", UserRole: func(string) string { return "read-only" }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	srv := httptest.NewTLSServer(s.Handler)
	defer srv.Close()

	u, _ := url.Parse(srv.URL + "/sdk")
	vc, err := vim25.NewClient(context.Background(), soap.NewClient(u, true))
	if err != nil {
		t.Fatal(err)
	}
	if got := vc.ServiceContent.About.LicenseProductVersion; got != "8.0" {
		t.Errorf("licenseProductVersion %q, want 8.0", got)
	}
	m := session.NewManager(vc)
	if err := m.Login(context.Background(), url.UserPassword("viewer", "pw")); err != nil {
		t.Fatal(err)
	}
	if _, err := methods.GetCurrentTime(context.Background(), vc); err != nil {
		t.Errorf("CurrentTime refused for read-only user: %v", err)
	}
	if active, err := m.SessionIsActive(context.Background()); err != nil || !active {
		t.Errorf("SessionIsActive = %v, %v", active, err)
	}
	folder := object.NewFolder(vc, vc.ServiceContent.RootFolder)
	if _, err := folder.CreateFolder(context.Background(), "denied"); err == nil {
		t.Error("read-only user created a folder")
	}
	pc := property.DefaultCollector(vc)
	if err := pc.Retrieve(context.Background(), []types.ManagedObjectReference{vc.ServiceContent.RootFolder}, []string{"name"}, &[]mo.Folder{}); err != nil {
		t.Errorf("Retrieve refused for read-only user: %v", err)
	}
	if err := property.Wait(context.Background(), pc, vc.ServiceContent.RootFolder, []string{"name"}, func([]types.PropertyChange) bool { return true }); err != nil {
		t.Errorf("CreateFilter/WaitForUpdates refused for read-only user: %v", err)
	}
}

func TestSOAPOperationUsesDecodedBody(t *testing.T) {
	const envelope = `<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/">%s</soapenv:Envelope>`
	for _, tc := range []struct{ body, want string }{
		{`<!-- <Body><Query/> --><soapenv:Body><Destroy_Task xmlns="urn:vim25"/></soapenv:Body>`, "Destroy_Task"},
		{`<soapenv:Header><Query/></soapenv:Header><soapenv:Body><!-- <Fetch/> --><CurrentTime xmlns="urn:vim25"/></soapenv:Body>`, "CurrentTime"},
		{`<soapenv:Body><Query`, ""},
		{`<soapenv:Body><!-- <Query/> --></soapenv:Body>`, ""},
	} {
		if got := soapOperation([]byte(fmt.Sprintf(envelope, tc.body))); got != tc.want {
			t.Errorf("operation = %q, want %q", got, tc.want)
		}
	}
}

// The vSAN service-version document follows the selected vCenter: the captured
// 9.1.1 document is cut back to the newest namespace that release can speak.
func TestVsanServiceVersionsFollowVCenterRelease(t *testing.T) {
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	for apiVersion, want := range map[string]string{"9.1.1.0": "9.1.1.0", "9.0.0.0": "9.0.0.0", "8.0.3.0": "8.0.0.4"} {
		doc := selectVsanVersions(cat.VsanServiceVersions, apiVersion)
		var parsed struct {
			Namespace struct {
				Version string   `xml:"version"`
				Prior   []string `xml:"priorVersions>version"`
			} `xml:"namespace"`
		}
		if err := xml.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed.Namespace.Version != want {
			t.Errorf("vCenter %s advertises vSAN %s, want %s", apiVersion, parsed.Namespace.Version, want)
		}
		for _, newer := range parsed.Namespace.Prior {
			if newer == "9.1.1.0" && want != "9.1.1.0" {
				t.Errorf("vCenter %s still lists vSAN 9.1.1.0", apiVersion)
			}
		}
	}
}

// A free-form vCenter version whose full name does not follow the
// "<version>.<build>" release layout is reported by REST with the same version
// /sdk about reports, not with the build text left on it.
func TestRESTVersionMatchesAboutForCustomVersion(t *testing.T) {
	topo := topology.Phase1("rest-custom")
	topo.VCenterVersion = scenario.Version{Version: "9.2.0", Build: "99999", APIVersion: "9.2.0.0", FullName: "VMware vCenter Server 9.2.0 build-99999"}
	s, err := New(Options{Topology: topo})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	srv := httptest.NewTLSServer(s.Handler)
	defer srv.Close()
	login, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/session", nil)
	login.SetBasicAuth("someone", "anything")
	res, err := srv.Client().Do(login)
	if err != nil {
		t.Fatal(err)
	}
	var token string
	if err := json.NewDecoder(res.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/appliance/system/version", nil)
	req.Header.Set("vmware-api-session-id", token)
	res, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got map[string]string
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["version"] != "9.2.0" || got["name"] != "9.2.0" || got["build"] != "99999" {
		t.Fatalf("REST version: %v", got)
	}
}
