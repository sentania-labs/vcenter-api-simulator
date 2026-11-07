// Package catalog holds what the simulator knows about the vSAN Performance
// Service from the sanitized live capture under fixtures/live/9.1.1: the
// advertised entity types, the label set each family actually returns, how
// each family identifies its entities, and which families fault or echo an
// empty result on an ESA cluster.
//
// catalog.json is generated from the fixtures by `go generate ./internal/catalog`
// and embedded here, so the simulator never emits a field or label that the
// live service did not. catalog_test.go fails when the file is stale.
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	vsantypes "github.com/vmware/govmomi/vsan/types"
)

//go:generate go run ./gen -fixtures ../../fixtures/live/9.1.1 -out catalog.json

//go:embed catalog.json
var raw []byte

// Behaviour is what the live service did for a wildcard query of a family.
type Behaviour string

const (
	// Data: entities with series were returned.
	Data Behaviour = "data"
	// Empty: HTTP 200 with the query echoed back and an empty sampleInfo.
	Empty Behaviour = "empty"
	// InvalidArgument: SOAP fault InvalidArgument, invalidProperty entityRefId.
	InvalidArgument Behaviour = "invalid-argument"
	// NotSupported: SOAP fault NotSupported (PMem families).
	NotSupported Behaviour = "not-supported"
)

// Family is one entity type as observed.
type Family struct {
	Behaviour Behaviour `json:"behaviour"`
	// Labels in the order the live service returned them. Empty unless
	// Behaviour is Data.
	Labels []string `json:"labels,omitempty"`
	// RefShapes are the entityRefId shapes seen, with identifiers replaced by
	// their kind: "cluster", "node", "disk", "vm", "vdisk-path", "cpu-N",
	// "VSAN_0xHEX_NAME", "N", "vmnicN", "vmkN", "scsiN:M", "defaultTcpipStack".
	RefShapes []string `json:"refShapes,omitempty"`
	// Entities records the capture's cardinality for documentation and
	// tests; it is not a target the simulator reproduces.
	Entities int `json:"entities"`
	// Stats are the observed value statistics per label, the envelope the
	// simulator's values stay inside (package perf).
	Stats map[string]Stats `json:"stats,omitempty"`
}

// Stats summarise every value the live service returned for one label of
// a family: the five-number summary over all entities and samples, whether
// the service returned decimals, and how many samples were seen.
type Stats struct {
	Min   float64 `json:"min"`
	P25   float64 `json:"p25"`
	P50   float64 `json:"p50"`
	P75   float64 `json:"p75"`
	Max   float64 `json:"max"`
	Float bool    `json:"float,omitempty"`
	N     int     `json:"n"`
}

// About is the vCenter identity from the capture.
type About struct {
	Version      string `json:"version"`
	Build        string `json:"build"`
	ApiVersion   string `json:"apiVersion"`
	FullName     string `json:"fullName"`
	OsType       string `json:"osType"`
	ProductLine  string `json:"productLineId"`
	LicenseName  string `json:"licenseProductName"`
	LicenseVer   string `json:"licenseProductVersion"`
	EsxiVersion  string `json:"esxiVersion"`
	EsxiBuild    string `json:"esxiBuild"`
	EsxiFullName string `json:"esxiFullName"`
}

// EntityType is VsanPerfEntityType as vCenter 9.1.1 returns it. govmomi's
// type stops at graphs; the live service also returns advancedGraphs and
// verboseGraphs on most types, in this element order.
type EntityType struct {
	Name           string                    `xml:"name" json:"name"`
	Id             string                    `xml:"id" json:"id"`
	Graphs         []vsantypes.VsanPerfGraph `xml:"graphs" json:"graphs"`
	Description    string                    `xml:"description,omitempty" json:"description,omitempty"`
	AdvancedGraphs []vsantypes.VsanPerfGraph `xml:"advancedGraphs,omitempty" json:"advancedGraphs,omitempty"`
	VerboseGraphs  []vsantypes.VsanPerfGraph `xml:"verboseGraphs,omitempty" json:"verboseGraphs,omitempty"`
}

// AllGraphs returns graphs, advancedGraphs and verboseGraphs together.
func (e *EntityType) AllGraphs() []vsantypes.VsanPerfGraph {
	out := make([]vsantypes.VsanPerfGraph, 0, len(e.Graphs)+len(e.AdvancedGraphs)+len(e.VerboseGraphs))
	out = append(out, e.Graphs...)
	out = append(out, e.AdvancedGraphs...)
	return append(out, e.VerboseGraphs...)
}

// Catalog is the embedded knowledge.
type Catalog struct {
	Source              string                       `json:"source"`
	About               About                        `json:"about"`
	VimServiceVersions  string                       `json:"vimServiceVersions"`
	VsanServiceVersions string                       `json:"vsanServiceVersions"`
	EntityTypes         []EntityType                 `json:"entityTypes"`
	Families            map[string]Family            `json:"families"`
	MetricsCollectInt   int32                        `json:"metricsCollectInterval"`
	units               map[string]map[string]string // family -> label -> unit
}

var (
	once   sync.Once
	loaded *Catalog
	err    error
)

// Load returns the embedded catalog.
func Load() (*Catalog, error) {
	once.Do(func() {
		var c Catalog
		if e := json.Unmarshal(raw, &c); e != nil {
			err = fmt.Errorf("catalog.json: %w", e)
			return
		}
		c.units = make(map[string]map[string]string, len(c.EntityTypes))
		for _, et := range c.EntityTypes {
			m := c.units[et.Name]
			if m == nil {
				m = make(map[string]string)
				c.units[et.Name] = m
			}
			for _, g := range et.AllGraphs() {
				for _, mid := range g.Metrics {
					if _, ok := m[mid.Label]; !ok {
						m[mid.Label] = g.Unit
					}
				}
			}
		}
		loaded = &c
	})
	return loaded, err
}

// MustLoad is Load for package initialisation and tests.
func MustLoad() *Catalog {
	c, e := Load()
	if e != nil {
		panic(e)
	}
	return c
}

// Unit returns the advertised unit of a label within a family. Labels the
// service returns but does not advertise (the "<x>Actual" and "<x>Raw" twins)
// take the unit of their base label; anything else is "number".
func (c *Catalog) Unit(family, label string) string {
	m := c.units[family]
	if u, ok := m[label]; ok {
		return u
	}
	for _, suffix := range []string{"Actual", "Raw"} {
		if n := len(label) - len(suffix); n > 0 && label[n:] == suffix {
			if u, ok := m[label[:n]]; ok {
				return u
			}
		}
	}
	return "number"
}

// LabelStats returns the observed statistics of a label within a family.
// A twin label ("<x>Actual", "<x>Raw") the service returned but the capture
// did not, falls back to its base label; ok is false when nothing was
// observed.
func (c *Catalog) LabelStats(family, label string) (Stats, bool) {
	f, found := c.Families[family]
	if !found {
		return Stats{}, false
	}
	if s, ok := f.Stats[label]; ok {
		return s, true
	}
	for _, suffix := range []string{"Actual", "Raw"} {
		if n := len(label) - len(suffix); n > 0 && label[n:] == suffix {
			if s, ok := f.Stats[label[:n]]; ok {
				return s, true
			}
		}
	}
	return Stats{}, false
}

// DataFamilies lists the families that return entities, sorted.
func (c *Catalog) DataFamilies() []string {
	var out []string
	for name, f := range c.Families {
		if f.Behaviour == Data {
			out = append(out, name)
		}
	}
	sortStrings(out)
	return out
}
