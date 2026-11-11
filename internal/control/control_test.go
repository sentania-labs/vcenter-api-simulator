package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/activity"
	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/vsanhealth"
)

func tenClusters(t *testing.T) *scenario.Scenario {
	t.Helper()
	s, err := scenario.Load("../../examples/scenarios/ten-clusters.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s.Latency = scenario.Latency{}
	return s
}

func newCtl(t *testing.T, sc *scenario.Scenario, dir, hash string) *Controller {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	c, err := New(Options{Scenario: sc, SourceHash: hash, DataDir: dir, Endpoint: "sim.test", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// editable returns the applied scenario as the console edits it.
func editable(t *testing.T, c *Controller) *scenario.Scenario {
	t.Helper()
	return clone(Strip(c.applied().sc))
}

func put(t *testing.T, c *Controller, sc *scenario.Scenario) DraftView {
	t.Helper()
	b, err := json.Marshal(Strip(sc))
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.SetDraft(b, "")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func setHosts(sc *scenario.Scenario, key string, n int) {
	for i := range sc.Clusters {
		if sc.Clusters[i].Key == key {
			sc.Clusters[i].Hosts = n
			return
		}
	}
	sc.Clusters = append(sc.Clusters, scenario.ClusterOverride{Key: key, ClusterSpec: scenario.ClusterSpec{Hosts: n}})
}

func nodeUUIDs(c *Controller) map[string]string {
	out := map[string]string{}
	for _, cl := range c.applied().topo.Clusters() {
		for _, h := range cl.Hosts {
			out[h.Key] = h.NodeUUID
		}
	}
	return out
}

// Project's analytic counts must equal what the Performance Service
// enumerates, family by family, on a cluster with and without dedup.
func TestProjectionMatchesEntities(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	topo := c.applied().topo
	proj := Project(c.cat, topo)
	want := map[string]int{}
	for _, cl := range topo.Clusters() {
		for _, fam := range c.cat.DataFamilies() {
			if !cl.Dedup && strings.Contains(fam, "-esa-dedup-") {
				continue
			}
			refs, err := vsanhealth.Entities(c.cat, cl, fam)
			if err != nil {
				t.Fatal(err)
			}
			want[fam] += len(refs)
		}
	}
	for _, f := range proj.Families {
		if f.Behaviour != string(catalog.Data) {
			continue
		}
		if f.Objects != want[f.Family] {
			t.Errorf("%s: projected %d objects, service enumerates %d", f.Family, f.Objects, want[f.Family])
		}
	}
	if proj.Clusters != 10 || proj.All.Kinds != len(c.cat.Families) || proj.All.Calls != proj.All.Kinds*10 {
		t.Errorf("unexpected totals: %+v", proj.All)
	}
	if proj.Dash.Kinds >= proj.All.Kinds || proj.Dash.Entities >= proj.All.Entities {
		t.Errorf("dashboard scope should be smaller: %+v vs %+v", proj.Dash, proj.All)
	}
}

func TestDraftApplyKeepsIdentities(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	before := nodeUUIDs(c)
	oldSim := c.applied().sim

	if v := c.DraftState(); v.Pending.Exists {
		t.Fatal("a fresh controller has a pending draft")
	}
	sc := editable(t, c)
	setHosts(sc, "dc-01/cluster-02", 10)
	if v := put(t, c, sc); !v.Pending.Exists || v.Pending.Topology != 1 {
		t.Fatalf("pending after edit: %+v", v.Pending)
	}
	rv, err := c.Review()
	if err != nil {
		t.Fatal(err)
	}
	if !rv.RebuildsVCenter || rv.Churn {
		t.Errorf("a host addition rebuilds without churn: %+v", rv)
	}
	var hosts IdentityRow
	for _, r := range rv.Identities {
		if strings.HasPrefix(r.Label, "Host") {
			hosts = r
		}
	}
	if hosts.Added != 4 || hosts.Removed != 0 || hosts.Kept != 80 {
		t.Errorf("host identities: %+v", hosts)
	}
	if _, err := c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	r := c.applied()
	if r.version != 2 || r.sim == oldSim {
		t.Errorf("version %d, simulator replaced: %v", r.version, r.sim != oldSim)
	}
	after := nodeUUIDs(c)
	for k, u := range before {
		if after[k] != u {
			t.Errorf("%s changed identity", k)
		}
	}
	if len(after) != len(before)+4 {
		t.Errorf("hosts %d -> %d, want +4", len(before), len(after))
	}
	if v := c.DraftState(); v.Pending.Exists {
		t.Error("draft survived the apply")
	}
	if tl := c.Timeline(); len(tl) < 2 || tl[0].Version != 2 {
		t.Errorf("timeline: %+v", tl)
	}
}

func TestTelemetryOnlyApplyKeepsTheVCenter(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	oldSim := c.applied().sim
	sc := editable(t, c)
	sc.Latency.PerfQuery = &scenario.Delay{FixedMs: 200, JitterMs: 20}
	sc.Faults = []scenario.Fault{{Family: "vsan-pnic-net", Fault: scenario.FaultSystemError}}
	v := put(t, c, sc)
	if v.Pending.Topology != 0 || v.Pending.Telemetry != 2 {
		t.Fatalf("pending: %+v", v.Pending)
	}
	rv, _ := c.Review()
	if rv.RebuildsVCenter || rv.SessionsDropped {
		t.Errorf("telemetry-only review claims a rebuild: %+v", rv)
	}
	if _, err := c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	if c.applied().sim != oldSim {
		t.Error("the vCenter was rebuilt for a telemetry-only change")
	}
	cl := c.applied().topo.Clusters()[0].Key
	if c.applied().sim.Env.Knobs.FamilyFault("vsan-pnic-net", cl) == nil {
		t.Error("fault rule not live after apply")
	}
	if d := c.applied().sim.Env.Knobs.SDKDelay(); d != nil {
		t.Errorf("sdk delay %+v", d)
	}
}

func TestDeleteRegenerateResetSeed(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	before := nodeUUIDs(c)
	telBefore := map[string]string{}
	for _, cl := range c.applied().topo.Clusters() {
		telBefore[cl.Key] = cl.TelemetrySeed
	}
	const del, regen, reset = "dc-01/cluster-05", "dc-01/cluster-06", "dc-01/cluster-07"
	if _, err := c.DeleteCluster(del); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Regenerate(regen); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResetSeed(reset); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteCluster("dc-01/cluster-99"); err == nil {
		t.Error("deleting an unknown cluster should fail")
	}
	rv, err := c.Review()
	if err != nil {
		t.Fatal(err)
	}
	if !rv.Churn {
		t.Error("delete and regenerate are identity changes")
	}
	if _, err := c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	after := nodeUUIDs(c)
	for k, u := range before {
		switch {
		case strings.Contains(k, "/cluster-05/"):
			if _, ok := after[k]; ok {
				t.Errorf("%s survived the delete", k)
			}
		case strings.Contains(k, "/cluster-06/"):
			if after[k] == u || after[k] == "" {
				t.Errorf("%s kept its identity through Regenerate", k)
			}
		default:
			if after[k] != u {
				t.Errorf("%s changed identity (%s)", k, u)
			}
		}
	}
	for _, cl := range c.applied().topo.Clusters() {
		if cl.Key == reset {
			if cl.TelemetrySeed == telBefore[reset] {
				t.Error("Reset cluster seed did not change the telemetry seed")
			}
		} else if cl.TelemetrySeed != telBefore[cl.Key] {
			t.Errorf("%s telemetry seed changed", cl.Key)
		}
	}
	// the other clusters keep their numbers, and the deleted one stays out
	if n := len(c.applied().topo.Clusters()); n != 9 {
		t.Errorf("%d clusters, want 9", n)
	}
	if _, err := c.DeleteCluster(del); err == nil {
		t.Error("deleting it twice should fail")
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	sc := tenClusters(t)
	c := newCtl(t, sc, dir, "file-v1")
	e := editable(t, c)
	setHosts(e, "dc-01/cluster-02", 10)
	put(t, c, e)
	if _, err := c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	ids := nodeUUIDs(c)
	c.Close()

	// same scenario file: the console's edits come back with their identities
	c2 := newCtl(t, tenClusters(t), dir, "file-v1")
	if c2.applied().version != 2 {
		t.Errorf("restored version %d, want 2", c2.applied().version)
	}
	for k, u := range ids {
		if nodeUUIDs(c2)[k] != u {
			t.Fatalf("%s changed identity across a restart", k)
		}
	}
	c2.Close()

	// an edited scenario file wins, and the version moves on
	c3 := newCtl(t, tenClusters(t), dir, "file-v2")
	if c3.applied().version != 3 {
		t.Errorf("version after a file change: %d, want 3", c3.applied().version)
	}
	if n := len(nodeUUIDs(c3)); n != 80 {
		t.Errorf("file wins: %d hosts, want the file's 80", n)
	}
}

func TestScheduledApply(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	sc := editable(t, c)
	setHosts(sc, "dc-01/cluster-02", 7)
	put(t, c, sc)
	res, err := c.Apply("boundary")
	if err != nil || !res.Scheduled {
		t.Fatalf("%+v %v", res, err)
	}
	if res.At.Second() != 0 || res.At.Minute()%5 != 0 || !res.At.After(c.now()) {
		t.Errorf("boundary %v is not the next five-minute mark", res.At)
	}
	if p := c.DraftState().Pending; !p.Scheduled {
		t.Errorf("pending: %+v", p)
	}
	if _, err := c.SetDraft([]byte(`{}`), ""); err == nil {
		t.Error("editing while scheduled should be refused")
	}
	if _, err := c.CancelScheduled(); err != nil {
		t.Fatal(err)
	}
	if p := c.DraftState().Pending; p.Scheduled || !p.Exists {
		t.Errorf("after cancel: %+v", p)
	}
	if c.applied().version != 1 {
		t.Error("a cancelled schedule applied")
	}
	if _, err := c.CancelScheduled(); !errors.Is(err, ErrNoDraft) {
		t.Errorf("second cancel: %v", err)
	}
}

func TestInvalidDraftIsRefusedAndAnEqualDraftVanishes(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	sc := editable(t, c)
	setHosts(sc, "dc-01/cluster-02", 2)
	b, _ := json.Marshal(Strip(sc))
	if _, err := c.SetDraft(b, ""); err == nil {
		t.Fatal("two hosts is not a vSAN cluster")
	} else if _, ok := err.(*Invalid); !ok {
		t.Errorf("want *Invalid, got %T", err)
	}
	b, _ = json.Marshal(map[string]any{"scenario": 1})
	if _, err := c.SetDraft([]byte(`{"nope":1}`), ""); err == nil {
		t.Error("unknown fields must be refused")
	}
	// editing back to the applied scenario removes the draft
	put(t, c, editable(t, c))
	if c.DraftState().Pending.Exists {
		t.Error("a draft equal to the applied scenario should not be pending")
	}
	if _, err := c.Review(); !errors.Is(err, ErrNoDraft) {
		t.Errorf("review without a draft: %v", err)
	}
}

func TestGenerateAndPresets(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	pv, err := c.PreviewGenerate(GenParams{Datacenters: []scenario.Datacenter{{Name: "dc-east", Clusters: 30}, {Name: "dc-west", Clusters: 20}},
		Hosts: 8, VMsPerHost: scenario.IntPtr(10), DisksPerVM: 2, ClaimedDevices: 2, Threads: 64, DomWorlds: scenario.IntPtr(80)})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Projection.Clusters != 50 || pv.Projection.Hosts != 400 || pv.Selected.Calls != 69*50 {
		t.Errorf("preview: clusters %d hosts %d calls %d", pv.Projection.Clusters, pv.Projection.Hosts, pv.Selected.Calls)
	}
	if len(pv.Warnings) == 0 {
		t.Error("50 clusters should warn about fan-out")
	}
	if _, err := c.PreviewGenerate(GenParams{Datacenters: []scenario.Datacenter{{Name: "Bad Name", Clusters: 1}}}); err == nil {
		t.Error("an invalid generator form must be refused")
	}

	// a pending edit is named when a generated draft replaces it
	e := editable(t, c)
	setHosts(e, "dc-01/cluster-02", 9)
	put(t, c, e)
	v, err := c.Generate(GenParams{Seed: "gen-1", Datacenters: []scenario.Datacenter{{Name: "dc-01", Clusters: 3}}, Hosts: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.Discarded, "discarded") {
		t.Errorf("discarded note: %q", v.Discarded)
	}
	rv, _ := c.Review()
	if !rv.Churn || len(rv.Warnings) == 0 {
		t.Errorf("a new seed replaces every identity: %+v", rv)
	}
	if _, err := c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	if n := len(c.applied().topo.Clusters()); n != 3 || c.applied().sc.Seed != "gen-1" {
		t.Errorf("after generate: %d clusters, seed %q", n, c.applied().sc.Seed)
	}

	// presets: telemetry ones keep the topology, topology ones bring their own
	if _, err := c.ApplyPreset("family-fault"); err != nil {
		t.Fatal(err)
	}
	rv, _ = c.Review()
	if rv.RebuildsVCenter || rv.TelemetryCount == 0 {
		t.Errorf("a telemetry preset must not rebuild: %+v", rv)
	}
	c.DiscardDraft()
	if _, err := c.ApplyPreset("phase1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyPreset("nope"); err == nil {
		t.Error("unknown preset must be refused")
	}
	for _, p := range c.Presets() {
		c.DiscardDraft()
		if _, err := c.ApplyPreset(p.Name); err != nil {
			t.Errorf("preset %s: %v", p.Name, err)
		}
	}
}

func TestTreeAndClusterView(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	tr := c.Tree()
	if tr.Clusters != 10 || tr.Overridden != 2 || tr.PendingN != 0 {
		t.Errorf("tree: %+v", tr)
	}
	e := editable(t, c)
	setHosts(e, "dc-01/cluster-02", 10)
	put(t, c, e)
	if _, err := c.DeleteCluster("dc-01/cluster-09"); err != nil {
		t.Fatal(err)
	}
	tr = c.Tree()
	states := map[string]string{}
	for _, dc := range tr.Datacenters {
		for _, cl := range dc.Clusters {
			states[cl.Name] = cl.State
		}
	}
	if states["cluster-02"] != "pending" || states["cluster-09"] != "removed" || states["cluster-01"] != "" {
		t.Errorf("states: %v", states)
	}
	cv, err := c.Cluster("dc-01/cluster-02")
	if err != nil {
		t.Fatal(err)
	}
	var unchanged, added int
	for _, h := range cv.Hosts {
		switch h.Identity {
		case "unchanged":
			unchanged++
			if !strings.HasPrefix(h.MoRef, "host-") {
				t.Errorf("host %s has no MoRef", h.Name)
			}
		case "new":
			added++
		}
	}
	if unchanged != 6 || added != 4 || cv.State != "pending" || cv.HostsOld != 6 {
		t.Errorf("cluster-02: unchanged %d added %d state %s hostsOld %d", unchanged, added, cv.State, cv.HostsOld)
	}
	if cv.Override == nil || cv.Override.Hosts != 10 {
		t.Errorf("override: %+v", cv.Override)
	}
	if _, err := c.Cluster("dc-01/cluster-99"); err == nil {
		t.Error("unknown cluster")
	}
}

// The projection counts entities without building them; the count must equal
// what the wildcard handler enumerates for every family on both architectures.
func TestPerfEntityCountMatchesEnumeration(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	e := editable(t, c)
	e.Clusters[0].Architecture = "osa"
	put(t, c, e)
	mixed, _ := c.previewTopology(c.draft)
	osa := 0
	for _, cl := range mixed.Clusters() {
		if cl.Spec.Architecture == "osa" {
			osa++
		}
		for _, family := range vsanhealth.PerfFamilyNames(c.cat, cl) {
			refs, err := vsanhealth.PerfEntities(c.cat, cl, family)
			if err != nil {
				t.Fatal(err)
			}
			got, err := vsanhealth.PerfEntityCount(c.cat, cl, family)
			if err != nil || got != len(refs) {
				t.Errorf("%s %s: count %d (%v), enumerated %d", cl.Key, family, got, err, len(refs))
			}
		}
	}
	if osa != 1 {
		t.Fatalf("%d OSA clusters, want 1", osa)
	}
}

// A mixed ESA and OSA topology must be labelled per cluster: the datastore
// type follows the cluster's architecture and the projection counts OSA
// clusters so the console does not call every cluster ESA.
func TestArchitectureIsLabelledPerCluster(t *testing.T) {
	sc := tenClusters(t)
	c := newCtl(t, sc, "", "t")
	if got := Project(c.cat, c.applied().topo).OSAClusters; got != 0 {
		t.Fatalf("ESA-only topology: %d OSA clusters", got)
	}
	e := editable(t, c)
	for i := range e.Clusters {
		if e.Clusters[i].Key == "dc-01/cluster-01" {
			e.Clusters[i].Architecture = "osa"
		}
	}
	if e.Defaults.Architecture != "" {
		t.Fatalf("test assumes an ESA default, got %q", e.Defaults.Architecture)
	}
	put(t, c, e)
	mixed, _ := c.previewTopology(c.draft)
	if got := Project(c.cat, mixed).OSAClusters; got != 1 {
		t.Errorf("mixed topology: %d OSA clusters, want 1", got)
	}
	for key, want := range map[string]string{"dc-01/cluster-01": "vSAN OSA", "dc-01/cluster-02": "vSAN ESA"} {
		cv, err := c.Cluster(key)
		if err != nil {
			t.Fatal(err)
		}
		if got := cv.Datastores[0].Type; got != want {
			t.Errorf("%s datastore type %q, want %q", key, got, want)
		}
	}
}

// ---- the API plane ------------------------------------------------------------

const soapEnv = `<?xml version="1.0" encoding="UTF-8"?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><soapenv:Body>%s</soapenv:Body></soapenv:Envelope>`

type apiClient struct {
	t   *testing.T
	srv *httptest.Server
	hc  *http.Client
}

func newAPIClient(t *testing.T, c *Controller) *apiClient {
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &apiClient{t: t, srv: srv, hc: &http.Client{Jar: jar, Timeout: 20 * time.Second}}
}

func (a *apiClient) post(path, body string) (int, string) {
	a.t.Helper()
	res, err := a.hc.Post(a.srv.URL+path, "text/xml", strings.NewReader(fmt.Sprintf(soapEnv, body)))
	if err != nil {
		a.t.Fatalf("POST %s: %v", path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func (a *apiClient) login(user, pass string) bool {
	a.t.Helper()
	_, r := a.post("/sdk", `<Login xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><userName>`+user+`</userName><password>`+pass+`</password></Login>`)
	return !strings.Contains(r, "Login failure")
}

func (a *apiClient) perf(cluster, family string) (int, string) {
	now := time.Now().UTC()
	return a.post("/vsanHealth", fmt.Sprintf(`<VsanPerfQueryPerf xmlns="urn:vsan"><_this type="VsanPerformanceManager">vsan-performance-manager</_this><querySpecs><entityRefId>%s:*</entityRefId><startTime>%s</startTime><endTime>%s</endTime></querySpecs><cluster type="ClusterComputeResource">%s</cluster></VsanPerfQueryPerf>`,
		family, now.Add(-15*time.Minute).Format("2006-01-02T15:04:05.000Z"), now.Format("2006-01-02T15:04:05.000Z"), cluster))
}

func TestRequestLogAndFaultClassification(t *testing.T) {
	sc := tenClusters(t)
	sc.Faults = []scenario.Fault{{Family: "host-domowner", Fault: scenario.FaultSystemError}}
	c := newCtl(t, sc, "", "t")
	a := newAPIClient(t, c)
	if !a.login("administrator@vsphere.local", "VMware1!") {
		t.Fatal("login refused")
	}
	if a.login("administrator@vsphere.local", "wrong") {
		t.Fatal("wrong password accepted")
	}
	cl := c.applied().sim.Clusters[0]
	ref := cl.Ref.Value
	_, ok := a.perf(ref, "vsan-pnic-net")
	if strings.Contains(ok, "faultstring") {
		t.Fatalf("pnic query faulted: %.300s", ok)
	}
	_, bad := a.perf(ref, "host-domowner")
	if !strings.Contains(bad, "faultstring") {
		t.Fatalf("injected fault not served: %.300s", bad)
	}
	a.perf(ref, "disk-group")     // answers empty by design
	a.perf(ref, "no-such-family") // a real, unexpected fault

	calls := c.Log.Query(activity.Filter{Limit: 50})
	byKey := map[string]activity.Call{}
	for _, call := range calls {
		byKey[call.Method+"/"+call.Family+"/"+call.Result] = call
	}
	if call, ok := byKey["VsanPerfQueryPerf/vsan-pnic-net/ok"]; !ok || call.Entities == 0 || call.Empty != 0 || call.Cluster != cl.Cluster.Key || call.Bytes == 0 {
		t.Errorf("ok perf call: %+v", call)
	}
	if call, ok := byKey["VsanPerfQueryPerf/host-domowner/injected"]; !ok || call.Fault != "SystemError" {
		t.Errorf("injected perf call: %+v", call)
	}
	if call, ok := byKey["VsanPerfQueryPerf/disk-group/ok"]; !ok || call.Empty == 0 || call.Empty != call.Entities {
		t.Errorf("empty perf call: %+v", call)
	}
	if _, ok := byKey["VsanPerfQueryPerf/no-such-family/fault"]; !ok {
		t.Errorf("unexpected fault not classified: %v", keys(byKey))
	}
	if call, ok := byKey["Login//denied"]; !ok || call.Surface != "sdk" || call.Body != "" {
		t.Errorf("refused login: %+v", call)
	}
	for _, call := range c.Log.Query(activity.Filter{Surface: "sdk", Limit: 50}) {
		if call.Body != "" {
			t.Error("a /sdk body (a login carries a password) must never be kept")
		}
	}
	// the detail view keeps the /vsanHealth body, and nothing secret is in it
	if full, ok := c.Log.Get(byKey["VsanPerfQueryPerf/vsan-pnic-net/ok"].Seq); !ok || !strings.Contains(full.Body, "vsan-pnic-net:*") || strings.Contains(full.Body, "VMware1!") {
		t.Errorf("detail: %+v", full)
	}

	sum := c.Log.Summarize(time.Now())
	if sum.Faults.Injected != 1 || sum.Faults.Unexpected != 1 || sum.Faults.Denied != 1 {
		t.Errorf("faults: %+v", sum.Faults)
	}
	if got := sum.Cycle.Calls; got == 0 || sum.Cycle.Client == "" {
		t.Errorf("cycle: %+v", sum.Cycle)
	}
	sawInjected := false
	for _, f := range sum.Cycle.Findings {
		if f.Severity == "warn" && strings.Contains(f.Title, "injected") {
			sawInjected = true
		}
	}
	if !sawInjected {
		t.Errorf("findings: %+v", sum.Cycle.Findings)
	}
}

func keys(m map[string]activity.Call) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPauseRefusesConnections(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	a := newAPIClient(t, c)
	c.SetPaused(true)
	if _, err := a.hc.Post(a.srv.URL+"/sdk", "text/xml", strings.NewReader(fmt.Sprintf(soapEnv, `<RetrieveServiceContent xmlns="urn:vim25"><_this type="ServiceInstance">ServiceInstance</_this></RetrieveServiceContent>`))); err == nil {
		t.Error("a paused simulator must drop the connection")
	}
	if got := c.Log.Query(activity.Filter{Result: activity.ResultRefused}); len(got) != 1 {
		t.Errorf("refused calls: %+v", got)
	}
	c.SetPaused(false)
	if !a.login(DefaultUsername, DefaultPassword) && len(c.opts.Credentials) == 0 && len(tenClusters(t).Auth.Users) == 0 {
		t.Error("resume should serve again")
	}
	if c.Paused() {
		t.Error("still paused")
	}
}

func TestTimeoutFaultIsInjected(t *testing.T) {
	old := vsanhealth.TimeoutHold
	vsanhealth.TimeoutHold = 50 * time.Millisecond
	defer func() { vsanhealth.TimeoutHold = old }()
	sc := tenClusters(t)
	sc.Faults = []scenario.Fault{{Family: "vsan-pnic-net", Fault: scenario.FaultTimeout}}
	c := newCtl(t, sc, "", "t")
	a := newAPIClient(t, c)
	a.login("administrator@vsphere.local", "VMware1!")
	a.perf(c.applied().sim.Clusters[0].Ref.Value, "vsan-pnic-net")
	got := c.Log.Query(activity.Filter{Family: "vsan-pnic-net"})
	if len(got) != 1 || got[0].Result != activity.ResultInjected || got[0].DurationMs < 50 {
		t.Errorf("timeout call: %+v", got)
	}
}

func TestStripRemovesSecrets(t *testing.T) {
	sc := tenClusters(t)
	sc.TLS = scenario.TLS{CertFile: "/x/c", KeyFile: "/x/k"}
	b, _ := json.Marshal(Strip(sc))
	if strings.Contains(string(b), "VMware1!") || strings.Contains(string(b), "administrator@") || strings.Contains(string(b), "/x/k") {
		t.Errorf("secrets in the editable view: %s", b)
	}
	c := newCtl(t, sc, "", "t")
	if _, err := os.Stat(filepath.Join(c.opts.DataDir, "console.json")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(c.opts.DataDir, "console.json"))
	if strings.Contains(string(raw), "VMware1!") || strings.Contains(string(raw), "administrator@") {
		t.Error("console.json holds credentials")
	}
}

// An unauthenticated query is refused before any fault rule is consulted, so
// the log must not call it injected.
func TestUnauthenticatedQueryIsNotInjected(t *testing.T) {
	sc := tenClusters(t)
	sc.Faults = []scenario.Fault{{Family: "host-domowner", Fault: scenario.FaultSystemError}}
	c := newCtl(t, sc, "", "t")
	a := newAPIClient(t, c) // no login
	a.perf(c.applied().sim.Clusters[0].Ref.Value, "host-domowner")
	got := c.Log.Query(activity.Filter{Family: "host-domowner"})
	if len(got) != 1 || got[0].Result != activity.ResultDenied || got[0].Fault != "NotAuthenticated" {
		t.Errorf("unauthenticated call: %+v", got)
	}
}

// Removing something preserves surviving MoRefs; telemetry edits do not move the version.
func TestRemovalKeepsSurvivorMoRefs(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	refs := func() map[string]string {
		m := map[string]string{}
		for _, r := range c.applied().sim.Clusters {
			m[r.Cluster.Key] = r.Ref.Value
			for i, h := range r.Cluster.Hosts {
				m[h.Key] = r.Hosts[i].Value
			}
			for i, v := range r.Cluster.VMs {
				m[v.Key] = r.VMs[i].Value
			}
		}
		return m
	}
	before := refs()
	if _, err := c.DeleteCluster("dc-01/cluster-02"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	after := refs()
	if len(after) >= len(before) {
		t.Fatalf("removal did not shrink the inventory: %d -> %d", len(before), len(after))
	}
	for key, ref := range after {
		if before[key] != ref {
			t.Errorf("%s moved from %q to %q", key, before[key], ref)
		}
	}
}

func TestTelemetryOnlyChangeKeepsVersion(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	e := editable(t, c)
	setHosts(e, "dc-01/cluster-02", 12)
	put(t, c, e)
	if rv, _ := c.Review(); rv.Churn {
		t.Errorf("adding hosts reported identity churn: %+v", rv)
	}
	c.DiscardDraft()

	e = editable(t, c)
	e.Latency.SDK = &scenario.Delay{FixedMs: 5}
	put(t, c, e)
	rv, _ := c.Review()
	if rv.To != rv.From || c.DraftState().Pending.Target != rv.From {
		t.Errorf("a telemetry-only change moves the version: %d -> %d", rv.From, rv.To)
	}
	if _, err := c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	if c.applied().version != 1 {
		t.Errorf("version %d after a telemetry apply", c.applied().version)
	}
}

// A failed build must leave the running vCenter alone and say so until dismissed.
func TestFailedApplyIsReportedAndHarmless(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	e := editable(t, c)
	setHosts(e, "dc-01/cluster-02", 9)
	put(t, c, e)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	if _, err := c.Apply("now"); err == nil {
		t.Fatal("the build should fail without a temp directory")
	}
	st := c.State()
	if !strings.Contains(st.LastErr, "failed to apply") || !strings.Contains(st.LastErr, "v1 is still serving") {
		t.Errorf("last error: %q", st.LastErr)
	}
	if c.applied().version != 1 || !c.DraftState().Pending.Exists {
		t.Error("a failed apply must keep the version and the draft")
	}
	if tl := c.Timeline(); tl[0].Text != "Apply failed: v1 still serving" {
		t.Errorf("timeline: %+v", tl[0])
	}
	c.DismissError()
	if c.State().LastErr != "" {
		t.Error("dismiss did not clear the error")
	}
}

func TestDeleteClusterDropsItsScopedFaultRules(t *testing.T) {
	sc := tenClusters(t)
	const del, other = "dc-01/cluster-03", "dc-01/cluster-04"
	sc.Faults = []scenario.Fault{
		{Family: "vsan-pnic-net", Fault: scenario.FaultSystemError, Clusters: []string{del}},
		{Family: "host-domowner", Fault: scenario.FaultSystemError, Clusters: []string{del, other}},
	}
	c := newCtl(t, sc, "", "t")
	v, err := c.DeleteCluster(del)
	if err != nil {
		t.Fatal(err)
	}
	got := v.Scenario.Faults
	if len(got) != 1 || got[0].Family != "host-domowner" || len(got[0].Clusters) != 1 || got[0].Clusters[0] != other {
		t.Fatalf("faults after delete = %+v; the rule scoped only to the deleted cluster must go, not widen to all clusters", got)
	}
}

// Loading a telemetry preset must not discard what is already pending.
func TestTelemetryPresetKeepsThePendingDraft(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	e := editable(t, c)
	setHosts(e, "dc-01/cluster-02", 9)
	put(t, c, e)
	if _, err := c.Regenerate("dc-01/cluster-04"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetDraft(mustJSON(t, Strip(c.draft.Scenario)), ScopeDashboard); err != nil {
		t.Fatal(err)
	}
	v, err := c.ApplyPreset("brown-out")
	if err != nil {
		t.Fatal(err)
	}
	var hosts int
	for _, o := range v.Scenario.Clusters {
		if o.Key == "dc-01/cluster-02" {
			hosts = o.Hosts
		}
	}
	if hosts != 9 {
		t.Errorf("the pending host edit was discarded: %+v", v.Scenario.Clusters)
	}
	if len(v.Regenerate) != 1 || v.Regenerate[0] != "dc-01/cluster-04" {
		t.Errorf("regeneration marker lost: %v", v.Regenerate)
	}
	if v.Scope != ScopeDashboard {
		t.Errorf("scope %q", v.Scope)
	}
	if len(v.Scenario.Faults) != 1 || v.Scenario.Faults[0].Family != "host-domowner" || v.Scenario.Latency.PerfQuery == nil || v.Scenario.Latency.PerfQuery.FixedMs != 300 {
		t.Errorf("preset not applied: %+v %+v", v.Scenario.Faults, v.Scenario.Latency)
	}
	if rv, _ := c.Review(); rv.TopologyCount == 0 || rv.TelemetryCount == 0 {
		t.Errorf("review must show both: %+v", rv)
	}

	// a topology preset replaces the topology but keeps the pending scope
	v, err = c.ApplyPreset("phase1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Scope != ScopeDashboard {
		t.Errorf("topology preset reset the scope to %q", v.Scope)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOSAGeneratorPreservesSettingsAndReviewsChanges(t *testing.T) {
	sc := scenario.Phase1("osa-generator")
	sc.Defaults.Architecture = "osa"
	sc.Defaults.Hosts = 3
	primary := false
	sc.Defaults.StatsPrimary = &primary
	c := newCtl(t, sc, "", "t")
	params := GenParams{Datacenters: []scenario.Datacenter{{Name: "dc-01", Clusters: 1}}, Hosts: 3}
	generated, err := c.Generate(params)
	if err != nil {
		t.Fatal(err)
	}
	if generated.Scenario.Defaults.Architecture != "osa" || generated.Scenario.Defaults.StatsPrimary == nil || *generated.Scenario.Defaults.StatsPrimary {
		t.Fatalf("generator lost OSA settings: %+v", generated.Scenario.Defaults)
	}
	c.DiscardDraft()
	online := true
	params.Architecture = "esa"
	params.StatsPrimary = &online
	if _, err := c.Generate(params); err != nil {
		t.Fatal(err)
	}
	review, err := c.Review()
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]Change{}
	for _, ch := range review.Changes {
		changes[ch.Field] = ch
	}
	if changes["Architecture"].Before != "OSA" || changes["Architecture"].After != "ESA" || changes["Stats primary"].Before != "offline" || changes["Stats primary"].After != "online" {
		t.Fatalf("missing settings in review: %+v", review.Changes)
	}
	for _, unchanged := range review.NotAffected {
		if strings.Contains(unchanged, "other clusters") {
			t.Errorf("changed cluster listed as unaffected: %s", unchanged)
		}
	}
	c.DiscardDraft()
	if _, err := c.ApplyPreset("osa"); err != nil {
		t.Fatal(err)
	}
	if got := c.DraftState().Scenario.Defaults.Architecture; got != "osa" {
		t.Errorf("OSA preset: %q", got)
	}
}

func TestArchitectureReviewCountsPerformanceIdentityChanges(t *testing.T) {
	sc := scenario.Phase1("arch-review")
	sc.Defaults.Hosts = 3
	c := newCtl(t, sc, "", "t")
	edit := editable(t, c)
	edit.Clusters = append(edit.Clusters, scenario.ClusterOverride{Key: "dc-01/cluster-01", ClusterSpec: scenario.ClusterSpec{Architecture: "osa"}})
	put(t, c, edit)
	review, err := c.Review()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ch := range review.Changes {
		if ch.Field == "Architecture" && ch.Before == "ESA" && ch.After == "OSA" {
			found = true
		}
	}
	if !found {
		t.Fatalf("architecture missing in review: %+v", review.Changes)
	}
	var perf IdentityRow
	for _, row := range review.Identities {
		if row.Label == "Performance entities" {
			perf = row
		}
	}
	if perf.Kept == 0 || perf.Added == 0 || perf.Removed == 0 || !review.Churn {
		t.Errorf("architecture change must count retained, new and removed performance identities: %+v, churn=%v", perf, review.Churn)
	}
	tree := c.Tree()
	if tree.Datacenters[0].Clusters[0].Architecture != "osa" || tree.Datacenters[0].Clusters[0].State != "pending" {
		t.Errorf("tree does not show pending OSA: %+v", tree.Datacenters[0].Clusters[0])
	}
	view, err := c.Cluster("dc-01/cluster-01")
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range view.Checks {
		if check.Title == "Dedup is off" {
			t.Error("ESA dedup warning should not apply to OSA")
		}
	}
}

func TestStatsPrimaryChangeReviewAndWarning(t *testing.T) {
	sc := scenario.Phase1("osa-primary-review")
	sc.Defaults.Architecture, sc.Defaults.Hosts = "osa", 3
	c := newCtl(t, sc, "", "t")
	edit := editable(t, c)
	offline := false
	edit.Clusters = append(edit.Clusters, scenario.ClusterOverride{Key: "dc-01/cluster-01", ClusterSpec: scenario.ClusterSpec{StatsPrimary: &offline}})
	put(t, c, edit)
	review, err := c.Review()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range review.Changes {
		if change.Field == "Stats primary" && change.Before == "online" && change.After == "offline" {
			found = true
		}
	}
	if !found {
		t.Fatalf("stats-primary change not reviewed: %+v", review.Changes)
	}
	for _, row := range review.Identities {
		if row.Label == "Performance entities" && (row.Kept != review.Before.Entities || row.Added != 0 || row.Removed != 0) {
			t.Errorf("offline primary should retain performance identities: %+v", row)
		}
	}
	if review.After.Entities != 0 {
		t.Errorf("offline primary should project no samples: %+v", review.After)
	}
	if review.Churn {
		t.Error("stats unavailability alone must not claim stable UUIDs were replaced")
	}
	view, err := c.Cluster("dc-01/cluster-01")
	if err != nil {
		t.Fatal(err)
	}
	warned := false
	for _, check := range view.Checks {
		if check.Title == "Stats primary is offline" {
			warned = true
		}
	}
	if !warned {
		t.Errorf("missing offline warning: %+v", view.Checks)
	}
}

func TestOSACompatibilityAndDashboardCoverage(t *testing.T) {
	sc := scenario.Phase1("osa-compat")
	sc.Defaults.Architecture = "osa"
	primary := false
	sc.Defaults.StatsPrimary = &primary
	c := newCtl(t, sc, "", "t")
	compat := c.Compatibility()
	for name, want := range map[string]int{"data": 30, "empty": 26, "invalid-argument": 16, "not-supported": 2} {
		if got := compat.OSAKinds[name]; got != want {
			t.Errorf("OSA %s: %d, want %d", name, got, want)
		}
	}
	for _, pending := range compat.Pending {
		if strings.Contains(pending, "schema-only") || strings.Contains(pending, "is emulated") {
			t.Errorf("pending list should name only unverified behavior: %q", pending)
		}
	}
	view, err := c.Cluster("dc-01/cluster-01")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, check := range view.Checks {
		if check.Title == "Stats primary is offline" {
			found = true
		}
		if check.Title == "Dedup is off" {
			t.Error("OSA cannot use ESA dedup warning")
		}
	}
	if !found {
		t.Error("missing primary warning")
	}
	projection := Project(c.cat, c.applied().topo)
	flags := map[string]bool{}
	for _, family := range projection.Families {
		flags[family.Family] = family.Dashboard
	}
	if !flags["cache-disk"] || !flags["capacity-disk"] || flags["disk-group"] {
		t.Errorf("dashboard scope: cache=%v capacity=%v group=%v", flags["cache-disk"], flags["capacity-disk"], flags["disk-group"])
	}
}

// Applying a cluster removal and restarting from the saved state must expose
// the same references and UUIDs for each surviving cluster, host and VM.
func TestRemovalIdentityAcrossApplyAndRestart(t *testing.T) {
	sc, err := scenario.Parse([]byte("seed: apply-identity\ndatacenters: [{name: dc, clusters: 3}]\ndefaults: {hosts: 3, vms: {perHost: 1}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := newCtl(t, sc, dir, "same")
	identities := func(c *Controller) map[string]string {
		m := map[string]string{}
		for _, refs := range c.applied().sim.Clusters {
			cl := refs.Cluster
			m[cl.Key] = fmt.Sprint(refs.Ref, cl.VsanUUID, cl.StatsUUID)
			for i, h := range cl.Hosts {
				m[h.Key] = fmt.Sprint(refs.Hosts[i], h.HardwareUUID, h.NodeUUID)
			}
			for i, v := range cl.VMs {
				m[v.Key] = fmt.Sprint(refs.VMs[i], v.UUID, v.InstanceUUID)
			}
		}
		return m
	}
	before := identities(c)
	if _, err := c.DeleteCluster("dc/cluster-02"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	after := identities(c)
	for key, value := range after {
		if before[key] != value {
			t.Errorf("%s changed on apply", key)
		}
	}
	c.Close()
	restarted := newCtl(t, sc, dir, "same")
	for key, value := range identities(restarted) {
		if after[key] != value {
			t.Errorf("%s changed on restart", key)
		}
	}
}

func TestGrowingHostsKeepsMoRefsOnApplyAndRestart(t *testing.T) {
	parse := func(devices int) *scenario.Scenario {
		sc, err := scenario.Parse([]byte(fmt.Sprintf("seed: growth\ndatacenters: [{name: dc, clusters: 2}]\ndefaults: {hosts: 3, host: {localDevices: %d}, vms: {perHost: 1}}\n", devices)))
		if err != nil {
			t.Fatal(err)
		}
		return sc
	}
	refs := func(c *Controller) map[string]string {
		m := map[string]string{}
		for _, r := range c.applied().sim.Clusters {
			m[r.Cluster.Key] = r.Ref.Value
			for i, h := range r.Cluster.Hosts {
				m[h.Key] = r.Hosts[i].Value
			}
			for i, v := range r.Cluster.VMs {
				m[v.Key] = r.VMs[i].Value
			}
		}
		return m
	}
	same := func(what string, want, got map[string]string) {
		t.Helper()
		if len(want) != len(got) {
			t.Fatalf("%s: %d objects, want %d", what, len(got), len(want))
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s is %q, was %q", what, k, got[k], v)
			}
		}
	}
	dir := t.TempDir()
	c := newCtl(t, parse(1), dir, "one")
	original := refs(c)

	e := editable(t, c)
	e.Defaults.Host.LocalDevices = scenario.IntPtr(2)
	put(t, c, e)
	if _, err := c.Apply("now"); err != nil {
		t.Fatalf("adding a device to existing hosts must apply: %v", err)
	}
	same("after apply", original, refs(c))
	c.Close()

	c = newCtl(t, parse(3), dir, "three")
	same("after editing the scenario file while stopped", original, refs(c))
}

func TestReviewDistinguishesRestoredAndNewIdentities(t *testing.T) {
	sc, err := scenario.Parse([]byte("seed: review-restored\ndatacenters: [{name: dc, clusters: 1}]\ndefaults: {hosts: 4, vms: {perHost: 1}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	c := newCtl(t, sc, "", "same")
	smaller := editable(t, c)
	setHosts(smaller, "dc/cluster-01", 3)
	put(t, c, smaller)
	if _, err := c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	bigger := editable(t, c)
	setHosts(bigger, "dc/cluster-01", 5)
	put(t, c, bigger)
	rv, err := c.Review()
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"Host", "VM", "MoRefs"} {
		found := false
		for _, row := range rv.Identities {
			if strings.HasPrefix(row.Label, prefix) {
				found = true
				want := 1
				if prefix == "MoRefs" {
					want = 2
				}
				if row.Restored != want || row.Added != want || row.Removed != 0 {
					t.Errorf("%s accounting: %+v", prefix, row)
				}
			}
		}
		if !found {
			t.Errorf("missing %s row", prefix)
		}
	}
}

func TestReviewRestorationDependsOnRetirement(t *testing.T) {
	sc, err := scenario.Parse([]byte("seed: retired\ndatacenters: [{name: dc, clusters: 2}]\ndefaults: {hosts: 3, vms: {perHost: 1}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			c := newCtl(t, sc, "", "same")
			identities := func(c *Controller) map[string]string {
				result := map[string]string{}
				for _, refs := range c.applied().sim.Clusters {
					cl := refs.Cluster
					result[cl.Key] = fmt.Sprint(refs.Ref, cl.VsanUUID, cl.StatsUUID, cl.PolicyUUID)
					for i, h := range cl.Hosts {
						result[h.Key] = fmt.Sprint(refs.Hosts[i], h.HardwareUUID, h.NodeUUID)
					}
					for i, vm := range cl.VMs {
						result[vm.Key] = fmt.Sprint(refs.VMs[i], vm.UUID, vm.InstanceUUID)
					}
				}
				return result
			}
			before := identities(c)
			if explicit {
				if _, err := c.DeleteCluster("dc/cluster-02"); err != nil {
					t.Fatal(err)
				}
			} else {
				next := editable(t, c)
				next.Datacenters[0].Clusters = 1
				put(t, c, next)
			}
			if _, err := c.Apply("now"); err != nil {
				t.Fatal(err)
			}
			next := editable(t, c)
			next.Datacenters[0].Clusters = 2
			next.Removed = nil
			put(t, c, next)
			rv, err := c.Review()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range rv.Identities {
				if strings.HasPrefix(row.Label, "Cluster") {
					found = true
					if row.Added != 0 || row.Restored != 1 {
						t.Errorf("returning: %+v", row)
					}
				}
			}
			if !found {
				t.Fatal("missing cluster row")
			}
			if _, err := c.Apply("now"); err != nil {
				t.Fatal(err)
			}
			after := identities(c)
			for key, value := range before {
				if after[key] != value {
					t.Errorf("%s changed after re-add: %s -> %s", key, value, after[key])
				}
			}
		})
	}
}

func TestGenerateKeepsNFSDatastores(t *testing.T) {
	c := newCtl(t, tenClusters(t), "", "t")
	nfs := scenario.NFSDatastore{Name: "shared-nfs", RemoteHost: "nfs01.example.test", RemotePath: "/vol/shared", Capacity: 5_000_000_000_000, FreeSpace: 3_000_000_000_000}
	if _, err := c.Generate(GenParams{Seed: "nfs-1", Datacenters: []scenario.Datacenter{{Name: "dc-01", Clusters: 1, NFS: []scenario.NFSDatastore{nfs}}}, Hosts: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	dcs := c.applied().topo.Datacenters
	if len(dcs) != 1 || len(dcs[0].NFS) != 1 || dcs[0].NFS[0].Spec.Name != "shared-nfs" || dcs[0].NFS[0].Spec.RemoteHost != "nfs01.example.test" {
		t.Errorf("generated topology lost NFS datastores: %+v", dcs)
	}
}

func TestReviewAccountsForNFSDatastoresAcrossDraftPaths(t *testing.T) {
	base := scenario.Phase1("nfs-review")
	base.Datacenters[0].NFS = []scenario.NFSDatastore{{Name: "shared-nfs", RemoteHost: "nfs.sim.invalid", RemotePath: "/exports/a", Capacity: 1000, FreeSpace: 800}}
	c := newCtl(t, base, "", "t")
	check := func(wantObject, effect string) {
		t.Helper()
		rv, err := c.Review()
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range rv.Changes {
			if ch.Object == wantObject && ch.Field == "NFS datastore" && strings.Contains(ch.Effect, effect) {
				if !rv.RebuildsVCenter || rv.TopologyCount == 0 {
					t.Fatalf("NFS change must rebuild: %+v", rv)
				}
				return
			}
		}
		t.Fatalf("missing NFS %s %s in %+v", wantObject, effect, rv.Changes)
	}
	sc := Strip(base)
	sc.Datacenters[0].NFS[0].RemotePath = "/exports/b"
	if _, err := c.SetDraft(mustJSON(t, sc), ""); err != nil {
		t.Fatal(err)
	}
	check("dc-01/shared-nfs", "updates")
	sc.Datacenters[0].NFS = append(sc.Datacenters[0].NFS, scenario.NFSDatastore{Name: "new-nfs", RemoteHost: "nfs.sim.invalid", RemotePath: "/exports/new", Capacity: 2000, FreeSpace: 1000})
	if _, err := c.SetDraft(mustJSON(t, sc), ""); err != nil {
		t.Fatal(err)
	}
	check("dc-01/new-nfs", "adds")
	sc.Datacenters[0].NFS = nil
	if _, err := c.SetDraft(mustJSON(t, sc), ""); err != nil {
		t.Fatal(err)
	}
	check("dc-01/shared-nfs", "removes")
	c.DiscardDraft()
	if _, err := c.ApplyPreset("phase1"); err != nil {
		t.Fatal(err)
	}
	check("dc-01/shared-nfs", "removes")
	c.DiscardDraft()
	if _, err := c.Generate(GenParams{Seed: base.Seed, Datacenters: []scenario.Datacenter{{Name: "dc-01", Clusters: 1}}, Hosts: 3}); err != nil {
		t.Fatal(err)
	}
	check("dc-01/shared-nfs", "removes")
}

func TestNFSReviewAndIDsAfterRemoveAndRestore(t *testing.T) {
	sc, err := scenario.Parse([]byte(`seed: nfs-restored
datacenters:
  - name: dc
    clusters: 1
    nfs:
      - {name: first-nfs, remoteHost: nfs.sim.invalid, remotePath: /first, capacity: 1000, freeSpace: 800}
      - {name: second-nfs, remoteHost: nfs.sim.invalid, remotePath: /second, capacity: 1000, freeSpace: 800}
defaults: {hosts: 3, vms: {perHost: 1}}
`))
	if err != nil {
		t.Fatal(err)
	}
	c := newCtl(t, sc, "", "same")
	datastoreIDs := func() map[string]string {
		t.Helper()
		srv := httptest.NewServer(c.applied().sim.Handler)
		defer srv.Close()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/session", nil)
		req.SetBasicAuth(DefaultUsername, DefaultPassword)
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var token string
		if err := json.NewDecoder(res.Body).Decode(&token); err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/vcenter/datastore", nil)
		req.Header.Set("vmware-api-session-id", token)
		res, err = srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("datastore list: %s", res.Status)
		}
		var rows []struct {
			Name string `json:"name"`
			ID   string `json:"datastore"`
		}
		if err := json.NewDecoder(res.Body).Decode(&rows); err != nil {
			t.Fatal(err)
		}
		ids := map[string]string{}
		for _, r := range rows {
			ids[r.Name] = r.ID
		}
		return ids
	}
	before := datastoreIDs()
	if before["first-nfs"] == "" || before["second-nfs"] == "" {
		t.Fatal("initial NFS datastores missing")
	}
	firstUUID := c.applied().topo.Datacenters[0].NFS[0].UUID
	removed := editable(t, c)
	removed.Datacenters[0].NFS = removed.Datacenters[0].NFS[1:]
	put(t, c, removed)
	if _, err := c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	afterRemove := datastoreIDs()
	for name, id := range before {
		if name != "first-nfs" && afterRemove[name] != id {
			t.Errorf("%s changed after removal: %s -> %s", name, id, afterRemove[name])
		}
	}
	restored := editable(t, c)
	restored.Datacenters[0].NFS = append(restored.Datacenters[0].NFS, sc.Datacenters[0].NFS[0], scenario.NFSDatastore{Name: "third-nfs", RemoteHost: "nfs.sim.invalid", RemotePath: "/third", Capacity: 1000, FreeSpace: 800})
	put(t, c, restored)
	review, err := c.Review()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range review.Identities {
		if row.Label == "NFS datastore identities" {
			found = true
			if row.Kept != 1 || row.Restored != 1 || row.Added != 1 || row.Removed != 0 {
				t.Errorf("NFS identity accounting: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("NFS identity row missing")
	}
	if _, err := c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	afterRestore := datastoreIDs()
	for name, id := range before {
		if afterRestore[name] != id {
			t.Errorf("%s changed after restore: %s -> %s", name, id, afterRestore[name])
		}
	}
	for _, n := range c.applied().topo.Datacenters[0].NFS {
		if n.Spec.Name == "first-nfs" && n.UUID != firstUUID {
			t.Errorf("NFS UUID changed: %s -> %s", firstUUID, n.UUID)
		}
	}
}

func TestISCSIDraftReviewAndStableIdentity(t *testing.T) {
	sc := scenario.Phase1("iscsi-review")
	sc.Datacenters[0].ISCSI = []scenario.ISCSIStorage{{Cluster: "dc-01/cluster-01", Adapters: []scenario.ISCSIAdapter{{Name: "adapter-1", Kind: "software"}}, Targets: []scenario.ISCSITarget{{Name: "array", Address: "array.example.test", IQN: "iqn.2026-01.test.example:array"}}, LUNs: []scenario.ISCSILUN{{Name: "shared-vmfs", Target: "array", Number: 1, Capacity: 1048576, FreeSpace: 1024, Paths: []scenario.ISCSIPath{{Adapter: "adapter-1", State: "active"}}}}}}
	c := newCtl(t, sc, "", "test")
	read := func() (string, string, string) {
		t.Helper()
		r := c.applied()
		lun := r.topo.Datacenters[0].ISCSI[0].LUNs[0]
		srv := httptest.NewServer(r.sim.Handler)
		defer srv.Close()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/session", nil)
		req.SetBasicAuth("reader", "password")
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var token string
		if err = json.NewDecoder(res.Body).Decode(&token); err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/vcenter/datastore", nil)
		req.Header.Set("vmware-api-session-id", token)
		res, err = srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var rows []struct {
			Name string `json:"name"`
			ID   string `json:"datastore"`
		}
		if err = json.NewDecoder(res.Body).Decode(&rows); err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Name == "shared-vmfs" {
				return row.ID, lun.UUID, lun.CanonicalName
			}
		}
		t.Fatal("datastore missing")
		return "", "", ""
	}
	id, uuid, canonical := read()
	draft := editable(t, c)
	draft.Datacenters[0].ISCSI[0].LUNs[0].FreeSpace = 2048
	draft.Datacenters[0].ISCSI[0].LUNs[0].Paths[0].State = "dead"
	put(t, c, draft)
	review, err := c.Review()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range review.Changes {
		if change.Field == "iSCSI storage" && strings.Contains(change.After, "dead") {
			found = true
		}
	}
	if !found || !review.RebuildsVCenter {
		t.Fatal("storage change missing from review")
	}
	if _, err = c.Apply("now"); err != nil {
		t.Fatal(err)
	}
	nextID, nextUUID, nextCanonical := read()
	if id != nextID || uuid != nextUUID || canonical != nextCanonical {
		t.Fatalf("identity changed after path edit")
	}
}

func TestGeneratePrunesStorageReferencesToMissingHosts(t *testing.T) {
	sc := scenario.Phase1("gen-storage")
	sc.Datacenters[0].Clusters = 1
	sc.Clusters = []scenario.ClusterOverride{{Key: "dc-01/cluster-01", ClusterSpec: scenario.ClusterSpec{Hosts: 6}}}
	nfs := scenario.NFSDatastore{Name: "shared-nfs", RemoteHost: "nfs01.example.test", RemotePath: "/vol/shared", Capacity: 5_000_000_000_000, FreeSpace: 3_000_000_000_000, InaccessibleHosts: []string{"dc-01/cluster-01/esx06"}}
	sc.Datacenters[0].NFS = []scenario.NFSDatastore{nfs}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	c := newCtl(t, sc, "", "t")
	p := GenParams{Seed: "gen-storage", Datacenters: []scenario.Datacenter{{Name: "dc-01", Clusters: 1, NFS: []scenario.NFSDatastore{nfs}}}, Hosts: 4}
	if _, err := c.PreviewGenerate(p); err != nil {
		t.Fatalf("preview with a host that the generated topology no longer has: %v", err)
	}
	if _, err := c.Generate(p); err != nil {
		t.Fatalf("generate with a host that the generated topology no longer has: %v", err)
	}
}

func TestStorageDraftMarksAffectedClusterAndStandby(t *testing.T) {
	sc := scenario.Phase1("storage-pending")
	sc.Datacenters[0].Clusters = 2
	c := newCtl(t, sc, "", "pending")
	draft := editable(t, c)
	draft.Datacenters[0].NFS = []scenario.NFSDatastore{{Name: "scoped", RemoteHost: "nfs.example.test", RemotePath: "/shared", Capacity: 4096, FreeSpace: 2048, Clusters: []string{"dc-01/cluster-01"}}}
	draft.Datacenters[0].ISCSI = []scenario.ISCSIStorage{{Cluster: "dc-01/cluster-01", Adapters: []scenario.ISCSIAdapter{{Name: "a", Kind: "software"}}, Targets: []scenario.ISCSITarget{{Name: "t", Address: "array.example.test", IQN: "iqn.2026-01.test.example:array"}}, LUNs: []scenario.ISCSILUN{{Name: "shared", Target: "t", Capacity: 1048576, FreeSpace: 1024, Paths: []scenario.ISCSIPath{{Adapter: "a", State: "standby"}}}}}}
	put(t, c, draft)
	tree := c.Tree()
	if tree.PendingN != 1 || tree.Datacenters[0].Clusters[0].State != "pending" || tree.Datacenters[0].Clusters[1].State != "" {
		t.Fatalf("tree pending: %+v", tree)
	}
	v, err := c.Cluster("dc-01/cluster-01")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "pending" {
		t.Fatalf("cluster pending state %q", v.State)
	}
	found := false
	for _, ds := range v.Datastores {
		if ds.Name == "shared" && strings.Contains(ds.State, "standby") {
			found = true
		}
	}
	if !found {
		t.Fatal("standby path missing from storage state")
	}
}

func TestDefaultAndExplicitCredentialPolicies(t *testing.T) {
	cases := []struct {
		name       string
		sc         *scenario.Scenario
		extra      map[string]string
		any        bool
		user, pass string
	}{
		{name: "default", sc: scenario.Phase1("default-auth"), user: DefaultUsername, pass: DefaultPassword},
		{name: "environment override", sc: scenario.Phase1("env-auth"), extra: map[string]string{"operator": "example-test-credential"}, user: "operator", pass: "example-test-credential"},
		{name: "any login", sc: scenario.Phase1("open-auth"), any: true, user: "arbitrary", pass: "example-test-credential"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctl, err := New(Options{Scenario: tc.sc, DataDir: t.TempDir(), Credentials: tc.extra, AnyLogin: tc.any})
			if err != nil {
				t.Fatal(err)
			}
			defer ctl.Close()
			api := newAPIClient(t, ctl)
			if !tc.any {
				bad := newAPIClient(t, ctl)
				code, body := bad.post("/sdk", `<Login xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><userName>`+tc.user+`</userName><password>wrong-credential</password></Login>`)
				if code != http.StatusInternalServerError || !strings.Contains(body, "InvalidLogin") {
					t.Fatalf("wrong SOAP credential: %d %s", code, body)
				}
			}
			if !api.login(tc.user, tc.pass) {
				t.Fatal("expected SOAP credential refused")
			}
			code, body := api.perf(ctl.applied().sim.Clusters[0].Ref.Value, "vsan-pnic-net")
			if code != http.StatusOK || strings.Contains(body, "NotAuthenticated") {
				t.Fatalf("vSAN session: %d", code)
			}
			for _, bad := range []bool{false, true} {
				req, _ := http.NewRequest(http.MethodPost, api.srv.URL+"/api/session", nil)
				pass := tc.pass
				if bad {
					pass = "wrong-credential"
				}
				req.SetBasicAuth(tc.user, pass)
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				want := http.StatusCreated
				if bad && !tc.any {
					want = http.StatusUnauthorized
				}
				if res.StatusCode != want {
					t.Fatalf("REST bad=%v status=%d want=%d", bad, res.StatusCode, want)
				}
			}
		})
	}
}

func TestCredentialDraftApplyRevokesSessionsAndPersists(t *testing.T) {
	dir := t.TempDir()
	sc := scenario.Phase1("credential-draft")
	ctl, err := New(Options{Scenario: sc, SourceHash: "source-a", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	api := newAPIClient(t, ctl)
	if !api.login(DefaultUsername, DefaultPassword) {
		t.Fatal("default login failed")
	}
	req, _ := http.NewRequest(http.MethodPost, api.srv.URL+"/api/session", nil)
	req.SetBasicAuth(DefaultUsername, DefaultPassword)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var token string
	if err = json.NewDecoder(res.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	oldSim := ctl.applied().sim
	if _, err = ctl.PutUser(DefaultUsername, "updated-test-credential", "administrator"); err != nil {
		t.Fatal(err)
	}
	viewBytes, _ := json.Marshal(ctl.CredentialsView())
	if strings.Contains(string(viewBytes), "updated-test-credential") || strings.Contains(string(viewBytes), "$2") {
		t.Fatal("credential draft exposed a secret")
	}
	if _, err = ctl.Review(); err != nil {
		t.Fatal(err)
	}
	if _, err = ctl.Apply("now"); err != nil {
		t.Fatal(err)
	}
	if ctl.applied().sim != oldSim {
		t.Fatal("credentials rebuilt the simulator")
	}
	persisted, err := os.ReadFile(filepath.Join(dir, "vcenter-users.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "updated-test-credential") || strings.Contains(string(persisted), DefaultPassword) {
		t.Fatal("credential file contains a plaintext password")
	}
	info, err := os.Stat(filepath.Join(dir, "vcenter-users.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential file is not owner-only")
	}
	code, _ := api.perf(ctl.applied().sim.Clusters[0].Ref.Value, "vsan-pnic-net")
	if code != http.StatusInternalServerError {
		t.Fatalf("SOAP session survived: %d", code)
	}
	req, _ = http.NewRequest(http.MethodGet, api.srv.URL+"/api/vcenter/datastore", nil)
	req.Header.Set("vmware-api-session-id", token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("REST session survived: %d", res.StatusCode)
	}
	if newAPIClient(t, ctl).login(DefaultUsername, DefaultPassword) {
		t.Fatal("old password accepted")
	}
	if !newAPIClient(t, ctl).login(DefaultUsername, "updated-test-credential") {
		t.Fatal("new password rejected")
	}
	ctl.Close()
	ctl, err = New(Options{Scenario: sc, SourceHash: "source-a", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	if !newAPIClient(t, ctl).login(DefaultUsername, "updated-test-credential") {
		t.Fatal("password change did not persist")
	}
	ctl.Close()
	ctl, err = New(Options{Scenario: sc, SourceHash: "source-b", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	if !newAPIClient(t, ctl).login(DefaultUsername, DefaultPassword) {
		t.Fatal("changed scenario did not restore default")
	}
}

func TestReadOnlyUserCanReadButCannotMutate(t *testing.T) {
	ctl, err := New(Options{Scenario: scenario.Phase1("readonly"), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	admin := newAPIClient(t, ctl)
	if !admin.login(DefaultUsername, DefaultPassword) {
		t.Fatal("admin login failed")
	}
	if _, err = ctl.PutUser("reader@vsphere.local", "readonly-test-credential", "read-only"); err != nil {
		t.Fatal(err)
	}
	if _, err = ctl.Apply("now"); err != nil {
		t.Fatal(err)
	}
	// Adding another user does not sign out the existing administrator.
	status, _ := admin.perf(ctl.applied().sim.Clusters[0].Ref.Value, "vsan-pnic-net")
	if status != http.StatusOK {
		t.Fatalf("unrelated session revoked: %d", status)
	}
	reader := newAPIClient(t, ctl)
	if !reader.login("reader@vsphere.local", "readonly-test-credential") {
		t.Fatal("reader login failed")
	}
	status, body := reader.perf(ctl.applied().sim.Clusters[0].Ref.Value, "vsan-pnic-net")
	if status != http.StatusOK || strings.Contains(body, "NoPermission") {
		t.Fatalf("read-only performance query denied: %d", status)
	}
	u, _ := url.Parse(reader.srv.URL)
	cookies := reader.hc.Jar.Cookies(u)
	if len(cookies) == 0 {
		t.Fatal("reader has no session")
	}
	status, body = reader.post("/sdk", `<HasPrivilegeOnEntity xmlns="urn:vim25"><_this type="AuthorizationManager">AuthorizationManager</_this><entity type="Folder">group-d1</entity><sessionId>`+cookies[0].Value+`</sessionId><privId>Global.Licenses</privId></HasPrivilegeOnEntity>`)
	if status != http.StatusOK || !strings.Contains(body, "<returnval>false</returnval>") {
		t.Fatalf("read-only license privilege not refused: %d", status)
	}
	status, body = reader.post("/sdk", `<Destroy_Task xmlns="urn:vim25"><_this type="Folder">group-d1</_this></Destroy_Task>`)
	if status != http.StatusInternalServerError || !strings.Contains(body, "NoPermission") {
		t.Fatalf("read-only mutation not refused: %d %s", status, body)
	}
	if _, err = ctl.DeleteUser("reader@vsphere.local"); err != nil {
		t.Fatal(err)
	}
	if _, err = ctl.Apply("now"); err != nil {
		t.Fatal(err)
	}
	status, _ = reader.perf(ctl.applied().sim.Clusters[0].Ref.Value, "vsan-pnic-net")
	if status != http.StatusInternalServerError {
		t.Fatalf("removed user's session survived: %d", status)
	}
}
