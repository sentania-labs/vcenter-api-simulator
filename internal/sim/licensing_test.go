package sim_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

const (
	licenseAction = `"urn:internalcis.license/4.0"`
	licenseThis   = `<_this type="CisLicenseManagementSystemManagementService">cis.license.management.SystemManagementService</_this>`
	evaluationID  = "urn:vri:com.vmware.license.license:Evaluation"
)

// licenseLogin signs in to the license service and returns its cookie.
func licenseLogin(t *testing.T, srv *httptest.Server) *http.Cookie {
	t.Helper()
	res, out := postTo(t, srv, "/ls/sdk", licenseAction, soapEnvelope(`<Login xmlns="urn:internalcis.license">`+
		`<_this type="CisLicenseSessionManagementService">cis.license.SessionManagementService</_this>`+
		`<username>operator</username><password>s3cret</password></Login>`))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("license login %d: %s", res.StatusCode, out)
	}
	return licenseCookieValue(t, res)
}

// searchAssets sends one search and returns the assets, having checked that
// the answer sets the session cookie again.
func searchAssets(t *testing.T, srv *httptest.Server, cookie *http.Cookie, spec string) []element {
	t.Helper()
	res, out := postTo(t, srv, "/ls/sdk", licenseAction, soapEnvelope(`<SearchAssets xmlns="urn:internalcis.license">`+licenseThis+spec+`</SearchAssets>`), cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("SearchAssets %d: %s", res.StatusCode, out)
	}
	if again := licenseCookieValue(t, res); again.Value != cookie.Value {
		t.Errorf("the answer set cookie %q, want the session's own again", again.Value)
	}
	body := soapBody(t, out)
	if body.XMLName.Local != "SearchAssetsResponse" || body.XMLName.Space != "urn:internalcis.license" {
		t.Fatalf("answer is %v", body.XMLName)
	}
	return body.all("returnval")
}

func assetSpec(kind, inner string) string {
	return `<assetSearchSpec xsi:type="` + kind + `">` + inner + `</assetSearchSpec>`
}

// properties reads an element's key and value pairs with their types.
func properties(e element) (keys []string, values, types map[string]string) {
	values, types = map[string]string{}, map[string]string{}
	for _, p := range e.all("properties") {
		k := p.text("key")
		keys = append(keys, k)
		values[k], types[k] = p.text("value"), p.child("value").attr("type")
	}
	return
}

// The vCenter's own asset, as the search VCF Operations makes for it
// returns it: in evaluation, in the element order a real vCenter uses.
func TestLicenseServiceVCenterAssetInEvaluation(t *testing.T) {
	_, srv, topo := signOnSim(t, nil)
	cookie := licenseLogin(t, srv)
	host := strings.TrimPrefix(srv.URL, "http://")
	name, _, _ := strings.Cut(host, ":")

	assets := searchAssets(t, srv, cookie, assetSpec("CisLicenseManagementAssetSearchSpecByProductFamilyNames",
		`<scopeId>`+topo.NodeID+`</scopeId><maxResults>200</maxResults><offset>0</offset><productFamilyNames>VMware VirtualCenter Server</productFamilyNames>`))
	if len(assets) != 1 {
		t.Fatalf("%d assets, want exactly the vCenter", len(assets))
	}
	a := assets[0]
	if got, want := a.text("id"), "urn:vri:com.vmware.license.asset:"+topo.InstanceUUID+"-"+topo.NodeID; got != want {
		t.Errorf("id %q, want %q", got, want)
	}
	info := a.child("info")
	if got, want := strings.Join(info.names(), ","), "instanceId,scopeId,name,version,product,evalInfo,costUnitUsage,license,inEvaluation"; got != want {
		t.Errorf("info elements %s, want %s", got, want)
	}
	if info.text("instanceId") != topo.InstanceUUID || info.text("scopeId") != topo.NodeID || info.text("name") != name {
		t.Errorf("instance %q scope %q name %q", info.text("instanceId"), info.text("scopeId"), info.text("name"))
	}
	if got := info.text("version"); got != "9.1.1.0" || len(got) != 7 {
		t.Errorf("version %q, want the four-part 9.1.1.0", got)
	}
	// what the adapter needs: a product, a license with its info, and the
	// evaluation flag
	product := info.child("product")
	if got, want := strings.Join(product.names(), ","), "id,name,editionKey,family"; got != want {
		t.Errorf("product elements %s, want %s", got, want)
	}
	if product.text("id") != "VMware VirtualCenter Server9.1vcEvalserver" || product.text("name") != "Evaluation Mode" || product.text("editionKey") != "vcEval" ||
		product.text("family", "name") != "VMware VirtualCenter Server" || product.text("family", "version") != "9.1" {
		t.Errorf("product %v", product)
	}
	if info.text("inEvaluation") != "true" {
		t.Errorf("inEvaluation %q", info.text("inEvaluation"))
	}
	if u := info.child("costUnitUsage"); u.text("unit", "id") != "server" || u.text("value") != "1" {
		t.Errorf("cost unit usage %v", u)
	}

	license := info.child("license")
	if license.text("id") != evaluationID || license.text("info", "name") != "Evaluation" {
		t.Errorf("license %q named %q", license.text("id"), license.text("info", "name"))
	}
	expiration := license.text("info", "expiration")
	ends, err := time.Parse("2006-01-02T15:04:05Z", expiration)
	if err != nil || len(expiration) != 20 {
		t.Fatalf("expiration %q, want a 20-character time", expiration)
	}
	if left := time.Until(ends); left < 88*24*time.Hour || left > 91*24*time.Hour {
		t.Errorf("the evaluation runs out in %v, want about 90 days", left)
	}
	keys, values, types := properties(license.child("info"))
	if got, want := strings.Join(keys, ","), "expirationHours,expirationMinutes,expirationDate"; got != want {
		t.Errorf("license properties %s, want only %s", got, want)
	}
	if types["expirationHours"] != "xsd:long" || types["expirationMinutes"] != "xsd:long" || types["expirationDate"] != "xsd:dateTime" || values["expirationDate"] != expiration {
		t.Errorf("license property types %v values %v", types, values)
	}
	if hours, _ := strconv.Atoi(values["expirationHours"]); hours < 88*24 || hours > 90*24 {
		t.Errorf("expirationHours %q", values["expirationHours"])
	}

	// the evaluation block, as every vCenter asset carries it
	eval := info.child("evalInfo")
	if eval.text("id") != evaluationID || eval.text("info", "name") != "Evaluation" || eval.text("info", "expiration") != expiration {
		t.Errorf("evalInfo %v", eval)
	}
	if got, want := strings.Join(eval.child("info").names(), ","), "name,expiration,restrictions,properties,properties,properties"; got != want {
		t.Errorf("evalInfo elements %s, want %s", got, want)
	}
	r := eval.child("info").child("restrictions")
	if got, want := strings.Join(r.names(), ","), "product,costUnitsLimit,capability"; got != want {
		t.Errorf("restriction elements %s, want %s", got, want)
	}
	if r.text("product", "editionKey") != "vcEval" {
		t.Errorf("restriction product %v", r.child("product"))
	}
	if l := r.child("costUnitsLimit"); l.text("unit", "id") != "server" || l.text("value") != "0" || l.text("enforcementType") != "hardEnforced" {
		t.Errorf("cost limit %v", l)
	}
	var features []string
	for _, f := range r.child("capability").all("features") {
		features = append(features, f.text("id"))
	}
	if got, want := strings.Join(features, ","), "linkedvc,woe,vsa,xvp,vcha,backuprestore,appliancemigration"; got != want {
		t.Errorf("features %s, want %s", got, want)
	}
	if got := r.child("capability").text("version"); len(got) != 7 {
		t.Errorf("capability version %q, want 7 characters", got)
	}

	// nothing a licensed vCenter would carry
	raw := fmt.Sprint(a)
	for _, never := range []string{"signedEntitlement", "entitlementId", "allocationId", "onEntitlement", "licenseServerId"} {
		if strings.Contains(raw, never) {
			t.Errorf("the asset names %s, which only a licensed vCenter has", never)
		}
	}
}

// Every form of search the adapter builds, and paging through the result.
func TestLicenseServiceSearchFormsAndPaging(t *testing.T) {
	_, srv, topo := signOnSim(t, nil)
	cookie := licenseLogin(t, srv)
	scope := `<scopeId>` + topo.NodeID + `</scopeId>`
	families := func(assets []element) string {
		count := map[string]int{}
		for _, a := range assets {
			count[a.child("info").child("product").text("family", "name")]++
		}
		return fmt.Sprintf("vc=%d esx=%d", count["VMware VirtualCenter Server"], count["VMware ESX Server"])
	}
	for _, tc := range []struct{ name, spec, want string }{
		{"plain, by scope", assetSpec("CisLicenseManagementAssetSearchSpec", scope+`<maxResults>200</maxResults>`), "vc=1 esx=4"},
		{"no spec at all", "", "vc=1 esx=4"},
		{"another scope", assetSpec("CisLicenseManagementAssetSearchSpec", `<scopeId>00000000-0000-0000-0000-000000000000</scopeId><maxResults>200</maxResults>`), "vc=0 esx=0"},
		{"by state: evaluation", assetSpec("CisLicenseManagementAssetSearchSpecByLicenseState", scope+`<maxResults>200</maxResults><offset>0</offset><licenseState>evaluation</licenseState>`), "vc=1 esx=4"},
		{"by state: licensed", assetSpec("CisLicenseManagementAssetSearchSpecByLicenseState", scope+`<maxResults>200</maxResults><offset>0</offset><licenseState>licensed</licenseState>`), "vc=0 esx=0"},
		{"by license id: a license", assetSpec("CisLicenseManagementAssetSearchSpecByLicenseId", scope+`<maxResults>200</maxResults><offset>0</offset><licenseId>urn:vri:com.vmware.license.license:11111111-1111-4111-8111-111111111111</licenseId>`), "vc=0 esx=0"},
		{"by license id: the evaluation", assetSpec("CisLicenseManagementAssetSearchSpecByLicenseId", scope+`<maxResults>200</maxResults><offset>0</offset><licenseId>`+evaluationID+`</licenseId>`), "vc=1 esx=4"},
		{"by family: vCenter", assetSpec("CisLicenseManagementAssetSearchSpecByProductFamilyNames", scope+`<maxResults>200</maxResults><offset>0</offset><productFamilyNames>VMware VirtualCenter Server</productFamilyNames>`), "vc=1 esx=0"},
		{"by family: the usage families", assetSpec("CisLicenseManagementAssetSearchSpecByProductFamilyNames", scope+`<maxResults>200</maxResults><offset>0</offset>`+
			`<productFamilyNames>VMware ESX Server</productFamilyNames><productFamilyNames>VMware VSAN</productFamilyNames>`+
			`<productFamilyNames>VMware Private AI Foundation with NVIDIA, and SQL Support</productFamilyNames><productFamilyNames>VMware VirtualCenter Server</productFamilyNames>`), "vc=1 esx=4"},
		{"by family: hosts", assetSpec("CisLicenseManagementAssetSearchSpecByProductFamilyNames", scope+`<maxResults>200</maxResults><offset>0</offset><productFamilyNames>VMware ESX Server</productFamilyNames>`), "vc=0 esx=4"},
		{"by family: one with no assets", assetSpec("CisLicenseManagementAssetSearchSpecByProductFamilyNames", scope+`<maxResults>200</maxResults><offset>0</offset><productFamilyNames>VMware VSAN</productFamilyNames>`), "vc=0 esx=0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := families(searchAssets(t, srv, cookie, tc.spec)); got != tc.want {
				t.Errorf("%s, want %s", got, tc.want)
			}
		})
	}

	t.Run("a host asset", func(t *testing.T) {
		hosts := searchAssets(t, srv, cookie, assetSpec("CisLicenseManagementAssetSearchSpecByProductFamilyNames", scope+`<maxResults>200</maxResults><offset>0</offset><productFamilyNames>VMware ESX Server</productFamilyNames>`))
		info := hosts[0].child("info")
		if !strings.HasPrefix(info.text("instanceId"), "host-") || hosts[0].text("id") != "urn:vri:com.vmware.license.asset:"+info.text("instanceId")+"-"+topo.NodeID {
			t.Errorf("host asset id %q instance %q", hosts[0].text("id"), info.text("instanceId"))
		}
		if info.text("product", "editionKey") != "esxEval" || info.text("inEvaluation") != "true" || info.text("license", "id") != evaluationID || info.text("license", "info", "name") != "Evaluation" {
			t.Errorf("host asset %v", info)
		}
		keys, _, _ := properties(info.child("license").child("info"))
		if strings.Join(keys, ",") != "expirationHours,expirationMinutes,expirationDate" {
			t.Errorf("host license properties %v", keys)
		}
		units := map[string]string{}
		for _, u := range info.all("costUnitUsage") {
			units[u.text("unit", "id")] = u.text("value")
		}
		// the built-in scenario's hosts: one processor, 16 threads, 8 cores
		if units["cpuPackage"] != "1" || units["cpuCore"] != "8" {
			t.Errorf("cost units %v", units)
		}
	})

	t.Run("paging", func(t *testing.T) {
		page := func(offset, size int) []string {
			var ids []string
			for _, a := range searchAssets(t, srv, cookie, assetSpec("CisLicenseManagementAssetSearchSpec", scope+fmt.Sprintf(`<maxResults>%d</maxResults><offset>%d</offset>`, size, offset))) {
				ids = append(ids, a.text("id"))
			}
			return ids
		}
		all := page(0, 200)
		if len(all) != 5 {
			t.Fatalf("%d assets in one page, want 5", len(all))
		}
		// the way the adapter pages: again with the offset advanced while a
		// page comes back full
		var paged []string
		for offset := 0; ; offset += 2 {
			got := page(offset, 2)
			paged = append(paged, got...)
			if len(got) < 2 {
				break
			}
		}
		if strings.Join(paged, " ") != strings.Join(all, " ") {
			t.Errorf("paged %v\n  want %v", paged, all)
		}
		if got := page(5, 2); len(got) != 0 {
			t.Errorf("a page past the end holds %v", got)
		}
		if got := page(99, 2); len(got) != 0 {
			t.Errorf("a page far past the end holds %v", got)
		}
	})

	t.Run("a search the simulator does not know stays unanswered", func(t *testing.T) {
		res, _ := postTo(t, srv, "/ls/sdk", licenseAction, soapEnvelope(`<SearchAssets xmlns="urn:internalcis.license">`+licenseThis+assetSpec("CisLicenseManagementAssetSearchSpecBySomethingElse", scope)+`</SearchAssets>`), cookie)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("an unknown search form: status %d, want 404", res.StatusCode)
		}
		res, _ = postTo(t, srv, "/ls/sdk", licenseAction, soapEnvelope(`<SearchAssets xmlns="urn:internalcis.license"><_this type="CisLicenseManagementSystemManagementService">something.else</_this></SearchAssets>`), cookie)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("another management object: status %d, want 404", res.StatusCode)
		}
	})
}

// There are no licenses to list, the service tells the time, and none of
// the reads answers a caller that is not signed in.
func TestLicenseServiceLicensesTimeAndSignIn(t *testing.T) {
	_, srv, _ := signOnSim(t, nil)
	cookie := licenseLogin(t, srv)

	res, out := postTo(t, srv, "/ls/sdk", licenseAction, soapEnvelope(`<SearchLicenses xmlns="urn:internalcis.license">`+licenseThis+`</SearchLicenses>`), cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("SearchLicenses %d: %s", res.StatusCode, out)
	}
	if body := soapBody(t, out); body.XMLName.Local != "SearchLicensesResponse" || len(body.Children) != 0 {
		t.Errorf("SearchLicenses: %s", out)
	}
	licenseCookieValue(t, res)

	res, out = postTo(t, srv, "/ls/sdk", licenseAction, soapEnvelope(`<QuerySystemTime xmlns="urn:internalcis.license">`+licenseThis+`</QuerySystemTime>`), cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("QuerySystemTime %d: %s", res.StatusCode, out)
	}
	at := soapBody(t, out).text("returnval")
	if when, err := time.Parse("2006-01-02T15:04:05.000Z", at); err != nil || len(at) != 24 || time.Since(when).Abs() > time.Minute {
		t.Errorf("system time %q, want the time now in 24 characters", at)
	}

	for _, op := range []string{"SearchAssets", "SearchLicenses", "QuerySystemTime"} {
		body := soapEnvelope(`<` + op + ` xmlns="urn:internalcis.license">` + licenseThis + `</` + op + `>`)
		for name, cookies := range map[string][]*http.Cookie{"no cookie": nil, "a made-up cookie": {{Name: "vmware_soap_session", Value: strings.Repeat("0", 36)}}} {
			res, out := postTo(t, srv, "/ls/sdk", licenseAction, body, cookies...)
			if res.StatusCode != http.StatusInternalServerError || strings.Contains(string(out), "Response") {
				t.Errorf("%s with %s: status %d: %s", op, name, res.StatusCode, out)
			}
		}
	}
	// a signed-out session reads nothing either
	postTo(t, srv, "/ls/sdk", licenseAction, soapEnvelope(`<Logout xmlns="urn:internalcis.license"><_this type="CisLicenseSessionManagementService">cis.license.SessionManagementService</_this></Logout>`), cookie)
	if res, _ := postTo(t, srv, "/ls/sdk", licenseAction, soapEnvelope(`<SearchLicenses xmlns="urn:internalcis.license">`+licenseThis+`</SearchLicenses>`), cookie); res.StatusCode != http.StatusInternalServerError {
		t.Errorf("SearchLicenses after sign-out: status %d", res.StatusCode)
	}
}

// In the expired state the vCenter is still in evaluation, with an
// expiration in the past and no time left.
func TestLicenseServiceExpiredEvaluation(t *testing.T) {
	s, srv, topo := signOnSim(t, func(o *sim.Options) { o.Licensing = scenario.Licensing{State: scenario.LicenseExpiredEvaluation} })
	cookie := licenseLogin(t, srv)
	spec := assetSpec("CisLicenseManagementAssetSearchSpecByProductFamilyNames", `<scopeId>`+topo.NodeID+`</scopeId><maxResults>200</maxResults><offset>0</offset><productFamilyNames>VMware VirtualCenter Server</productFamilyNames>`)
	read := func() (inEval string, ends time.Time, hours string) {
		info := searchAssets(t, srv, cookie, spec)[0].child("info")
		ends, err := time.Parse("2006-01-02T15:04:05Z", info.text("license", "info", "expiration"))
		if err != nil {
			t.Fatal(err)
		}
		_, values, _ := properties(info.child("license").child("info"))
		return info.text("inEvaluation"), ends, values["expirationHours"]
	}
	inEval, ends, hours := read()
	if inEval != "true" || !ends.Before(time.Now()) || hours != "0" {
		t.Errorf("expired: inEvaluation %s, ends %v, hours left %s", inEval, ends, hours)
	}
	// and back, while it runs
	s.SetLicensing(scenario.Licensing{})
	if _, ends, hours = read(); !ends.After(time.Now()) || hours == "0" {
		t.Errorf("in evaluation again: ends %v, hours left %s", ends, hours)
	}
}

// With many hosts the adapter's pages of 200 cover every asset once.
func TestLicenseServicePagesOfTwoHundred(t *testing.T) {
	sc, err := scenario.Parse([]byte("seed: paging\ndatacenters:\n  - name: dc-01\n    clusters: 6\ndefaults:\n  hosts: 40\n  host: {threads: 2, domWorlds: 0, localDevices: 0}\n  vms: {perHost: 0}\n"))
	if err != nil {
		t.Fatal(err)
	}
	topo := topology.Generate(sc, registry.InMemory(sc.Seed))
	s, err := sim.New(sim.Options{Topology: topo, Credentials: signOnUsers})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s.Handler)
	t.Cleanup(srv.Close)
	cookie := licenseLogin(t, srv)
	seen := map[string]bool{}
	pages := 0
	for offset := 0; ; offset += 200 {
		got := searchAssets(t, srv, cookie, assetSpec("CisLicenseManagementAssetSearchSpec", fmt.Sprintf(`<scopeId>%s</scopeId><maxResults>200</maxResults><offset>%d</offset>`, topo.NodeID, offset)))
		pages++
		for _, a := range got {
			if seen[a.text("id")] {
				t.Fatalf("asset %s came back twice", a.text("id"))
			}
			seen[a.text("id")] = true
		}
		if len(got) < 200 {
			break
		}
	}
	if len(seen) != 241 || pages != 2 {
		t.Errorf("%d assets in %d pages, want 241 (240 hosts and the vCenter) in 2", len(seen), pages)
	}
}

// LoginBySessionTicket answers the fault a real vCenter answers a ticket it
// did not issue, whatever the ticket.
func TestLoginBySessionTicketIsInvalidTicket(t *testing.T) {
	_, srv, _ := signOnSim(t, nil)
	session := sdkLogin(t, srv)
	for name, ticket := range map[string]string{"a made-up value": "cst-0123456789", "a session key": session, "empty": ""} {
		res, out := postTo(t, srv, "/sdk", `"urn:vim25/8.0.2.0"`, soapEnvelope(`<LoginBySessionTicket xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><ticketData>`+ticket+`</ticketData></LoginBySessionTicket>`))
		if res.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500", name, res.StatusCode)
		}
		fault := soapBody(t, out)
		detail := fault.child("detail").child("InvalidTicketFault")
		if fault.XMLName.Local != "Fault" || fault.text("faultcode") != "ServerFaultCode" || fault.text("faultstring") != "The session ticket is invalid." ||
			detail.XMLName.Space != "urn:vim25" || detail.attr("type") != "InvalidTicket" {
			t.Errorf("%s: fault %s", name, out)
		}
		if len(res.Cookies()) != 0 {
			t.Errorf("%s: a session was issued: %v", name, res.Cookies())
		}
		if strings.Contains(string(out), session) && ticket == session {
			t.Errorf("%s: the fault repeats the ticket", name)
		}
	}
	// the session that was signed in is untouched
	res, out := postTo(t, srv, "/sdk", `"urn:vim25/8.0.2.0"`, soapEnvelope(`<Fetch xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><prop>currentSession</prop></Fetch>`), &http.Cookie{Name: "vmware_soap_session", Value: session})
	if res.StatusCode != http.StatusOK || !strings.Contains(string(out), "UserSession") {
		t.Errorf("the signed-in session after the refusals: %d %s", res.StatusCode, out)
	}
}

func findExtension(t *testing.T, srv *httptest.Server, cookie *http.Cookie, key string) element {
	t.Helper()
	res, out := postTo(t, srv, "/sdk", `"urn:vim25/8.0.2.0"`, soapEnvelope(`<FindExtension xmlns="urn:vim25"><_this type="ExtensionManager">ExtensionManager</_this><extensionKey>`+key+`</extensionKey></FindExtension>`), cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("FindExtension %d: %s", res.StatusCode, out)
	}
	return soapBody(t, out).child("returnval")
}

// marker is the license-managed extension as a VCF Operations registers it.
func marker(identity, url string) string {
	return `<extension><description><label>VMware Cloud Foundation Operations</label><summary>VMware Cloud Foundation Operations</summary></description>` +
		`<key>com.vmware.vrops.license.managed</key><company>Broadcom, Inc.</company><version>9.1.1</version><subjectName>made-up subject</subjectName>` +
		`<lastHeartbeatTime>2026-11-16T00:00:00Z</lastHeartbeatTime>` +
		`<managedEntityInfo><type>LM_IDENTITY</type><description>` + identity + `</description></managedEntityInfo>` +
		`<managedEntityInfo><type>LM_URL</type><description>` + url + `</description></managedEntityInfo>` +
		`<shownInSolutionManager>false</shownInSolutionManager></extension>`
}

func managerInfo(ext element) map[string]string {
	out := map[string]string{}
	for _, m := range ext.all("managedEntityInfo") {
		out[m.text("type")] = m.text("description")
	}
	return out
}

// With no manager in the scenario the marker is absent; with one, it is
// there in the shape a real vCenter shows, naming the id and address given.
func TestLicenseManagedMarkerFromScenario(t *testing.T) {
	const (
		id  = "11111111-2222-4333-8444-555555555555"
		url = "https://operations.example.test"
	)
	s, srv, _ := signOnSim(t, nil)
	cookie := &http.Cookie{Name: "vmware_soap_session", Value: sdkLogin(t, srv)}
	if ext := findExtension(t, srv, cookie, scenario.LicenseManagedKey); len(ext.Children) != 0 {
		t.Fatalf("the marker is there with no manager in the scenario: %v", ext)
	}

	s.SetLicensing(scenario.Licensing{Manager: &scenario.LicenseManager{Identity: id, URL: url}})
	ext := findExtension(t, srv, cookie, scenario.LicenseManagedKey)
	if got, want := strings.Join(ext.names(), ","), "description,key,company,version,lastHeartbeatTime,managedEntityInfo,managedEntityInfo,shownInSolutionManager"; got != want {
		t.Errorf("marker elements %s, want %s (a subject name is left out unless an Operations registered one)", got, want)
	}
	if ext.text("description", "label") != "VMware Cloud Foundation Operations" || ext.text("description", "summary") != "VMware Cloud Foundation Operations" ||
		ext.text("key") != "com.vmware.vrops.license.managed" || ext.text("company") != "Broadcom, Inc." || ext.text("version") != "9.1.1" || ext.text("shownInSolutionManager") != "false" {
		t.Errorf("marker %v", ext)
	}
	if d := ext.child("description"); len(d.Attrs) != 0 || strings.Join(d.names(), ",") != "label,summary" {
		t.Errorf("description has attributes %v and elements %v, want none and label,summary", d.Attrs, d.names())
	}
	if infos := ext.all("managedEntityInfo"); len(infos) != 2 || infos[0].text("type") != "LM_IDENTITY" || infos[0].text("description") != id ||
		infos[1].text("type") != "LM_URL" || infos[1].text("description") != url {
		t.Errorf("managed entity info %v", infos)
	}
	if _, err := time.Parse(time.RFC3339Nano, ext.text("lastHeartbeatTime")); err != nil {
		t.Errorf("lastHeartbeatTime %q", ext.text("lastHeartbeatTime"))
	}

	// another id replaces it; none removes it
	s.SetLicensing(scenario.Licensing{Manager: &scenario.LicenseManager{Identity: "99999999-2222-4333-8444-555555555555", URL: url}})
	if got := managerInfo(findExtension(t, srv, cookie, scenario.LicenseManagedKey))["LM_IDENTITY"]; got != "99999999-2222-4333-8444-555555555555" {
		t.Errorf("after a change the marker names %q", got)
	}
	s.SetLicensing(scenario.Licensing{})
	if ext := findExtension(t, srv, cookie, scenario.LicenseManagedKey); len(ext.Children) != 0 {
		t.Errorf("the marker is still there: %v", ext)
	}
	// the other extensions are left alone throughout
	if ext := findExtension(t, srv, cookie, "com.vmware.govmomi.simulator"); ext.text("key") == "" {
		t.Error("another extension was removed with the marker")
	}
}

// A VCF Operations can claim the simulator: register the marker, read it
// back as registered, change it, and remove it. Each change is reported.
func TestLicenseManagedMarkerRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var reports []*scenario.LicenseManager
	reported := func(n int) []*scenario.LicenseManager {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			got := append([]*scenario.LicenseManager(nil), reports...)
			mu.Unlock()
			if len(got) >= n {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d reports of a changed marker, want %d", len(got), n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	var running atomic.Pointer[sim.Simulator]
	s, srv, _ := signOnSim(t, func(o *sim.Options) {
		// a report carries nothing: the marker is read when it arrives
		o.LicenseManagerChanged = func() {
			m := running.Load().LicenseManager()
			mu.Lock()
			reports = append(reports, m)
			mu.Unlock()
		}
	})
	running.Store(s)
	cookie := &http.Cookie{Name: "vmware_soap_session", Value: sdkLogin(t, srv)}
	call := func(op, inner string, cookies ...*http.Cookie) (*http.Response, []byte) {
		return postTo(t, srv, "/sdk", `"urn:vim25/8.0.2.0"`, soapEnvelope(`<`+op+` xmlns="urn:vim25"><_this type="ExtensionManager">ExtensionManager</_this>`+inner+`</`+op+`>`), cookies...)
	}
	const (
		mine  = "aaaaaaaa-0000-4000-8000-000000000001"
		other = "bbbbbbbb-0000-4000-8000-000000000002"
	)

	// not signed in: refused, and nothing registered
	if res, _ := call("RegisterExtension", marker(mine, "https://ops-a.example.test")); res.StatusCode != http.StatusInternalServerError {
		t.Errorf("RegisterExtension without a session: status %d", res.StatusCode)
	}
	if ext := findExtension(t, srv, cookie, scenario.LicenseManagedKey); len(ext.Children) != 0 {
		t.Fatal("an anonymous caller registered the marker")
	}

	res, out := call("RegisterExtension", marker(mine, "https://ops-a.example.test"), cookie)
	if res.StatusCode != http.StatusOK || soapBody(t, out).XMLName.Local != "RegisterExtensionResponse" {
		t.Fatalf("RegisterExtension %d: %s", res.StatusCode, out)
	}
	ext := findExtension(t, srv, cookie, scenario.LicenseManagedKey)
	if got, want := strings.Join(ext.names(), ","), "description,key,company,version,subjectName,lastHeartbeatTime,managedEntityInfo,managedEntityInfo,shownInSolutionManager"; got != want {
		t.Errorf("marker elements %s, want %s", got, want)
	}
	if len(ext.child("description").Attrs) != 0 {
		t.Errorf("description has attributes %v, want none", ext.child("description").Attrs)
	}
	if info := managerInfo(ext); info["LM_IDENTITY"] != mine || info["LM_URL"] != "https://ops-a.example.test" || ext.text("subjectName") != "made-up subject" {
		t.Errorf("marker after RegisterExtension: %v", ext)
	}
	if got := reported(1)[0]; got == nil || got.Identity != mine || got.URL != "https://ops-a.example.test" || got.SubjectName != "made-up subject" || got.Version != "9.1.1" {
		t.Errorf("report of the registration: %+v", got)
	}

	// registering it a second time is refused, as vCenter refuses a key it has
	if res, _ := call("RegisterExtension", marker(other, "https://ops-b.example.test"), cookie); res.StatusCode != http.StatusInternalServerError {
		t.Errorf("a second RegisterExtension: status %d", res.StatusCode)
	}

	res, out = call("UpdateExtension", marker(other, "https://ops-b.example.test"), cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("UpdateExtension %d: %s", res.StatusCode, out)
	}
	if info := managerInfo(findExtension(t, srv, cookie, scenario.LicenseManagedKey)); info["LM_IDENTITY"] != other || info["LM_URL"] != "https://ops-b.example.test" {
		t.Errorf("marker after UpdateExtension: %v", info)
	}
	if got := reported(2)[1]; got == nil || got.Identity != other {
		t.Errorf("report of the update: %+v", got)
	}

	res, out = call("UnregisterExtension", `<extensionKey>com.vmware.vrops.license.managed</extensionKey>`, cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("UnregisterExtension %d: %s", res.StatusCode, out)
	}
	if ext := findExtension(t, srv, cookie, scenario.LicenseManagedKey); len(ext.Children) != 0 {
		t.Errorf("marker after UnregisterExtension: %v", ext)
	}
	if got := reported(3)[2]; got != nil {
		t.Errorf("report of the removal: %+v", got)
	}

	// changes to another extension are not reported as the marker's
	plugin := strings.Replace(marker(mine, "https://ops-a.example.test"), "com.vmware.vrops.license.managed", "com.example.plugin", 1)
	if res, _ := call("RegisterExtension", plugin, cookie); res.StatusCode != http.StatusOK {
		t.Fatalf("registering another extension: %d", res.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if got := reported(3); len(got) != 3 {
		t.Errorf("%d reports after another extension was registered, want still 3", len(got))
	}

	// nor are they when the marker is one the scenario described
	s.SetLicensing(scenario.Licensing{Manager: &scenario.LicenseManager{Identity: mine, URL: "https://ops-a.example.test"}})
	plugin2 := strings.Replace(plugin, "com.example.plugin", "com.example.plugin2", 1)
	if res, _ := call("RegisterExtension", plugin2, cookie); res.StatusCode != http.StatusOK {
		t.Fatalf("registering another extension: %d", res.StatusCode)
	}
	if res, _ := call("UnregisterExtension", `<extensionKey>com.example.plugin2</extensionKey>`, cookie); res.StatusCode != http.StatusOK {
		t.Fatalf("removing another extension: %d", res.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if got := reported(3); len(got) != 3 {
		t.Errorf("%d reports after another extension changed beside a described marker, want still 3", len(got))
	}
	s.SetLicensing(scenario.Licensing{})

	// a read-only account may read the marker and not change it
	viewer := &http.Cookie{Name: "vmware_soap_session", Value: sdkLoginAs(t, srv, "viewer@vsphere.local", "l00king")}
	if res, _ := call("RegisterExtension", marker(mine, "https://ops-a.example.test"), viewer); res.StatusCode != http.StatusInternalServerError {
		t.Errorf("RegisterExtension by a read-only account: status %d", res.StatusCode)
	}
	if ext := findExtension(t, srv, viewer, scenario.LicenseManagedKey); len(ext.Children) != 0 {
		t.Error("a read-only account registered the marker")
	}
}

// Reports of a changed marker may be delivered in any order. A report that
// arrives late must leave its reader with the marker as the vCenter holds
// it then, not as it was when the report was made: otherwise an older
// marker is put over a newer one, and saved.
func TestLicenseManagedReportsCarryNoStaleMarker(t *testing.T) {
	var (
		mu      sync.Mutex
		seen    []*scenario.LicenseManager
		first   = true
		release = make(chan struct{})
		running atomic.Pointer[sim.Simulator]
	)
	s, srv, _ := signOnSim(t, func(o *sim.Options) {
		o.LicenseManagerChanged = func() {
			mu.Lock()
			held := first
			first = false
			mu.Unlock()
			if held {
				<-release // the first report is delivered last
			}
			m := running.Load().LicenseManager()
			mu.Lock()
			seen = append(seen, m)
			mu.Unlock()
		}
	})
	running.Store(s)
	cookie := &http.Cookie{Name: "vmware_soap_session", Value: sdkLogin(t, srv)}
	call := func(op, inner string) {
		t.Helper()
		res, out := postTo(t, srv, "/sdk", `"urn:vim25/8.0.2.0"`, soapEnvelope(`<`+op+` xmlns="urn:vim25"><_this type="ExtensionManager">ExtensionManager</_this>`+inner+`</`+op+`>`), cookie)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", op, res.StatusCode, out)
		}
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(seen) }
	wait := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); count() < n; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%d reports arrived, want %d", count(), n)
			}
		}
	}
	const older, newer = "aaaaaaaa-0000-4000-8000-000000000001", "bbbbbbbb-0000-4000-8000-000000000002"
	call("RegisterExtension", marker(older, "https://ops-a.example.test")) // its report is held back
	call("UpdateExtension", marker(newer, "https://ops-b.example.test"))
	wait(1) // the report of the update has arrived, before the report of the registration
	close(release)
	wait(2)
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	if last == nil || last.Identity != newer {
		t.Errorf("the report delivered last left its reader with %+v, want the marker the vCenter holds, %s", last, newer)
	}
	if got := s.LicenseManager(); got == nil || got.Identity != newer {
		t.Errorf("the vCenter holds %+v", got)
	}
	// the helper that compares a held marker with a described one
	if !sim.SameLicenseManager(nil, nil) || sim.SameLicenseManager(last, nil) || sim.SameLicenseManager(nil, last) {
		t.Error("SameLicenseManager is wrong about an absent marker")
	}
	if !sim.SameLicenseManager(last, &scenario.LicenseManager{Identity: newer, URL: "https://ops-b.example.test", SubjectName: "made-up subject"}) {
		t.Error("a marker is not the same as its own description with the defaults left out")
	}
	if sim.SameLicenseManager(last, &scenario.LicenseManager{Identity: older, URL: "https://ops-b.example.test"}) {
		t.Error("a marker is the same as a description with another id")
	}
}

func sdkLoginAs(t *testing.T, srv *httptest.Server, user, password string) string {
	t.Helper()
	res, out := postTo(t, srv, "/sdk", `"urn:vim25/8.0"`, soapEnvelope(`<Login xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><userName>`+user+`</userName><password>`+password+`</password></Login>`))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("sdk login %d: %s", res.StatusCode, out)
	}
	for _, ck := range res.Cookies() {
		if ck.Name == "vmware_soap_session" {
			return ck.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}
