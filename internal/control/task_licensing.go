package control

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
)

// Licensing: what the vCenter tells a tool about its own license. Three
// things can be chosen, and none of them is a license: whether the
// evaluation is still running, whether the accounts tools sign in with may
// manage licenses, and which VCF Operations, if any, the vCenter says
// manages its license.

func init() {
	tasks["licensing"] = taskDef{form: formLicensing, plan: planLicensing}
}

// exampleManagerURL is the address the form suggests for another VCF
// Operations: a name that can never be a real system's.
const exampleManagerURL = "https://operations.example.test"

type licensingParams struct {
	State    string `json:"state"`    // evaluation | expired
	Groups   string `json:"groups"`   // in | out
	Manager  string `json:"manager"`  // none | other
	Identity string `json:"identity"` // the other Operations' id
	URL      string `json:"url"`      // its address
}

// licensingValues are the choices as they stand.
func licensingValues(l scenario.Licensing) licensingParams {
	v := licensingParams{State: "evaluation", Groups: "in", Manager: "none"}
	if l.Expired() {
		v.State = "expired"
	}
	if l.WithoutLicensingGroups {
		v.Groups = "out"
	}
	if l.Manager != nil {
		v.Manager, v.Identity, v.URL = "other", l.Manager.Identity, l.Manager.URL
	}
	return v
}

func formLicensing(c *Controller, _ map[string]string, _ Caller) (*Form, error) {
	r := c.applied()
	cur := licensingValues(r.sc.Licensing)
	values := map[string]any{"state": cur.State, "groups": cur.Groups, "manager": cur.Manager, "identity": cur.Identity, "url": cur.URL}
	if cur.Manager == "none" {
		// already answered: a made-up id that belongs to this simulator, and
		// an address that is nobody's
		values["identity"], values["url"] = r.topo.ManagerID, exampleManagerURL
	}
	return &Form{Title: "Change how licensing looks to tools", Crumbs: [][2]string{{"Security", "/security"}},
		Intro:   "What this vCenter tells a tool about its own license. None of this is a license: the simulator can say it is in evaluation, but it cannot say it is licensed.",
		NowNote: "Tools stay signed in. They see the change the next time they ask.",
		Values:  values,
		Options: map[string]any{"current": cur}}, nil
}

func planLicensing(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in licensingParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	p := &Plan{Go: "Change how licensing looks", dest: "/security"}
	r := c.applied()
	cur := licensingValues(r.sc.Licensing)
	next := r.sc.Licensing
	switch in.State {
	case "evaluation":
		next.State = ""
	case "expired":
		next.State = scenario.LicenseExpiredEvaluation
	default:
		p.bad("state", "Choose one of the two.")
	}
	switch in.Groups {
	case "in":
		next.WithoutLicensingGroups = false
	case "out":
		next.WithoutLicensingGroups = true
	default:
		p.bad("groups", "Choose one of the two.")
	}
	in.Identity, in.URL = strings.TrimSpace(in.Identity), strings.TrimSpace(in.URL)
	if cur.URL != in.URL {
		in.URL = strings.TrimRight(in.URL, "/")
	}
	switch in.Manager {
	case "none":
		next.Manager = nil
		in.Identity, in.URL = "", ""
	case "other":
		if next.Manager != nil && next.Manager.Identity == in.Identity && next.Manager.URL == in.URL {
			// the same one: it is kept as it is, with whatever it
			// registered itself with, and not checked again
			break
		}
		if !scenario.ValidManagerIdentity(in.Identity) {
			p.bad("identity", "Enter an id in the form 00000000-0000-0000-0000-000000000000. A made-up one is fine.")
		}
		if !scenario.ValidManagerURL(in.URL) {
			p.bad("url", "Enter an address that starts with https://, for example %s.", exampleManagerURL)
		}
		next.Manager = &scenario.LicenseManager{Identity: in.Identity, URL: in.URL}
	default:
		p.bad("manager", "Choose one of the two.")
	}
	if in == cur {
		p.Idle = "Nothing is different yet. Change an answer and this box will say what happens."
		return p, nil
	}
	if in.State != cur.State {
		ends := plainDate(sim.EvaluationEnds(next, c.now()))
		if next.Expired() {
			p.Sum = append(p.Sum, say("Tools that ask about the license are told the %s, on %s.", bold("evaluation has run out"), ends))
		} else {
			p.Sum = append(p.Sum, say("Tools that ask about the license are told vCenter is %s, with 90 days left. The 90 days never count down.", bold("in evaluation")))
		}
	}
	if in.Groups != cur.Groups {
		if next.WithoutLicensingGroups {
			p.Sum = append(p.Sum, say("The accounts tools sign in with are %s. A tool that checks, such as VCF Operations, warns that it cannot manage licenses with the account.", bold("no longer licensing administrators")))
		} else {
			p.Sum = append(p.Sum, say("The accounts tools sign in with are %s again, so a tool that checks finds it may manage licenses.", bold("licensing administrators")))
		}
	}
	if in.Manager != cur.Manager || in.Identity != cur.Identity || in.URL != cur.URL {
		if next.Manager == nil {
			p.Sum = append(p.Sum, say("vCenter no longer says that a VCF Operations manages its license."))
		} else {
			shown := in.URL
			if shown == "" {
				shown = "the address you enter"
			}
			p.Sum = append(p.Sum, say("vCenter says its license is %s. A VCF Operations with a different id sees a vCenter that belongs to another one.", bold("managed by the VCF Operations at "+shown)))
		}
	}
	p.Sum = append(p.Sum, note("Tools stay signed in. Clusters, hosts and history are not touched."),
		note("This is not a license. A licensed vCenter is not something the simulator can show."))
	if len(p.Errors) > 0 {
		return p, nil
	}
	p.text = "Change how licensing looks to tools"
	p.apply = func() (*Outcome, error) {
		sc := clone(c.applied().sc)
		sc.Licensing = next
		if _, err := c.applyScenario(sc, false); err != nil {
			return nil, err
		}
		return &Outcome{Done: "Licensing now looks different to tools. " + licensingSentence(next, c.now()), history: "Changed how licensing looks to tools. " + licensingSentence(next, c.now())}, nil
	}
	return p, nil
}

// licensingSentence says the three choices in one line.
func licensingSentence(l scenario.Licensing, now time.Time) string {
	state := "vCenter is in evaluation"
	if l.Expired() {
		state = "The evaluation has run out"
	}
	groups := "accounts are licensing administrators"
	if l.WithoutLicensingGroups {
		groups = "accounts are not licensing administrators"
	}
	manager := "no VCF Operations manages the license"
	if l.Manager != nil {
		manager = "the license is managed by the VCF Operations at " + l.Manager.URL
	}
	return fmt.Sprintf("%s, %s, and %s.", state, groups, manager)
}

// LicensingView is licensing as the Security screen shows it.
type LicensingView struct {
	// Expired: the evaluation has run out. Ends is when it runs out, or ran
	// out.
	Expired bool      `json:"expired"`
	Ends    time.Time `json:"ends"`
	// Admins: the accounts tools sign in with are licensing administrators.
	Admins bool `json:"admins"`
	// ManagerURL and ManagerID name the VCF Operations the vCenter says
	// manages its license; both are empty when it names none.
	ManagerURL string `json:"managerUrl,omitempty"`
	ManagerID  string `json:"managerId,omitempty"`
	Managed    bool   `json:"managed"`
}

func (c *Controller) licensingView() LicensingView {
	l := c.applied().sc.Licensing
	v := LicensingView{Expired: l.Expired(), Ends: sim.EvaluationEnds(l, c.now()), Admins: !l.WithoutLicensingGroups}
	if l.Manager != nil {
		v.Managed, v.ManagerURL, v.ManagerID = true, l.Manager.URL, l.Manager.Identity
	}
	return v
}

// licenseManagerChanged records that a tool registered, changed or removed
// the "license managed by" marker on the running vCenter, so that the
// marker is still there after a restart or a change that builds a new
// vCenter. from is the vCenter the tool spoke to.
func (c *Controller) licenseManagerChanged(from *sim.Simulator, m *scenario.LicenseManager) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.applied()
	if cur == nil || cur.sim != from {
		return // a vCenter that has since been replaced
	}
	have := cur.sc.Licensing.Manager
	if (have == nil) == (m == nil) && (m == nil || *have == *m) {
		return
	}
	next := clone(cur.sc)
	next.Licensing.Manager = m
	if err := clone(next).Validate(); err != nil {
		// what the tool registered cannot be kept; the running vCenter
		// still has it
		log.Printf("control: license manager marker not saved: %v", err)
		return
	}
	c.cur.Store(&running{version: cur.version, at: cur.at, sc: next, topo: cur.topo, sim: cur.sim})
	switch {
	case m == nil:
		c.addHistory("A tool removed the mark that says a VCF Operations manages this vCenter's license.")
	case m.URL != "":
		c.addHistory(fmt.Sprintf("A tool marked this vCenter's license as managed by the VCF Operations at %s.", m.URL))
	default:
		c.addHistory("A tool marked this vCenter's license as managed by a VCF Operations.")
	}
	c.save()
}
