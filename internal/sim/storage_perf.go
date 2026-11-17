package sim

import (
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vim25/xml"
)

// storagePerformance serves standard vSphere performance from the shared workload model.
type storagePerformance struct {
	*simulator.PerformanceManager
	seed string
	now  func() time.Time
}

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
