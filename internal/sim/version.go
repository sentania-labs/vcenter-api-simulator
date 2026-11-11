package sim

import (
	"encoding/xml"
	"strings"
)

// selectVimVersions retains the selected known namespace and older namespaces
// from the captured ordered document. A custom API version has no known
// predecessor list, so only that explicitly provided version is advertised.
func selectVimVersions(raw, version string) string {
	type versions struct {
		Values []string `xml:"version"`
	}
	type namespace struct {
		Name    string   `xml:"name"`
		Version string   `xml:"version"`
		Prior   versions `xml:"priorVersions"`
	}
	type doc struct {
		XMLName   xml.Name  `xml:"namespaces"`
		Version   string    `xml:"version,attr"`
		Namespace namespace `xml:"namespace"`
	}
	var d doc
	if xml.Unmarshal([]byte(raw), &d) != nil {
		return raw
	}
	if d.Namespace.Version == version {
		return raw
	}
	all := append([]string{d.Namespace.Version}, d.Namespace.Prior.Values...)
	var prior []string
	for i, v := range all {
		if v == version {
			for _, older := range all[i+1:] {
				if !strings.HasPrefix(older, "m") {
					prior = append(prior, older)
				}
			}
			break
		}
	}
	d.Namespace.Version = version
	d.Namespace.Prior.Values = prior
	b, err := xml.Marshal(d)
	if err != nil {
		return raw
	}
	return xml.Header + string(b)
}

// selectVsanVersions advertises the newest captured vSAN namespace that the
// selected vCenter API version can speak: the same version when the capture
// lists it, otherwise the newest entry of the same major release.
func selectVsanVersions(raw, apiVersion string) string {
	var d struct {
		Namespace struct {
			Version string `xml:"version"`
			Prior   struct {
				Values []string `xml:"version"`
			} `xml:"priorVersions"`
		} `xml:"namespace"`
	}
	if xml.Unmarshal([]byte(raw), &d) != nil {
		return raw
	}
	all := append([]string{d.Namespace.Version}, d.Namespace.Prior.Values...)
	major, _, _ := strings.Cut(apiVersion, ".")
	top := ""
	for _, v := range all {
		if v == apiVersion {
			top = v
			break
		}
		if top == "" && strings.HasPrefix(v, major+".") {
			top = v
		}
	}
	if top == "" {
		return raw
	}
	return selectVimVersions(raw, top)
}
