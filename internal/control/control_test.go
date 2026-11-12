package control

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/activity"
	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/querylog"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/serve"
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

// small is one four-host cluster with one user, which builds quickly.
func small(t *testing.T) *scenario.Scenario {
	t.Helper()
	s := scenario.Phase1("small")
	s.Auth.Users = []scenario.User{{Name: "administrator@vsphere.local", Password: "first-password"}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	return s
}

// clock is a clock the test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newCtlAt(t *testing.T, sc *scenario.Scenario, dir, hash string, now func() time.Time) *Controller {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	cert, err := serve.LoadOrCreateCertificate(filepath.Join(dir, "tls"), []string{"sim.test", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.EnableConsole(dir); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Scenario: sc, SourceHash: hash, DataDir: dir, Endpoint: "sim.test", Version: "test", Certificate: cert, Now: now, APIAddrs: []string{"127.0.0.1:8443"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func newCtl(t *testing.T, sc *scenario.Scenario, dir, hash string) *Controller {
	return newCtlAt(t, sc, dir, hash, nil)
}

// A change that builds a new vCenter keeps the replaced one answering for a
// while. Closing the controller inside that time still removes everything
// both of them wrote to the temporary directory.
func TestCloseLeavesNoTemporaryFilesBehind(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	c := newCtl(t, small(t), "", "t")
	must(t, c, "add-hosts", map[string]string{"cluster": "dc-01/cluster-01"}, map[string]any{"n": 1})
	during, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(during) == 0 {
		t.Fatal("the simulated vCenter wrote nothing to the temporary directory, so this test proves nothing")
	}
	c.Close()
	left, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		t.Errorf("left behind after Close: %s", e.Name())
	}
}

func params(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// do presses a task's button with its form's own values, changed by set.
func do(t *testing.T, c *Controller, kind string, q map[string]string, set map[string]any) *RunResult {
	t.Helper()
	form, err := c.TaskForm(kind, q, Caller{Host: "console.test:9090"})
	if err != nil {
		t.Fatalf("%s form: %v", kind, err)
	}
	for k, v := range set {
		form.Values[k] = v
	}
	r, err := c.TaskRun(kind, params(t, form.Values), nil, true, Caller{Host: "console.test:9090"})
	if err != nil {
		t.Fatalf("%s: %v", kind, err)
	}
	return r
}

// must is do for a change that is expected to be made.
func must(t *testing.T, c *Controller, kind string, q map[string]string, set map[string]any) *Outcome {
	t.Helper()
	r := do(t, c, kind, q, set)
	if r.Outcome == nil {
		t.Fatalf("%s was not made: errors %v, failed %v", kind, r.Errors, r.Failed)
	}
	return r.Outcome
}

func preview(t *testing.T, c *Controller, kind string, q map[string]string, set map[string]any) string {
	t.Helper()
	form, err := c.TaskForm(kind, q, Caller{})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range set {
		form.Values[k] = v
	}
	p, err := c.TaskPreview(kind, params(t, form.Values), Caller{})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, it := range p.Sum {
		lines = append(lines, it.T)
	}
	return strings.Join(lines, "\n") + p.Idle
}

func identities(c *Controller) map[string]string {
	out := map[string]string{}
	for _, r := range c.applied().sim.Clusters {
		out[r.Cluster.Key] = r.Ref.Value + " " + r.Cluster.VsanUUID
		for i, h := range r.Cluster.Hosts {
			out[h.Key] = r.Hosts[i].Value + " " + h.NodeUUID
		}
	}
	return out
}

func history(c *Controller) string {
	var lines []string
	for _, e := range c.History() {
		lines = append(lines, e.Text)
	}
	return strings.Join(lines, "\n")
}

func wantContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("want %q in:\n%s", want, got)
	}
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
}

// Every kind of vSAN performance data has one plain name of its own.
func TestEveryFamilyHasAPlainName(t *testing.T) {
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for fam := range cat.Families {
		name, ok := familyNames[fam]
		if !ok {
			t.Errorf("%s has no plain name", fam)
			continue
		}
		if other, dup := seen[name]; dup {
			t.Errorf("%s and %s share the name %q", fam, other, name)
		}
		seen[name] = fam
	}
	for _, fam := range mainFamilies {
		if _, ok := cat.Families[fam]; !ok {
			t.Errorf("mainFamilies lists %s, which the catalog does not have", fam)
		}
	}
}

// Adding clusters names them, says so beforehand, and leaves every existing
// identity alone.
func TestAddClusterNamesItAndKeepsIdentities(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	before := identities(c)
	wantContains(t, preview(t, c, "add-cluster", nil, map[string]any{"count": 2}), "**2 clusters, cluster-02 to cluster-03**")
	out := must(t, c, "add-cluster", nil, map[string]any{"count": 2, "hosts": 3, "vms": 2})
	wantContains(t, out.Done, "2 clusters (cluster-02 to cluster-03) added to dc-01 with 3 hosts and 6 VMs each. They are live now.")
	if len(out.Marks) != 2 || out.Marks[0].ID != "dc-01/cluster-02" || out.Marks[0].Tag != "New" {
		t.Errorf("marks %+v", out.Marks)
	}
	after := identities(c)
	for key, id := range before {
		if after[key] != id {
			t.Errorf("%s changed identity: %q -> %q", key, id, after[key])
		}
	}
	if v := c.Clusters(); len(v.Rows) != 3 || v.Rows[2].Hosts != 3 || v.Rows[2].VMs != 6 {
		t.Errorf("clusters %+v", v.Rows)
	}
	wantContains(t, history(c), "2 clusters (cluster-02 to cluster-03) added to dc-01")

	// a new datacenter is named too
	out = must(t, c, "add-cluster", nil, map[string]any{"dc": "new"})
	wantContains(t, out.Done, "cluster-01 added to dc-02")
	if _, err := c.Cluster("dc-02/cluster-01"); err != nil {
		t.Error(err)
	}
}

func TestTaskErrorsArePlainAndChangeNothing(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	r := do(t, c, "add-cluster", nil, map[string]any{"hosts": 2, "vms": 500, "count": 0})
	if r.Outcome != nil {
		t.Fatal("an invalid change was made")
	}
	for field, want := range map[string]string{"hosts": "Enter a number of hosts from 3 to 64.", "vms": "Enter a number of VMs from 0 to 200.", "count": "Enter a number of clusters from 1 to 50."} {
		if r.Errors[field] != want {
			t.Errorf("%s: %q, want %q", field, r.Errors[field], want)
		}
	}
	if len(c.Clusters().Rows) != 1 {
		t.Error("the cluster list changed")
	}
	// a form box left empty is an error, not a zero
	r = do(t, c, "add-cluster", nil, map[string]any{"vms": ""})
	if r.Errors["vms"] == "" {
		t.Error("an empty number was accepted")
	}
}

// Hosts are added at the end and removed by name; the names and identities
// of the hosts that stay do not move, and a removed number is not reused.
func TestAddAndRemoveHostsByName(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	key := "dc-01/cluster-01"
	q := map[string]string{"cluster": key}
	before := identities(c)
	out := must(t, c, "add-hosts", q, map[string]any{"n": 2})
	wantContains(t, out.Done, "cluster-01 now has 6 hosts.")
	if len(out.Marks) != 2 || out.Marks[1].ID != key+"/esx06" {
		t.Errorf("marks %+v", out.Marks)
	}
	wantContains(t, preview(t, c, "remove-hosts", q, map[string]any{"pick": []string{"esx02", "esx05"}}), "Removes **esx02 and esx05**")
	must(t, c, "remove-hosts", q, map[string]any{"pick": []string{"esx02", "esx05"}})
	cl, err := c.Cluster(key)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range cl.Hosts {
		names = append(names, h.Name)
	}
	if got := strings.Join(names, " "); got != "esx01 esx03 esx04 esx06" {
		t.Fatalf("hosts %s", got)
	}
	after := identities(c)
	for _, h := range []string{"esx01", "esx03", "esx04"} {
		if before[key+"/"+h] != after[key+"/"+h] {
			t.Errorf("%s changed identity", h)
		}
	}
	// the next host added is esx07, not a reused esx02
	must(t, c, "add-hosts", q, map[string]any{"n": 1})
	cl, _ = c.Cluster(key)
	if last := cl.Hosts[len(cl.Hosts)-1].Name; last != "esx07" {
		t.Errorf("new host is %s, want esx07", last)
	}
	// vSAN needs three hosts
	r := do(t, c, "remove-hosts", q, map[string]any{"pick": []string{"esx01", "esx03", "esx04"}})
	if r.Outcome != nil || !strings.Contains(r.Errors["pick"], "At least 3 hosts have to stay") {
		t.Errorf("removing down to two hosts: %+v", r)
	}
}

func TestRemoveClusterKeepsTheOthersAndTheLastOneStays(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	if _, err := c.RemoveCluster("dc-01/cluster-01"); err == nil || !strings.Contains(err.Error(), "only cluster") {
		t.Fatalf("removing the only cluster: %v", err)
	}
	must(t, c, "add-cluster", nil, map[string]any{"hosts": 3, "vms": 1})
	before := identities(c)
	done, err := c.RemoveCluster("dc-01/cluster-01")
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, done, "cluster-01 removed, with its 4 hosts.")
	after := identities(c)
	if len(after) >= len(before) {
		t.Fatal("nothing was removed")
	}
	for key, id := range after {
		if before[key] != id {
			t.Errorf("%s moved: %q -> %q", key, before[key], id)
		}
	}
}

func TestReplaceEverythingAsksFirstThenBuilds(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	form, err := c.TaskForm("replace-everything", nil, Caller{})
	if err != nil {
		t.Fatal(err)
	}
	form.Values["start"], form.Values["key"] = "tiny", "lab-b"
	r, err := c.TaskRun("replace-everything", params(t, form.Values), nil, false, Caller{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Confirm == nil || r.Outcome != nil || r.Confirm.OK != "Yes, replace everything" {
		t.Fatalf("no confirm before replacing everything: %+v", r)
	}
	before := identities(c)
	out := must(t, c, "replace-everything", nil, map[string]any{"start": "own", "dcs": 2, "per": 2, "hmin": 3, "hmax": 3, "vmin": 1, "vmax": 1, "key": "lab-b"})
	wantContains(t, out.Done, "New environment built: 4 clusters, 12 hosts and 12 VMs.")
	v := c.Clusters()
	if len(v.Rows) != 4 || len(v.Datacenters) != 2 || v.Rows[3].Key != "dc-02/cluster-02" {
		t.Fatalf("clusters %+v", v.Rows)
	}
	if identities(c)["dc-01/cluster-01"] == before["dc-01/cluster-01"] {
		t.Error("a new layout name kept the old identities")
	}
	r = do(t, c, "replace-everything", nil, map[string]any{"start": "own", "dcs": 1, "per": 1, "hmin": 2, "hmax": 1, "vmin": 0, "vmax": 0})
	if !strings.Contains(r.Errors["hmin"], "Use 3 to 64 hosts") {
		t.Errorf("errors %v", r.Errors)
	}
}

// Storage asks for a name and a size; the simulator invents the rest, and
// every later change finds the datastore by its name.
func TestStorageTasks(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	sum := preview(t, c, "add-iscsi", nil, nil)
	wantContains(t, sum, "Adds a **2 TB** iSCSI datastore called **iscsi-ds-1**, half full.")
	wantContains(t, sum, "All 4 hosts in **cluster-01** connect to it")
	out := must(t, c, "add-iscsi", nil, nil)
	wantContains(t, out.Done, "iSCSI datastore iscsi-ds-1 (2 TB) added for cluster-01. Every host in that cluster is connected to it.")
	out = must(t, c, "add-nfs", nil, nil)
	wantContains(t, out.Done, "NFS datastore nfs-ds-1 (4 TB) added for every cluster in dc-01.")

	st := c.Storage()
	if len(st.Rows) != 2 {
		t.Fatalf("storage %+v", st.Rows)
	}
	item, err := c.StorageItem("dc-01/iscsi-ds-1")
	if err != nil {
		t.Fatal(err)
	}
	if item.Size != "2 TB" || item.Free != "1 TB" || item.Full != "Half full" || len(item.Cut) != 0 || item.Details[0][1] != "10.0.50.21:3260" || !strings.HasPrefix(item.Details[1][1], "iqn.") {
		t.Errorf("item %+v", item)
	}
	// a second one gets its own name and address
	out = must(t, c, "add-iscsi", nil, nil)
	wantContains(t, out.Done, "iscsi-ds-2")
	r := do(t, c, "add-iscsi", nil, map[string]any{"name": "iscsi-ds-1"})
	wantContains(t, r.Errors["name"], "There is already storage called iscsi-ds-1")
	r = do(t, c, "add-nfs", nil, map[string]any{"name": "Bad Name"})
	wantContains(t, r.Errors["name"], "Use only small letters, numbers and hyphens")

	for _, id := range []string{"dc-01/iscsi-ds-1", "dc-01/nfs-ds-1"} {
		q := map[string]string{"id": id}
		must(t, c, "storage-disconnect", q, map[string]any{"pick": []string{"esx02"}})
		item, _ = c.StorageItem(id)
		if len(item.Cut) != 1 || item.Cut[0] != "esx02 in cluster-01" {
			t.Errorf("%s cut %v", id, item.Cut)
		}
		if _, err := c.ReconnectStorage(id); err != nil {
			t.Fatal(err)
		}
		if item, _ = c.StorageItem(id); len(item.Cut) != 0 {
			t.Errorf("%s still cut after reconnect: %v", id, item.Cut)
		}
		out := must(t, c, "storage-size", q, map[string]any{"size": 500, "unit": "GB", "full": "Nearly full"})
		wantContains(t, out.Done, "is now 500 GB, nearly full.")
		if item, _ = c.StorageItem(id); item.Size != "500 GB" || item.Full != "Nearly full" {
			t.Errorf("%s after resize: %+v", id, item)
		}
		if _, err := c.RemoveStorage(id); err != nil {
			t.Fatal(err)
		}
		if _, err := c.StorageItem(id); err == nil {
			t.Errorf("%s is still there", id)
		}
	}
	if cl, _ := c.Cluster("dc-01/cluster-01"); len(cl.Storage) != 1 || cl.Storage[0].Name != "iscsi-ds-2" {
		t.Errorf("cluster storage %+v", cl.Storage)
	}
}

func TestVCenterVersionAndTheHosts(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	before := identities(c)
	out := must(t, c, "vcenter-version", nil, map[string]any{"vc": "9.0", "hosts": "all"})
	wantContains(t, out.Done, "vCenter now says it is 9.0. 4 hosts moved to ESX 9.0 with it.")
	if v := c.Versions(); v.VCenter != "9.0" || len(v.ByESX) != 1 || v.ByESX[0].Label != "9.0" {
		t.Fatalf("versions %+v", v)
	}
	for key, id := range before {
		if identities(c)[key] != id {
			t.Errorf("%s changed identity with the version", key)
		}
	}
	// leaving the hosts where they are when vCenter goes down makes them
	// newer than vCenter, and the form says so before it happens
	form, _ := c.TaskForm("vcenter-version", nil, Caller{})
	form.Values["vc"], form.Values["hosts"] = "8.0u3", "none"
	p, err := c.TaskPreview("vcenter-version", params(t, form.Values), Caller{})
	if err != nil {
		t.Fatal(err)
	}
	opts := fmt.Sprint(p.Extra["hostOpts"])
	wantContains(t, opts, "Put all 4 hosts on ESX 8.0 Update 3 as well")
	wantContains(t, opts, "**4 hosts would then be newer than vCenter.**")
	out = must(t, c, "vcenter-version", nil, map[string]any{"vc": "8.0u3", "hosts": "none"})
	wantContains(t, out.Done, "No host was changed.")
	if v := c.Versions(); v.VCenter != "8.0 Update 3" || v.ByESX[0].Label != "9.0" {
		t.Errorf("versions %+v", v)
	}
	// a typed version gets a build and a product name
	out = must(t, c, "vcenter-version", nil, map[string]any{"vc": "other", "other": "9.2.0", "hosts": "all"})
	wantContains(t, out.Done, "vCenter now says it is 9.2.0. 4 hosts moved to ESX 9.2.0 with it.")
	vc := c.applied().topo.VCenterVersion
	if vc.Build == "" || !strings.Contains(vc.FullName, "9.2.0") || vc.APIVersion != "9.2.0.0" {
		t.Errorf("derived version %+v", vc)
	}
	r := do(t, c, "vcenter-version", nil, map[string]any{"vc": "other", "other": "nine"})
	wantContains(t, r.Errors["other"], "Type a version number like 9.2.0")
	r = do(t, c, "vcenter-version", nil, nil)
	wantContains(t, r.Errors["vc"], "vCenter is already 9.2.0.")
}

func TestMixedClusterVersions(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	q := map[string]string{"cluster": "dc-01/cluster-01"}
	set := map[string]any{"mode": "mixed", "ver": "9.1.1", "low": "8.0u3", "pick": []string{"esx01", "esx02"}}
	wantContains(t, preview(t, c, "cluster-versions", q, set), "**esx01 and esx02** run **ESX 8.0 Update 3**.")
	out := must(t, c, "cluster-versions", q, set)
	wantContains(t, out.Done, "cluster-01 is now mixed: esx01 and esx02 on ESX 8.0 Update 3, the rest on ESX 9.1.1.")
	v := c.Versions()
	if !v.Rows[0].Mixed || v.Rows[0].Label != "2 hosts on ESX 9.1.1, 2 on ESX 8.0 Update 3" {
		t.Errorf("row %+v", v.Rows[0])
	}
	r := do(t, c, "cluster-versions", q, map[string]any{"mode": "mixed", "ver": "9.1.1", "low": "9.1.1", "pick": []string{"esx01"}})
	wantContains(t, r.Errors["low"], "Both boxes say the same version.")
	r = do(t, c, "cluster-versions", q, map[string]any{"mode": "mixed", "ver": "9.1.1", "low": "9.0", "pick": []string{"esx01", "esx02", "esx03", "esx04"}})
	wantContains(t, r.Errors["pick"], "You ticked every host.")

	// hosts newer than vCenter need saying so twice
	must(t, c, "vcenter-version", nil, map[string]any{"vc": "9.0", "hosts": "all"})
	r = do(t, c, "cluster-versions", q, map[string]any{"mode": "same", "ver": "9.1.1"})
	wantContains(t, r.Errors["allow"], "tick this box to go ahead anyway")
	out = must(t, c, "cluster-versions", q, map[string]any{"mode": "same", "ver": "9.1.1", "allow": true})
	wantContains(t, out.Done, "Every host in cluster-01 now runs ESX 9.1.1.")
}

const soapEnv = `<?xml version="1.0" encoding="UTF-8"?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><soapenv:Body>%s</soapenv:Body></soapenv:Envelope>`

// apiClient is a stand-in tool: one address, one name, one cookie jar.
type apiClient struct {
	t     *testing.T
	srv   *httptest.Server
	hc    *http.Client
	agent string
}

func newTool(t *testing.T, c *Controller, agent string) *apiClient {
	return toolOn(t, httptest.NewServer(c), agent)
}

func toolOn(t *testing.T, srv *httptest.Server, agent string) *apiClient {
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &apiClient{t: t, srv: srv, hc: &http.Client{Jar: jar, Timeout: 20 * time.Second}, agent: agent}
}

func (a *apiClient) do(method, path, body string) (int, string) {
	a.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, a.srv.URL+path, rd)
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("User-Agent", a.agent)
	if body != "" {
		req.Header.Set("Content-Type", "text/xml")
	}
	res, err := a.hc.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func (a *apiClient) post(path, body string) (int, string) {
	return a.do(http.MethodPost, path, fmt.Sprintf(soapEnv, body))
}

func (a *apiClient) login(user, pass string) bool {
	a.t.Helper()
	st, r := a.post("/sdk", `<Login xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><userName>`+user+`</userName><password>`+pass+`</password></Login>`)
	return st == 200 && !strings.Contains(r, "Login failure")
}

func (a *apiClient) content() int {
	st, _ := a.post("/sdk", `<RetrieveServiceContent xmlns="urn:vim25"><_this type="ServiceInstance">ServiceInstance</_this></RetrieveServiceContent>`)
	return st
}

func (a *apiClient) perf(cluster, family string) (int, string) {
	now := time.Now().UTC()
	return a.post("/vsanHealth", fmt.Sprintf(`<VsanPerfQueryPerf xmlns="urn:vsan"><_this type="VsanPerformanceManager">vsan-performance-manager</_this><querySpecs><entityRefId>%s:*</entityRefId><startTime>%s</startTime><endTime>%s</endTime></querySpecs><cluster type="ClusterComputeResource">%s</cluster></VsanPerfQueryPerf>`,
		family, now.Add(-15*time.Minute).Format("2006-01-02T15:04:05.000Z"), now.Format("2006-01-02T15:04:05.000Z"), cluster))
}

func clusterRef(c *Controller) string { return c.applied().sim.Clusters[0].Ref.Value }

// lastCall is the newest request in the log.
func lastCall(c *Controller) activity.Call {
	all := c.Log.All()
	return all[len(all)-1]
}

func askOf(t *testing.T, c *Controller, title string) Ask {
	t.Helper()
	for _, a := range c.Requests().Asks {
		if strings.Contains(a.T, title) {
			return a
		}
	}
	t.Fatalf("no request row with %q in %+v", title, c.Requests().Asks)
	return Ask{}
}

// With no account supplied, nothing can sign in until first-run set-up
// chooses the password; there is no published default.
func TestFirstRunAccount(t *testing.T) {
	sc := scenario.Phase1("first")
	c := newCtl(t, sc, "", "t")
	if !c.AccountNeeded() || !c.Home().AccountNeeded {
		t.Fatal("a fresh simulator with no supplied account should need one")
	}
	tool := newTool(t, c, "tool/1")
	for _, pw := range []string{"VMware123!VMware123!", "VMware1!", "x"} {
		if tool.login(DefaultUsername, pw) {
			t.Fatalf("signed in with %q before any password was chosen", pw)
		}
	}
	if err := c.SetupAccount("short"); err == nil || !strings.Contains(err.Error(), "That is 5 characters. Use at least 8") {
		t.Fatalf("short password: %v", err)
	}
	if err := c.SetupAccount("chosen-password"); err != nil {
		t.Fatal(err)
	}
	if c.AccountNeeded() || !tool.login(DefaultUsername, "chosen-password") {
		t.Fatal("the chosen password does not sign in")
	}
	if got := c.Home().Connect; got.Username != DefaultUsername || got.Password != "inuse" {
		t.Errorf("connect %+v", got)
	}
	if err := c.SetupAccount("another-password"); err == nil {
		t.Error("set-up ran twice")
	}
}

// The account survives an edit of the scenario file when the console made it.
func TestConsoleAccountsSurviveAScenarioEdit(t *testing.T) {
	dir := t.TempDir()
	c := newCtl(t, scenario.Phase1("first"), dir, "hash-1")
	if err := c.SetupAccount("chosen-password"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c2 := newCtl(t, scenario.Phase1("first"), dir, "hash-2")
	if c2.AccountNeeded() {
		t.Fatal("the account was lost when the scenario file changed")
	}
	if !newTool(t, c2, "tool/1").login(DefaultUsername, "chosen-password") {
		t.Error("the chosen password no longer signs in")
	}
}

func TestUsers(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	admin := "administrator@vsphere.local"
	r := do(t, c, "add-user", nil, map[string]any{"pw": "short"})
	wantContains(t, r.Errors["pw"], `That is 5 characters. Use at least 8, or press "Make one for me".`)
	form, _ := c.TaskForm("add-user", nil, Caller{})
	if form.Values["name"] != "tool-1@vsphere.local" || form.Values["role"] != "read" {
		t.Errorf("new users should be suggested a name and start read-only: %+v", form.Values)
	}
	out := must(t, c, "add-user", nil, map[string]any{"pw": "collector-pw-1"})
	wantContains(t, out.Done, "tool-1@vsphere.local can now sign in with read-only access.")

	tool := newTool(t, c, "collector/2.0")
	if !tool.login("tool-1@vsphere.local", "collector-pw-1") || tool.content() != 200 {
		t.Fatal("the new user cannot sign in")
	}
	users := c.Users()
	var row UserRow
	for _, u := range users.Users {
		if u.Name == "tool-1@vsphere.local" {
			row = u
		}
	}
	if row.Role != "read" || row.Last == nil || len(row.InUse) != 1 {
		t.Fatalf("user row %+v", row)
	}

	// access changes without a new password; the old password still works
	q := map[string]string{"name": "tool-1@vsphere.local"}
	wantContains(t, preview(t, c, "change-user", q, map[string]any{"role": "full"}), "is signed in as this user right now.** It is signed out and signs in again by itself.")
	out = must(t, c, "change-user", q, map[string]any{"role": "full"})
	wantContains(t, out.Done, "tool-1@vsphere.local now has full access.")
	if !newTool(t, c, "other/1").login("tool-1@vsphere.local", "collector-pw-1") {
		t.Error("changing access changed the password")
	}
	r = do(t, c, "change-user", q, nil)
	wantContains(t, r.Errors["pw"], "Nothing has been changed.")

	// removing a user a tool is signed in as says so, and Home knows why
	if !tool.login("tool-1@vsphere.local", "collector-pw-1") || tool.content() != 200 {
		t.Fatal("sign in again")
	}
	done, tone, err := c.RemoveUser("tool-1@vsphere.local")
	if err != nil {
		t.Fatal(err)
	}
	if tone != "warn" || !strings.Contains(done, "was signed out** and is now being refused") {
		t.Errorf("remove said %q (%s)", done, tone)
	}
	home := c.Home()
	if len(home.Tools) == 0 || home.Tools[0].State != ToolRemoved {
		t.Fatalf("tools after removing its user: %+v", home.Tools)
	}
	found := false
	for _, a := range home.Attention {
		found = found || strings.Contains(a.T, "can no longer sign in")
	}
	if !found {
		t.Errorf("Home does not say the tool can no longer sign in: %+v", home.Attention)
	}
	if tool.login("tool-1@vsphere.local", "collector-pw-1") {
		t.Error("a removed user signed in")
	}

	// a tool whose sign-in ended and that keeps asking without signing in
	// again is not shown as connected
	other := newTool(t, c, "other/1")
	if !other.login(admin, "first-password") || other.content() != 200 {
		t.Fatal("sign in as the administrator")
	}
	must(t, c, "change-user", map[string]string{"name": admin}, map[string]any{"role": "read"})
	other.perf(clusterRef(c), "host-domclient") // refused: its session is gone
	var st string
	for _, tv := range c.Home().Tools {
		if tv.Agent == "other/1" {
			st = tv.State
		}
	}
	if st != ToolSignedOut {
		t.Errorf("a tool that is being refused for not being signed in is shown as %q", st)
	}
	if !other.login(admin, "first-password") {
		t.Fatal("sign in again")
	}
	for _, tv := range c.Home().Tools {
		if tv.Agent == "other/1" && tv.State != ToolConnected {
			t.Errorf("after signing in again it is %q", tv.State)
		}
	}
	wantContains(t, askOf(t, c, "which has been removed").Short, "The user was removed")

	if _, _, err := c.RemoveUser(admin); err == nil || !strings.Contains(err.Error(), "only user") {
		t.Errorf("removing the last user: %v", err)
	}
}

// One place knows whether a tool is connected, and every screen reads it.
func TestToolsAreTrackedAcrossPauseAndCertificate(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	tool := newTool(t, c, "collector/2.0")
	if !tool.login("administrator@vsphere.local", "first-password") || tool.content() != 200 {
		t.Fatal("sign in")
	}
	home := c.Home()
	if len(home.Tools) != 1 || home.Tools[0].State != ToolConnected || home.Tools[0].Agent != "collector/2.0" || home.Tools[0].User != "administrator@vsphere.local" {
		t.Fatalf("tools %+v", home.Tools)
	}
	c.SetPaused(true)
	if st := tool.content(); st != 0 {
		t.Errorf("a paused simulator answered with %d", st)
	}
	if got := c.Home().Tools[0].State; got != ToolPaused {
		t.Errorf("state while paused: %s", got)
	}
	if lastCall(c).Reason != activity.ReasonPaused {
		t.Errorf("a request dropped by a pause was recorded as %s", lastCall(c).Reason)
	}
	c.SetPaused(false)
	if got := c.Home().Tools[0].State; got != ToolConnected {
		t.Errorf("state after resuming: %s", got)
	}

	fp := c.Security().Certificate.Fingerprint
	out := must(t, c, "new-certificate", nil, nil)
	wantContains(t, out.Done, "New certificate made.")
	sec := c.Security().Certificate
	if sec.Fingerprint == fp || sec.Kind != "made" {
		t.Errorf("certificate did not change: %+v", sec)
	}
	home = c.Home()
	if home.Tools[0].State != ToolCert {
		t.Fatalf("after a new certificate the tool is %s", home.Tools[0].State)
	}
	if _, body := tool.perf(clusterRef(c), "host-domclient"); !strings.Contains(body, "NotAuthenticated") {
		t.Errorf("the tool's session survived the certificate change: %s", body)
	}
	// it comes back by signing in again, and the history says so
	if !tool.login("administrator@vsphere.local", "first-password") {
		t.Fatal("sign in again")
	}
	if got := c.Home().Tools[0].State; got != ToolConnected {
		t.Errorf("state after reconnecting: %s", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(history(c), "reconnected") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	wantContains(t, history(c), "The tool at 127.0.0.1 reconnected.")
}

func TestNewCertificateSuggestsNames(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	form, err := c.TaskForm("new-certificate", nil, Caller{Host: "console.lab.test:9090"})
	if err != nil {
		t.Fatal(err)
	}
	names := fmt.Sprint(form.Values["names"])
	for _, want := range []string{"sim.test", "console.lab.test", "127.0.0.1"} {
		wantContains(t, names, want)
	}
	r := do(t, c, "new-certificate", nil, map[string]any{"names": " "})
	wantContains(t, r.Errors["names"], "Enter at least one name or address")
}

func TestOwnCertificateIsCheckedBeforeItIsUsed(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	dir := t.TempDir()
	a, err := serve.LoadOrCreateCertificate(filepath.Join(dir, "a"), []string{"other.test"})
	if err != nil {
		t.Fatal(err)
	}
	_ = a
	crt, _ := os.ReadFile(filepath.Join(dir, "a", "cert.pem"))
	key, _ := os.ReadFile(filepath.Join(dir, "a", "key.pem"))
	if _, err := serve.LoadOrCreateCertificate(filepath.Join(dir, "b"), []string{"b.test"}); err != nil {
		t.Fatal(err)
	}
	wrongKey, _ := os.ReadFile(filepath.Join(dir, "b", "key.pem"))

	form, _ := c.TaskForm("own-certificate", nil, Caller{})
	form.Values["crt"], form.Values["key"] = string(crt), string(wrongKey)
	p, err := c.TaskPreview("own-certificate", params(t, form.Values), Caller{})
	if err != nil {
		t.Fatal(err)
	}
	found := p.Extra["found"].(map[string]any)
	if found["state"] != "bad" || !strings.Contains(fmt.Sprint(found["text"]), "valid for other.test") || !strings.Contains(fmt.Sprint(found["dropped"]), "It does not cover sim.test and 127.0.0.1") {
		t.Errorf("found %+v", found)
	}
	r := do(t, c, "own-certificate", nil, map[string]any{"crt": string(crt), "key": string(wrongKey)})
	wantContains(t, r.Errors["key"], "This key does not belong to the certificate.")
	r = do(t, c, "own-certificate", nil, map[string]any{"crt": "not a certificate", "key": string(key)})
	wantContains(t, r.Errors["crt"], "That file does not hold a certificate.")
	fp := c.Security().Certificate.Fingerprint
	must(t, c, "own-certificate", nil, map[string]any{"crt": string(crt), "key": string(key)})
	if got := c.Security().Certificate; got.Fingerprint == fp || got.Names[0] != "other.test" {
		t.Errorf("certificate %+v", got)
	}
}

func TestIdleSignOut(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	if c.Security().Idle != "" {
		t.Fatal("idle sign-out should start as never")
	}
	r := do(t, c, "idle", nil, nil)
	wantContains(t, r.Errors["idle"], "It is already set to never.")
	out := must(t, c, "idle", nil, map[string]any{"idle": "15 minutes"})
	wantContains(t, out.Done, "Tools are signed out after 15 minutes of no activity.")
	if c.Security().Idle != "15 minutes" {
		t.Errorf("idle %q", c.Security().Idle)
	}
}

// A fault takes effect when the button is pressed and stops when it is
// turned off, without a new vCenter; what failed stays explained.
func TestFaultsAreImmediateAndStayExplained(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	tool := newTool(t, c, "collector/2.0")
	if !tool.login("administrator@vsphere.local", "first-password") {
		t.Fatal("sign in")
	}
	running := c.applied().sim
	if st, _ := tool.perf(clusterRef(c), "vsan-pnic-net"); st != 200 {
		t.Fatalf("perf before the fault: %d", st)
	}
	wantContains(t, preview(t, c, "fault", map[string]string{"preset": "error"}, nil), "**Requests for statistics about host network cards come back with an error, on every cluster.**")
	out := must(t, c, "fault", map[string]string{"preset": "error"}, nil)
	wantContains(t, out.Done, "Switched on: requests for statistics about host network cards come back with an error, on every cluster.")
	if c.applied().sim != running {
		t.Fatal("switching a fault on built a new vCenter")
	}
	if st, body := tool.perf(clusterRef(c), "vsan-pnic-net"); st != 500 || !strings.Contains(body, "SystemError") {
		t.Fatalf("perf with the fault on: %d %s", st, body)
	}
	call := lastCall(c)
	if call.Reason != activity.ReasonFault || call.Cause == 0 {
		t.Fatalf("faulted call recorded as %+v", call)
	}
	if st, _ := tool.perf(clusterRef(c), "host-domclient"); st != 200 {
		t.Error("the fault reached another kind of data")
	}
	on := c.Faults()
	if len(on.On) != 1 || on.Ready[1].On != on.On[0].ID {
		t.Fatalf("faults view %+v", on)
	}
	ask := askOf(t, c, "vSAN performance data for host network cards")
	if ask.St != "fault" || ask.Cond != on.On[0].ID {
		t.Errorf("ask %+v", ask)
	}
	home := c.Home()
	if home.Faults != 1 || home.Attention[0].Act != "condOff" {
		t.Errorf("home %+v", home.Attention)
	}
	if v := c.Requests().Verdict; v.Tone != "warn" || !strings.Contains(v.Big, "except what you are failing on purpose") {
		t.Errorf("verdict %+v", v)
	}

	text, err := c.ConditionOff(on.On[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, text, "**Turned off.** Requests for statistics about host network cards are answered again. No slowness or faults are switched on now.")
	if st, _ := tool.perf(clusterRef(c), "vsan-pnic-net"); st != 200 {
		t.Error("the fault is still on")
	}
	// the hour's failed requests still say why they failed
	found := false
	for _, a := range c.Requests().Asks {
		if a.Label == "Answered now" && strings.Contains(a.Short, "Failed on purpose until {{time:") {
			found = true
		}
	}
	if !found {
		t.Errorf("no row explains the ended fault: %+v", c.Requests().Asks)
	}
	if v := c.Requests().Verdict; v.Tone != "ok" {
		t.Errorf("verdict after the fault ended: %+v", v)
	}
}

func TestPerformanceServiceOffAndSlowness(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	tool := newTool(t, c, "collector/2.0")
	if !tool.login("administrator@vsphere.local", "first-password") {
		t.Fatal("sign in")
	}
	running := c.applied().sim
	must(t, c, "fault", map[string]string{"preset": "perfdown"}, nil)
	if c.applied().sim != running {
		t.Fatal("switching the performance service off built a new vCenter")
	}
	if st, body := tool.perf(clusterRef(c), "host-domclient"); st != 500 || !strings.Contains(body, "Stats primary") {
		t.Fatalf("perf with the service off: %d %s", st, body)
	}
	if call := lastCall(c); call.Reason != activity.ReasonFault || call.Cause == 0 {
		t.Errorf("recorded as %+v", call)
	}
	wantContains(t, c.Faults().On[0].Text, "The vSAN performance service is switched off, on cluster-01.")
	if _, err := c.ConditionOff(0); err != nil {
		t.Fatal(err)
	}
	if st, _ := tool.perf(clusterRef(c), "host-domclient"); st != 200 {
		t.Error("the performance service did not come back")
	}

	must(t, c, "fault", map[string]string{"preset": "slow"}, map[string]any{"ms": 300, "jitter": 0})
	start := time.Now()
	tool.perf(clusterRef(c), "host-domclient")
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Errorf("a slow answer took %s", took)
	}
	r := do(t, c, "fault", map[string]string{"preset": "slow"}, map[string]any{"ms": 0})
	wantContains(t, r.Errors["ms"], "Enter how much slower, from 1 to 60,000 milliseconds.")
	r = do(t, c, "fault", nil, map[string]any{"kind": "error", "where": "some", "pick": []string{}})
	wantContains(t, r.Errors["pick"], "Tick at least one cluster")
}

func TestTimedFaultTurnsItselfOff(t *testing.T) {
	ck := &clock{t: time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)}
	c := newCtlAt(t, small(t), "", "t", ck.now)
	must(t, c, "fault", map[string]string{"preset": "timeout"}, map[string]any{"until": "15m"})
	on := c.Faults().On
	if len(on) != 1 || on[0].Until == nil || !on[0].Until.Equal(ck.now().Add(15*time.Minute)) {
		t.Fatalf("on %+v", on)
	}
	ck.add(16 * time.Minute)
	if on = c.Faults().On; len(on) != 0 {
		t.Fatalf("still on after its time: %+v", on)
	}
	wantContains(t, history(c), "A fault turned itself off. Requests for all vSAN performance data are answered again.")
	if c.Home().Faults != 0 {
		t.Error("Home still counts the fault")
	}
}

// The scenario file's own fault rules, latency and statsPrimary: false
// become conditions, so the console lists everything that is on.
func TestScenarioFaultsBecomeConditions(t *testing.T) {
	sc, err := scenario.Load("../../examples/scenarios/faults-and-latency.yaml")
	if err != nil {
		t.Fatal(err)
	}
	off := false
	sc.Clusters[0].StatsPrimary = &off
	c := newCtl(t, sc, "", "t")
	r := c.applied()
	if len(r.sc.Faults) != 0 || r.sc.Latency.PerfQuery != nil || r.sc.Latency.SDK != nil || len(r.sc.Latency.Families) != 0 {
		t.Errorf("faults and latency were left in the scenario: %+v %+v", r.sc.Faults, r.sc.Latency)
	}
	var texts []string
	for _, v := range c.Faults().On {
		texts = append(texts, v.Text)
	}
	all := strings.Join(texts, "\n")
	wantContains(t, all, "The simulator says statistics about host network cards are not available, on cluster-03.")
	wantContains(t, all, "The vSAN performance service is switched off, on cluster-01.")
	wantContains(t, all, "Requests for lists of clusters, hosts and VMs take about")
	if r.topo.Clusters()[0].PrimaryOnline() {
		t.Error("cluster-01's performance service should be off")
	}
	if !r.topo.Clusters()[1].PrimaryOnline() {
		t.Error("cluster-02's performance service should be on")
	}
}

// A scheduled change waits without locking anything; it is planned again
// when its time comes, so one that no longer makes sense fails cleanly.
func TestSchedulingWithoutTheLock(t *testing.T) {
	ck := &clock{t: time.Date(2026, 11, 12, 14, 31, 0, 0, time.UTC)}
	c := newCtlAt(t, small(t), "", "t", ck.now)
	must(t, c, "add-cluster", nil, map[string]any{"hosts": 3, "vms": 1})
	at := ck.now().Add(4 * time.Minute)
	form, _ := c.TaskForm("add-hosts", map[string]string{"cluster": "dc-01/cluster-02"}, Caller{})
	r, err := c.TaskRun("add-hosts", params(t, form.Values), &at, false, Caller{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Scheduled == nil || r.Scheduled.Text != "Add 2 hosts to cluster-02" || !r.Scheduled.At.Equal(at) {
		t.Fatalf("scheduled %+v", r)
	}
	if cl, _ := c.Cluster("dc-01/cluster-02"); len(cl.Hosts) != 3 {
		t.Fatal("a scheduled change was made at once")
	}
	if home := c.Home(); len(home.Scheduled) != 1 {
		t.Fatalf("Home does not list it: %+v", home.Scheduled)
	}
	wantContains(t, history(c), "Scheduled for {{time:2026-11-12T14:35:00Z}}: add 2 hosts to cluster-02.")

	// other changes are still possible
	must(t, c, "add-hosts", map[string]string{"cluster": "dc-01/cluster-01"}, map[string]any{"n": 1})

	// a second one, then cancel it
	later := ck.now().Add(9 * time.Minute)
	r, err = c.TaskRun("idle", params(t, map[string]any{"idle": "1 hour"}), &later, false, Caller{})
	if err != nil || r.Scheduled == nil {
		t.Fatalf("second schedule: %+v %v", r, err)
	}
	text, err := c.ScheduledCancel(r.Scheduled.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, text, `**Cancelled.** "Change the idle sign-out to 1 hour" will not happen. Nothing was changed.`)
	if c.Security().Idle != "" {
		t.Error("a cancelled change was made")
	}

	// "Do it now" makes the first one
	id := c.Home().Scheduled[0].ID
	now, err := c.ScheduledNow(id)
	if err != nil || now.Outcome == nil {
		t.Fatalf("do it now: %+v %v", now, err)
	}
	if cl, _ := c.Cluster("dc-01/cluster-02"); len(cl.Hosts) != 5 {
		t.Errorf("cluster-02 has %d hosts", len(cl.Hosts))
	}
	if len(c.Home().Scheduled) != 0 {
		t.Error("it is still listed as scheduled")
	}

	// a change scheduled for a cluster that is then removed fails cleanly,
	// with its own values kept for "Try again"
	r, err = c.TaskRun("add-hosts", params(t, form.Values), &at, false, Caller{})
	if err != nil || r.Scheduled == nil {
		t.Fatal("schedule again")
	}
	if _, err := c.RemoveCluster("dc-01/cluster-02"); err != nil {
		t.Fatal(err)
	}
	res, err := c.ScheduledNow(r.Scheduled.ID)
	if err != nil || !res.Failed {
		t.Fatalf("a change to a removed cluster: %+v %v", res, err)
	}
	f := c.Home().Failure
	if f == nil || f.What != `The change "Add 2 hosts to cluster-02" was not made.` || !strings.Contains(f.Why, "There is no cluster called cluster-02") || f.Kind != "add-hosts" || !strings.Contains(string(f.Params), "dc-01/cluster-02") {
		t.Fatalf("failure %+v", f)
	}
	wantContains(t, history(c), "Failed: add 2 hosts to cluster-02.")
	c.DismissFailure()
	if c.Home().Failure != nil {
		t.Error("the failure bar was not hidden")
	}

	// a time that has passed is refused
	past := ck.now().Add(-time.Minute)
	r, _ = c.TaskRun("idle", params(t, map[string]any{"idle": "1 hour"}), &past, false, Caller{})
	if r.Errors["at"] == "" {
		t.Error("a time in the past was accepted")
	}
}

func TestScheduledChangeFiresOnItsOwn(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	at := time.Now().Add(300 * time.Millisecond)
	r, err := c.TaskRun("idle", params(t, map[string]any{"idle": "8 hours"}), &at, false, Caller{})
	if err != nil || r.Scheduled == nil {
		t.Fatalf("%+v %v", r, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.Security().Idle == "" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if c.Security().Idle != "8 hours" {
		t.Fatal("the scheduled change did not happen")
	}
	if len(c.Home().Scheduled) != 0 {
		t.Error("it is still listed")
	}
}

func TestFailureKeepsNoSecrets(t *testing.T) {
	got := string(scrub(json.RawMessage(`{"name":"a","pw":"secret-1","key":"-----BEGIN","cur":"x","crt":"y","role":"read"}`)))
	for _, bad := range []string{"secret-1", "BEGIN", `"cur"`, `"crt"`} {
		if strings.Contains(got, bad) {
			t.Errorf("kept %s in %s", bad, got)
		}
	}
	wantContains(t, got, `"name":"a"`)
}

// What was changed survives a restart; what was only scheduled does not,
// and the history says so.
func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	c := newCtl(t, small(t), dir, "h")
	must(t, c, "add-hosts", map[string]string{"cluster": "dc-01/cluster-01"}, map[string]any{"n": 1})
	must(t, c, "remove-hosts", map[string]string{"cluster": "dc-01/cluster-01"}, map[string]any{"pick": []string{"esx02"}})
	must(t, c, "add-nfs", nil, nil)
	must(t, c, "fault", map[string]string{"preset": "slow"}, nil)
	at := time.Now().Add(time.Hour)
	if r, err := c.TaskRun("idle", params(t, map[string]any{"idle": "1 day"}), &at, false, Caller{}); err != nil || r.Scheduled == nil {
		t.Fatal("schedule")
	}
	before := identities(c)
	c.Close()

	c2 := newCtl(t, small(t), dir, "h")
	after := identities(c2)
	if len(after) != len(before) {
		t.Fatalf("inventory changed across the restart: %d -> %d", len(before), len(after))
	}
	for key, id := range before {
		if after[key] != id {
			t.Errorf("%s changed identity across the restart", key)
		}
	}
	if len(c2.Storage().Rows) != 1 || len(c2.Faults().On) != 1 {
		t.Errorf("storage %d, faults %d", len(c2.Storage().Rows), len(c2.Faults().On))
	}
	if len(c2.Home().Scheduled) != 0 {
		t.Error("a scheduled change survived the restart")
	}
	wantContains(t, history(c2), "(change the idle sign-out to 1 day) was dropped because the simulator restarted.")
	wantContains(t, history(c2), "Simulator started with 1 cluster, 4 hosts and 4 VMs.")
}

// The Requests headline counts only what the simulator truly cannot answer.
func TestRequestsSayWhyEachWasAnsweredAsItWas(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	capture, err := querylog.NewCapture(t.TempDir(), "", querylog.Settings{Unanswered: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	c.Capture = capture
	tool := toolOn(t, httptest.NewServer(capture.Wrap(c)), "collector/2.0")
	stranger := toolOn(t, httptest.NewServer(capture.Wrap(c)), "curl/8")
	if v := c.Requests().Verdict; v.Tone != "none" {
		t.Fatalf("verdict before anything connected: %+v", v)
	}
	if stranger.login("administrator@vsphere.local", "wrong-password") {
		t.Fatal("a wrong password signed in")
	}
	if !tool.login("administrator@vsphere.local", "first-password") {
		t.Fatal("sign in")
	}
	ref := clusterRef(c)
	tool.content()
	tool.perf(ref, "host-domclient") // data
	tool.perf(ref, "cache-disk")     // "there are none" on an ESA cluster
	tool.perf(ref, "clom-disk")      // refused by a real vCenter too
	tool.perf(ref, "cluster-pmem")   // likewise
	tool.do(http.MethodPost, "/ls/sdk", fmt.Sprintf(soapEnv, `<QueryAssignedLicenses xmlns="urn:vim25"><_this type="LicenseAssignmentManager">LicenseAssignmentManager</_this></QueryAssignedLicenses>`))
	tool.post("/sdk", `<QueryPerfCounterByLevel xmlns="urn:vim25"><_this type="PerformanceManager">PerfMgr</_this><level>1</level></QueryPerfCounterByLevel>`)
	tool.do(http.MethodGet, "/rest/vcenter/namespace-management/clusters", "")
	tool.do(http.MethodGet, "/sdk/vim.wsdl", "")
	tool.do(http.MethodGet, "/sdk/vimServiceVersions.xml", "")
	tool.content() // it carries on

	v := c.Requests()
	want := map[string]string{
		"Which license this vCenter has":                                  "unknown",
		"The list of measurements vCenter can report":                     "unknown",
		"Whether Kubernetes is turned on for any cluster":                 "unknown",
		"The file that describes how to talk to vCenter":                  "unknown",
		"Sign-in with a wrong password, from 127.0.0.1":                   "denied",
		"Signing in and out":                                              "ok",
		"vSAN performance data for host disk activity":                    "ok",
		"vSAN performance data for cache disks":                           "ok",
		"vSAN performance data that a real vCenter also refuses, 2 kinds": "ok",
		"Which versions of its interface vCenter offers":                  "ok",
	}
	got := map[string]string{}
	for _, a := range v.Asks {
		got[a.T] = a.St
	}
	for title, st := range want {
		if got[title] != st {
			t.Errorf("%q is %q, want %q (all: %v)", title, got[title], st, got)
		}
	}
	if v.Counts["unknown"] != 4 || v.Counts["denied"] != 1 || v.Counts["fault"] != 0 {
		t.Errorf("counts %v", v.Counts)
	}
	if v.Asks[0].St != "unknown" {
		t.Error("problems are not listed first")
	}
	if v.Verdict.Tone != "bad" || v.Verdict.Big != "4 kinds of request are not being answered." || !strings.Contains(v.Verdict.Line, `The tool at 127.0.0.1 ("collector/2.0") is connected. It carried on collecting without them.`) {
		t.Errorf("verdict %+v", v.Verdict)
	}
	wantContains(t, askOf(t, c, "Which license").Short, "The tool carried on. VCF Operations' own adapter would stop")
	wantContains(t, askOf(t, c, "cache disks").Short, `The answer was "there are none"`)
	wantContains(t, askOf(t, c, "also refuses").Short, `as on a real vCenter`)

	// capture holds only the four the simulator has no answer for
	if cv := v.Capture; cv.Kept != 4 || cv.Kinds != 4 || !cv.Unanswered {
		t.Errorf("capture %+v", cv)
	}
	home := c.Home()
	if home.Unanswered != 4 {
		t.Errorf("Home counts %d unanswered", home.Unanswered)
	}
	var titles []string
	for _, a := range home.Attention {
		titles = append(titles, a.T+" / "+a.D)
	}
	all := strings.Join(titles, "\n")
	wantContains(t, all, "4 kinds of request are not being answered")
	wantContains(t, all, "One is the license, which would stop a different tool")
	wantContains(t, all, "Someone else tried a wrong password 1 time")
	if len(home.Tools) != 1 || home.Tools[0].Unknown != 4 || !home.Tools[0].Carried {
		t.Errorf("tools %+v", home.Tools)
	}

	// turning capture off keeps what was captured; deleting is the one
	// thing that discards it
	text, err := c.SetCapture("unanswered", false)
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, text, "The 4 already captured are kept")
	if c.Requests().Capture.Kept != 4 {
		t.Error("turning capture off threw the captured requests away")
	}
	if _, err := c.ClearCapture(); err != nil {
		t.Fatal(err)
	}
	if c.Requests().Capture.Kept != 0 {
		t.Error("delete did not delete")
	}

	// every request, one at a time
	log := c.RequestLog()
	if len(log) < 10 || log[0].T != "Signing in and out" {
		t.Errorf("log %+v", log[:min(3, len(log))])
	}
	var seq uint64
	for _, row := range log {
		if strings.Contains(row.T, "host disk activity") {
			seq = row.Seq
		}
	}
	sent, err := c.RequestBody(seq)
	if err != nil || !strings.Contains(sent.Body, "host-domclient:*") || sent.Line != "POST /vsanHealth" {
		t.Errorf("sent %+v %v", sent, err)
	}
	for _, row := range log {
		if row.Tech == "Login" {
			if s, _ := c.RequestBody(row.Seq); s.Body != "" || !strings.Contains(s.Note, "sign-in") {
				t.Errorf("a sign-in kept its text: %+v", s)
			}
		}
	}
}

var hostRefRe = regexp.MustCompile(`host-[0-9]+`)

// Every request in the live captures was answered by a real vCenter, so the
// simulator must never call one of them "not answered". This is the check
// that keeps the Requests headline honest.
func TestCapturedRequestsAreNeverCalledUnanswered(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	tool := newTool(t, c, "capture-replay")
	if !tool.login("administrator@vsphere.local", "first-password") {
		t.Fatal("sign in")
	}
	refs := c.applied().sim.Clusters[0]
	files, _ := filepath.Glob("../../fixtures/live/9.1.1/*.req.xml")
	if len(files) < 50 {
		t.Fatalf("only %d captured requests found", len(files))
	}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(file)
		if strings.HasPrefix(name, "nocookie-") {
			continue
		}
		req := strings.ReplaceAll(string(b), "domain-c6004", refs.Ref.Value)
		req = hostRefRe.ReplaceAllString(req, refs.Hosts[0].Value)
		path := "/vsanHealth"
		if strings.Contains(req, "urn:vim25") && !strings.Contains(req, "urn:vsan") {
			path = "/sdk"
		}
		before := len(c.Log.All())
		tool.do(http.MethodPost, path, req)
		calls := c.Log.All()
		if len(calls) != before+1 {
			t.Fatalf("%s: not recorded", name)
		}
		if call := calls[len(calls)-1]; call.Reason == activity.ReasonUnknown || call.Reason == activity.ReasonDenied {
			t.Errorf("%s: a request a real vCenter answered is recorded as %q (fault %q, status %d)", name, call.Reason, call.Fault, call.Status)
		}
	}
	// the one captured without a session is a refused sign-in, not a gap
	b, _ := os.ReadFile("../../fixtures/live/9.1.1/nocookie-vsanHealth.req.xml")
	stranger := newTool(t, c, "no-session")
	stranger.do(http.MethodPost, "/vsanHealth", strings.ReplaceAll(string(b), "domain-c6004", refs.Ref.Value))
	if call := lastCall(c); call.Reason != activity.ReasonDenied {
		t.Errorf("a request without a session is recorded as %q", call.Reason)
	}
	if n := c.Requests().Counts["unknown"]; n != 0 {
		t.Errorf("%d kinds of captured request are called not answered", n)
	}
}

func TestStripRemovesSecrets(t *testing.T) {
	sc := tenClusters(t)
	sc.TLS.CertFile, sc.TLS.KeyFile = "/secret/cert.pem", "/secret/key.pem"
	b, _ := json.Marshal(Strip(sc))
	for _, bad := range []string{"VMware1!", "/secret/", "password"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("stripped scenario still has %q", bad)
		}
	}
}

func TestAddressAndItsCheck(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	if got := c.Address(); got != "https://sim.test:8443" {
		t.Errorf("address %q", got)
	}
	c.opts.PublicURL = "https://vcsim.lab.example/"
	if got := c.Home().Address; got != "https://vcsim.lab.example" {
		t.Errorf("address %q", got)
	}
	// nothing listens at that name from here, and the check says so plainly
	c.opts.PublicURL = "https://127.0.0.1:1"
	c.opts.APIAddrs = []string{"127.0.0.1:1"}
	ok, text := c.CheckAddress()
	if ok || !strings.Contains(text, "did not answer") {
		t.Errorf("check: %v %q", ok, text)
	}
}

func TestHistoryIsPlainSentences(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	must(t, c, "add-cluster", nil, map[string]any{"hosts": 3})
	c.SetPaused(true)
	c.SetPaused(false)
	h := history(c)
	for _, want := range []string{"Simulator resumed.", "Simulator paused.", "cluster-02 added to dc-01 with 3 hosts and 24 VMs.", "Simulator started with 1 cluster, 4 hosts and 4 VMs."} {
		wantContains(t, h, want)
	}
	if home := c.Home(); len(home.Recent) != 3 || home.HistoryCount != 4 || home.Recent[0].Text != "Simulator resumed." {
		t.Errorf("recent %+v", home.Recent)
	}
}
