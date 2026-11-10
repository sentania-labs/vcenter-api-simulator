package vsanhealth

import (
	"reflect"

	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	vim "github.com/vmware/govmomi/vim25/types"
	vsan "github.com/vmware/govmomi/vsan/types"
)

// govmomi does not yet define this vSAN method.
type VsanClusterGetClaimedCapacity struct {
	This    vim.ManagedObjectReference `xml:"_this"`
	Cluster vim.ManagedObjectReference `xml:"cluster"`
}

func init() {
	vim.Add("vsan:VsanClusterGetClaimedCapacity", reflect.TypeOf(VsanClusterGetClaimedCapacity{}))
}

type claimedResponse struct {
	Returnval int64 `xml:"returnval"`
}
type claimedBody struct {
	Res    *claimedResponse `xml:"urn:vsan VsanClusterGetClaimedCapacityResponse,omitempty"`
	Fault_ *soap.Fault      `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault,omitempty"`
}

func (b *claimedBody) Fault() *soap.Fault { return b.Fault_ }

func (s *ClusterConfigSystem) VsanClusterGetClaimedCapacity(ctx *simulator.Context, req *VsanClusterGetClaimedCapacity) soap.HasFault {
	cl := s.env.Cluster(&req.Cluster)
	if cl == nil {
		return &claimedBody{Fault_: simulator.Fault("cluster not found", &vim.ManagedObjectNotFound{Obj: req.Cluster})}
	}
	return &claimedBody{Res: &claimedResponse{Returnval: topology.VsanCapacity(cl)}}
}

type SpaceReportSystem struct {
	vim.ManagedObjectReference
	env *Env
}

type spaceSummary struct {
	ObjType        string `xml:"objType,omitempty"`
	Overhead       int64  `xml:"overheadB"`
	Temporary      int64  `xml:"temporaryOverheadB"`
	Primary        int64  `xml:"primaryCapacityB"`
	Provision      int64  `xml:"provisionCapacityB,omitempty"`
	Reserved       int64  `xml:"reservedCapacityB"`
	OverReserved   int64  `xml:"overReservedB"`
	PhysicalUsed   int64  `xml:"physicalUsedB"`
	Used           int64  `xml:"usedB"`
	ObjTypeExt     string `xml:"objTypeExt,omitempty"`
	ObjTypeExtDesc string `xml:"objTypeExtDesc,omitempty"`
	Snapshot       int64  `xml:"snapshotUsedB"`
}
type spaceDetail struct {
	Items []spaceSummary `xml:"spaceUsageByObjectType"`
}
type spaceUsage struct {
	Total       int64                    `xml:"totalCapacityB"`
	Free        int64                    `xml:"freeCapacityB"`
	Overview    spaceSummary             `xml:"spaceOverview"`
	Detail      spaceDetail              `xml:"spaceDetail"`
	Uncommitted int64                    `xml:"uncommittedB"`
	Threshold   vsan.VsanHealthThreshold `xml:"capacityHealthThreshold"`
}
type spaceResponse struct {
	Returnval spaceUsage `xml:"returnval"`
}
type spaceBody struct {
	Res    *spaceResponse `xml:"urn:vsan VsanQuerySpaceUsageResponse,omitempty"`
	Fault_ *soap.Fault    `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault,omitempty"`
}

func (b *spaceBody) Fault() *soap.Fault { return b.Fault_ }
func (s *SpaceReportSystem) VsanQuerySpaceUsage(ctx *simulator.Context, req *vsan.VsanQuerySpaceUsage) soap.HasFault {
	cl := s.env.Cluster(&req.Cluster)
	if cl == nil {
		return &spaceBody{Fault_: simulator.Fault("cluster not found", &vim.ManagedObjectNotFound{Obj: req.Cluster})}
	}
	total, used := topology.VsanCapacity(cl), topology.VsanUsed(cl)
	summary := spaceSummary{Primary: used, Provision: used, PhysicalUsed: used, Used: used}
	detail := spaceDetail{Items: []spaceSummary{
		{ObjType: "statsdb"}, {ObjType: "traceobject"},
		{ObjType: "checksumOverhead"}, {ObjType: "fileSystemOverhead"},
		{ObjType: "vm", Primary: used, PhysicalUsed: used, Used: used},
	}}
	return &spaceBody{Res: &spaceResponse{Returnval: spaceUsage{Total: total, Free: total - used, Overview: summary, Detail: detail, Threshold: vsan.VsanHealthThreshold{YellowValue: total / 10 * 7, RedValue: total / 10 * 9}}}}
}

type efficiencyState struct {
	LogicalCapacity      int64              `xml:"logicalCapacity"`
	LogicalUsed          int64              `xml:"logicalCapacityUsed"`
	PhysicalCapacity     int64              `xml:"physicalCapacity"`
	PhysicalUsed         int64              `xml:"physicalCapacityUsed"`
	DedupMetadata        int64              `xml:"dedupMetadataSize"`
	Metadata             efficiencyMetadata `xml:"spaceEfficiencyMetadataSize"`
	EsaDedup             int64              `xml:"esaDedupSpaceSaving"`
	EsaCompression       int64              `xml:"esaCompressionSpaceSaving"`
	TotalWithoutOverhead int64              `xml:"totalSpaceUsedWithoutOverhead"`
}
type efficiencyMetadata struct {
	Dedup       int64 `xml:"dedupMetadataSize"`
	Compression int64 `xml:"compressionMetadataSize"`
}
type efficiencyResponse struct {
	Returnval efficiencyState `xml:"returnval"`
}
type efficiencyBody struct {
	Res    *efficiencyResponse `xml:"urn:vsan QueryClusterDataEfficiencyCapacityStateResponse,omitempty"`
	Fault_ *soap.Fault         `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault,omitempty"`
}

func (b *efficiencyBody) Fault() *soap.Fault { return b.Fault_ }
func (s *DiskManagementSystem) QueryClusterDataEfficiencyCapacityState(ctx *simulator.Context, req *vsan.QueryClusterDataEfficiencyCapacityState) soap.HasFault {
	cl := s.env.Cluster(&req.Cluster)
	if cl == nil {
		return &efficiencyBody{Fault_: simulator.Fault("cluster not found", &vim.ManagedObjectNotFound{Obj: req.Cluster})}
	}
	return &efficiencyBody{Res: &efficiencyResponse{Returnval: efficiencyState{PhysicalCapacity: topology.VsanCapacity(cl), PhysicalUsed: topology.VsanUsed(cl)}}}
}
