package sim

import (
	"context"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"

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
}
