package sim

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/xml"
)

// The simulator answers these paths itself; every other request keeps the
// behaviour of the govmomi mux and the request capture behind it.
const (
	licensePath       = "/ls/sdk"
	licenseNamespace  = "urn:internalcis.license"
	licenseServiceRef = "cis.license.SessionManagementService"
	licenseServiceMo  = "CisLicenseSessionManagementService"
	lookupPath        = "/lookupservice/sdk"
	caRootPath        = "/rest/vcenter/certificate-authority/get-root"
	caRootAPIPath     = "/api/vcenter/certificate-authority/get-root"
	restSessionHeader = "vmware-api-session-id"
	// licenseCookiePath is the observed cookie scope of a license session.
	licenseCookiePath = "/ls"
	// licenseValueLength is the observed length of a license cookie value.
	licenseValueLength = 36
	// maxLocalSOAPBody bounds the body read for the locally answered services.
	maxLocalSOAPBody = 32 << 20
)

// The wire types mirror the observed elements. LoginResponse and LogoutResponse
// have no children and no text, so they encode as empty elements.
type licenseLoginWire struct {
	XMLName xml.Name `xml:"urn:internalcis.license LoginResponse"`
}

type licenseLogoutWire struct {
	XMLName xml.Name `xml:"urn:internalcis.license LogoutResponse"`
}

// licenseFaultWire is the SOAP 1.1 fault; its detail carries one element, named
// by the fault type it holds.
type licenseFaultWire struct {
	XMLName xml.Name    `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault"`
	Code    string      `xml:"faultcode"`
	String  string      `xml:"faultstring"`
	Detail  detailValue `xml:"detail"`
}

type detailValue struct {
	XMLName xml.Name `xml:"detail"`
	Fault   any      `xml:",any"`
}

// runtimeFaultDetail is the observed invalid-request fault detail: the empty
// RuntimeFaultFault whose type is the unqualified InvalidRequest. The
// bad-credential fault of a real vCenter was never observed, so it is not
// claimed here; every unusable request gets this one shape.
type runtimeFaultDetail struct{}

func (d *runtimeFaultDetail) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Space: licenseNamespace, Local: "RuntimeFaultFault"}
	start.Attr = []xml.Attr{{Name: xml.Name{Local: "xsi:type"}, Value: "InvalidRequest"}}
	return e.EncodeElement("", start)
}

// lookupContent is the observed answer of the lookup service: the four managed
// references, each present once with a string type attribute and a synthetic
// value whose text lengths are 13, 19, 28 and 4.
type lookupContent struct {
	XMLName   xml.Name        `xml:"urn:internallookup RetrieveServiceContentResponse"`
	Returnval lookupReference `xml:"returnval"`
}

// lookupReference is the returnval element. Its own name is bare, as the
// captured answer has it, and each reference child carries its own namespace
// name, so the encoder binds every one of them like the capture does.
type lookupReference struct {
	XMLName        xml.Name  `xml:"returnval"`
	LookupService  lookupRef `xml:"urn:internallookup lookupService"`
	ServiceReg     lookupRef `xml:"urn:internallookup serviceRegistration"`
	DeploymentInfo lookupRef `xml:"urn:internallookup deploymentInformationService"`
	L10n           lookupRef `xml:"urn:internallookup l10n"`
}

// lookupRef is one managed-reference element: a string type attribute and the
// reference text, as the observed references carry them.
type lookupRef struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

// nullFetchResponse is the empty {urn:vim25}FetchResponse for an anonymous
// SessionManager.currentSession read: no returnval and no attributes.
type nullFetchResponse struct {
	XMLName xml.Name `xml:"urn:vim25 FetchResponse"`
}

// licenseSession is one license-service session. Its state is separate from
// the /sdk session even though the cookie name is shared.
type licenseSession struct {
	user string
	last time.Time
}

type licenseSessions struct {
	mu          sync.Mutex
	tokens      map[string]licenseSession
	credentials map[string]string
	check       func(string, string) bool
	timeout     *atomic.Int64
}

func (s *licenseSessions) authorized(user, password string) bool {
	if user == "" || password == "" {
		return false
	}
	if s.check != nil {
		return s.check(user, password)
	}
	if len(s.credentials) == 0 {
		return true
	}
	expected, ok := s.credentials[user]
	return ok && subtle.ConstantTimeCompare([]byte(expected), []byte(password)) == 1
}

func (s *licenseSessions) valid(value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.tokens[value]
	if !ok {
		return false
	}
	if s.timeout != nil {
		if timeout := time.Duration(s.timeout.Load()); timeout > 0 && time.Since(session.last) >= timeout {
			delete(s.tokens, value)
			return false
		}
	}
	session.last = time.Now()
	s.tokens[value] = session
	return true
}

func (s *licenseSessions) put(value, user string) {
	s.mu.Lock()
	s.tokens[value] = licenseSession{user: user, last: time.Now()}
	s.mu.Unlock()
}

func (s *licenseSessions) revoke(affected func(user string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for value, session := range s.tokens {
		if affected(session.user) {
			delete(s.tokens, value)
		}
	}
}

func (s *licenseSessions) end(value string) {
	s.mu.Lock()
	delete(s.tokens, value)
	s.mu.Unlock()
}

// newLicenseValue returns an opaque session value of the observed length.
func newLicenseValue() string {
	var b [licenseValueLength / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// licenseCookie sets the host-only session cookie scoped to /ls: Secure,
// HttpOnly, no Domain and no Expires, as observed.
func licenseCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     soap.SessionCookieName,
		Value:    value,
		Path:     licenseCookiePath,
		Secure:   true,
		HttpOnly: true,
	})
}

// serveLicense answers Login and Logout on /ls/sdk. Every other operation
// keeps today's answer and stays covered by the request capture.
func (s *licenseSessions) serve(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != licensePath && r.URL.Path != "/ls" {
		return false
	}
	if r.Method != http.MethodPost {
		return false
	}
	body, err := readBody(r, maxLocalSOAPBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return true
	}
	switch soapOperation(body) {
	case "Login":
		value, user := s.login(body)
		if value == "" {
			licenseFault(w)
			return true
		}
		s.put(value, user)
		licenseCookie(w, value)
		encodeSOAP(w, http.StatusOK, licenseLoginWire{})
	case "Logout":
		value, cerr := r.Cookie(soap.SessionCookieName)
		if cerr != nil || !s.valid(value.Value) {
			licenseFault(w)
			return true
		}
		// the observed Logout reissues the same cookie rather than expiring it,
		// and ends the session server-side
		s.end(value.Value)
		licenseCookie(w, value.Value)
		encodeSOAP(w, http.StatusOK, licenseLogoutWire{})
	default:
		rewind(r, body)
		return false
	}
	return true
}

// login checks the observed lowercase request shape and the simulator's own
// credentials, and returns the new session value with its user. The optional
// SOAP Header, whose unqualified operationID is a tracing string rather than
// authentication, is accepted either way.
func (s *licenseSessions) login(body []byte) (string, string) {
	req := decodeSOAPRequest(body)
	if req.this != licenseServiceRef || req.targetType != licenseServiceMo || len(req.texts) != 2 {
		return "", ""
	}
	if !s.authorized(req.texts[0], req.texts[1]) {
		return "", ""
	}
	return newLicenseValue(), req.texts[0]
}

// licenseFault answers an unusable license-service request. The bad-credential
// fault of a real vCenter was not observed, so the fault keeps the observed
// invalid-request shape and is documented as unverified.
func licenseFault(w http.ResponseWriter) {
	encodeSOAP(w, http.StatusInternalServerError, &licenseFaultWire{
		Code:   "ServerFaultCode",
		String: "Invalid request",
		Detail: detailValue{Fault: new(runtimeFaultDetail)},
	})
}

// serveLookup answers RetrieveServiceContent on /lookupservice/sdk
// anonymously; other lookup operations keep today's answer and are captured.
func serveLookup(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != lookupPath && r.URL.Path != "/lookupservice" {
		return false
	}
	if r.Method != http.MethodPost {
		return false
	}
	body, err := readBody(r, maxLocalSOAPBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return true
	}
	if soapOperation(body) != "RetrieveServiceContent" {
		rewind(r, body)
		return false
	}
	encodeSOAP(w, http.StatusOK, lookupReply())
	return true
}

func lookupReply() lookupContent {
	return lookupContent{Returnval: lookupReference{
		LookupService:  lookupRef{Type: "LookupLookupService", Value: "lookupService"},
		ServiceReg:     lookupRef{Type: "LookupServiceRegistration", Value: "serviceRegistration"},
		DeploymentInfo: lookupRef{Type: "LookupDeploymentInformationService", Value: "deploymentInformationService"},
		L10n:           lookupRef{Type: "LookupL10n", Value: "l10n"},
	}}
}

// caRoot answers the certificate-authority root read with the certificate the
// simulator itself serves. The REST session header works on both prefixes, and
// the numbered 1.0 and 2.0 forms answer the same value.
func (h *handler) caRoot(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if !caRootPathMatches(path) {
		return false
	}
	if r.Method != http.MethodGet {
		return false
	}
	if !h.rest.valid(r.Header.Get(restSessionHeader)) {
		w.WriteHeader(http.StatusUnauthorized)
		return true
	}
	text := h.caPEM()
	if text == "" {
		w.WriteHeader(http.StatusNotImplemented)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"value":%q}`, text)
	return true
}

// caRootPathMatches reports one of the answered certificate-authority forms:
// the base form of either prefix and its numbered 1.0 and 2.0 spellings.
func caRootPathMatches(path string) bool {
	switch path {
	case caRootPath, caRootAPIPath,
		"/rest/1.0/vcenter/certificate-authority/get-root",
		"/rest/2.0/vcenter/certificate-authority/get-root",
		"/api/1.0/vcenter/certificate-authority/get-root",
		"/api/2.0/vcenter/certificate-authority/get-root":
		return true
	}
	return false
}

// currentSessionNull is the successful empty answer for an anonymous
// SessionManager.currentSession read, applied on every property path.
func (h *handler) currentSessionNull(w http.ResponseWriter, r *http.Request, body []byte) bool {
	if !strings.HasPrefix(r.URL.Path, vim25.Path) || soapOperation(body) != "Fetch" {
		return false
	}
	if !anonymousCurrentSession(h, r, body) {
		return false
	}
	encodeSOAP(w, http.StatusOK, nullFetchResponse{})
	return true
}

// anonymousCurrentSession reports a Fetch of SessionManager.currentSession that
// no session authenticates; a signed-in caller keeps the UserSession answer.
func anonymousCurrentSession(h *handler, r *http.Request, body []byte) bool {
	req := decodeSOAPRequest(body)
	if req.targetType != "SessionManager" || len(req.texts) != 1 || req.texts[0] != "currentSession" {
		return false
	}
	cookie, err := r.Cookie(soap.SessionCookieName)
	return err != nil || cookie.Value == "" || !h.liveSOAPSession(cookie.Value)
}

// liveSOAPSession reports whether this key is a session the store still lists.
// A terminated session is removed from that list, which is what makes a
// logged-out read anonymous. The probe view is used the way
// expireSOAPSession uses it, so the per-session session list is what is read.
func (h *handler) liveSOAPSession(key string) bool {
	sm := h.registry.SessionManager()
	probe := &simulator.Session{Map: h.registry, Registry: simulator.NewRegistry()}
	view := probe.Get(sm.Reference()).(*simulator.SessionManager)
	for _, session := range view.SessionList {
		if session.Key == key {
			return true
		}
	}
	return false
}

// soapElementTexts projects a SOAP body's first operation to its _this reference
// and type and the text of its value elements in order. The operation is the
// Body's first child, so a tracing Header element such as an unqualified
// operationID is never taken for one.
type soapRequest struct {
	operation  string
	this       string
	targetType string
	texts      []string
}

func decodeSOAPRequest(body []byte) soapRequest {
	var req soapRequest
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var (
		pending string
		inBody  bool
		inOp    bool
		depth   int
		opDepth int
	)
	for {
		tok, err := decoder.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case !inBody:
				if t.Name.Local == "Body" {
					inBody = true
				}
				continue
			case !inOp:
				if isSOAPWrapper(t.Name.Local) {
					continue
				}
				inOp = true
				opDepth = depth
			}
			pending = ""
			switch t.Name.Local {
			case "_this":
				for _, a := range t.Attr {
					if a.Name.Local == "type" {
						req.targetType = a.Value
					}
				}
				pending = "_this"
			case "prop", "username", "password":
				pending = t.Name.Local
			}
		case xml.CharData:
			if !inOp || pending == "" {
				continue
			}
			text := strings.TrimSpace(string(t))
			switch pending {
			case "_this":
				req.this = text
			default:
				if text != "" {
					req.texts = append(req.texts, text)
				}
			}
		case xml.EndElement:
			pending = ""
			switch {
			case inOp && depth == opDepth:
				if req.operation == "" {
					req.operation = t.Name.Local
				}
				inOp = false
			}
			depth--
		}
	}
	return req
}

// isSOAPWrapper names the elements that are not an operation.
func isSOAPWrapper(local string) bool {
	switch local {
	case "Envelope", "Header", "Body", "Fault", "detail":
		return true
	}
	return false
}

// readBody reads at most limit bytes of a request body.
func readBody(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		return nil, err
	}
	rewind(r, data)
	return data, nil
}

func rewind(r *http.Request, body []byte) {
	if r.Body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
}

// soapEnvelope is the SOAP 1.1 envelope the observed answers carry. The four
// namespace declarations are exactly those of the captured responses; prefixes
// must be declared as attributes, because the encoder otherwise renames a
// prefixed element to its namespace's default.
type soapEnvelope struct {
	XMLName xml.Name      `xml:"soapenv:Envelope"`
	Enc     string        `xml:"xmlns:soapenc,attr"`
	Env     string        `xml:"xmlns:soapenv,attr"`
	XSD     string        `xml:"xmlns:xsd,attr"`
	XSI     string        `xml:"xmlns:xsi,attr"`
	Body    soapBodyValue `xml:"soapenv:Body"`
}

// soapBodyValue is the Body element, whose one child is the answered operation.
// Naming the element in this field's own XMLName is what keeps the soapenv
// prefix: the encoder takes a nested element's name from that field.
type soapBodyValue struct {
	XMLName xml.Name `xml:"soapenv:Body"`
	Child   any      `xml:",any"`
}

// encodeSOAP writes a SOAP 1.1 envelope with the observed namespace bindings.
func encodeSOAP(w http.ResponseWriter, status int, body any) {
	var out bytes.Buffer
	_, _ = fmt.Fprint(&out, xml.Header)
	enc := xml.NewEncoder(&out)
	err := enc.Encode(&soapEnvelope{
		Enc:  "http://schemas.xmlsoap.org/soap/encoding/",
		Env:  "http://schemas.xmlsoap.org/soap/envelope/",
		XSD:  "http://www.w3.org/2001/XMLSchema",
		XSI:  "http://www.w3.org/2001/XMLSchema-instance",
		Body: soapBodyValue{Child: body},
	})
	if err == nil {
		err = enc.Flush()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/xml;charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(out.Bytes())
}

// caPEM is the PEM of the certificate the simulator itself serves.
func (h *handler) caPEM() string {
	if h.tls == nil {
		return ""
	}
	var cert *tls.Certificate
	if current, err := h.tls.GetCertificate(nil); err == nil {
		cert = current
	}
	if cert == nil || cert.Leaf == nil || len(cert.Leaf.Raw) == 0 {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Leaf.Raw}))
}
