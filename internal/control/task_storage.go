package control

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// Shared storage: iSCSI datastores (one cluster each) and NFS datastores
// (one datacenter, some or all of its clusters). The forms ask for a name,
// a size and how full; the simulator invents every connection detail and
// shows it afterwards.

func init() {
	tasks["add-iscsi"] = taskDef{form: formAddISCSI, plan: planAddISCSI}
	tasks["add-nfs"] = taskDef{form: formAddNFS, plan: planAddNFS}
	tasks["storage-size"] = taskDef{form: formStorageSize, plan: planStorageSize}
	tasks["storage-disconnect"] = taskDef{form: formDisconnect, plan: planDisconnect}
}

// fullness is "how full" as the forms ask it: the label and the share of
// the datastore that is free.
var fullness = []struct {
	Label string
	Free  float64
}{{"Empty", 1}, {"A quarter full", 0.75}, {"Half full", 0.5}, {"Nearly full", 0.1}}

func fullLabels() []string {
	out := make([]string, len(fullness))
	for i, f := range fullness {
		out[i] = f.Label
	}
	return out
}

// fullOf picks the label nearest to a datastore's free share.
func fullOf(capacity, free int64) string {
	if capacity <= 0 {
		return fullness[0].Label
	}
	share := float64(free) / float64(capacity)
	best, gap := 0, 2.0
	for i, f := range fullness {
		d := share - f.Free
		if d < 0 {
			d = -d
		}
		if d < gap {
			best, gap = i, d
		}
	}
	return fullness[best].Label
}

func freeShare(label string) (float64, bool) {
	for _, f := range fullness {
		if f.Label == label {
			return f.Free, true
		}
	}
	return 0, false
}

// sizeBytes turns "2" and "TB" into bytes.
func sizeBytes(n int, unit string) int64 {
	if unit == "TB" {
		return int64(n) << 40
	}
	return int64(n) << 30
}

// sizeParts is the reverse, for a form's boxes: a whole number and a unit.
func sizeParts(b int64) (int, string) {
	if b >= 1<<40 && b%(1<<40) == 0 {
		return int(b >> 40), "TB"
	}
	return int(max(1, b>>30)), "GB"
}

var storeNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// store is one shared datastore, wherever the scenario keeps it.
type store struct {
	ID, Name, DC string
	ISCSI        bool
	Capacity     int64
	Free         int64
	// Clusters are the cluster keys that use it; All means every cluster of
	// the datacenter, now and later.
	Clusters []string
	All      bool
	// Cut are the keys of hosts that cannot reach it.
	Cut []string
	// connection details
	Server, Folder, Target string
	Port                   int32
	Adapters, PathsPerHost int

	dc, i, j int // indexes into the scenario: datacenter, NFS or iSCSI entry, LUN
}

// stores lists every shared datastore of a scenario.
func stores(sc *scenario.Scenario) []store {
	var out []store
	hostsOf := func(key string) []string {
		var hs []string
		gone := sc.RemovedHosts(key)
		for n := 1; n <= sc.HostNumbers(key); n++ {
			if !gone[n] {
				hs = append(hs, key+"/"+hostName(n))
			}
		}
		return hs
	}
	for di, dc := range sc.Datacenters {
		var inDC []string
		for _, k := range sc.ClusterKeys() {
			if datacenterOf(k) == dc.Name {
				inDC = append(inDC, k)
			}
		}
		for ni, n := range dc.NFS {
			s := store{ID: dc.Name + "/" + n.Name, Name: n.Name, DC: dc.Name, Capacity: n.Capacity, Free: n.FreeSpace,
				Server: n.RemoteHost, Folder: n.RemotePath, dc: di, i: ni}
			if len(n.Clusters) == 0 {
				s.All, s.Clusters = true, inDC
			} else {
				s.Clusters = append([]string(nil), n.Clusters...)
			}
			for _, h := range n.InaccessibleHosts {
				if slices.Contains(s.Clusters, h[:strings.LastIndexByte(h, '/')]) {
					s.Cut = append(s.Cut, h)
				}
			}
			out = append(out, s)
		}
		for si, st := range dc.ISCSI {
			targets := map[string]scenario.ISCSITarget{}
			for _, t := range st.Targets {
				targets[t.Name] = t
			}
			for li, l := range st.LUNs {
				t := targets[l.Target]
				port := t.Port
				if port == 0 {
					port = 3260
				}
				s := store{ID: dc.Name + "/" + l.Name, Name: l.Name, DC: dc.Name, ISCSI: true, Capacity: l.Capacity, Free: l.FreeSpace,
					Clusters: []string{st.Cluster}, Server: t.Address, Target: t.IQN, Port: port, Adapters: len(st.Adapters), dc: di, i: si, j: li}
				for _, a := range st.Adapters {
					if _, ok := l.PathState(a.Name, ""); ok {
						s.PathsPerHost++
					}
				}
				for _, h := range hostsOf(st.Cluster) {
					if !l.Reachable(st.Adapters, h) {
						s.Cut = append(s.Cut, h)
					}
				}
				out = append(out, s)
			}
		}
	}
	return out
}

func findStore(sc *scenario.Scenario, id string) (store, bool) {
	for _, s := range stores(sc) {
		if s.ID == id {
			return s, true
		}
	}
	return store{}, false
}

func noStore(id string) error {
	name := id
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		name = id[i+1:]
	}
	return invalid("There is no storage called %s. It may have been removed.", name)
}

// usedBy says which clusters use a datastore.
func usedBy(s store) string {
	if s.All {
		return "Every cluster in " + s.DC
	}
	names := make([]string, 0, len(s.Clusters))
	for _, k := range s.Clusters {
		names = append(names, shortCluster(k))
	}
	return listAnd(names)
}

func kindName(s store) string {
	if s.ISCSI {
		return "iSCSI"
	}
	return "NFS"
}

// hostLabel is "esx02 in cluster-01".
func hostLabel(key string) string {
	i := strings.LastIndexByte(key, '/')
	return key[i+1:] + " in " + shortCluster(key[:i])
}

// ---------------------------------------------------------------------------
// the screens

// StorageRow is one shared datastore in the list and on its own page.
type StorageRow struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	DC     string `json:"dc"`
	UsedBy string `json:"usedBy"`
	Size   string `json:"size"`
	Free   string `json:"free"`
	Full   string `json:"full"`
	// Cut lists the hosts that cannot reach it, as "esx02 in cluster-01".
	Cut []string `json:"cut"`
	// Details are the connection details tools will see, as label and
	// value pairs in order.
	Details [][2]string `json:"details,omitempty"`
}

func storageRow(s store, details bool) StorageRow {
	r := StorageRow{ID: s.ID, Name: s.Name, Kind: kindName(s), DC: s.DC, UsedBy: usedBy(s), Size: sizeText(s.Capacity), Free: sizeText(s.Free),
		Full: fullOf(s.Capacity, s.Free), Cut: []string{}}
	for _, h := range s.Cut {
		r.Cut = append(r.Cut, hostLabel(h))
	}
	sort.Strings(r.Cut)
	if !details {
		return r
	}
	if s.ISCSI {
		r.Details = [][2]string{{"Storage server", fmt.Sprintf("%s:%d", s.Server, s.Port)}, {"Target name", s.Target},
			{"Each host connects with", plural(s.Adapters, "iSCSI adapter") + " and " + plural(s.PathsPerHost, "path")}}
	} else {
		r.Details = [][2]string{{"NFS server", s.Server}, {"Shared folder", s.Folder}, {"Kind", "NFS version 3"}}
	}
	return r
}

// StorageView is the Storage screen.
type StorageView struct {
	Rows     []StorageRow `json:"rows"`
	Clusters int          `json:"clusters"`
	Hosts    int          `json:"hosts"`
	// Multi is true when there is more than one datacenter, so the list
	// says which one each datastore is in.
	Multi bool `json:"multi"`
}

// Storage lists the shared datastores.
func (c *Controller) Storage() StorageView {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.applied()
	cl, h, _, _ := r.topo.Counts()
	v := StorageView{Rows: []StorageRow{}, Clusters: cl, Hosts: h, Multi: len(r.sc.Datacenters) > 1}
	for _, s := range stores(r.sc) {
		v.Rows = append(v.Rows, storageRow(s, false))
	}
	return v
}

// StorageItem returns one datastore with its connection details.
func (c *Controller) StorageItem(id string) (*StorageRow, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := findStore(c.applied().sc, id)
	if !ok {
		return nil, noStore(id)
	}
	row := storageRow(s, true)
	return &row, nil
}

// ---------------------------------------------------------------------------
// add an iSCSI datastore

type storageParams struct {
	ID      string   `json:"id"`
	Cluster string   `json:"cluster"`
	DC      string   `json:"dc"`
	Who     string   `json:"who"`
	Pick    []string `json:"pick"`
	Name    string   `json:"name"`
	Size    N        `json:"size"`
	Unit    string   `json:"unit"`
	Full    string   `json:"full"`
	Edit    bool     `json:"edit"`
	Server  string   `json:"server"`
	Port    N        `json:"port"`
	Target  string   `json:"target"`
	Folder  string   `json:"folder"`
}

// nextStoreName is the first free "<prefix>-N" in every datacenter.
func nextStoreName(all []store, prefix string) string {
	for i := 1; ; i++ {
		name := fmt.Sprintf("%s-%d", prefix, i)
		if !slices.ContainsFunc(all, func(s store) bool { return s.Name == name }) {
			return name
		}
	}
}

// freeAddress is the first address "<base><n>" no datastore uses yet.
func freeAddress(all []store, base string, from int) string {
	for n := from; ; n++ {
		addr := fmt.Sprintf("%s%d", base, n)
		if !slices.ContainsFunc(all, func(s store) bool { return s.Server == addr }) {
			return addr
		}
	}
}

func (p *Plan) checkSize(in storageParams, all []store, adding bool, example string) {
	if adding {
		switch {
		case !storeNameRe.MatchString(in.Name):
			p.bad("name", "Use only small letters, numbers and hyphens, for example %s.", example)
		case slices.ContainsFunc(all, func(s store) bool { return s.Name == in.Name }):
			p.bad("name", "There is already storage called %s. Choose another name.", in.Name)
		}
	}
	if !in.Size.in(1, 9999) {
		p.bad("size", "Enter a size from 1 to 9999.")
	}
	if in.Unit != "GB" && in.Unit != "TB" {
		p.bad("size", "Choose GB or TB.")
	}
	if _, ok := freeShare(in.Full); !ok {
		p.bad("full", "Choose how full it is from the list.")
	}
}

func clusterChoices(c *Controller) [][2]string {
	r := c.applied()
	var out [][2]string
	for _, cl := range r.topo.Clusters() {
		out = append(out, [2]string{cl.Key, fmt.Sprintf("%s (%s, %s)", cl.Name, plural(len(cl.Hosts), "host"), cl.Datacenter.Name)})
	}
	return out
}

func iqnFor(c *Controller, cluster string, n int) string {
	return fmt.Sprintf("iqn.%s.example.lab:%s-%d", c.now().Format("2006-01"), shortCluster(cluster), n)
}

func formAddISCSI(c *Controller, q map[string]string, _ Caller) (*Form, error) {
	r := c.applied()
	all := stores(r.sc)
	n := 1
	for _, s := range all {
		if s.ISCSI {
			n++
		}
	}
	clusters := r.topo.Clusters()
	cluster := clusters[len(clusters)-1].Key
	if c.findCluster(q["cluster"]) != nil {
		cluster = q["cluster"]
	}
	targets := map[string]string{}
	for _, cl := range clusters {
		targets[cl.Key] = iqnFor(c, cl.Key, n)
	}
	name := nextStoreName(all, "iscsi-ds")
	return &Form{Title: "Add an iSCSI datastore", Crumbs: [][2]string{{"Storage", "/storage"}}, Mix: true,
		Intro: "A shared datastore that every host in one cluster connects to. The boxes are already filled in; change only what matters to your test.",
		Values: map[string]any{"cluster": cluster, "name": name, "size": 2, "unit": "TB", "full": "Half full", "edit": false,
			"server": freeAddress(all, "10.0.50.", 20+n), "port": 3260, "target": targets[cluster]},
		Options: map[string]any{"clusters": clusterChoices(c), "targets": targets, "full": fullLabels(), "example": name}}, nil
}

func planAddISCSI(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in storageParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	old := c.applied()
	all := stores(old.sc)
	p := &Plan{Go: "Add iSCSI datastore", dest: "/storage"}
	cl := c.findCluster(in.Cluster)
	if cl == nil {
		p.bad("cluster", "Choose a cluster from the list.")
	}
	p.checkSize(in, all, true, nextStoreName(all, "iscsi-ds"))
	in.Server, in.Target = strings.TrimSpace(in.Server), strings.TrimSpace(in.Target)
	if in.Edit {
		if in.Server == "" || strings.ContainsAny(in.Server, " /") {
			p.bad("server", "Enter the address the storage server should appear to have, for example 10.0.50.21.")
		}
		if !strings.HasPrefix(in.Target, "iqn.") {
			p.bad("target", `A target name starts with iqn. Untick "I want to choose them myself" to get a valid one back.`)
		}
		if !in.Port.in(1, 65535) {
			p.bad("port", "Enter a port from 1 to 65535. The usual one is 3260.")
		}
	}
	if len(p.Errors) > 0 {
		return p, nil
	}
	if !in.Edit {
		n := 1
		for _, s := range all {
			if s.ISCSI {
				n++
			}
		}
		in.Server, in.Target, in.Port = freeAddress(all, "10.0.50.", 20+n), iqnFor(c, cl.Key, n), N{3260, true}
	}
	capacity := sizeBytes(in.Size.V, in.Unit)
	share, _ := freeShare(in.Full)
	size := fmt.Sprintf("%d %s", in.Size.V, in.Unit)
	p.Sum = []Item{
		say("Adds a %s iSCSI datastore called %s, %s.", bold(size), bold(in.Name), strings.ToLower(in.Full)),
		say("All %s in %s connect to it and report it as working.", plural(len(cl.Hosts), "host"), bold(cl.Name)),
		note("No other cluster can see it."), keepNote}

	next := clone(old.sc)
	di := -1
	for i, dc := range next.Datacenters {
		if dc.Name == cl.Datacenter.Name {
			di = i
		}
	}
	dc := &next.Datacenters[di]
	si := -1
	for i, st := range dc.ISCSI {
		if st.Cluster == cl.Key {
			si = i
		}
	}
	if si < 0 {
		dc.ISCSI = append(dc.ISCSI, scenario.ISCSIStorage{Cluster: cl.Key, Adapters: []scenario.ISCSIAdapter{{Name: "adapter-1", Kind: "software"}}})
		si = len(dc.ISCSI) - 1
	}
	st := &dc.ISCSI[si]
	// a target is named after its datastore; names already taken get a number
	target := in.Name
	for n := 2; slices.ContainsFunc(st.Targets, func(t scenario.ISCSITarget) bool { return t.Name == target }); n++ {
		target = fmt.Sprintf("%s-%d", in.Name, n)
	}
	st.Targets = append(st.Targets, scenario.ISCSITarget{Name: target, Address: in.Server, IQN: in.Target, Port: int32(in.Port.V)})
	lun := scenario.ISCSILUN{Name: in.Name, Target: target, Number: 0, Capacity: capacity, FreeSpace: int64(float64(capacity) * share)}
	for _, a := range st.Adapters {
		lun.Paths = append(lun.Paths, scenario.ISCSIPath{Adapter: a.Name, State: "active"})
	}
	st.LUNs = append(st.LUNs, lun)
	if err := clone(next).Validate(); err != nil {
		p.bad("name", "%s", plainScenarioError(err))
		return p, nil
	}
	id := dc.Name + "/" + in.Name
	p.text = fmt.Sprintf("Add iSCSI datastore %s for %s", in.Name, clusterLabel(old.sc, cl.Key))
	p.apply = func() (*Outcome, error) {
		if _, err := c.applyScenario(next, false); err != nil {
			return nil, err
		}
		done := fmt.Sprintf("iSCSI datastore %s (%s) added for %s.", in.Name, size, clusterLabel(old.sc, cl.Key))
		return &Outcome{Done: done + " Every host in that cluster is connected to it.", history: done, Marks: []Mark{{Type: "storage", ID: id, Tag: "New"}}}, nil
	}
	return p, nil
}

// plainScenarioError rewords a scenario validation error for a form.
func plainScenarioError(err error) string {
	msg := strings.TrimPrefix(err.Error(), "scenario: ")
	if strings.Contains(msg, "invalid iSCSI target") {
		return "That storage server or target name cannot be used. Check them, or untick the box to let the simulator choose."
	}
	return upper1(msg) + "."
}

// ---------------------------------------------------------------------------
// add an NFS datastore

func clustersByDC(c *Controller) map[string][][2]string {
	out := map[string][][2]string{}
	for _, cl := range c.applied().topo.Clusters() {
		out[cl.Datacenter.Name] = append(out[cl.Datacenter.Name], [2]string{cl.Key, cl.Name})
	}
	return out
}

func formAddNFS(c *Controller, q map[string]string, _ Caller) (*Form, error) {
	r := c.applied()
	all := stores(r.sc)
	n := 1
	for _, s := range all {
		if !s.ISCSI {
			n++
		}
	}
	dcs := datacenterNames(r.sc)
	name := nextStoreName(all, "nfs-ds")
	v := map[string]any{"dc": dcs[0], "who": "all", "pick": []string{}, "name": name, "size": 4, "unit": "TB", "full": "A quarter full", "edit": false,
		"server": freeAddress(all, "10.0.60.", 10+n), "folder": "/exports/" + name}
	if cl := c.findCluster(q["cluster"]); cl != nil {
		v["dc"], v["who"], v["pick"] = cl.Datacenter.Name, "some", []string{cl.Key}
	}
	return &Form{Title: "Add an NFS datastore", Crumbs: [][2]string{{"Storage", "/storage"}}, Mix: true,
		Intro:   "A shared datastore that hosts in one datacenter connect to. The boxes are already filled in; change only what matters to your test.",
		Values:  v,
		Options: map[string]any{"dcs": dcs, "clusters": clustersByDC(c), "full": fullLabels(), "example": name}}, nil
}

func planAddNFS(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in storageParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	old := c.applied()
	all := stores(old.sc)
	p := &Plan{Go: "Add NFS datastore", dest: "/storage"}
	di := -1
	for i, dc := range old.sc.Datacenters {
		if dc.Name == in.DC {
			di = i
		}
	}
	if di < 0 {
		p.bad("dc", "Choose a datacenter from the list.")
		return p, nil
	}
	p.checkSize(in, all, true, nextStoreName(all, "nfs-ds"))
	hostsIn := map[string]int{}
	var inDC []string
	for _, cl := range old.topo.Clusters() {
		if cl.Datacenter.Name == in.DC {
			inDC = append(inDC, cl.Key)
			hostsIn[cl.Key] = len(cl.Hosts)
		}
	}
	var pick []string
	if in.Who == "some" {
		for _, k := range inDC {
			if slices.Contains(in.Pick, k) {
				pick = append(pick, k)
			}
		}
		if len(pick) == 0 {
			p.bad("pick", `Tick at least one cluster, or choose "Every cluster".`)
		}
	}
	in.Server, in.Folder = strings.TrimSpace(in.Server), strings.TrimSpace(in.Folder)
	if in.Edit {
		if in.Server == "" || strings.ContainsAny(in.Server, " /") {
			p.bad("server", "Enter the address the NFS server should appear to have, for example 10.0.60.11.")
		}
		if !strings.HasPrefix(in.Folder, "/") {
			p.bad("folder", "A shared folder starts with a slash, for example /exports/nfs-ds-2.")
		}
	} else {
		n := 1
		for _, s := range all {
			if !s.ISCSI {
				n++
			}
		}
		in.Server, in.Folder = freeAddress(all, "10.0.60.", 10+n), "/exports/"+in.Name
	}
	size := fmt.Sprintf("%d %s", in.Size.V, in.Unit)
	if in.Size.OK && storeNameRe.MatchString(in.Name) {
		p.Sum = append(p.Sum, say("Adds a %s NFS datastore called %s, %s.", bold(size), bold(in.Name), strings.ToLower(in.Full)))
		hosts := 0
		if in.Who == "some" {
			for _, k := range pick {
				hosts += hostsIn[k]
			}
			switch {
			case len(pick) == 0:
				p.Sum = append(p.Sum, say("Tick at least one cluster."))
			case len(pick) > 4:
				p.Sum = append(p.Sum, say("The %s in %s connect to it and report it as working.", plural(hosts, "host"), bold(plural(len(pick), "cluster"))))
			default:
				p.Sum = append(p.Sum, say("The %s in %s connect to it and report it as working.", plural(hosts, "host"), bold(listAnd(clusterShorts(pick)))))
			}
		} else {
			for _, k := range inDC {
				hosts += hostsIn[k]
			}
			p.Sum = append(p.Sum, say("Every host in %s (%s in %s) connects to it and reports it as working.", bold(in.DC), plural(hosts, "host"), plural(len(inDC), "cluster")))
		}
		p.Sum = append(p.Sum, keepNote)
	}
	if len(p.Errors) > 0 {
		return p, nil
	}
	capacity := sizeBytes(in.Size.V, in.Unit)
	share, _ := freeShare(in.Full)
	next := clone(old.sc)
	next.Datacenters[di].NFS = append(next.Datacenters[di].NFS, scenario.NFSDatastore{Name: in.Name, RemoteHost: in.Server, RemotePath: in.Folder,
		Capacity: capacity, FreeSpace: int64(float64(capacity) * share), Clusters: pick})
	if err := clone(next).Validate(); err != nil {
		p.bad("name", "%s", plainScenarioError(err))
		return p, nil
	}
	who := "every cluster in " + in.DC
	if in.Who == "some" {
		who = listAnd(clusterShorts(pick))
	}
	p.text = "Add NFS datastore " + in.Name
	p.apply = func() (*Outcome, error) {
		if _, err := c.applyScenario(next, false); err != nil {
			return nil, err
		}
		return &Outcome{Done: fmt.Sprintf("NFS datastore %s (%s) added for %s.", in.Name, size, who), Marks: []Mark{{Type: "storage", ID: in.DC + "/" + in.Name, Tag: "New"}}}, nil
	}
	return p, nil
}

func clusterShorts(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, shortCluster(k))
	}
	return out
}

// ---------------------------------------------------------------------------
// change size, disconnect hosts, reconnect, remove

func storeCrumbs(s store) [][2]string {
	return [][2]string{{"Storage", "/storage"}, {s.Name, "/storage/" + s.ID}}
}

func formStorageSize(c *Controller, q map[string]string, _ Caller) (*Form, error) {
	s, ok := findStore(c.applied().sc, q["id"])
	if !ok {
		return nil, noStore(q["id"])
	}
	n, unit := sizeParts(s.Capacity)
	return &Form{Title: "Change the size of " + s.Name, Crumbs: storeCrumbs(s),
		Values:  map[string]any{"id": s.ID, "size": n, "unit": unit, "full": fullOf(s.Capacity, s.Free)},
		Options: map[string]any{"full": fullLabels()}}, nil
}

// setStore writes a datastore's size back into a scenario.
func setStore(sc *scenario.Scenario, s store, capacity, free int64) {
	if s.ISCSI {
		l := &sc.Datacenters[s.dc].ISCSI[s.i].LUNs[s.j]
		l.Capacity, l.FreeSpace = capacity, free
		return
	}
	n := &sc.Datacenters[s.dc].NFS[s.i]
	n.Capacity, n.FreeSpace = capacity, free
}

func planStorageSize(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in storageParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	old := c.applied()
	s, ok := findStore(old.sc, in.ID)
	if !ok {
		return nil, noStore(in.ID)
	}
	p := &Plan{Go: "Change size", dest: "/storage/" + s.ID}
	p.checkSize(in, nil, false, "")
	if len(p.Errors) > 0 {
		return p, nil
	}
	capacity := sizeBytes(in.Size.V, in.Unit)
	share, _ := freeShare(in.Full)
	free := int64(float64(capacity) * share)
	size := fmt.Sprintf("%d %s", in.Size.V, in.Unit)
	if capacity == s.Capacity && in.Full == fullOf(s.Capacity, s.Free) {
		p.Idle = "Nothing is different yet. Change the size or how full it is and this box will say what happens."
		return p, nil
	}
	p.Sum = []Item{say("%s becomes %s, %s.", s.Name, bold(size), strings.ToLower(in.Full)), note("Tools see the same datastore with new size and free space.")}
	next := clone(old.sc)
	setStore(next, s, capacity, free)
	p.text = "Change the size of " + s.Name
	p.apply = func() (*Outcome, error) {
		if _, err := c.applyScenario(next, false); err != nil {
			return nil, err
		}
		return &Outcome{Done: fmt.Sprintf("%s is now %s, %s.", s.Name, size, strings.ToLower(in.Full)), Marks: []Mark{{Type: "storage", ID: s.ID, Tag: "Changed"}}}, nil
	}
	return p, nil
}

func formDisconnect(c *Controller, q map[string]string, _ Caller) (*Form, error) {
	r := c.applied()
	s, ok := findStore(r.sc, q["id"])
	if !ok {
		return nil, noStore(q["id"])
	}
	hosts := map[string][]string{}
	var clusters [][2]string
	for _, cl := range r.topo.Clusters() {
		if slices.Contains(s.Clusters, cl.Key) {
			clusters = append(clusters, [2]string{cl.Key, cl.Name})
			hosts[cl.Key] = hostNames(cl)
		}
	}
	if len(clusters) == 0 {
		return nil, invalid("No cluster uses %s, so there is no host to disconnect.", s.Name)
	}
	return &Form{Title: "Disconnect hosts from " + s.Name, Crumbs: storeCrumbs(s),
		Intro:   "The hosts you tick report that they cannot reach this storage. Use it to see how a tool shows a storage problem.",
		Values:  map[string]any{"id": s.ID, "cluster": clusters[0][0], "pick": []string{}},
		Options: map[string]any{"clusters": clusters, "hosts": hosts}}, nil
}

func planDisconnect(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in storageParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	old := c.applied()
	s, ok := findStore(old.sc, in.ID)
	if !ok {
		return nil, noStore(in.ID)
	}
	p := &Plan{Go: "Disconnect hosts", dest: "/storage/" + s.ID}
	cl := c.findCluster(in.Cluster)
	if cl == nil || !slices.Contains(s.Clusters, in.Cluster) {
		p.bad("cluster", "Choose a cluster from the list.")
		return p, nil
	}
	have := hostNames(cl)
	var pick []string
	for _, h := range have {
		if slices.Contains(in.Pick, h) {
			pick = append(pick, h)
		}
	}
	if len(pick) == 0 {
		p.bad("pick", "Tick at least one host.")
		p.Idle = "Tick the hosts and this box will say what happens."
		return p, nil
	}
	verb := "report"
	if len(pick) == 1 {
		verb = "reports"
	}
	p.Sum = []Item{
		say("%s in %s %s %s as unreachable.", bold(listAnd(pick)), cl.Name, verb, s.Name),
		say("Every other host keeps working with it."),
		note(`It stays this way until you press "Reconnect every host" on the storage page.`)}
	next := clone(old.sc)
	for _, h := range pick {
		key := cl.Key + "/" + h
		if s.ISCSI {
			st := &next.Datacenters[s.dc].ISCSI[s.i]
			l := &st.LUNs[s.j]
			for _, a := range st.Adapters {
				l.Paths = slices.DeleteFunc(l.Paths, func(x scenario.ISCSIPath) bool { return x.Adapter == a.Name && x.Host == key })
				l.Paths = append(l.Paths, scenario.ISCSIPath{Adapter: a.Name, Host: key, State: "dead"})
			}
		} else {
			n := &next.Datacenters[s.dc].NFS[s.i]
			if !slices.Contains(n.InaccessibleHosts, key) {
				n.InaccessibleHosts = append(n.InaccessibleHosts, key)
			}
		}
	}
	p.text = fmt.Sprintf("Disconnect %s from %s", plural(len(pick), "host"), s.Name)
	p.apply = func() (*Outcome, error) {
		if _, err := c.applyScenario(next, false); err != nil {
			return nil, err
		}
		return &Outcome{Done: fmt.Sprintf("%s in %s can no longer reach %s.", listAnd(pick), clusterLabel(old.sc, cl.Key), s.Name)}, nil
	}
	return p, nil
}

// storageChange applies an immediate change to one datastore and returns
// the words that say what happened.
func (c *Controller) storageChange(id, text string, change func(*scenario.Scenario, store), done, history func(store) string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep()
	old := c.applied()
	s, ok := findStore(old.sc, id)
	if !ok {
		return "", noStore(id)
	}
	next := clone(old.sc)
	change(next, s)
	if _, err := c.applyScenario(next, false); err != nil {
		c.fail(text+" "+s.Name, err, "", nil, "/storage/"+id)
		return "", err
	}
	c.failure = nil
	c.addHistory(history(s))
	c.save()
	return done(s), nil
}

// ReconnectStorage lets every host reach a datastore again.
func (c *Controller) ReconnectStorage(id string) (string, error) {
	return c.storageChange(id, "Reconnect every host to", func(sc *scenario.Scenario, s store) {
		if !s.ISCSI {
			sc.Datacenters[s.dc].NFS[s.i].InaccessibleHosts = nil
			return
		}
		l := &sc.Datacenters[s.dc].ISCSI[s.i].LUNs[s.j]
		l.Paths = slices.DeleteFunc(l.Paths, func(x scenario.ISCSIPath) bool { return x.Host != "" })
		working := slices.ContainsFunc(l.Paths, func(x scenario.ISCSIPath) bool { return x.State == "active" || x.State == "standby" })
		if !working {
			for i := range l.Paths {
				l.Paths[i].State = "active"
			}
		}
	}, func(s store) string { return fmt.Sprintf("Every host can reach %s again.", s.Name) },
		func(s store) string { return fmt.Sprintf("Every host reconnected to %s.", s.Name) })
}

// RemoveStorage removes a shared datastore.
func (c *Controller) RemoveStorage(id string) (string, error) {
	return c.storageChange(id, "Remove", func(sc *scenario.Scenario, s store) {
		dc := &sc.Datacenters[s.dc]
		if !s.ISCSI {
			dc.NFS = slices.Delete(dc.NFS, s.i, s.i+1)
			return
		}
		st := &dc.ISCSI[s.i]
		target := st.LUNs[s.j].Target
		st.LUNs = slices.Delete(st.LUNs, s.j, s.j+1)
		if !slices.ContainsFunc(st.LUNs, func(l scenario.ISCSILUN) bool { return l.Target == target }) {
			st.Targets = slices.DeleteFunc(st.Targets, func(t scenario.ISCSITarget) bool { return t.Name == target })
		}
		if len(st.LUNs) == 0 {
			dc.ISCSI = slices.Delete(dc.ISCSI, s.i, s.i+1)
		}
	}, func(s store) string { return fmt.Sprintf("%s was removed.", s.Name) },
		func(s store) string { return fmt.Sprintf("Storage %s removed.", s.Name) })
}
