// Command gen builds catalog.json from the sanitized fixture set.
//
// It decodes the captured SOAP responses with govmomi's own XML decoder and
// types, so the embedded entity types are exactly what the live service
// advertised, and it derives each family's observed label order, identifier
// shape and behaviour from the per-family VsanPerfQueryPerf captures.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/xml"
	"github.com/vmware/govmomi/vsan/methods"
	vsantypes "github.com/vmware/govmomi/vsan/types"
)

const clusterMoid = "domain-c6004" // the captured cluster's MoRef, part of every fixture file name

type family struct {
	Behaviour string   `json:"behaviour"`
	Labels    []string `json:"labels,omitempty"`
	RefShapes []string `json:"refShapes,omitempty"`
	Entities  int      `json:"entities"`
	// Stats are the observed value statistics per label (mirrors
	// catalog.Stats): min, p25, median, p75, max over every entity and
	// sample in the capture, and whether the service returned decimals.
	Stats map[string]stats `json:"stats,omitempty"`
}

type stats struct {
	Min   float64 `json:"min"`
	P25   float64 `json:"p25"`
	P50   float64 `json:"p50"`
	P75   float64 `json:"p75"`
	Max   float64 `json:"max"`
	Float bool    `json:"float,omitempty"`
	N     int     `json:"n"`
}

type about struct {
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

// entityType mirrors catalog.EntityType (kept in step by catalog_test.go).
type entityType struct {
	Name           string                    `xml:"name" json:"name"`
	Id             string                    `xml:"id" json:"id"`
	Graphs         []vsantypes.VsanPerfGraph `xml:"graphs" json:"graphs"`
	Description    string                    `xml:"description,omitempty" json:"description,omitempty"`
	AdvancedGraphs []vsantypes.VsanPerfGraph `xml:"advancedGraphs,omitempty" json:"advancedGraphs,omitempty"`
	VerboseGraphs  []vsantypes.VsanPerfGraph `xml:"verboseGraphs,omitempty" json:"verboseGraphs,omitempty"`
}

type entityTypesResponse struct {
	Returnval []entityType `xml:"returnval"`
}

type entityTypesBody struct {
	Res *entityTypesResponse `xml:"urn:vsan VsanPerfGetSupportedEntityTypesResponse,omitempty"`
}

type catalog struct {
	Source              string            `json:"source"`
	About               about             `json:"about"`
	VimServiceVersions  string            `json:"vimServiceVersions"`
	VsanServiceVersions string            `json:"vsanServiceVersions"`
	EntityTypes         []entityType      `json:"entityTypes"`
	Families            map[string]family `json:"families"`
	MetricsCollectInt   int32             `json:"metricsCollectInterval"`
}

func main() {
	fixtures := flag.String("fixtures", "fixtures/live/9.1.1", "sanitized capture directory")
	out := flag.String("out", "catalog.json", "output file")
	flag.Parse()
	data, err := Build(*fixtures)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// quantile is the nearest-rank quantile of sorted xs.
func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	i := int(q*float64(len(xs)-1) + 0.5)
	return xs[i]
}

func read(dir, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, name))
}

// decode unmarshals a captured SOAP response into body (a methods.*Body).
func decode(data []byte, body any) error {
	env := soap.Envelope{Body: body}
	return xml.NewDecoder(bytes.NewReader(data)).Decode(&env)
}

var (
	uuidRe  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	cpuRe   = regexp.MustCompile(`^cpu-\d+$`)
	worldRe = regexp.MustCompile(`^VSAN_0x[0-9a-f]+_\w+$`)
	numRe   = regexp.MustCompile(`^\d+$`)
	vmnicRe = regexp.MustCompile(`^vmnic\d+$`)
	vmkRe   = regexp.MustCompile(`^vmk\d+$`)
	scsiRe  = regexp.MustCompile(`^scsi\d+:\d+$`)
	// <detail><InvalidArgumentFault xsi:type="vim25:InvalidArgument" ...
	faultTypeRe = regexp.MustCompile(`<detail><(\w+)Fault `)
)

// Build returns the catalog JSON for a fixture directory.
func Build(dir string) ([]byte, error) {
	c := catalog{Source: "fixtures/live/9.1.1 (sanitized capture of vCenter 9.1.1, one vSAN ESA cluster)", Families: map[string]family{}, MetricsCollectInt: 300}

	// About, from the capture summary.
	var summary struct {
		About struct {
			Version, Build, ApiVersion, FullName, OsType string
			ProductLineId, LicenseProductName            string
			LicenseProductVersion                        string
		}
		Inventory struct {
			Hosts []struct{ Product, Version, Build string }
			Vms   []struct{ InstanceUuid string }
		}
	}
	b, err := read(dir, "summary.json")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &summary); err != nil {
		return nil, fmt.Errorf("summary.json: %w", err)
	}
	c.About = about{
		Version: summary.About.Version, Build: summary.About.Build, ApiVersion: summary.About.ApiVersion,
		FullName: summary.About.FullName, OsType: summary.About.OsType, ProductLine: summary.About.ProductLineId,
		LicenseName: summary.About.LicenseProductName, LicenseVer: summary.About.LicenseProductVersion,
	}
	if len(summary.Inventory.Hosts) > 0 {
		h := summary.Inventory.Hosts[0]
		c.About.EsxiVersion, c.About.EsxiBuild, c.About.EsxiFullName = h.Version, h.Build, h.Product
	}
	vmUUIDs := map[string]bool{}
	for _, vm := range summary.Inventory.Vms {
		vmUUIDs[vm.InstanceUuid] = true
	}

	// Service version documents, verbatim.
	if b, err = read(dir, "00-vimServiceVersions.resp.xml"); err != nil {
		return nil, err
	}
	c.VimServiceVersions = string(b)
	if b, err = read(dir, "00-vsanServiceVersions.resp.xml"); err != nil {
		return nil, err
	}
	c.VsanServiceVersions = string(b)

	// Entity types.
	if b, err = read(dir, "entitytypes-"+clusterMoid+".resp.xml"); err != nil {
		return nil, err
	}
	var et entityTypesBody
	if err := decode(b, &et); err != nil {
		return nil, fmt.Errorf("entitytypes: %w", err)
	}
	if et.Res == nil || len(et.Res.Returnval) == 0 {
		return nil, fmt.Errorf("entitytypes: no entity types decoded")
	}
	c.EntityTypes = et.Res.Returnval

	// Identifier sets for classifying entityRefIds.
	if b, err = read(dir, "clusterconfig-"+clusterMoid+".resp.xml"); err != nil {
		return nil, err
	}
	var cc methods.VsanClusterGetConfigBody
	if err := decode(b, &cc); err != nil {
		return nil, fmt.Errorf("clusterconfig: %w", err)
	}
	clusterUUID := cc.Res.Returnval.DefaultConfig.Uuid
	if b, err = read(dir, "nodeinfo-"+clusterMoid+".resp.xml"); err != nil {
		return nil, err
	}
	var ni methods.VsanPerfQueryNodeInformationBody
	if err := decode(b, &ni); err != nil {
		return nil, fmt.Errorf("nodeinfo: %w", err)
	}
	nodeUUIDs := map[string]bool{}
	for _, n := range ni.Res.Returnval {
		nodeUUIDs[n.VsanNodeUuid] = true
	}
	diskUUIDs := map[string]bool{}
	md, _ := filepath.Glob(filepath.Join(dir, "manageddisks-"+clusterMoid+"-*.resp.xml"))
	vsanUUIDRe := regexp.MustCompile(`<vsanUuid>([0-9a-f-]{36})</vsanUuid>`)
	for _, f := range md {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, m := range vsanUUIDRe.FindAllStringSubmatch(string(b), -1) {
			diskUUIDs[m[1]] = true
		}
	}
	kind := func(part string) string {
		switch {
		case part == clusterUUID:
			return "cluster"
		case nodeUUIDs[part]:
			return "node"
		case diskUUIDs[part]:
			return "disk"
		case vmUUIDs[part]:
			return "vm"
		case strings.HasSuffix(part, ".vmdk"):
			return "vdisk-path"
		case uuidRe.MatchString(part):
			return "uuid"
		case cpuRe.MatchString(part):
			return "cpu-N"
		case worldRe.MatchString(part):
			return "VSAN_0xHEX_NAME"
		case numRe.MatchString(part):
			return "N"
		case vmnicRe.MatchString(part):
			return "vmnicN"
		case vmkRe.MatchString(part):
			return "vmkN"
		case scsiRe.MatchString(part):
			return "scsiN:M"
		}
		return part
	}

	// Per-family captures.
	for _, t := range c.EntityTypes {
		name := t.Name
		if _, done := c.Families[name]; done {
			continue
		}
		b, err := read(dir, "perf-"+clusterMoid+"-"+name+".resp.xml")
		if err != nil {
			return nil, fmt.Errorf("family %s: %w", name, err)
		}
		var qp methods.VsanPerfQueryPerfBody
		if err := decode(b, &qp); err != nil {
			return nil, fmt.Errorf("perf %s: %w", name, err)
		}
		f := family{}
		switch {
		case qp.Fault_ != nil:
			// The fault detail carries xsi:type="vim25:InvalidArgument"; read
			// the detail element name rather than rely on prefix resolution.
			switch m := faultTypeRe.FindSubmatch(b); {
			case m == nil:
				return nil, fmt.Errorf("perf %s: fault detail type not found", name)
			case string(m[1]) == "InvalidArgument":
				f.Behaviour = "invalid-argument"
			case string(m[1]) == "NotSupported":
				f.Behaviour = "not-supported"
			default:
				return nil, fmt.Errorf("perf %s: unexpected fault %s", name, m[1])
			}
		case qp.Res == nil:
			return nil, fmt.Errorf("perf %s: no response", name)
		default:
			shapes := map[string]bool{}
			seen := map[string]bool{}
			values := map[string][]float64{}
			floats := map[string]bool{}
			for _, e := range qp.Res.Returnval {
				if strings.Contains(e.EntityRefId, "*") {
					continue
				}
				f.Entities++
				_, tail, _ := strings.Cut(e.EntityRefId, ":")
				var parts []string
				for i, p := range strings.Split(tail, "|") {
					if i == 0 && strings.HasPrefix(p, "/") {
						p = p[1:]
					}
					parts = append(parts, kind(p))
				}
				shapes[strings.Join(parts, "|")] = true
				for _, v := range e.Value {
					if !seen[v.MetricId.Label] {
						seen[v.MetricId.Label] = true
						f.Labels = append(f.Labels, v.MetricId.Label)
					}
					// "None" marks a sample the service had no value for
					// (entities created mid-window)
					for _, raw := range strings.Split(v.Values, ",") {
						if raw == "" || raw == "None" {
							continue
						}
						x, err := strconv.ParseFloat(raw, 64)
						if err != nil {
							return nil, fmt.Errorf("perf %s: %s: value %q", name, v.MetricId.Label, raw)
						}
						if strings.Contains(raw, ".") {
							floats[v.MetricId.Label] = true
						}
						values[v.MetricId.Label] = append(values[v.MetricId.Label], x)
					}
				}
			}
			if f.Entities == 0 {
				f.Behaviour = "empty"
			} else {
				f.Behaviour = "data"
				for s := range shapes {
					f.RefShapes = append(f.RefShapes, s)
				}
				sort.Strings(f.RefShapes)
				f.Stats = map[string]stats{}
				for label, xs := range values {
					sort.Float64s(xs)
					f.Stats[label] = stats{
						Min: xs[0], P25: quantile(xs, 0.25), P50: quantile(xs, 0.5), P75: quantile(xs, 0.75), Max: xs[len(xs)-1],
						Float: floats[label], N: len(xs),
					}
				}
			}
		}
		c.Families[name] = f
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", " ")
	if err := enc.Encode(&c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
