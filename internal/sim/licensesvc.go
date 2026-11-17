package sim

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/xml"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// The license service's reads. The simulator presents itself in evaluation:
// the vCenter and every host are assets whose license is the evaluation one,
// and there are no licenses to list. A licensed vCenter presents a signed
// entitlement instead, which the simulator cannot make and does not try to.

const (
	licenseSystemRef = "cis.license.management.SystemManagementService"
	licenseSystemMo  = "CisLicenseManagementSystemManagementService"

	evaluationLicenseID = "urn:vri:com.vmware.license.license:Evaluation"
	assetIDPrefix       = "urn:vri:com.vmware.license.asset:"

	familyVCenter = "VMware VirtualCenter Server"
	familyESX     = "VMware ESX Server"

	// evaluationDays is how far ahead the evaluation always runs out. The
	// simulator keeps the state the operator chose: an evaluation that is
	// not expired never becomes so by itself.
	evaluationDays = 90

	licenseTime = "2006-01-02T15:04:05Z"
)

// evaluationFeatures are the features of a vCenter evaluation.
var evaluationFeatures = []string{"linkedvc", "woe", "vsa", "xvp", "vcha", "backuprestore", "appliancemigration"}

type licSearchAssetsRequest struct {
	This thisRef `xml:"_this"`
	Spec *struct {
		Attrs              []xml.Attr `xml:",any,attr"`
		ScopeID            string     `xml:"scopeId"`
		MaxResults         *int       `xml:"maxResults"`
		Offset             *int       `xml:"offset"`
		ProductFamilyNames []string   `xml:"productFamilyNames"`
		LicenseState       *string    `xml:"licenseState"`
		LicenseID          *string    `xml:"licenseId"`
	} `xml:"assetSearchSpec"`
}

// assetSearchSpecs are the forms of search the simulator answers.
var assetSearchSpecs = map[string]bool{
	"":                                    true,
	"CisLicenseManagementAssetSearchSpec": true,
	"CisLicenseManagementAssetSearchSpecByProductFamilyNames": true,
	"CisLicenseManagementAssetSearchSpecByLicenseState":       true,
	"CisLicenseManagementAssetSearchSpecByLicenseId":          true,
}

type licSearchAssetsResponse struct {
	XMLName   xml.Name   `xml:"urn:internalcis.license SearchAssetsResponse"`
	Returnval []licAsset `xml:"returnval"`
}

type licSearchLicensesResponse struct {
	XMLName xml.Name `xml:"urn:internalcis.license SearchLicensesResponse"`
}

type licSystemTimeResponse struct {
	XMLName   xml.Name `xml:"urn:internalcis.license QuerySystemTimeResponse"`
	Returnval string   `xml:"returnval"`
}

type licAsset struct {
	ID   string       `xml:"id"`
	Info licAssetInfo `xml:"info"`
}

type licAssetInfo struct {
	InstanceID    string        `xml:"instanceId"`
	ScopeID       string        `xml:"scopeId"`
	Name          string        `xml:"name"`
	Version       string        `xml:"version"`
	Product       licProduct    `xml:"product"`
	EvalInfo      *licLicense   `xml:"evalInfo"`
	CostUnitUsage []licCostUnit `xml:"costUnitUsage"`
	License       licLicense    `xml:"license"`
	InEvaluation  bool          `xml:"inEvaluation"`
}

type licProduct struct {
	ID         string    `xml:"id"`
	Name       string    `xml:"name"`
	EditionKey string    `xml:"editionKey"`
	Family     licFamily `xml:"family"`
}

type licFamily struct {
	Name    string `xml:"name"`
	Version string `xml:"version"`
}

type licLicense struct {
	ID   string         `xml:"id"`
	Info licLicenseInfo `xml:"info"`
}

type licLicenseInfo struct {
	Name         string           `xml:"name"`
	Expiration   string           `xml:"expiration"`
	Restrictions []licRestriction `xml:"restrictions"`
	Properties   []licProperty    `xml:"properties"`
}

type licRestriction struct {
	Product        licProduct     `xml:"product"`
	CostUnitsLimit *licCostLimit  `xml:"costUnitsLimit"`
	Capability     *licCapability `xml:"capability"`
}

type licCostLimit struct {
	Unit            licUnit `xml:"unit"`
	Value           int64   `xml:"value"`
	EnforcementType string  `xml:"enforcementType"`
}

type licCapability struct {
	Version    string        `xml:"version"`
	Features   []licUnit     `xml:"features"`
	Properties []licProperty `xml:"properties"`
}

type licUnit struct {
	ID string `xml:"id"`
}

type licCostUnit struct {
	Unit  licUnit `xml:"unit"`
	Value int64   `xml:"value"`
}

type licProperty struct {
	Key   string   `xml:"key"`
	Value licValue `xml:"value"`
}

// licValue is a typed value: the xsi:type the envelope's prefixes name, and
// the text.
type licValue struct {
	Type string `xml:"xsi:type,attr"`
	Text string `xml:",chardata"`
}

// serveLicenseRead answers SearchAssets, SearchLicenses and QuerySystemTime
// for a signed-in license session. It reports false for an operation, a
// reference or a search form it does not know, which then stays unanswered.
func (h *handler) serveLicenseRead(w http.ResponseWriter, r *http.Request, op string, body []byte) bool {
	switch op {
	case "SearchAssets", "SearchLicenses", "QuerySystemTime":
	default:
		return false
	}
	var search licSearchAssetsRequest
	if err := decodeOperation(body, &search); err != nil {
		return false
	}
	if strings.TrimSpace(search.This.Value) != licenseSystemRef || search.This.Type != licenseSystemMo {
		return false
	}
	if op == "SearchAssets" && search.Spec != nil {
		kind := ""
		for _, a := range search.Spec.Attrs {
			if a.Name.Local == "type" {
				kind = localName(strings.TrimSpace(a.Value))
			}
		}
		if !assetSearchSpecs[kind] {
			return false
		}
	}
	cookie, err := r.Cookie(soap.SessionCookieName)
	if err != nil || !h.license.valid(cookie.Value) {
		// not signed in. What a real vCenter answers here was not observed,
		// so this is the one fault shape the license service was seen to use.
		licenseFault(w)
		return true
	}
	// every answer of a real vCenter sets the session cookie again
	licenseCookie(w, cookie.Value)
	now := h.sim.Env.Now().UTC()
	switch op {
	case "SearchLicenses":
		encodeSOAP(w, http.StatusOK, licSearchLicensesResponse{})
	case "QuerySystemTime":
		encodeSOAP(w, http.StatusOK, licSystemTimeResponse{Returnval: now.Format(stsTime)})
	default:
		assets := h.licenseAssets(clientHost(r), now)
		if spec := search.Spec; spec != nil {
			assets = selectAssets(assets, h.sim.Topology.NodeID, spec.ScopeID, spec.ProductFamilyNames, spec.LicenseState, spec.LicenseID)
			offset, limit := 0, len(assets)
			if spec.Offset != nil && *spec.Offset > 0 {
				offset = *spec.Offset
			}
			if spec.MaxResults != nil && *spec.MaxResults > 0 {
				limit = *spec.MaxResults
			}
			offset = min(offset, len(assets))
			assets = assets[offset:min(len(assets), offset+limit)]
		}
		encodeSOAP(w, http.StatusOK, licSearchAssetsResponse{Returnval: assets})
	}
	return true
}

// localName drops a prefix from a qualified name.
func localName(name string) string {
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		return name[i+1:]
	}
	return name
}

// selectAssets applies a search: the scope, then whichever of the product
// families, the license state and the license id the search names. Every
// asset is in evaluation, so the state "evaluation" and the evaluation
// license select all of them and any other state or license selects none.
func selectAssets(assets []licAsset, node, scope string, families []string, state, license *string) []licAsset {
	if scope = strings.TrimSpace(scope); scope != "" && scope != node {
		return nil
	}
	if state != nil && strings.TrimSpace(*state) != "evaluation" {
		return nil
	}
	if license != nil && strings.TrimSpace(*license) != evaluationLicenseID {
		return nil
	}
	if len(families) == 0 {
		return assets
	}
	want := map[string]bool{}
	for _, f := range families {
		want[strings.TrimSpace(f)] = true
	}
	var out []licAsset
	for _, a := range assets {
		if want[a.Info.Product.Family.Name] {
			out = append(out, a)
		}
	}
	return out
}

// EvaluationEnds is when the evaluation runs out: always evaluationDays
// ahead of today while in evaluation, and yesterday once it is expired.
func EvaluationEnds(l scenario.Licensing, now time.Time) time.Time {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if l.Expired() {
		return day.AddDate(0, 0, -1)
	}
	return day.AddDate(0, 0, evaluationDays)
}

// expirationProperties are the three properties an evaluation license
// carries: the time left in hours and in minutes, and the date it ends.
func expirationProperties(ends, now time.Time) []licProperty {
	var hours, minutes int64
	if left := ends.Sub(now); left > 0 {
		hours, minutes = int64(left/time.Hour), int64(left%time.Hour/time.Minute)
	}
	return []licProperty{
		{Key: "expirationHours", Value: licValue{Type: "xsd:long", Text: strconv.FormatInt(hours, 10)}},
		{Key: "expirationMinutes", Value: licValue{Type: "xsd:long", Text: strconv.FormatInt(minutes, 10)}},
		{Key: "expirationDate", Value: licValue{Type: "xsd:dateTime", Text: ends.Format(licenseTime)}},
	}
}

// licenseAssets lists the vCenter and then every host, each in evaluation.
// LicenseInfo.restrictions and LicenseProductRestriction.capability are required
// by the cis.license client, including on the assigned evaluation license.
//
// The vCenter's asset is put together from parts a real vCenter was seen to
// send: the evaluation block every vCenter asset carries, and the license of
// an asset that is in evaluation (the evaluation's id and name, when it runs
// out and the three expiration properties). A host's asset follows the same
// pattern with the host evaluation edition.
func (h *handler) licenseAssets(host string, now time.Time) []licAsset {
	topo := h.sim.Topology
	ends := EvaluationEnds(*h.licensing.Load(), now)
	props := expirationProperties(ends, now)
	license := licLicense{ID: evaluationLicenseID, Info: licLicenseInfo{Name: "Evaluation", Expiration: ends.Format(licenseTime), Properties: props}}

	name, _ := splitHost(host)
	family := topo.VCenterVersion.LicenseVersion()
	vcEval := licProduct{ID: familyVCenter + family + "vcEvalserver", Name: "Evaluation Mode", EditionKey: "vcEval", Family: licFamily{Name: familyVCenter, Version: family}}
	capability := &licCapability{Version: topo.VCenterVersion.APIVersion, Properties: props}
	for _, f := range evaluationFeatures {
		capability.Features = append(capability.Features, licUnit{ID: f})
	}
	evalInfo := license
	evalInfo.Info.Restrictions = []licRestriction{{
		Product:        vcEval,
		CostUnitsLimit: &licCostLimit{Unit: licUnit{ID: "server"}, Value: 0, EnforcementType: "hardEnforced"},
		Capability:     capability,
	}}
	assets := []licAsset{{
		ID: assetIDPrefix + topo.InstanceUUID + "-" + topo.NodeID,
		Info: licAssetInfo{
			InstanceID: topo.InstanceUUID, ScopeID: topo.NodeID, Name: name, Version: topo.VCenterVersion.APIVersion,
			Product: vcEval, EvalInfo: &evalInfo,
			CostUnitUsage: []licCostUnit{{Unit: licUnit{ID: "server"}, Value: 1}},
			License:       evalInfo, InEvaluation: true,
		},
	}}
	for _, refs := range h.sim.Clusters {
		for i, ref := range refs.Hosts {
			if i >= len(refs.Cluster.Hosts) {
				break
			}
			th := refs.Cluster.Hosts[i]
			// one processor of half as many cores as threads, as the host's
			// hardware summary has it
			packages, cores := int64(1), int64(max(th.Threads/2, 1))
			esx := th.Version.LicenseVersion()
			hostProduct := licProduct{ID: familyESX + esx + "esxEvalcpuPackage", Name: "Evaluation Mode", EditionKey: "esxEval", Family: licFamily{Name: familyESX, Version: esx}}
			hostLicense := license
			hostLicense.Info.Restrictions = []licRestriction{{
				Product:    hostProduct,
				Capability: &licCapability{Version: th.Version.APIVersion, Properties: props},
			}}
			assets = append(assets, licAsset{
				ID: assetIDPrefix + ref.Value + "-" + topo.NodeID,
				Info: licAssetInfo{
					InstanceID: ref.Value, ScopeID: topo.NodeID, Name: th.Name, Version: th.Version.APIVersion,
					Product: hostProduct,
					CostUnitUsage: []licCostUnit{
						{Unit: licUnit{ID: "cpuPackage"}, Value: packages},
						{Unit: licUnit{ID: "cpuCore"}, Value: cores},
					},
					License: hostLicense, InEvaluation: true,
				},
			})
		}
	}
	return assets
}
