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
	"8.0u3": {Preset: "8.0u3", Version: "8.0.3", Build: "24022510", FullName: "VMware ESXi 8.0.3 build-24022510"},
	"9.0":   {Preset: "9.0", Version: "9.0.0", Build: "24755229", FullName: "VMware ESX 9.0.0 build-24755229"},
	"9.1.1": {Preset: "9.1.1", Version: "9.1.1", Build: "25782009", APIVersion: "9.1.1.0", FullName: "VMware ESXi 9.1.1.0100.25782009"},
}

func (v Version) empty() bool { return v == Version{} }
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
	if fallback == "" && s.ESX.empty() {
		return Version{}, fmt.Errorf("custom vCenter version requires an ESX version")
	}
	spec := s.ESX
	for _, c := range s.Clusters {
		if c.Key == cluster && !c.ESX.empty() {
			spec = c.ESX
		}
	}
	if h, ok := s.HostVersions[host]; ok {
		spec = h
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
			return fmt.Errorf("host %s version %s is newer than vCenter %s; set forceNewerHosts to allow it", host, v.Version, vc.Version)
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
