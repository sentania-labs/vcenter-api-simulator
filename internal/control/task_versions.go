package control

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// Versions: which release the simulated vCenter and its hosts say they
// are. A host is "pinned" when something gives it a version of its own (a
// per-host version, its cluster's version, or the scenario's default ESX
// version); every other host follows vCenter.

func init() {
	tasks["vcenter-version"] = taskDef{form: formVCenterVersion, plan: planVCenterVersion}
	tasks["cluster-versions"] = taskDef{form: formClusterVersions, plan: planClusterVersions}
}

// versionID is how the console names a version in a form: the preset's id,
// or the number itself.
func versionID(v scenario.Version) string {
	if v.Preset != "" {
		return v.Preset
	}
	return v.Version
}

// versionChoices are the versions a list offers: the presets, and any other
// version vCenter or a host runs now. product is "vCenter" or "ESX".
func (c *Controller) versionChoices(product string) [][2]string {
	r := c.applied()
	vcenter, _ := scenario.VersionPresets()
	seen := map[string]bool{}
	var vs []scenario.Version
	add := func(v scenario.Version) {
		if v.Version != "" && !seen[v.Version] {
			seen[v.Version] = true
			vs = append(vs, v)
		}
	}
	for _, v := range vcenter {
		add(v)
	}
	add(r.topo.VCenterVersion)
	for _, cl := range r.topo.Clusters() {
		for _, h := range cl.Hosts {
			add(h.Version)
		}
	}
	sort.SliceStable(vs, func(i, j int) bool {
		newer, _ := scenario.VersionNewer(vs[j].Version, vs[i].Version)
		return newer
	})
	var out [][2]string
	for _, v := range vs {
		out = append(out, [2]string{versionID(v), product + " " + scenario.VersionLabel(v)})
	}
	return out
}

// pinned reports whether a host has a version of its own in sc.
func pinned(sc *scenario.Scenario, cluster, host string) bool {
	if _, ok := sc.HostVersions[host]; ok {
		return true
	}
	for _, o := range sc.Clusters {
		if o.Key == cluster && o.ESX != (scenario.Version{}) {
			return true
		}
	}
	return sc.ESX != (scenario.Version{})
}

// hostKeys lists every host key of a scenario with its cluster key.
func hostKeys(sc *scenario.Scenario) [][2]string {
	var out [][2]string
	for _, key := range sc.ClusterKeys() {
		gone := sc.RemovedHosts(key)
		for n := 1; n <= sc.HostNumbers(key); n++ {
			if !gone[n] {
				out = append(out, [2]string{key, key + "/" + hostName(n)})
			}
		}
	}
	return out
}

// newerHosts counts the hosts of sc that are newer than its vCenter.
func newerHosts(sc *scenario.Scenario) int {
	vc, err := sc.VCenterVersion()
	if err != nil {
		return 0
	}
	n := 0
	for _, h := range hostKeys(sc) {
		if hv, err := sc.HostVersion(h[0], h[1]); err == nil {
			if newer, _ := scenario.VersionNewer(hv.Version, vc.Version); newer {
				n++
			}
		}
	}
	return n
}

// esxSpec is the scenario value that gives a host the release v.
func esxSpec(v scenario.Version) scenario.Version {
	spec, err := scenario.VersionChoice("esx", versionID(v))
	if err != nil {
		return v
	}
	return spec
}

// ---------------------------------------------------------------------------
// change the vCenter version

type vcParams struct {
	VC    string `json:"vc"`
	Other string `json:"other"`
	Hosts string `json:"hosts"`
}

func formVCenterVersion(c *Controller, _ map[string]string, _ Caller) (*Form, error) {
	cur := c.applied().topo.VCenterVersion
	return &Form{Title: "Change the vCenter version", Crumbs: [][2]string{{"Versions", "/versions"}}, Mix: true,
		Intro:   fmt.Sprintf("vCenter says it is %s now.", scenario.VersionLabel(cur)),
		Values:  map[string]any{"vc": versionID(cur), "other": "", "hosts": "all"},
		Options: map[string]any{"versions": c.versionChoices("vCenter"), "current": versionID(cur)}}, nil
}

func planVCenterVersion(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in vcParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	old := c.applied()
	sc := old.sc
	cur := old.topo.VCenterVersion
	curLabel := scenario.VersionLabel(cur)
	p := &Plan{Go: "Change vCenter version", dest: "/versions", Extra: map[string]any{}}
	choice := in.VC
	if in.VC == "other" {
		choice = strings.TrimSpace(in.Other)
	}
	spec, err := scenario.VersionChoice("vcenter", choice)
	var target scenario.Version
	if err == nil {
		probe := clone(sc)
		probe.VCenter = spec
		target, err = probe.VCenterVersion()
	}
	known := err == nil
	all := hostKeys(sc)
	match, others := 0, 0
	for _, h := range all {
		if pinned(sc, h[0], h[1]) {
			others++
		} else {
			match++
		}
	}
	// what each choice for the hosts would leave the scenario as
	build := func(mode string) *scenario.Scenario {
		next := clone(sc)
		next.VCenter = spec
		switch mode {
		case "all":
			next.ESX, next.HostVersions = scenario.Version{}, nil
			for i := range next.Clusters {
				next.Clusters[i].ESX = scenario.Version{}
			}
		case "none":
			if next.ESX == (scenario.Version{}) {
				next.ESX = esxSpec(cur)
				// a host that follows vCenter reports a version of its own;
				// pin exactly that, build and product name included
				for _, h := range all {
					if !pinned(sc, h[0], h[1]) {
						if hv, err := sc.HostVersion(h[0], h[1]); err == nil {
							if hv.Preset != "" {
								next.ESX = scenario.Version{Preset: hv.Preset}
							} else {
								next.ESX = hv
							}
						}
						break
					}
				}
			}
		}
		next.ForceNewerHosts = false
		next.ForceNewerHosts = newerHosts(next) > 0
		return next
	}
	ahead := func(mode string) int {
		if !known {
			return 0
		}
		return newerHosts(build(mode))
	}
	warn := func(mode string) string {
		if n := ahead(mode); n > 0 {
			return fmt.Sprintf(" **%s would then be newer than vCenter.** A real vCenter cannot manage that, so choose this only to test how a tool copes.", plural(n, "host"))
		}
		return ""
	}
	tgt := "the same version"
	if known {
		tgt = "ESX " + scenario.VersionLabel(target)
	}
	opts := [][3]string{{"all", fmt.Sprintf("Put all %s on %s as well", plural(len(all), "host"), tgt), "The usual choice: hosts and vCenter on the same version."}}
	if others > 0 && match > 0 {
		opts = append(opts, [3]string{"match", fmt.Sprintf("Move only the %s that match vCenter now", plural(match, "host")), fmt.Sprintf("The other %d keep the version they were given.%s", others, warn("match"))})
	}
	opts = append(opts, [3]string{"none", "Leave every host as it is", "Only vCenter changes." + warn("none")})
	p.Extra["hostOpts"] = opts
	mode := in.Hosts
	if mode != "match" && mode != "none" {
		mode = "all"
	}
	if mode == "match" && !(others > 0 && match > 0) {
		mode = "all"
	}
	p.Extra["hosts"] = mode
	if in.VC == "other" && !known {
		p.bad("other", "Type a version number like 9.2.0: numbers separated by dots.")
		p.Idle = "Type the version number, for example 9.2.0, and this box will say what happens."
		return p, nil
	}
	if !known {
		p.bad("vc", "Choose a version from the list.")
		return p, nil
	}
	if target.Version == cur.Version {
		p.bad("vc", "vCenter is already %s. Choose a different version, or press Cancel.", curLabel)
		p.Idle = "Nothing is different yet. Choose another version and this box will say what happens."
		p.Extra["same"] = true
		return p, nil
	}
	next := build(mode)
	label := scenario.VersionLabel(target)
	// the hosts whose version really changes
	moved := 0
	for _, h := range all {
		a, _ := sc.HostVersion(h[0], h[1])
		b, _ := next.HostVersion(h[0], h[1])
		if a.Version != b.Version {
			moved++
		}
	}
	p.Sum = append(p.Sum, say("vCenter says it is %s instead of %s.", bold(label), curLabel))
	switch mode {
	case "all":
		p.Sum = append(p.Sum, say("All %s move to ESX %s.", bold(plural(len(all), "host")), label))
	case "match":
		p.Sum = append(p.Sum, say("%s move to ESX %s. The other %d keep their versions.", bold(plural(match, "host")), label, others))
	default:
		p.Sum = append(p.Sum, say("No host changes. They stay on the versions they have now."))
	}
	if n := newerHosts(next); n > 0 {
		p.Sum = append(p.Sum, say("%s, which cannot happen on a real system.", bold(plural(n, "host will", "hosts will")+" be newer than vCenter")))
	}
	p.Sum = append(p.Sum, say("Tools that are signed in are signed out and sign in again by themselves."),
		note("Clusters, hosts and VMs keep their names, so your tool sees the same things with a new version."))
	p.text = fmt.Sprintf("Change vCenter from %s to %s", curLabel, label)
	if moved > 0 {
		p.text += fmt.Sprintf(" and move %s with it", plural(moved, "host"))
	}
	p.apply = func() (*Outcome, error) {
		if _, err := c.applyScenario(next, false); err != nil {
			return nil, err
		}
		rest := "No host was changed."
		if moved > 0 {
			rest = fmt.Sprintf("%s moved to ESX %s with it.", plural(moved, "host"), label)
		}
		return &Outcome{Done: fmt.Sprintf("vCenter now says it is %s. %s", label, rest)}, nil
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// change ESX versions in one cluster

type clusterVersionParams struct {
	Cluster string   `json:"cluster"`
	Mode    string   `json:"mode"`
	Ver     string   `json:"ver"`
	Low     string   `json:"low"`
	Pick    []string `json:"pick"`
	Allow   bool     `json:"allow"`
}

// versionCounts tallies a cluster's hosts by release, most common first.
func versionCounts(cl *topology.Cluster) (order []scenario.Version, n map[string]int) {
	n = map[string]int{}
	for _, h := range cl.Hosts {
		if n[h.Version.Version] == 0 {
			order = append(order, h.Version)
		}
		n[h.Version.Version]++
	}
	sort.SliceStable(order, func(i, j int) bool {
		if n[order[i].Version] != n[order[j].Version] {
			return n[order[i].Version] > n[order[j].Version]
		}
		newer, _ := scenario.VersionNewer(order[i].Version, order[j].Version)
		return newer
	})
	return order, n
}

// versionLabel says what a cluster's hosts run: "ESX 9.1.1", or
// "4 hosts on ESX 9.0, 2 on ESX 8.0 Update 3" for a mix.
func versionLabel(cl *topology.Cluster) (label string, mixed bool) {
	order, n := versionCounts(cl)
	if len(order) == 0 {
		return "", false
	}
	if len(order) == 1 {
		return "ESX " + scenario.VersionLabel(order[0]), false
	}
	sort.SliceStable(order, func(i, j int) bool {
		newer, _ := scenario.VersionNewer(order[i].Version, order[j].Version)
		return newer
	})
	var parts []string
	for i, v := range order {
		who := ""
		if i == 0 {
			who = "hosts "
			if n[v.Version] == 1 {
				who = "host "
			}
		}
		parts = append(parts, fmt.Sprintf("%d %son ESX %s", n[v.Version], who, scenario.VersionLabel(v)))
	}
	return strings.Join(parts, ", "), true
}

func formClusterVersions(c *Controller, q map[string]string, _ Caller) (*Form, error) {
	cl := c.findCluster(q["cluster"])
	if cl == nil {
		return nil, noCluster(q["cluster"])
	}
	r := c.applied()
	order, _ := versionCounts(cl)
	main := order[0]
	choices := c.versionChoices("ESX")
	low := choices[0][0]
	for i, ch := range choices {
		if ch[0] == versionID(main) && i > 0 {
			low = choices[i-1][0]
		}
	}
	mode := "same"
	pick := []string{shortHost(cl.Hosts[0].Name)}
	if len(order) > 1 {
		mode, low, pick = "mixed", versionID(order[1]), nil
		for _, h := range cl.Hosts {
			if h.Version.Version != main.Version {
				pick = append(pick, shortHost(h.Name))
			}
		}
	}
	label, mixed := versionLabel(cl)
	now := "all on " + label
	if mixed {
		now = "a mix: " + label
	}
	return &Form{Title: "Change ESX versions in " + cl.Name, Crumbs: [][2]string{{"Versions", "/versions"}}, Mix: true,
		Intro:   fmt.Sprintf("%s has %s, %s. vCenter is %s.", cl.Name, plural(len(cl.Hosts), "host"), now, scenario.VersionLabel(r.topo.VCenterVersion)),
		Values:  map[string]any{"cluster": cl.Key, "mode": mode, "ver": versionID(main), "low": low, "pick": pick, "allow": false},
		Options: map[string]any{"versions": choices, "hosts": hostNames(cl), "vcenter": scenario.VersionLabel(r.topo.VCenterVersion)}}, nil
}

func planClusterVersions(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in clusterVersionParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	cl := c.findCluster(in.Cluster)
	if cl == nil {
		return nil, noCluster(in.Cluster)
	}
	old := c.applied()
	sc := old.sc
	vc := old.topo.VCenterVersion
	p := &Plan{Go: "Change versions", dest: "/versions", Extra: map[string]any{}}
	resolve := func(id string) (spec, full scenario.Version, ok bool) {
		spec, err := scenario.VersionChoice("esx", id)
		if err != nil {
			return spec, full, false
		}
		full, err = scenario.ResolveESX(spec)
		return spec, full, err == nil
	}
	verSpec, ver, ok := resolve(in.Ver)
	if !ok {
		p.bad("ver", "Choose a version from the list.")
		return p, nil
	}
	mixed := in.Mode == "mixed"
	var lowSpec, low scenario.Version
	if mixed {
		if lowSpec, low, ok = resolve(in.Low); !ok {
			p.bad("low", "Choose a version from the list.")
			return p, nil
		}
	}
	names := hostNames(cl)
	var pick []string
	for _, h := range names {
		if slices.Contains(in.Pick, h) {
			pick = append(pick, h)
		}
	}
	tooNew := ""
	for _, v := range []scenario.Version{ver, low} {
		if v.Version == "" {
			continue
		}
		if newer, _ := scenario.VersionNewer(v.Version, vc.Version); newer && tooNew == "" {
			tooNew = scenario.VersionLabel(v)
		}
	}
	p.Extra["tooNew"] = tooNew
	if mixed {
		if ver.Version == low.Version {
			p.bad("low", `Both boxes say the same version. Choose two different versions, or choose "Every host on the same version" above.`)
		}
		switch {
		case len(pick) == 0:
			p.bad("pick", "Tick at least one host.")
		case len(pick) >= len(names):
			p.bad("pick", `You ticked every host. Leave at least one on the other version, or choose "Every host on the same version" above.`)
		}
	}
	if tooNew != "" && !in.Allow {
		p.bad("allow", "Pick an older version above, or tick this box to go ahead anyway.")
	}
	// what each host would run
	want := func(h string) scenario.Version {
		if mixed && slices.Contains(pick, h) {
			return low
		}
		return ver
	}
	same := true
	for _, h := range cl.Hosts {
		if h.Version.Version != want(shortHost(h.Name)).Version {
			same = false
		}
	}
	if same {
		p.Idle = "Nothing is different yet. This is what the cluster runs now. Change something and this box will say what happens."
		if len(p.Errors) == 0 {
			p.bad("ver", "Nothing has been changed. Choose a different version, or press Cancel.")
		}
		return p, nil
	}
	var tn *Item
	if tooNew != "" {
		it := say(`%s, which cannot happen on a real system. This will only go ahead if you tick "Do it anyway".`, bold(fmt.Sprintf("ESX %s is newer than vCenter %s", tooNew, scenario.VersionLabel(vc))))
		tn = &it
	}
	verLabel, lowLabel := scenario.VersionLabel(ver), scenario.VersionLabel(low)
	if !mixed {
		p.Sum = append(p.Sum, say("All %s in %s run %s.", plural(len(names), "host"), cl.Name, bold("ESX "+verLabel)))
		if tn != nil {
			p.Sum = append(p.Sum, *tn)
		}
		p.Sum = append(p.Sum, note("Hosts keep their names, so your tool sees the same hosts with a new version."))
	} else {
		if len(pick) == 0 {
			p.Sum = append(p.Sum, say("Tick the hosts that run the other version."))
		} else {
			verb := "run"
			if len(pick) == 1 {
				verb = "runs"
			}
			p.Sum = append(p.Sum, say("%s %s %s.", bold(listAnd(pick)), verb, bold("ESX "+lowLabel)))
		}
		p.Sum = append(p.Sum, say("The other %s run %s.", plural(len(names)-len(pick), "host"), bold("ESX "+verLabel)))
		if tn != nil {
			p.Sum = append(p.Sum, *tn)
		}
		p.Sum = append(p.Sum, note("Hosts keep their names, so your tool sees the same hosts with new versions."))
	}
	p.Sum = append(p.Sum, note("Other clusters are not touched."))
	if len(p.Errors) > 0 {
		return p, nil
	}
	next := clone(sc)
	o := override(next, cl.Key)
	o.ESX = verSpec
	if ver.Version == vc.Version && next.ESX == (scenario.Version{}) {
		o.ESX = scenario.Version{} // the same as vCenter: follow it, as an untouched cluster does
	}
	for key := range next.HostVersions {
		if strings.HasPrefix(key, cl.Key+"/") {
			delete(next.HostVersions, key)
		}
	}
	if mixed {
		if next.HostVersions == nil {
			next.HostVersions = map[string]scenario.Version{}
		}
		for _, h := range pick {
			next.HostVersions[cl.Key+"/"+h] = lowSpec
		}
	}
	if len(next.HostVersions) == 0 {
		next.HostVersions = nil
	}
	next.ForceNewerHosts = false
	next.ForceNewerHosts = newerHosts(next) > 0
	name := clusterLabel(sc, cl.Key)
	p.text = "Change ESX versions in " + name
	p.apply = func() (*Outcome, error) {
		if _, err := c.applyScenario(next, false); err != nil {
			return nil, err
		}
		done := fmt.Sprintf("Every host in %s now runs ESX %s.", name, verLabel)
		if mixed {
			done = fmt.Sprintf("%s is now mixed: %s on ESX %s, the rest on ESX %s.", name, listAnd(pick), lowLabel, verLabel)
		}
		return &Outcome{Done: done, Marks: []Mark{{Type: "cluster", ID: cl.Key, Tag: "Changed"}}}, nil
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// the screen

// VersionRow is one cluster on the Versions screen.
type VersionRow struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	DC    string `json:"dc"`
	Hosts int    `json:"hosts"`
	Label string `json:"label"`
	Mixed bool   `json:"mixed"`
}

// VersionCount is how many hosts run one release.
type VersionCount struct {
	Label string `json:"label"`
	Hosts int    `json:"hosts"`
}

// VersionsView is the Versions screen.
type VersionsView struct {
	VCenter string         `json:"vcenter"`
	Hosts   int            `json:"hosts"`
	ByESX   []VersionCount `json:"byEsx"`
	Rows    []VersionRow   `json:"rows"`
	Multi   bool           `json:"multi"`
}

// Versions returns what vCenter and every cluster's hosts run.
func (c *Controller) Versions() VersionsView {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.applied()
	v := VersionsView{VCenter: scenario.VersionLabel(r.topo.VCenterVersion), Rows: []VersionRow{}, Multi: len(r.topo.Datacenters) > 1}
	count := map[string]int{}
	byNumber := map[string]scenario.Version{}
	for _, cl := range r.topo.Clusters() {
		label, mixed := versionLabel(cl)
		v.Rows = append(v.Rows, VersionRow{Key: cl.Key, Name: cl.Name, DC: cl.Datacenter.Name, Hosts: len(cl.Hosts), Label: label, Mixed: mixed})
		v.Hosts += len(cl.Hosts)
		for _, h := range cl.Hosts {
			count[h.Version.Version]++
			byNumber[h.Version.Version] = h.Version
		}
	}
	var numbers []string
	for n := range count {
		numbers = append(numbers, n)
	}
	sort.Slice(numbers, func(i, j int) bool {
		newer, _ := scenario.VersionNewer(numbers[i], numbers[j])
		return newer
	})
	for _, n := range numbers {
		v.ByESX = append(v.ByESX, VersionCount{Label: scenario.VersionLabel(byNumber[n]), Hosts: count[n]})
	}
	return v
}
