package control

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/signon"
)

const (
	stsAddress = "/sts/STSService/vsphere.local"
	ssoAddress = "/sso-adminserver/sdk/vsphere.local"
	markerKey  = "com.vmware.vrops.license.managed"
)

// tokenGroups asks the token service for a token and returns the groups it
// names, in order.
func tokenGroups(t *testing.T, a *apiClient, user, password string) string {
	t.Helper()
	st, body := a.do(http.MethodPost, stsAddress, `<S:Envelope xmlns:S="http://schemas.xmlsoap.org/soap/envelope/"><S:Header>`+
		`<wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">`+
		`<wsse:UsernameToken><wsse:Username>`+user+`</wsse:Username><wsse:Password>`+password+`</wsse:Password></wsse:UsernameToken></wsse:Security></S:Header>`+
		`<S:Body><RequestSecurityToken xmlns="http://docs.oasis-open.org/ws-sx/ws-trust/200512">`+
		`<RequestType>http://docs.oasis-open.org/ws-sx/ws-trust/200512/Issue</RequestType></RequestSecurityToken></S:Body></S:Envelope>`)
	if st != http.StatusOK {
		t.Fatalf("token request: %d %s", st, body)
	}
	var groups []string
	for _, m := range regexp.MustCompile(`<saml2:AttributeValue xsi:type="xsd:string">vsphere\.local\\([^<]+)</saml2:AttributeValue>`).FindAllStringSubmatch(body, -1) {
		groups = append(groups, m[1])
	}
	return strings.Join(groups, ",")
}

// issuerOf fetches the signing certificate, base64.
func issuerOf(t *testing.T, a *apiClient) string {
	t.Helper()
	st, body := a.post(ssoAddress, `<GetIssuersCertificates xmlns="urn:sso"><_this type="SsoAdminConfigurationManagementService">configurationManagementService</_this></GetIssuersCertificates>`)
	m := regexp.MustCompile(`<returnval>([^<]+)</returnval>`).FindStringSubmatch(body)
	if st != http.StatusOK || m == nil {
		t.Fatalf("GetIssuersCertificates: %d %s", st, body)
	}
	if _, err := base64.StdEncoding.DecodeString(m[1]); err != nil {
		t.Fatal(err)
	}
	return m[1]
}

// evaluationState reads the vCenter's own asset from the license service:
// whether its expiration is ahead.
func evaluationAhead(t *testing.T, a *apiClient) bool {
	t.Helper()
	if st, body := a.post("/ls/sdk", `<Login xmlns="urn:internalcis.license"><_this type="CisLicenseSessionManagementService">cis.license.SessionManagementService</_this><username>administrator@vsphere.local</username><password>first-password</password></Login>`); st != http.StatusOK {
		t.Fatalf("license login: %d %s", st, body)
	}
	st, body := a.post("/ls/sdk", `<SearchAssets xmlns="urn:internalcis.license"><_this type="CisLicenseManagementSystemManagementService">cis.license.management.SystemManagementService</_this>`+
		`<assetSearchSpec xsi:type="CisLicenseManagementAssetSearchSpecByProductFamilyNames"><maxResults>200</maxResults><offset>0</offset><productFamilyNames>VMware VirtualCenter Server</productFamilyNames></assetSearchSpec></SearchAssets>`)
	m := regexp.MustCompile(`<license><id>urn:vri:com\.vmware\.license\.license:Evaluation</id><info><name>Evaluation</name><expiration>([^<]+)</expiration>`).FindStringSubmatch(body)
	if st != http.StatusOK || m == nil || !strings.Contains(body, "<inEvaluation>true</inEvaluation>") {
		t.Fatalf("SearchAssets: %d %s", st, body)
	}
	ends, err := time.Parse("2006-01-02T15:04:05Z", m[1])
	if err != nil {
		t.Fatal(err)
	}
	return ends.After(time.Now())
}

func findMarker(t *testing.T, a *apiClient) string {
	t.Helper()
	st, body := a.post("/sdk", `<FindExtension xmlns="urn:vim25"><_this type="ExtensionManager">ExtensionManager</_this><extensionKey>`+markerKey+`</extensionKey></FindExtension>`)
	if st != http.StatusOK {
		t.Fatalf("FindExtension: %d %s", st, body)
	}
	return body
}

// The licensing task changes what tools are told at once, without a new
// vCenter: tools stay signed in and every identity is kept. The choices
// survive a restart.
func TestLicensingTask(t *testing.T) {
	dir := t.TempDir()
	c := newCtl(t, small(t), dir, "t")
	tool := newTool(t, c, "tool/1")
	if !tool.login("administrator@vsphere.local", "first-password") {
		t.Fatal("login")
	}
	before, version := identities(c), c.applied().version

	// as it starts: in evaluation, licensing administrators, no manager
	if v := c.Security().Licensing; v.Expired || !v.Admins || v.Managed || !v.Ends.After(time.Now().Add(88*24*time.Hour)) {
		t.Fatalf("licensing at start: %+v", v)
	}
	if !evaluationAhead(t, tool) {
		t.Fatal("the evaluation is not ahead at start")
	}
	wantContains(t, tokenGroups(t, tool, "administrator@vsphere.local", "first-password"), "LicenseService.Administrators")

	// nothing changed is not a change
	r := do(t, c, "licensing", nil, nil)
	wantContains(t, r.Errors["_"], "Nothing has been changed yet.")
	wantContains(t, preview(t, c, "licensing", nil, nil), "Nothing is different yet.")

	// what the box promises
	said := preview(t, c, "licensing", nil, map[string]any{"state": "expired", "groups": "out"})
	wantContains(t, said, "evaluation has run out")
	wantContains(t, said, "no longer licensing administrators")
	wantContains(t, said, "Tools stay signed in.")
	wantContains(t, said, "This is not a license.")

	out := must(t, c, "licensing", nil, map[string]any{"state": "expired", "groups": "out"})
	wantContains(t, out.Done, "The evaluation has run out, accounts are not licensing administrators, and no VCF Operations manages the license.")
	if v := c.Security().Licensing; !v.Expired || v.Admins || !v.Ends.Before(time.Now()) {
		t.Errorf("licensing after the change: %+v", v)
	}
	if evaluationAhead(t, tool) {
		t.Error("tools are still told the evaluation is ahead")
	}
	if got := tokenGroups(t, tool, "administrator@vsphere.local", "first-password"); got != "Everyone,SystemConfiguration.Administrators" {
		t.Errorf("token groups after the switch: %s", got)
	}
	// no new vCenter was built: the session and the identities are the same
	if st, body := tool.post("/sdk", `<Fetch xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><prop>currentSession</prop></Fetch>`); st != http.StatusOK || !strings.Contains(body, "UserSession") {
		t.Errorf("the tool was signed out: %d %s", st, body)
	}
	if c.applied().version != version {
		t.Errorf("a new vCenter was built: version %d, was %d", c.applied().version, version)
	}
	after := identities(c)
	for k, v := range before {
		if after[k] != v {
			t.Errorf("identity %s changed", k)
		}
	}
	wantContains(t, history(c), "Changed how licensing looks to tools.")

	// a restart keeps the choices
	c.Close()
	c2 := newCtl(t, small(t), dir, "t")
	if v := c2.Security().Licensing; !v.Expired || v.Admins {
		t.Errorf("licensing after a restart: %+v", v)
	}
	// and back
	must(t, c2, "licensing", nil, map[string]any{"state": "evaluation", "groups": "in"})
	if v := c2.Security().Licensing; v.Expired || !v.Admins {
		t.Errorf("licensing after changing back: %+v", v)
	}

	// it can be scheduled like any other change
	at := time.Now().Add(time.Hour)
	form, err := c2.TaskForm("licensing", nil, Caller{})
	if err != nil {
		t.Fatal(err)
	}
	form.Values["state"] = "expired"
	res, err := c2.TaskRun("licensing", params(t, form.Values), &at, false, Caller{})
	if err != nil || res.Scheduled == nil {
		t.Fatalf("scheduling: %v %+v", err, res)
	}
	if c2.Security().Licensing.Expired {
		t.Error("a scheduled change was made at once")
	}
}

// The operator can say another VCF Operations manages the license, with an
// id and an address they type. The marker shows on the vCenter, survives a
// change that builds a new vCenter and a restart, and can be taken away.
func TestLicensingTaskNamesAManager(t *testing.T) {
	dir := t.TempDir()
	c := newCtl(t, small(t), dir, "t")
	tool := newTool(t, c, "tool/1")
	if !tool.login("administrator@vsphere.local", "first-password") {
		t.Fatal("login")
	}
	if body := findMarker(t, tool); strings.Contains(body, "returnval") {
		t.Fatalf("the marker is there at start: %s", body)
	}

	// the form suggests a made-up id and an address that is nobody's
	form, err := c.TaskForm("licensing", nil, Caller{})
	if err != nil {
		t.Fatal(err)
	}
	suggested, _ := form.Values["identity"].(string)
	if !scenario.ValidManagerIdentity(suggested) || suggested == c.applied().topo.InstanceUUID || form.Values["url"] != "https://operations.example.test" {
		t.Errorf("suggested manager: %v at %v", form.Values["identity"], form.Values["url"])
	}

	// values that are not an id or an address are refused by name
	r := do(t, c, "licensing", nil, map[string]any{"manager": "other", "identity": "our operations", "url": "operations.example.test"})
	wantContains(t, r.Errors["identity"], "Enter an id in the form")
	wantContains(t, r.Errors["url"], "starts with https://")
	if c.Security().Licensing.Managed {
		t.Fatal("a refused change was made")
	}

	const id, url = "11111111-2222-4333-8444-555555555555", "https://ops-primary.example.test"
	wantContains(t, preview(t, c, "licensing", nil, map[string]any{"manager": "other", "identity": id, "url": url}), "managed by the VCF Operations at "+url)
	out := must(t, c, "licensing", nil, map[string]any{"manager": "other", "identity": id, "url": url + "/"})
	wantContains(t, out.Done, "the license is managed by the VCF Operations at "+url+".")
	if v := c.Security().Licensing; !v.Managed || v.ManagerID != id || v.ManagerURL != url {
		t.Errorf("licensing view: %+v", v)
	}
	shows := func(a *apiClient) {
		t.Helper()
		body := findMarker(t, a)
		for _, want := range []string{
			`<key>` + markerKey + `</key>`, `<company>Broadcom, Inc.</company>`, `<label>VMware Cloud Foundation Operations</label>`,
			`<managedEntityInfo><type>LM_IDENTITY</type><description>` + id + `</description></managedEntityInfo>`,
			`<managedEntityInfo><type>LM_URL</type><description>` + url + `</description></managedEntityInfo>`,
			`<shownInSolutionManager>false</shownInSolutionManager>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the marker lacks %s: %s", want, body)
			}
		}
	}
	shows(tool)

	// a change that builds a new vCenter keeps it
	must(t, c, "add-hosts", map[string]string{"cluster": "dc-01/cluster-01"}, map[string]any{"n": 1})
	if !tool.login("administrator@vsphere.local", "first-password") {
		t.Fatal("login after the rebuild")
	}
	shows(tool)

	// so does a restart
	c.Close()
	c2 := newCtl(t, small(t), dir, "t")
	tool2 := newTool(t, c2, "tool/2")
	if !tool2.login("administrator@vsphere.local", "first-password") {
		t.Fatal("login after the restart")
	}
	shows(tool2)

	// and it can be taken away again
	must(t, c2, "licensing", nil, map[string]any{"manager": "none"})
	if body := findMarker(t, tool2); strings.Contains(body, "returnval") {
		t.Errorf("the marker is still there: %s", body)
	}
	if c2.Security().Licensing.Managed {
		t.Error("the Security screen still names a manager")
	}
}

// A VCF Operations that registers the marker itself has claimed the
// simulator: the console says so, and the claim outlives the running
// vCenter and a restart, with what the Operations registered.
func TestAToolCanClaimTheLicense(t *testing.T) {
	dir := t.TempDir()
	c := newCtl(t, small(t), dir, "t")
	tool := newTool(t, c, "ops/1")
	if !tool.login("administrator@vsphere.local", "first-password") {
		t.Fatal("login")
	}
	const id, url = "aaaaaaaa-0000-4000-8000-000000000001", "https://ops-dev.example.test"
	extension := func(id, url string) string {
		return `<extension><description><label>VMware Cloud Foundation Operations</label><summary>VMware Cloud Foundation Operations</summary></description>` +
			`<key>` + markerKey + `</key><company>Broadcom, Inc.</company><version>9.0.2</version><subjectName>made-up subject</subjectName>` +
			`<lastHeartbeatTime>2026-11-16T00:00:00Z</lastHeartbeatTime>` +
			`<managedEntityInfo><type>LM_IDENTITY</type><description>` + id + `</description></managedEntityInfo>` +
			`<managedEntityInfo><type>LM_URL</type><description>` + url + `</description></managedEntityInfo>` +
			`<shownInSolutionManager>false</shownInSolutionManager></extension>`
	}
	waitFor := func(c *Controller, what string, ok func(LicensingView) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok(c.Security().Licensing) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the console still shows %+v", what, c.Security().Licensing)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if st, body := tool.post("/sdk", `<RegisterExtension xmlns="urn:vim25"><_this type="ExtensionManager">ExtensionManager</_this>`+extension(id, url)+`</RegisterExtension>`); st != http.StatusOK {
		t.Fatalf("RegisterExtension: %d %s", st, body)
	}
	waitFor(c, "after RegisterExtension", func(v LicensingView) bool { return v.Managed && v.ManagerID == id && v.ManagerURL == url })
	wantContains(t, history(c), "A tool marked this vCenter's license as managed by the VCF Operations at "+url+".")
	// the request was answered, and is filed under add-ons
	if call := lastCall(c); call.Method != "RegisterExtension" || call.Reason != "answered" {
		t.Errorf("the registration was recorded as %s, %s", call.Method, call.Reason)
	}

	// the form now shows the claim, and pressing the button with it as it
	// is changes nothing
	r := do(t, c, "licensing", nil, nil)
	wantContains(t, r.Errors["_"], "Nothing has been changed yet.")
	// another choice can be changed while the claim stays as registered
	must(t, c, "licensing", nil, map[string]any{"groups": "out"})
	if v := c.Security().Licensing; v.Admins || !v.Managed || v.ManagerID != id {
		t.Errorf("after changing the groups beside a claim: %+v", v)
	}
	if body := findMarker(t, tool); !strings.Contains(body, `<subjectName>made-up subject</subjectName>`) {
		t.Errorf("the claim lost what the Operations registered: %s", body)
	}

	// a new vCenter and a restart both keep what the Operations registered,
	// subject name and version included
	must(t, c, "add-hosts", map[string]string{"cluster": "dc-01/cluster-01"}, map[string]any{"n": 1})
	c.Close()
	c2 := newCtl(t, small(t), dir, "t")
	tool2 := newTool(t, c2, "ops/1")
	if !tool2.login("administrator@vsphere.local", "first-password") {
		t.Fatal("login after the restart")
	}
	body := findMarker(t, tool2)
	for _, want := range []string{`<description>` + id + `</description>`, `<description>` + url + `</description>`, `<subjectName>made-up subject</subjectName>`, `<version>9.0.2</version>`} {
		if !strings.Contains(body, want) {
			t.Errorf("after a restart the marker lacks %s: %s", want, body)
		}
	}

	// the Operations lets go
	if st, body := tool2.post("/sdk", `<UnregisterExtension xmlns="urn:vim25"><_this type="ExtensionManager">ExtensionManager</_this><extensionKey>`+markerKey+`</extensionKey></UnregisterExtension>`); st != http.StatusOK {
		t.Fatalf("UnregisterExtension: %d %s", st, body)
	}
	waitFor(c2, "after UnregisterExtension", func(v LicensingView) bool { return !v.Managed })
	wantContains(t, history(c2), "A tool removed the mark that says a VCF Operations manages this vCenter's license.")
}

// The key tokens are signed with is kept in the data directory: the same
// after a restart, and after a change that builds a new vCenter.
func TestSigningKeyOutlivesARestart(t *testing.T) {
	dir := t.TempDir()
	c := newCtl(t, small(t), dir, "t")
	first := issuerOf(t, newTool(t, c, "tool/1"))
	if info, err := os.Stat(filepath.Join(dir, signon.File)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the key file: %v %v", info, err)
	}
	must(t, c, "add-hosts", map[string]string{"cluster": "dc-01/cluster-01"}, map[string]any{"n": 1})
	if issuerOf(t, newTool(t, c, "tool/1")) != first {
		t.Error("the signing certificate changed with a new vCenter")
	}
	c.Close()
	c2 := newCtl(t, small(t), dir, "t")
	if issuerOf(t, newTool(t, c2, "tool/1")) != first {
		t.Error("the signing certificate changed across a restart")
	}
	// the console's saved state never holds the key
	state, err := os.ReadFile(filepath.Join(dir, "console.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state), "PRIVATE KEY") {
		t.Error("the console's state file holds a private key")
	}
}

// With a certificate file from the deployment, the lookup service names
// that certificate; a file that cannot be used stops the start.
func TestLookupTrustFileFromTheDeployment(t *testing.T) {
	dir := t.TempDir()
	c := newCtl(t, small(t), dir, "t")
	front, err := os.ReadFile(filepath.Join(dir, "tls", "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	// stand-in for the certificate of something in front of the simulator
	other := t.TempDir()
	c0 := newCtl(t, small(t), other, "t")
	theirs, err := os.ReadFile(filepath.Join(other, "tls", "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	c0.Close()
	file := filepath.Join(t.TempDir(), "front.pem")
	if err := os.WriteFile(file, theirs, 0o600); err != nil {
		t.Fatal(err)
	}
	opts := c.opts
	opts.LookupTrustFile = file
	c2, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c2.Close)
	srv := httptest.NewServer(c2)
	tool := toolOn(t, srv, "tool/1")
	st, body := tool.post("/lookupservice/sdk", `<List xmlns="urn:internallookup"><_this type="LookupServiceRegistration">ServiceRegistration</_this><filterCriteria>`+
		`<serviceType><product>com.vmware.cis</product><type>cs.identity</type></serviceType><endpointType><protocol>wsTrust</protocol><type>com.vmware.cis.cs.identity.sso</type></endpointType></filterCriteria></List>`)
	if st != http.StatusOK {
		t.Fatalf("List: %d %s", st, body)
	}
	der := func(pemText []byte) string {
		lines := strings.Split(strings.TrimSpace(string(pemText)), "\n")
		return strings.Join(lines[1:len(lines)-1], "")
	}
	if !strings.Contains(body, "<sslTrust>"+der(theirs)+"</sslTrust>") {
		t.Error("the lookup service does not name the deployment's certificate")
	}
	if strings.Contains(body, der(front)) {
		t.Error("the lookup service still names the simulator's own certificate")
	}
	// the address is the one the tool used
	wantContains(t, body, "<url>https://"+strings.TrimPrefix(srv.URL, "http://")+stsAddress+"</url>")
	// the request is filed as answered, in plain words
	if call := lastCall(c2); call.Reason != "answered" || kindOf(call) != "Where vCenter's other services are (the lookup service)" {
		t.Errorf("the lookup request was recorded as %s, %q", call.Reason, kindOf(call))
	}

	opts.LookupTrustFile = filepath.Join(t.TempDir(), "missing.pem")
	if broken, err := New(opts); err == nil {
		broken.Close()
		t.Error("a certificate file that is not there was accepted")
	}
}

// A token request with a wrong password is a refused sign-in, not a
// request the simulator cannot answer, and a stale session ticket is a
// refusal a real vCenter makes too.
func TestSignOnRefusalsAreNotUnanswered(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	tool := newTool(t, c, "tool/1")
	st, _ := tool.do(http.MethodPost, stsAddress, `<S:Envelope xmlns:S="http://schemas.xmlsoap.org/soap/envelope/"><S:Header>`+
		`<wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">`+
		`<wsse:UsernameToken><wsse:Username>administrator@vsphere.local</wsse:Username><wsse:Password>wrong</wsse:Password></wsse:UsernameToken></wsse:Security></S:Header>`+
		`<S:Body><RequestSecurityToken xmlns="http://docs.oasis-open.org/ws-sx/ws-trust/200512">`+
		`<RequestType>http://docs.oasis-open.org/ws-sx/ws-trust/200512/Issue</RequestType></RequestSecurityToken></S:Body></S:Envelope>`)
	if call := lastCall(c); st != http.StatusInternalServerError || call.Reason != "denied" {
		t.Errorf("a wrong password at the token service: status %d, recorded as %s", st, call.Reason)
	}
	st, _ = tool.post("/sdk", `<LoginBySessionTicket xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><ticketData>made-up</ticketData></LoginBySessionTicket>`)
	if call := lastCall(c); st != http.StatusInternalServerError || call.Reason != "refused" || call.Fault != "InvalidTicket" {
		t.Errorf("a stale ticket: status %d, recorded as %s (%s)", st, call.Reason, call.Fault)
	}
	if n := c.Requests().Counts["unknown"]; n != 0 {
		t.Errorf("%d requests count as unanswered, want none", n)
	}
}
