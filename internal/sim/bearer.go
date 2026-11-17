package sim

import (
	"crypto/subtle"
	"encoding/xml"
	"github.com/sentania/vcenter-api-simulator/internal/signon"
	"reflect"
	"sort"
	"strings"
	"time"
)

// bearerUser accepts the simulator's own bearer assertions. Re-rendering the
// signed fields lets the Java client change XML formatting without changing
// what was signed. Neither certificates supplied by a caller nor real tokens
// are trusted. The assertion's subject and lifetime must still be valid.
type issuedBearer struct {
	user, subject string
	until         time.Time
}

func (s *restSessions) rememberBearer(id, user, subject string, until, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.issued == nil {
		s.issued = map[string]issuedBearer{}
	}
	for key, bearer := range s.issued {
		if !now.Before(bearer.until) {
			delete(s.issued, key)
		}
	}
	s.issued[id] = issuedBearer{user: user, subject: subject, until: until}
}
func (h *handler) bearerUser(raw string) (string, bool) {
	var a struct {
		XMLName xml.Name `xml:"urn:oasis:names:tc:SAML:2.0:assertion Assertion"`
		ID      string   `xml:"ID,attr"`
		Issued  string   `xml:"IssueInstant,attr"`
		Issuer  string   `xml:"Issuer"`
		Subject struct {
			Name string `xml:"NameID"`
		} `xml:"Subject"`
		Conditions struct {
			From  string `xml:"NotBefore,attr"`
			Until string `xml:"NotOnOrAfter,attr"`
		} `xml:"Conditions"`
		Signature struct {
			Value string `xml:"SignatureValue"`
		} `xml:"Signature"`
		Attributes struct {
			Items []struct {
				Name   string   `xml:"Name,attr"`
				Values []string `xml:"AttributeValue"`
			} `xml:"Attribute"`
		} `xml:"AttributeStatement"`
	}
	if xml.Unmarshal([]byte(raw), &a) != nil || a.ID == "" || a.Subject.Name == "" {
		return "", false
	}
	issued, e1 := time.Parse(time.RFC3339Nano, a.Issued)
	from, e2 := time.Parse(time.RFC3339Nano, a.Conditions.From)
	until, e3 := time.Parse(time.RFC3339Nano, a.Conditions.Until)
	now := h.env.Now().UTC()
	if e1 != nil || e2 != nil || e3 != nil || now.Before(from) || !now.Before(until) {
		return "", false
	}
	var groups []string
	for _, attr := range a.Attributes.Items {
		if attr.Name == "http://rsa.com/schemas/attr-names/2009/01/GroupIdentity" {
			for _, value := range attr.Values {
				domain, group, ok := strings.Cut(value, `\`)
				if !ok || domain != signon.SystemDomain {
					return "", false
				}
				groups = append(groups, group)
			}
		}
	}
	material, err := h.issuer.Material()
	if err != nil {
		return "", false
	}
	signed, err := (assertion{ID: a.ID, Issuer: a.Issuer, Subject: a.Subject.Name, Issued: issued, From: from, Until: until, Groups: groups}).sign(material)
	if err != nil {
		return "", false
	}
	var expected struct {
		Signature struct {
			Value string `xml:"SignatureValue"`
		} `xml:"Signature"`
	}
	if !sameAssertion(raw, signed) || xml.Unmarshal([]byte(signed), &expected) != nil || subtle.ConstantTimeCompare([]byte(expected.Signature.Value), []byte(a.Signature.Value)) != 1 {
		return "", false
	}
	h.rest.mu.Lock()
	recorded, ok := h.rest.issued[a.ID]
	h.rest.mu.Unlock()
	if !ok || recorded.subject != a.Subject.Name || !now.Before(recorded.until) {
		return "", false
	}
	return recorded.user, true
}

// Compare every assertion field as well, so an unrecognized attribute,
// certificate, or signed statement cannot be changed while retaining an
// issued signature. Namespace placement and formatting may differ in Java.
type assertionElement struct {
	Name     xml.Name
	Attrs    []xml.Attr         `xml:",any,attr"`
	Text     string             `xml:",chardata"`
	Children []assertionElement `xml:",any"`
}

func (e *assertionElement) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	type element assertionElement
	var v element
	if err := d.DecodeElement(&v, &start); err != nil {
		return err
	}
	*e = assertionElement(v)
	e.Name = start.Name
	attrs := e.Attrs[:0]
	for _, a := range e.Attrs {
		if a.Name.Space != "xmlns" && !(a.Name.Space == "" && a.Name.Local == "xmlns") {
			attrs = append(attrs, a)
		}
	}
	e.Attrs = attrs
	if len(e.Attrs) == 0 {
		e.Attrs = nil
	}
	sort.Slice(e.Attrs, func(i, j int) bool {
		a, b := e.Attrs[i].Name, e.Attrs[j].Name
		if a.Space != b.Space {
			return a.Space < b.Space
		}
		return a.Local < b.Local
	})
	if strings.TrimSpace(e.Text) == "" {
		e.Text = ""
	}
	return nil
}
func sameAssertion(a, b string) bool {
	var first, second assertionElement
	return xml.Unmarshal([]byte(a), &first) == nil && xml.Unmarshal([]byte(b), &second) == nil && reflect.DeepEqual(first, second)
}
