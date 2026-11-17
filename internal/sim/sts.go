package sim

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vmware/govmomi/vim25/xml"

	"github.com/sentania/vcenter-api-simulator/internal/signon"
)

// The token service issues a bearer SAML token to an account that names
// itself with a user name and password: a WS-Trust Issue. The token is
// signed with the simulator's own key, whose certificate the sign-on admin
// service hands out, so a tool that checks the signature finds it good. The
// token lists the groups the scenario gives an administrator; a tool reads
// from them whether the account may manage licenses.

const (
	nsSOAP  = "http://schemas.xmlsoap.org/soap/envelope/"
	nsTrust = "http://docs.oasis-open.org/ws-sx/ws-trust/200512"
	nsWSSE  = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd"
	nsWSU   = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd"
	nsSAML  = "urn:oasis:names:tc:SAML:2.0:assertion"
	nsDS    = "http://www.w3.org/2000/09/xmldsig#"
	nsXSD   = "http://www.w3.org/2001/XMLSchema"
	nsXSI   = "http://www.w3.org/2001/XMLSchema-instance"

	algExcC14N   = "http://www.w3.org/2001/10/xml-exc-c14n#"
	algEnveloped = "http://www.w3.org/2000/09/xmldsig#enveloped-signature"
	algRSASHA256 = "http://www.w3.org/2001/04/xmldsig-more#rsa-sha256"
	algSHA256    = "http://www.w3.org/2001/04/xmlenc#sha256"

	tokenTypeSAML2 = "urn:oasis:names:tc:SAML:2.0:assertion"
	keyTypeBearer  = nsTrust + "/Bearer"
	requestIssue   = nsTrust + "/Issue"

	// tokenLifetime is what a request that names no lifetime gets, and
	// maxTokenLifetime the most a request may ask for.
	tokenLifetime    = 300 * time.Second
	maxTokenLifetime = 30 * 24 * time.Hour

	stsTime = "2006-01-02T15:04:05.000Z"
)

type stsRequest struct {
	Header struct {
		Security struct {
			UsernameToken *struct {
				Username string `xml:"Username"`
				Password string `xml:"Password"`
			} `xml:"UsernameToken"`
		} `xml:"Security"`
	} `xml:"Header"`
	Body struct {
		RST *struct {
			RequestType string `xml:"RequestType"`
			KeyType     string `xml:"KeyType"`
			Lifetime    struct {
				Created string `xml:"Created"`
				Expires string `xml:"Expires"`
			} `xml:"Lifetime"`
		} `xml:"RequestSecurityToken"`
	} `xml:"Body"`
}

// serveSTS answers a WS-Trust Issue for a bearer token on the token
// service's address. Any other request there stays unanswered and is
// captured.
func (h *handler) serveSTS(w http.ResponseWriter, r *http.Request) bool {
	if (r.URL.Path != stsPath && r.URL.Path != "/sts/STSService") || r.Method != http.MethodPost {
		return false
	}
	body, err := readBody(r, maxLocalSOAPBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return true
	}
	var req stsRequest
	if err := xml.Unmarshal(body, &req); err != nil || req.Body.RST == nil {
		rewind(r, body)
		return false
	}
	rst := req.Body.RST
	if strings.TrimSpace(rst.RequestType) != requestIssue || (strings.TrimSpace(rst.KeyType) != "" && strings.TrimSpace(rst.KeyType) != keyTypeBearer) {
		rewind(r, body)
		return false
	}
	// serialized with a credential change, like every other sign-in
	h.accessMu.RLock()
	defer h.accessMu.RUnlock()
	token := req.Header.Security.UsernameToken
	if token == nil || !h.license.authorized(token.Username, token.Password) {
		stsFault(w)
		return true
	}
	material, err := h.issuer.Material()
	if err != nil {
		http.Error(w, "the sign-on key is not available", http.StatusInternalServerError)
		return true
	}
	now := h.sim.Env.Now().UTC()
	from, until := tokenWindow(rst.Lifetime.Created, rst.Lifetime.Expires, now)
	subject := token.Username
	if !strings.Contains(subject, "@") {
		subject += "@" + signon.SystemDomain
	}
	a := assertion{
		ID:      "_" + uuid.NewString(),
		Issuer:  "https://" + clientHost(r) + "/websso/SAML2/Metadata/" + signon.SystemDomain,
		Subject: subject,
		Issued:  now,
		From:    from,
		Until:   until,
		Groups:  h.tokenGroups(token.Username),
	}
	signed, err := a.sign(material)
	if err != nil {
		http.Error(w, "the token could not be signed", http.StatusInternalServerError)
		return true
	}
	h.rest.rememberBearer(a.ID, token.Username, a.Subject, a.Until, now)
	var out strings.Builder
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	out.WriteString(`<soapenv:Envelope xmlns:soapenv="` + nsSOAP + `"><soapenv:Body>`)
	out.WriteString(`<wst:RequestSecurityTokenResponseCollection xmlns:wst="` + nsTrust + `"><wst:RequestSecurityTokenResponse>`)
	out.WriteString(`<wst:RequestedSecurityToken>` + signed + `</wst:RequestedSecurityToken>`)
	out.WriteString(`<wst:Lifetime><wsu:Created xmlns:wsu="` + nsWSU + `">` + from.Format(stsTime) + `</wsu:Created>`)
	out.WriteString(`<wsu:Expires xmlns:wsu="` + nsWSU + `">` + until.Format(stsTime) + `</wsu:Expires></wst:Lifetime>`)
	out.WriteString(`<wst:TokenType>` + tokenTypeSAML2 + `</wst:TokenType><wst:KeyType>` + keyTypeBearer + `</wst:KeyType>`)
	out.WriteString(`<wst:Delegatable>false</wst:Delegatable><wst:SignatureAlgorithm>` + algRSASHA256 + `</wst:SignatureAlgorithm>`)
	out.WriteString(`</wst:RequestSecurityTokenResponse></wst:RequestSecurityTokenResponseCollection></soapenv:Body></soapenv:Envelope>`)
	w.Header().Set("Content-Type", "text/xml;charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(out.String()))
	return true
}

// tokenWindow is the validity a token gets: the lifetime the request asked
// for, on the request's own clock, when it is a usable one; otherwise five
// minutes from now.
func tokenWindow(created, expires string, now time.Time) (from, until time.Time) {
	c, cerr := time.Parse(time.RFC3339Nano, strings.TrimSpace(created))
	e, eerr := time.Parse(time.RFC3339Nano, strings.TrimSpace(expires))
	if cerr == nil && eerr == nil && e.After(c) && e.Sub(c) <= maxTokenLifetime {
		return c.UTC(), e.UTC()
	}
	return now, now.Add(tokenLifetime)
}

// tokenGroups are the groups a token lists for an account: the scenario's
// for an administrator, and Everyone alone for a read-only account, which is
// in no administrators group.
func (h *handler) tokenGroups(user string) []string {
	if h.role != nil && h.role(user) == "read-only" {
		return []string{"Everyone"}
	}
	return h.licensing.Load().TokenGroups()
}

// stsFault refuses a token request whose user name and password are not an
// account's. A real vCenter's answer to a wrong password was never observed,
// so this is the standard WS-Security fault for it, documented as unverified.
func stsFault(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/xml;charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><soapenv:Envelope xmlns:soapenv="`+nsSOAP+`"><soapenv:Body><soapenv:Fault>`+
		`<faultcode xmlns:wsse="`+nsWSSE+`">wsse:FailedAuthentication</faultcode><faultstring>Invalid credentials</faultstring>`+
		`</soapenv:Fault></soapenv:Body></soapenv:Envelope>`)
}

// assertion is the content of one token.
type assertion struct {
	ID      string
	Issuer  string
	Subject string
	Issued  time.Time
	From    time.Time
	Until   time.Time
	Groups  []string
}

// sign renders the assertion with an enveloped signature: SHA-256 digest of
// the assertion in exclusive canonical form, RSA-SHA256 over the canonical
// SignedInfo.
//
// The assertion is written in canonical form to begin with: namespaces
// declared where they are first used, attributes in order, no white space
// between elements, every empty element with a start and an end tag. What a
// verifier canonicalizes is therefore byte for byte what was digested here,
// and no canonicalizer is needed to produce it. The xsd and xsi prefixes are
// declared on the assertion itself and named in the transform's prefix list,
// because xsd appears only inside attribute values.
func (a assertion) sign(m *signon.Material) (string, error) {
	var head, tail strings.Builder
	at := a.Issued.Format(stsTime)
	head.WriteString(`<saml2:Assertion xmlns:saml2="` + nsSAML + `" xmlns:xsd="` + nsXSD + `" xmlns:xsi="` + nsXSI + `"`)
	head.WriteString(` ID="` + xmlAttr(a.ID) + `" IssueInstant="` + at + `" Version="2.0">`)
	head.WriteString(`<saml2:Issuer Format="urn:oasis:names:tc:SAML:2.0:nameid-format:entity">` + xmlText(a.Issuer) + `</saml2:Issuer>`)

	until := a.Until.Format(stsTime)
	tail.WriteString(`<saml2:Subject><saml2:NameID Format="http://schemas.xmlsoap.org/claims/UPN">` + xmlText(a.Subject) + `</saml2:NameID>`)
	tail.WriteString(`<saml2:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer">`)
	tail.WriteString(`<saml2:SubjectConfirmationData NotOnOrAfter="` + until + `"></saml2:SubjectConfirmationData>`)
	tail.WriteString(`</saml2:SubjectConfirmation></saml2:Subject>`)
	tail.WriteString(`<saml2:Conditions NotBefore="` + a.From.Format(stsTime) + `" NotOnOrAfter="` + until + `">`)
	tail.WriteString(`<saml2:ProxyRestriction Count="0"></saml2:ProxyRestriction></saml2:Conditions>`)
	tail.WriteString(`<saml2:AuthnStatement AuthnInstant="` + at + `"><saml2:AuthnContext>`)
	tail.WriteString(`<saml2:AuthnContextClassRef>urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport</saml2:AuthnContextClassRef>`)
	tail.WriteString(`</saml2:AuthnContext></saml2:AuthnStatement>`)
	tail.WriteString(`<saml2:AttributeStatement>`)
	groups := make([]string, len(a.Groups))
	for i, g := range a.Groups {
		groups[i] = signon.SystemDomain + `\` + g
	}
	given, family := a.Subject, signon.SystemDomain
	if name, domain, ok := strings.Cut(a.Subject, "@"); ok {
		given, family = name, domain
	}
	for _, attr := range []struct {
		friendly, name string
		values         []string
	}{
		{"Groups", "http://rsa.com/schemas/attr-names/2009/01/GroupIdentity", groups},
		{"Subject Type", "http://vmware.com/schemas/attr-names/2011/07/isSolution", []string{"false"}},
		{"Service Account", "http://vmware.com/schemas/attr-names/2025/05/isServiceAccount", []string{"false"}},
		{"givenName", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/givenname", []string{given}},
		{"surname", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/surname", []string{family}},
	} {
		tail.WriteString(`<saml2:Attribute FriendlyName="` + attr.friendly + `" Name="` + attr.name + `" NameFormat="urn:oasis:names:tc:SAML:2.0:attrname-format:uri">`)
		for _, v := range attr.values {
			tail.WriteString(`<saml2:AttributeValue xsi:type="xsd:string">` + xmlText(v) + `</saml2:AttributeValue>`)
		}
		tail.WriteString(`</saml2:Attribute>`)
	}
	tail.WriteString(`</saml2:AttributeStatement></saml2:Assertion>`)

	digest := sha256.Sum256([]byte(head.String() + tail.String()))
	info := `<ds:CanonicalizationMethod Algorithm="` + algExcC14N + `"></ds:CanonicalizationMethod>` +
		`<ds:SignatureMethod Algorithm="` + algRSASHA256 + `"></ds:SignatureMethod>` +
		`<ds:Reference URI="#` + xmlAttr(a.ID) + `"><ds:Transforms>` +
		`<ds:Transform Algorithm="` + algEnveloped + `"></ds:Transform>` +
		`<ds:Transform Algorithm="` + algExcC14N + `"><ec:InclusiveNamespaces xmlns:ec="` + algExcC14N + `" PrefixList="xsd xsi"></ec:InclusiveNamespaces></ds:Transform>` +
		`</ds:Transforms><ds:DigestMethod Algorithm="` + algSHA256 + `"></ds:DigestMethod>` +
		`<ds:DigestValue>` + base64.StdEncoding.EncodeToString(digest[:]) + `</ds:DigestValue></ds:Reference></ds:SignedInfo>`
	// on its own, SignedInfo declares the ds prefix its parent declares in
	// the document
	signedInfo := sha256.Sum256([]byte(`<ds:SignedInfo xmlns:ds="` + nsDS + `">` + info))
	value, err := rsa.SignPKCS1v15(rand.Reader, m.Key, crypto.SHA256, signedInfo[:])
	if err != nil {
		return "", err
	}
	signature := `<ds:Signature xmlns:ds="` + nsDS + `"><ds:SignedInfo>` + info +
		`<ds:SignatureValue>` + base64.StdEncoding.EncodeToString(value) + `</ds:SignatureValue>` +
		`<ds:KeyInfo><ds:X509Data><ds:X509Certificate>` + base64.StdEncoding.EncodeToString(m.Leaf) + `</ds:X509Certificate>` +
		`<ds:X509Certificate>` + base64.StdEncoding.EncodeToString(m.Root) + `</ds:X509Certificate></ds:X509Data></ds:KeyInfo></ds:Signature>`
	return head.String() + signature + tail.String(), nil
}

// xmlText and xmlAttr escape text and attribute values the way canonical XML
// writes them.
var (
	textEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", "&#xD;")
	attrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", `"`, "&quot;", "\t", "&#x9;", "\n", "&#xA;", "\r", "&#xD;")
)

func xmlText(s string) string { return textEscaper.Replace(s) }
func xmlAttr(s string) string { return attrEscaper.Replace(s) }
