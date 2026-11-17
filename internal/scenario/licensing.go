package scenario

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Licensing is how the simulated vCenter presents its own licensing: the
// state its license service reports, the groups its sign-on tokens list, and
// which VCF Operations, if any, it says manages its license.
//
// Nothing here is a license. The simulator presents an evaluation, which a
// vCenter can do with no entitlement at all; a licensed vCenter presents a
// signed entitlement that only the vendor can issue, and that is not built.
type Licensing struct {
	// State is "evaluation" (the default: in evaluation, running out in the
	// future) or "expired-evaluation" (the evaluation has run out).
	State string `yaml:"state,omitempty" json:"state,omitempty"`
	// Groups are the groups of the sign-on domain a token lists for an
	// administrator. Empty means the four DefaultGroups.
	Groups []string `yaml:"groups,omitempty" json:"groups,omitempty"`
	// WithoutLicensingGroups leaves LicensingGroups out of every token, so
	// that a tool sees an account that may not manage licenses.
	WithoutLicensingGroups bool `yaml:"withoutLicensingGroups,omitempty" json:"withoutLicensingGroups,omitempty"`
	// Manager is the VCF Operations the vCenter says manages its license;
	// nil means none does.
	Manager *LicenseManager `yaml:"manager,omitempty" json:"manager,omitempty"`
}

// LicenseManager is the "license managed by" marker a VCF Operations leaves
// on a vCenter: the extension com.vmware.vrops.license.managed. Identity and
// URL are whatever the operator typed, or whatever an Operations registered
// itself with; the simulator never supplies a real system's values.
type LicenseManager struct {
	// Identity is LM_IDENTITY: the id of the Operations that manages the
	// license.
	Identity string `yaml:"identity" json:"identity"`
	// URL is LM_URL: that Operations' address.
	URL string `yaml:"url" json:"url"`
	// The rest is what the Operations registered itself with. Empty values
	// take the simulator's defaults (label, company and version) or stay
	// out of the answer (subject name).
	Label       string `yaml:"label,omitempty" json:"label,omitempty"`
	Summary     string `yaml:"summary,omitempty" json:"summary,omitempty"`
	Company     string `yaml:"company,omitempty" json:"company,omitempty"`
	Version     string `yaml:"version,omitempty" json:"version,omitempty"`
	SubjectName string `yaml:"subjectName,omitempty" json:"subjectName,omitempty"`
}

// License states.
const (
	LicenseEvaluation        = "evaluation"
	LicenseExpiredEvaluation = "expired-evaluation"
)

// LicenseManagedKey is the extension a VCF Operations registers on a vCenter
// whose license it manages.
const LicenseManagedKey = "com.vmware.vrops.license.managed"

// DefaultGroups are the groups a token lists for an administrator unless the
// scenario lists its own.
var DefaultGroups = []string{"Administrators", "Everyone", "LicenseService.Administrators", "SystemConfiguration.Administrators"}

// LicensingGroups are the two groups either of which makes an account a
// licensing administrator.
var LicensingGroups = []string{"LicenseService.Administrators", "Administrators"}

// Expired reports whether the evaluation has run out.
func (l Licensing) Expired() bool { return l.State == LicenseExpiredEvaluation }

// TokenGroups are the groups a token lists for an administrator.
func (l Licensing) TokenGroups() []string {
	groups := l.Groups
	if len(groups) == 0 {
		groups = DefaultGroups
	}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		if l.WithoutLicensingGroups && IsLicensingGroup(g) {
			continue
		}
		out = append(out, g)
	}
	return out
}

// IsLicensingGroup reports one of the LicensingGroups.
func IsLicensingGroup(name string) bool {
	for _, g := range LicensingGroups {
		if g == name {
			return true
		}
	}
	return false
}

var (
	groupRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]{0,62}[A-Za-z0-9]$|^[A-Za-z0-9]$`)
	identityRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// ValidManagerIdentity reports whether s has the form of an Operations id:
// a UUID.
func ValidManagerIdentity(s string) bool { return identityRe.MatchString(s) }

// ValidManagerURL reports whether s is an https address with a host and
// nothing after it but an optional path.
func ValidManagerURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && !strings.ContainsAny(s, " \t\r\n<>\"")
}

func (l Licensing) validate() error {
	switch l.State {
	case "", LicenseEvaluation, LicenseExpiredEvaluation:
	default:
		return fmt.Errorf("scenario: licensing.state %q is not one of %s, %s", l.State, LicenseEvaluation, LicenseExpiredEvaluation)
	}
	seen := map[string]bool{}
	for _, g := range l.Groups {
		if !groupRe.MatchString(g) {
			return fmt.Errorf("scenario: licensing.groups: %q is not a group name (letters, digits, dots, hyphens, underscores and spaces)", g)
		}
		if seen[g] {
			return fmt.Errorf("scenario: licensing.groups: %q listed twice", g)
		}
		seen[g] = true
	}
	if m := l.Manager; m != nil {
		// A tool may have registered the marker itself, with whatever it
		// chose to say, so only the size and the line breaks are checked
		// here. The console checks what an operator types more closely.
		for name, v := range map[string]string{"identity": m.Identity, "url": m.URL, "label": m.Label, "summary": m.Summary, "company": m.Company, "version": m.Version, "subjectName": m.SubjectName} {
			if len(v) > 2048 || strings.ContainsAny(v, "\r\n\t") {
				return fmt.Errorf("scenario: licensing.manager.%s is too long or holds a line break", name)
			}
		}
	}
	return nil
}
