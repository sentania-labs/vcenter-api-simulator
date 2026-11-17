package sim_test

import (
	"bytes"
	"encoding/xml"
	"sort"
	"strings"
	"testing"
)

// canonical renders one element of a document in exclusive canonical form
// (xml-exc-c14n), reading the document as a verifier would: from its bytes,
// not from how the simulator put them together. inclusive lists the
// prefixes an InclusiveNamespaces PrefixList names; skip leaves a subtree
// out, which is how the enveloped-signature transform removes the signature.
//
// It handles what a SOAP answer holds: elements, attributes, namespaces and
// text. It is the test's own implementation, so a signature that verifies
// here was not made to match the code that signed it.
func canonical(t *testing.T, doc []byte, local string, inclusive []string, skip func(xml.StartElement) bool) []byte {
	t.Helper()
	d := xml.NewDecoder(bytes.NewReader(doc))
	var out bytes.Buffer
	scope := []map[string]string{{}}    // namespaces in scope, by element depth
	rendered := []map[string]string{{}} // namespaces already written, by output depth
	inside, skipping := 0, 0
	for {
		tok, err := d.RawToken()
		if err != nil {
			break
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			ns := map[string]string{}
			for k, v := range scope[len(scope)-1] {
				ns[k] = v
			}
			var attrs []xml.Attr
			for _, a := range tk.Attr {
				switch {
				case a.Name.Space == "xmlns":
					ns[a.Name.Local] = a.Value
				case a.Name.Space == "" && a.Name.Local == "xmlns":
					ns[""] = a.Value
				default:
					attrs = append(attrs, a)
				}
			}
			scope = append(scope, ns)
			if skipping > 0 {
				skipping++
				continue
			}
			if inside == 0 && tk.Name.Local != local {
				continue
			}
			if inside > 0 && skip != nil && skip(tk) {
				skipping = 1
				continue
			}
			inside++
			used := map[string]bool{tk.Name.Space: true}
			for _, a := range attrs {
				if a.Name.Space != "" {
					used[a.Name.Space] = true
				}
			}
			for _, p := range inclusive {
				if _, ok := ns[p]; ok {
					used[p] = true
				}
			}
			var prefixes []string
			for p := range used {
				prefixes = append(prefixes, p)
			}
			sort.Strings(prefixes)
			seen := map[string]string{}
			for k, v := range rendered[len(rendered)-1] {
				seen[k] = v
			}
			out.WriteString("<" + qname(tk.Name))
			for _, p := range prefixes {
				uri, declared := ns[p]
				if !declared && p == "" {
					continue
				}
				if !declared {
					t.Fatalf("prefix %q is used and not declared", p)
				}
				if have, ok := seen[p]; ok && have == uri {
					continue
				}
				seen[p] = uri
				if p == "" {
					out.WriteString(` xmlns="` + c14nAttr(uri) + `"`)
				} else {
					out.WriteString(` xmlns:` + p + `="` + c14nAttr(uri) + `"`)
				}
			}
			rendered = append(rendered, seen)
			sort.SliceStable(attrs, func(i, j int) bool {
				ui, uj := ns[attrs[i].Name.Space], ns[attrs[j].Name.Space]
				if attrs[i].Name.Space == "" {
					ui = ""
				}
				if attrs[j].Name.Space == "" {
					uj = ""
				}
				if ui != uj {
					return ui < uj
				}
				return attrs[i].Name.Local < attrs[j].Name.Local
			})
			for _, a := range attrs {
				out.WriteString(" " + qname(a.Name) + `="` + c14nAttr(a.Value) + `"`)
			}
			out.WriteString(">")
		case xml.EndElement:
			scope = scope[:len(scope)-1]
			if skipping > 0 {
				skipping--
				continue
			}
			if inside == 0 {
				continue
			}
			out.WriteString("</" + qname(tk.Name) + ">")
			rendered = rendered[:len(rendered)-1]
			if inside--; inside == 0 {
				return out.Bytes()
			}
		case xml.CharData:
			if inside > 0 && skipping == 0 {
				out.WriteString(c14nText(string(tk)))
			}
		}
	}
	t.Fatalf("element %s not found, or the document ended inside it", local)
	return nil
}

func qname(n xml.Name) string {
	if n.Space == "" {
		return n.Local
	}
	return n.Space + ":" + n.Local
}

func c14nText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", "&#xD;").Replace(s)
}

func c14nAttr(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", `"`, "&quot;", "\t", "&#x9;", "\n", "&#xA;", "\r", "&#xD;").Replace(s)
}

// The canonicalizer is checked against a document whose canonical form is
// written out by hand from the specification's rules: declarations pushed
// down to where a prefix is first used, attributes in order, an empty
// element opened and closed, and an unused declaration dropped.
func TestCanonicalForm(t *testing.T) {
	doc := `<r xmlns:a="urn:a" xmlns:b="urn:b" xmlns:unused="urn:u"><a:e  z="1" b:y="2" a:x="3"><b:f/>t &amp; &lt;u&gt;</a:e></r>`
	want := `<a:e xmlns:a="urn:a" xmlns:b="urn:b" z="1" a:x="3" b:y="2"><b:f></b:f>t &amp; &lt;u&gt;</a:e>`
	if got := string(canonical(t, []byte(doc), "e", nil, nil)); got != want {
		t.Errorf("canonical form\n got %s\nwant %s", got, want)
	}
	// a prefix named as inclusive is declared at the top even though no
	// element or attribute name uses it
	doc = `<r xmlns:a="urn:a" xmlns:xsd="urn:x"><a:e v="xsd:string"><a:f/></a:e></r>`
	want = `<a:e xmlns:a="urn:a" xmlns:xsd="urn:x" v="xsd:string"><a:f></a:f></a:e>`
	if got := string(canonical(t, []byte(doc), "e", []string{"xsd"}, nil)); got != want {
		t.Errorf("inclusive prefix\n got %s\nwant %s", got, want)
	}
}
