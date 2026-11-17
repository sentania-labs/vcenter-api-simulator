package sim

import (
	"reflect"
	"strings"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// vcsim removes absent optional properties from the first page, but not
// continuation pages. DynamicProperty.val is required by VMware's clients.
// Keep the same omission rule on every page, including session collectors.
type propertyCollector struct{ simulator.PropertyCollector }

func (p *propertyCollector) ContinueRetrievePropertiesEx(ctx *simulator.Context, req *types.ContinueRetrievePropertiesEx) soap.HasFault {
	response := p.PropertyCollector.ContinueRetrievePropertiesEx(ctx, req)
	if body, ok := response.(*methods.ContinueRetrievePropertiesExBody); ok && body.Res != nil {
		shapeProperties(ctx, body.Res.Returnval.Objects)
	}
	return response
}
func (p *propertyCollector) RetrievePropertiesEx(ctx *simulator.Context, req *types.RetrievePropertiesEx) soap.HasFault {
	response := p.PropertyCollector.RetrievePropertiesEx(ctx, req)
	if body, ok := response.(*methods.RetrievePropertiesExBody); ok && body.Res != nil && body.Res.Returnval != nil {
		shapeProperties(ctx, body.Res.Returnval.Objects)
	}
	return response
}
func (p *propertyCollector) RetrieveProperties(ctx *simulator.Context, req *types.RetrieveProperties) soap.HasFault {
	response := p.PropertyCollector.RetrieveProperties(ctx, req)
	if body, ok := response.(*methods.RetrievePropertiesBody); ok && body.Res != nil {
		shapeProperties(ctx, body.Res.Returnval)
	}
	return response
}
func (p *propertyCollector) CreatePropertyCollector(ctx *simulator.Context, _ *types.CreatePropertyCollector) soap.HasFault {
	collector := &propertyCollector{}
	collector.Self = types.ManagedObjectReference{Type: "PropertyCollector"}
	ref := ctx.Session.Put(collector).Reference()
	return &methods.CreatePropertyCollectorBody{Res: &types.CreatePropertyCollectorResponse{Returnval: ref}}
}
func shapeProperties(ctx *simulator.Context, objects []types.ObjectContent) {
	var state *performanceState
	var frame map[perfRef]workload
	var perf *storagePerformance
	for _, obj := range objects {
		if obj.Obj.Type != "HostSystem" && obj.Obj.Type != "VirtualMachine" {
			continue
		}
		for _, prop := range obj.PropSet {
			if prop.Name == "summary" || strings.HasPrefix(prop.Name, "summary.quickStats") {
				perf, _ = ctx.Map.Get(types.ManagedObjectReference{Type: "PerformanceManager", Value: "PerfMgr"}).(*storagePerformance)
				if perf != nil {
					state = readPerformanceState(ctx)
					frame = state.sample(perf.seed, perf.now().Truncate(20*time.Second))
				}
				break
			}
		}
		if state != nil {
			break
		}
	}

	for i := range objects {
		o := &objects[i]
		props := o.PropSet[:0]
		for _, prop := range o.PropSet {
			if absentProperty(prop.Val) {
				continue
			}
			// The backing directory is private to vcsim's file operations. A
			// collector sees the public datastore URL, just as in summary.url.
			if o.Obj.Type == "Datastore" {
				if ds, ok := ctx.Map.Get(o.Obj).(*simulator.Datastore); ok {
					ctx.Map.WithLock(ctx, ds, func() {
						switch v := prop.Val.(type) {
						case *types.VsanDatastoreInfo:
							clone := *v
							clone.Url = ds.Summary.Url
							prop.Val = &clone
						case types.VsanDatastoreInfo:
							v.Url = ds.Summary.Url
							prop.Val = v
						case string:
							if prop.Name == "info.url" {
								prop.Val = ds.Summary.Url
							}
						}
					})
				}
			}
			if state != nil {
				if n := state.nodes[o.Obj]; n != nil && (o.Obj.Type == "VirtualMachine" || o.Obj.Type == "HostSystem") {
					prop = quickStatsProperty(prop, n, frame[o.Obj], perf.now())
				}
			}
			props = append(props, prop)
		}
		o.PropSet = props
	}
}
func absentProperty(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	return (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil()
}

func (p *propertyCollector) WaitForUpdates(ctx *simulator.Context, req *types.WaitForUpdates) soap.HasFault {
	response := p.PropertyCollector.WaitForUpdates(ctx, req)
	if body, ok := response.(*methods.WaitForUpdatesBody); ok && body.Res != nil {
		shapeUpdates(ctx, &body.Res.Returnval)
	}
	return response
}
func (p *propertyCollector) WaitForUpdatesEx(ctx *simulator.Context, req *types.WaitForUpdatesEx) soap.HasFault {
	response := p.PropertyCollector.WaitForUpdatesEx(ctx, req)
	if body, ok := response.(*methods.WaitForUpdatesExBody); ok && body.Res != nil {
		shapeUpdates(ctx, body.Res.Returnval)
	}
	return response
}
func shapeUpdates(ctx *simulator.Context, update *types.UpdateSet) {
	if update == nil {
		return
	}
	var objects []types.ObjectContent
	for _, filter := range update.FilterSet {
		for _, obj := range filter.ObjectSet {
			shaped := types.ObjectContent{Obj: obj.Obj}
			for _, change := range obj.ChangeSet {
				shaped.PropSet = append(shaped.PropSet, types.DynamicProperty{Name: change.Name, Val: change.Val})
			}
			objects = append(objects, shaped)
		}
	}
	shapeProperties(ctx, objects)
	index := 0
	for i := range update.FilterSet {
		for j := range update.FilterSet[i].ObjectSet {
			obj := &update.FilterSet[i].ObjectSet[j]
			for k := range obj.ChangeSet {
				for _, prop := range objects[index].PropSet {
					if prop.Name == obj.ChangeSet[k].Name {
						obj.ChangeSet[k].Val = prop.Val
						break
					}
				}
			}
			index++
		}
	}
}
