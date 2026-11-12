package scenario

import (
	"fmt"
	"strconv"
	"strings"
)

// Version selects a named release or provides explicit product metadata.
type Version struct {
	Preset     string `yaml:"preset,omitempty" json:"preset,omitempty"`
	Version    string `yaml:"version,omitempty" json:"version,omitempty"`
	Build      string `yaml:"build,omitempty" json:"build,omitempty"`
	APIVersion string `yaml:"apiVersion,omitempty" json:"apiVersion,omitempty"`
	FullName   string `yaml:"fullName,omitempty" json:"fullName,omitempty"`
}

// Release/build pairs are published by Broadcom's vCenter and ESX build tables.
// The 9.1.1 strings are additionally backed by the repository's live capture.
var vcVersions = map[string]Version{
	"8.0u3": {Preset: "8.0u3", Version: "8.0.3", Build: "24022515", APIVersion: "8.0.3.0", FullName: "VMware vCenter Server 8.0.3.00000.24022515"},
	"9.0":   {Preset: "9.0", Version: "9.0.0", Build: "24755230", APIVersion: "9.0.0.0", FullName: "VMware vCenter Server 9.0.0.0.24755230"},
	"9.1.1": {Preset: "9.1.1", Version: "9.1.1", Build: "25784172", APIVersion: "9.1.1.0", FullName: "VMware vCenter Server 9.1.1.0100.25784172"},
}
var esxVersions = map[string]Version{
	"8.0u3": {Preset: "8.0u3", Version: "8.0.3", Build: "24022510", APIVersion: "8.0.3.0", FullName: "VMware ESXi 8.0.3 build-24022510"},
	"9.0":   {Preset: "9.0", Version: "9.0.0", Build: "24755229", APIVersion: "9.0.0.0", FullName: "VMware ESX 9.0.0 build-24755229"},
	"9.1.1": {Preset: "9.1.1", Version: "9.1.1", Build: "25782009", APIVersion: "9.1.1.0", FullName: "VMware ESXi 9.1.1.0100.25782009"},
}

func (v Version) empty() bool { return v == Version{} }

// LicenseVersion is the major.minor release used as licenseProductVersion.
func (v Version) LicenseVersion() string {
	parts := strings.Split(v.Version, ".")
	if len(parts) < 2 {
		return v.Version
	}
	return parts[0] + "." + parts[1]
}
func resolveVersion(v Version, presets map[string]Version, fallback string) (Version, error) {
	if v.empty() {
		v.Preset = fallback
	}
	if v.Preset != "" {
		base, ok := presets[v.Preset]
		if !ok {
			return Version{}, fmt.Errorf("unknown version preset %q", v.Preset)
		}
		if v.Version != "" || v.Build != "" || v.APIVersion != "" || v.FullName != "" {
			return Version{}, fmt.Errorf("preset cannot be combined with custom version fields")
		}
		return base, nil
	}
	if v.Version == "" || v.Build == "" || v.APIVersion == "" || v.FullName == "" {
		return Version{}, fmt.Errorf("custom version requires version, build, apiVersion and fullName")
	}
	return v, nil
}

// VersionPresets lists selectable vCenter and host releases in ascending order.
func VersionPresets() (vcenter, esx []Version) {
	for _, id := range []string{"8.0u3", "9.0", "9.1.1"} {
		vcenter = append(vcenter, vcVersions[id])
		esx = append(esx, esxVersions[id])
	}
	return
}

func (s *Scenario) VCenterVersion() (Version, error) {
	return resolveVersion(s.VCenter, vcVersions, "9.1.1")
}
func (s *Scenario) HostVersion(cluster, host string) (Version, error) {
	vc, err := s.VCenterVersion()
	if err != nil {
		return Version{}, err
	}
	fallback := vc.Preset
	spec := s.ESX
	for _, c := range s.Clusters {
		if c.Key == cluster && !c.ESX.empty() {
			spec = c.ESX
		}
	}
	if h, ok := s.HostVersions[host]; ok {
		spec = h
	}
	if spec.empty() && fallback == "" {
		return Version{Version: vc.Version, Build: vc.Build, APIVersion: vc.APIVersion, FullName: "VMware ESXi " + vc.Version + " build-" + vc.Build}, nil
	}
	return resolveVersion(spec, esxVersions, fallback)
}
func versionNumbers(v string) ([]int, error) {
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("version %q must contain at least major and minor", v)
	}
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid version %q", v)
		}
		out[i] = n
	}
	return out, nil
}
func VersionNewer(a, b string) (bool, error) {
	x, err := versionNumbers(a)
	if err != nil {
		return false, err
	}
	y, err := versionNumbers(b)
	if err != nil {
		return false, err
	}
	for i := 0; i < max(len(x), len(y)); i++ {
		p, q := 0, 0
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			return p > q, nil
		}
	}
	return false, nil
}
func (s *Scenario) validateVersions() error {
	vc, err := s.VCenterVersion()
	if err != nil {
		return err
	}
	if _, err = versionNumbers(vc.Version); err != nil {
		return err
	}
	check := func(cluster, host string) error {
		v, err := s.HostVersion(cluster, host)
		if err != nil {
			return err
		}
		isNewer, err := VersionNewer(v.Version, vc.Version)
		if err != nil {
			return err
		}
		if isNewer && !s.ForceNewerHosts {
			where := "Default ESX version"
			if host != "" {
				where = "Host " + host
			} else if cluster != "" {
				where = "ESX version for " + cluster
			}
			return fmt.Errorf("%s (%s) is newer than vCenter %s. Select Allow hosts newer than vCenter to continue.", where, v.Version, vc.Version)
		}
		return nil
	}
	if err := check("", ""); err != nil {
		return err
	}
	for _, c := range s.Clusters {
		if err := check(c.Key, ""); err != nil {
			return err
		}
	}
	for key := range s.HostVersions {
		i := strings.LastIndex(key, "/")
		if i < 0 {
			return fmt.Errorf("invalid host version key %q", key)
		}
		if err := check(key[:i], key); err != nil {
			return err
		}
	}
	return nil
}

// VersionLabel is the release as a person says it: "8.0 Update 3", "9.0",
// "9.1.1", or the number itself for a release that is not a preset.
func VersionLabel(v Version) string {
	switch v.Version {
	case "8.0.3":
		return "8.0 Update 3"
	case "9.0.0":
		return "9.0"
	case "":
		return v.Preset
	}
	return v.Version
}

// VersionChoice turns what the console offers (a preset id such as "8.0u3",
// or a typed number such as "9.2.0") into a Version for the scenario. A
// typed number that is one of the presets becomes that preset. For any
// other number the build, API version and product name are derived: they
// are plausible strings, not ones a real release carries.
func VersionChoice(product, choice string) (Version, error) {
	presets := vcVersions
	if product == "esx" {
		presets = esxVersions
	}
	if _, ok := presets[choice]; ok {
		return Version{Preset: choice}, nil
	}
	n, err := versionNumbers(choice)
	if err != nil || len(n) > 3 {
		return Version{}, fmt.Errorf("%q is not a version number like 9.2.0", choice)
	}
	for len(n) < 3 {
		n = append(n, 0)
	}
	number := fmt.Sprintf("%d.%d.%d", n[0], n[1], n[2])
	for id, p := range presets {
		if p.Version == number {
			return Version{Preset: id}, nil
		}
	}
	build := 24000000 + (n[0]-8)*1500000 + n[1]*120000 + n[2]*11000 + 515
	if build < 1000000 {
		build = 1000000 + n[0]*1000 + n[1]*100 + n[2]
	}
	v := Version{Version: number, APIVersion: number + ".0"}
	if product == "esx" {
		v.Build = strconv.Itoa(build - 6)
		v.FullName = "VMware ESXi " + number + " build-" + v.Build
	} else {
		v.Build = strconv.Itoa(build)
		v.FullName = "VMware vCenter Server " + number + " build-" + v.Build
	}
	return v, nil
}

// ResolveESX returns the full host version a scenario Version names.
func ResolveESX(v Version) (Version, error) { return resolveVersion(v, esxVersions, "") }
