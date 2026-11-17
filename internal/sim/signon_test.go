package sim_test

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

const (
	lookupAction = `"urn:internallookup/2.0"`
	ssoAction    = `"urn:sso/version2"`
	stsAction    = `"http://docs.oasis-open.org/ws-sx/ws-trust/200512/RST/Issue"`
	ssoPath      = "/sso-adminserver/sdk/vsphere.local"
	tokenPath    = "/sts/STSService/vsphere.local"
)

// The accounts of these tests: one administrator, one read-only.
var signOnUsers = map[string]string{"operator": "s3cret", "viewer@vsphere.local": "l00king"}

// signOnSim builds a simulator with the two accounts and returns it, its
// server and its topology.
func signOnSim(t *testing.T, change func(*sim.Options)) (*sim.Simulator, *httptest.Server, *topology.Topology) {
	t.Helper()
	sc := scenario.Phase1("sign-on")
	topo := topology.Generate(sc, registry.InMemory(sc.Seed))
	cert, err := newCertificate(t)
	if err != nil {
		t.Fatal(err)
	}
	opts := sim.Options{Topology: topo, Credentials: signOnUsers, Certificate: cert,
		UserRole: func(user string) string {
			if user == "viewer@vsphere.local" {
				return "read-only"
			}
			return "administrator"
		}}
	if change != nil {
		change(&opts)
	}
	s, err := sim.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s.Handler)
	t.Cleanup(srv.Close)
	return s, srv, topo
}

func soapEnvelope(body string) []byte {
	return []byte(`<?xml version='1.0' encoding='UTF-8'?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"` +
		` xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">` +
		`<soapenv:Body>` + body + `</soapenv:Body></soapenv:Envelope>`)
}

// lookupList is the List request exactly as VCF Operations sends it, with
// the four values of the filter.
func lookupList(ref, product, service, protocol, endpoint string) []byte {
	return soapEnvelope(`<List xmlns="urn:internallookup"><_this type="LookupServiceRegistration">` + ref + `</_this>` +
		`<filterCriteria><serviceType><product>` + product + `</product><type>` + service + `</type></serviceType>` +
		`<endpointType><protocol>` + protocol + `</protocol><type>` + endpoint + `</type></endpointType></filterCriteria></List>`)
}

// element is a parsed element: its name, attributes, text and children in
// document order.
type element struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []element  `xml:",any"`
}

func (e element) names() []string {
	var out []string
	for _, c := range e.Children {
		out = append(out, c.XMLName.Local)
	}
	return out
}

func (e element) child(name string) element {
	for _, c := range e.Children {
		if c.XMLName.Local == name {
			return c
		}
	}
	return element{}
}

func (e element) all(name string) []element {
	var out []element
	for _, c := range e.Children {
		if c.XMLName.Local == name {
			out = append(out, c)
		}
	}
	return out
}

func (e element) attr(name string) string {
	for _, a := range e.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func (e element) text(path ...string) string {
	for _, p := range path {
		e = e.child(p)
	}
	return strings.TrimSpace(e.Text)
}

// soapBody parses an answer and returns the element inside the SOAP Body.
func soapBody(t *testing.T, data []byte) element {
	t.Helper()
	var env element
	if err := xml.Unmarshal(data, &env); err != nil {
		t.Fatalf("answer is not XML: %v: %s", err, data)
	}
	body := env.child("Body")
	if len(body.Children) != 1 {
		t.Fatalf("the SOAP Body holds %d elements, want one: %s", len(body.Children), data)
	}
	return body.Children[0]
}

// anonymousSessionCookie checks the cookie a real vCenter sets on every
// lookup and sign-on admin answer: 36 characters, Secure, HttpOnly, no Path.
func anonymousSessionCookie(t *testing.T, res *http.Response) {
	t.Helper()
	for _, ck := range res.Cookies() {
		if ck.Name != "vmware_soap_session" {
			continue
		}
		if len(ck.Value) != 36 || !ck.Secure || !ck.HttpOnly || ck.Path != "" {
			t.Errorf("cookie: length %d secure %v httpOnly %v path %q, want 36, Secure, HttpOnly and no Path", len(ck.Value), ck.Secure, ck.HttpOnly, ck.Path)
		}
		return
	}
	t.Errorf("no vmware_soap_session cookie in %v", res.Cookies())
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// The lookup service answers the two filters VCF Operations sends and the
// token-service filter with one registration each, in the element order a
// real vCenter uses, and any other filter with an empty list.
func TestLookupList(t *testing.T) {
	s, srv, topo := signOnSim(t, nil)
	host := strings.TrimPrefix(srv.URL, "http://")
	name, _, _ := strings.Cut(host, ":")
	leaf := base64.StdEncoding.EncodeToString(servedLeaf(t, s))

	list := func(t *testing.T, product, service, protocol, endpoint string) []element {
		t.Helper()
		res, out := postTo(t, srv, "/lookupservice/sdk", lookupAction, lookupList("ServiceRegistration", product, service, protocol, endpoint))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", res.StatusCode, out)
		}
		if ct := res.Header.Get("Content-Type"); ct != "text/xml;charset=utf-8" {
			t.Errorf("Content-Type %q", ct)
		}
		anonymousSessionCookie(t, res)
		body := soapBody(t, out)
		if body.XMLName.Local != "ListResponse" || body.XMLName.Space != "urn:internallookup" {
			t.Fatalf("answer is %v, want ListResponse in urn:internallookup", body.XMLName)
		}
		return body.all("returnval")
	}

	t.Run("vcenterserver", func(t *testing.T) {
		regs := list(t, "com.vmware.cis", "vcenterserver", "vmomi", "com.vmware.vim")
		if len(regs) != 1 {
			t.Fatalf("%d registrations, want 1", len(regs))
		}
		reg := regs[0]
		want := []string{"serviceVersion", "vendorNameResourceKey", "vendorNameDefault", "vendorProductInfoResourceKey", "vendorProductInfoDefault",
			"serviceEndpoints", "serviceAttributes", "serviceAttributes", "serviceAttributes", "serviceAttributes",
			"serviceNameResourceKey", "serviceNameDefault", "serviceDescriptionResourceKey", "serviceDescriptionDefault",
			"ownerId", "serviceType", "nodeId", "serviceId", "siteId"}
		if got := reg.names(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("elements\n got %v\nwant %v", got, want)
		}
		for _, empty := range []string{"vendorNameResourceKey", "vendorNameDefault", "vendorProductInfoResourceKey", "vendorProductInfoDefault", "serviceNameDefault", "serviceDescriptionDefault"} {
			if reg.text(empty) != "" {
				t.Errorf("%s = %q, want present and empty", empty, reg.text(empty))
			}
		}
		if got := reg.text("serviceVersion"); got != "9.1" {
			t.Errorf("serviceVersion %q, want 9.1 for vCenter 9.1.1", got)
		}
		ep := reg.child("serviceEndpoints")
		if got, want := ep.names(), "url,endpointType,sslTrust,endpointAttributes"; strings.Join(got, ",") != want {
			t.Errorf("endpoint elements %v, want %s", got, want)
		}
		if got, want := ep.text("url"), "https://"+host+"/sdk"; got != want {
			t.Errorf("url %q, want %q", got, want)
		}
		if ep.text("endpointType", "protocol") != "vmomi" || ep.text("endpointType", "type") != "com.vmware.vim" {
			t.Errorf("endpoint type %v", ep.child("endpointType"))
		}
		if ep.text("sslTrust") != leaf {
			t.Error("sslTrust is not the certificate the listener serves")
		}
		if k, v := ep.text("endpointAttributes", "key"), ep.text("endpointAttributes", "value"); k != "cis.common.ep.localurl" || v != "http://"+name+":8085/sdk" {
			t.Errorf("endpoint attribute %q = %q", k, v)
		}
		attrs := map[string]string{}
		var keys []string
		for _, a := range reg.all("serviceAttributes") {
			attrs[a.text("key")] = a.text("value")
			keys = append(keys, a.text("key"))
		}
		if got, want := strings.Join(keys, ","), "com.vmware.cis.cm.GroupInternalId,com.vmware.cis.cm.ControlScript,com.vmware.vim.vcenter.instanceName,com.vmware.cis.cm.HostId"; got != want {
			t.Errorf("service attributes %s, want %s", got, want)
		}
		// the lengths a real vCenter showed for the two values it kept to itself
		if len(attrs["com.vmware.cis.cm.GroupInternalId"]) != 22 || len(attrs["com.vmware.cis.cm.ControlScript"]) != 28 {
			t.Errorf("attribute lengths %d and %d, want 22 and 28", len(attrs["com.vmware.cis.cm.GroupInternalId"]), len(attrs["com.vmware.cis.cm.ControlScript"]))
		}
		if attrs["com.vmware.vim.vcenter.instanceName"] != name {
			t.Errorf("instanceName %q, want the name the client used, %q", attrs["com.vmware.vim.vcenter.instanceName"], name)
		}
		hostID := attrs["com.vmware.cis.cm.HostId"]
		if reg.text("serviceNameResourceKey") != "AboutInfo.vpx.name" || reg.text("serviceDescriptionResourceKey") != "AboutInfo.vpx.name" {
			t.Errorf("name keys %q and %q", reg.text("serviceNameResourceKey"), reg.text("serviceDescriptionResourceKey"))
		}
		if got, want := reg.text("ownerId"), "vpxd-svc-acct-"+hostID+"@vsphere.local"; got != want {
			t.Errorf("ownerId %q, want %q", got, want)
		}
		if reg.text("serviceType", "product") != "com.vmware.cis" || reg.text("serviceType", "type") != "vcenterserver" {
			t.Errorf("service type %v", reg.child("serviceType"))
		}
		// three different ids: the service id is the instance UUID, the node
		// id is not, and the host id is a third
		node, service := reg.text("nodeId"), reg.text("serviceId")
		if service != topo.InstanceUUID {
			t.Errorf("serviceId %q, want about.instanceUuid %q", service, topo.InstanceUUID)
		}
		for what, id := range map[string]string{"nodeId": node, "HostId": hostID} {
			if !uuidRe.MatchString(id) || id == topo.InstanceUUID {
				t.Errorf("%s %q must be a UUID of its own", what, id)
			}
		}
		if node == hostID {
			t.Error("nodeId and HostId are the same")
		}
		if len(reg.text("siteId")) != 18 {
			t.Errorf("siteId %q, want 18 characters", reg.text("siteId"))
		}
	})

	for _, tc := range []struct{ name, protocol, endpoint, path string }{
		{"sign-on admin", "vmomi", "com.vmware.cis.cs.identity.admin", ssoPath},
		{"token service", "wsTrust", "com.vmware.cis.cs.identity.sso", tokenPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			regs := list(t, "com.vmware.cis", "cs.identity", tc.protocol, tc.endpoint)
			if len(regs) != 1 {
				t.Fatalf("%d registrations, want 1", len(regs))
			}
			reg := regs[0]
			want := "serviceVersion,vendorNameResourceKey,vendorNameDefault,vendorProductInfoResourceKey,vendorProductInfoDefault," +
				"serviceEndpoints,serviceAttributes,serviceNameResourceKey,serviceNameDefault,serviceDescriptionResourceKey,serviceDescriptionDefault," +
				"ownerId,serviceType,nodeId,serviceId,siteId"
			if got := strings.Join(reg.names(), ","); got != want {
				t.Errorf("elements\n got %s\nwant %s", got, want)
			}
			if reg.text("serviceVersion") != "2.0" {
				t.Errorf("serviceVersion %q, want 2.0", reg.text("serviceVersion"))
			}
			eps := reg.all("serviceEndpoints")
			if len(eps) != 1 {
				t.Fatalf("%d endpoints, want the matching one", len(eps))
			}
			ep := eps[0]
			if got, want := strings.Join(ep.names(), ","), "url,endpointType,sslTrust"; got != want {
				t.Errorf("endpoint elements %s, want %s", got, want)
			}
			if got, want := ep.text("url"), "https://"+host+tc.path; got != want {
				t.Errorf("url %q, want %q", got, want)
			}
			if ep.text("endpointType", "protocol") != tc.protocol || ep.text("endpointType", "type") != tc.endpoint {
				t.Errorf("endpoint type %v", ep.child("endpointType"))
			}
			if ep.text("sslTrust") != leaf {
				t.Error("sslTrust is not the certificate the listener serves")
			}
			if k := reg.text("serviceAttributes", "key"); k != "Capabilities" || len(reg.text("serviceAttributes", "value")) != 9 {
				t.Errorf("service attribute %q = %q, want Capabilities with 9 characters", k, reg.text("serviceAttributes", "value"))
			}
			if !strings.HasSuffix(reg.text("ownerId"), "@vsphere.local") {
				t.Errorf("ownerId %q", reg.text("ownerId"))
			}
			if reg.text("serviceType", "type") != "cs.identity" {
				t.Errorf("service type %v", reg.child("serviceType"))
			}
			// the node and the site are the vCenter registration's
			vc := list(t, "com.vmware.cis", "vcenterserver", "vmomi", "com.vmware.vim")[0]
			if reg.text("nodeId") != vc.text("nodeId") || reg.text("siteId") != vc.text("siteId") {
				t.Error("the sign-on registration is on another node or site than the vCenter's")
			}
			if id := reg.text("serviceId"); !uuidRe.MatchString(id) || id == vc.text("serviceId") {
				t.Errorf("serviceId %q must be a UUID of its own", id)
			}
		})
	}

	t.Run("a filter that matches nothing", func(t *testing.T) {
		for _, f := range [][4]string{
			{"com.vmware.cis", "cs.license", "vmomi", "com.vmware.cis.license"},
			{"com.vmware.cis", "vcenterserver", "wsTrust", "com.vmware.vim"},
			{"com.vmware.other", "cs.identity", "vmomi", "com.vmware.cis.cs.identity.admin"},
		} {
			if regs := list(t, f[0], f[1], f[2], f[3]); len(regs) != 0 {
				t.Errorf("filter %v returned %d registrations, want none", f, len(regs))
			}
		}
		// no filter at all is also answered, with nothing
		res, out := postTo(t, srv, "/lookupservice/sdk", lookupAction, soapEnvelope(`<List xmlns="urn:internallookup"><_this type="LookupServiceRegistration">ServiceRegistration</_this></List>`))
		if res.StatusCode != http.StatusOK || len(soapBody(t, out).Children) != 0 {
			t.Errorf("unfiltered List: status %d, body %s", res.StatusCode, out)
		}
	})

	t.Run("a reference the service did not hand out", func(t *testing.T) {
		res, out := postTo(t, srv, "/lookupservice/sdk", lookupAction, lookupList("serviceRegistration", "com.vmware.cis", "vcenterserver", "vmomi", "com.vmware.vim"))
		if res.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500: %s", res.StatusCode, out)
		}
		fault := soapBody(t, out)
		detail := fault.child("detail").child("RuntimeFaultFault")
		if fault.XMLName.Local != "Fault" || detail.XMLName.Space != "urn:internallookup" || detail.attr("type") != "ManagedObjectNotFound" {
			t.Errorf("fault %s", out)
		}
	})
}

// servedLeaf is the certificate the simulator's listener serves, as the
// certificate-authority read returns it.
func servedLeaf(t *testing.T, s *sim.Simulator) []byte {
	t.Helper()
	srv := httptest.NewServer(s.Handler)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/session", nil)
	req.SetBasicAuth("operator", "s3cret")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var token string
	_ = jsonDecode(res, &token)
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/vcenter/certificate-authority/get-root", nil)
	req.Header.Set("vmware-api-session-id", token)
	res, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := jsonDecode(res, &got); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(got["value"]))
	if block == nil {
		t.Fatalf("no certificate in %v", got)
	}
	return block.Bytes
}

// The addresses the lookup service hands out are built on the address the
// client used: the host a proxy forwards, when there is one. A host with no
// port gets 443 on the vCenter address, as a real vCenter writes it.
func TestLookupAddressesFollowTheClient(t *testing.T) {
	_, srv, _ := signOnSim(t, nil)
	url := func(t *testing.T, forwarded, endpoint, protocol, service string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/lookupservice/sdk", bytes.NewReader(lookupList("ServiceRegistration", "com.vmware.cis", service, protocol, endpoint)))
		req.Header.Set("Content-Type", "text/xml; charset=utf-8")
		if forwarded != "" {
			req.Header.Set("X-Forwarded-Host", forwarded)
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(res.Body)
		return soapBody(t, buf.Bytes()).child("returnval").child("serviceEndpoints").text("url")
	}
	if got := url(t, "vcenter.example.test", "com.vmware.vim", "vmomi", "vcenterserver"); got != "https://vcenter.example.test:443/sdk" {
		t.Errorf("vCenter address behind a proxy: %q", got)
	}
	if got := url(t, "vcenter.example.test", "com.vmware.cis.cs.identity.admin", "vmomi", "cs.identity"); got != "https://vcenter.example.test"+ssoPath {
		t.Errorf("sign-on admin address behind a proxy: %q", got)
	}
	if got := url(t, "vcenter.example.test:8443, proxy.example.test", "com.vmware.cis.cs.identity.sso", "wsTrust", "cs.identity"); got != "https://vcenter.example.test:8443"+tokenPath {
		t.Errorf("token address behind two proxies: %q", got)
	}
	// a forwarded host that is not a host is not used
	if got := url(t, `evil"/><x>`, "com.vmware.cis.cs.identity.sso", "wsTrust", "cs.identity"); got != "https://"+strings.TrimPrefix(srv.URL, "http://")+tokenPath {
		t.Errorf("a malformed forwarded host was used: %q", got)
	}
}

// With an operator's certificate file, sslTrust is that certificate and not
// the listener's, and a replaced file is picked up.
func TestLookupTrustFromOperatorFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "front.pem")
	front := func() []byte {
		cert, err := newCertificate(t)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Current().Certificate[0]})
	}
	first := front()
	if err := os.WriteFile(file, first, 0o600); err != nil {
		t.Fatal(err)
	}
	trust, err := sim.NewTrustSource(file)
	if err != nil {
		t.Fatal(err)
	}
	s, srv, _ := signOnSim(t, func(o *sim.Options) { o.Trust = trust })
	read := func() string {
		_, out := postTo(t, srv, "/lookupservice/sdk", lookupAction, lookupList("ServiceRegistration", "com.vmware.cis", "cs.identity", "vmomi", "com.vmware.cis.cs.identity.admin"))
		return soapBody(t, out).child("returnval").child("serviceEndpoints").text("sslTrust")
	}
	der := func(p []byte) string { b, _ := pem.Decode(p); return base64.StdEncoding.EncodeToString(b.Bytes) }
	if got := read(); got != der(first) {
		t.Error("sslTrust is not the operator's certificate")
	}
	if read() == base64.StdEncoding.EncodeToString(servedLeaf(t, s)) {
		t.Error("sslTrust is still the listener's certificate")
	}
	second := front()
	if err := os.WriteFile(file, second, 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for read() != der(second) {
		if time.Now().After(deadline) {
			t.Fatal("the replaced certificate file was not picked up")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// a file with no certificate in it is refused at start
	if err := os.WriteFile(file, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sim.NewTrustSource(file); err == nil {
		t.Error("a file without a certificate was accepted")
	}
}

func ssoCall(op, typ, ref string) []byte {
	return soapEnvelope(`<` + op + ` xmlns="urn:sso"><_this type="` + typ + `">` + ref + `</_this></` + op + `>`)
}

// The three sign-on admin reads answer without a sign-in, in the shapes a
// real vCenter uses.
func TestSSOAdminReads(t *testing.T) {
	_, srv, _ := signOnSim(t, nil)
	call := func(t *testing.T, op, typ, ref string) element {
		t.Helper()
		res, out := postTo(t, srv, ssoPath, ssoAction, ssoCall(op, typ, ref))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s status %d: %s", op, res.StatusCode, out)
		}
		anonymousSessionCookie(t, res)
		body := soapBody(t, out)
		if body.XMLName.Local != op+"Response" || body.XMLName.Space != "urn:sso" {
			t.Fatalf("answer is %v, want %sResponse in urn:sso", body.XMLName, op)
		}
		return body
	}

	t.Run("SsoAdminServiceInstance", func(t *testing.T) {
		rv := call(t, "SsoAdminServiceInstance", "SsoAdminServiceInstance", "SsoAdminServiceInstance").child("returnval")
		about := rv.child("aboutInfo")
		if got, want := strings.Join(about.names(), ","), "version,build,apiRevision,clusterId,deploymentId"; got != want {
			t.Errorf("aboutInfo elements %s, want %s", got, want)
		}
		if about.text("version") != "0" || about.text("build") != "N/A" || about.text("clusterId") != "N/A" || about.text("deploymentId") != "N/A" {
			t.Errorf("aboutInfo %v", about)
		}
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(about.text("apiRevision")) {
			t.Errorf("apiRevision %q, want 64 hex characters", about.text("apiRevision"))
		}
		want := [][3]string{
			{"sessionManager", "SsoSessionManager", "ssoSessionManager"},
			{"configurationManagementService", "SsoAdminConfigurationManagementService", "configurationManagementService"},
			{"smtpManagementService", "SsoAdminSmtpManagementService", "smtpManagementService"},
			{"principalDiscoveryService", "SsoAdminPrincipalDiscoveryService", "principalDiscoveryService"},
			{"principalManagementService", "SsoAdminPrincipalManagementService", "principalManagementService"},
			{"roleManagementService", "SsoAdminRoleManagementService", "roleManagementService"},
			{"passwordPolicyService", "SsoAdminPasswordPolicyService", "passwordPolicyService"},
			{"lockoutPolicyService", "SsoAdminLockoutPolicyService", "lockoutPolicyService"},
			{"domainManagementService", "SsoAdminDomainManagementService", "domainManagementService"},
			{"identitySourceManagementService", "SsoAdminIdentitySourceManagementService", "identitySourceManagementService"},
			{"systemManagementService", "SsoAdminSystemManagementService", "systemManagementService"},
			{"deploymentInformationService", "SsoAdminDeploymentInformationService", "deploymentInformationService"},
			{"replicationService", "SsoAdminReplicationService", "replicationService"},
		}
		refs := rv.Children[1:]
		if len(refs) != len(want) {
			t.Fatalf("%d references after aboutInfo, want %d: %v", len(refs), len(want), rv.names())
		}
		for i, w := range want {
			if refs[i].XMLName.Local != w[0] || refs[i].attr("type") != w[1] || strings.TrimSpace(refs[i].Text) != w[2] {
				t.Errorf("reference %d is <%s type=%q>%s, want <%s type=%q>%s", i, refs[i].XMLName.Local, refs[i].attr("type"), refs[i].Text, w[0], w[1], w[2])
			}
		}
	})

	t.Run("IdS_getSystemDomainName", func(t *testing.T) {
		body := call(t, "IdS_getSystemDomainName", "SsoAdminIdentitySourceManagementService", "identitySourceManagementService")
		// the domain is the last part of both sign-on addresses
		if got := body.text("returnval"); got != "vsphere.local" || !strings.HasSuffix(ssoPath, "/"+got) || !strings.HasSuffix(tokenPath, "/"+got) {
			t.Errorf("system domain %q", got)
		}
	})

	t.Run("GetIssuersCertificates", func(t *testing.T) {
		body := call(t, "GetIssuersCertificates", "SsoAdminConfigurationManagementService", "configurationManagementService")
		certs := body.all("returnval")
		if len(certs) != 1 {
			t.Fatalf("%d certificates, want one", len(certs))
		}
		der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(certs[0].Text))
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		// as on a real vCenter: an RSA 3072 signing certificate, signed with
		// SHA-256 by another certificate, and not the TLS certificate
		key, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok || key.N.BitLen() != 3072 {
			t.Errorf("signing key %T, want RSA 3072", cert.PublicKey)
		}
		if cert.SignatureAlgorithm != x509.SHA256WithRSA {
			t.Errorf("signature algorithm %v, want SHA256-RSA", cert.SignatureAlgorithm)
		}
		if bytes.Equal(cert.RawIssuer, cert.RawSubject) {
			t.Error("the signing certificate is self-signed")
		}
	})

	t.Run("anything else stays unanswered", func(t *testing.T) {
		res, _ := postTo(t, srv, ssoPath, ssoAction, ssoCall("GetTrustedCertificates", "SsoAdminConfigurationManagementService", "configurationManagementService"))
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("an operation that is not built: status %d, want 404", res.StatusCode)
		}
		res, _ = postTo(t, srv, "/sso-adminserver/sdk/other.domain", ssoAction, ssoCall("IdS_getSystemDomainName", "SsoAdminIdentitySourceManagementService", "identitySourceManagementService"))
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("another domain's address: status %d, want 404", res.StatusCode)
		}
	})
}

// tokenRequest is a WS-Trust Issue for a bearer token with a user name and
// password, in the form a real vCenter's token service accepted.
func tokenRequest(user, password string, created, expires time.Time) []byte {
	const layout = "2006-01-02T15:04:05.000Z"
	return []byte(`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/"><SOAP-ENV:Header>` +
		`<wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd" xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">` +
		`<wsu:Timestamp><wsu:Created>` + created.Format(layout) + `</wsu:Created><wsu:Expires>` + expires.Format(layout) + `</wsu:Expires></wsu:Timestamp>` +
		`<wsse:UsernameToken><wsse:Username>` + user + `</wsse:Username><wsse:Password>` + password + `</wsse:Password></wsse:UsernameToken>` +
		`</wsse:Security></SOAP-ENV:Header><SOAP-ENV:Body>` +
		`<RequestSecurityToken xmlns="http://docs.oasis-open.org/ws-sx/ws-trust/200512" xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">` +
		`<TokenType>urn:oasis:names:tc:SAML:2.0:assertion</TokenType>` +
		`<RequestType>http://docs.oasis-open.org/ws-sx/ws-trust/200512/Issue</RequestType>` +
		`<Lifetime><wsu:Created>` + created.Format(layout) + `</wsu:Created><wsu:Expires>` + expires.Format(layout) + `</wsu:Expires></Lifetime>` +
		`<Renewing Allow="false" OK="false"/><Delegatable>false</Delegatable>` +
		`<KeyType>http://docs.oasis-open.org/ws-sx/ws-trust/200512/Bearer</KeyType>` +
		`<SignatureAlgorithm>http://www.w3.org/2001/04/xmldsig-more#rsa-sha256</SignatureAlgorithm>` +
		`</RequestSecurityToken></SOAP-ENV:Body></SOAP-ENV:Envelope>`)
}

// issuerCertificate fetches the signing certificate the sign-on admin
// service hands out.
func issuerCertificate(t *testing.T, srv *httptest.Server) *x509.Certificate {
	t.Helper()
	res, out := postTo(t, srv, ssoPath, ssoAction, ssoCall("GetIssuersCertificates", "SsoAdminConfigurationManagementService", "configurationManagementService"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GetIssuersCertificates %d: %s", res.StatusCode, out)
	}
	der, err := base64.StdEncoding.DecodeString(soapBody(t, out).text("returnval"))
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// issueToken asks for a token and returns the answer and its assertion.
func issueToken(t *testing.T, srv *httptest.Server, user, password string) ([]byte, element) {
	t.Helper()
	now := time.Now().UTC()
	res, out := postTo(t, srv, tokenPath, stsAction, tokenRequest(user, password, now, now.Add(300*time.Second)))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("token request for %s: status %d: %s", user, res.StatusCode, out)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/xml;charset=utf-8" {
		t.Errorf("Content-Type %q", ct)
	}
	if len(res.Cookies()) != 0 {
		t.Errorf("the token service set a cookie: %v", res.Cookies())
	}
	collection := soapBody(t, out)
	if collection.XMLName.Local != "RequestSecurityTokenResponseCollection" {
		t.Fatalf("answer is %v", collection.XMLName)
	}
	rstr := collection.child("RequestSecurityTokenResponse")
	if got, want := strings.Join(rstr.names(), ","), "RequestedSecurityToken,Lifetime,TokenType,KeyType,Delegatable,SignatureAlgorithm"; got != want {
		t.Errorf("response elements %s, want %s", got, want)
	}
	if rstr.text("TokenType") != "urn:oasis:names:tc:SAML:2.0:assertion" || rstr.text("KeyType") != "http://docs.oasis-open.org/ws-sx/ws-trust/200512/Bearer" ||
		rstr.text("Delegatable") != "false" || rstr.text("SignatureAlgorithm") != "http://www.w3.org/2001/04/xmldsig-more#rsa-sha256" {
		t.Errorf("response values %v", rstr)
	}
	for _, at := range []string{rstr.text("Lifetime", "Created"), rstr.text("Lifetime", "Expires")} {
		if len(at) != 24 {
			t.Errorf("lifetime time %q, want 24 characters", at)
		}
	}
	return out, rstr.child("RequestedSecurityToken").child("Assertion")
}

// verifyToken checks an assertion's signature the way a tool does: digest
// of the assertion in canonical form without its signature, and the RSA
// signature over the canonical SignedInfo, against a certificate.
func verifyToken(t *testing.T, answer []byte, assertion element, cert *x509.Certificate) error {
	t.Helper()
	info := assertion.child("Signature").child("SignedInfo")
	ref := info.child("Reference")
	if ref.attr("URI") != "#"+assertion.attr("ID") {
		t.Errorf("reference %q does not name the assertion %q", ref.attr("URI"), assertion.attr("ID"))
	}
	prefixes := strings.Fields(ref.child("Transforms").all("Transform")[1].child("InclusiveNamespaces").attr("PrefixList"))
	body := canonical(t, answer, "Assertion", prefixes, func(e xml.StartElement) bool { return e.Name.Local == "Signature" })
	digest := sha256.Sum256(body)
	if got := base64.StdEncoding.EncodeToString(digest[:]); got != ref.text("DigestValue") {
		t.Errorf("digest of the assertion is %s, the token says %s", got, ref.text("DigestValue"))
	}
	signedInfo := sha256.Sum256(canonical(t, answer, "SignedInfo", nil, nil))
	value, err := base64.StdEncoding.DecodeString(assertion.child("Signature").text("SignatureValue"))
	if err != nil {
		t.Fatal(err)
	}
	return rsa.VerifyPKCS1v15(cert.PublicKey.(*rsa.PublicKey), crypto.SHA256, signedInfo[:], value)
}

// groupsOf lists the groups a token names.
func groupsOf(assertion element) []string {
	var out []string
	for _, a := range assertion.child("AttributeStatement").all("Attribute") {
		if a.attr("Name") == "http://rsa.com/schemas/attr-names/2009/01/GroupIdentity" {
			for _, v := range a.all("AttributeValue") {
				out = append(out, strings.TrimSpace(v.Text))
			}
		}
	}
	return out
}

// A token request with the right user name and password gets a token in
// the shape a real vCenter issues, whose signature verifies with the
// certificate GetIssuersCertificates returns.
func TestTokenServiceIssuesSignedToken(t *testing.T) {
	_, srv, _ := signOnSim(t, nil)
	host := strings.TrimPrefix(srv.URL, "http://")
	answer, a := issueToken(t, srv, "operator", "s3cret")
	cert := issuerCertificate(t, srv)

	if err := verifyToken(t, answer, a, cert); err != nil {
		t.Errorf("the signature does not verify with the issuer's certificate: %v", err)
	}

	// the shape
	if got, want := strings.Join(a.names(), ","), "Issuer,Signature,Subject,Conditions,AuthnStatement,AttributeStatement"; got != want {
		t.Errorf("assertion elements %s, want %s", got, want)
	}
	if a.XMLName.Space != "urn:oasis:names:tc:SAML:2.0:assertion" || a.attr("Version") != "2.0" || len(a.attr("ID")) != 37 {
		t.Errorf("assertion %v version %q id %q", a.XMLName, a.attr("Version"), a.attr("ID"))
	}
	issuer := a.child("Issuer")
	if issuer.attr("Format") != "urn:oasis:names:tc:SAML:2.0:nameid-format:entity" || strings.TrimSpace(issuer.Text) != "https://"+host+"/websso/SAML2/Metadata/vsphere.local" {
		t.Errorf("issuer %q format %q", issuer.Text, issuer.attr("Format"))
	}
	sig := a.child("Signature")
	if got, want := strings.Join(sig.names(), ","), "SignedInfo,SignatureValue,KeyInfo"; got != want {
		t.Errorf("signature elements %s, want %s", got, want)
	}
	info := sig.child("SignedInfo")
	if info.child("CanonicalizationMethod").attr("Algorithm") != "http://www.w3.org/2001/10/xml-exc-c14n#" ||
		info.child("SignatureMethod").attr("Algorithm") != "http://www.w3.org/2001/04/xmldsig-more#rsa-sha256" {
		t.Errorf("signature algorithms %v", info)
	}
	ref := info.child("Reference")
	transforms := ref.child("Transforms").all("Transform")
	if len(transforms) != 2 || transforms[0].attr("Algorithm") != "http://www.w3.org/2000/09/xmldsig#enveloped-signature" ||
		transforms[1].attr("Algorithm") != "http://www.w3.org/2001/10/xml-exc-c14n#" || transforms[1].child("InclusiveNamespaces").attr("PrefixList") != "xsd xsi" {
		t.Errorf("transforms %v", transforms)
	}
	if ref.child("DigestMethod").attr("Algorithm") != "http://www.w3.org/2001/04/xmlenc#sha256" || len(ref.text("DigestValue")) != 44 {
		t.Errorf("digest %v", ref)
	}
	if len(sig.text("SignatureValue")) != 512 {
		t.Errorf("signature value has %d characters, want 512", len(sig.text("SignatureValue")))
	}
	// two certificates: the signing one, which is the issuer's, then a
	// self-signed root that issued it
	chain := sig.child("KeyInfo").child("X509Data").all("X509Certificate")
	if len(chain) != 2 {
		t.Fatalf("%d certificates in the token, want 2", len(chain))
	}
	leafDER, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(chain[0].Text))
	rootDER, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(chain[1].Text))
	if !bytes.Equal(leafDER, cert.Raw) {
		t.Error("the token's first certificate is not the one GetIssuersCertificates returns")
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(root.RawIssuer, root.RawSubject) {
		t.Error("the token's second certificate is not self-signed")
	}
	if err := cert.CheckSignatureFrom(root); err != nil {
		t.Errorf("the root did not issue the signing certificate: %v", err)
	}

	subject := a.child("Subject")
	if id := subject.child("NameID"); id.attr("Format") != "http://schemas.xmlsoap.org/claims/UPN" || strings.TrimSpace(id.Text) != "operator@vsphere.local" {
		t.Errorf("name id %q format %q", id.Text, id.attr("Format"))
	}
	conf := subject.child("SubjectConfirmation")
	if conf.attr("Method") != "urn:oasis:names:tc:SAML:2.0:cm:bearer" || conf.child("SubjectConfirmationData").attr("NotOnOrAfter") == "" {
		t.Errorf("subject confirmation %v", conf)
	}
	cond := a.child("Conditions")
	from, err1 := time.Parse(time.RFC3339Nano, cond.attr("NotBefore"))
	until, err2 := time.Parse(time.RFC3339Nano, cond.attr("NotOnOrAfter"))
	if err1 != nil || err2 != nil || until.Sub(from) != 300*time.Second {
		t.Errorf("conditions %q to %q, want the 300 seconds asked for", cond.attr("NotBefore"), cond.attr("NotOnOrAfter"))
	}
	if cond.child("ProxyRestriction").attr("Count") != "0" {
		t.Errorf("proxy restriction %v", cond.child("ProxyRestriction"))
	}
	if got := a.child("AuthnStatement").child("AuthnContext").text("AuthnContextClassRef"); got != "urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport" {
		t.Errorf("authentication context %q", got)
	}
	attrs := a.child("AttributeStatement").all("Attribute")
	wantAttrs := [][2]string{
		{"Groups", "http://rsa.com/schemas/attr-names/2009/01/GroupIdentity"},
		{"Subject Type", "http://vmware.com/schemas/attr-names/2011/07/isSolution"},
		{"Service Account", "http://vmware.com/schemas/attr-names/2025/05/isServiceAccount"},
		{"givenName", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/givenname"},
		{"surname", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/surname"},
	}
	if len(attrs) != len(wantAttrs) {
		t.Fatalf("%d attributes, want %d", len(attrs), len(wantAttrs))
	}
	for i, w := range wantAttrs {
		if attrs[i].attr("FriendlyName") != w[0] || attrs[i].attr("Name") != w[1] || attrs[i].attr("NameFormat") != "urn:oasis:names:tc:SAML:2.0:attrname-format:uri" {
			t.Errorf("attribute %d: %v", i, attrs[i].Attrs)
		}
		for _, v := range attrs[i].all("AttributeValue") {
			if v.attr("type") != "xsd:string" {
				t.Errorf("attribute %s value type %q", w[0], v.attr("type"))
			}
		}
	}
	want := []string{`vsphere.local\Administrators`, `vsphere.local\Everyone`, `vsphere.local\LicenseService.Administrators`, `vsphere.local\SystemConfiguration.Administrators`}
	if got := groupsOf(a); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("groups %v, want %v", got, want)
	}

	// a token that was changed after it was signed does not verify: the
	// check above is a real one
	forged := bytes.Replace(answer, []byte(`vsphere.local\Everyone`), []byte(`vsphere.local\Somebody`), 1)
	var env element
	if err := xml.Unmarshal(forged, &env); err != nil {
		t.Fatal(err)
	}
	fa := env.child("Body").Children[0].child("RequestSecurityTokenResponse").child("RequestedSecurityToken").child("Assertion")
	body := canonical(t, forged, "Assertion", []string{"xsd", "xsi"}, func(e xml.StartElement) bool { return e.Name.Local == "Signature" })
	digest := sha256.Sum256(body)
	if base64.StdEncoding.EncodeToString(digest[:]) == fa.child("Signature").child("SignedInfo").child("Reference").text("DigestValue") {
		t.Error("a changed token still matches its digest")
	}
}

// A wrong password, an unknown user and a request with no user name token
// are refused, and no token comes back.
func TestTokenServiceRefusesWrongCredentials(t *testing.T) {
	_, srv, _ := signOnSim(t, nil)
	now := time.Now().UTC()
	for name, body := range map[string][]byte{
		"wrong password": tokenRequest("operator", "nope", now, now.Add(time.Minute)),
		"unknown user":   tokenRequest("nobody", "s3cret", now, now.Add(time.Minute)),
		"empty password": tokenRequest("operator", "", now, now.Add(time.Minute)),
		"no user token": []byte(`<S:Envelope xmlns:S="http://schemas.xmlsoap.org/soap/envelope/"><S:Body><RequestSecurityToken xmlns="http://docs.oasis-open.org/ws-sx/ws-trust/200512">` +
			`<RequestType>http://docs.oasis-open.org/ws-sx/ws-trust/200512/Issue</RequestType></RequestSecurityToken></S:Body></S:Envelope>`),
	} {
		res, out := postTo(t, srv, tokenPath, stsAction, body)
		if res.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500: %s", name, res.StatusCode, out)
		}
		if strings.Contains(string(out), "Assertion") || strings.Contains(string(out), "RequestedSecurityToken") {
			t.Errorf("%s: a token came back: %s", name, out)
		}
		if soapBody(t, out).XMLName.Local != "Fault" {
			t.Errorf("%s: not a fault: %s", name, out)
		}
	}
	// a request that is not an Issue for a bearer token is not answered
	renew := bytes.Replace(tokenRequest("operator", "s3cret", now, now.Add(time.Minute)), []byte("200512/Issue<"), []byte("200512/Renew<"), 1)
	if res, _ := postTo(t, srv, tokenPath, stsAction, renew); res.StatusCode != http.StatusNotFound {
		t.Errorf("a Renew request: status %d, want 404", res.StatusCode)
	}
}

// The scenario's switch leaves the two licensing groups out of the token,
// and a read-only account is never in an administrators group. A custom
// group list is used as given.
func TestTokenGroupsFollowTheScenario(t *testing.T) {
	s, srv, _ := signOnSim(t, nil)
	groups := func(user, password string) string {
		_, a := issueToken(t, srv, user, password)
		return strings.Join(groupsOf(a), ",")
	}
	s.SetLicensing(scenario.Licensing{WithoutLicensingGroups: true})
	if got, want := groups("operator", "s3cret"), `vsphere.local\Everyone,vsphere.local\SystemConfiguration.Administrators`; got != want {
		t.Errorf("without the licensing groups: %s, want %s", got, want)
	}
	// the token is still a good one
	answer, a := issueToken(t, srv, "operator", "s3cret")
	if err := verifyToken(t, answer, a, issuerCertificate(t, srv)); err != nil {
		t.Errorf("the token without the groups does not verify: %v", err)
	}
	s.SetLicensing(scenario.Licensing{})
	if got := groups("operator", "s3cret"); !strings.Contains(got, `vsphere.local\LicenseService.Administrators`) || !strings.Contains(got, `vsphere.local\Administrators`) {
		t.Errorf("with the switch off again: %s", got)
	}
	if got := groups("viewer@vsphere.local", "l00king"); got != `vsphere.local\Everyone` {
		t.Errorf("read-only account: %s, want Everyone alone", got)
	}
	s.SetLicensing(scenario.Licensing{Groups: []string{"Everyone", "Operators", "Administrators"}, WithoutLicensingGroups: true})
	if got, want := groups("operator", "s3cret"), `vsphere.local\Everyone,vsphere.local\Operators`; got != want {
		t.Errorf("custom groups: %s, want %s", got, want)
	}
}

// A user name with characters XML escapes is signed as written, and the
// token still verifies.
func TestTokenForNameThatNeedsEscaping(t *testing.T) {
	_, srv, _ := signOnSim(t, func(o *sim.Options) {
		o.Credentials = map[string]string{`a&b<c>"d"@vsphere.local`: "s3cret"}
		o.UserRole = nil
	})
	answer, a := issueToken(t, srv, `a&amp;b&lt;c&gt;"d"@vsphere.local`, "s3cret")
	if got := strings.TrimSpace(a.child("Subject").child("NameID").Text); got != `a&b<c>"d"@vsphere.local` {
		t.Errorf("name id %q", got)
	}
	if err := verifyToken(t, answer, a, issuerCertificate(t, srv)); err != nil {
		t.Errorf("does not verify: %v", err)
	}
}

// vCenter's own setting that names its token service points at the path
// the simulator serves, under the address tools use.
func TestSignOnSettingsNameTheServedPaths(t *testing.T) {
	_, srv, _ := signOnSim(t, func(o *sim.Options) { o.PublicAddress = "https://vcenter.example.test:8443" })
	cookie := &http.Cookie{Name: "vmware_soap_session", Value: sdkLogin(t, srv)}
	for key, want := range map[string]string{
		"config.vpxd.sso.sts.uri":        "https://vcenter.example.test:8443" + tokenPath,
		"config.vpxd.sso.admin.uri":      "https://vcenter.example.test:8443" + ssoPath,
		"config.vpxd.sso.groupcheck.uri": "https://vcenter.example.test:8443" + ssoPath,
	} {
		_, out := postTo(t, srv, "/sdk", `"urn:vim25/8.0.2.0"`, soapEnvelope(`<QueryOptions xmlns="urn:vim25"><_this type="OptionManager">VpxSettings</_this><name>`+key+`</name></QueryOptions>`), cookie)
		if got := soapBody(t, out).child("returnval").text("value"); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// another simulator in the same process keeps its own setting
	_, other, _ := signOnSim(t, nil)
	cookie = &http.Cookie{Name: "vmware_soap_session", Value: sdkLogin(t, other)}
	_, out := postTo(t, other, "/sdk", `"urn:vim25/8.0.2.0"`, soapEnvelope(`<QueryOptions xmlns="urn:vim25"><_this type="OptionManager">VpxSettings</_this><name>config.vpxd.sso.sts.uri</name></QueryOptions>`), cookie)
	if got := soapBody(t, out).child("returnval").text("value"); got != "https://127.0.0.1"+tokenPath {
		t.Errorf("a second simulator's setting is %q", got)
	}
}
