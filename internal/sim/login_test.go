package sim

import (
	"context"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"

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
