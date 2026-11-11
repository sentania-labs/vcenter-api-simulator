package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

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

func TestReadEndpoints(t *testing.T) {
	srv, _ := newAdmin(t)
	for _, p := range []string{"/api/state", "/api/tree", "/api/draft", "/api/presets", "/api/version-presets", "/api/timeline", "/api/families", "/api/compat",
		"/api/activity", "/api/clusters/dc-01/cluster-03"} {
		code, b := do(t, "GET", srv.URL+p, "", "", nil)
		if code != 200 || !json.Valid(b) {
			t.Errorf("GET %s: %d %.200s", p, code, b)
		}
	}
	if code, _ := do(t, "GET", srv.URL+"/api/review", "", "", nil); code != http.StatusConflict {
		t.Errorf("review without a draft: %d", code)
	}
	if code, _ := do(t, "GET", srv.URL+"/api/clusters/dc-01/nope", "", "", nil); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown cluster: %d", code)
	}
	code, b := do(t, "GET", srv.URL+"/", "", "", nil)
	if code != 200 || !strings.Contains(string(b), "control console") {
		t.Errorf("index: %d", code)
	}
	for _, f := range []string{"/js/app.js", "/lattice/tokens.css", "/lattice/lattice.css", "/app.css"} {
		if code, _ := do(t, "GET", srv.URL+f, "", "", nil); code != 200 {
			t.Errorf("GET %s: %d", f, code)
		}
	}
}

func TestStateNeverCarriesCredentials(t *testing.T) {
	srv, _ := newAdmin(t)
	for _, p := range []string{"/api/state", "/api/draft", "/api/tree"} {
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
	if code, _ := do(t, "POST", srv.URL+"/api/apply/cancel", "", "", nil); code != http.StatusForbidden {
		t.Errorf("no body, no header: %d", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/api/pause", j, `{"paused":true}`, map[string]string{"Origin": srv.URL}); code != 200 {
		t.Errorf("same origin: %d", code)
	}
}

func TestEditReviewApplyOverHTTP(t *testing.T) {
	srv, c := newAdmin(t)
	j := "application/json"
	_, b := do(t, "GET", srv.URL+"/api/draft", "", "", nil)
	var v struct{ Scenario map[string]any }
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	v.Scenario["latency"] = map[string]any{"perfQuery": map[string]any{"fixedMs": 150, "jitterMs": 10}}
	body, _ := json.Marshal(map[string]any{"scenario": v.Scenario, "scope": "dashboard"})
	if code, b := do(t, "PUT", srv.URL+"/api/draft", j, string(body), nil); code != 200 {
		t.Fatalf("PUT draft: %d %s", code, b)
	}
	if code, b := do(t, "PUT", srv.URL+"/api/draft", j, `{"scenario":{"seed":"x","datacenters":[]}}`, nil); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "error") {
		t.Errorf("invalid draft: %d %s", code, b)
	}
	code, b := do(t, "GET", srv.URL+"/api/review", "", "", nil)
	var rv control.Review
	if code != 200 || json.Unmarshal(b, &rv) != nil || rv.TelemetryCount != 2 || rv.RebuildsVCenter || rv.Scope != "dashboard" {
		t.Fatalf("review: %d %s", code, b)
	}
	if code, b := do(t, "POST", srv.URL+"/api/apply", j, `{"when":"soon"}`, nil); code != http.StatusUnprocessableEntity {
		t.Errorf("bad when: %d %s", code, b)
	}
	if code, b := do(t, "POST", srv.URL+"/api/apply", j, `{"when":"now"}`, nil); code != 200 {
		t.Fatalf("apply: %d %s", code, b)
	}
	if st := c.State(); st.Scope != "dashboard" || st.Topology.Version != 1 || st.Scenario.Latency == "none added" {
		t.Errorf("state after a telemetry-only apply: scope %s v%d (the topology version must not move) latency %q", st.Scope, st.Topology.Version, st.Scenario.Latency)
	}
	if code, _ := do(t, "POST", srv.URL+"/api/apply", j, `{"when":"now"}`, nil); code != http.StatusConflict {
		t.Errorf("apply with nothing pending: %d", code)
	}
}

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
		if code := host("GET", "/api/state", "", "", h); code != http.StatusForbidden {
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
		if code := host("GET", "/api/state", "", "", h); code != 200 {
			t.Errorf("read with Host %q: %d, want 200", h, code)
		}
		if code := host("POST", "/api/pause", j, `{"paused":false}`, h); code != 200 {
			t.Errorf("write with Host %q: %d, want 200", h, code)
		}
	}
}

func TestConsoleRefusesFraming(t *testing.T) {
	srv, _ := newAdmin(t)
	for _, path := range []string{"/", "/api/state"} {
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

// The console's draft-save behaviour is tested in JavaScript against a fake
// admin API; this runs it with node when node is installed.
func TestConsoleScriptTests(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "ui/js/telemetry.test.mjs", "ui/js/topology.test.mjs")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}

func TestProtectedCaptureAPI(t *testing.T) {
	dir := t.TempDir()
	c, err := control.New(control.Options{Scenario: scenario.Phase1("capture"), SourceHash: "capture", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	capture, err := querylog.NewCapture(dir, "", querylog.Settings{Unanswered: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	protected, err := ProtectedHandler(c, nil, dir, "console-test-password", false, capture)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(protected)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/capture", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", res.StatusCode)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/api/auth/login", strings.NewReader(`{"password":"console-test-password"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("login: %d", res.StatusCode)
	}
	cookie := res.Cookies()[0]
	call := func(method, path, body string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		r.AddCookie(cookie)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		} else if method == "DELETE" {
			r.Header.Set("X-Console", "1")
		}
		out, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	res = call("PUT", "/api/capture", `{"full":true,"unanswered":true,"stdout":false}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings: %d", res.StatusCode)
	}
	res.Body.Close()
	capture.Wrap(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/ls/sdk", strings.NewReader(`<Envelope><Body><Fetch><_this type="PropertyCollector">pc</_this><pathSet>config.product</pathSet></Fetch></Body></Envelope>`)))
	res = call("GET", "/api/capture/unanswered.jsonl", "")
	b, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK || !strings.Contains(string(b), "config.product") {
		t.Fatalf("download: %d %s %v", res.StatusCode, b, err)
	}
	res = call("DELETE", "/api/capture", "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK || capture.State().UnansweredHeld != 0 {
		t.Fatalf("clear: %d", res.StatusCode)
	}
}
