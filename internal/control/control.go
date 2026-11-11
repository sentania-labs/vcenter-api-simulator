// Package control is the simulator's live control plane: it owns the applied
// scenario, the running simulated vCenter, the operator's pending draft and
// the request log, and applies a draft by building a new vCenter next to the
// old one and swapping them at a five-minute bucket boundary. The admin API
// (package admin) is a thin JSON layer over it.
//
// Rules the console relies on:
//
//   - Nothing reaches the API by accident. Every edit goes to the draft;
//     Apply is the only way a draft becomes the served topology.
//   - A draft applies at the next five-minute bucket boundary unless the
//     operator asks for "now".
//   - Identities (registry serials) survive an apply for every entity the
//     draft keeps. Regenerate retires a cluster's identities;
//     a new seed replaces them all.
//   - A telemetry-only change (latency, faults) swaps the knobs in place and
//     leaves the running vCenter, its sessions and its history alone.
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/activity"
	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/credentials"
	"github.com/sentania/vcenter-api-simulator/internal/perf"
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
	Credentials    map[string]string
	SessionTimeout time.Duration
	AnyLogin       bool
	Certificate    *serve.Certificate
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	Version, Commit string
	// Endpoint is the host name the API is advertised under.
	Endpoint string
	// APIAddrs and AdminAddr are the listeners, for display.
	APIAddrs  []string
	AdminAddr string
}

// running is one built vCenter.
type running struct {
	version int
	at      time.Time
	sc      *scenario.Scenario
	topo    *topology.Topology
	sim     *sim.Simulator
}

// Draft is the operator's pending change.
type Draft struct {
	Scenario    *scenario.Scenario
	Regenerate  []string // cluster keys whose identities are replaced
	Credentials *credentials.Snapshot
	Scope       string
	BasedOn     int
	Origin      string // edit | generator | preset:<name>
	Discarded   string // what a generated draft replaced
	Created     time.Time
}

// Scheduled is a draft waiting for its bucket boundary.
type Scheduled struct {
	Draft *Draft
	At    time.Time
	timer *time.Timer
}

// TimelineEntry is one applied change.
type TimelineEntry struct {
	At         time.Time `json:"at"`
	Bucket     time.Time `json:"bucket"`
	Version    int       `json:"version"`
	Area       string    `json:"area"` // Topology | Telemetry | Service
	Text       string    `json:"text"`
	History    string    `json:"history"`
	Identities string    `json:"identities"`
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

	mu        sync.Mutex // guards everything below
	reg       *registry.Registry
	base      *scenario.Scenario
	scope     string
	draft     *Draft
	scheduled *Scheduled
	timeline  []TimelineEntry
	started   time.Time
	pausedAt  time.Time // when the API last started or stopped serving
	buildMs   int64
	lastErr   string
	lastErrAt time.Time
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
	now := opts.Now
	c := &Controller{opts: opts, cat: cat, now: func() time.Time { return now().UTC() }, base: opts.Scenario, scope: ScopeAll}
	c.started = c.now()
	if err := c.loadRuntimeSettings(); err != nil {
		return nil, fmt.Errorf("runtime settings: %w", err)
	}
	c.Log = activity.New(activity.DefaultSize, c.started)
	c.Log.EmptyKind = func(f string) bool { fam, ok := cat.Families[f]; return ok && fam.Behaviour != catalog.Data }

	sc := clone(opts.Scenario)
	version := 1
	st, err := loadState(c.statePath())
	if err != nil {
		return nil, err
	}
	if st != nil {
		c.timeline = st.Timeline
		if st.Scope != "" {
			c.scope = st.Scope
		}
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
			sc, version = restored, st.Version
		} else {
			// the scenario file changed since the console last applied: the
			// file wins, and the version moves on
			version = st.Version + 1
			c.timeline = append(c.timeline, TimelineEntry{At: c.now(), Bucket: bucket(c.now()), Version: version, Area: "Service",
				Text: "Scenario file changed; started from the file", History: "preserved", Identities: "per registry"})
		}
	}
	baseCredentials, err := c.credentials(sc)
	if err != nil {
		return nil, err
	}
	var readonly []string
	for _, user := range sc.Auth.Users {
		if user.PasswordEnv == "VCSIM_READONLY_PASSWORD" {
			readonly = append(readonly, user.Name)
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
	if st == nil {
		c.timeline = append(c.timeline, TimelineEntry{At: c.now(), Bucket: bucket(c.now()), Version: version, Area: "Topology",
			Text: fmt.Sprintf("Topology v%d built: %s", version, countsLine(r.topo)), History: "seeded", Identities: "new registry"})
	}
	_ = c.persist()
	return c, nil
}

func (c *Controller) statePath() string { return filepath.Join(c.opts.DataDir, "console.json") }

func countsLine(t *topology.Topology) string {
	cl, h, v, d := t.Counts()
	return fmt.Sprintf("%d clusters, %d hosts, %d VMs, %d devices", cl, h, v, d)
}

// bucket is the five-minute bucket a time falls in.
func bucket(t time.Time) time.Time { return t.UTC().Truncate(perf.Interval) }

// build generates a topology from sc against reg and starts a simulator.
func (c *Controller) build(sc *scenario.Scenario, reg *registry.Registry, version int) (*running, error) {
	start := time.Now()
	topo := topology.Generate(sc, reg)
	if err := validateHostVersionTargets(sc, topo); err != nil {
		return nil, err
	}
	s, err := sim.New(sim.Options{
		Topology: topo, Registry: reg, Authenticate: c.auth.Authenticate, UserRole: c.auth.Role, SessionTimeout: c.opts.SessionTimeout,
		Faults: sc.Faults, Latency: sc.Latency, Now: c.now,
	})
	if err != nil {
		return nil, err
	}
	c.buildMs = time.Since(start).Milliseconds()
	return &running{version: version, at: c.now(), sc: sc, topo: topo, sim: s}, nil
}

const DefaultUsername = "administrator@vsphere.local"
const DefaultPassword = "VMware123!VMware123!"

// credentials applies the explicit any-login opt-out and configured users
// before falling back to the published built-in administrator account.
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
	if len(creds) == 0 {
		creds[DefaultUsername] = DefaultPassword
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

// Close stops the running vCenter and any scheduled apply.
func (c *Controller) Close() {
	c.mu.Lock()
	if c.scheduled != nil && c.scheduled.timer != nil {
		c.scheduled.timer.Stop()
	}
	c.mu.Unlock()
	if r := c.cur.Load(); r != nil {
		r.sim.Close()
	}
}

// ---------------------------------------------------------------------------
// the API plane

// ServeHTTP is the API listeners' handler: it routes to the running vCenter,
// records each request, and refuses the connection while paused.
func (c *Controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cur := c.cur.Load()
	surface := ""
	switch {
	case strings.HasPrefix(r.URL.Path, "/vsanHealth"):
		surface = "vsanHealth"
	case strings.HasPrefix(r.URL.Path, "/sdk"):
		surface = "sdk"
	}
	if surface == "" {
		cur.sim.Handler.ServeHTTP(w, r)
		return
	}
	start := c.now()
	client, _, _ := net.SplitHostPort(r.RemoteAddr)
	call := activity.Call{Time: start, Surface: surface, Client: client}
	var body []byte
	if r.Method == http.MethodPost {
		var err error
		if body, err = io.ReadAll(io.LimitReader(r.Body, 32<<20)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		call.Method = activity.SDKMethod(body)
	}
	if call.Method == "" {
		call.Method = r.Method + " " + strings.TrimPrefix(r.URL.Path, "/")
	}
	env := cur.sim.Env
	if surface == "vsanHealth" {
		call.Family, call.Cluster = env.Target(body)
		safeBody := querylog.Redact(body)
		call.Body = safeBody[:min(len(safeBody), activity.MaxBody)]
	}
	if c.paused.Load() {
		call.Result, call.Status = activity.ResultRefused, 0
		c.Log.Add(call)
		panic(http.ErrAbortHandler) // drop the connection: the pack's cycle fails instead of reading empty data
	}
	cap := activity.NewCapture(w, call.Method == "VsanPerfQueryPerf")
	cur.sim.Handler.ServeHTTP(cap, r)
	call.DurationMs = float64(c.now().Sub(start)) / float64(time.Millisecond)
	call.Bytes, call.Status = cap.Bytes, cap.Status
	call.Entities, call.Empty = cap.Entities, cap.Empty
	call.Result = activity.ResultOK
	typ, msg, faulted := cap.Fault()
	if faulted || cap.Status >= 400 {
		// A server fault can echo input. Do not keep its text when the request
		// contained credentials, a session value, or an unparseable body.
		if len(body) > 0 && querylog.Redact(body) != string(body) {
			msg = ""
		}
		call.Result, call.Fault, call.Message = activity.ResultFault, typ, msg
		if typ == "NotAuthenticated" || typ == "InvalidLogin" {
			call.Result = activity.ResultDenied
		}
	}
	if surface == "vsanHealth" && call.Cluster != "" {
		var inj *scenario.Fault
		if call.Family != "" && call.Method == "VsanPerfQueryPerf" {
			inj = env.Knobs.FamilyFault(call.Family, call.Cluster)
		} else {
			inj = env.Knobs.MethodFault(call.Method, call.Cluster)
		}
		// only the fault the rule injects counts as injected: an unauthenticated
		// request is refused before any rule is consulted
		if inj != nil && (inj.Fault == scenario.FaultTimeout || (faulted && injectedType(inj.Fault) == typ)) {
			call.Result = activity.ResultInjected
			if call.Fault == "" {
				call.Fault = inj.Fault
			}
		}
	}
	c.Log.Add(call)
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

// Strip returns a copy without the parts the console never edits or shows:
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

// normal is a canonical form for comparing scenarios: the JSON of the
// stripped copy.
func normal(sc *scenario.Scenario, parts ...string) string {
	s := Strip(sc)
	if len(parts) == 0 {
		b, _ := json.Marshal(s)
		return string(b)
	}
	var out []string
	for _, p := range parts {
		var v any
		switch p {
		case "topology":
			v = struct {
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
			}{s.Seed, s.TelemetrySeed, s.Datacenters, s.Defaults, s.Vary, s.Clusters, s.Removed, s.VCenter, s.ESX, s.HostVersions, s.ForceNewerHosts}
		case "telemetry":
			v = struct {
				F []scenario.Fault
				L scenario.Latency
			}{s.Faults, s.Latency}
		}
		b, _ := json.Marshal(v)
		out = append(out, string(b))
	}
	return strings.Join(out, "|")
}

// ---------------------------------------------------------------------------
// persistence

type persisted struct {
	Version   int             `json:"version"`
	AppliedAt time.Time       `json:"appliedAt"`
	Source    string          `json:"source"`
	Scenario  json.RawMessage `json:"scenario"`
	Scope     string          `json:"scope"`
	Timeline  []TimelineEntry `json:"timeline"`
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

// persist writes the applied scenario and the timeline atomically. Called
// with c.mu held or from New.
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
	out, err := json.MarshalIndent(persisted{Version: r.version, AppliedAt: r.at, Source: c.opts.SourceHash,
		Scenario: b, Scope: c.scope, Timeline: tl}, "", " ")
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

// injectedType is the SOAP fault type a rule's fault kind produces.
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
