package sim

import (
	"strconv"
	"strings"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vim25/xml"
)

// storagePerformance keeps the observed datastore counters consistent with
// configured space. Other inventory retains vcsim's performance behavior.
type storagePerformance struct {
	*simulator.PerformanceManager
	stores map[types.ManagedObjectReference]bool
}

var storageCounterIDs = []int32{178, 179, 281, 282, 283, 284, 136, 137}

// vcsim's normal PerfMetricIntSeries uses omitempty on the value slice. Its
// XML encoder applies that flag to each element, dropping zero samples. Shape
// only QueryPerf's response, retaining the standard xsi types on the wire.
type storagePerfSeries struct {
	Type  string             `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr"`
	ID    types.PerfMetricId `xml:"id"`
	Value []int64            `xml:"value"`
}
type storagePerfEntity struct {
	metric types.BasePerfEntityMetricBase
}

func (e storagePerfEntity) MarshalXML(enc *xml.Encoder, start xml.StartElement) error {
	switch m := e.metric.(type) {
	case *types.PerfEntityMetric:
		if m == nil {
			return nil
		}
		wire := struct {
			Type       string                       `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr"`
			Entity     types.ManagedObjectReference `xml:"entity"`
			SampleInfo []types.PerfSampleInfo       `xml:"sampleInfo,omitempty"`
			Value      []storagePerfSeries          `xml:"value,omitempty"`
		}{Type: "PerfEntityMetric", Entity: m.Entity, SampleInfo: m.SampleInfo}
		for _, raw := range m.Value {
			if s, ok := raw.(*types.PerfMetricIntSeries); ok && s != nil {
				wire.Value = append(wire.Value, storagePerfSeries{Type: "PerfMetricIntSeries", ID: s.Id, Value: s.Value})
			}
		}
		return enc.EncodeElement(wire, start)
	case *types.PerfEntityMetricCSV:
		if m == nil {
			return nil
		}
		wire := struct {
			Type          string                       `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr"`
			Entity        types.ManagedObjectReference `xml:"entity"`
			SampleInfoCSV string                       `xml:"sampleInfoCSV"`
			Value         []types.PerfMetricSeriesCSV  `xml:"value,omitempty"`
		}{Type: "PerfEntityMetricCSV", Entity: m.Entity, SampleInfoCSV: m.SampleInfoCSV, Value: m.Value}
		return enc.EncodeElement(wire, start)
	default:
		return nil
	}
}

type storagePerfResponse struct {
	Returnval []storagePerfEntity `xml:"returnval,omitempty"`
}
type storagePerfBody struct {
	Res    *storagePerfResponse `xml:"urn:vim25 QueryPerfResponse,omitempty"`
	Fault_ *soap.Fault          `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault,omitempty"`
}

func (b *storagePerfBody) Fault() *soap.Fault { return b.Fault_ }

func (p *storagePerformance) QueryAvailablePerfMetric(ctx *simulator.Context, req *types.QueryAvailablePerfMetric) soap.HasFault {
	if !p.stores[req.Entity] {
		return p.PerformanceManager.QueryAvailablePerfMetric(ctx, req)
	}
	body := &methods.QueryAvailablePerfMetricBody{Res: &types.QueryAvailablePerfMetricResponse{}}
	for _, id := range storageCounterIDs {
		body.Res.Returnval = append(body.Res.Returnval, types.PerfMetricId{CounterId: id})
	}
	return body
}

func storageCounterValue(ds *simulator.Datastore, id int32) int64 {
	used := (ds.Summary.Capacity - ds.Summary.FreeSpace) / 1024
	switch id {
	case 281, 282, 284:
		return used
	case 283:
		return ds.Summary.Capacity / 1024
	default:
		return 0 // The simulator performs no storage I/O.
	}
}

func (p *storagePerformance) QueryPerf(ctx *simulator.Context, req *types.QueryPerf) soap.HasFault {
	response := p.PerformanceManager.QueryPerf(ctx, req)
	body, ok := response.(*methods.QueryPerfBody)
	if !ok || body.Fault_ != nil || body.Res == nil {
		return response
	}
	needsWire := false
	for _, entry := range body.Res.Returnval {
		switch m := entry.(type) {
		case nil:
			continue
		case *types.PerfEntityMetric:
			if m == nil {
				continue
			}
		case *types.PerfEntityMetricCSV:
			if m == nil {
				continue
			}
		}
		ref := entry.GetPerfEntityMetricBase().Entity
		if !p.stores[ref] {
			continue
		}
		needsWire = true
		ds, ok := ctx.Map.Get(ref).(*simulator.Datastore)
		if !ok {
			continue
		}
		switch v := entry.(type) {
		case *types.PerfEntityMetric:
			for _, series := range v.Value {
				if s, ok := series.(*types.PerfMetricIntSeries); ok && s != nil {
					value := storageCounterValue(ds, s.Id.CounterId)
					for i := range s.Value {
						s.Value[i] = value
					}
				}
			}
		case *types.PerfEntityMetricCSV:
			for i := range v.Value {
				series := &v.Value[i]
				if series.Value == "" {
					continue
				}
				count := len(strings.Split(series.Value, ","))
				values := make([]string, count)
				value := strconv.FormatInt(storageCounterValue(ds, series.Id.CounterId), 10)
				for i := range values {
					values[i] = value
				}
				series.Value = strings.Join(values, ",")
			}
		}
	}
	if needsWire {
		wire := &storagePerfBody{Res: &storagePerfResponse{}}
		for _, entry := range body.Res.Returnval {
			switch m := entry.(type) {
			case *types.PerfEntityMetric:
				if m != nil {
					wire.Res.Returnval = append(wire.Res.Returnval, storagePerfEntity{metric: m})
				}
			case *types.PerfEntityMetricCSV:
				if m != nil {
					wire.Res.Returnval = append(wire.Res.Returnval, storagePerfEntity{metric: m})
				}
			case nil: // An absent series cannot contribute samples.
			default:
				return body
			}
		}
		return wire
	}
	return body
}

func (s *Simulator) installStoragePerformance(refs map[string]*simulator.Datastore) {
	if len(refs) == 0 {
		return
	}
	ctx := s.model.Service.Context
	ref := *s.model.ServiceContent.PerfManager
	base, ok := ctx.Map.Get(ref).(*simulator.PerformanceManager)
	if !ok {
		return
	}
	p := &storagePerformance{PerformanceManager: base, stores: map[types.ManagedObjectReference]bool{}}
	for _, ds := range refs {
		p.stores[ds.Self] = true
	}
	ctx.Map.Put(p)
}
