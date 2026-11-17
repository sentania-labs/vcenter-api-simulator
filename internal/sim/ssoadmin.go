package sim

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"

	"github.com/vmware/govmomi/vim25/xml"

	"github.com/sentania/vcenter-api-simulator/internal/signon"
)

// The sign-on admin service answers three reads without a sign-in, the three
// a tool makes before it asks for a token: the service content, the name of
// the system domain, and the certificate tokens are signed with.

type ssoRef struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type ssoServiceContent struct {
	XMLName   xml.Name `xml:"urn:sso SsoAdminServiceInstanceResponse"`
	Returnval struct {
		AboutInfo struct {
			Version      string `xml:"version"`
			Build        string `xml:"build"`
			APIRevision  string `xml:"apiRevision"`
			ClusterID    string `xml:"clusterId"`
			DeploymentID string `xml:"deploymentId"`
		} `xml:"aboutInfo"`
		SessionManager         ssoRef `xml:"sessionManager"`
		ConfigurationManager   ssoRef `xml:"configurationManagementService"`
		SMTPManager            ssoRef `xml:"smtpManagementService"`
		PrincipalDiscovery     ssoRef `xml:"principalDiscoveryService"`
		PrincipalManagement    ssoRef `xml:"principalManagementService"`
		RoleManagement         ssoRef `xml:"roleManagementService"`
		PasswordPolicy         ssoRef `xml:"passwordPolicyService"`
		LockoutPolicy          ssoRef `xml:"lockoutPolicyService"`
		DomainManagement       ssoRef `xml:"domainManagementService"`
		IdentitySourceManager  ssoRef `xml:"identitySourceManagementService"`
		SystemManagement       ssoRef `xml:"systemManagementService"`
		DeploymentInformation  ssoRef `xml:"deploymentInformationService"`
		ReplicationInformation ssoRef `xml:"replicationService"`
	} `xml:"returnval"`
}

type ssoDomainName struct {
	XMLName   xml.Name `xml:"urn:sso IdS_getSystemDomainNameResponse"`
	Returnval string   `xml:"returnval"`
}

type ssoIssuersCertificates struct {
	XMLName   xml.Name `xml:"urn:sso GetIssuersCertificatesResponse"`
	Returnval []string `xml:"returnval"`
}

// serveSSOAdmin answers the three reads on the sign-on admin address. Any
// other operation, and any other domain's address, stays unanswered and is
// captured.
func (h *handler) serveSSOAdmin(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != ssoAdminPath || r.Method != http.MethodPost {
		return false
	}
	body, err := readBody(r, maxLocalSOAPBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return true
	}
	switch soapOperation(body) {
	case "SsoAdminServiceInstance":
		anonymousCookie(w)
		encodeSOAP(w, http.StatusOK, h.ssoContent())
	case "IdS_getSystemDomainName":
		anonymousCookie(w)
		encodeSOAP(w, http.StatusOK, ssoDomainName{Returnval: signon.SystemDomain})
	case "GetIssuersCertificates":
		material, err := h.issuer.Material()
		if err != nil {
			http.Error(w, "the sign-on key is not available", http.StatusInternalServerError)
			return true
		}
		anonymousCookie(w)
		encodeSOAP(w, http.StatusOK, ssoIssuersCertificates{Returnval: []string{base64.StdEncoding.EncodeToString(material.Leaf)}})
	default:
		rewind(r, body)
		return false
	}
	return true
}

func (h *handler) ssoContent() ssoServiceContent {
	var c ssoServiceContent
	about := &c.Returnval.AboutInfo
	about.Version, about.Build, about.ClusterID, about.DeploymentID = "0", "N/A", "N/A", "N/A"
	// a real vCenter's revision is 64 hex characters; this one is the
	// simulator's own, the same for as long as the seed is
	sum := sha256.Sum256([]byte("vcenter-api-simulator sign-on api revision|" + h.sim.Topology.Seed))
	about.APIRevision = hex.EncodeToString(sum[:])
	rv := &c.Returnval
	rv.SessionManager = ssoRef{"SsoSessionManager", "ssoSessionManager"}
	rv.ConfigurationManager = ssoRef{"SsoAdminConfigurationManagementService", "configurationManagementService"}
	rv.SMTPManager = ssoRef{"SsoAdminSmtpManagementService", "smtpManagementService"}
	rv.PrincipalDiscovery = ssoRef{"SsoAdminPrincipalDiscoveryService", "principalDiscoveryService"}
	rv.PrincipalManagement = ssoRef{"SsoAdminPrincipalManagementService", "principalManagementService"}
	rv.RoleManagement = ssoRef{"SsoAdminRoleManagementService", "roleManagementService"}
	rv.PasswordPolicy = ssoRef{"SsoAdminPasswordPolicyService", "passwordPolicyService"}
	rv.LockoutPolicy = ssoRef{"SsoAdminLockoutPolicyService", "lockoutPolicyService"}
	rv.DomainManagement = ssoRef{"SsoAdminDomainManagementService", "domainManagementService"}
	rv.IdentitySourceManager = ssoRef{"SsoAdminIdentitySourceManagementService", "identitySourceManagementService"}
	rv.SystemManagement = ssoRef{"SsoAdminSystemManagementService", "systemManagementService"}
	rv.DeploymentInformation = ssoRef{"SsoAdminDeploymentInformationService", "deploymentInformationService"}
	rv.ReplicationInformation = ssoRef{"SsoAdminReplicationService", "replicationService"}
	return c
}
