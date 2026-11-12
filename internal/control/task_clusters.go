package control

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// Limits of the cluster forms. The simulator itself would take more; these
// keep a typing slip from building something enormous.
const (
	maxHosts    = 64
	maxVMs      = 200
	maxClusters = 50
)

var keepNote = note("Clusters, hosts and VMs that already exist are not touched.")

func init() {
	tasks["add-cluster"] = taskDef{form: formAddCluster, plan: planAddCluster}
	tasks["replace-everything"] = taskDef{form: formReplace, plan: planReplace}
	tasks["add-hosts"] = taskDef{form: formAddHosts, plan: planAddHosts}
	tasks["remove-hosts"] = taskDef{form: formRemoveHosts, plan: planRemoveHosts}
	tasks["hardware"] = taskDef{form: formHardware, plan: planHardware}
}

// findCluster returns a served cluster by key.
func (c *Controller) findCluster(key string) *topology.Cluster {
	for _, cl := range c.applied().topo.Clusters() {
		if cl.Key == key {
			return cl
		}
	}
	return nil
}

func noCluster(key string) error {
	return invalid("There is no cluster called %s. It may have been removed.", shortCluster(key))
}

// override returns the scenario's explicit settings for a cluster, adding
// an empty one when there is none.
func override(sc *scenario.Scenario, key string) *scenario.ClusterOverride {
	for i := range sc.Clusters {
		if sc.Clusters[i].Key == key {
			return &sc.Clusters[i]
		}
	}
	sc.Clusters = append(sc.Clusters, scenario.ClusterOverride{Key: key})
	return &sc.Clusters[len(sc.Clusters)-1]
}

// projectOf is what a scenario would cost a tool to collect, generated
// against a copy of the registry so that nothing is minted for real.
func (c *Controller) projectOf(sc *scenario.Scenario, fresh bool) *Projection {
	reg := c.reg.Clone()
	if fresh || sc.Seed != c.applied().sc.Seed {
		reg = registry.InMemory(sc.Seed)
	}
	return Project(c.cat, topology.Generate(sc, reg))
}

// loadLine says how the size of one full collection changes.
func loadLine(a, b *Projection) Item {
	if a.All.Calls == b.All.Calls {
		if a.All.Bytes == b.All.Bytes {
			return note("One full collection by a tool stays at about %s requests (%s).", num(a.All.Calls), megabytes(a.All.Bytes))
		}
		dir := "grows"
		if b.All.Bytes < a.All.Bytes {
			dir = "shrinks"
		}
		return note("One full collection by a tool stays at about %s requests, and the data it gets back %s from %s to %s.", num(a.All.Calls), dir, megabytes(a.All.Bytes), megabytes(b.All.Bytes))
	}
	return note("One full collection by a tool goes from about %s requests (%s) to about %s requests (%s).", num(a.All.Calls), megabytes(a.All.Bytes), num(b.All.Calls), megabytes(b.All.Bytes))
}

func vsanName(architecture string) string {
	if architecture == "osa" {
		return "vSAN OSA"
	}
	return "vSAN ESA"
}

func datacenterNames(sc *scenario.Scenario) []string {
	var out []string
	for _, dc := range sc.Datacenters {
		out = append(out, dc.Name)
	}
	return out
}

func nextDatacenter(sc *scenario.Scenario) string {
	have := map[string]bool{}
	for _, dc := range sc.Datacenters {
		have[dc.Name] = true
	}
	for n := len(sc.Datacenters) + 1; ; n++ {
		if name := fmt.Sprintf("dc-%02d", n); !have[name] {
			return name
		}
	}
}

// ---------------------------------------------------------------------------
// add a cluster

type addClusterParams struct {
	DC    string `json:"dc"`
	Hosts N      `json:"hosts"`
	VMs   N      `json:"vms"`
	VSAN  string `json:"vsan"`
	Count N      `json:"count"`
}

func formAddCluster(c *Controller, q map[string]string, _ Caller) (*Form, error) {
	sc := c.applied().sc
	dcs := datacenterNames(sc)
	f := &Form{Title: "Add a cluster", Crumbs: [][2]string{{"Clusters", "/clusters"}}, Mix: true,
		Intro:   "Every box already holds a sensible value. Change what you want, then press Add cluster.",
		Values:  map[string]any{"dc": dcs[0], "hosts": 4, "vms": 8, "vsan": "ESA", "count": 1},
		Options: map[string]any{"dcs": dcs, "newDc": nextDatacenter(sc), "minHosts": scenario.MinHosts, "maxHosts": maxHosts}}
	return f, nil
}

func planAddCluster(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in addClusterParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	old := c.applied()
	sc := old.sc
	p := &Plan{Go: "Add cluster", dest: "/clusters"}
	if in.Count.OK && in.Count.V > 1 {
		p.Go = fmt.Sprintf("Add %d clusters", in.Count.V)
	}
	isNew := in.DC == "new"
	dcName := in.DC
	if isNew {
		dcName = nextDatacenter(sc)
	}
	dcIndex := -1
	for i, dc := range sc.Datacenters {
		if dc.Name == dcName {
			dcIndex = i
		}
	}
	if !isNew && dcIndex < 0 {
		p.bad("dc", "Choose a datacenter from the list.")
	}
	if !in.Hosts.in(scenario.MinHosts, maxHosts) {
		p.bad("hosts", "Enter a number of hosts from %d to %d.", scenario.MinHosts, maxHosts)
	}
	if !in.VMs.in(0, maxVMs) {
		p.bad("vms", "Enter a number of VMs from 0 to %d.", maxVMs)
	}
	if !in.Count.in(1, maxClusters) {
		p.bad("count", "Enter a number of clusters from 1 to %d.", maxClusters)
	}
	if in.VSAN != "ESA" && in.VSAN != "OSA" {
		in.VSAN = "ESA"
	}
	if len(p.Errors) > 0 {
		return p, nil
	}
	h, v, count := in.Hosts.V, in.VMs.V, in.Count.V
	next := clone(sc)
	if isNew {
		next.Datacenters = append(next.Datacenters, scenario.Datacenter{Name: dcName})
		dcIndex = len(next.Datacenters) - 1
	}
	first := next.Datacenters[dcIndex].Clusters + 1
	var keys, names []string
	architecture := "esa"
	if in.VSAN == "OSA" {
		architecture = "osa"
	}
	for i := 0; i < count; i++ {
		key := scenario.ClusterKey(dcName, first+i)
		keys = append(keys, key)
		names = append(names, shortCluster(key))
		o := override(next, key)
		o.Hosts, o.VMs.PerHost, o.Architecture = h, scenario.IntPtr(v), architecture
	}
	next.Datacenters[dcIndex].Clusters += count

	what := names[0]
	if count > 1 {
		what = fmt.Sprintf("%d clusters, %s to %s", count, names[0], names[count-1])
	}
	where := dcName
	if isNew {
		where = "a new datacenter, " + dcName
	}
	it := "It has"
	if count > 1 {
		it = "Each has"
	}
	local := "no local disk"
	switch n := scenario.Int(next.Defaults.Host.LocalDevices); {
	case n == 1:
		local = "one local disk per host"
	case n > 1:
		local = fmt.Sprintf("%d local disks per host", n)
	}
	p.Sum = append(p.Sum,
		say("Adds %s to %s. The simulator chooses the name.", bold(what), where),
		say("%s %s and %s, its own %s datastore and %s.", it, bold(plural(h, "host")), bold(plural(h*v, "VM")), vsanName(architecture), local))
	vc, _ := next.VCenterVersion()
	if hv, err := next.HostVersion(keys[0], keys[0]+"/esx01"); err == nil {
		same := ""
		if hv.Version == vc.Version {
			same = ", the same as vCenter"
		}
		p.Sum = append(p.Sum, say("Its hosts run ESX %s%s.", scenario.VersionLabel(hv), same))
	}
	if !isNew {
		var shared []string
		for _, n := range sc.Datacenters[dcIndex].NFS {
			if len(n.Clusters) == 0 {
				shared = append(shared, n.Name)
			}
		}
		if len(shared) > 0 {
			p.Sum = append(p.Sum, say("Its hosts also connect to %s, which every cluster in %s shares.", listAnd(shared), dcName))
		}
	}
	p.Sum = append(p.Sum, keepNote, loadLine(Project(c.cat, old.topo), c.projectOf(next, false)))

	doneWhat := names[0]
	if count > 1 {
		doneWhat = fmt.Sprintf("%d clusters (%s to %s)", count, names[0], names[count-1])
	}
	each := ""
	live := "It is"
	if count > 1 {
		each, live = " each", "They are"
	}
	p.text = fmt.Sprintf("Add %s with %s%s", doneWhat, plural(h, "host"), each)
	p.apply = func() (*Outcome, error) {
		if _, err := c.applyScenario(next, false); err != nil {
			return nil, err
		}
		done := fmt.Sprintf("%s added to %s with %s and %s%s.", doneWhat, dcName, plural(h, "host"), plural(h*v, "VM"), each)
		out := &Outcome{Done: done + " " + live + " live now.", history: done}
		for _, k := range keys {
			out.Marks = append(out.Marks, Mark{Type: "cluster", ID: k, Tag: "New"})
		}
		return out, nil
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// replace everything with a new environment

// sizes are the ready environment sizes: datacenters, clusters in each,
// hosts from and to, VMs per host from and to.
var sizes = map[string][6]int{
	"tiny":    {1, 1, 4, 4, 3, 3},
	"ten":     {1, 10, 4, 16, 2, 8},
	"fifty":   {2, 25, 8, 8, 10, 10},
	"hundred": {2, 50, 4, 16, 2, 8},
}

type replaceParams struct {
	Start string `json:"start"`
	DCs   N      `json:"dcs"`
	Per   N      `json:"per"`
	HMin  N      `json:"hmin"`
	HMax  N      `json:"hmax"`
	VMin  N      `json:"vmin"`
	VMax  N      `json:"vmax"`
	Mix   string `json:"mix"`
	Key   string `json:"key"`
}

var layoutNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,39}$`)

func formReplace(c *Controller, _ map[string]string, _ Caller) (*Form, error) {
	seed := c.applied().sc.Seed
	name := "lab-a"
	for i := 0; name == seed; i++ {
		name = "lab-" + string(rune('b'+i))
	}
	return &Form{Title: "Replace everything with a new environment", Crumbs: [][2]string{{"Clusters", "/clusters"}}, Danger: true, Mix: true,
		Intro:   `Builds a whole simulated vCenter in one go. Use it to start a test at a particular size. To grow what you have instead, go back and use "Add a cluster".`,
		Values:  map[string]any{"start": "ten", "dcs": 1, "per": 10, "hmin": 4, "hmax": 16, "vmin": 2, "vmax": 8, "mix": "ESA", "key": name},
		Options: map[string]any{"minHosts": scenario.MinHosts, "maxHosts": maxHosts}}, nil
}

func planReplace(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in replaceParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	old := c.applied()
	sc := old.sc
	p := &Plan{Go: "Replace the environment", dest: "/clusters"}
	x, ready := sizes[in.Start]
	if !ready {
		x = [6]int{in.DCs.V, in.Per.V, in.HMin.V, in.HMax.V, in.VMin.V, in.VMax.V}
		if !in.DCs.in(1, 8) || !in.Per.in(1, 100) {
			p.bad("dcs", "Use 1 to 8 datacenters and 1 to 100 clusters in each.")
		}
		if !in.HMin.in(scenario.MinHosts, maxHosts) || !in.HMax.in(scenario.MinHosts, maxHosts) || in.HMin.V > in.HMax.V {
			p.bad("hmin", "Use %d to %d hosts, with the first number no bigger than the second.", scenario.MinHosts, maxHosts)
		}
		if !in.VMin.in(0, maxVMs) || !in.VMax.in(0, maxVMs) || in.VMin.V > in.VMax.V {
			p.bad("vmin", "Use 0 to %d VMs, with the first number no bigger than the second.", maxVMs)
		}
	}
	if in.Key != "" && !layoutNameRe.MatchString(in.Key) {
		p.bad("key", "Use only letters, numbers and hyphens in the layout name, 40 at most.")
	}
	if len(p.Errors) > 0 {
		return p, nil
	}
	dcs, per, hmin, hmax, vmin, vmax := x[0], x[1], x[2], x[3], x[4], x[5]
	next := clone(sc)
	next.Datacenters = nil
	for d := 1; d <= dcs; d++ {
		next.Datacenters = append(next.Datacenters, scenario.Datacenter{Name: fmt.Sprintf("dc-%02d", d), Clusters: per})
	}
	next.Clusters, next.Removed, next.HostVersions = nil, nil, nil
	next.Vary = scenario.Vary{}
	next.Defaults.Hosts = hmin
	if hmax > hmin {
		for h := hmin; h <= hmax; h++ {
			next.Vary.Hosts = append(next.Vary.Hosts, h)
		}
	}
	next.Defaults.VMs.PerHost = scenario.IntPtr(vmin)
	if vmax > vmin {
		next.Vary.VMsPerHost = &scenario.Range{Min: vmin, Max: vmax}
	}
	next.Defaults.Architecture = "esa"
	switch in.Mix {
	case "OSA":
		next.Defaults.Architecture = "osa"
	case "mix":
		i := 0
		for _, dc := range next.Datacenters {
			for n := 1; n <= dc.Clusters; n++ {
				if i++; i%6 == 0 {
					override(next, scenario.ClusterKey(dc.Name, n)).Architecture = "osa"
				}
			}
		}
	}
	sameName := in.Key == "" || in.Key == sc.Seed
	if in.Key != "" {
		next.Seed = in.Key
	}
	// slowness and faults stay; ones that name clusters keep only the
	// clusters that still exist
	exists := map[string]bool{}
	for _, k := range next.ClusterKeys() {
		exists[k] = true
	}
	dropped := 0
	var conds []scenario.Condition
	for _, cond := range next.Conditions {
		if len(cond.Clusters) > 0 {
			var keep []string
			for _, k := range cond.Clusters {
				if exists[k] {
					keep = append(keep, k)
				}
			}
			if len(keep) == 0 {
				dropped++
				continue
			}
			cond.Clusters = keep
		}
		conds = append(conds, cond)
	}
	next.Conditions = conds
	if err := next.Validate(); err != nil {
		p.bad("start", "%s", upper1(err.Error()))
		return p, nil
	}

	before, after := Project(c.cat, old.topo), c.projectOf(next, true)
	stores := 0
	for _, dc := range sc.Datacenters {
		stores += len(dc.NFS)
		for _, st := range dc.ISCSI {
			stores += len(st.LUNs)
		}
	}
	gone := fmt.Sprintf("%s, %s, %s", plural(before.Clusters, "cluster"), plural(before.Hosts, "host"), plural(before.VMs, "VM"))
	if stores > 0 {
		gone += " and " + plural(stores, "shared datastore")
	} else {
		gone = fmt.Sprintf("%s, %s and %s", plural(before.Clusters, "cluster"), plural(before.Hosts, "host"), plural(before.VMs, "VM"))
	}
	p.Sum = append(p.Sum,
		say("%s: %s.", bold("Removes everything there is now"), gone),
		say("Builds %s in %s with %s and %s.", bold(plural(after.Clusters, "cluster")), plural(dcs, "datacenter"), bold(plural(after.Hosts, "host")), bold(plural(after.VMs, "VM"))))
	if sameName {
		p.Sum = append(p.Sum, say("The layout name is the one in use now, so clusters that come out the same size look the same to your tool as before. The rest look as if they were deleted and new ones added. Use a different name for a completely fresh start."))
	} else {
		p.Sum = append(p.Sum, say("Your tool will see brand-new clusters, hosts and VMs. Anything it collected before will look as if it was deleted."))
	}
	if dropped > 0 {
		p.Sum = append(p.Sum, say("%s only on clusters that are being removed, so %s.", plural(dropped, "fault or slowness setting applies", "faults or slowness settings apply"), map[bool]string{true: "it is turned off", false: "they are turned off"}[dropped == 1]))
	}
	p.Sum = append(p.Sum,
		note("Users, the certificate, recorded requests and any other slowness or faults are kept."),
		note("One full collection by a tool will be about %s requests (%s). Now it is about %s requests (%s).", num(after.All.Calls), megabytes(after.All.Bytes), num(before.All.Calls), megabytes(before.All.Bytes)))
	p.Confirm = &Confirm{Title: "Replace the whole environment?", OK: "Yes, replace everything", Body: []string{
		fmt.Sprintf("%s and everything in them will be removed and %s built in their place.", plural(before.Clusters, "cluster"), plural(after.Clusters, "new cluster")),
		"This cannot be undone. Your tool will see everything it knew as deleted."}}
	p.text = fmt.Sprintf("Replace everything with a new environment of %s", plural(after.Clusters, "cluster"))
	p.apply = func() (*Outcome, error) {
		if _, err := c.applyScenario(next, true); err != nil {
			return nil, err
		}
		done := fmt.Sprintf("New environment built: %s, %s and %s.", plural(after.Clusters, "cluster"), plural(after.Hosts, "host"), plural(after.VMs, "VM"))
		return &Outcome{Done: done + " It is live now.", history: done}, nil
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// add hosts, remove hosts, change hardware

type clusterParams struct {
	Cluster string   `json:"cluster"`
	N       N        `json:"n"`
	Pick    []string `json:"pick"`
}

func clusterCrumbs(key string) [][2]string {
	return [][2]string{{"Clusters", "/clusters"}, {shortCluster(key), "/cluster/" + key}}
}

func formAddHosts(c *Controller, q map[string]string, _ Caller) (*Form, error) {
	cl := c.findCluster(q["cluster"])
	if cl == nil {
		return nil, noCluster(q["cluster"])
	}
	return &Form{Title: "Add hosts to " + cl.Name, Crumbs: clusterCrumbs(cl.Key), Mix: true,
		Intro:   fmt.Sprintf("%s has %s now.", cl.Name, plural(len(cl.Hosts), "host")),
		Values:  map[string]any{"cluster": cl.Key, "n": min(2, max(1, maxHosts-len(cl.Hosts)))},
		Options: map[string]any{"max": maxHosts - len(cl.Hosts)}}, nil
}

func hostName(n int) string { return fmt.Sprintf("esx%02d", n) }

func planAddHosts(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in clusterParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	cl := c.findCluster(in.Cluster)
	if cl == nil {
		return nil, noCluster(in.Cluster)
	}
	old := c.applied()
	p := &Plan{Go: "Add hosts", dest: "/cluster/" + cl.Key}
	cur, room := len(cl.Hosts), maxHosts-len(cl.Hosts)
	switch {
	case room <= 0:
		p.bad("n", "%s already has %d hosts, the most the console adds.", cl.Name, maxHosts)
	case !in.N.in(1, room):
		p.bad("n", "Enter a number from 1 to %d.", room)
	}
	if len(p.Errors) > 0 {
		return p, nil
	}
	n := in.N.V
	numbers := old.sc.HostNumbers(cl.Key)
	next := clone(old.sc)
	override(next, cl.Key).Hosts = numbers + n
	var names []string
	for i := 1; i <= n; i++ {
		names = append(names, hostName(numbers+i))
	}
	perHost := scenario.Int(cl.Spec.VMs.PerHost)
	called := fmt.Sprintf("The new host is called %s and gets %s.", names[0], plural(perHost, "VM"))
	if n > 1 {
		join := "to"
		if n == 2 {
			join = "and"
		}
		called = fmt.Sprintf("The new hosts are called %s %s %s and each get %s.", names[0], join, names[n-1], plural(perHost, "VM"))
	}
	p.Sum = []Item{
		say("%s goes from %d to %s.", cl.Name, cur, bold(plural(cur+n, "host"))),
		say("%s", called), keepNote, loadLine(Project(c.cat, old.topo), c.projectOf(next, false))}
	p.text = fmt.Sprintf("Add %s to %s", plural(n, "host"), clusterLabel(old.sc, cl.Key))
	p.apply = func() (*Outcome, error) {
		if _, err := c.applyScenario(next, false); err != nil {
			return nil, err
		}
		done := fmt.Sprintf("%s now has %s.", clusterLabel(old.sc, cl.Key), plural(cur+n, "host"))
		marked := "The new ones are marked New in the list below."
		if n == 1 {
			marked = "The new one is marked New in the list below."
		}
		out := &Outcome{Done: done + " " + marked, history: fmt.Sprintf("%s added to %s. It now has %s.", plural(n, "host"), clusterLabel(old.sc, cl.Key), plural(cur+n, "host"))}
		for _, name := range names {
			out.Marks = append(out.Marks, Mark{Type: "host", ID: cl.Key + "/" + name, Tag: "New"})
		}
		return out, nil
	}
	return p, nil
}

func shortHost(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			return name[:i]
		}
	}
	return name
}

func hostNames(cl *topology.Cluster) []string {
	out := make([]string, 0, len(cl.Hosts))
	for _, h := range cl.Hosts {
		out = append(out, shortHost(h.Name))
	}
	return out
}

func formRemoveHosts(c *Controller, q map[string]string, _ Caller) (*Form, error) {
	cl := c.findCluster(q["cluster"])
	if cl == nil {
		return nil, noCluster(q["cluster"])
	}
	return &Form{Title: "Remove hosts from " + cl.Name, Crumbs: clusterCrumbs(cl.Key), Mix: true, Danger: true,
		Intro:   fmt.Sprintf("%s has %s now.", cl.Name, plural(len(cl.Hosts), "host")),
		Values:  map[string]any{"cluster": cl.Key, "pick": []string{}},
		Options: map[string]any{"hosts": hostNames(cl), "minHosts": scenario.MinHosts}}, nil
}

func planRemoveHosts(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in clusterParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	cl := c.findCluster(in.Cluster)
	if cl == nil {
		return nil, noCluster(in.Cluster)
	}
	old := c.applied()
	p := &Plan{Go: "Remove hosts", dest: "/cluster/" + cl.Key}
	have := map[string]bool{}
	for _, name := range hostNames(cl) {
		have[name] = true
	}
	var pick []string
	var numbers []int
	seen := map[string]bool{}
	for _, name := range in.Pick {
		n, ok := scenario.StorageHostOrdinal(name)
		if !ok || !have[name] || seen[name] {
			continue
		}
		seen[name] = true
		pick = append(pick, name)
		numbers = append(numbers, n)
	}
	sort.Strings(pick)
	if len(pick) == 0 {
		p.bad("pick", "Tick at least one host to remove.")
		p.Idle = "Tick the hosts to remove and this box will say what happens."
		return p, nil
	}
	left := len(cl.Hosts) - len(pick)
	if left < scenario.MinHosts {
		p.bad("pick", "At least %d hosts have to stay, because vSAN needs three. To remove them all, remove the cluster instead.", scenario.MinHosts)
	}
	perHost := scenario.Int(cl.Spec.VMs.PerHost)
	them := "those hosts"
	if len(pick) == 1 {
		them = "that host"
	}
	vmsLine := say("Your tool will see %s as deleted.", them)
	if gone := len(pick) * perHost; gone > 0 {
		vmsLine = say("Your tool will see %s and %s as deleted: the cluster's highest-numbered ones. The VMs that are left are shared out over the hosts that stay.", them, plural(gone, "VM"))
	}
	p.Sum = []Item{
		say("Removes %s.", bold(listAnd(pick))),
		say("%s goes from %d to %s, and from %s to %s.", cl.Name, len(cl.Hosts), plural(left, "host"), num(len(cl.VMs)), plural(left*perHost, "VM")),
		vmsLine,
		note("The other clusters are not touched.")}
	if len(p.Errors) > 0 {
		return p, nil
	}
	next := clone(old.sc)
	o := override(next, cl.Key)
	o.RemovedHosts = append(o.RemovedHosts, numbers...)
	sort.Ints(o.RemovedHosts)
	if _, err := reconcileStorage(next, old.sc); err != nil {
		return nil, &Invalid{err.Error()}
	}
	p.text = fmt.Sprintf("Remove %s from %s", plural(len(pick), "host"), clusterLabel(old.sc, cl.Key))
	p.apply = func() (*Outcome, error) {
		if _, err := c.applyScenario(next, false); err != nil {
			return nil, err
		}
		return &Outcome{Done: fmt.Sprintf("%s removed. %s now has %s.", listAnd(pick), clusterLabel(old.sc, cl.Key), plural(left, "host")),
			history: fmt.Sprintf("%s removed from %s. It now has %s.", listAnd(pick), clusterLabel(old.sc, cl.Key), plural(left, "host"))}, nil
	}
	return p, nil
}

type hardwareParams struct {
	Cluster string `json:"cluster"`
	CPU     N      `json:"cpu"`
	NICs    N      `json:"nics"`
	Disks   N      `json:"disks"`
	Local   N      `json:"local"`
	VMs     N      `json:"vms"`
	VDisks  N      `json:"vdisks"`
	Dedup   bool   `json:"dedup"`
	Comp    bool   `json:"comp"`
}

func formHardware(c *Controller, q map[string]string, _ Caller) (*Form, error) {
	cl := c.findCluster(q["cluster"])
	if cl == nil {
		return nil, noCluster(q["cluster"])
	}
	s := cl.Spec
	return &Form{Title: "Change host hardware in " + cl.Name, Crumbs: clusterCrumbs(cl.Key), Mix: true,
		Intro: "These apply to every host in the cluster. They change how much there is for a tool to collect.",
		Values: map[string]any{"cluster": cl.Key, "cpu": s.Host.Threads, "nics": s.Host.VsanPnics, "disks": s.Host.ClaimedDevices,
			"local": scenario.Int(s.Host.LocalDevices), "vms": scenario.Int(s.VMs.PerHost), "vdisks": s.VMs.DisksPerVM, "dedup": cl.Dedup, "comp": cl.Compression},
		Options: map[string]any{"esa": s.Architecture != "osa"}}, nil
}

func planHardware(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in hardwareParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	cl := c.findCluster(in.Cluster)
	if cl == nil {
		return nil, noCluster(in.Cluster)
	}
	old := c.applied()
	p := &Plan{Go: "Change hardware", dest: "/cluster/" + cl.Key}
	s := cl.Spec
	esa := s.Architecture != "osa"
	type row struct {
		key, label string
		in         N
		cur        int
		lo, hi     int
	}
	rows := []row{
		{"cpu", "Processor threads per host", in.CPU, s.Host.Threads, 1, 512},
		{"nics", "vSAN network cards per host", in.NICs, s.Host.VsanPnics, 1, 8},
		{"disks", "vSAN disks per host", in.Disks, s.Host.ClaimedDevices, 1, 24},
		{"local", "Local disks per host", in.Local, scenario.Int(s.Host.LocalDevices), 0, 8},
		{"vms", "VMs per host", in.VMs, scenario.Int(s.VMs.PerHost), 0, maxVMs},
		{"vdisks", "Virtual disks per VM", in.VDisks, s.VMs.DisksPerVM, 1, 16},
	}
	for _, r := range rows {
		if !r.in.in(r.lo, r.hi) {
			p.bad(r.key, "Enter a number from %d to %d.", r.lo, r.hi)
			continue
		}
		if r.in.V != r.cur {
			p.Sum = append(p.Sum, say("%s: %d becomes %s.", r.label, r.cur, bold(num(r.in.V))))
		}
	}
	onOff := map[bool]string{true: "on", false: "off"}
	if esa && in.Dedup != cl.Dedup {
		p.Sum = append(p.Sum, say("Deduplication turns %s.", bold(onOff[in.Dedup])))
	}
	if esa && in.Comp != cl.Compression {
		p.Sum = append(p.Sum, say("Compression turns %s.", bold(onOff[in.Comp])))
	}
	if len(p.Errors) > 0 {
		return p, nil
	}
	if len(p.Sum) == 0 {
		p.Idle = "Nothing is different yet. Change a number and this box will say what happens."
		return p, nil
	}
	p.Sum = append(p.Sum, say("Applies to all %s in %s.", plural(len(cl.Hosts), "host"), cl.Name),
		note("Hosts keep their names, so your tool sees the same hosts with different hardware."))
	next := clone(old.sc)
	o := override(next, cl.Key)
	o.Host.Threads, o.Host.VsanPnics, o.Host.ClaimedDevices = in.CPU.V, in.NICs.V, in.Disks.V
	o.Host.LocalDevices, o.VMs.PerHost, o.VMs.DisksPerVM = scenario.IntPtr(in.Local.V), scenario.IntPtr(in.VMs.V), in.VDisks.V
	if esa {
		dedup, comp := in.Dedup, in.Comp
		o.Dedup, o.Compression = &dedup, &comp
	}
	p.text = "Change host hardware in " + clusterLabel(old.sc, cl.Key)
	p.apply = func() (*Outcome, error) {
		if _, err := c.applyScenario(next, false); err != nil {
			return nil, err
		}
		return &Outcome{Done: fmt.Sprintf("Hardware changed for all %s in %s.", plural(len(cl.Hosts), "host"), clusterLabel(old.sc, cl.Key)),
			Marks: []Mark{{Type: "cluster", ID: cl.Key, Tag: "Changed"}}}, nil
	}
	return p, nil
}

// RemoveCluster removes a cluster at once, with the storage only it uses,
// and returns the words that say so.
func (c *Controller) RemoveCluster(key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep()
	cl := c.findCluster(key)
	if cl == nil {
		return "", noCluster(key)
	}
	old := c.applied()
	if len(old.sc.ClusterKeys()) == 1 {
		return "", invalid("This is the only cluster. The simulator needs at least one, so add another before removing this one.")
	}
	next := clone(old.sc)
	next.Removed = append(next.Removed, key)
	sort.Strings(next.Removed)
	var keep []scenario.ClusterOverride
	for _, o := range next.Clusters {
		if o.Key != key {
			keep = append(keep, o)
		}
	}
	next.Clusters = keep
	var conds, ended []scenario.Condition
	for _, cond := range next.Conditions {
		if len(cond.Clusters) > 0 {
			var ks []string
			for _, k := range cond.Clusters {
				if k != key {
					ks = append(ks, k)
				}
			}
			if len(ks) == 0 {
				ended = append(ended, cond)
				continue
			}
			cond.Clusters = ks
		}
		conds = append(conds, cond)
	}
	next.Conditions = conds
	if _, err := reconcileStorage(next, old.sc); err != nil {
		return "", &Invalid{err.Error()}
	}
	text := "Remove " + clusterLabel(old.sc, key)
	if _, err := c.applyScenario(next, false); err != nil {
		c.fail(text, err, "", nil, "/cluster/"+key)
		return "", err
	}
	done := fmt.Sprintf("%s removed, with its %s.", clusterLabel(old.sc, key), plural(len(cl.Hosts), "host"))
	c.failure = nil
	c.addHistory(done)
	for _, cond := range ended {
		// it applied to this cluster alone, so it went with it
		c.ended = append(c.ended, EndedCondition{ID: cond.ID, Kind: cond.Kind, Text: c.condText(old.sc, cond), At: c.now()})
		c.addHistory(fmt.Sprintf("Turned off with %s, the only cluster it applied to: %s", clusterLabel(old.sc, key), lower1(c.condText(old.sc, cond))))
	}
	c.save()
	return done + " The other clusters are unchanged.", nil
}
