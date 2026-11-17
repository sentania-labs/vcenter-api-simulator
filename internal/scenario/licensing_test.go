package scenario

import (
	"encoding/json"
	"strings"
	"testing"
)

const licensingBase = "seed: licensing\ndatacenters:\n  - name: dc-01\n    clusters: 1\n"

// A scenario that says nothing about licensing is in evaluation, with the
// four groups a real vCenter listed and no manager.
func TestLicensingDefaults(t *testing.T) {
	s, err := Parse([]byte(licensingBase))
	if err != nil {
		t.Fatal(err)
	}
	l := s.Licensing
	if l.Expired() || l.Manager != nil || l.WithoutLicensingGroups {
		t.Errorf("defaults: %+v", l)
	}
	if got, want := strings.Join(l.TokenGroups(), ","), "Administrators,Everyone,LicenseService.Administrators,SystemConfiguration.Administrators"; got != want {
		t.Errorf("groups %s, want %s", got, want)
	}
	// and it leaves no trace in the saved form, so an older simulator reads
	// a state file this one wrote
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"licensing":`) {
		t.Errorf("a default scenario's JSON names licensing: %s", b)
	}
}

func TestLicensingFromTheFile(t *testing.T) {
	s, err := Parse([]byte(licensingBase + `licensing:
  state: expired-evaluation
  withoutLicensingGroups: true
  groups: [Everyone, Administrators, Operators, LicenseService.Administrators]
  manager:
    identity: 11111111-2222-4333-8444-555555555555
    url: https://operations.example.test
`))
	if err != nil {
		t.Fatal(err)
	}
	l := s.Licensing
	if !l.Expired() || l.Manager == nil || l.Manager.Identity != "11111111-2222-4333-8444-555555555555" || l.Manager.URL != "https://operations.example.test" {
		t.Errorf("parsed: %+v", l)
	}
	// the switch drops the two licensing groups and keeps the order of the rest
	if got, want := strings.Join(l.TokenGroups(), ","), "Everyone,Operators"; got != want {
		t.Errorf("groups %s, want %s", got, want)
	}
	// the saved form reads back the same
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back Scenario
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Licensing.State != l.State || *back.Licensing.Manager != *l.Manager || strings.Join(back.Licensing.Groups, ",") != strings.Join(l.Groups, ",") || !back.Licensing.WithoutLicensingGroups {
		t.Errorf("read back: %+v", back.Licensing)
	}
}

func TestLicensingIsValidated(t *testing.T) {
	for name, text := range map[string]string{
		"a state there is not":  "licensing:\n  state: licensed\n",
		"a group listed twice":  "licensing:\n  groups: [Everyone, Everyone]\n",
		"a group with a slash":  "licensing:\n  groups: ['vsphere.local\\Everyone']\n",
		"an empty group":        "licensing:\n  groups: ['']\n",
		"a manager line break":  "licensing:\n  manager:\n    identity: \"a\\nb\"\n    url: https://operations.example.test\n",
		"a field there is not":  "licensing:\n  signedEntitlement: abc\n",
		"a manager field wrong": "licensing:\n  manager:\n    entitlement: abc\n",
	} {
		if _, err := Parse([]byte(licensingBase + text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// what a tool registered for itself is kept as it is, even when it is
	// not an id or an address an operator could have typed
	if _, err := Parse([]byte(licensingBase + "licensing:\n  manager:\n    identity: not-a-uuid\n    url: ''\n")); err != nil {
		t.Errorf("a marker a tool registered: %v", err)
	}
}

func TestManagerValuesAnOperatorTypes(t *testing.T) {
	for _, ok := range []string{"11111111-2222-4333-8444-555555555555", "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"} {
		if !ValidManagerIdentity(ok) {
			t.Errorf("identity %q refused", ok)
		}
	}
	for _, bad := range []string{"", "operations", "11111111-2222-4333-8444-55555555555", "11111111-2222-4333-8444-5555555555555", " 11111111-2222-4333-8444-555555555555"} {
		if ValidManagerIdentity(bad) {
			t.Errorf("identity %q accepted", bad)
		}
	}
	for _, ok := range []string{"https://operations.example.test", "https://operations.example.test:8443/ui", "https://192.0.2.10"} {
		if !ValidManagerURL(ok) {
			t.Errorf("address %q refused", ok)
		}
	}
	for _, bad := range []string{"", "operations.example.test", "http://operations.example.test", "https://", "https://user:pw@operations.example.test", "https://operations.example.test/?a=b", "https://operations.example.test/#x", "https://operations example.test"} {
		if ValidManagerURL(bad) {
			t.Errorf("address %q accepted", bad)
		}
	}
}
