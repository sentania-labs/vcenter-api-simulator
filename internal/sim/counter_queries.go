package sim

import (
	"reflect"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// The simulator has no guest agent producing internal counters. An empty
// successful result is intentional; ordinary counters come from PerfCounter.
type QueryPerfCounterInt struct {
	This types.ManagedObjectReference `xml:"_this"`
}
type internalCounterResponse struct{}
type internalCounterBody struct {
	Res *internalCounterResponse `xml:"urn:vim25 QueryPerfCounterIntResponse"`
}

func (*internalCounterBody) Fault() *soap.Fault { return nil }
func init()                                     { types.Add("QueryPerfCounterInt", reflect.TypeOf(QueryPerfCounterInt{})) }
func (p *storagePerformance) QueryPerfCounterInt(_ *simulator.Context, _ *QueryPerfCounterInt) soap.HasFault {
	return &internalCounterBody{Res: &internalCounterResponse{}}
}
func (p *storagePerformance) QueryPerfCounterByLevel(_ *simulator.Context, req *types.QueryPerfCounterByLevel) soap.HasFault {
	body := &methods.QueryPerfCounterByLevelBody{Res: &types.QueryPerfCounterByLevelResponse{}}
	for _, c := range p.PerfCounter {
		if c.Level > 0 && c.Level <= req.Level {
			body.Res.Returnval = append(body.Res.Returnval, c)
		}
	}
	return body
}

// Operations lowers the collection level of the GPU counters it discovers.
// Keep that mapping on this simulator's manager, so later reads agree.
func (p *storagePerformance) UpdateCounterLevelMapping(ctx *simulator.Context, req *types.UpdateCounterLevelMapping) soap.HasFault {
	for _, mapping := range req.CounterLevelMap {
		found := false
		for _, counter := range p.PerfCounter {
			if counter.Key == mapping.CounterId {
				found = true
				break
			}
		}
		if !found || (mapping.AggregateLevel != 0 && (mapping.AggregateLevel < 1 || mapping.AggregateLevel > 4)) || (mapping.PerDeviceLevel != 0 && (mapping.PerDeviceLevel < 1 || mapping.PerDeviceLevel > 4)) {
			return &methods.UpdateCounterLevelMappingBody{Fault_: simulator.Fault("Invalid counter mapping", &types.InvalidArgument{InvalidProperty: "counterLevelMap"})}
		}
	}
	counters := append([]types.PerfCounterInfo(nil), p.PerfCounter...)
	for _, mapping := range req.CounterLevelMap {
		for i := range counters {
			if counters[i].Key == mapping.CounterId {
				if mapping.AggregateLevel != 0 {
					counters[i].Level = mapping.AggregateLevel
				}
				if mapping.PerDeviceLevel != 0 {
					counters[i].PerDeviceLevel = mapping.PerDeviceLevel
				}
			}
		}
	}
	ctx.Map.Update(ctx, p, []types.PropertyChange{{Name: "perfCounter", Val: counters}})
	return &methods.UpdateCounterLevelMappingBody{Res: &types.UpdateCounterLevelMappingResponse{}}
}

func (p *storagePerformance) QueryPerfCounter(ctx *simulator.Context, req *types.QueryPerfCounter) soap.HasFault {
	response := p.PerformanceManager.QueryPerfCounter(ctx, req)
	if body, ok := response.(*methods.QueryPerfCounterBody); ok && body.Res != nil {
		for i, id := range req.CounterId {
			for _, counter := range p.PerfCounter {
				if counter.Key == id {
					body.Res.Returnval[i] = counter
					break
				}
			}
		}
	}
	return response
}
