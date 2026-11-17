package sim

import (
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// vcsim removes absent optional properties from the first page, but not
// continuation pages. DynamicProperty.val is required by VMware's clients.
// Keep the same omission rule on every page, including session collectors.
type propertyCollector struct {
	simulator.PropertyCollector
	active  *simulator.PropertyCollector
	waitMu  sync.Mutex
	stateMu sync.RWMutex
}

// govmomi tracks objects already sent inside each filter, and retains pending
// pages before checking the version. A fresh empty version restarts both. Use
// its public filter constructor to reset that private state, then restore the
// original identities so clients can still retrieve and destroy their filters.
func (p *propertyCollector) restartSnapshot(ctx *simulator.Context) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	filters := append([]types.ManagedObjectReference(nil), p.delegate().Filter...)
	ctx.Map.RemoveHandler(p.delegate())
	p.active = &simulator.PropertyCollector{PropertyCollector: mo.PropertyCollector{Self: p.Self}}
	for _, ref := range filters {
		old, ok := ctx.Session.Get(ref).(*simulator.PropertyFilter)
		if !ok {
			continue
		}
		response := p.active.CreateFilter(ctx, &types.CreateFilter{This: p.Self, Spec: old.Spec, PartialUpdates: old.PartialUpdates}).(*methods.CreateFilterBody)
		freshRef := response.Res.Returnval
		fresh := ctx.Session.Get(freshRef).(*simulator.PropertyFilter)
		ctx.Map.RemoveHandler(fresh)
		ctx.Session.Remove(ctx, freshRef)
		fresh.Self = ref
		ctx.Session.Put(fresh)
		ctx.Map.AddHandler(fresh)
		p.active.Filter[len(p.active.Filter)-1] = ref
	}
}

func (p *propertyCollector) delegate() *simulator.PropertyCollector {
	if p.active != nil {
		return p.active
	}
	return &p.PropertyCollector
}

// Property reads must see the live delegate's filter list, including filters
// destroyed directly through their stable references.
func (p *propertyCollector) Get() mo.Reference {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.delegate()
}

// Cancellation can arrive while a wait is in progress. Keep the delegate
// alive through the call instead of overwriting a mutex used by its handlers.
func (p *propertyCollector) CancelWaitForUpdates(req *types.CancelWaitForUpdates) soap.HasFault {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.delegate().CancelWaitForUpdates(req)
}

func (p *propertyCollector) CreateFilter(ctx *simulator.Context, req *types.CreateFilter) soap.HasFault {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.delegate().CreateFilter(ctx, req)
}

func (p *propertyCollector) DestroyPropertyCollector(ctx *simulator.Context, req *types.DestroyPropertyCollector) soap.HasFault {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.delegate().DestroyPropertyCollector(ctx, req)
}

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
	response := p.WaitForUpdatesEx(ctx, &types.WaitForUpdatesEx{This: req.This, Version: req.Version})
	body := response.(*methods.WaitForUpdatesExBody)
	result := &methods.WaitForUpdatesBody{Fault_: body.Fault_}
	if body.Res != nil && body.Res.Returnval != nil {
		result.Res = &types.WaitForUpdatesResponse{Returnval: *body.Res.Returnval}
	}
	return result
}

func (p *propertyCollector) WaitForUpdatesEx(ctx *simulator.Context, req *types.WaitForUpdatesEx) soap.HasFault {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	if req.Version == "" {
		p.restartSnapshot(ctx)
	}
	response := p.delegate().WaitForUpdatesEx(ctx, req)
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
