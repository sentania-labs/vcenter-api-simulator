package vsanhealth

import (
	_ "embed"
	"encoding/json"
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
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// The schema is service-wide: the OSA walk and the ESA capture advertise
// the same graphs and metric labels. These two supplementary catalogs come
// from the sanitized OSA walk.
//
//go:embed fixtures/aggregate.json
var aggregateSchema []byte

//go:embed fixtures/diagnostics.json
var diagnosticSchema []byte

func (m *PerfManager) VsanPerfGetAggregatedEntityTypes(ctx *simulator.Context, req *vsantypes.VsanPerfGetAggregatedEntityTypes) soap.HasFault {
	res := &vsantypes.VsanPerfGetAggregatedEntityTypesResponse{}
	if err := json.Unmarshal(aggregateSchema, &res.Returnval); err != nil {
		return &methods.VsanPerfGetAggregatedEntityTypesBody{Fault_: simulator.Fault(err.Error(), &vim25types.SystemError{})}
	}
	return &methods.VsanPerfGetAggregatedEntityTypesBody{Res: res}
}

func (m *PerfManager) VsanPerfGetSupportedDiagnosticExceptions(ctx *simulator.Context, req *vsantypes.VsanPerfGetSupportedDiagnosticExceptions) soap.HasFault {
	res := &vsantypes.VsanPerfGetSupportedDiagnosticExceptionsResponse{}
	if err := json.Unmarshal(diagnosticSchema, &res.Returnval); err != nil {
		return &methods.VsanPerfGetSupportedDiagnosticExceptionsBody{Fault_: simulator.Fault(err.Error(), &vim25types.SystemError{})}
	}
	return &methods.VsanPerfGetSupportedDiagnosticExceptionsBody{Res: res}
}

// maxWildcardWindow is the live limit on a wildcard query's window.
const maxWildcardWindow = 12 * time.Hour

// PerfManager is VsanPerformanceManager:vsan-performance-manager.
type PerfManager struct {
	vim25types.ManagedObjectReference
	env *Env
}

func (m *PerfManager) VsanPerfQueryStatsObjectInformation(ctx *simulator.Context, req *vsantypes.VsanPerfQueryStatsObjectInformation) soap.HasFault {
	info := vsantypes.VsanObjectInformation{DirectoryName: "unknown"}
	cl := m.env.Cluster(req.Cluster)
	if cl != nil && (cl.Spec.StatsPrimary == nil || *cl.Spec.StatsPrimary) {
		info.DirectoryName = ".vsan.stats"
		info.VsanObjectUuid = cl.StatsUUID
		info.VsanHealth = "healthy"
		for _, kv := range [][2]string{{"hostFailuresToTolerate", "1"}, {"cacheReservation", "0"}, {"stripeWidth", "1"}, {"proportionalCapacity", "0"}, {"forceProvisioning", "0"}, {"iopsLimit", "0"}, {"checksumDisabled", "0"}, {"subFailuresToTolerate", "0"}} {
			info.PolicyAttributes = append(info.PolicyAttributes, vim25types.KeyValue{Key: kv[0], Value: kv[1]})
		}
		info.PolicyAttributes = append(info.PolicyAttributes,
			vim25types.KeyValue{Key: "spbmProfileGenerationNumber", Value: "0"},
			vim25types.KeyValue{Key: "spbmProfileId", Value: cl.PolicyUUID},
			vim25types.KeyValue{Key: "spbmProfileName", Value: "vSAN Default Storage Policy"},
			vim25types.KeyValue{Key: "compressionDisabled", Value: "0"})
		info.SpbmProfileUuid, info.SpbmProfileGenerationId = cl.PolicyUUID, "0"
		now := m.env.Now().UTC()
		info.SpbmComplianceResult = &vsantypes.VsanStorageComplianceResult{CheckTime: &now, Profile: cl.PolicyUUID, ObjectUUID: cl.StatsUUID, ComplianceStatus: "compliant", OperationalStatus: &vsantypes.VsanStorageOperationalStatus{Healthy: vim25types.NewBool(true)}, ObjPolicyGenerationId: "0"}
	}
	return &methods.VsanPerfQueryStatsObjectInformationBody{Res: &vsantypes.VsanPerfQueryStatsObjectInformationResponse{Returnval: info}}
}

func (m *PerfManager) VsanPerfQueryTimeRanges(ctx *simulator.Context, req *vsantypes.VsanPerfQueryTimeRanges) soap.HasFault {
	cl := m.env.Cluster(req.Cluster)
	if cl != nil && cl.Spec.StatsPrimary != nil && !*cl.Spec.StatsPrimary {
		return &methods.VsanPerfQueryTimeRangesBody{Fault_: faultStatsPrimaryMissing()}
	}
	return &methods.VsanPerfQueryTimeRangesBody{Res: &vsantypes.VsanPerfQueryTimeRangesResponse{}}
}

func (m *PerfManager) VsanPerfQueryClusterHealth(ctx *simulator.Context, req *vsantypes.VsanPerfQueryClusterHealth) soap.HasFault {
	cl := m.env.Cluster(&req.Cluster)
	primary := cl == nil || cl.Spec.StatsPrimary == nil || *cl.Spec.StatsPrimary
	group, err := m.perfHealthGroup(cl, primary)
	if err != nil {
		return &methods.VsanPerfQueryClusterHealthBody{Fault_: simulator.Fault(err.Error(), &vim25types.SystemError{})}
	}
	return &methods.VsanPerfQueryClusterHealthBody{Res: &vsantypes.VsanPerfQueryClusterHealthResponse{Returnval: []vim25types.BaseDynamicData{group}}}
}

// VsanPerfQueryNodeInformation lists the cluster's hosts as the Performance
// Service sees them (fixture nodeinfo-domain-c6004): the first host is the
// CMMDS and stats master and carries masterInfo.
func (m *PerfManager) VsanPerfQueryNodeInformation(ctx *simulator.Context, req *vsantypes.VsanPerfQueryNodeInformation) soap.HasFault {
	res := &vsantypes.VsanPerfQueryNodeInformationResponse{}
	cl := m.env.Cluster(req.Cluster)
	if fault := m.env.methodFault("VsanPerfQueryNodeInformation", cl); fault != nil {
		return &methods.VsanPerfQueryNodeInformationBody{Fault_: fault}
	}
	if cl != nil && len(cl.Hosts) > 0 {
		primary := cl.Spec.StatsPrimary == nil || *cl.Spec.StatsPrimary
		master := cl.Hosts[0].NodeUUID
		now := m.env.Now().UTC()
		sinceCollect := int64(now.Sub(now.Truncate(perf.Interval)) / time.Second)
		for i, h := range cl.Hosts {
			n := vsantypes.VsanPerfNodeInformation{
				Version:        m.env.Catalog.About.EsxiVersion,
				Hostname:       h.Name,
				IsCmmdsMaster:  i == 0,
				IsStatsMaster:  i == 0 && primary,
				VsanMasterUuid: master,
				VsanNodeUuid:   h.NodeUUID,
				DiagnosticMode: vim25types.NewBool(false),
			}
			if i == 0 && primary {
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
	cl := m.env.Cluster(req.Cluster)
	if cl != nil && cl.Spec.StatsPrimary != nil && !*cl.Spec.StatsPrimary {
		return &queryPerfBody{Fault_: faultStatsPrimaryMissing()}
	}
	wildcards := 0
	for _, spec := range req.QuerySpecs {
		if strings.HasSuffix(spec.EntityRefId, ":*") {
			wildcards++
		}
	}
	if wildcards > 1 {
		return &queryPerfBody{Fault_: faultWildcardLimit()}
	}
	now := m.env.Now()
	res := &queryPerfResponse{}
	for _, spec := range req.QuerySpecs {
		family, tail, ok := splitRef(spec.EntityRefId)
		f, known := EffectiveFamily(m.env.Catalog, cl, family)
		var osa osaFamily
		osaData := false
		if cl != nil && cl.Spec.Architecture == "osa" {
			osa, osaData = osaFamilyFor(family)
		}
		if !ok || !known || tail == "" {
			return &queryPerfBody{Fault_: faultInvalidRef()}
		}
		// an injected fault (latency and timeout holds ran before dispatch)
		if cl != nil {
			if inj := m.env.Knobs.FamilyFault(family, cl.Key); inj != nil {
				if fault := m.env.Knobs.Fault(inj, family); fault != nil {
					return &queryPerfBody{Fault_: fault}
				}
			}
		}
		switch f.Behaviour {
		case catalog.InvalidArgument:
			if cl != nil && cl.Spec.Architecture == "osa" && family == "computeCluster-remotedomclient" {
				return &queryPerfBody{Fault_: faultInvalidProperty(spec.EntityRefId)}
			}
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
		if f.Behaviour == catalog.Empty || cl == nil {
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
		refs, err := PerfEntities(m.env.Catalog, cl, family)
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
				st, ok := m.env.Catalog.LabelStats(family, label)
				if osaData {
					st, ok = osa.Stats[label]
				}
				vals := make([]string, len(buckets))
				for i, b := range buckets {
					switch {
					case osaData && osaValueIsNone(osa, label):
						vals[i] = "None"
					case osaData && family == "vsan-cluster-capacity" && label == "total":
						vals[i] = strconv.FormatInt(topology.VsanCapacity(cl), 10)
					case osaData && family == "vsan-cluster-capacity" && label == "used":
						vals[i] = strconv.FormatInt(topology.VsanUsed(cl), 10)
					case osaData && family == "vsan-cluster-capacity" && label == "free":
						vals[i] = strconv.FormatInt(topology.VsanCapacity(cl)-topology.VsanUsed(cl), 10)
					default:
						vals[i] = perf.Value(cl.TelemetrySeed, ref, label, st, ok, unit, b)
					}
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
