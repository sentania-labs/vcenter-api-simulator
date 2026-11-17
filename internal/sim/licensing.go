package sim

import (
	"bytes"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/signon"
)

// extensionWrites are the operations that can change the "license managed
// by" marker.
var extensionWrites = map[string]bool{"RegisterExtension": true, "UpdateExtension": true, "UnregisterExtension": true}

// The defaults of a marker the operator described with an id and an address
// only: what a real vCenter showed for the marker a VCF Operations left.
const (
	managerLabel   = "VMware Cloud Foundation Operations"
	managerCompany = "Broadcom, Inc."
)

// ownSettings gives the simulated vCenter its own copy of vcsim's advanced
// settings. vcsim shares one list between every instance in the process and
// writes to it when an instance starts.
func ownSettings(m *simulator.Model) {
	om := m.Service.Context.Map.OptionManager()
	own := make([]types.BaseOptionValue, 0, len(om.Setting))
	for _, o := range om.Setting {
		v := *o.GetOptionValue()
		own = append(own, &v)
	}
	om.Setting = own
}

// signOnSettings points the vCenter's three sign-on settings at the paths
// the simulator serves, under the address tools use when one is known. It
// runs after vcsim's own server has started, because starting it rewrites
// these settings to that server's internal address.
func signOnSettings(m *simulator.Model, public string) {
	base := "https://127.0.0.1"
	if u, err := url.Parse(public); err == nil && u.Scheme == "https" && validHost(u.Host) {
		base = "https://" + u.Host
	}
	values := map[string]string{
		"config.vpxd.sso.sts.uri":        base + stsPath,
		"config.vpxd.sso.admin.uri":      base + ssoAdminPath,
		"config.vpxd.sso.groupcheck.uri": base + ssoAdminPath,
		"config.vpxd.sso.default.admin":  "Administrator@" + signon.SystemDomain,
	}
	for _, o := range m.Service.Context.Map.OptionManager().Setting {
		if v := o.GetOptionValue(); values[v.Key] != "" {
			v.Value = values[v.Key]
		}
	}
}

// SetLicensing changes how the vCenter presents its licensing while it
// runs. Sessions, inventory and history are left alone.
func (s *Simulator) SetLicensing(l scenario.Licensing) {
	h := s.Handler.(*handler)
	stored := l
	h.licensing.Store(&stored)
	h.managerMu.Lock()
	defer h.managerMu.Unlock()
	want := l.Manager
	h.manager = nil
	reg := s.model.Service.Context.Map
	em := reg.ExtensionManager()
	reg.WithLock(&simulator.Context{}, em, func() {
		var kept []types.Extension
		for _, x := range em.ExtensionList {
			if x.Key != scenario.LicenseManagedKey {
				kept = append(kept, x)
				continue
			}
			// what a tool registered itself stays as it registered it
			if want != nil && sameManager(managerOf(x), want) {
				kept = append(kept, x)
				h.manager, want = managerOf(x), nil
			}
		}
		if want != nil {
			x := managerExtension(want, s.Topology.VCenterVersion.Version, s.Env.Now().UTC())
			kept = append(kept, x)
			// remembered as the vCenter now holds it, so that only a
			// change a tool makes is ever reported as one
			h.manager = managerOf(x)
		}
		em.ExtensionList = kept
	})
}

// managerExtension is the marker extension for a described manager: the
// elements a real vCenter showed for the marker a VCF Operations left, with
// the id and address the scenario gives. The subject name is left out
// unless an Operations registered one.
func managerExtension(m *scenario.LicenseManager, version string, now time.Time) types.Extension {
	x := types.Extension{
		Description:       &types.Description{Label: m.Label, Summary: m.Summary},
		Key:               scenario.LicenseManagedKey,
		Company:           m.Company,
		Version:           m.Version,
		SubjectName:       m.SubjectName,
		LastHeartbeatTime: now,
		ManagedEntityInfo: []types.ExtManagedEntityInfo{
			{Type: "LM_IDENTITY", Description: m.Identity},
			{Type: "LM_URL", Description: m.URL},
		},
		ShownInSolutionManager: types.NewBool(false),
	}
	d := x.Description.(*types.Description)
	if d.Label == "" {
		d.Label = managerLabel
	}
	if d.Summary == "" {
		d.Summary = d.Label
	}
	if x.Company == "" {
		x.Company = managerCompany
	}
	if x.Version == "" {
		x.Version = version
	}
	return x
}

// reportLicenseManager tells the owner of the scenario when a tool has
// registered, changed or removed the marker, so that the change outlives
// the running vCenter.
//
// The report carries nothing: the owner reads the marker with
// LicenseManager when the report reaches it. Reports are delivered on
// their own goroutines and may arrive in any order, and a report that
// carried the marker as it was when the report was made could then put an
// older marker over a newer one.
func (h *handler) reportLicenseManager() {
	if h.managerChanged == nil {
		return
	}
	h.managerMu.Lock()
	defer h.managerMu.Unlock()
	now := h.currentManager()
	if (now == nil) == (h.manager == nil) && (now == nil || *now == *h.manager) {
		return
	}
	h.manager = now
	go h.managerChanged()
}

// currentManager reads the marker the vCenter holds; nil when it holds none.
func (h *handler) currentManager() *scenario.LicenseManager {
	var now *scenario.LicenseManager
	em := h.registry.ExtensionManager()
	h.registry.WithLock(&simulator.Context{}, em, func() {
		for _, x := range em.ExtensionList {
			if x.Key == scenario.LicenseManagedKey {
				now = managerOf(x)
			}
		}
	})
	return now
}

// LicenseManager is the "license managed by" marker as the running vCenter
// holds it now, or nil when it holds none.
func (s *Simulator) LicenseManager() *scenario.LicenseManager {
	return s.Handler.(*handler).currentManager()
}

// SameLicenseManager reports whether the marker a vCenter holds is the one
// a scenario describes. A description that leaves a value out stands for
// the default a new marker gets.
func SameLicenseManager(held, described *scenario.LicenseManager) bool {
	if held == nil || described == nil {
		return held == nil && described == nil
	}
	return sameManager(held, described)
}

// sameManager reports whether a registered marker is the one described: a
// description that leaves a value out takes the default a new marker gets.
func sameManager(have, want *scenario.LicenseManager) bool {
	if *have == *want {
		return true
	}
	return *have == *managerOf(managerExtension(want, have.Version, time.Time{}))
}

// plainDescription holds a FindExtension answer back until it is complete
// and removes the type attribute vcsim writes on an extension's description.
// A real vCenter writes the element bare, and the attribute says nothing: it
// names the type the element already has.
var typedDescription = regexp.MustCompile(`<description xmlns:[A-Za-z_][A-Za-z0-9_.-]*="http://www\.w3\.org/2001/XMLSchema-instance" [A-Za-z_][A-Za-z0-9_.-]*:type="Description">`)

type plainDescription struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (p *plainDescription) WriteHeader(status int) {
	if p.status == 0 {
		p.status = status
	}
}

func (p *plainDescription) Write(b []byte) (int, error) { return p.body.Write(b) }

func (p *plainDescription) finish() {
	out := typedDescription.ReplaceAll(p.body.Bytes(), []byte(`<description>`))
	p.Header().Del("Content-Length")
	if p.status == 0 {
		p.status = http.StatusOK
	}
	p.ResponseWriter.WriteHeader(p.status)
	_, _ = p.ResponseWriter.Write(out)
}

// managerOf reads the manager a marker extension names.
func managerOf(x types.Extension) *scenario.LicenseManager {
	m := &scenario.LicenseManager{Company: x.Company, Version: x.Version, SubjectName: x.SubjectName}
	if x.Description != nil {
		d := x.Description.GetDescription()
		m.Label, m.Summary = d.Label, d.Summary
	}
	for _, info := range x.ManagedEntityInfo {
		switch info.Type {
		case "LM_IDENTITY":
			m.Identity = strings.TrimSpace(info.Description)
		case "LM_URL":
			m.URL = strings.TrimSpace(info.Description)
		}
	}
	return m
}
