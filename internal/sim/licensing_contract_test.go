package sim_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// These wire contracts come from the 9.1.1 adapter's runtime VMODL
// annotations, not the simulator's structs: cis.license data interfaces in
// license-cis-vmodl-bindings, lookup ServiceRegistration.Info (including
// MutableServiceInfo/CommonServiceInfo), and sso ServiceContent/AboutInfo.
// A getter without @optional requires an element even if its value is empty.
// Optional objects are checked recursively whenever the answer includes them.
// The proprietary jars are used only for local proof, never in CI or the repo.
type wireContract struct {
	required  []string
	children  map[string]*wireContract
	reference bool
}

func wire(required string, children map[string]*wireContract) *wireContract {
	return &wireContract{required: strings.Fields(required), children: children}
}

func (c *wireContract) check(e element) error {
	for _, name := range c.required {
		if len(e.all(name)) == 0 {
			return fmt.Errorf("%s: missing required %s", e.XMLName.Local, name)
		}
	}
	if c.reference && (e.attr("type") == "" || strings.TrimSpace(e.Text) == "") {
		return fmt.Errorf("%s: missing managed reference type or value", e.XMLName.Local)
	}
	for _, child := range e.Children {
		if nested := c.children[child.XMLName.Local]; nested != nil {
			if err := nested.check(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// Check every required field's absence as well as the complete emitted answer.
// This catches an incomplete oracle or a validator that silently accepts a
// missing nested field, the failure the Go XML decoder did not catch originally.
func checkWireContract(t *testing.T, e element, c *wireContract) {
	t.Helper()
	if err := c.check(e); err != nil {
		t.Fatal(err)
	}
	for _, name := range c.required {
		missing := e
		missing.Children = nil
		for _, child := range e.Children {
			if child.XMLName.Local != name {
				missing.Children = append(missing.Children, child)
			}
		}
		if err := c.check(missing); err == nil {
			t.Errorf("accepted absent required %s/%s", e.XMLName.Local, name)
		}
	}
	if c.reference {
		missing := e
		missing.Attrs = nil
		if c.check(missing) == nil {
			t.Error("accepted absent reference type")
		}
		missing = e
		missing.Text = ""
		if c.check(missing) == nil {
			t.Error("accepted absent reference value")
		}
	}
	for _, child := range e.Children {
		if nested := c.children[child.XMLName.Local]; nested != nil {
			checkWireContract(t, child, nested)
		}
	}
}

func assetWireContract() *wireContract {
	unit := wire("id", nil)
	property := wire("key value", nil)
	family := wire("name version", nil)
	product := wire("id name editionKey family", map[string]*wireContract{"family": family})
	capability := wire("version", map[string]*wireContract{"features": unit, "properties": property})
	limit := wire("unit value enforcementType", map[string]*wireContract{"unit": unit})
	restriction := wire("product capability", map[string]*wireContract{"product": product, "capability": capability, "costUnitsLimit": limit})
	licenseInfo := wire("name restrictions", map[string]*wireContract{"restrictions": restriction, "properties": property})
	license := wire("id info", map[string]*wireContract{"info": licenseInfo})
	usage := wire("unit value", map[string]*wireContract{"unit": unit})
	info := wire("instanceId name product inEvaluation", map[string]*wireContract{
		"product": product, "license": license, "evalInfo": license, "costUnitUsage": usage, "properties": property, "featuresInUse": unit,
	})
	return wire("id info", map[string]*wireContract{"info": info})
}

func TestSearchAssetsRequiredFields(t *testing.T) {
	_, srv, _ := signOnSim(t, nil)
	cookie := licenseLogin(t, srv)
	assets := searchAssets(t, srv, cookie, "")
	if len(assets) != 5 {
		t.Fatalf("want one vCenter and four hosts, got %d", len(assets))
	}
	for i, asset := range assets {
		t.Run(fmt.Sprint(i), func(t *testing.T) { checkWireContract(t, asset, assetWireContract()) })
	}
}

func TestLicenseLoginRequiredFields(t *testing.T) {
	_, srv, _ := signOnSim(t, nil)
	res, out := postTo(t, srv, "/ls/sdk", licenseAction, soapEnvelope(`<Login xmlns="urn:internalcis.license"><_this type="CisLicenseSessionManagementService">cis.license.SessionManagementService</_this><username>operator</username><password>s3cret</password></Login>`))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("Login status %d", res.StatusCode)
	}
	answer := soapBody(t, out)
	// SessionManagementService.login returns void, so no returnval is required.
	if answer.XMLName.Local != "LoginResponse" || answer.XMLName.Space != "urn:internalcis.license" || len(answer.Children) != 0 {
		t.Fatalf("Login must return an empty response: %v", answer)
	}
	licenseCookieValue(t, res)
}

func TestLookupRequiredFields(t *testing.T) {
	_, srv, _ := signOnSim(t, nil)
	ref := &wireContract{reference: true}
	content := wire("lookupService deploymentInformationService l10n", map[string]*wireContract{
		"lookupService": ref, "serviceRegistration": ref, "deploymentInformationService": ref, "l10n": ref,
	})
	attribute := wire("key value", nil)
	endpoint := wire("url endpointType", map[string]*wireContract{"endpointAttributes": attribute})
	registration := wire("serviceVersion ownerId serviceType serviceId siteId", map[string]*wireContract{
		"serviceType": wire("product type", nil), "serviceEndpoints": endpoint, "serviceAttributes": attribute,
	})
	t.Run("RetrieveServiceContent", func(t *testing.T) {
		res, out := postTo(t, srv, "/lookupservice/sdk", lookupAction, soapEnvelope(`<RetrieveServiceContent xmlns="urn:internallookup"><_this type="LookupServiceInstance">ServiceInstance</_this></RetrieveServiceContent>`))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status %d", res.StatusCode)
		}
		checkWireContract(t, soapBody(t, out), wire("returnval", map[string]*wireContract{"returnval": content}))
	})
	for _, filter := range []struct{ name, service, protocol, endpoint string }{
		{"vCenter", "vcenterserver", "vmomi", "com.vmware.vim"},
		{"SSO", "cs.identity", "vmomi", "com.vmware.cis.cs.identity.admin"},
		{"STS", "cs.identity", "wsTrust", "com.vmware.cis.cs.identity.sso"},
	} {
		t.Run(filter.name, func(t *testing.T) {
			res, out := postTo(t, srv, "/lookupservice/sdk", lookupAction, lookupList("ServiceRegistration", "com.vmware.cis", filter.service, filter.protocol, filter.endpoint))
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status %d", res.StatusCode)
			}
			answer := soapBody(t, out)
			if len(answer.all("returnval")) != 1 {
				t.Fatal("expected one registration")
			}
			checkWireContract(t, answer, wire("", map[string]*wireContract{"returnval": registration}))
		})
	}
}

func TestSSOAdminRequiredFields(t *testing.T) {
	_, srv, _ := signOnSim(t, nil)
	ref := &wireContract{reference: true}
	fields := "aboutInfo sessionManager configurationManagementService smtpManagementService principalDiscoveryService principalManagementService roleManagementService passwordPolicyService lockoutPolicyService domainManagementService deploymentInformationService replicationService"
	children := map[string]*wireContract{"aboutInfo": wire("version build apiRevision clusterId deploymentId", nil)}
	for _, name := range strings.Fields(fields) {
		if name != "aboutInfo" {
			children[name] = ref
		}
	}
	children["identitySourceManagementService"] = ref
	children["systemManagementService"] = ref
	for _, tc := range []struct {
		op, kind, id string
		contract     *wireContract
	}{
		{"SsoAdminServiceInstance", "SsoAdminServiceInstance", "SsoAdminServiceInstance", wire(fields, children)},
		{"IdS_getSystemDomainName", "SsoAdminIdentitySourceManagementService", "identitySourceManagementService", wire("", nil)},
		{"IdS_getSslCertificateManager", "SsoAdminIdentitySourceManagementService", "identitySourceManagementService", ref},
		{"GetIssuersCertificates", "SsoAdminConfigurationManagementService", "configurationManagementService", wire("", nil)},
	} {
		t.Run(tc.op, func(t *testing.T) {
			request := fmt.Sprintf(`<%s xmlns="urn:sso"><_this type="%s">%s</_this></%s>`, tc.op, tc.kind, tc.id, tc.op)
			res, out := postTo(t, srv, ssoPath, ssoAction, soapEnvelope(request))
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status %d", res.StatusCode)
			}
			answer := soapBody(t, out)
			if answer.XMLName.Local != tc.op+"Response" || answer.XMLName.Space != "urn:sso" {
				t.Fatalf("unexpected response %v", answer.XMLName)
			}
			checkWireContract(t, answer, wire("returnval", map[string]*wireContract{"returnval": tc.contract}))
			if tc.op == "IdS_getSslCertificateManager" && answer.child("returnval").attr("type") != "SsoAdminCertificateManager" {
				t.Fatal("wrong manager type")
			}
		})
	}
}
