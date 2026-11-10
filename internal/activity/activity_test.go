package activity

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCaptureCountsAcrossWriteBoundaries(t *testing.T) {
	body := strings.Repeat(`<returnval><entityRefId>x:*</entityRefId><sampleInfo></sampleInfo></returnval>`, 40) +
		`<returnval><entityRefId>y:1</entityRefId><sampleInfo>2026</sampleInfo></returnval>`
	for _, chunk := range []int{1, 7, 11, 13, 24, 64, 1000, len(body)} {
		rec := httptest.NewRecorder()
		c := NewCapture(rec, true)
		for i := 0; i < len(body); i += chunk {
			j := min(i+chunk, len(body))
			if _, err := c.Write([]byte(body[i:j])); err != nil {
				t.Fatal(err)
			}
		}
		if c.Entities != 41 || c.Empty != 40 || c.Bytes != int64(len(body)) {
			t.Errorf("chunk %d: entities %d empty %d bytes %d", chunk, c.Entities, c.Empty, c.Bytes)
		}
	}
}

func TestCaptureReadsAFault(t *testing.T) {
	rec := httptest.NewRecorder()
	c := NewCapture(rec, false)
	c.WriteHeader(500)
	c.Write([]byte(`<soapenv:Fault><faultcode>ServerFaultCode</faultcode><faultstring>A general system error occurred: host-domowner</faultstring><detail><SystemError xmlns="urn:vim25" xsi:type="SystemError"><reason>x</reason></SystemError></detail></soapenv:Fault>`))
	typ, msg, ok := c.Fault()
	if !ok || typ != "SystemError" || !strings.Contains(msg, "host-domowner") || c.Status != 500 {
		t.Errorf("%q %q %v %d", typ, msg, ok, c.Status)
	}
}

func TestSDKMethod(t *testing.T) {
	for body, want := range map[string]string{
		`<soapenv:Envelope><soapenv:Body><Login xmlns="urn:vim25"><userName>u</userName></Login></soapenv:Body></soapenv:Envelope>`: "Login",
		`<s:Envelope><s:Body xmlns:x="y">
			<RetrievePropertiesEx xmlns="urn:vim25"/></s:Body></s:Envelope>`: "RetrievePropertiesEx",
		`not soap`: "",
	} {
		if got := SDKMethod([]byte(body)); got != want {
			t.Errorf("%q -> %q, want %q", body[:20], got, want)
		}
	}
}

func TestRingKeepsTheLastCalls(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	l := New(5, now)
	for i := 0; i < 12; i++ {
		l.Add(Call{Time: now.Add(time.Duration(i) * time.Second), Method: "m", Surface: "sdk", Result: ResultOK})
	}
	got := l.Query(Filter{Limit: 100})
	if len(got) != 5 || got[0].Seq != 12 || got[4].Seq != 8 {
		t.Errorf("ring: %+v", got)
	}
	if tot := l.Totals(); tot.Requests != 12 || tot.Held != 5 {
		t.Errorf("totals: %+v", tot)
	}
	if got := l.Query(Filter{AfterSeq: 10}); len(got) != 2 {
		t.Errorf("after: %+v", got)
	}
	if _, ok := l.Get(3); ok {
		t.Error("a call that left the ring is gone")
	}
}

func TestCycleAndDiagnosis(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	add := func(l *Log, at time.Time, c Call) {
		c.Time = at
		if c.Client == "" {
			c.Client = "10.0.0.2"
		}
		l.Add(c)
	}

	// an old cycle, a gap, then a cycle where the login worked and every
	// query came back empty
	l := New(100, now)
	add(l, now.Add(-30*time.Minute), Call{Surface: "sdk", Method: "Login", Result: ResultOK, Client: "10.0.0.1"})
	add(l, now.Add(-30*time.Minute), Call{Surface: "vsanHealth", Method: "VsanPerfQueryPerf", Family: "host-cpu", Result: ResultOK, Entities: 4, Client: "10.0.0.1"})
	add(l, now.Add(-2*time.Minute), Call{Surface: "sdk", Method: "Login", Result: ResultOK})
	add(l, now.Add(-90*time.Second), Call{Surface: "vsanHealth", Method: "VsanPerfQueryPerf", Family: "disk-group", Result: ResultOK, Entities: 1, Empty: 1, DurationMs: 12})
	add(l, now.Add(-60*time.Second), Call{Surface: "vsanHealth", Method: "VsanPerfQueryPerf", Family: "vsan-iscsi-host", Result: ResultOK, Entities: 1, Empty: 1})
	s := l.Summarize(now)
	if s.Cycle.Calls != 3 || s.Cycle.Client != "10.0.0.2" || s.Cycle.Series != 0 || s.Cycle.Empty != 2 {
		t.Errorf("cycle: %+v", s.Cycle)
	}
	if !hasFinding(s.Cycle.Findings, "bad", "Every performance query returned empty") {
		t.Errorf("findings: %+v", s.Cycle.Findings)
	}
	if len(s.Minutes) != 60 || s.Minutes[59].Requests != 0 {
		t.Errorf("minutes: %d", len(s.Minutes))
	}
	if s.Cycle.Steps[0].Name != "Login · RetrieveContent" || s.Cycle.Steps[len(s.Cycle.Steps)-1].Name != "VsanPerfQueryPerf" {
		t.Errorf("step order: %+v", s.Cycle.Steps)
	}

	// refused logins
	l = New(100, now)
	add(l, now.Add(-10*time.Second), Call{Surface: "sdk", Method: "Login", Result: ResultDenied, Fault: "InvalidLogin"})
	if f := l.Summarize(now).Cycle.Findings; !hasFinding(f, "bad", "Authentication failed") {
		t.Errorf("findings: %+v", f)
	}

	// injected faults are warned about, not called a failure
	l = New(100, now)
	add(l, now.Add(-10*time.Second), Call{Surface: "sdk", Method: "Login", Result: ResultOK})
	add(l, now.Add(-9*time.Second), Call{Surface: "vsanHealth", Method: "VsanPerfQueryPerf", Family: "vsan-pnic-net", Result: ResultInjected, Fault: "SystemError"})
	add(l, now.Add(-8*time.Second), Call{Surface: "vsanHealth", Method: "VsanPerfQueryPerf", Family: "host-cpu", Result: ResultOK, Entities: 4})
	s = l.Summarize(now)
	if s.Faults.Injected != 1 || s.Faults.Unexpected != 0 {
		t.Errorf("faults: %+v", s.Faults)
	}
	if !hasFinding(s.Cycle.Findings, "warn", "1 performance queries answered with an injected fault") {
		t.Errorf("findings: %+v", s.Cycle.Findings)
	}

	// no traffic
	if f := New(10, now).Summarize(now).Cycle.Findings; len(f) != 0 {
		t.Errorf("an empty log has no cycle: %+v", f)
	}
}

func hasFinding(fs []Finding, sev, title string) bool {
	for _, f := range fs {
		if f.Severity == sev && f.Title == title {
			return true
		}
	}
	return false
}

func TestDiagnosisNamesProvisionalKindsAndSeparatesAuthFaults(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	l := New(100, now)
	l.EmptyKind = func(f string) bool { return f == "disk-group" || f == "vsan-iscsi-host" }
	add := func(at time.Duration, c Call) {
		c.Time, c.Client = now.Add(at), "10.0.0.2"
		l.Add(c)
	}
	add(-20*time.Second, Call{Surface: "sdk", Method: "Login", Result: ResultOK})
	add(-19*time.Second, Call{Surface: "vsanHealth", Method: "VsanPerfQueryPerf", Family: "disk-group", Result: ResultOK, Entities: 1, Empty: 1})
	add(-18*time.Second, Call{Surface: "vsanHealth", Method: "VsanPerfQueryPerf", Family: "vsan-iscsi-host", Result: ResultOK, Entities: 1, Empty: 1})
	f := l.Summarize(now).Cycle.Findings
	var detail string
	for _, x := range f {
		if x.Title == "Every performance query returned empty" {
			detail = x.Detail
		}
	}
	if !strings.Contains(detail, "disk-group, vsan-iscsi-host") || !strings.Contains(detail, "authentication was fine") {
		t.Errorf("detail: %q", detail)
	}

	// calls that carry no session are an authentication problem, not "unexpected" query failures
	l = New(100, now)
	add(-20*time.Second, Call{Surface: "sdk", Method: "Login", Result: ResultFault, Fault: "InvalidLogin"})
	add(-19*time.Second, Call{Surface: "vsanHealth", Method: "VsanPerfQueryPerf", Family: "host-cpu", Result: ResultDenied, Fault: "NotAuthenticated"})
	s := l.Summarize(now)
	for _, x := range s.Cycle.Findings {
		if strings.Contains(x.Title, "without an injected rule") {
			t.Errorf("an unauthenticated query was reported as an unexpected failure: %+v", x)
		}
	}
	if !hasFinding(s.Cycle.Findings, "bad", "Authentication failed") || !hasFinding(s.Cycle.Findings, "bad", "1 performance queries were refused as not authenticated") {
		t.Errorf("findings: %+v", s.Cycle.Findings)
	}
	if st := s.Cycle.Steps[len(s.Cycle.Steps)-1]; st.Series != 0 {
		t.Errorf("series: %+v", st)
	}
}

func TestACollectionIsSplitFromTheNextLogin(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	l := New(100, now)
	add := func(at time.Duration, c Call) {
		c.Time, c.Client = now.Add(at), "10.0.0.2"
		l.Add(c)
	}
	// a refused login and its queries, then 5 s later a good login whose queries all came back empty
	add(-30*time.Second, Call{Surface: "sdk", Method: "Login", Result: ResultDenied, Fault: "InvalidLogin"})
	add(-29*time.Second, Call{Surface: "vsanHealth", Method: "VsanPerfQueryPerf", Family: "host-cpu", Result: ResultDenied, Fault: "NotAuthenticated"})
	add(-10*time.Second, Call{Surface: "sdk", Method: "Login", Result: ResultOK})
	add(-9*time.Second, Call{Surface: "vsanHealth", Method: "VsanPerfQueryPerf", Family: "disk-group", Result: ResultOK, Entities: 1, Empty: 1})
	cy := l.Summarize(now).Cycle
	if cy.Calls != 2 || hasFinding(cy.Findings, "bad", "Authentication failed") {
		t.Errorf("the latest collection should not inherit the earlier refusals: %+v", cy)
	}
	// a login right after queries is the next collection, even inside the gap
	l = New(100, now)
	add(-9*time.Second, Call{Surface: "vsanHealth", Method: "VsanPerfQueryPerf", Family: "host-cpu", Result: ResultOK, Entities: 3})
	add(-8*time.Second, Call{Surface: "sdk", Method: "Login", Result: ResultOK})
	if cy = l.Summarize(now).Cycle; cy.Calls != 1 {
		t.Errorf("cycle: %+v", cy)
	}
}

func TestNextCollectionStartsAtTheServiceContentCall(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	l := New(100, now)
	add := func(at time.Duration, c Call) {
		c.Time, c.Client = now.Add(at), "10.0.0.2"
		l.Add(c)
	}
	add(-12*time.Second, Call{Surface: "sdk", Method: "RetrieveServiceContent", Result: ResultOK})
	add(-11*time.Second, Call{Surface: "sdk", Method: "Login", Result: ResultDenied, Fault: "InvalidLogin"})
	add(-10*time.Second, Call{Surface: "vsanHealth", Method: "VsanPerfQueryPerf", Family: "host-cpu", Result: ResultDenied, Fault: "NotAuthenticated"})
	add(-6*time.Second, Call{Surface: "sdk", Method: "RetrieveServiceContent", Result: ResultOK})
	add(-5*time.Second, Call{Surface: "sdk", Method: "Login", Result: ResultOK})
	add(-4*time.Second, Call{Surface: "vsanHealth", Method: "VsanPerfQueryPerf", Family: "disk-group", Result: ResultOK, Entities: 1, Empty: 1})
	cy := l.Summarize(now).Cycle
	if cy.Calls != 3 || hasFinding(cy.Findings, "bad", "Authentication failed") || !hasFinding(cy.Findings, "bad", "Every performance query returned empty") {
		t.Errorf("the second session must be judged on its own: %+v", cy)
	}
}

// The console's Telemetry tab reads summary.families.length; a quiet log (no
// collector traffic yet) must serialize empty lists as [] not null.
func TestSummaryOfAQuietLogSerializesEmptyListsNotNull(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	b, err := json.Marshal(New(5, now).Summarize(now))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"methods", "families", "duration", "minutes", "clusters"} {
		if s := string(got[k]); !strings.HasPrefix(s, "[") {
			t.Errorf("%s = %s, want a JSON array", k, s)
		}
	}
}
