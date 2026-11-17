package sim

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/xml"

	"github.com/sentania/vcenter-api-simulator/internal/signon"
)

// The lookup service is where a tool asks a vCenter for the addresses of its
// other services. The simulator registers two: itself (vcenterserver) and its
// sign-on service (cs.identity, with an admin and a token endpoint).
const (
	lookupNamespace = "urn:internallookup"
	// lookupRegistrationRef is the reference a real vCenter hands out for
	// its registration service, and the only one List accepts.
	lookupRegistrationRef = "ServiceRegistration"

	ssoAdminPath = "/sso-adminserver/sdk/" + signon.SystemDomain
	stsPath      = "/sts/STSService/" + signon.SystemDomain

	// lookupSiteID is the simulator's own site name. A real vCenter's was
	// seen only as 18 characters.
	lookupSiteID = "Default-First-Site"
)

// lookupFilter is the four values of a List filter.
type lookupFilter struct{ product, service, protocol, endpoint string }

// The three filters the simulator answers. Anything else matches nothing.
var (
	filterVCenter = lookupFilter{"com.vmware.cis", "vcenterserver", "vmomi", "com.vmware.vim"}
	filterSSO     = lookupFilter{"com.vmware.cis", "cs.identity", "vmomi", "com.vmware.cis.cs.identity.admin"}
	filterSTS     = lookupFilter{"com.vmware.cis", "cs.identity", "wsTrust", "com.vmware.cis.cs.identity.sso"}
)

type lookupListRequest struct {
	This   thisRef `xml:"_this"`
	Filter *struct {
		ServiceType *struct {
			Product string `xml:"product"`
			Type    string `xml:"type"`
		} `xml:"serviceType"`
		EndpointType *struct {
			Protocol string `xml:"protocol"`
			Type     string `xml:"type"`
		} `xml:"endpointType"`
	} `xml:"filterCriteria"`
}

// thisRef is a request's managed object reference.
type thisRef struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

// The List answer, with the elements in the order a real vCenter sends them.
// The vendor and default-name elements are present and empty there, so they
// are never omitted here.
type lookupListResponse struct {
	XMLName   xml.Name             `xml:"urn:internallookup ListResponse"`
	Returnval []lookupRegistration `xml:"returnval"`
}

type lookupRegistration struct {
	ServiceVersion                string            `xml:"serviceVersion"`
	VendorNameResourceKey         string            `xml:"vendorNameResourceKey"`
	VendorNameDefault             string            `xml:"vendorNameDefault"`
	VendorProductInfoResourceKey  string            `xml:"vendorProductInfoResourceKey"`
	VendorProductInfoDefault      string            `xml:"vendorProductInfoDefault"`
	ServiceEndpoints              []lookupEndpoint  `xml:"serviceEndpoints"`
	ServiceAttributes             []lookupAttribute `xml:"serviceAttributes"`
	ServiceNameResourceKey        string            `xml:"serviceNameResourceKey"`
	ServiceNameDefault            string            `xml:"serviceNameDefault"`
	ServiceDescriptionResourceKey string            `xml:"serviceDescriptionResourceKey"`
	ServiceDescriptionDefault     string            `xml:"serviceDescriptionDefault"`
	OwnerID                       string            `xml:"ownerId"`
	ServiceType                   lookupServiceType `xml:"serviceType"`
	NodeID                        string            `xml:"nodeId"`
	ServiceID                     string            `xml:"serviceId"`
	SiteID                        string            `xml:"siteId"`
}

type lookupEndpoint struct {
	URL                string             `xml:"url"`
	EndpointType       lookupEndpointType `xml:"endpointType"`
	SSLTrust           []string           `xml:"sslTrust"`
	EndpointAttributes []lookupAttribute  `xml:"endpointAttributes"`
}

type lookupEndpointType struct {
	Protocol string `xml:"protocol"`
	Type     string `xml:"type"`
}

type lookupServiceType struct {
	Product string `xml:"product"`
	Type    string `xml:"type"`
}

type lookupAttribute struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

// lookupFaultDetail is the detail of the fault a real vCenter answers a
// wrong reference with.
type lookupFaultDetail struct{}

func (d *lookupFaultDetail) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Space: lookupNamespace, Local: "RuntimeFaultFault"}
	start.Attr = []xml.Attr{{Name: xml.Name{Local: "xsi:type"}, Value: "ManagedObjectNotFound"}}
	return e.EncodeElement("", start)
}

// serveLookup answers RetrieveServiceContent and List on /lookupservice/sdk,
// both without a sign-in; other lookup operations stay unanswered and are
// captured.
func (h *handler) serveLookup(w http.ResponseWriter, r *http.Request) bool {
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
	switch soapOperation(body) {
	case "RetrieveServiceContent":
		anonymousCookie(w)
		encodeSOAP(w, http.StatusOK, lookupReply())
	case "List":
		var req lookupListRequest
		if err := decodeOperation(body, &req); err != nil {
			rewind(r, body)
			return false
		}
		anonymousCookie(w)
		if strings.TrimSpace(req.This.Value) != lookupRegistrationRef {
			// the fault type is a real vCenter's; its text was not kept, so
			// the usual wording for this fault stands in for it
			encodeSOAP(w, http.StatusInternalServerError, &licenseFaultWire{
				Code:   "ServerFaultCode",
				String: "The object has already been deleted or has not been completely created",
				Detail: detailValue{Fault: new(lookupFaultDetail)},
			})
			return true
		}
		encodeSOAP(w, http.StatusOK, lookupListResponse{Returnval: h.lookupList(r, req)})
	default:
		rewind(r, body)
		return false
	}
	return true
}

func lookupReply() lookupContent {
	return lookupContent{Returnval: lookupReference{
		LookupService:  lookupRef{Type: "LookupLookupService", Value: "lookupService"},
		ServiceReg:     lookupRef{Type: "LookupServiceRegistration", Value: lookupRegistrationRef},
		DeploymentInfo: lookupRef{Type: "LookupDeploymentInformationService", Value: "deploymentInformationService"},
		L10n:           lookupRef{Type: "LookupL10n", Value: "l10n"},
	}}
}

// lookupList is the registrations a filter selects: the one registration of
// the three filters the simulator knows, and none for any other filter.
func (h *handler) lookupList(r *http.Request, req lookupListRequest) []lookupRegistration {
	if req.Filter == nil || req.Filter.ServiceType == nil || req.Filter.EndpointType == nil {
		return nil
	}
	f := lookupFilter{
		product:  strings.TrimSpace(req.Filter.ServiceType.Product),
		service:  strings.TrimSpace(req.Filter.ServiceType.Type),
		protocol: strings.TrimSpace(req.Filter.EndpointType.Protocol),
		endpoint: strings.TrimSpace(req.Filter.EndpointType.Type),
	}
	host := clientHost(r)
	name, _ := splitHost(host)
	trust := h.sslTrust()
	topo := h.sim.Topology
	switch f {
	case filterVCenter:
		return []lookupRegistration{{
			ServiceVersion: topo.VCenterVersion.LicenseVersion(),
			ServiceEndpoints: []lookupEndpoint{{
				URL:          "https://" + withPort(host, "443") + "/sdk",
				EndpointType: lookupEndpointType{Protocol: f.protocol, Type: f.endpoint},
				SSLTrust:     trust,
				EndpointAttributes: []lookupAttribute{
					{Key: "cis.common.ep.localurl", Value: "http://" + net.JoinHostPort(name, "8085") + "/sdk"},
				},
			}},
			// The first two values and the site id were seen on a real
			// vCenter only as lengths; these are the simulator's own, of
			// those lengths. No tool the simulator was built for reads them.
			ServiceAttributes: []lookupAttribute{
				{Key: "com.vmware.cis.cm.GroupInternalId", Value: "com.vmware.vim.vcenter"},
				{Key: "com.vmware.cis.cm.ControlScript", Value: "service-control-default-vmon"},
				{Key: "com.vmware.vim.vcenter.instanceName", Value: name},
				{Key: "com.vmware.cis.cm.HostId", Value: topo.HostID},
			},
			ServiceNameResourceKey:        "AboutInfo.vpx.name",
			ServiceDescriptionResourceKey: "AboutInfo.vpx.name",
			OwnerID:                       "vpxd-svc-acct-" + topo.HostID + "@" + signon.SystemDomain,
			ServiceType:                   lookupServiceType{Product: f.product, Type: f.service},
			NodeID:                        topo.NodeID,
			ServiceID:                     topo.InstanceUUID,
			SiteID:                        lookupSiteID,
		}}
	case filterSSO, filterSTS:
		url := "https://" + host + ssoAdminPath
		if f == filterSTS {
			url = "https://" + host + stsPath
		}
		return []lookupRegistration{{
			ServiceVersion: "2.0",
			ServiceEndpoints: []lookupEndpoint{{
				URL:          url,
				EndpointType: lookupEndpointType{Protocol: f.protocol, Type: f.endpoint},
				SSLTrust:     trust,
			}},
			ServiceAttributes: []lookupAttribute{{Key: "Capabilities", Value: "MultiSite"}},
			OwnerID:           "vcsim@" + signon.SystemDomain,
			ServiceType:       lookupServiceType{Product: f.product, Type: f.service},
			NodeID:            topo.NodeID,
			ServiceID:         topo.SignOnID,
			SiteID:            lookupSiteID,
		}}
	}
	return nil
}

// clientHost is the address the client used to reach the simulator: the
// host a proxy in front of it says the client asked for, or else the
// request's own Host. Every address the simulator hands back is built on it,
// so a tool is sent back to where it came from.
func clientHost(r *http.Request) string {
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		if first := strings.TrimSpace(strings.Split(fwd, ",")[0]); validHost(first) {
			host = first
		}
	}
	if !validHost(host) {
		return "localhost"
	}
	return host
}

// validHost accepts a host name or address with an optional port and
// nothing that could change the meaning of a URL built from it.
func validHost(host string) bool {
	if host == "" || len(host) > 261 {
		return false
	}
	for _, c := range host {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == ':', c == '[', c == ']', c == '_':
		default:
			return false
		}
	}
	return true
}

// splitHost separates a host from its port; port is empty when there is none.
func splitHost(host string) (name, port string) {
	if n, p, err := net.SplitHostPort(host); err == nil {
		return n, p
	}
	return strings.Trim(host, "[]"), ""
}

// withPort adds a port to a host that names none.
func withPort(host, port string) string {
	name, have := splitHost(host)
	if have != "" {
		return host
	}
	return net.JoinHostPort(name, port)
}

// anonymousCookie sets the session cookie a real vCenter sets on every
// answer of its lookup and sign-on admin services: 36 characters, Secure and
// HttpOnly, with no Path. Nothing is signed in by it.
func anonymousCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: soap.SessionCookieName, Value: newLicenseValue(), Secure: true, HttpOnly: true})
}

// decodeOperation decodes a SOAP body's operation element into v.
func decodeOperation(body []byte, v any) error {
	d := xml.NewDecoder(bytes.NewReader(body))
	inBody := false
	for {
		tok, err := d.Token()
		if err != nil {
			return errors.New("no operation in the SOAP body")
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !inBody {
			inBody = start.Name.Local == "Body"
			continue
		}
		return d.DecodeElement(v, &start)
	}
}

// TrustSource is the certificate a tool sees at the simulator's address when
// that is not the simulator's own: the certificate of a proxy that ends TLS
// in front of it. The file is read again when it changes, so a renewed
// certificate is picked up without a restart.
type TrustSource struct {
	file string

	mu      sync.Mutex
	mod     time.Time
	checked time.Time
	certs   []string
}

// NewTrustSource reads a PEM file of one or more certificates, the one the
// client sees first. It fails when the file holds no certificate.
func NewTrustSource(file string) (*TrustSource, error) {
	t := &TrustSource{file: file}
	certs, mod, err := t.read()
	if err != nil {
		return nil, err
	}
	t.certs, t.mod, t.checked = certs, mod, time.Now()
	return t, nil
}

func (t *TrustSource) read() ([]string, time.Time, error) {
	info, err := os.Stat(t.file)
	if err != nil {
		return nil, time.Time{}, err
	}
	data, err := os.ReadFile(t.file)
	if err != nil {
		return nil, time.Time{}, err
	}
	var out []string
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return nil, time.Time{}, fmt.Errorf("%s: %w", t.file, err)
		}
		out = append(out, base64.StdEncoding.EncodeToString(block.Bytes))
	}
	if len(out) == 0 {
		return nil, time.Time{}, fmt.Errorf("%s holds no certificate", t.file)
	}
	return out, info.ModTime(), nil
}

// current returns the certificates, re-reading the file at most once a
// second and only when it changed. A file that no longer reads keeps the
// last good answer.
func (t *TrustSource) current() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if now := time.Now(); now.Sub(t.checked) >= time.Second {
		t.checked = now
		if info, err := os.Stat(t.file); err == nil && !info.ModTime().Equal(t.mod) {
			if certs, mod, err := t.read(); err == nil {
				t.certs, t.mod = certs, mod
			} else {
				t.mod = info.ModTime()
			}
		}
	}
	return t.certs
}

// sslTrust is the certificate a tool must trust to call the addresses the
// lookup service hands out: the operator's, when one was supplied, or else
// the one the simulator's own listener serves.
func (h *handler) sslTrust() []string {
	if h.trust != nil {
		return h.trust.current()
	}
	if h.tls == nil {
		return nil
	}
	cert, err := h.tls.GetCertificate(nil)
	if err != nil || cert == nil || len(cert.Certificate) == 0 {
		return nil
	}
	return []string{base64.StdEncoding.EncodeToString(cert.Certificate[0])}
}
