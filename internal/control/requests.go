package control

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/activity"
	"github.com/sentania/vcenter-api-simulator/internal/querylog"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// Requests: what tools asked for in the last hour, grouped into kinds of
// thing a person would recognise, each with what happened and why. The
// headline, the Home screen and the tab's badge are all counted from the
// same groups, so they agree.

// licenseNote is why an unanswered license request matters even when the
// tool that asked carried on.
const licenseNote = "The vCenter adapter built into VCF Operations is different: it asks for the license first and will not start collecting without an answer."

// Ask is one kind of thing a tool asked for.
type Ask struct {
	ID   int    `json:"id"`
	T    string `json:"t"`
	Tech string `json:"tech"`
	N    int    `json:"n"`
	// St is the group it is filtered under: unknown, fault, denied or ok.
	St string `json:"st"`
	// Tone and Label are the badge; Short the words beside it; Long the
	// explanation in Details.
	Tone  string `json:"tone"`
	Label string `json:"label"`
	Short string `json:"short,omitempty"`
	Long  string `json:"long"`
	Who   string `json:"who"`
	User  string `json:"user,omitempty"`
	// Cond is the id of the switched-on condition that is failing it.
	Cond int       `json:"cond,omitempty"`
	Last time.Time `json:"last"`

	order  float64
	client string
	first  time.Time
}

type askKey struct {
	client, title, reason string
	cause                 int
}

// callTitle names one request in plain words, with what is special about
// it: which kind of performance data, or why a sign-in was refused.
func (c *Controller) callTitle(call activity.Call) string {
	switch {
	case call.Reason == activity.ReasonDenied && (call.Method == "Login" || call.Method == "POST /api/session"):
		switch {
		case call.User == "":
			return "Sign-in without a username, from " + call.Client
		case c.tools.wasRemoved(call.User):
			return fmt.Sprintf("Sign-in as %s, which has been removed", call.User)
		case !c.auth.Snapshot().Has(call.User):
			return fmt.Sprintf("Sign-in as %s, which is not a user here, from %s", call.User, call.Client)
		}
		return "Sign-in with a wrong password, from " + call.Client
	case call.Reason == activity.ReasonDenied:
		return "Requests made without signing in first"
	case call.Method == "VsanPerfQueryPerf" && call.Family != "":
		return "vSAN performance data for " + lower1(familyName(call.Family))
	case call.Method == "VsanPerfQueryPerf":
		return "vSAN performance data"
	}
	return kindOf(call)
}

// asks groups the last hour's requests. c.mu is held.
func (c *Controller) asks() []Ask {
	now := c.now()
	sc := c.applied().sc
	calls := c.Log.Since(now.Add(-time.Hour))
	type group struct {
		Ask
		families map[string]bool
		techs    map[string]bool
		perf     bool
	}
	groups := map[askKey]*group{}
	var order []askKey
	lastGood := map[string]time.Time{} // per client: its last request that got an answer
	addrTools := map[string]int{}
	for _, t := range c.toolViews() {
		addrTools[t.Addr]++
	}
	for _, call := range calls {
		ck := toolKey(call.Client, call.Agent)
		switch call.Reason {
		case activity.ReasonAnswered, activity.ReasonEmpty, activity.ReasonRefused:
			lastGood[ck] = call.Time
		}
		perf := call.Method == "VsanPerfQueryPerf"
		title := c.callTitle(call)
		if perf && call.Reason != activity.ReasonDenied {
			title = "perf" // one row per outcome; worded once the kinds are counted
		}
		if call.Reason == activity.ReasonPaused {
			title = "Everything asked for while the simulator was paused"
		}
		key := askKey{ck, title, call.Reason, call.Cause}
		g := groups[key]
		if g == nil {
			g = &group{families: map[string]bool{}, techs: map[string]bool{}, perf: perf && title == "perf"}
			g.T, g.client, g.first, g.User, g.Cond = title, ck, call.Time, call.User, call.Cause
			g.Who = "someone else, at " + call.Client
			if c.tools.known(call.Client, call.Agent) {
				g.Who = "the tool at " + call.Client
				if addrTools[call.Client] > 1 && call.Agent != "" {
					g.Who += ` ("` + call.Agent + `")`
				}
			}
			groups[key] = g
			order = append(order, key)
		}
		g.N++
		g.Last = call.Time
		if call.User != "" {
			g.User = call.User
		}
		if call.Family != "" {
			g.families[call.Family] = true
		}
		g.techs[techName(call)] = true
	}
	ended := map[int]EndedCondition{}
	for _, e := range c.ended {
		ended[e.ID] = e
	}
	on := map[int]scenario.Condition{}
	for _, cond := range sc.Conditions {
		on[cond.ID] = cond
	}
	out := make([]Ask, 0, len(order))
	for i, key := range order {
		g := groups[key]
		a := g.Ask
		a.ID = i + 1
		var fams []string
		for f := range g.families {
			fams = append(fams, f)
		}
		sort.Strings(fams)
		var techs []string
		for t := range g.techs {
			techs = append(techs, t)
		}
		sort.Strings(techs)
		a.Tech = strings.Join(techs[:min(len(techs), 4)], ", ")
		if len(techs) > 4 {
			a.Tech += ", and " + plural(len(techs)-4, "more")
		}
		if g.perf {
			a.T, a.Tech = perfTitle(sc, key.reason, fams, on[key.cause]), "VsanPerfQueryPerf"
			if len(fams) > 0 {
				a.Tech += ": " + strings.Join(fams[:min(len(fams), 6)], ", ")
				if len(fams) > 6 {
					a.Tech += ", and " + plural(len(fams)-6, "more")
				}
			}
		}
		switch key.reason {
		case activity.ReasonAnswered:
			a.St, a.Tone, a.Label, a.order = "ok", "ok", "Answered", 3
			a.Long = "The simulator gave a full answer."
		case activity.ReasonEmpty:
			a.St, a.Tone, a.Label, a.order = "ok", "ok", "Answered", 4
			a.Short = `The answer was "there are none", which is normal here`
			a.Long = `Answered with "there are none". That is normal: a real vCenter says the same for this kind of cluster.`
		case activity.ReasonRefused:
			a.St, a.Tone, a.Label, a.order = "ok", "ok", "Answered", 5
			a.Short = `The answer was "not supported", as on a real vCenter`
			a.Long = "Answered with a refusal. That is normal: a real vCenter refuses these requests too, so your tool already copes with it."
		case activity.ReasonFault:
			a.St, a.Tone, a.Label, a.order = "fault", "warn", "Failing on purpose", 1
			a.Long = "You switched a fault on. Turn it off on the Slowness and faults screen."
			if e, was := ended[key.cause]; was {
				a.St, a.Tone, a.Label, a.order, a.Cond = "ok", "ok", "Answered now", 2.5, 0
				a.Short = "Failed on purpose until " + timeToken(e.At)
				a.Long = fmt.Sprintf("Answered now. Until %s these requests failed on purpose, because of a fault you had switched on.", timeToken(e.At))
			} else if _, still := on[key.cause]; !still {
				a.Cond = 0
			}
		case activity.ReasonPaused:
			a.St, a.Tone, a.Label, a.order, a.Cond = "fault", "warn", "Failing on purpose", 1, 0
			a.Tech = "every request"
			a.Short = "Refused while the simulator was paused"
			a.Long = "You paused the simulator, so it dropped these connections. Resume it and requests are answered again."
		case activity.ReasonDenied:
			a.St, a.Tone, a.Label, a.order = "denied", "warn", "Sign-in refused", 2
			switch {
			case strings.Contains(a.T, "has been removed"):
				a.Short = "The user was removed"
				a.Long = "This user was removed on the Users screen, so the sign-in is refused. Give the tool another account, or add the user again."
			case strings.Contains(a.T, "not a user here") || strings.Contains(a.T, "without a username"):
				a.Short = "There is no such user"
				a.Long = "No user has that name. Check the username the tool uses, or add the user on the Users screen."
			case strings.HasPrefix(a.T, "Requests made without"):
				a.Short = "Not signed in"
				a.Long = "These requests carried no sign-in, so they were refused, as a real vCenter refuses them. A tool does this when its sign-in failed or its session ended."
			default:
				a.Short = "Wrong password"
				a.Long = "The password was wrong. If this is your tool, check the password it uses."
			}
		default:
			a.St, a.Tone, a.Label, a.order = "unknown", "bad", "Not answered", 0
			carried := lastGood[g.client].After(g.first)
			license := a.T == kindLicense
			switch {
			case carried && license:
				a.Short = "The tool carried on. VCF Operations' own adapter would stop"
			case carried:
				a.Short = "The tool carried on without it"
			default:
				a.Short = "The tool asked for nothing more afterwards"
			}
			a.Long = "The simulator does not know how to answer this yet."
			if carried {
				a.Long += " The tool that asked carried on collecting anyway, so it does not seem to depend on the answer."
			} else {
				a.Long += " The tool that asked has not been answered anything since, so it may depend on this."
			}
			if license {
				a.Long += " " + licenseNote
			}
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].order < out[j].order })
	return out
}

// perfTitle words one outcome of vSAN performance queries.
func perfTitle(sc *scenario.Scenario, reason string, fams []string, cond scenario.Condition) string {
	kinds := plural(len(fams), "kind")
	named := func() string {
		var names []string
		for _, f := range fams {
			names = append(names, lower1(familyName(f)))
		}
		return listAnd(names)
	}
	switch reason {
	case activity.ReasonEmpty:
		if len(fams) > 0 && len(fams) <= 2 {
			return "vSAN performance data for " + named()
		}
		return "vSAN performance data that has nothing to report here, " + kinds
	case activity.ReasonRefused:
		return "vSAN performance data that a real vCenter also refuses, " + kinds
	case activity.ReasonFault:
		title := "vSAN performance data, " + kinds
		if len(fams) == 1 {
			title = "vSAN performance data for " + named()
		}
		if len(cond.Clusters) > 0 {
			title += ", on " + listAnd(clusterLabels(sc, cond.Clusters))
		}
		return title
	}
	if len(fams) == 1 {
		return "vSAN performance data for " + named()
	}
	return "vSAN performance data, " + kinds
}

// Verdict is the Requests screen's headline.
type Verdict struct {
	// Tone is none (nothing has ever connected), bad, warn or ok.
	Tone string `json:"tone"`
	Big  string `json:"big"`
	Line string `json:"line"`
}

func countAsks(asks []Ask) map[string]int {
	n := map[string]int{"all": len(asks), "unknown": 0, "fault": 0, "denied": 0, "ok": 0}
	for _, a := range asks {
		n[a.St]++
	}
	return n
}

// verdict is computed from the same groups the list shows.
func (c *Controller) verdict(asks []Ask, tools []ToolView) Verdict {
	n := countAsks(asks)
	if len(tools) == 0 && len(asks) == 0 {
		return Verdict{Tone: "none", Big: "Nothing has connected yet."}
	}
	var live []ToolView
	for _, t := range tools {
		if t.Connected() {
			live = append(live, t)
		}
	}
	who := "No tool has signed in yet."
	switch {
	case len(live) == 1:
		who = fmt.Sprintf("The tool at %s is connected.", live[0].Addr)
		if live[0].Agent != "" {
			who = fmt.Sprintf("The tool at %s (%q) is connected.", live[0].Addr, live[0].Agent)
		}
	case len(live) > 1:
		who = fmt.Sprintf("%d tools are connected.", len(live))
	case len(tools) > 0:
		t := tools[0]
		before := "before it was signed out"
		switch t.State {
		case ToolPaused:
			before = "before you paused the simulator"
		case ToolQuiet:
			before = "before it went quiet"
		case ToolDenied:
			before = "before its sign-in was refused"
		case ToolSignedOut:
			before = "before its sign-in ended"
		case ToolLeft:
			before = "before it signed out"
		}
		who = fmt.Sprintf("No tool is connected right now. This is what the tool at %s asked for %s.", t.Addr, before)
	}
	switch {
	case n["unknown"] > 0:
		carried := true
		for _, a := range asks {
			if a.St == "unknown" && strings.HasPrefix(a.Short, "The tool asked for nothing") {
				carried = false
			}
		}
		it, verb := "them", "are"
		if n["unknown"] == 1 {
			it, verb = "it", "is"
		}
		tail := " It carried on collecting without " + it + "."
		if !carried {
			tail = " A tool asked for nothing more after one of them, so it may depend on the answer."
		}
		return Verdict{Tone: "bad", Big: fmt.Sprintf("%s of request %s not being answered.", plural(n["unknown"], "kind"), verb), Line: who + tail}
	case n["fault"] > 0:
		why := "because of a fault you switched on"
		paused := true
		for _, a := range asks {
			if a.St == "fault" && !strings.Contains(a.Short, "paused") {
				paused = false
			}
		}
		if paused {
			why = "because you paused the simulator"
		}
		return Verdict{Tone: "warn", Big: "Everything is answered, except what you are failing on purpose.",
			Line: fmt.Sprintf("%s %s failing %s.", who, plural(n["fault"], "kind of request is", "kinds of request are"), why)}
	}
	line := who
	signIns, unsigned := 0, false
	for _, a := range asks {
		if a.St == "denied" {
			if strings.HasPrefix(a.T, "Sign-in") {
				signIns++
			} else {
				unsigned = true
			}
		}
	}
	if signIns > 0 {
		line += fmt.Sprintf(" %s refused: a wrong password, or a user that is not there.", plural(signIns, "kind of sign-in was", "kinds of sign-in were"))
	}
	if unsigned {
		line += " Some requests were refused because they were made without signing in first."
	}
	return Verdict{Tone: "ok", Big: "Everything asked for is being answered.", Line: line}
}

// CaptureView is "Request capture" on the Requests screen.
type CaptureView struct {
	Available  bool `json:"available"`
	Unanswered bool `json:"unanswered"`
	Full       bool `json:"full"`
	// Debug is the license-service request log on stdout.
	Debug bool `json:"debug"`
	// Kept and Kinds count the unanswered requests held; FullKept the
	// requests full capture holds.
	Kept      int        `json:"kept"`
	Kinds     int        `json:"kinds"`
	FullKept  int        `json:"fullKept"`
	FullSince *time.Time `json:"fullSince,omitempty"`
	Error     string     `json:"error,omitempty"`
}

func (c *Controller) captureView() CaptureView {
	if c.Capture == nil {
		return CaptureView{}
	}
	st := c.Capture.State()
	return CaptureView{Available: true, Unanswered: st.Settings.Unanswered, Full: st.Settings.Full, Debug: st.Settings.Debug, Kept: st.UnansweredHeld, Kinds: st.UnansweredKinds,
		FullKept: st.FullHeld, FullSince: st.FullSince, Error: st.Error}
}

// RequestsView is the Requests screen.
type RequestsView struct {
	Verdict   Verdict        `json:"verdict"`
	Counts    map[string]int `json:"counts"`
	Asks      []Ask          `json:"asks"`
	MultiTool bool           `json:"multiTool"`
	Capture   CaptureView    `json:"capture"`
	ReportURL string         `json:"reportUrl"`
}

// Requests returns what was asked for in the last hour.
func (c *Controller) Requests() RequestsView {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep()
	asks := c.asks()
	tools := c.toolViews()
	return RequestsView{Verdict: c.verdict(asks, tools), Counts: countAsks(asks), Asks: asks, MultiTool: len(tools) > 1,
		Capture: c.captureView(), ReportURL: c.opts.ReportURL}
}

// SetCapture turns one of the three kinds of capture on or off and returns
// the words that say what that did.
func (c *Controller) SetCapture(kind string, on bool) (string, error) {
	if c.Capture == nil {
		return "", invalid("Request capture is not available in this installation.")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.Capture.State()
	next := st.Settings
	switch kind {
	case "full":
		next.Full = on
	case "unanswered":
		next.Unanswered = on
	case "debug":
		next.Debug = on
	default:
		return "", invalid("There is no such kind of capture.")
	}
	if err := c.Capture.Set(next); err != nil {
		return "", err
	}
	kept := st.UnansweredHeld
	state := map[bool]string{true: "on", false: "off"}[on]
	if kind == "full" {
		c.addHistory("Capture of every request in full turned " + state + ".")
		c.save()
		if on {
			return `**Capturing.** Every request is captured from now on. Repeat the action in your tool, then download and turn this off. The Requests tab shows "Capturing all" while it is on.`, nil
		}
		return "**Stopped.** Full capture is off, and what it had captured was deleted.", nil
	}
	if kind == "debug" {
		c.addHistory("Recording of what tools ask about licensing turned " + state + ".")
		c.save()
		if on {
			return "**On.** What tools ask the simulator about licensing is recorded in the simulator's own log from now on, with passwords and sign-in tokens left out.", nil
		}
		return "**Off.** What tools ask the simulator about licensing is no longer recorded.", nil
	}
	c.addHistory("Capture of unanswered requests turned " + state + ".")
	c.save()
	if on {
		msg := "**On.** Requests that get no answer are captured from now on."
		if kept > 0 {
			msg += fmt.Sprintf(" The %s captured earlier are still here.", num(kept))
		}
		return msg, nil
	}
	msg := "**Off.** New unanswered requests will not be captured."
	if kept > 0 {
		msg += fmt.Sprintf(" The %s already captured are kept, and you can still download them.", num(kept))
	}
	return msg + " They are still counted at the top of this screen.", nil
}

// ClearCapture deletes everything capture holds.
func (c *Controller) ClearCapture() (string, error) {
	if c.Capture == nil {
		return "", invalid("Request capture is not available in this installation.")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Capture.Clear(); err != nil {
		return "", err
	}
	c.addHistory("Captured requests deleted.")
	c.save()
	return "**Deleted.** Nothing is held any more, and capture starts again from now.", nil
}

// ---------------------------------------------------------------------------
// every request, one at a time

// LogRow is one single request.
type LogRow struct {
	Seq     uint64    `json:"seq"`
	Time    time.Time `json:"time"`
	Who     string    `json:"who"`
	T       string    `json:"t"`
	Tech    string    `json:"tech"`
	Cluster string    `json:"cluster,omitempty"`
	St      string    `json:"st"`
	Tone    string    `json:"tone"`
	Label   string    `json:"label"`
	Ms      float64   `json:"ms"`
}

// logLimit is how many of the latest requests the engineers' page lists.
const logLimit = 500

// RequestLog returns the latest single requests, newest first.
func (c *Controller) RequestLog() []LogRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	calls := c.Log.All()
	if len(calls) > logLimit {
		calls = calls[len(calls)-logLimit:]
	}
	sc := c.applied().sc
	out := make([]LogRow, 0, len(calls))
	for i := len(calls) - 1; i >= 0; i-- {
		call := calls[i]
		row := LogRow{Seq: call.Seq, Time: call.Time, T: c.callTitle(call), Tech: techName(call), Ms: call.DurationMs, Who: "someone else, at " + call.Client}
		if c.tools.known(call.Client, call.Agent) {
			row.Who = "the tool at " + call.Client
		}
		if call.Cluster != "" {
			row.Cluster = clusterLabel(sc, call.Cluster)
		}
		switch call.Reason {
		case activity.ReasonAnswered, activity.ReasonEmpty, activity.ReasonRefused:
			row.St, row.Tone, row.Label = "ok", "ok", "Answered"
		case activity.ReasonFault, activity.ReasonPaused:
			row.St, row.Tone, row.Label = "fault", "warn", "Failing on purpose"
		case activity.ReasonDenied:
			row.St, row.Tone, row.Label = "denied", "warn", "Sign-in refused"
		default:
			row.St, row.Tone, row.Label = "unknown", "bad", "Not answered"
		}
		out = append(out, row)
	}
	return out
}

// RequestSent is the exact text of one request, as far as it is kept.
type RequestSent struct {
	Line string `json:"line"`
	Body string `json:"body,omitempty"`
	// Note says why there is no body, when there is none.
	Note string `json:"note,omitempty"`
}

// RequestBody returns what a tool sent in one request.
func (c *Controller) RequestBody(seq uint64) (*RequestSent, error) {
	call, ok := c.Log.Get(seq)
	if !ok {
		return nil, invalid("That request is no longer in the list. Only the latest requests are kept.")
	}
	out := &RequestSent{Line: call.Method, Body: call.Body}
	if i := strings.IndexByte(call.Method, ' '); i < 0 || !strings.HasPrefix(call.Method[i+1:], "/") {
		out.Line = "POST /" + map[string]string{activity.SurfaceVsan: "vsanHealth"}[call.Surface]
		if call.Surface != activity.SurfaceVsan {
			out.Line = "POST /sdk"
		}
	}
	if call.Body == "" {
		switch {
		case call.Method == "Login" || strings.Contains(strings.ToLower(call.Method), "session") || strings.Contains(strings.ToLower(call.Method), "login"):
			out.Note = "This is a sign-in, so nothing more than its name is kept."
		case strings.HasPrefix(call.Method, "GET ") || strings.HasPrefix(call.Method, "DELETE "):
			out.Note = "A request like this carries no text of its own: the address is all of it."
		default:
			out.Note = "The text of this request was not kept."
		}
	}
	return out, nil
}

// SupportRow is one line of "What the simulator can answer".
type SupportRow struct {
	Part  string `json:"part"`
	State string `json:"state"` // full | part | no
	Notes string `json:"notes"`
}

// Supported is the compatibility list in plain words, and the version the
// simulated vCenter reports.
func (c *Controller) Supported() (rows []SupportRow, vcenter string) {
	kinds := len(c.cat.Families)
	return []SupportRow{
		{"Signing in, staying signed in, signing out", "full", "Checked against a real vCenter."},
		{"Lists of datacenters, clusters, hosts, VMs, datastores and networks", "full", "Checked against a real vCenter."},
		{fmt.Sprintf("vSAN performance data, all %d kinds", kinds), "full", fmt.Sprintf("Values are made up but stay inside the range a real vCenter showed. A tool usually asks for only some of the %d.", kinds)},
		{"vSAN settings and disk lists", "full", "Checked against a real vCenter."},
		{"iSCSI and NFS datastores that hosts connect to (the ones on the Storage screen)", "full", "Tools see them, with their size and free space. Their activity counters read zero."},
		{"vSAN health summary", "part", "The main results are there. Some detailed checks are missing."},
		{"The newer web interface (REST) for lists", "part", "Reading lists works. Some details of VMs are missing."},
		{"The files that describe how to talk to vCenter", "part", "One of the two files is served."},
		{"Distributed switches", "part", "Present, but reported as the basic type rather than the VMware type."},
		{"The list of measurements vCenter can report (performance counters)", "no", "Tools that ask for the list get no answer. vSAN performance data itself, above, is answered."},
		{"Kubernetes (Supervisor) information", "no", "Tools that ask the older web interface whether Kubernetes is turned on get no answer."},
		{"Licensing", "no", "Tools that ask which license vCenter has get no answer. Most carry on. " + licenseNote},
		{"Fibre Channel storage, and vSAN itself acting as a file server or an iSCSI server", "no", "Tools are told there are none. This does not affect the iSCSI and NFS datastores above."},
	}, scenario.VersionLabel(c.applied().topo.VCenterVersion)
}

// ensure the capture type is the one main hands over
var _ = (*querylog.Capture)(nil)
