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

// Made-up secrets. None of them may ever be written anywhere.
const (
	secretUser      = "svc-collector@made-up.example"
	secretPassword  = "Tr0ub4dor-and-then-some"
	secretTicket    = "cst-VCT-52f3a0c1-made-up-ticket-0001"
	secretClone     = "cst-VCT-clone-made-up-ticket-0002"
	secretAssertion = "made-up-assertion-subject"
	secretTrust     = "MIIDmadeUpCertificateBody"
	secretSession   = "made-up-session-cookie-value"
	secretBinary    = "bWFkZS11cC1iaW5hcnktdG9rZW4="
)

var allSecrets = []string{secretUser, secretPassword, secretTicket, secretClone, secretAssertion, secretTrust, secretSession, secretBinary}

func sdkBody(op, inner string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body>` +
		`<` + op + ` xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this>` + inner + `</` + op + `></soapenv:Body></soapenv:Envelope>`
}

// tokenBody is a token request that carries one of everything: a user name
// and password, a binary token, and an assertion presented for renewal.
const tokenBody = `<S:Envelope xmlns:S="http://schemas.xmlsoap.org/soap/envelope/"><S:Header>` +
	`<wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd" xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">` +
	`<wsu:Timestamp wsu:Id="_ts"><wsu:Created>2026-11-16T00:00:00.000Z</wsu:Created><wsu:Expires>2026-11-16T00:05:00.000Z</wsu:Expires></wsu:Timestamp>` +
	`<wsse:UsernameToken><wsse:Username>` + secretUser + `</wsse:Username><wsse:Password>` + secretPassword + `</wsse:Password></wsse:UsernameToken>` +
	`<wsse:BinarySecurityToken ValueType="x509">` + secretBinary + `</wsse:BinarySecurityToken>` +
	`<saml2:Assertion xmlns:saml2="urn:oasis:names:tc:SAML:2.0:assertion" ID="_a1"><saml2:Subject><saml2:NameID>` + secretAssertion + `</saml2:NameID></saml2:Subject></saml2:Assertion>` +
	`</wsse:Security></S:Header><S:Body>` +
	`<RequestSecurityToken xmlns="http://docs.oasis-open.org/ws-sx/ws-trust/200512">` +
	`<TokenType>urn:oasis:names:tc:SAML:2.0:assertion</TokenType><RequestType>http://docs.oasis-open.org/ws-sx/ws-trust/200512/Issue</RequestType>` +
	`<Lifetime><wsu:Created xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">2026-11-16T00:00:00.000Z</wsu:Created></Lifetime>` +
	`<Renewing Allow="false" OK="false"/><Delegatable>false</Delegatable>` +
	`<KeyType>http://docs.oasis-open.org/ws-sx/ws-trust/200512/Bearer</KeyType>` +
	`<OnBehalfOf><Address>` + secretUser + `</Address></OnBehalfOf>` +
	`</RequestSecurityToken></S:Body></S:Envelope>`

// secretRequests is one request of each kind the capture logs, each
// carrying secrets.
var secretRequests = []struct{ method, path, body string }{
	{"POST", "/sts/STSService/vsphere.local", tokenBody},
	{"POST", "/sdk", sdkBody("LoginBySessionTicket", `<ticketData>`+secretTicket+`</ticketData>`)},
	{"POST", "/sdk", sdkBody("CloneSession", `<cloneTicket>`+secretClone+`</cloneTicket>`)},
	{"POST", "/sdk", sdkBody("AcquireCloneTicket", ``)},
	{"POST", "/sdk", sdkBody("AcquireSessionTicket", `<serviceKey>`+secretTicket+`</serviceKey>`)},
	{"POST", "/sdk", sdkBody("Login", `<userName>`+secretUser+`</userName><password>`+secretPassword+`</password>`)},
	{"POST", "/lookupservice/sdk", `<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body><Create xmlns="urn:internallookup">` +
		`<_this type="LookupServiceRegistration">ServiceRegistration</_this><serviceId>made-up</serviceId><createSpec><ownerId>` + secretUser + `</ownerId>` +
		`<serviceEndpoints><url>https://made-up.example/sdk</url><sslTrust>` + secretTrust + `</sslTrust></serviceEndpoints></createSpec></Create></soapenv:Body></soapenv:Envelope>`},
	{"POST", "/sso-adminserver/sdk/vsphere.local", `<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body><Login xmlns="urn:sso">` +
		`<_this type="SsoSessionManager">ssoSessionManager</_this><userName>` + secretUser + `</userName><password>` + secretPassword + `</password></Login></soapenv:Body></soapenv:Envelope>`},
	{"POST", "/api", `{"jsonrpc":"2.0","method":"invoke","id":"1","params":{"serviceId":"com.vmware.cis.session","operationId":"create",` +
		`"ctx":{"securityCtx":{"schemeId":"com.vmware.vapi.std.security.user_pass","userName":"` + secretUser + `","password":"` + secretPassword + `"}},"input":{"STRUCTURE":{"operation-input":{}}}}}`},
	{"POST", "/ls/sdk", `<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body><Login xmlns="urn:internalcis.license">` +
		`<_this type="CisLicenseSessionManagementService">cis.license.SessionManagementService</_this><username>` + secretUser + `</username><password>` + secretPassword + `</password></Login></soapenv:Body></soapenv:Envelope>`},
}

// No password, ticket, assertion, user name, certificate or session value
// reaches anything the capture writes: not the log on stdout, not the list
// of unanswered requests, not the full log and its file. Every request is
// sent twice, answered and unanswered, because the two are kept in
// different places.
func TestNothingSecretIsLogged(t *testing.T) {
	var stdout bytes.Buffer
	dir := t.TempDir()
	c, err := NewCapture(dir, "", Settings{Full: true, Unanswered: true, Stdout: true, Debug: true}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusInternalServerError} {
		h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			if status == http.StatusInternalServerError {
				_, _ = w.Write([]byte(`<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body><soapenv:Fault><faultcode>ServerFaultCode</faultcode><faultstring>no</faultstring><detail><InvalidRequestFault xmlns="urn:vim25" xsi:type="InvalidRequest"/></detail></soapenv:Fault></soapenv:Body></soapenv:Envelope>`))
			}
		}))
		for _, rq := range secretRequests {
			req := httptest.NewRequest(rq.method, rq.path+"?ticket="+secretTicket, strings.NewReader(rq.body))
			req.Header.Set("Content-Type", "text/xml; charset=utf-8")
			req.Header.Set("Cookie", `vmware_soap_session="`+secretSession+`"`)
			req.Header.Set("Authorization", "Bearer "+secretSession)
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
	}
	file, err := os.ReadFile(c.State().Path)
	if err != nil {
		t.Fatal(err)
	}
	held, err := json.Marshal([]any{c.Records(false), c.Records(true)})
	if err != nil {
		t.Fatal(err)
	}
	var downloads bytes.Buffer
	_ = c.WriteJSONL(&downloads, true)
	_ = c.WriteJSONL(&downloads, false)
	for where, text := range map[string]string{"stdout": stdout.String(), "the full log file": string(file), "the held records": string(held), "the downloads": downloads.String()} {
		if text == "" {
			t.Errorf("%s is empty: the test wrote nothing there", where)
		}
		for _, secret := range allSecrets {
			if strings.Contains(text, secret) {
				t.Errorf("%s holds the secret %q", where, secret)
			}
		}
	}
	// and the log is not empty of meaning: every request is in it
	if n := strings.Count(stdout.String(), `"event":"request-capture"`); n != 3*(len(secretRequests)-1) {
		t.Errorf("%d requests logged on stdout, want %d (every request but the /sdk Login, three times)", n, 3*(len(secretRequests)-1))
	}
}

func debugLines(t *testing.T, out *bytes.Buffer) []debugRecord {
	t.Helper()
	var recs []debugRecord
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var rec debugRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		recs = append(recs, rec)
	}
	return recs
}

// The capture logs the lookup, sign-on admin and token services, a POST to
// /api, and the four session operations of /sdk. It logs nothing else.
func TestDebugCaptureWhatIsLogged(t *testing.T) {
	c, out := debugCapture(t, Settings{Debug: true})
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	send := func(method, path, body string) {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, strings.NewReader(body)))
	}
	logged := [][3]string{
		{"POST", "/lookupservice/sdk", `<e:Envelope xmlns:e="http://schemas.xmlsoap.org/soap/envelope/"><e:Body><List xmlns="urn:internallookup"/></e:Body></e:Envelope>`},
		{"GET", "/lookupservice/mob", ""},
		{"POST", "/sso-adminserver/sdk/vsphere.local", `<e:Envelope xmlns:e="http://schemas.xmlsoap.org/soap/envelope/"><e:Body><GetIssuersCertificates xmlns="urn:sso"/></e:Body></e:Envelope>`},
		{"POST", "/sts/STSService/vsphere.local", tokenBody},
		{"POST", "/api", `{"jsonrpc":"2.0"}`},
		{"POST", "/ls/sdk", `<e:Envelope xmlns:e="http://schemas.xmlsoap.org/soap/envelope/"><e:Body><SearchAssets xmlns="urn:internalcis.license"/></e:Body></e:Envelope>`},
		{"POST", "/sdk", sdkBody("LoginBySessionTicket", `<ticketData>x</ticketData>`)},
		{"POST", "/sdk", sdkBody("AcquireSessionTicket", ``)},
		{"POST", "/sdk", sdkBody("AcquireCloneTicket", ``)},
		{"POST", "/sdk", sdkBody("CloneSession", `<cloneTicket>x</cloneTicket>`)},
	}
	for _, rq := range logged {
		send(rq[0], rq[1], rq[2])
	}
	recs := debugLines(t, out)
	if len(recs) != len(logged) {
		t.Fatalf("%d requests logged, want %d", len(recs), len(logged))
	}
	wantOps := []string{"List", "", "GetIssuersCertificates", "RequestSecurityToken", "", "SearchAssets", "LoginBySessionTicket", "AcquireSessionTicket", "AcquireCloneTicket", "CloneSession"}
	for i, rec := range recs {
		if rec.Method != logged[i][0] || rec.Path != logged[i][1] || rec.Operation != wantOps[i] {
			t.Errorf("record %d is %s %s operation %q, want %s %s operation %q", i, rec.Method, rec.Path, rec.Operation, logged[i][0], logged[i][1], wantOps[i])
		}
	}

	out.Reset()
	for _, rq := range [][3]string{
		{"POST", "/sdk", sdkBody("RetrieveProperties", `<specSet/>`)},
		{"POST", "/sdk", sdkBody("Login", `<userName>u</userName><password>p</password>`)},
		// names a session operation without being one
		{"POST", "/sdk", sdkBody("FindExtension", `<extensionKey>AcquireCloneTicket</extensionKey>`)},
		{"GET", "/sdk", ""},
		{"GET", "/api", ""},
		{"POST", "/api/session", ""},
		{"GET", "/api/vcenter/cluster", ""},
		{"POST", "/vsanHealth", sdkBody("CloneSession", ``)},
		{"POST", "/stsx", ""},
		{"POST", "/lsx/sdk", ""},
	} {
		send(rq[0], rq[1], rq[2])
	}
	if out.Len() != 0 {
		t.Errorf("requests that are not about licensing or sign-on were logged: %s", out.String())
	}
}

// A session operation is logged with whether a session cookie came with
// it, and a refused one with the fault it was refused with. A refusal is
// not an unanswered request, so this log is the only place it shows.
func TestDebugCaptureCookieAndRefusal(t *testing.T) {
	c, out := debugCapture(t, Settings{Debug: true, Unanswered: true})
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("vmware_soap_session"); err == nil {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body><AcquireCloneTicketResponse xmlns="urn:vim25"><returnval>` + secretClone + `</returnval></AcquireCloneTicketResponse></soapenv:Body></soapenv:Envelope>`))
			return
		}
		// the handler knows this is a refusal, as the controller does
		Note(r, false, "192.0.2.1", "made-up-agent")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><soapenv:Body><soapenv:Fault><faultcode>ServerFaultCode</faultcode><faultstring>The session is not authenticated.</faultstring><detail><NotAuthenticatedFault xmlns="urn:vim25" xsi:type="NotAuthenticated"/></detail></soapenv:Fault></soapenv:Body></soapenv:Envelope>`))
	}))
	refused := httptest.NewRequest("POST", "/sdk", strings.NewReader(sdkBody("AcquireCloneTicket", ``)))
	h.ServeHTTP(httptest.NewRecorder(), refused)
	answered := httptest.NewRequest("POST", "/sdk", strings.NewReader(sdkBody("AcquireCloneTicket", ``)))
	answered.Header.Set("Cookie", `vmware_soap_session="`+secretSession+`"`)
	h.ServeHTTP(httptest.NewRecorder(), answered)

	recs := debugLines(t, out)
	if len(recs) != 2 {
		t.Fatalf("%d records, want 2", len(recs))
	}
	if r := recs[0]; r.Operation != "AcquireCloneTicket" || r.Status != http.StatusInternalServerError || r.Fault != "NotAuthenticated" || r.CookieSent {
		t.Errorf("the refusal: %+v", r)
	}
	if r := recs[1]; r.Operation != "AcquireCloneTicket" || r.Status != http.StatusOK || r.Fault != "" || !r.CookieSent {
		t.Errorf("the answered one: %+v", r)
	}
	if !strings.Contains(out.String(), `"cookieSent":false`) || !strings.Contains(out.String(), `"cookieSent":true`) {
		t.Errorf("cookieSent is not written both ways: %s", out.String())
	}
	if strings.Contains(out.String(), secretClone) || strings.Contains(out.String(), secretSession) {
		t.Errorf("the ticket of the answer or the cookie was logged: %s", out.String())
	}
	// the refusal is in no other list
	if n := len(c.Records(true)); n != 0 {
		t.Errorf("%d unanswered records, want none: a refusal is not unanswered", n)
	}
}

// A token request keeps its shape in the log: every element, the fixed
// names it asks for and its times, with the text of everything else gone.
func TestTokenRequestKeepsItsShape(t *testing.T) {
	got := scrubBodyAt("/sts/STSService/vsphere.local", []byte(tokenBody))
	for _, keep := range []string{
		`<wsse:UsernameToken><wsse:Username>[scrubbed:text len=`, `</wsse:Username><wsse:Password>[scrubbed:text len=`,
		`<wsse:BinarySecurityToken ValueType="x509">[scrubbed:text len=`,
		`<saml2:Assertion xmlns:saml2="urn:oasis:names:tc:SAML:2.0:assertion" ID="_a1">[scrubbed:Assertion len=`, `</saml2:Assertion>`,
		`<TokenType>urn:oasis:names:tc:SAML:2.0:assertion</TokenType>`,
		`<RequestType>http://docs.oasis-open.org/ws-sx/ws-trust/200512/Issue</RequestType>`,
		`<wsu:Created>2026-11-16T00:00:00.000Z</wsu:Created>`, `<Renewing Allow="false" OK="false"/>`, `<Delegatable>false</Delegatable>`,
		`<KeyType>http://docs.oasis-open.org/ws-sx/ws-trust/200512/Bearer</KeyType>`, `<OnBehalfOf><Address>[scrubbed:text len=`, `</S:Envelope>`,
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("lost %q:\n%s", keep, got)
		}
	}
	for _, secret := range allSecrets {
		if strings.Contains(got, secret) {
			t.Errorf("kept the secret %q:\n%s", secret, got)
		}
	}
	if strings.Contains(got, "Subject") {
		t.Errorf("kept the inside of the assertion:\n%s", got)
	}
	// a token request that is cut off or broken gives nothing away
	for _, in := range []string{
		tokenBody[:strings.Index(tokenBody, secretPassword)+8],
		strings.Replace(tokenBody, "</wsse:Password>", "</wsse:Passw", 1),
		strings.Replace(tokenBody, `<saml2:Subject>`, `<saml2:Subject`, 1),
	} {
		got := scrubBodyAt("/sts/STSService/vsphere.local", []byte(in))
		for _, secret := range allSecrets {
			if strings.Contains(got, secret) || strings.Contains(got, secret[:8]) {
				t.Errorf("a broken request kept %q:\n%s", secret, got)
			}
		}
	}
	// anywhere else the same body is scrubbed as before: the whole of the
	// Security header and of the request go
	elsewhere := scrubBodyAt("/ls/sdk", []byte(tokenBody))
	if strings.Contains(elsewhere, "UsernameToken") || !strings.Contains(elsewhere, "[scrubbed:Security len=") {
		t.Errorf("outside the token service: %s", elsewhere)
	}
}
