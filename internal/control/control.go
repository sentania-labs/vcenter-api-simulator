// Package control is the simulator's live control plane: it owns the served
// scenario, the running simulated vCenter, the request log, the list of
// connected tools and the changes scheduled for later. The admin API
// (package admin) is a thin JSON layer over it.
//
// Rules the console relies on:
//
//   - A change takes effect when it is asked for, one change at a time, or
//     at a time later the same day. There is no shared draft and no lock:
//     a scheduled change never stops another change from being made.
//   - Identities (registry serials) survive a change for every entity the
//     change keeps. Only "Replace everything" starts a new registry.
//   - Slowness and faults (conditions) swap in place and leave the running
//     vCenter, its sessions and its history alone.
//   - Every screen reads what is true from here: whether a tool is
//     connected, as whom, and if not, since when and why.
//   - The admin plane is separate from the API plane and is never exposed on
//     the API listeners.
package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/activity"
	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/credentials"
	"github.com/sentania/vcenter-api-simulator/internal/querylog"
	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/serve"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// Options configure a Controller.
type Options struct {
	// Scenario is the baseline: the file the process was started with (or
	// the built-in one). Auth and TLS always come from it.
	Scenario *scenario.Scenario
	// SourceHash identifies the baseline so a persisted console state is
	// ignored once the operator edits the scenario file.
	SourceHash string
	DataDir    string
	// Regenerate discards the identity registry at start.
	Regenerate bool
	// Credentials are extra logins (flag or environment) added to the
	// scenario's users.
	Credentials map[string]string
	// CredentialRole is "administrator" (default) or "read-only" for every
	// user in Credentials.
	CredentialRole string
	SessionTimeout time.Duration
	AnyLogin       bool
	Certificate    *serve.Certificate
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	Version, Commit string
	// Endpoint is the host name the API is advertised under.
	Endpoint string
	// PublicURL, when set, is the address tools should use, as one string
	// (a deployment behind an ingress knows it; the simulator does not).
	PublicURL string
	// ReportURL is where the console sends people to report a request the
	// simulator cannot answer.
	ReportURL string
	// APIAddrs and AdminAddr are the listeners.
	APIAddrs  []string
	AdminAddr string
}

// DefaultReportURL is the simulator project's "new issue" page.
const DefaultReportURL = "https://github.com/sentania-labs/vcenter-api-simulator/issues/new"

// DefaultUsername is the first vCenter account. The console's first-run
// set-up gives it a password; nothing accepts it before then.
const DefaultUsername = "administrator@vsphere.local"

// running is one built vCenter.
type running struct {
	version int
	at      time.Time
	sc      *scenario.Scenario
	topo    *topology.Topology
	sim     *sim.Simulator
}

// TimelineEntry is one line of the history: something that was changed.
type TimelineEntry struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// Controller is the control plane.
type Controller struct {
	opts Options
	cat  *catalog.Catalog
	now  func() time.Time

	cur    atomic.Pointer[running]
	paused atomic.Bool
	Log    *activity.Log
	auth   *credentials.Store
	tools  *toolbook

	// Capture is the request capture, when the process has one. The
	// Requests screen and Home read it.
	Capture *querylog.Capture

	// ConsolePassword changes the console's own password; the admin plane
	// sets it. session is the caller's, which stays signed in.
	ConsolePassword func(current, next, session string) error
	// ConsoleManaged reports whether the console password comes from the
	// deployment, so it cannot be changed here.
	ConsoleManaged func() bool

	mu       sync.Mutex // guards everything below
	reg      *registry.Registry
	base     *scenario.Scenario
	timeline []TimelineEntry
	started  time.Time
	pausedAt time.Time // when the API last stopped serving
	buildMs  int64
	seq      int // ids of scheduled changes and conditions
	sched    []*Scheduled
	failure  *Failure
	unsaved  string // why the state file could not be written, while it cannot
	ended    []EndedCondition
	expiry   *time.Timer

	signMu    sync.Mutex // guards the two below; taken after mu when both are held, never the other way round
	signins   map[string]time.Time
	signinsAt time.Time // when signins was last written

	drainMu  sync.Mutex // guards the two below
	draining map[*sim.Simulator]bool
	closed   bool
}

// New builds the simulated vCenter and the control plane around it.
func New(opts Options) (*Controller, error) {
	cat, err := catalog.Load()
	if err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ReportURL == "" {
		opts.ReportURL = DefaultReportURL
	}
	now := opts.Now
	c := &Controller{opts: opts, cat: cat, now: func() time.Time { return now().UTC() }, base: opts.Scenario, signins: map[string]time.Time{}}
	c.started = c.now()
	c.tools = newToolbook()
	if err := c.loadRuntimeSettings(); err != nil {
		return nil, fmt.Errorf("runtime settings: %w", err)
	}
	c.loadSignins()
	c.Log = activity.New(activity.DefaultSize, c.started)

	sc := clone(opts.Scenario)
	version := 1
	st, err := loadState(c.statePath())
	if err != nil {
		return nil, err
	}
	fromFile := true
	if st != nil {
		c.timeline = st.Timeline
		c.seq = st.Seq
		if st.Source == opts.SourceHash && st.Scenario != nil {
			restored, err := decodeScenarioJSON(st.Scenario, opts.Scenario)
			var removed int
			if err == nil {
				removed, err = reconcileStorage(restored, nil)
			}
			if err == nil {
				err = restored.Validate()
			}
			if err != nil {
				return nil, fmt.Errorf("control: console state %s: %w (delete it to start from the scenario file)", c.statePath(), err)
			}
			if removed > 0 {
				log.Printf("control: recovered saved console state: removed %d dangling storage references", removed)
			}
			sc, version, fromFile = restored, st.Version, false
		} else {
			// the scenario file changed since the console last changed
			// anything: the file wins
			version = st.Version + 1
		}
	}
	c.lift(sc)
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	baseCredentials, err := c.credentials(sc)
	if err != nil {
		return nil, err
	}
	var readonly []string
	for _, user := range sc.Auth.Users {
		if user.Role == "read-only" {
			readonly = append(readonly, user.Name)
		}
	}
	if opts.CredentialRole == "read-only" {
		for name := range opts.Credentials {
			readonly = append(readonly, name)
		}
	}
	c.auth, err = credentials.New(opts.DataDir, opts.SourceHash, baseCredentials, opts.AnyLogin || sc.Auth.AnyLogin, readonly)
	if err != nil {
		return nil, err
	}
	c.reg, err = registry.Open(filepath.Join(opts.DataDir, "registry.json"), sc.Seed, opts.Regenerate)
	if err != nil {
		return nil, err
	}
	r, err := c.build(sc, c.reg, version)
	if err != nil {
		return nil, err
	}
	if err := c.reg.Save(); err != nil {
		r.sim.Close()
		return nil, err
	}
	c.cur.Store(r)
	c.syncConditions(r)
	cl, h, v, _ := r.topo.Counts()
	started := fmt.Sprintf("Simulator started with %s, %s and %s.", plural(cl, "cluster"), plural(h, "host"), plural(v, "VM"))
	if st != nil && fromFile {
		started = "The scenario file was changed, so the simulator started again from the file. It has " + plural(cl, "cluster") + ", " + plural(h, "host") + " and " + plural(v, "VM") + "."
	}
	c.timeline = append(c.timeline, TimelineEntry{At: c.now(), Text: started})
	if st != nil {
		for _, s := range st.Scheduled {
			c.timeline = append(c.timeline, TimelineEntry{At: c.now(), Text: fmt.Sprintf("Not done: the change scheduled for %s (%s) was dropped because the simulator restarted.", timeToken(s.At), lower1(s.Text))})
		}
	}
	c.save()
	c.armExpiry()
	return c, nil
}

func (c *Controller) statePath() string { return filepath.Join(c.opts.DataDir, "console.json") }

// build generates a topology from sc against reg and starts a simulator.
func (c *Controller) build(sc *scenario.Scenario, reg *registry.Registry, version int) (*running, error) {
	start := time.Now()
	topo := topology.Generate(sc, reg)
	if err := validateHostVersionTargets(sc, topo); err != nil {
		return nil, err
	}
	s, err := sim.New(sim.Options{
		Topology: topo, Registry: reg, Authenticate: c.auth.Authenticate, UserRole: c.auth.Role, SessionTimeout: c.opts.SessionTimeout,
		Faults: sc.Faults, Latency: sc.Latency, Conditions: sc.Conditions, Now: c.now, Certificate: c.opts.Certificate,
	})
	if err != nil {
		return nil, err
	}
	c.buildMs = time.Since(start).Milliseconds()
	return &running{version: version, at: c.now(), sc: sc, topo: topo, sim: s}, nil
}

// credentials are the accounts the deployment supplies: the scenario's
// users and the flag or environment user. With none, and without any-login,
// there is no account until the console's first-run set-up makes one: the
// simulator has no published default password.
func (c *Controller) credentials(sc *scenario.Scenario) (map[string]string, error) {
	if c.opts.AnyLogin || sc.Auth.AnyLogin {
		return map[string]string{}, nil
	}
	creds, err := sc.Credentials()
	if err != nil {
		return nil, err
	}
	for name, password := range c.opts.Credentials {
		creds[name] = password
	}
	return creds, nil
}

func validateHostVersionTargets(sc *scenario.Scenario, topo *topology.Topology) error {
	hosts := map[string]bool{}
	for _, cluster := range topo.Clusters() {
		for _, host := range cluster.Hosts {
			hosts[host.Key] = true
		}
	}
	for key := range sc.HostVersions {
		if !hosts[key] {
			return fmt.Errorf("host version override %q does not match a host", key)
		}
	}
	return nil
}

// dropVanishedHostVersions removes per-host versions of hosts sc no longer
// generates (a removed host or cluster).
func dropVanishedHostVersions(sc *scenario.Scenario) {
	if len(sc.HostVersions) == 0 {
		return
	}
	hosts := map[string]bool{}
	for _, key := range sc.ClusterKeys() {
		gone := sc.RemovedHosts(key)
		for n := 1; n <= sc.HostNumbers(key); n++ {
			if !gone[n] {
				hosts[fmt.Sprintf("%s/esx%02d", key, n)] = true
			}
		}
	}
	for key := range sc.HostVersions {
		if !hosts[key] {
			delete(sc.HostVersions, key)
		}
	}
	if len(sc.HostVersions) == 0 {
		sc.HostVersions = nil
	}
}

// Close stops the running vCenter and every scheduled change.
func (c *Controller) Close() {
	c.mu.Lock()
	for _, s := range c.sched {
		if s.timer != nil {
			s.timer.Stop()
		}
	}
	if c.expiry != nil {
		c.expiry.Stop()
	}
	c.mu.Unlock()
	c.signMu.Lock()
	c.saveSignins(true)
	c.signMu.Unlock()
	if r := c.cur.Load(); r != nil {
		r.sim.Close()
	}
	c.drainMu.Lock()
	c.closed = true
	left := c.draining
	c.draining = nil
	c.drainMu.Unlock()
	for s := range left {
		s.Close()
	}
}

// drain lets a replaced vCenter keep answering the requests that were in
// flight at the swap for drainGrace, then closes it. Close closes whatever
// is still draining, so a replaced vCenter's temporary files never outlive
// the controller.
func (c *Controller) drain(s *sim.Simulator) {
	c.drainMu.Lock()
	if c.closed {
		c.drainMu.Unlock()
		s.Close()
		return
	}
	if c.draining == nil {
		c.draining = map[*sim.Simulator]bool{}
	}
	c.draining[s] = true
	c.drainMu.Unlock()
	time.AfterFunc(drainGrace, func() {
		c.drainMu.Lock()
		mine := c.draining[s]
		delete(c.draining, s)
		c.drainMu.Unlock()
		if mine {
			s.Close()
		}
	})
}

// ---------------------------------------------------------------------------
// the API plane

// checkAgent is what the console's own "Check the address works" calls
// itself. Its requests are answered and not recorded: it is not a tool.
const checkAgent = "vcsimulator-console-check"

func surfaceOf(path string) string {
	switch {
	case strings.HasPrefix(path, "/vsanHealth"):
		return activity.SurfaceVsan
	case strings.HasPrefix(path, "/sdk"):
		return activity.SurfaceSDK
	case path == "/api" || strings.HasPrefix(path, "/api/") || path == "/rest" || strings.HasPrefix(path, "/rest/"):
		return activity.SurfaceREST
	}
	return activity.SurfaceOther
}

// ServeHTTP is the API listeners' handler: it routes to the running vCenter,
// records each request with the reason it was answered as it was, keeps the
// list of connected tools, and refuses every connection while paused.
func (c *Controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cur := c.cur.Load()
	if r.UserAgent() == checkAgent {
		if c.paused.Load() {
			panic(http.ErrAbortHandler)
		}
		cur.sim.Handler.ServeHTTP(w, r)
		return
	}
	start := c.now()
	client, _, _ := net.SplitHostPort(r.RemoteAddr)
	surface := surfaceOf(r.URL.Path)
	call := activity.Call{Time: start, Surface: surface, Client: client, Agent: r.UserAgent()}
	soapSurface := surface == activity.SurfaceSDK || surface == activity.SurfaceVsan
	var body []byte
	if r.Method == http.MethodPost && r.Body != nil {
		var err error
		if body, err = io.ReadAll(io.LimitReader(r.Body, 32<<20)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if soapSurface {
			call.Method = activity.SDKMethod(body)
		}
	}
	soapOp := call.Method
	if call.Method == "" {
		call.Method = r.Method + " " + r.URL.Path
	}
	call.User = cur.sim.SessionUser(r)
	signIn := false
	switch {
	case soapOp == "Login":
		call.User, signIn = activity.LoginUser(body), true
	case surface == activity.SurfaceREST && r.URL.Path == "/api/session" && r.Method == http.MethodPost:
		call.User, _, _ = r.BasicAuth()
		signIn = true
	}
	env := cur.sim.Env
	if surface == activity.SurfaceVsan {
		call.Family, call.Cluster = env.Target(body)
	}
	if soapSurface && len(body) > 0 {
		safeBody := querylog.Redact(body)
		call.Body = safeBody[:min(len(safeBody), activity.MaxBody)]
	}
	if c.paused.Load() {
		call.Reason = activity.ReasonPaused
		c.record(r, call, false)
		panic(http.ErrAbortHandler) // drop the connection: a collection fails instead of reading empty data
	}
	capture := activity.NewCapture(w, soapOp == "VsanPerfQueryPerf")
	cur.sim.Handler.ServeHTTP(capture, r)
	call.DurationMs = float64(c.now().Sub(start)) / float64(time.Millisecond)
	call.Bytes, call.Status = capture.Bytes, capture.Status
	call.Entities, call.Empty = capture.Entities, capture.Empty
	typ, msg, faulted := capture.Fault()
	if faulted || capture.Status >= 400 {
		// A server fault can echo input. Do not keep its text when the request
		// contained credentials, a session value, or an unparseable body.
		if len(body) > 0 && querylog.Redact(body) != string(body) {
			msg = ""
		}
		call.Fault, call.Message = typ, msg
	}
	facts := activity.Facts{Surface: surface, HTTPMethod: r.Method, Path: r.URL.Path, Method: soapOp, Status: capture.Status,
		FaultType: typ, Faulted: faulted, ObjectType: activity.ObjectType(body), Entities: capture.Entities, Empty: capture.Empty}
	if surface == activity.SurfaceVsan && call.Cluster != "" {
		// only the fault a rule injects counts as switched on: a request that
		// is not signed in is refused before any rule is consulted
		switchedOn := func(inj *scenario.Fault) bool {
			if inj == nil || !(inj.Fault == scenario.FaultTimeout || (faulted && injectedType(inj.Fault) == typ)) {
				return false
			}
			facts.Injected, call.Cause = true, inj.Cause
			if call.Fault == "" {
				call.Fault = inj.Fault
			}
			return true
		}
		if soapOp == "VsanPerfQueryPerf" {
			// one query can name several kinds; the fault may be on any of them
			for _, family := range env.Families(body) {
				if switchedOn(env.Knobs.FamilyFault(family, call.Cluster)) {
					call.Family = family
					break
				}
			}
		} else {
			switchedOn(env.Knobs.MethodFault(soapOp, call.Cluster))
		}
		if !facts.Injected && faulted && typ == "NotFound" {
			if id, off := perfOffCause(cur.sc, call.Cluster, start); off {
				facts.PerfOff, call.Cause = true, id
			}
		}
	}
	call.Reason = activity.Classify(facts)
	c.record(r, call, signIn)
}

// record files one finished request everywhere that needs to know of it.
func (c *Controller) record(r *http.Request, call activity.Call, signIn bool) {
	c.Log.Add(call)
	querylog.Note(r, call.Reason == activity.ReasonUnknown, call.Client, call.Agent)
	back := c.tools.note(call, signIn)
	if signIn && call.Reason == activity.ReasonAnswered && call.User != "" {
		c.noteSignin(call.User, call.Time)
	}
	if back != "" {
		// not on the request's own time: a change in progress holds the lock
		go func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.addHistory(fmt.Sprintf("The tool at %s reconnected.", back))
			c.save()
		}()
	}
}

// perfOffCause finds the condition that has the performance service of a
// cluster switched off.
func perfOffCause(sc *scenario.Scenario, cluster string, at time.Time) (int, bool) {
	for _, cond := range sc.Conditions {
		if cond.Kind == scenario.CondPerfDown && cond.Selects(cluster) && cond.ActiveAt(at) {
			return cond.ID, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// scenario <-> JSON

func clone(sc *scenario.Scenario) *scenario.Scenario {
	b, err := json.Marshal(sc)
	if err != nil {
		panic(err)
	}
	var out scenario.Scenario
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return &out
}

// Strip returns a copy without the parts the console never shows or saves:
// the users (passwords) and the certificate paths.
func Strip(sc *scenario.Scenario) *scenario.Scenario {
	out := clone(sc)
	out.Auth, out.TLS = scenario.Auth{}, scenario.TLS{}
	return out
}

// decodeScenarioJSON takes credentials and TLS settings from the baseline.
func decodeScenarioJSON(b []byte, base *scenario.Scenario) (*scenario.Scenario, error) {
	var sc scenario.Scenario
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil {
		return nil, fmt.Errorf("scenario: %w", err)
	}
	sc.Auth, sc.TLS = base.Auth, base.TLS
	return &sc, nil
}

// topologyForm is a canonical form of everything that needs a new vCenter
// when it changes. Faults, latency and conditions are left out: they swap
// in place.
func topologyForm(sc *scenario.Scenario) string {
	s := Strip(sc)
	b, _ := json.Marshal(struct {
		Seed, Tel       string
		DC              []scenario.Datacenter
		Def             scenario.ClusterSpec
		Vary            scenario.Vary
		Cl              []scenario.ClusterOverride
		Rm              []string
		VCenter         scenario.Version
		ESX             scenario.Version
		HostVersions    map[string]scenario.Version
		ForceNewerHosts bool
	}{s.Seed, s.TelemetrySeed, s.Datacenters, s.Defaults, s.Vary, s.Clusters, s.Removed, s.VCenter, s.ESX, s.HostVersions, s.ForceNewerHosts})
	return string(b)
}

// ---------------------------------------------------------------------------
// persistence

type persistedSchedule struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

type persisted struct {
	Version   int                 `json:"version"`
	AppliedAt time.Time           `json:"appliedAt"`
	Source    string              `json:"source"`
	Scenario  json.RawMessage     `json:"scenario"`
	Seq       int                 `json:"seq,omitempty"`
	Scheduled []persistedSchedule `json:"scheduled,omitempty"`
	Timeline  []TimelineEntry     `json:"timeline"`
}

func loadState(path string) (*persisted, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("control: %s: %w", path, err)
	}
	return &p, nil
}

// persist writes the served scenario and the history atomically. Called with
// c.mu held or from New. Scheduled changes are saved as their time and words
// only: they do not survive a restart, and the history says so when one
// happens.
func (c *Controller) persist() error {
	r := c.cur.Load()
	b, err := json.Marshal(Strip(r.sc))
	if err != nil {
		return err
	}
	tl := c.timeline
	if len(tl) > 500 {
		tl = tl[len(tl)-500:]
	}
	var waiting []persistedSchedule
	for _, s := range c.sched {
		waiting = append(waiting, persistedSchedule{At: s.At, Text: s.Text})
	}
	out, err := json.MarshalIndent(persisted{Version: r.version, AppliedAt: r.at, Source: c.opts.SourceHash,
		Scenario: b, Seq: c.seq, Scheduled: waiting, Timeline: tl}, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.opts.DataDir, 0o700); err != nil {
		return err
	}
	tmp := c.statePath() + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.statePath())
}

// injectedType is the SOAP fault type a fault kind produces.
func injectedType(kind string) string {
	switch kind {
	case scenario.FaultInvalidArgument:
		return "InvalidArgument"
	case scenario.FaultNotSupported:
		return "NotSupported"
	case scenario.FaultSystemError:
		return "SystemError"
	}
	return ""
}

// ---------------------------------------------------------------------------
// last sign-in of each user

func (c *Controller) signinsPath() string { return filepath.Join(c.opts.DataDir, "last-signin.json") }

func (c *Controller) loadSignins() {
	b, err := os.ReadFile(c.signinsPath())
	if err != nil {
		return
	}
	var m map[string]time.Time
	if json.Unmarshal(b, &m) == nil && m != nil {
		c.signins = m
	}
}

// noteSignin remembers when a user last signed in. The file is written at
// most once a minute and when the simulator stops.
func (c *Controller) noteSignin(user string, at time.Time) {
	c.signMu.Lock()
	defer c.signMu.Unlock()
	c.signins[user] = at
	c.saveSignins(false)
}

// lastSignin is when a user last signed in.
func (c *Controller) lastSignin(user string) (time.Time, bool) {
	c.signMu.Lock()
	defer c.signMu.Unlock()
	t, ok := c.signins[user]
	return t, ok
}

// saveSignins writes the file; signMu is held.
func (c *Controller) saveSignins(force bool) {
	if !force && c.now().Sub(c.signinsAt) < time.Minute {
		return
	}
	c.signinsAt = c.now()
	b, err := json.Marshal(c.signins)
	if err != nil || os.MkdirAll(c.opts.DataDir, 0o700) != nil {
		return
	}
	tmp := c.signinsPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, c.signinsPath())
	}
}

// ---------------------------------------------------------------------------
// start-up summary

// Banner is the start-up summary, one line per item.
func (c *Controller) Banner(took time.Duration) []string {
	r := c.applied()
	cl, h, v, d := r.topo.Counts()
	out := []string{
		fmt.Sprintf("vCenter %s instanceUuid=%s seed=%q registry=%d ids built in %s", r.topo.VCenterVersion.Version, r.topo.InstanceUUID, r.sc.Seed, c.reg.Len(), took.Round(time.Millisecond)),
		fmt.Sprintf("%d clusters, %d hosts, %d vms, %d devices", cl, h, v, d),
	}
	for _, ref := range r.sim.Clusters {
		k := ref.Cluster
		out = append(out, fmt.Sprintf("%s (%s) vsan uuid=%s hosts=%d vms=%d dedup=%v", k.Key, ref.Ref.Value, k.VsanUUID, len(k.Hosts), len(k.VMs), k.Dedup))
	}
	return out
}

// LoginUsers lists the accepted user names. anyone is true when every user
// name and password is accepted.
func (c *Controller) LoginUsers() (users []string, anyone bool) {
	snapshot := c.auth.Snapshot()
	if snapshot.AnyLogin {
		return nil, true
	}
	for _, user := range snapshot.Users() {
		users = append(users, user.Name)
	}
	sort.Strings(users)
	return users, false
}

// applied is the running vCenter; callers hold c.mu or only read.
func (c *Controller) applied() *running { return c.cur.Load() }

// Invalid wraps a message the operator can fix (a 422, not a 500).
type Invalid struct{ Msg string }

func (e *Invalid) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &Invalid{fmt.Sprintf(format, a...)} }
