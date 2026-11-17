package sim

import (
	"reflect"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// Query is the SearchIndex inventory query used by Operations 9.1.1 to read
// VVOL datastore alarms. It is not part of govmomi's public vim25 bindings.
type Query struct {
	This      types.ManagedObjectReference `xml:"_this"`
	QuerySpec struct {
		Properties   []string `xml:"properties"`
		ResourceType string   `xml:"resourceType"`
		Filters      []struct {
			Predicates []struct {
				PropertyPath    string        `xml:"propertyPath"`
				Operator        string        `xml:"operator"`
				ComparableValue types.AnyType `xml:"comparableValue,typeattr"`
			} `xml:"predicates"`
		} `xml:"filters"`
		Limit int `xml:"limit"`
	} `xml:"querySpec"`
}

type searchIndex struct{ *simulator.SearchIndex }
type queryResponse struct {
	Returnval struct{} `xml:"returnval"`
}
type queryBody struct {
	Res    *queryResponse `xml:"urn:vim25 QueryResponse,omitempty"`
	Fault_ *soap.Fault    `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault,omitempty"`
}

func (b *queryBody) Fault() *soap.Fault { return b.Fault_ }
func init()                             { types.Add("Query", reflect.TypeOf(Query{})) }

func (s *searchIndex) Query(ctx *simulator.Context, req *Query) soap.HasFault {
	invalid := func() soap.HasFault {
		return &queryBody{Fault_: simulator.Fault("Unsupported search query", &types.InvalidArgument{InvalidProperty: "querySpec"})}
	}
	q := req.QuerySpec
	if q.ResourceType != "Datastore" || q.Limit < 0 || len(q.Properties) == 0 || len(q.Filters) != 1 || len(q.Filters[0].Predicates) != 1 {
		return invalid()
	}
	for _, p := range q.Properties {
		if p != "@moRef" && p != "triggeredAlarmState" {
			return invalid()
		}
	}
	p := q.Filters[0].Predicates[0]
	if p.PropertyPath != "summary.type" || p.Operator != "Equal" || p.ComparableValue != "VVOL" {
		return invalid()
	}
	// The topology models vSAN, VMFS, and NFS, and has no VVOL datastores.
	// Refuse rather than report an empty result if that contract changes.
	for _, obj := range ctx.Map.All("Datastore") {
		if ds, ok := obj.(*simulator.Datastore); ok && ds.Summary.Type == "VVOL" {
			return &queryBody{Fault_: simulator.Fault("VVOL alarm queries are not modeled", &types.NotSupported{})}
		}
	}
	return &queryBody{Res: &queryResponse{}}
}
