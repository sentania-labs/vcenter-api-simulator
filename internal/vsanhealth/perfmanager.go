package vsanhealth

import (
	"strconv"
	"strings"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	vim25types "github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vsan/methods"
	vsantypes "github.com/vmware/govmomi/vsan/types"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/perf"
)

// maxWildcardWindow is the live limit on a wildcard query's window.
const maxWildcardWindow = 12 * time.Hour

// PerfManager is VsanPerformanceManager:vsan-performance-manager.
type PerfManager struct {
	vim25types.ManagedObjectReference
	env *Env
}

// VsanPerfQueryNodeInformation lists the cluster's hosts as the Performance
// Service sees them (fixture nodeinfo-domain-c6004): the first host is the
// CMMDS and stats master and carries masterInfo.
func (m *PerfManager) VsanPerfQueryNodeInformation(ctx *simulator.Context, req *vsantypes.VsanPerfQueryNodeInformation) soap.HasFault {
	res := &vsantypes.VsanPerfQueryNodeInformationResponse{}
	cl := m.env.Cluster(req.Cluster)
	if cl != nil && len(cl.Hosts) > 0 {
		master := cl.Hosts[0].NodeUUID
		now := m.env.Now().UTC()
		sinceCollect := int64(now.Sub(now.Truncate(perf.Interval)) / time.Second)
		for i, h := range cl.Hosts {
			n := vsantypes.VsanPerfNodeInformation{
				Version:        m.env.Catalog.About.EsxiVersion,
				Hostname:       h.Name,
				IsCmmdsMaster:  i == 0,
				IsStatsMaster:  i == 0,
				VsanMasterUuid: master,
				VsanNodeUuid:   h.NodeUUID,
				DiagnosticMode: vim25types.NewBool(false),
			}
			if i == 0 {
				n.MasterInfo = &vsantypes.VsanPerfMasterInformation{
					SecSinceLastStatsWrite:    sinceCollect + 1,
					SecSinceLastStatsCollect:  sinceCollect + 5,
					StatsIntervalSec:          int64(perf.Interval / time.Second),
					StatsDirectoryPercentFree: 95,
					VerboseMode:               vim25types.NewBool(false),
				}
			}
			res.Returnval = append(res.Returnval, n)
		}
	}
	return &methods.VsanPerfQueryNodeInformationBody{Res: res}
}

type entityTypesResponse struct {
	Returnval []catalog.EntityType `xml:"returnval"`
}

type entityTypesBody struct {
	Res    *entityTypesResponse `xml:"urn:vsan VsanPerfGetSupportedEntityTypesResponse,omitempty"`
	Fault_ *soap.Fault          `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault,omitempty"`
}

func (b *entityTypesBody) Fault() *soap.Fault { return b.Fault_ }

// VsanPerfGetSupportedEntityTypes advertises exactly the captured catalog.
func (m *PerfManager) VsanPerfGetSupportedEntityTypes(ctx *simulator.Context, req *vsantypes.VsanPerfGetSupportedEntityTypes) soap.HasFault {
	return &entityTypesBody{Res: &entityTypesResponse{Returnval: m.env.Catalog.EntityTypes}}
}

// entityMetricCSV mirrors VsanPerfEntityMetricCSV but always emits
// sampleInfo, as the live service does for an empty result
// (perf-domain-c6004-disk-group.resp.xml: <sampleInfo></sampleInfo>).
type entityMetricCSV struct {
	EntityRefId string                              `xml:"entityRefId"`
	SampleInfo  string                              `xml:"sampleInfo"`
	Value       []vsantypes.VsanPerfMetricSeriesCSV `xml:"value,omitempty"`
}

type queryPerfResponse struct {
	Returnval []entityMetricCSV `xml:"returnval"`
}

type queryPerfBody struct {
	Res    *queryPerfResponse `xml:"urn:vsan VsanPerfQueryPerfResponse,omitempty"`
	Fault_ *soap.Fault        `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault,omitempty"`
}

func (b *queryPerfBody) Fault() *soap.Fault { return b.Fault_ }

// VsanPerfQueryPerf answers wildcard and specific entity queries with the
// bucket rule and fault shapes measured live (recon section 5.3).
func (m *PerfManager) VsanPerfQueryPerf(ctx *simulator.Context, req *vsantypes.VsanPerfQueryPerf) soap.HasFault {
	wildcards := 0
	for _, spec := range req.QuerySpecs {
		if strings.HasSuffix(spec.EntityRefId, ":*") {
			wildcards++
		}
	}
	if wildcards > 1 {
		return &queryPerfBody{Fault_: faultWildcardLimit()}
	}
	cl := m.env.Cluster(req.Cluster)
	now := m.env.Now()
	res := &queryPerfResponse{}
	for _, spec := range req.QuerySpecs {
		family, tail, ok := splitRef(spec.EntityRefId)
		f, known := m.env.Catalog.Families[family]
		if !ok || !known || tail == "" {
			return &queryPerfBody{Fault_: faultInvalidRef()}
		}
		switch f.Behaviour {
		case catalog.InvalidArgument:
			return &queryPerfBody{Fault_: faultInvalidRef()}
		case catalog.NotSupported:
			return &queryPerfBody{Fault_: faultNotSupportedPMem()}
		}
		wildcard := tail == "*"
		if wildcard {
			if spec.StartTime == nil || spec.EndTime == nil ||
				spec.EndTime.Sub(*spec.StartTime) > maxWildcardWindow {
				return &queryPerfBody{Fault_: faultWildcardRange(family)}
			}
		}
		if f.Behaviour == catalog.Empty || cl == nil || (!cl.Dedup && strings.Contains(family, "-esa-dedup-")) {
			res.Returnval = append(res.Returnval, entityMetricCSV{EntityRefId: spec.EntityRefId})
			continue
		}
		var start, end time.Time
		if spec.StartTime != nil {
			start = *spec.StartTime
		}
		if spec.EndTime != nil {
			end = *spec.EndTime
		}
		buckets := perf.Buckets(start, end, now)
		refs, err := Entities(m.env.Catalog, cl, family)
		if err != nil {
			return &queryPerfBody{Fault_: simulator.Fault(err.Error(), &vim25types.SystemError{})}
		}
		if !wildcard {
			// a specific entity: only that one, if it exists
			want := spec.EntityRefId
			if _, t, _ := splitRef(want); strings.HasPrefix(t, "/") {
				want = family + ":" + t[1:]
			}
			var keep []string
			for _, r := range refs {
				if r == want {
					keep = append(keep, r)
				}
			}
			refs = keep
		}
		if len(buckets) == 0 || len(refs) == 0 {
			res.Returnval = append(res.Returnval, entityMetricCSV{EntityRefId: spec.EntityRefId})
			continue
		}
		stamps := strings.Join(perf.Format(buckets), ",")
		for _, ref := range refs {
			e := entityMetricCSV{EntityRefId: ref, SampleInfo: stamps}
			for _, label := range f.Labels {
				unit := m.env.Catalog.Unit(family, label)
				vals := make([]string, len(buckets))
				for i, b := range buckets {
					vals[i] = strconv.FormatInt(perf.Value(m.env.Topo.Seed, ref, label, unit, b), 10)
				}
				e.Value = append(e.Value, vsantypes.VsanPerfMetricSeriesCSV{
					MetricId: vsantypes.VsanPerfMetricId{Label: label, MetricsCollectInterval: m.env.Catalog.MetricsCollectInt},
					Values:   strings.Join(vals, ","),
				})
			}
			res.Returnval = append(res.Returnval, e)
		}
	}
	return &queryPerfBody{Res: res}
}
