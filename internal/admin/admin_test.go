package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/control"
	"github.com/sentania/vcenter-api-simulator/internal/querylog"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

func newAdmin(t *testing.T) (*httptest.Server, *control.Controller) {
	t.Helper()
	sc, err := scenario.Load("../../examples/scenarios/ten-clusters.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := control.New(control.Options{Scenario: sc, SourceHash: "t", DataDir: t.TempDir(), Endpoint: "sim.test", Version: "test",
		APIAddrs: []string{"127.0.0.1:8443"}, AdminAddr: "127.0.0.1:9090"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	srv := httptest.NewServer(Handler(c, []string{"console.example.net"}))
	t.Cleanup(srv.Close)
	return srv, c
}

func do(t *testing.T, method, url, ctype, body string, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

var reads = []string{"/api/home", "/api/history", "/api/clusters", "/api/clusters/dc-01/cluster-03", "/api/storage", "/api/versions", "/api/users", "/api/security",
	"/api/faults", "/api/requests", "/api/requests/log", "/api/requests/supported",
	"/api/task/add-cluster", "/api/task/replace-everything", "/api/task/add-hosts?cluster=dc-01/cluster-03", "/api/task/remove-hosts?cluster=dc-01/cluster-03",
	"/api/task/hardware?cluster=dc-01/cluster-03", "/api/task/add-iscsi", "/api/task/add-nfs", "/api/task/vcenter-version", "/api/task/cluster-versions?cluster=dc-01/cluster-03",
	"/api/task/add-user", "/api/task/change-user?name=administrator@vsphere.local", "/api/task/idle", "/api/task/fault?preset=slow", "/api/task/fault"}

func TestReadEndpoints(t *testing.T) {
	srv, _ := newAdmin(t)
	for _, p := range reads {
		code, b := do(t, "GET", srv.URL+p, "", "", nil)
		if code != 200 || !json.Valid(b) {
			t.Errorf("GET %s: %d %.200s", p, code, b)
		}
	}
	for _, p := range []string{"/api/clusters/dc-01/nope", "/api/storage/dc-01/nope", "/api/task/nope", "/api/task/add-hosts?cluster=dc-01/nope"} {
		code, b := do(t, "GET", srv.URL+p, "", "", nil)
		var e struct{ Error string }
		if code != http.StatusUnprocessableEntity || json.Unmarshal(b, &e) != nil || e.Error == "" {
			t.Errorf("GET %s: %d %s", p, code, b)
		}
	}
	code, b := do(t, "GET", srv.URL+"/", "", "", nil)
	if code != 200 || !strings.Contains(string(b), "<title>vCenter Simulator</title>") {
		t.Errorf("index: %d", code)
	}
	for _, f := range []string{"/js/app.js", "/js/task.js", "/js/home.js", "/lattice/tokens.css", "/lattice/lattice.css", "/app.css", "/login", "/claim"} {
		if code, _ := do(t, "GET", srv.URL+f, "", "", nil); code != 200 {
			t.Errorf("GET %s: %d", f, code)
		}
	}
}

func TestNothingCarriesCredentials(t *testing.T) {
	srv, _ := newAdmin(t)
	for _, p := range reads {
		_, b := do(t, "GET", srv.URL+p, "", "", nil)
		if strings.Contains(string(b), "VMware1!") {
			t.Errorf("%s leaks a password", p)
		}
	}
}

func TestWritesAreGuarded(t *testing.T) {
	srv, _ := newAdmin(t)
	j := "application/json"
	// a form post (what a cross-site page can send without a preflight) is refused
	if code, _ := do(t, "POST", srv.URL+"/api/pause", "text/plain", `{"paused":true}`, nil); code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", code)
	}
	// a cross-origin page is refused even with JSON
	if code, _ := do(t, "POST", srv.URL+"/api/pause", j, `{"paused":true}`, map[string]string{"Origin": "http://evil.example"}); code != http.StatusForbidden {
		t.Errorf("cross origin: %d", code)
	}
	// an empty body needs the console's header
	if code, _ := do(t, "POST", srv.URL+"/api/failure/dismiss", "", "", nil); code != http.StatusForbidden {
		t.Errorf("no body, no header: %d", code)
	}
	if code, _ := do(t, "DELETE", srv.URL+"/api/faults/0", "", "", nil); code != http.StatusForbidden {
		t.Errorf("delete with no body and no header: %d", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/api/pause", j, `{"paused":true}`, map[string]string{"Origin": srv.URL}); code != 200 {
		t.Errorf("same origin: %d", code)
	}
}

// A change over HTTP: what it would do, what is wrong with it, making it,
// scheduling it and cancelling it.
func TestTaskOverHTTP(t *testing.T) {
	srv, c := newAdmin(t)
	j := "application/json"
	post := func(path, body string) (int, map[string]any) {
		t.Helper()
		code, b := do(t, "POST", srv.URL+path, j, body, nil)
		var v map[string]any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatalf("POST %s: %d %s", path, code, b)
		}
		return code, v
	}
	good := `{"dc":"dc-01","hosts":4,"vms":8,"vsan":"ESA","count":1}`
	code, v := post("/api/task/add-cluster/preview", `{"params":`+good+`}`)
	if code != 200 || !strings.Contains(v["sum"].([]any)[0].(map[string]any)["t"].(string), "Adds **cluster-11** to dc-01") || v["go"] != "Add cluster" {
		t.Fatalf("preview: %d %v", code, v)
	}
	code, v = post("/api/task/add-cluster", `{"params":{"dc":"dc-01","hosts":"1","vms":8,"vsan":"ESA","count":1}}`)
	if code != 200 || v["errors"].(map[string]any)["hosts"] != "Enter a number of hosts from 3 to 64." {
		t.Fatalf("invalid change: %d %v", code, v)
	}
	if len(c.Clusters().Rows) != 10 {
		t.Fatal("an invalid change was made")
	}
	code, v = post("/api/task/add-cluster", `{"params":`+good+`}`)
	if code != 200 || !strings.Contains(v["outcome"].(map[string]any)["done"].(string), "cluster-11 added to dc-01 with 4 hosts and 32 VMs. It is live now.") || v["dest"] != "/clusters" {
		t.Fatalf("change: %d %v", code, v)
	}
	at := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	code, v = post("/api/task/idle", `{"params":{"idle":"1 hour"},"at":"`+at+`"}`)
	if code != 200 || v["scheduled"] == nil {
		t.Fatalf("schedule: %d %v", code, v)
	}
	id := int(v["scheduled"].(map[string]any)["id"].(float64))
	_, b := do(t, "GET", srv.URL+"/api/home", "", "", nil)
	if !strings.Contains(string(b), `"text":"Change the idle sign-out to 1 hour"`) {
		t.Errorf("Home does not list the scheduled change: %.300s", b)
	}
	code, b = do(t, "DELETE", srv.URL+"/api/scheduled/"+itoa(id), "", "", map[string]string{"X-Console": "1"})
	if code != 200 || !strings.Contains(string(b), "Cancelled.") {
		t.Errorf("cancel: %d %s", code, b)
	}
	code, b = do(t, "DELETE", srv.URL+"/api/scheduled/"+itoa(id), "", "", map[string]string{"X-Console": "1"})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "no longer scheduled") {
		t.Errorf("cancel twice: %d %s", code, b)
	}
	code, b = do(t, "DELETE", srv.URL+"/api/faults/999", "", "", map[string]string{"X-Console": "1"})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "not switched on") {
		t.Errorf("turning off nothing: %d %s", code, b)
	}
	code, b = do(t, "GET", srv.URL+"/api/requests/log.csv", "", "", nil)
	if code != 200 || !strings.HasPrefix(string(b), "Time,Asked by,What was asked for") {
		t.Errorf("spreadsheet: %d %.100s", code, b)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestHostHeaderIsValidated(t *testing.T) {
	srv, _ := newAdmin(t)
	j := "application/json"
	host := func(method, path, ctype, body, h string) int {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Host = h
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for _, h := range []string{"evil.example", "evil.example:9090", "127.0.0.1.evil.example", "console.example.net.evil.example"} {
		if code := host("GET", "/api/home", "", "", h); code != http.StatusForbidden {
			t.Errorf("read with Host %q: %d, want 403", h, code)
		}
		if code := host("GET", "/", "", "", h); code != http.StatusForbidden {
			t.Errorf("UI with Host %q: %d, want 403", h, code)
		}
		if code := host("POST", "/api/pause", j, `{"paused":true}`, h); code != http.StatusForbidden {
			t.Errorf("write with Host %q: %d, want 403", h, code)
		}
	}
	for _, h := range []string{"localhost", "localhost:9090", "LOCALHOST:9090", "127.0.0.1", "127.0.0.1:9090", "[::1]", "[::1]:9090", "console.example.net", "console.example.net:8080"} {
		if code := host("GET", "/api/home", "", "", h); code != 200 {
			t.Errorf("read with Host %q: %d, want 200", h, code)
		}
		if code := host("POST", "/api/pause", j, `{"paused":false}`, h); code != 200 {
			t.Errorf("write with Host %q: %d, want 200", h, code)
		}
	}
}

func TestConsoleRefusesFraming(t *testing.T) {
	srv, _ := newAdmin(t)
	for _, path := range []string{"/", "/api/home"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("GET %s: %d", path, res.StatusCode)
		}
		if got := res.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s X-Frame-Options = %q, want DENY", path, got)
		}
		if got := res.Header.Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
			t.Errorf("%s Content-Security-Policy = %q", path, got)
		}
	}
}

// The console's screens are drawn in JavaScript; their tests run with node
// when node is installed.
func TestConsoleScriptTests(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	files, _ := filepath.Glob("ui/js/*.test.mjs")
	if len(files) == 0 {
		t.Fatal("no console script tests found")
	}
	cmd := exec.Command(node, append([]string{"--test"}, files...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}

// client is a browser signed in to a protected console.
type client struct {
	t      *testing.T
	srv    *httptest.Server
	cookie *http.Cookie
}

func (cl *client) call(method, path, body string) (int, string) {
	cl.t.Helper()
	r, _ := http.NewRequest(method, cl.srv.URL+path, strings.NewReader(body))
	if cl.cookie != nil {
		r.AddCookie(cl.cookie)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	} else if method != "GET" {
		r.Header.Set("X-Console", "1")
	}
	out, err := http.DefaultClient.Do(r)
	if err != nil {
		cl.t.Fatal(err)
	}
	defer out.Body.Close()
	b, _ := io.ReadAll(out.Body)
	if len(out.Cookies()) > 0 && out.Cookies()[0].Value != "" {
		cl.cookie = out.Cookies()[0]
	}
	return out.StatusCode, string(b)
}

func protectedConsole(t *testing.T, sc *scenario.Scenario, seed string) (*client, *control.Controller, *querylog.Capture) {
	t.Helper()
	dir := t.TempDir()
	c, err := control.New(control.Options{Scenario: sc, SourceHash: "p", DataDir: dir, APIAddrs: []string{"127.0.0.1:8443"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	capture, err := querylog.NewCapture(dir, "", querylog.Settings{Unanswered: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Capture = capture
	protected, err := ProtectedHandler(c, nil, dir, seed, false)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(protected)
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}, c, capture
}

func withUser(t *testing.T) *scenario.Scenario {
	sc := scenario.Phase1("console")
	sc.Auth.Users = []scenario.User{{Name: "administrator@vsphere.local", Password: "first-password"}}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestProtectedCaptureAPI(t *testing.T) {
	password := testPassword()
	cl, _, capture := protectedConsole(t, withUser(t), password)
	if code, _ := cl.call("GET", "/api/requests", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", code)
	}
	body, _ := json.Marshal(map[string]string{"password": password})
	if code, _ := cl.call("POST", "/api/auth/login", string(body)); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	code, out := cl.call("PUT", "/api/capture", `{"kind":"full","on":true}`)
	if code != http.StatusOK || !strings.Contains(out, "Capturing.") {
		t.Fatalf("turn full capture on: %d %s", code, out)
	}
	capture.Wrap(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/ls/sdk", strings.NewReader(`<Envelope><Body><Fetch><_this type="PropertyCollector">pc</_this><pathSet>config.product</pathSet></Fetch></Body></Envelope>`)))
	for _, kind := range []string{"unanswered", "all"} {
		code, out = cl.call("GET", "/api/capture/"+kind, "")
		if code != http.StatusOK || !strings.Contains(out, "config.product") {
			t.Fatalf("download %s: %d %s", kind, code, out)
		}
	}
	// turning full capture off discards what it held and keeps the unanswered ones
	code, out = cl.call("PUT", "/api/capture", `{"kind":"full","on":false}`)
	if code != http.StatusOK || !strings.Contains(out, "what it had captured was deleted") {
		t.Fatalf("turn full capture off: %d %s", code, out)
	}
	if st := capture.State(); st.FullHeld != 0 || st.UnansweredHeld != 1 {
		t.Fatalf("after turning full capture off: %+v", st)
	}
	if code, _ = cl.call("DELETE", "/api/capture", ""); code != http.StatusOK || capture.State().UnansweredHeld != 0 {
		t.Fatalf("clear: %d", code)
	}
}

// The console password can be changed from the console: the session that
// changed it stays, every other session ends, and a wrong current password
// goes back to the form.
func TestConsolePasswordChange(t *testing.T) {
	cl, c, _ := protectedConsole(t, withUser(t), "")
	first, second := "first-console-pw", "second-console-pw"
	body := func(pw string) string { b, _ := json.Marshal(map[string]string{"password": pw}); return string(b) }
	if code, _ := cl.call("POST", "/api/auth/claim", body("short")); code != http.StatusBadRequest {
		t.Fatalf("a short console password was accepted: %d", code)
	}
	if code, _ := cl.call("POST", "/api/auth/claim", body(first)); code != http.StatusNoContent {
		t.Fatalf("claim: %d", code)
	}
	other := &client{t: t, srv: cl.srv}
	if code, _ := other.call("POST", "/api/auth/login", body(first)); code != http.StatusNoContent {
		t.Fatal("second session")
	}
	change := func(cur, pw, pw2 string) map[string]any {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"params": map[string]string{"cur": cur, "pw": pw, "pw2": pw2}})
		code, out := cl.call("POST", "/api/task/console-password", string(b))
		var v map[string]any
		if code != 200 || json.Unmarshal([]byte(out), &v) != nil {
			t.Fatalf("change: %d %s", code, out)
		}
		return v
	}
	if v := change("not-it-at-all", second, second); v["errors"].(map[string]any)["cur"] != "That is not the current password." {
		t.Fatalf("wrong current password: %v", v)
	}
	if c.Home().Failure != nil {
		t.Error("a wrong current password was recorded as a change that failed")
	}
	if v := change(first, "short", "short"); v["errors"].(map[string]any)["pw"] != "Use at least 8 characters." {
		t.Fatalf("short new password: %v", v)
	}
	if v := change(first, second, "different-one"); v["errors"].(map[string]any)["pw2"] != "The two new passwords are different." {
		t.Fatalf("mismatch: %v", v)
	}
	if v := change(first, second, second); v["outcome"] == nil {
		t.Fatalf("change: %v", v)
	}
	if code, _ := cl.call("GET", "/api/home", ""); code != 200 {
		t.Errorf("the session that changed the password was signed out: %d", code)
	}
	if code, _ := other.call("GET", "/api/home", ""); code != http.StatusUnauthorized {
		t.Errorf("another session survived the password change: %d", code)
	}
	if code, _ := other.call("POST", "/api/auth/login", body(first)); code != http.StatusUnauthorized {
		t.Error("the old console password still signs in")
	}
	if code, _ := other.call("POST", "/api/auth/login", body(second)); code != http.StatusNoContent {
		t.Error("the new console password does not sign in")
	}
}

// A console password supplied by the deployment cannot be changed here.
func TestManagedConsolePasswordIsReadOnly(t *testing.T) {
	password := testPassword()
	cl, c, _ := protectedConsole(t, withUser(t), password)
	body, _ := json.Marshal(map[string]string{"password": password})
	if code, _ := cl.call("POST", "/api/auth/login", string(body)); code != http.StatusNoContent {
		t.Fatal("login")
	}
	if !c.Security().ConsoleManaged {
		t.Error("Security does not say the console password is managed")
	}
	code, out := cl.call("GET", "/api/task/console-password", "")
	if code != http.StatusUnprocessableEntity || !strings.Contains(out, "installed with") {
		t.Errorf("form: %d %s", code, out)
	}
}

// Until first-run set-up has chosen the tools' password the console serves
// that step and nothing else.
func TestSetupAccountComesFirst(t *testing.T) {
	cl, c, _ := protectedConsole(t, scenario.Phase1("fresh"), "")
	body, _ := json.Marshal(map[string]string{"password": "console-password"})
	if code, _ := cl.call("POST", "/api/auth/claim", string(body)); code != http.StatusNoContent {
		t.Fatal("claim")
	}
	code, out := cl.call("GET", "/api/home", "")
	if code != 200 || !strings.Contains(out, `"accountNeeded":true`) {
		t.Fatalf("home: %d %.200s", code, out)
	}
	for _, p := range []string{"/api/clusters", "/api/users", "/api/task/add-cluster"} {
		if code, _ := cl.call("GET", p, ""); code != http.StatusConflict {
			t.Errorf("GET %s before set-up: %d", p, code)
		}
	}
	code, out = cl.call("POST", "/api/setup/account", `{"password":"abc"}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(out, "That is 3 characters. Use at least 8") {
		t.Errorf("short account password: %d %s", code, out)
	}
	if code, out = cl.call("POST", "/api/setup/account", `{"password":"tools-password"}`); code != 200 {
		t.Fatalf("set-up: %d %s", code, out)
	}
	if c.AccountNeeded() {
		t.Fatal("still needed")
	}
	if code, _ := cl.call("GET", "/api/clusters", ""); code != 200 {
		t.Errorf("clusters after set-up: %d", code)
	}
}

func TestConsoleClaimRoutesAndRestart(t *testing.T) {
	claimBody, _ := json.Marshal(map[string]string{"password": testPassword()})
	dir := t.TempDir()
	ctl, err := control.New(control.Options{Scenario: withUser(t), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	makeServer := func() *httptest.Server {
		handler, err := ProtectedHandler(ctl, nil, dir, "", false)
		if err != nil {
			t.Fatal(err)
		}
		return httptest.NewServer(handler)
	}
	server := makeServer()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string, cookie *http.Cookie) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := get("/", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/claim" {
		t.Fatalf("fresh console: %d", res.StatusCode)
	}
	for _, path := range []string{"/claim", "/auth.js", "/app.css", "/lattice/lattice.css"} {
		res = get(path, nil)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("claim asset %s: %d", path, res.StatusCode)
		}
	}
	res = get("/api/home", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unclaimed API: %d", res.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/auth/claim", strings.NewReader(string(claimBody)))
	req.Header.Set("Content-Type", "application/json")
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("claim: %d", res.StatusCode)
	}
	cookie := res.Cookies()[0]
	res = get("/", cookie)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("claimed console: %d", res.StatusCode)
	}
	server.Close()
	server = makeServer()
	defer server.Close()
	res = get("/api/home", cookie)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session survived restart: %d", res.StatusCode)
	}
	res = get("/", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
		t.Fatalf("claimed redirect: %d", res.StatusCode)
	}
}
