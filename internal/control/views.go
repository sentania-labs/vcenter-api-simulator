package control

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// toolViews lists the tools as they stand right now. c.mu is held.
func (c *Controller) toolViews() []ToolView {
	return c.tools.list(c.now(), c.paused.Load(), c.pausedAt)
}

// Address is the address tools should use, as one string.
func (c *Controller) Address() string {
	if c.opts.PublicURL != "" {
		return strings.TrimRight(c.opts.PublicURL, "/")
	}
	host := c.opts.Endpoint
	if host == "" {
		host = "localhost"
	}
	port := "443"
	if len(c.opts.APIAddrs) > 0 {
		if _, p, err := net.SplitHostPort(c.opts.APIAddrs[0]); err == nil {
			port = p
		}
	}
	if port == "443" {
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, port)
}

// ---------------------------------------------------------------------------
// Home

// Attention is one thing that needs looking at, with the one button that
// deals with it.
type Attention struct {
	K     string `json:"k"` // bad | warn | info
	T     string `json:"t"`
	D     string `json:"d"`
	Label string `json:"label"`
	// Href is where the button goes; or Act names what it does at once
	// (retry, condOff) with ID.
	Href string `json:"href,omitempty"`
	Act  string `json:"act,omitempty"`
	ID   int    `json:"id,omitempty"`
}

// HomeTool is a tool with what it has been asking for.
type HomeTool struct {
	ToolView
	// Asks says in a phrase what it asks for; Unknown and Fault count the
	// kinds of request not answered and failing on purpose. Carried is
	// false when it asked for nothing more after an unanswered request.
	Asks    string `json:"asks"`
	Unknown int    `json:"unknown"`
	Fault   int    `json:"fault"`
	Carried bool   `json:"carried"`
}

// Estimate is the size of one full collection.
type Estimate struct {
	Requests int    `json:"requests"`
	Size     string `json:"size"`
}

// Connect is what to give a tool so that it can connect.
type Connect struct {
	Username string `json:"username"`
	AnyLogin bool   `json:"anyLogin"`
	// Password says where the account's password came from: any (every
	// password is accepted), inuse (the account is in use by a tool),
	// console (set on the Users screen), setup (chosen in first-run
	// set-up) or installer (supplied by the deployment).
	Password string `json:"password"`
	InUseBy  string `json:"inUseBy,omitempty"`
	// Cert is made, own or managed; Issuer who issued an own certificate.
	Cert   string `json:"cert"`
	Issuer string `json:"issuer,omitempty"`
}

// HomeView is Home, and what the shell around every screen needs.
type HomeView struct {
	Now      time.Time  `json:"now"`
	Running  bool       `json:"running"`
	PausedAt *time.Time `json:"pausedAt,omitempty"`
	Address  string     `json:"address"`
	VCenter  string     `json:"vcenter"`
	Counts   struct {
		Clusters    int `json:"clusters"`
		Hosts       int `json:"hosts"`
		VMs         int `json:"vms"`
		Datastores  int `json:"datastores"`
		Datacenters int `json:"datacenters"`
	} `json:"counts"`
	EstimateAll  Estimate        `json:"estimateAll"`
	EstimateDash Estimate        `json:"estimateDash"`
	Tools        []HomeTool      `json:"tools"`
	Attention    []Attention     `json:"attention"`
	Scheduled    []ScheduledView `json:"scheduled"`
	Recent       []TimelineEntry `json:"recent"`
	HistoryCount int             `json:"historyCount"`
	Connect      Connect         `json:"connect"`
	Failure      *Failure        `json:"failure,omitempty"`
	// Unanswered and Faults are the tab badges; Capturing is true while
	// every request is captured in full.
	Unanswered int  `json:"unanswered"`
	Faults     int  `json:"faults"`
	Capturing  bool `json:"capturing"`
	// AccountNeeded: first-run set-up has not chosen the tools' password.
	AccountNeeded bool `json:"accountNeeded"`
}

// Home assembles Home.
func (c *Controller) Home() *HomeView {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep()
	r := c.applied()
	now := c.now()
	v := &HomeView{Now: now, Running: !c.paused.Load(), Address: c.Address(), VCenter: scenario.VersionLabel(r.topo.VCenterVersion),
		Tools: []HomeTool{}, Attention: []Attention{}, Scheduled: c.scheduledViews(), Failure: c.failure, AccountNeeded: c.AccountNeeded()}
	if !v.Running {
		at := c.pausedAt
		v.PausedAt = &at
	}
	proj := Project(c.cat, r.topo)
	v.Counts.Clusters, v.Counts.Hosts, v.Counts.VMs = proj.Clusters, proj.Hosts, proj.VMs
	v.Counts.Datastores, v.Counts.Datacenters = proj.Datastores, len(r.topo.Datacenters)
	v.EstimateAll = Estimate{proj.All.Calls, megabytes(proj.All.Bytes)}
	v.EstimateDash = Estimate{proj.Dash.Calls, megabytes(proj.Dash.Bytes)}

	asks := c.asks()
	tools := c.toolViews()
	counts := countAsks(asks)
	live := 0
	for _, t := range tools {
		ht := HomeTool{ToolView: t, Carried: true}
		key := toolKey(t.Addr, t.Agent)
		var topics []string
		seen := map[string]bool{}
		topic := func(s string) {
			if !seen[s] {
				seen[s] = true
				topics = append(topics, s)
			}
		}
		for _, a := range asks {
			if a.client != key {
				continue
			}
			switch a.St {
			case "unknown":
				ht.Unknown++
				if strings.HasPrefix(a.Short, "The tool asked for nothing") {
					ht.Carried = false
				}
			case "fault":
				ht.Fault++
			}
			switch {
			case a.T == kindInventory:
				topic("lists of clusters, hosts and VMs")
			case strings.HasPrefix(a.T, "vSAN performance data"):
				topic("vSAN performance data")
			case a.T == "vSAN health summary":
				topic("vSAN health")
			case a.T == "vSAN settings of each cluster" || a.T == "vSAN disks in each host":
				topic("vSAN settings and disks")
			}
		}
		ht.Asks = listAnd(topics)
		if t.Connected() {
			live++
		}
		v.Tools = append(v.Tools, ht)
	}

	// what needs attention, most serious first
	add := func(a Attention) { v.Attention = append(v.Attention, a) }
	if c.unsaved != "" {
		add(Attention{K: "bad", T: "The simulator could not save what was changed",
			D: "The changes are live, but they will be lost if the simulator restarts. The machine it runs on may be out of disk space; once there is room, the next change saves everything.", Label: "See what was changed", Href: "/home/history"})
	}
	if f := c.failure; f != nil {
		add(Attention{K: "bad", T: "A change failed at " + timeToken(f.At), D: f.What + " Nothing was changed.", Label: "Try again", Act: "retry"})
	}
	for _, t := range tools {
		if t.Since == nil {
			continue
		}
		switch t.State {
		case ToolRemoved:
			add(Attention{K: "bad", T: fmt.Sprintf("The tool at %s can no longer sign in", t.Addr),
				D: fmt.Sprintf("Its account, %s, was removed at %s. Give the tool another account, or add that user again.", t.User, timeToken(*t.Since)), Label: "Go to Users", Href: "/users"})
		case ToolCert:
			add(Attention{K: "bad", T: fmt.Sprintf("The tool at %s has not reconnected since the certificate was replaced", t.Addr),
				D: fmt.Sprintf("It was signed out at %s and will stay away until it is told to trust the new certificate.", timeToken(*t.Since)), Label: "See the certificate", Href: "/security"})
		case ToolSignedOut:
			add(Attention{K: "bad", T: fmt.Sprintf("The tool at %s is not signed in any more", t.Addr),
				D: fmt.Sprintf("Its requests have been refused since %s because its sign-in ended. A tool normally signs in again by itself; if this one does not, restart its collection.", timeToken(*t.Since)), Label: "See what it asks for", Href: "/requests?show=denied"})
		case ToolDenied:
			add(Attention{K: "bad", T: fmt.Sprintf("The tool at %s is being refused when it signs in", t.Addr),
				D: fmt.Sprintf("Its last sign-in was refused at %s. Check the username and password it uses.", timeToken(*t.Since)), Label: "Go to Users", Href: "/users"})
		}
	}
	if u := counts["unknown"]; u > 0 {
		it, verb := "them", "are"
		if u == 1 {
			it, verb = "it", "is"
		}
		d := "The tool that asked carried on collecting without " + it + "."
		license := false
		for _, a := range asks {
			if a.St != "unknown" {
				continue
			}
			if strings.HasPrefix(a.Short, "The tool asked for nothing") {
				d = "A tool asked for nothing more after one of them, so it may depend on the answer."
			}
			license = license || a.T == kindLicense
		}
		if license {
			d += " One is the license, which would stop a different tool: the vCenter adapter built into VCF Operations."
		}
		add(Attention{K: "bad", T: fmt.Sprintf("%s of request %s not being answered", plural(u, "kind"), verb), D: d, Label: "See them", Href: "/requests?show=unknown"})
	}
	conds := c.conditionViews()
	for _, cond := range conds {
		if cond.Starts != nil {
			continue
		}
		title := "A fault is switched on"
		if cond.Kind == scenario.CondSlow {
			title = "Slowness is switched on"
		}
		add(Attention{K: "warn", T: title, D: cond.Text, Label: "Turn it off", Act: "condOff", ID: cond.ID})
		v.Faults++
	}
	for _, a := range asks {
		if a.St != "denied" || !strings.HasPrefix(a.Who, "someone else") || !strings.HasPrefix(a.T, "Sign-in") {
			continue
		}
		from := strings.TrimPrefix(a.Who, "someone else, at ")
		aside := ""
		if live > 0 {
			aside = " (not the connected tool)"
		}
		as := ""
		if a.User != "" {
			as = ", as " + a.User
		}
		what := "a wrong password"
		switch a.Short {
		case "The user was removed":
			what = "to sign in as a removed user"
		case "There is no such user":
			what = "to sign in as a user that does not exist"
		}
		title := fmt.Sprintf("Someone else tried %s %s", what, plural(a.N, "time"))
		tail := ""
		if len(tools) == 0 {
			// nothing has signed in yet, so this may well be the person's own tool
			title = fmt.Sprintf("Something tried %s %s", what, plural(a.N, "time"))
			tail = " If it is your tool, check the username and password it uses."
		}
		add(Attention{K: "warn", T: title,
			D: fmt.Sprintf("From %s%s%s, last at %s.%s", from, aside, as, timeToken(a.Last), tail), Label: "See it", Href: "/requests?show=denied"})
	}
	capture := c.captureView()
	if capture.Full {
		since := ""
		if capture.FullSince != nil {
			since = "Since " + timeToken(*capture.FullSince) + ". "
		}
		add(Attention{K: "info", T: "Every request is being captured in full", D: since + "Turn it off when you have what you need.", Label: "Go to Requests", Href: "/requests"})
	}
	v.Connect.Cert = "made"
	if cert := c.certificateView(); cert != nil {
		v.Connect.Cert, v.Connect.Issuer = cert.Kind, cert.Issuer
		if cert.Kind == "own" && cert.SelfSigned {
			v.Connect.Issuer = ""
		}
		renew, href := "Renew it", "/security/new-certificate"
		if cert.Kind == "managed" {
			renew, href = "See the certificate", "/security"
		}
		switch {
		case cert.Days < 0:
			add(Attention{K: "bad", T: "The certificate has run out", D: "Tools refuse to connect until it is replaced.", Label: renew, Href: href})
		case cert.Days < 30:
			add(Attention{K: "warn", T: fmt.Sprintf("The certificate runs out in %s", plural(cert.Days, "day")), D: "Tools will refuse to connect after that.", Label: renew, Href: href})
		}
	}

	// what to give a tool: an account no tool is using, the first one else
	snapshot := c.auth.Snapshot()
	v.Connect.AnyLogin = snapshot.AnyLogin
	users := snapshot.Users()
	sort.SliceStable(users, func(i, j int) bool { return users[i].Name == DefaultUsername && users[j].Name != DefaultUsername })
	inUse := map[string]string{}
	for _, t := range tools {
		if t.Connected() {
			inUse[t.User] = t.Addr
		}
	}
	if len(users) > 0 {
		pick := users[0]
		for _, u := range users {
			if _, busy := inUse[u.Name]; !busy {
				pick = u
				break
			}
		}
		v.Connect.Username = pick.Name
		switch {
		case snapshot.AnyLogin:
			v.Connect.Password = "any"
		case inUse[pick.Name] != "":
			v.Connect.Password, v.Connect.InUseBy = "inuse", inUse[pick.Name]
		case pick.Origin != "":
			v.Connect.Password = pick.Origin
		default:
			v.Connect.Password = "installer"
		}
	} else if snapshot.AnyLogin {
		v.Connect.Password = "any"
	}

	v.Unanswered, v.Capturing = counts["unknown"], capture.Full
	v.HistoryCount = len(c.timeline)
	for i := len(c.timeline) - 1; i >= 0 && len(v.Recent) < 3; i-- {
		v.Recent = append(v.Recent, c.timeline[i])
	}
	if v.Recent == nil {
		v.Recent = []TimelineEntry{}
	}
	return v
}

// History returns everything that was changed, newest first.
func (c *Controller) History() []TimelineEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]TimelineEntry, len(c.timeline))
	for i, t := range c.timeline {
		out[len(c.timeline)-1-i] = t
	}
	return out
}

// CheckAddress tries the address tools are given, from the simulator's own
// machine, and says in a sentence what happened. ok is true only when the
// simulator answered there.
func (c *Controller) CheckAddress() (ok bool, text string) {
	address := c.Address()
	u, err := url.Parse(address)
	if err != nil || u.Host == "" {
		return false, "The address is not one the simulator can try. Whoever installed it has to correct the address setting."
	}
	hostport := u.Host
	if u.Port() == "" {
		hostport = net.JoinHostPort(u.Hostname(), "443")
	}
	answered, reached, ours := c.probe(hostport)
	switch {
	case answered && ours:
		return true, "Yes. The simulator answered at this address a moment ago."
	case answered:
		return false, "Something answered at this address, but it showed a different certificate from this simulator's. The address may point at another machine, or at something in front of the simulator that has its own certificate."
	case reached && c.paused.Load():
		return false, "The address is right, but the simulator is paused, so it refused the connection."
	case reached:
		return false, "The address reached the simulator, but it did not answer. Try again in a moment."
	}
	// not reachable by its public name: is the simulator answering at all?
	if len(c.opts.APIAddrs) > 0 {
		_, port, _ := net.SplitHostPort(c.opts.APIAddrs[0])
		if local, localReached, _ := c.probe(net.JoinHostPort("127.0.0.1", port)); local || localReached {
			return false, fmt.Sprintf("The simulator is running, but it could not reach itself at %s. Check that the name is right. A tool on another machine may still reach it if that machine knows the name.", address)
		}
	}
	return false, "The simulator did not answer at this address, even from its own machine."
}

// probe asks hostport for a small document the simulated vCenter always
// serves. answered: it was served. reached: a connection was made. ours:
// the certificate shown is this simulator's.
func (c *Controller) probe(hostport string) (answered, reached, ours bool) {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	// The check is of the simulator's own listener, whose certificate is
	// usually self-signed: it is compared with the one in use instead of
	// being verified against the system's authorities.
	conn, err := tls.DialWithDialer(dialer, "tcp", hostport, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // see above
	if err != nil {
		return false, false, false
	}
	defer conn.Close()
	reached = true
	if c.opts.Certificate != nil {
		if peers := conn.ConnectionState().PeerCertificates; len(peers) > 0 {
			sum := sha256.Sum256(peers[0].Raw)
			ours = hex.EncodeToString(sum[:]) == c.opts.Certificate.Info().Fingerprint
		}
	} else {
		ours = true
	}
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	host, _, _ := net.SplitHostPort(hostport)
	fmt.Fprintf(conn, "GET /sdk/vimServiceVersions.xml HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\nConnection: close\r\n\r\n", host, checkAgent)
	line, err := bufio.NewReader(conn).ReadString('\n')
	return err == nil && strings.Contains(line, " 200"), reached, ours
}

// ---------------------------------------------------------------------------
// Clusters

// ClusterRow is one cluster in the list.
type ClusterRow struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	DC      string `json:"dc"`
	Hosts   int    `json:"hosts"`
	VMs     int    `json:"vms"`
	VSAN    string `json:"vsan"`
	Version string `json:"version"`
}

// ClustersView is the Clusters screen.
type ClustersView struct {
	Rows        []ClusterRow `json:"rows"`
	Datacenters []string     `json:"datacenters"`
	Hosts       int          `json:"hosts"`
	VMs         int          `json:"vms"`
}

// Clusters lists the clusters.
func (c *Controller) Clusters() ClustersView {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.applied()
	v := ClustersView{Rows: []ClusterRow{}, Datacenters: []string{}}
	for _, dc := range r.topo.Datacenters {
		v.Datacenters = append(v.Datacenters, dc.Name)
	}
	for _, cl := range r.topo.Clusters() {
		label, mixed := versionLabel(cl)
		if mixed {
			label = "Mixed: " + label
		}
		v.Rows = append(v.Rows, ClusterRow{Key: cl.Key, Name: cl.Name, DC: cl.Datacenter.Name, Hosts: len(cl.Hosts), VMs: len(cl.VMs),
			VSAN: vsanName(cl.Spec.Architecture), Version: label})
		v.Hosts += len(cl.Hosts)
		v.VMs += len(cl.VMs)
	}
	return v
}

// HostRow is one host of a cluster.
type HostRow struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	VMs     int    `json:"vms"`
	Disks   string `json:"disks"`
	// ID is the internal id tools use for the host.
	ID string `json:"id"`
}

// ClusterStorage is one datastore a cluster uses.
type ClusterStorage struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// ClusterDetail is one cluster's page.
type ClusterDetail struct {
	Key     string           `json:"key"`
	Name    string           `json:"name"`
	DC      string           `json:"dc"`
	VSAN    string           `json:"vsan"`
	Version string           `json:"version"`
	VMs     int              `json:"vms"`
	Hosts   []HostRow        `json:"hosts"`
	Local   int              `json:"local"` // local disks per host
	Storage []ClusterStorage `json:"storage"`
	// ID and VsanID are the internal ids tools use for the cluster.
	ID     string `json:"id"`
	VsanID string `json:"vsanId"`
	// Only: this is the last cluster, which cannot be removed. CanShrink:
	// it has more hosts than vSAN's minimum, so some can be removed.
	Only      bool `json:"only"`
	CanShrink bool `json:"canShrink"`
	CanGrow   bool `json:"canGrow"`
}

// Cluster returns one cluster's page.
func (c *Controller) Cluster(key string) (*ClusterDetail, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.applied()
	cl := c.findCluster(key)
	if cl == nil {
		return nil, noCluster(key)
	}
	label, mixed := versionLabel(cl)
	if mixed {
		label = "Mixed: " + label
	}
	v := &ClusterDetail{Key: cl.Key, Name: cl.Name, DC: cl.Datacenter.Name, VSAN: vsanName(cl.Spec.Architecture), Version: label, VMs: len(cl.VMs),
		Hosts: []HostRow{}, Storage: []ClusterStorage{}, Local: scenario.Int(cl.Spec.Host.LocalDevices), VsanID: cl.VsanUUID,
		Only: len(r.topo.Clusters()) == 1, CanShrink: len(cl.Hosts) > scenario.MinHosts, CanGrow: len(cl.Hosts) < maxHosts}
	var refs []string
	if cr := r.sim.Cluster(key); cr != nil {
		v.ID = cr.Ref.Value
		for _, h := range cr.Hosts {
			refs = append(refs, h.Value)
		}
	}
	vmsOn := map[int]int{}
	for _, vm := range cl.VMs {
		vmsOn[vm.Host]++
	}
	for _, h := range cl.Hosts {
		claimed, local := 0, 0
		for _, d := range h.Devices {
			if d.Claimed {
				claimed++
			} else {
				local++
			}
		}
		row := HostRow{Name: shortHost(h.Name), Version: "ESX " + scenario.VersionLabel(h.Version), VMs: vmsOn[h.Index], Disks: fmt.Sprintf("%d vSAN, %d local", claimed, local)}
		if h.Index < len(refs) {
			row.ID = refs[h.Index]
		}
		v.Hosts = append(v.Hosts, row)
	}
	for _, s := range stores(r.sc) {
		uses := false
		for _, k := range s.Clusters {
			uses = uses || k == key
		}
		if !uses {
			continue
		}
		detail := fmt.Sprintf("%s, %s", kindName(s), sizeText(s.Capacity))
		if s.All {
			detail += ", shared with every cluster in " + s.DC
		} else if len(s.Clusters) > 1 {
			detail += ", shared with " + plural(len(s.Clusters)-1, "other cluster")
		}
		v.Storage = append(v.Storage, ClusterStorage{ID: s.ID, Name: s.Name, Detail: detail})
	}
	return v, nil
}
