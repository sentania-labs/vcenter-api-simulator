package sim

import (
	"reflect"
	"sort"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// DataProviderQuery is the internal host-to-datacenter query used by the
// Operations inventory client. Values are positional, including absent ones.
type DataProviderQuery struct {
	This      types.ManagedObjectReference `xml:"_this"`
	QuerySpec resourceQuery                `xml:"querySpec"`
}
type resourceQuery struct {
	Properties    []string `xml:"properties"`
	ResourceModel string   `xml:"resourceModel"`
	Filter        *struct {
		Criteria []struct {
			Property       string          `xml:"property"`
			Operator       string          `xml:"operator"`
			ComparableList []types.AnyType `xml:"comparableList"`
		} `xml:"criteria"`
	} `xml:"filter"`
	Offset           int  `xml:"offset"`
	Limit            *int `xml:"limit"`
	ReturnTotalCount bool `xml:"returnTotalCount"`
}
type resourceValue struct {
	Value types.AnyType `xml:"value,typeattr,omitempty"`
}
type resourceItem struct {
	Values []resourceValue `xml:"propertyValues"`
}
type resourceResult struct {
	Properties []string       `xml:"properties"`
	Items      []resourceItem `xml:"items,omitempty"`
	TotalCount *int64         `xml:"totalCount,omitempty"`
}
type resourceResponse struct {
	Returnval resourceResult `xml:"returnval"`
}
type resourceBody struct {
	Res    *resourceResponse `xml:"urn:vim25 DataProviderQueryResponse,omitempty"`
	Fault_ *soap.Fault       `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault,omitempty"`
}

func (b *resourceBody) Fault() *soap.Fault { return b.Fault_ }

type resourceModel struct{ types.ManagedObjectReference }

func init() { types.Add("DataProviderQuery", reflect.TypeOf(DataProviderQuery{})) }

func (p *resourceModel) DataProviderQuery(ctx *simulator.Context, req *DataProviderQuery) soap.HasFault {
	q := req.QuerySpec
	invalid := func() soap.HasFault {
		return &resourceBody{Fault_: simulator.Fault("Unsupported resource query", &types.InvalidArgument{InvalidProperty: "querySpec"})}
	}
	if q.ResourceModel != "HostSystem" || len(q.Properties) == 0 || q.Offset < 0 || (q.Limit != nil && *q.Limit < 0) {
		return invalid()
	}
	for _, prop := range q.Properties {
		if prop != "@modelKey" && prop != "dc" {
			return invalid()
		}
	}
	selected := map[types.ManagedObjectReference]bool{}
	if q.Filter != nil {
		if len(q.Filter.Criteria) != 1 {
			return invalid()
		}
		c := q.Filter.Criteria[0]
		if c.Property != "@modelKey" || c.Operator != "In" {
			return invalid()
		}
		for _, v := range c.ComparableList {
			ref, ok := v.(types.ManagedObjectReference)
			if !ok || ref.Type != "HostSystem" {
				return invalid()
			}
			selected[ref] = true
		}
	}
	var refs []types.ManagedObjectReference
	for _, obj := range ctx.Map.All("HostSystem") {
		ref := obj.Reference()
		if q.Filter == nil || selected[ref] {
			refs = append(refs, ref)
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Value < refs[j].Value })
	result := resourceResult{Properties: q.Properties}
	if q.ReturnTotalCount {
		total := int64(len(refs))
		result.TotalCount = &total
	}
	start := min(q.Offset, len(refs))
	end := len(refs)
	if q.Limit != nil {
		end = start + min(*q.Limit, end-start)
	}
	for _, ref := range refs[start:end] {
		dc := parentDatacenter(ctx, ref)
		item := resourceItem{}
		for _, prop := range q.Properties {
			v := resourceValue{}
			if prop == "@modelKey" {
				v.Value = ref
			} else if dc != nil {
				v.Value = *dc
			}
			item.Values = append(item.Values, v)
		}
		result.Items = append(result.Items, item)
	}
	return &resourceBody{Res: &resourceResponse{Returnval: result}}
}
func parentDatacenter(ctx *simulator.Context, ref types.ManagedObjectReference) *types.ManagedObjectReference {
	seen := map[types.ManagedObjectReference]bool{}
	for !seen[ref] {
		seen[ref] = true
		if ref.Type == "Datacenter" {
			return &ref
		}
		obj := ctx.Map.Get(ref)
		if obj == nil {
			return nil
		}
		entity, ok := obj.(mo.Entity)
		if !ok || entity.Entity().Parent == nil {
			return nil
		}
		ref = *entity.Entity().Parent
	}
	return nil
}
