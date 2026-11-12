package sim_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/querylog"
	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/serve"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// loginHeader is the 9.1 form: a SOAP Header with an unqualified operationID.
const loginHeader = `<soapenv:Header><operationID xsi:type="string">00000000-0000-4000-8000-0000000000000000000000000000</operationID></soapenv:Header>`

const loginBody = `<Login xmlns="urn:internalcis.license">` +
	`<_this type="CisLicenseSessionManagementService">cis.license.SessionManagementService</_this>` +
	`<username>operator</username><password>s3cret</password></Login>`

func licenseEnvelope(t *testing.T, header, body string) []byte {
	t.Helper()
	return []byte(`<?xml version='1.0' encoding='UTF-8'?>` +
		`<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"` +
		` xmlns:soapenc="http://schemas.xmlsoap.org/soap/encoding/"` +
		` xmlns:xsd="http://www.w3.org/2001/XMLSchema"` +
		` xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">` +
		header + `<soapenv:Body>` + body + `</soapenv:Body></soapenv:Envelope>`)
}

// serviceSim is the simulator plus the certificate the listener serves.
type serviceSim struct {
	t   *testing.T
	srv *httptest.Server
}

func startServices(t *testing.T) serviceSim {
	t.Helper()
	return startServicesLogged(t, nil)
}

// startServicesLogged builds the same simulator behind an optional capture.
func startServicesLogged(t *testing.T, capture *querylog.Capture) serviceSim {
	t.Helper()
	sc := scenario.Phase1("services")
	topo := topology.Generate(sc, registry.InMemory(sc.Seed))
	cert, err := newCertificate(t)
	if err != nil {
		t.Fatal(err)
	}
	s, err := sim.New(sim.Options{
		Topology: topo, Username: "operator", Password: "s3cret",
		Credentials: map[string]string{"operator": "s3cret"}, Certificate: cert,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var next http.Handler = s.Handler
	if capture != nil {
		next = capture.Wrap(s.Handler)
	}
	srv := httptest.NewServer(next)
	t.Cleanup(srv.Close)
	return serviceSim{t: t, srv: srv}
}

// newCertificate is the certificate the listener serves, from a temp directory.
func newCertificate(t *testing.T) (*serve.Certificate, error) {
	t.Helper()
	return serve.LoadOrCreateCertificate(t.TempDir(), []string{"localhost"})
}

func postTo(t *testing.T, srv *httptest.Server, path, soapAction string, body []byte, cookies ...*http.Cookie) (*http.Response, []byte) {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "text/xml; charset=utf-8")
	if soapAction != "" {
		r.Header.Set("SOAPAction", soapAction)
	}
	for _, ck := range cookies {
		r.AddCookie(ck)
	}
	res, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return res, []byte(out.String())
}

// Both captured Login forms answer the empty LoginResponse and set the /ls
// cookie with the observed attributes; a wrong password never returns success.
func TestLicenseServiceLoginBothForms(t *testing.T) {
	e := startServices(t)
	for _, tc := range []struct{ name, header string }{
		{"9.1 header form", loginHeader},
		{"9.0.2 no header", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := postTo(t, e.srv, "/ls/sdk", `"urn:internalcis.license/4.0"`, licenseEnvelope(t, tc.header, loginBody))
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
			}
			if ct := res.Header.Get("Content-Type"); ct != "text/xml;charset=utf-8" {
				t.Errorf("Content-Type %q", ct)
			}
			text := string(body)
			for _, want := range []string{`<soapenv:Body>`, `<LoginResponse xmlns="urn:internalcis.license">`} {
				if !strings.Contains(text, want) {
					t.Errorf("body %q missing %q", text, want)
				}
			}
			if strings.Contains(text, "returnval") || strings.Contains(text, "LoginResponse xmlns=\"urn:internalcis.license\" ") {
				t.Errorf("LoginResponse must be empty: %s", text)
			}
			cookie := licenseCookieValue(t, res)
			if len(cookie.Value) != 36 {
				t.Errorf("cookie value length %d, want 36", len(cookie.Value))
			}
		})
	}

	t.Run("wrong password", func(t *testing.T) {
		bad := strings.Replace(loginBody, "s3cret</password>", "nope</password>", 1)
		res, body := postTo(t, e.srv, "/ls/sdk", `"urn:internalcis.license/4.0"`, licenseEnvelope(t, loginHeader, bad))
		if res.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500 for a bad login: %s", res.StatusCode, body)
		}
		if strings.Contains(string(body), "LoginResponse") {
			t.Errorf("a bad login returned success: %s", body)
		}
		if !strings.Contains(string(body), "InvalidRequest") {
			t.Errorf("fault detail missing: %s", body)
		}
		if len(res.Cookies()) != 0 {
			t.Errorf("a refused login should not issue a session: %v", res.Cookies())
		}
	})
}

func licenseCookieValue(t *testing.T, res *http.Response) *http.Cookie {
	t.Helper()
	for _, ck := range res.Cookies() {
		if ck.Name == "vmware_soap_session" {
			if ck.Path != "/ls" || !ck.Secure || !ck.HttpOnly {
				t.Errorf("cookie attributes: path %q secure %v httpOnly %v", ck.Path, ck.Secure, ck.HttpOnly)
			}
			if !ck.Expires.IsZero() {
				t.Errorf("cookie has an Expires: %v", ck.Expires)
			}
			if ck.Domain != "" {
				t.Errorf("cookie has a Domain: %q", ck.Domain)
			}
			return ck
		}
	}
	t.Fatalf("no vmware_soap_session cookie in %v", res.Cookies())
	return nil
}

// Logout answers the empty LogoutResponse, reissues the same cookie, and the
// session stops working server-side. The license cookie does not authenticate /sdk.
func TestLicenseServiceLogoutAndSessionScope(t *testing.T) {
	e := startServices(t)
	res, body := postTo(t, e.srv, "/ls/sdk", `"urn:internalcis.license/4.0"`, licenseEnvelope(t, loginHeader, loginBody))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login %d: %s", res.StatusCode, body)
	}
	cookie := licenseCookieValue(t, res)

	logout := `<Logout xmlns="urn:internalcis.license"><_this type="CisLicenseSessionManagementService">cis.license.SessionManagementService</_this></Logout>`
	res2, body2 := postTo(t, e.srv, "/ls/sdk", `"urn:internalcis.license/4.0"`, licenseEnvelope(t, "", logout), cookie)
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("logout %d: %s", res2.StatusCode, body2)
	}
	if !strings.Contains(string(body2), `<LogoutResponse xmlns="urn:internalcis.license">`) {
		t.Errorf("logout body %q", body2)
	}
	if again := licenseCookieValue(t, res2); again.Value != cookie.Value {
		t.Errorf("logout reissued %q, want the same value %q", again.Value, cookie.Value)
	}

	// the same cookie no longer authenticates a license Logout
	res3, body3 := postTo(t, e.srv, "/ls/sdk", `"urn:internalcis.license/4.0"`, licenseEnvelope(t, "", logout), cookie)
	if res3.StatusCode != http.StatusInternalServerError {
		t.Errorf("repeat logout %d, want a fault after invalidation: %s", res3.StatusCode, body3)
	}

	// the license cookie does not authenticate /sdk: currentSession stays empty
	sdk := `<?xml version='1.0' encoding='UTF-8'?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body>` +
		`<Fetch xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><prop>currentSession</prop></Fetch>` +
		`</soapenv:Body></soapenv:Envelope>`
	res4, body4 := postTo(t, e.srv, "/sdk", `"urn:vim25/8.0"`, []byte(sdk), cookie)
	if res4.StatusCode != http.StatusOK {
		t.Fatalf("sdk fetch %d: %s", res4.StatusCode, body4)
	}
	if strings.Contains(string(body4), "UserSession") || strings.Contains(string(body4), "returnval") {
		t.Errorf("the license cookie authenticated /sdk: %s", body4)
	}
}

// The lookup service answers its service content anonymously with four references.
func TestLookupServiceContent(t *testing.T) {
	e := startServices(t)
	body := `<?xml version='1.0' encoding='UTF-8'?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body>` +
		`<RetrieveServiceContent xmlns="urn:internallookup"><_this type="LookupServiceInstance">ServiceInstance</_this>` +
		`</RetrieveServiceContent></soapenv:Body></soapenv:Envelope>`
	res, out := postTo(t, e.srv, "/lookupservice/sdk", `"urn:internallookup/2.0"`, []byte(body))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, out)
	}
	text := string(out)
	if !strings.Contains(text, `<soapenv:Body>`) {
		t.Errorf("no SOAP Body: %s", text)
	}
	wants := []struct {
		element string
		typ     string
		value   string
	}{
		{"lookupService", "LookupLookupService", "lookupService"},
		{"serviceRegistration", "LookupServiceRegistration", "serviceRegistration"},
		{"deploymentInformationService", "LookupDeploymentInformationService", "deploymentInformationService"},
		{"l10n", "LookupL10n", "l10n"},
	}
	for _, w := range wants {
		open := "<" + w.element + ` type="` + w.typ + `">` + w.value + "</" + w.element + ">"
		scoped := "<" + w.element + ` xmlns="urn:internallookup" type="` + w.typ + `">` + w.value + "</" + w.element + ">"
		if n := strings.Count(text, open) + strings.Count(text, scoped); n != 1 {
			t.Errorf("%s appears %d times, want once, in %s", open, n, text)
		}
	}
	// the observed reference text lengths are 13, 19, 28 and 4 characters
	for _, w := range wants {
		if len(w.value) == 0 {
			t.Errorf("empty reference for %s", w.element)
		}
	}
	for _, pair := range [][2]int{{len("lookupService"), 13}, {len("serviceRegistration"), 19}, {len("deploymentInformationService"), 28}, {len("l10n"), 4}} {
		if pair[0] != pair[1] {
			t.Errorf("reference length %d, want %d", pair[0], pair[1])
		}
	}
	// an unclassified lookup operation stays unanswered, as captured
	res2, _ := postTo(t, e.srv, "/lookupservice/sdk", `"urn:internallookup/2.0"`, []byte(`<?xml version='1.0'?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body><List xmlns="urn:internallookup"/></soapenv:Body></soapenv:Envelope>`))
	if res2.StatusCode != http.StatusNotFound {
		t.Errorf("List status %d, want the previous 404", res2.StatusCode)
	}
}

// The certificate-authority root needs a REST session and carries the
// simulator's own certificate; the same header works on /rest/ as on /api/.
func TestCertificateAuthorityRoot(t *testing.T) {
	e := startServices(t)
	token := restToken(t, e.srv)
	for _, tc := range []struct {
		path string
		want int
	}{
		// the REST form, its numbered spellings, and the /api form the REST
		// handler keeps answering behind this one
		{"/rest/vcenter/certificate-authority/get-root", http.StatusOK},
		{"/rest/2.0/vcenter/certificate-authority/get-root", http.StatusOK},
		{"/rest/1.0/vcenter/certificate-authority/get-root", http.StatusOK},
		{"/api/vcenter/certificate-authority/get-root", http.StatusOK},
	} {
		path, want := tc.path, tc.want
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
		res, err := e.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s anonymous status %d, want 401", path, res.StatusCode)
		}
		res.Body.Close()

		req, _ = http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
		req.Header.Set("vmware-api-session-id", token)
		res, err = e.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s with a session status %d, want %d", path, res.StatusCode, want)
		}
		if want != http.StatusOK {
			continue
		}
		var got map[string]string
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		pemText, ok := got["value"]
		if !ok || !strings.HasPrefix(pemText, "-----BEGIN CERTIFICATE-----") ||
			!strings.HasSuffix(strings.TrimSpace(pemText), "-----END CERTIFICATE-----") {
			t.Fatalf("%s: not a PEM certificate: %q", path, pemText)
		}
	}
}

func restToken(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/session", nil)
	req.SetBasicAuth("operator", "s3cret")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var token string
	if err := json.NewDecoder(res.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}
	return token
}

// An anonymous or logged-out currentSession read answers an empty FetchResponse;
// a signed-in caller still gets the UserSession.
func TestCurrentSessionNullAndUserSession(t *testing.T) {
	e := startServices(t)
	fetch := func(key string) string {
		this := "SessionManager"
		if key != "" {
			this = key
		}
		return `<?xml version='1.0' encoding='UTF-8'?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body>` +
			`<Fetch xmlns="urn:vim25"><_this type="SessionManager">` + this + `</_this><prop>currentSession</prop></Fetch></soapenv:Body></soapenv:Envelope>`
	}

	// anonymously: 200 with an empty FetchResponse
	res, body := postTo(t, e.srv, "/sdk", `"urn:vim25/8.0"`, []byte(fetch("")))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("anonymous fetch status %d: %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "FetchResponse") || strings.Contains(string(body), "returnval") {
		t.Errorf("anonymous body should be an empty FetchResponse: %s", body)
	}
	if strings.Contains(string(body), "NotAuthenticated") {
		t.Errorf("anonymous body should not be a fault: %s", body)
	}

	// signed in: the UserSession, and empty again after logout
	soapCookie := sdkLogin(t, e.srv)
	res, body = postTo(t, e.srv, "/sdk", `"urn:vim25/8.0"`, []byte(fetch(soapCookie)), &http.Cookie{Name: "vmware_soap_session", Value: soapCookie})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("signed-in fetch status %d: %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "UserSession") {
		t.Errorf("signed-in body should carry the UserSession: %s", body)
	}

	logout := `<?xml version='1.0' encoding='UTF-8'?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body>` +
		`<Logout xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this></Logout></soapenv:Body></soapenv:Envelope>`
	res, body = postTo(t, e.srv, "/sdk", `"urn:vim25/8.0"`, []byte(logout), &http.Cookie{Name: "vmware_soap_session", Value: soapCookie})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("logout status %d: %s", res.StatusCode, body)
	}
	res, body = postTo(t, e.srv, "/sdk", `"urn:vim25/8.0"`, []byte(fetch(soapCookie)), &http.Cookie{Name: "vmware_soap_session", Value: soapCookie})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("logged-out fetch status %d: %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "FetchResponse") || strings.Contains(string(body), "UserSession") {
		t.Errorf("logged-out body should be empty: %s", body)
	}
}

func sdkLogin(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	body := `<?xml version='1.0' encoding='UTF-8'?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body>` +
		`<Login xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><userName>operator</userName><password>s3cret</password></Login>` +
		`</soapenv:Body></soapenv:Envelope>`
	res, out := postTo(t, srv, "/sdk", `"urn:vim25/8.0"`, []byte(body))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("sdk login %d: %s", res.StatusCode, out)
	}
	for _, ck := range res.Cookies() {
		if ck.Name == "vmware_soap_session" {
			return ck.Value
		}
	}
	t.Fatalf("no sdk session cookie in %v", res.Cookies())
	return ""
}

// Every other license-service operation keeps today's answer.
func TestLicenseServiceOtherOperationsUnanswered(t *testing.T) {
	e := startServices(t)
	other := `<?xml version='1.0' encoding='UTF-8'?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body>` +
		`<RetrieveServiceContent xmlns="urn:internalcis.license"><_this type="CisLicenseSessionManagementService">cis.license.SessionManagementService</_this>` +
		`</RetrieveServiceContent></soapenv:Body></soapenv:Envelope>`
	res, _ := postTo(t, e.srv, "/ls/sdk", `"urn:internalcis.license/4.0"`, []byte(other))
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", res.StatusCode)
	}
	res, _ = postTo(t, e.srv, "/ls/sdk", `"urn:internalcis.license/4.0"`, licenseEnvelope(t, loginHeader, loginBody))
	if res.StatusCode != http.StatusOK {
		t.Errorf("login after an unanswered operation: %d", res.StatusCode)
	}
}

// The request capture records the operation name of a license-service request,
// whether it was answered locally or left unanswered.
func TestLicenseRequestCaptureRecordsOperation(t *testing.T) {
	capture, err := querylog.NewCapture(t.TempDir(), "", querylog.Settings{Full: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := startServicesLogged(t, capture)
	if _, _ = postTo(t, e.srv, "/ls/sdk", `"urn:internalcis.license/4.0"`, licenseEnvelope(t, loginHeader, loginBody)); false {
		t.Fatal("post failed")
	}
	other := `<?xml version='1.0' encoding='UTF-8'?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body>` +
		`<RetrieveServiceContent xmlns="urn:internalcis.license"><_this type="CisLicenseSessionManagementService">cis.license.SessionManagementService</_this>` +
		`</RetrieveServiceContent></soapenv:Body></soapenv:Envelope>`
	if _, _ = postTo(t, e.srv, "/ls/sdk", `"urn:internalcis.license/4.0"`, []byte(other)); false {
		t.Fatal("post failed")
	}
	records := capture.Records(false)
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if records[0].Operation != "Login" || records[0].Path != "/ls/sdk" {
		t.Errorf("login record = %+v, want the Login operation on /ls/sdk", records[0])
	}
	if records[1].Operation != "RetrieveServiceContent" || records[1].Path != "/ls/sdk" {
		t.Errorf("unanswered record = %+v, want the RetrieveServiceContent operation", records[1])
	}
}
