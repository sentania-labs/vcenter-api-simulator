package activity

import (
	"encoding/xml"
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
		l.Add(Call{Time: now.Add(time.Duration(i) * time.Second), Method: "m", Surface: SurfaceSDK, Reason: ReasonAnswered})
	}
	got := l.All()
	if len(got) != 5 || got[0].Seq != 8 || got[4].Seq != 12 {
		t.Fatalf("ring holds %d calls, first %d last %d", len(got), got[0].Seq, got[len(got)-1].Seq)
	}
	if tot := l.Totals(); tot.Requests != 12 || tot.Held != 5 {
		t.Errorf("totals %+v", tot)
	}
	if recent := l.Since(now.Add(10 * time.Second)); len(recent) != 2 || recent[0].Seq != 11 {
		t.Errorf("since: %+v", recent)
	}
	if _, ok := l.Get(3); ok {
		t.Error("a call that left the ring was found")
	}
	if c, ok := l.Get(9); !ok || c.Seq != 9 {
		t.Error("a held call was not found")
	}
}

// Classify decides what the Requests headline counts. "unknown" is only for
// a request the simulator has no answer for.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		f    Facts
		want string
	}{
		{"a full answer", Facts{Surface: SurfaceSDK, Method: "RetrieveServiceContent", Status: 200}, ReasonAnswered},
		{"a performance query with data", Facts{Surface: SurfaceVsan, Method: "VsanPerfQueryPerf", Status: 200, Entities: 4, Empty: 0}, ReasonAnswered},
		{"a performance query where every entity is empty", Facts{Surface: SurfaceVsan, Method: "VsanPerfQueryPerf", Status: 200, Entities: 4, Empty: 4}, ReasonEmpty},
		{"a performance query with no entity at all", Facts{Surface: SurfaceVsan, Method: "VsanPerfQueryPerf", Status: 200}, ReasonEmpty},
		{"a kind a real vCenter refuses", Facts{Surface: SurfaceVsan, Method: "VsanPerfQueryPerf", Status: 500, Faulted: true, FaultType: "InvalidArgument"}, ReasonRefused},
		{"persistent memory, not supported on a real vCenter either", Facts{Surface: SurfaceVsan, Method: "VsanPerfQueryPerf", Status: 500, Faulted: true, FaultType: "NotSupported"}, ReasonRefused},
		{"a fault the operator switched on", Facts{Surface: SurfaceVsan, Method: "VsanPerfQueryPerf", Status: 500, Faulted: true, FaultType: "SystemError", Injected: true}, ReasonFault},
		{"a switched-on fault wins over its own fault type", Facts{Surface: SurfaceVsan, Method: "VsanPerfQueryPerf", Status: 500, Faulted: true, FaultType: "InvalidArgument", Injected: true}, ReasonFault},
		{"no answer at all, on purpose", Facts{Surface: SurfaceVsan, Method: "VsanPerfQueryPerf", Status: 200, Injected: true}, ReasonFault},
		{"the performance service switched off", Facts{Surface: SurfaceVsan, Method: "VsanPerfQueryPerf", Status: 500, Faulted: true, FaultType: "NotFound", PerfOff: true}, ReasonFault},
		{"a system error nobody asked for", Facts{Surface: SurfaceVsan, Method: "VsanPerfQueryPerf", Status: 500, Faulted: true, FaultType: "SystemError"}, ReasonUnknown},
		{"a vSAN object the simulator does not model", Facts{Surface: SurfaceVsan, Method: "VsanQueryObjectIdentities", Status: 500, Faulted: true, FaultType: "ManagedObjectNotFound", ObjectType: "VsanObjectSystem"}, ReasonUnknown},
		{"a wrong password", Facts{Surface: SurfaceSDK, Method: "Login", Status: 500, Faulted: true, FaultType: "InvalidLogin"}, ReasonDenied},
		{"a request that is not signed in", Facts{Surface: SurfaceVsan, Method: "VsanPerfQueryPerf", Status: 500, Faulted: true, FaultType: "NotAuthenticated"}, ReasonDenied},
		{"not signed in, even with a fault switched on", Facts{Surface: SurfaceVsan, Method: "VsanPerfQueryPerf", Status: 500, Faulted: true, FaultType: "NotAuthenticated", PerfOff: true}, ReasonDenied},
		{"a REST request without a session", Facts{Surface: SurfaceREST, HTTPMethod: "GET", Path: "/api/vcenter/vm", Status: 401}, ReasonDenied},
		{"a method vcsim does not implement", Facts{Surface: SurfaceSDK, Method: "QueryPerfCounterByLevel", Status: 500, Faulted: true, FaultType: "MethodNotFound"}, ReasonUnknown},
		{"a property the simulator does not have", Facts{Surface: SurfaceSDK, Method: "RetrievePropertiesEx", Status: 500, Faulted: true, FaultType: "InvalidProperty"}, ReasonUnknown},
		{"a manager the simulator does not have", Facts{Surface: SurfaceSDK, Method: "QueryAssignedLicenses", Status: 500, Faulted: true, FaultType: "ManagedObjectNotFound", ObjectType: "LicenseAssignmentManager"}, ReasonUnknown},
		{"a cluster that was removed, as on a real vCenter", Facts{Surface: SurfaceSDK, Method: "RetrieveProperties", Status: 500, Faulted: true, FaultType: "ManagedObjectNotFound", ObjectType: "ClusterComputeResource"}, ReasonRefused},
		{"a read-only user asking to change something", Facts{Surface: SurfaceSDK, Method: "PowerOffVM_Task", Status: 500, Faulted: true, FaultType: "NoPermission"}, ReasonRefused},
		{"a bad argument, as a real vCenter answers it", Facts{Surface: SurfaceSDK, Method: "CreateContainerView", Status: 500, Faulted: true, FaultType: "InvalidArgument"}, ReasonRefused},
		{"a path nothing serves", Facts{Surface: SurfaceOther, HTTPMethod: "POST", Path: "/ls/sdk", Status: 404}, ReasonUnknown},
		{"the older REST interface", Facts{Surface: SurfaceREST, HTTPMethod: "GET", Path: "/rest/vcenter/namespace-management/clusters", Status: 404}, ReasonUnknown},
		{"a document the simulator does not have", Facts{Surface: SurfaceSDK, HTTPMethod: "GET", Path: "/sdk/vim.wsdl", Status: 404}, ReasonUnknown},
		{"a browser visiting /sdk, as a real vCenter answers it", Facts{Surface: SurfaceSDK, HTTPMethod: "GET", Path: "/sdk", Status: 404}, ReasonRefused},
		{"a browser visiting /vsanHealth", Facts{Surface: SurfaceVsan, HTTPMethod: "GET", Path: "/vsanHealth", Status: 501}, ReasonRefused},
		{"a REST path a real vCenter denies a read-only session", Facts{Surface: SurfaceREST, HTTPMethod: "GET", Path: "/api/content/library", Status: 403}, ReasonRefused},
		{"a REST filter a real vCenter rejects", Facts{Surface: SurfaceREST, HTTPMethod: "GET", Path: "/api/vcenter/vm", Status: 400}, ReasonRefused},
		{"a REST change the read-only interface cannot make", Facts{Surface: SurfaceREST, HTTPMethod: "POST", Path: "/api/vcenter/vm", Status: 405}, ReasonUnknown},
		{"a server error without a fault", Facts{Surface: SurfaceSDK, HTTPMethod: "POST", Path: "/sdk", Status: 500}, ReasonUnknown},
		{"paused", Facts{Surface: SurfaceSDK, Paused: true}, ReasonPaused},
	}
	for _, c := range cases {
		if got := Classify(c.f); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBodyReaders(t *testing.T) {
	login := []byte(`<soapenv:Body><Login xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><userName> ops@vsphere.local </userName><password>never-read</password></Login></soapenv:Body>`)
	if got := LoginUser(login); got != "ops@vsphere.local" {
		t.Errorf("login user %q", got)
	}
	if got := ObjectType(login); got != "SessionManager" {
		t.Errorf("object type %q", got)
	}
	if LoginUser([]byte("nothing")) != "" || ObjectType([]byte("nothing")) != "" {
		t.Error("something was read out of nothing")
	}
}

// The Login body is written by an XML encoder, which escapes quotes and
// ampersands; the account name is what a person typed.
func TestLoginUserIsTheNameThatWasSent(t *testing.T) {
	type login struct {
		XMLName  xml.Name `xml:"Login"`
		UserName string   `xml:"userName"`
	}
	for _, name := range []string{"o'brien@vsphere.local", `a"b&c@vsphere.local`, "<x>@vsphere.local"} {
		body, err := xml.Marshal(login{UserName: name})
		if err != nil {
			t.Fatal(err)
		}
		if got := LoginUser(body); got != name {
			t.Errorf("sent %q, read %q from %s", name, got, body)
		}
	}
}
