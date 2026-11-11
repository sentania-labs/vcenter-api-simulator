package querylog

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestLogRedactsLoginAndRecordsResponse(t *testing.T) {
	var log bytes.Buffer
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("test"))
	}), &log)
	body := `<Envelope><Body><Login><userName>alice</userName><password>secret</password></Login></Body></Envelope>`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(body)))
	if strings.Contains(log.String(), "secret") || strings.Contains(log.String(), "alice") {
		t.Fatal("credentials included in log")
	}
	var got record
	if err := json.Unmarshal(log.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Operation != "Login" || got.Request != "" || got.Status != 418 || got.Bytes != 4 {
		t.Fatalf("unexpected record: %+v", got)
	}
}

func TestLogRedactsEverySessionManagerOperation(t *testing.T) {
	for _, op := range []string{"Login", "CloneSession", "ImpersonateUser", "AcquireGenericServiceTicket"} {
		var log bytes.Buffer
		h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), &log)
		body := `<Envelope><Body><` + op + ` xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><cloneTicket>TICKET</cloneTicket></` + op + `></Body></Envelope>`
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(body)))
		var got record
		if err := json.Unmarshal(log.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(log.String(), "TICKET") || got.Operation != op || got.Request != "" {
			t.Fatalf("%s leaked or misrecorded: %s", op, log.String())
		}
	}
}

func TestLogKeepsOtherRequestBodies(t *testing.T) {
	var log bytes.Buffer
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), &log)
	body := `<Envelope><Body><RetrievePropertiesEx xmlns="urn:vim25"><_this type="PropertyCollector">propertyCollector</_this></RetrievePropertiesEx></Body></Envelope>`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(body)))
	var got record
	if err := json.Unmarshal(log.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Operation != "RetrievePropertiesEx" || got.Request != body {
		t.Fatalf("unexpected record: %+v", got)
	}
}

func TestLogRejectsOversizedBody(t *testing.T) {
	var log bytes.Buffer
	called := false
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }), &log)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/sdk", strings.NewReader(strings.Repeat("x", maxBody+1))))
	if rec.Code != http.StatusRequestEntityTooLarge || called {
		t.Fatalf("code %d, downstream called %v", rec.Code, called)
	}
}

func TestCaptureRedactsAndFindsUnanswered(t *testing.T) {
	var stdout bytes.Buffer
	c, err := NewCapture(t.TempDir(), "", Settings{Full: true, Unanswered: true, Stdout: true}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ls/sdk" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`<Envelope><Body><Fault><detail><InvalidPropertyFault /></detail></Fault></Body></Envelope>`))
	}))
	body := `<Envelope><Header><session>HEADER_SECRET</session></Header><Body><RetrievePropertiesEx><_this type="PropertyCollector">propertyCollector</_this><pathSet>config.product</pathSet><password>PASSWORD_SECRET</password><token>TOKEN_SECRET</token><sessionId>SESSION_SECRET</sessionId></RetrievePropertiesEx></Body></Envelope>`
	for _, path := range []string{"/sdk", "/ls/sdk"} {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "HEADER_SECRET")
		r.AddCookie(&http.Cookie{Name: "session", Value: "COOKIE_SECRET"})
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if c.State().UnansweredHeld != 2 || c.State().FullHeld != 2 {
		t.Fatalf("counts: %+v", c.State())
	}
	records := c.Records(true)
	if records[0].Fault != "InvalidProperty" || records[0].ObjectType != "PropertyCollector" || len(records[0].Properties) != 1 || records[0].Properties[0] != "config.product" || records[1].Status != 404 {
		t.Fatalf("records: %+v", records)
	}
	for _, secret := range []string{"HEADER_SECRET", "PASSWORD_SECRET", "TOKEN_SECRET", "SESSION_SECRET", "COOKIE_SECRET"} {
		if strings.Contains(stdout.String(), secret) {
			t.Fatalf("stdout leaked %s", secret)
		}
		b, _ := json.Marshal(records)
		if strings.Contains(string(b), secret) {
			t.Fatalf("records leaked %s", secret)
		}
	}
	if !strings.Contains(records[0].Request, "config.product") {
		t.Fatalf("missing research data: %s", records[0].Request)
	}
}

func TestCaptureSettingsPersistAndClear(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCapture(dir, "", Settings{Unanswered: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{Full: true, Unanswered: true, Stdout: true}
	if err = c.Set(want); err != nil {
		t.Fatal(err)
	}
	h := c.Wrap(http.NotFoundHandler())
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/missing", nil))
	if c.State().FullHeld != 1 || c.State().UnansweredHeld != 1 {
		t.Fatalf("state: %+v", c.State())
	}
	restarted, err := NewCapture(dir, "", Settings{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.State().Settings != want {
		t.Fatalf("settings: %+v", restarted.State())
	}
	before, err := os.ReadFile(c.State().Path)
	if err != nil || len(before) == 0 {
		t.Fatalf("log missing: %v", err)
	}
	if err = c.Clear(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(c.State().Path)
	if err != nil || len(after) != 0 || c.State().FullHeld != 0 {
		t.Fatalf("clear: %q %v", after, err)
	}
}

func TestCaptureOmitsMalformedAndSessionBodies(t *testing.T) {
	for _, body := range []string{`<Envelope><Body><Fetch><password>LEAK`, `<Envelope><Body><CloneSession><_this type="SessionManager">x</_this><ticket>LEAK</ticket></CloneSession></Body></Envelope>`, `{"password":"LEAK"}`} {
		if got := Redact([]byte(body)); got != "" {
			t.Fatalf("unsafe body retained: %q", got)
		}
	}
}

func TestCapturePartialPropertyFaultWithSuccessfulHTTP(t *testing.T) {
	c, err := NewCapture(t.TempDir(), "", Settings{Unanswered: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<Envelope><Body><RetrievePropertiesExResponse><returnval><objects><missingSet><path>config.invalid</path><fault type="InvalidProperty"/></missingSet></objects></returnval></RetrievePropertiesExResponse></Body></Envelope>`))
	}))
	body := `<Envelope><Body><RetrievePropertiesEx><_this type="PropertyCollector">pc</_this><specSet><propSet><type>HostSystem</type><pathSet>config.invalid</pathSet></propSet></specSet></RetrievePropertiesEx></Body></Envelope>`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(body)))
	got := c.Records(true)
	if len(got) != 1 || got[0].Status != 200 || got[0].Fault != "InvalidProperty" || got[0].ObjectType != "HostSystem" || len(got[0].Properties) != 1 {
		t.Fatalf("partial fault not captured: %+v", got)
	}
}

func TestCaptureLicensingDiscoveryRequests(t *testing.T) {
	c, err := NewCapture(t.TempDir(), "", Settings{Unanswered: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not supported", http.StatusInternalServerError)
	}))
	requests := []struct{ op, target, inner, property string }{
		{"QueryAssignedLicenses", "LicenseAssignmentManager", `<entityId>root</entityId>`, ""},
		{"QueryOptions", "OptionManager", `<name>config.OPERATIONS.vcf.sso.ops.cluster.id</name>`, "config.OPERATIONS.vcf.sso.ops.cluster.id"},
		{"FindExtension", "ExtensionManager", `<extensionKey>example.extension</extensionKey>`, "example.extension"},
	}
	for _, r := range requests {
		body := `<Envelope><Body><` + r.op + `><_this type="` + r.target + `">instance</_this>` + r.inner + `</` + r.op + `></Body></Envelope>`
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(body)))
	}
	got := c.Records(true)
	if len(got) != len(requests) {
		t.Fatalf("got %d records", len(got))
	}
	for i, r := range requests {
		if got[i].Operation != r.op || got[i].ObjectType != r.target || !strings.Contains(got[i].Request, r.inner) {
			t.Errorf("%s: %+v", r.op, got[i])
		}
		if r.property != "" && (len(got[i].Properties) != 1 || got[i].Properties[0] != r.property) {
			t.Errorf("%s property: %v", r.op, got[i].Properties)
		}
	}
}

func TestCaptureFetchPropertyWhenUnauthenticated(t *testing.T) {
	c, err := NewCapture(t.TempDir(), "", Settings{Unanswered: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`<Envelope><Body><Fault><detail><NotAuthenticatedFault/></detail></Fault></Body></Envelope>`))
	}))
	body := `<Envelope><Body><Fetch><_this type="HostSystem">host-1</_this><prop>config.product</prop></Fetch></Body></Envelope>`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(body)))
	got := c.Records(true)
	if len(got) != 1 || got[0].Fault != "NotAuthenticated" || got[0].ObjectType != "HostSystem" || len(got[0].Properties) != 1 || got[0].Properties[0] != "config.product" {
		t.Fatalf("Fetch not captured: %+v", got)
	}
}

func TestCaptureReportsWriteFailure(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("/dev/full unavailable")
	}
	c, err := NewCapture(t.TempDir(), "/dev/full", Settings{Full: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Wrap(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/missing", nil))
	if c.State().Error == "" {
		t.Fatal("full log write failure was not reported")
	}
}

func TestCaptureBoundsMemoryButRetainsFullFile(t *testing.T) {
	c, err := NewCapture(t.TempDir(), "", Settings{Full: true, Unanswered: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("x", maxRecordedBody+1000)
	body := `<Envelope><Body><Fetch><_this type="HostSystem">host</_this><prop>` + large + `</prop></Fetch></Body></Envelope>`
	c.Wrap(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(body)))
	records := c.Records(true)
	if len(records) != 1 || !records[0].RequestTruncated || len(records[0].Request) != maxRecordedBody {
		t.Fatal("unanswered memory record was not bounded")
	}
	var output bytes.Buffer
	if err = c.WriteJSONL(&output, false); err != nil {
		t.Fatal(err)
	}
	var saved record
	if err = json.Unmarshal(output.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Request != body || saved.RequestTruncated {
		t.Fatal("full file did not retain the entire redacted body")
	}
}

// A SOAP fault is a 500 in vcsim. Only faults that mean "the simulator cannot
// answer this" are unanswered; deliberate answers (injected faults, refused
// logins, missing privileges) are not, and a bare 5xx without a fault is.
func TestUnansweredClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"unknown method", 500, `<Envelope><Body><Fault><detail><InvalidRequestFault/></detail></Fault></Body></Envelope>`, true},
		{"method not found", 500, `<Envelope><Body><Fault><detail><MethodNotFoundFault/></detail></Fault></Body></Envelope>`, true},
		{"not supported", 500, `<Envelope><Body><Fault><detail><NotSupportedFault/></detail></Fault></Body></Envelope>`, true},
		{"invalid property", 500, `<Envelope><Body><Fault><detail><InvalidPropertyFault/></detail></Fault></Body></Envelope>`, true},
		{"managed object not found", 500, `<Envelope><Body><Fault><detail><ManagedObjectNotFoundFault/></detail></Fault></Body></Envelope>`, true},
		{"not authenticated", 500, `<Envelope><Body><Fault><detail><NotAuthenticatedFault/></detail></Fault></Body></Envelope>`, true},
		{"missing path", 404, ``, true},
		{"method not allowed", 405, ``, true},
		{"bare server error", 500, `internal error`, true},
		{"invalid login", 500, `<Envelope><Body><Fault><detail><InvalidLoginFault/></detail></Fault></Body></Envelope>`, false},
		{"no permission", 500, `<Envelope><Body><Fault><detail><NoPermissionFault/></detail></Fault></Body></Envelope>`, false},
		{"injected invalid argument", 500, `<Envelope><Body><Fault><detail><InvalidArgumentFault/></detail></Fault></Body></Envelope>`, false},
		{"injected system error", 500, `<Envelope><Body><Fault><detail><SystemErrorFault/></detail></Fault></Body></Envelope>`, false},
		{"success", 200, `<Envelope><Body/></Envelope>`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewCapture(t.TempDir(), "", Settings{Unanswered: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(`<Envelope><Body><Fetch/></Body></Envelope>`)))
			if got := len(c.Records(true)) == 1; got != tc.want {
				t.Fatalf("unanswered = %v, want %v", got, tc.want)
			}
		})
	}
}
