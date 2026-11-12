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

const loginEnvelope = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" xmlns:ds="http://www.w3.org/2000/09/xmldsig#" xmlns:ls="urn:cislicense">
<soapenv:Header><wsse:Security><wsse:BinarySecurityToken ValueType="x">QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=</wsse:BinarySecurityToken><saml:Assertion ID="_a1" IssueInstant="2026-11-12T00:00:00Z"><saml:Issuer>https://sso.example.invalid</saml:Issuer><ds:Signature><ds:SignatureValue>c2lnbmF0dXJlc2lnbmF0dXJlc2lnbmF0dXJlc2lnbmF0dXJlc2lnbmF0dXJlc2lnbmF0dXJlc2lnbmF0dXJl</ds:SignatureValue><ds:KeyInfo><ds:X509Data><ds:X509Certificate>MIIDcertcertcert</ds:X509Certificate></ds:X509Data></ds:KeyInfo></ds:Signature><saml:Subject><saml:NameID>svc-user@example.invalid</saml:NameID></saml:Subject></saml:Assertion></wsse:Security></soapenv:Header>
<soapenv:Body><ls:Login><ls:_this type="CisLicenseSessionManagementService">license-session-manager</ls:_this><ls:userName>alice-the-user</ls:userName><ls:password>hunter2hunter2</ls:password><ls:locale>en_US</ls:locale></ls:Login></soapenv:Body></soapenv:Envelope>`

func TestScrubLoginEnvelopeWithSAMLToken(t *testing.T) {
	got := ScrubBody([]byte(loginEnvelope))
	for _, leak := range []string{"QUJDREVG", "c2lnbmF0", "MIIDcert", "svc-user", "sso.example", "alice-the-user", "hunter2"} {
		if strings.Contains(got, leak) {
			t.Fatalf("leaked %q:\n%s", leak, got)
		}
	}
	for _, keep := range []string{
		`xmlns:ls="urn:cislicense"`, `<ls:Login>`, `type="CisLicenseSessionManagementService"`, `license-session-manager`,
		`<ls:locale>en_US</ls:locale>`, `<wsse:Security>`, `</wsse:Security>`, `<ls:userName>[scrubbed:userName len=14]</ls:userName>`,
		`<ls:password>[scrubbed:password len=14]</ls:password>`, `</soapenv:Body>`,
	} {
		if !strings.Contains(got, keep) {
			t.Fatalf("lost %q:\n%s", keep, got)
		}
	}
	if !strings.Contains(got, "[scrubbed:Security len=") {
		t.Fatalf("no Security marker:\n%s", got)
	}
	if strings.Index(got, "Header") > strings.Index(got, "Body") {
		t.Fatal("order changed")
	}
}

func TestScrubKeepsEmptyAndSelfClosing(t *testing.T) {
	got := ScrubBody([]byte(`<a><password/><userName></userName><b>x</b></a>`))
	if got != `<a><password/><userName></userName><b>x</b></a>` {
		t.Fatal(got)
	}
}

func TestScrubTruncatedAndMalformedXMLFailsClosed(t *testing.T) {
	for _, in := range []string{
		`<a><password>topsecretvalue`,
		`<a><x><password>topsecretvalue</password><b attr="`,
		`<a><y>ok</y><password>topsecretvalue</pass`,
	} {
		got := ScrubBody([]byte(in))
		if strings.Contains(got, "topsecretvalue") {
			t.Fatalf("leaked from %q: %s", in, got)
		}
	}
}

func TestScrubLongRunsAndNonXML(t *testing.T) {
	hex := strings.Repeat("ab12", 20)
	b64 := strings.Repeat("Qw9+", 20)
	short := strings.Repeat("a", 64)
	got := ScrubBody([]byte(`<r><k>` + hex + `</k><l>` + b64 + `</l><m>` + short + `</m></r>`))
	if strings.Contains(got, hex) || strings.Contains(got, b64) || !strings.Contains(got, short) || !strings.Contains(got, "[scrubbed:run len=80]") {
		t.Fatal(got)
	}
	got = ScrubBody([]byte(`{"userName":"bob","password":"pw","sessionId":"abc","name":"vm1"}`))
	if strings.Contains(got, "bob") || strings.Contains(got, `"pw"`) || strings.Contains(got, "abc") || !strings.Contains(got, `"name":"vm1"`) {
		t.Fatal(got)
	}
	got = ScrubBody([]byte("user=bob&password=pw&mode=x"))
	if strings.Contains(got, "pw") || !strings.Contains(got, "mode=x") {
		t.Fatal(got)
	}
	if got = ScrubBody([]byte{0xff, 0xfe, 0x00}); !strings.Contains(got, "binary") {
		t.Fatal(got)
	}
}

func debugCapture(t *testing.T, settings Settings) (*Capture, *bytes.Buffer) {
	var out bytes.Buffer
	c, err := NewCapture(t.TempDir(), "", settings, &out)
	if err != nil {
		t.Fatal(err)
	}
	return c, &out
}

func TestDebugCaptureLicenseServiceHeadersAndBody(t *testing.T) {
	c, out := debugCapture(t, Settings{Debug: true})
	var seen string
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		seen = b.String()
		http.NotFound(w, r)
	}))
	req := httptest.NewRequest("POST", "/ls/sdk?token=abc123&mode=x", strings.NewReader(loginEnvelope))
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.Header.Set("SOAPAction", `"urn:cislicense/1.0"`)
	req.Header.Set("User-Agent", "ops-client/1")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Authorization", "Basic c2VjcmV0")
	req.Header.Set("Cookie", "vmware_soap_session=\"cookie-secret\"")
	req.Header.Set("vmware-api-session-id", "sess-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen != loginEnvelope || rec.Code != 404 {
		t.Fatalf("handler must see the original request and answer as before: %d", rec.Code)
	}
	line := out.String()
	for _, leak := range []string{"c2VjcmV0", "cookie-secret", "sess-secret", "abc123", "hunter2", "alice-the-user"} {
		if strings.Contains(line, leak) {
			t.Fatalf("leaked %q: %s", leak, line)
		}
	}
	var got debugRecord
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Method != "POST" || got.Path != "/ls/sdk" || got.Status != 404 || got.Event != "request-capture" ||
		got.Headers["SOAPAction"] != `"urn:cislicense/1.0"` || got.Headers["User-Agent"] != "ops-client/1" || got.Headers["Content-Type"] != "text/xml; charset=utf-8" ||
		got.Headers["Authorization"] != "" || got.Headers["Cookie"] != "" || !strings.Contains(got.Query, "mode=x") ||
		!strings.Contains(got.Body, "CisLicenseSessionManagementService") || got.BodyBytes != len(loginEnvelope) {
		t.Fatalf("unexpected record: %+v", got)
	}
	names := strings.Join(got.HeaderNames, ",")
	if !strings.Contains(names, "Authorization") || !strings.Contains(names, "Cookie") || !strings.Contains(names, "Vmware-Api-Session-Id") {
		t.Fatalf("header names missing: %s", names)
	}
}

func TestDebugCaptureLicenseServiceOnly(t *testing.T) {
	c, out := debugCapture(t, Settings{Debug: true})
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
		case "/ls/sdk":
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	}))
	for _, rq := range [][2]string{{"GET", "/ls/sdk"}, {"GET", "/ok"}, {"GET", "/api/missing"}, {"PUT", "/ls/other"}} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(rq[0], rq[1], nil))
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || strings.Contains(out.String(), `"/ok"`) || strings.Contains(out.String(), "/api/missing") {
		t.Fatalf("unexpected lines: %q", lines)
	}
}

func TestDebugCaptureCutsLargeBody(t *testing.T) {
	c, out := debugCapture(t, Settings{Debug: true})
	h := c.Wrap(http.NotFoundHandler())
	body := "<a>" + strings.Repeat("<b>x</b>", 20000) + "</a>"
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/ls/sdk", strings.NewReader(body)))
	var got debugRecord
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.BodyTruncated || len(got.Body) > maxDebugBody || got.BodyCut == "" || got.BodyBytes != len(body) {
		t.Fatalf("not cut: %v %d %q %d", got.BodyTruncated, len(got.Body), got.BodyCut, got.BodyBytes)
	}
}

func TestDebugCaptureCanBeSwitchedOff(t *testing.T) {
	c, out := debugCapture(t, Settings{Debug: true})
	if err := c.Set(Settings{Unanswered: true}); err != nil {
		t.Fatal(err)
	}
	c.Wrap(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/ls/sdk", strings.NewReader("<a/>")))
	if out.Len() != 0 {
		t.Fatalf("logged while off: %s", out.String())
	}
	c2, err := NewCapture(c.settingsPath[:len(c.settingsPath)-len("/capture-settings.json")], "", Settings{Debug: true}, out)
	if err != nil || c2.State().Settings.Debug {
		t.Fatal("saved off setting must override the default", err)
	}
}

func TestDebugCaptureOnByDefaultWhenSettingsFileLacksIt(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir+"/capture-settings.json", `{"full":false,"unanswered":true,"stdout":false}`); err != nil {
		t.Fatal(err)
	}
	c, err := NewCapture(dir, "", Settings{Debug: true}, &bytes.Buffer{})
	if err != nil || !c.State().Settings.Debug {
		t.Fatal("debug must stay on", err)
	}
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0600) }

func TestScrubAssertionOutsideSecurityKeepsAttributes(t *testing.T) {
	got := ScrubBody([]byte(`<h><saml2:Assertion xmlns:saml2="urn:a" ID="_x"><saml2:Subject>who</saml2:Subject></saml2:Assertion><op/></h>`))
	if strings.Contains(got, "who") || !strings.Contains(got, `<saml2:Assertion xmlns:saml2="urn:a" ID="_x">[scrubbed:Assertion len=`) || !strings.HasSuffix(got, `</saml2:Assertion><op/></h>`) {
		t.Fatal(got)
	}
}

func TestScrubUsesOneSensitiveDefinition(t *testing.T) {
	for name, in := range map[string]string{
		"xml licenseKey":        `<r><ls:licenseKey>AAAAA-BBBBB-CCCCC-DDDDD-EEEEE</ls:licenseKey><ok>keep</ok></r>`,
		"xml newPassword":       `<r><newPassword>AAAAA-BBBBB</newPassword><ok>keep</ok></r>`,
		"xml signedEntitlement": `<r><signedEntitlement>AAAAA-BBBBB</signedEntitlement><ok>keep</ok></r>`,
		"xml accessToken":       `<r><accessToken>AAAAA-BBBBB</accessToken><ok>keep</ok></r>`,
		"json licenseKey":       `{"licenseKey":"AAAAA-BBBBB","ok":"keep"}`,
		"json privateKey":       `{"privateKey":"AAAAA-BBBBB","ok":"keep"}`,
		"form hash":             `hash=AAAAA-BBBBB&ok=keep`,
		"form licenseKey":       `licenseKey=AAAAA-BBBBB&ok=keep`,
	} {
		got := ScrubBody([]byte(in))
		if strings.Contains(got, "AAAAA") || !strings.Contains(got, "keep") || !strings.Contains(got, "[scrubbed:") {
			t.Errorf("%s: %q", name, got)
		}
	}
}

func TestScrubAttributesAndNonStringJSON(t *testing.T) {
	got := ScrubBody([]byte(`<ls:Login xmlns:ls="urn:ls" sessionId="0123456789abcdef" token='abc123' mode="keep"/>`))
	if strings.Contains(got, "0123456789abcdef") || strings.Contains(got, "abc123") || !strings.Contains(got, `xmlns:ls="urn:ls"`) || !strings.Contains(got, `mode="keep"`) {
		t.Errorf("attributes: %s", got)
	}
	got = ScrubBody([]byte(`<a><password x="1">pw12345</password><b token="zzz">ok</b></a>`))
	if strings.Contains(got, "pw12345") || strings.Contains(got, "zzz") || !strings.Contains(got, "<b ") || !strings.Contains(got, ">ok</b></a>") {
		t.Errorf("attribute plus element: %s", got)
	}
	for _, in := range []string{`{"password":12345,"ok":"keep"}`, `{"credentials":{"pass":"hunter2"},"ok":"keep"}`, `{"tokens":["hunter2"],"ok":"keep"}`} {
		got = ScrubBody([]byte(in))
		if strings.Contains(got, "12345") || strings.Contains(got, "hunter2") || !strings.Contains(got, `"ok":"keep"`) {
			t.Errorf("json %s: %s", in, got)
		}
	}
}

func TestScrubJSONStructurally(t *testing.T) {
	for name, in := range map[string]string{
		"nested":        `{"credentials":{"nested":{"first":"abc","second":"hunter2"}},"name":"vm1"}`,
		"escaped key":   `{"password":"hunter2","name":"vm1"}`,
		"escaped quote": `{"password":"hun\"ter2\\\"tail","name":"vm1"}`,
		"array value":   `{"tokens":["hunter2",{"a":"hunter2"}],"name":"vm1"}`,
		"number":        `{"password":123456,"name":"vm1"}`,
		"deep":          `{"outer":{"inner":[{"Secret":{"x":"hunter2"}}]},"name":"vm1"}`,
		"escaped name":  `{"userName":"hunter2tail","name":"vm1"}`,
	} {
		got := ScrubBody([]byte(in))
		for _, leak := range []string{"hunter2", "tail", "123456", `\"`} {
			if strings.Contains(got, leak) {
				t.Fatalf("%s leaked %q: %s", name, leak, got)
			}
		}
		if !strings.Contains(got, `"name":"vm1"`) || !json.Valid([]byte(got)) {
			t.Fatalf("%s lost structure: %s", name, got)
		}
	}
}

func TestScrubJSONKeepsOrderAndInvalidJSONIsOmitted(t *testing.T) {
	got := ScrubBody([]byte(`{"b":1,"a":[true,null,"x"],"c":{"z":1.50}}`))
	if got != `{"b":1,"a":[true,null,"x"],"c":{"z":1.50}}` {
		t.Fatal(got)
	}
	for _, in := range []string{`{"password":"hunter2`, `{"password":"hunter2",}`, `[{"token":"hunter2"} x`} {
		if got := ScrubBody([]byte(in)); strings.Contains(got, "hunter2") || !strings.Contains(got, "unparsed") {
			t.Fatalf("%q: %s", in, got)
		}
	}
}

func TestScrubFormDecodesNamesAndTakesWholeValue(t *testing.T) {
	for _, in := range []string{
		`pass%77ord=hunter2&mode=x`,
		`password[]=hunter2&mode=x`,
		`user%4Eame=hunter2&mode=x`,
		`mode=x;PASS%57ORD=hunter2`,
		`mode=x&password=hunter2%26tail&other=1`,
		`mode=x&pass%zzword=hunter2`,
		`mode=x&session+id=hunter2`,
		`mode=x&a=1&access_token=hunter2=more`,
	} {
		got := ScrubBody([]byte(in))
		if strings.Contains(got, "hunter2") || strings.Contains(got, "tail") || strings.Contains(got, "more") || !strings.Contains(got, "mode=x") {
			t.Fatalf("%q -> %s", in, got)
		}
	}
	if got := ScrubBody([]byte(`mode=x&a=1`)); got != `mode=x&a=1` {
		t.Fatal(got)
	}
}
