package vsanhealth

import (
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	vim "github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vsan/methods"
	vsan "github.com/vmware/govmomi/vsan/types"
)

// Operations queries each host's extended vSAN system for the syncing summary.
// Modeled storage is healthy and stable, with no objects awaiting recovery.
type HostSystemEx struct {
	vim.ManagedObjectReference
	env     *Env
	cluster *topology.Cluster
}

func (s *HostSystemEx) VsanQuerySyncingVsanObjects(_ *simulator.Context, req *vsan.VsanQuerySyncingVsanObjects) soap.HasFault {
	if fault := s.env.methodFault("VsanQuerySyncingVsanObjects", s.cluster); fault != nil {
		return &methods.VsanQuerySyncingVsanObjectsBody{Fault_: fault}
	}
	if req.Start < 0 || req.Limit < 0 {
		return &methods.VsanQuerySyncingVsanObjectsBody{Fault_: simulator.Fault("Invalid pagination", &vim.InvalidArgument{})}
	}
	return &syncingBody{Res: &syncingResponse{}}
}

// The SDK marks these counters optional, but Operations uses boxed Longs.
// Emit zeros explicitly, as the live idle-cluster summary does, so Java
// clients receive zero rather than null.
type syncingBody struct {
	Res    *syncingResponse `xml:"urn:vsan VsanQuerySyncingVsanObjectsResponse,omitempty"`
	Fault_ *soap.Fault      `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault,omitempty"`
}

func (b *syncingBody) Fault() *soap.Fault { return b.Fault_ }

type syncingResponse struct {
	Returnval syncingResult `xml:"returnval"`
}
type syncingResult struct {
	TotalObjectsToSync           int64           `xml:"totalObjectsToSync"`
	TotalBytesToSync             int64           `xml:"totalBytesToSync"`
	TotalRecoveryETA             int64           `xml:"totalRecoveryETA"`
	SyncingObjectRecoveryDetails syncingRecovery `xml:"syncingObjectRecoveryDetails"`
}
type syncingRecovery struct {
	ActivelySyncingObjectRecoveryETA int64 `xml:"activelySyncingObjectRecoveryETA"`
	QueuedForSyncObjectRecoveryETA   int64 `xml:"queuedForSyncObjectRecoveryETA"`
	SuspendedObjectRecoveryETA       int64 `xml:"suspendedObjectRecoveryETA"`
	ActiveObjectsToSync              int64 `xml:"activeObjectsToSync"`
	QueuedObjectsToSync              int64 `xml:"queuedObjectsToSync"`
	SuspendedObjectsToSync           int64 `xml:"suspendedObjectsToSync"`
	BytesToSyncForActiveObjects      int64 `xml:"bytesToSyncForActiveObjects"`
	BytesToSyncForQueuedObjects      int64 `xml:"bytesToSyncForQueuedObjects"`
	BytesToSyncForSuspendedObjects   int64 `xml:"bytesToSyncForSuspendedObjects"`
}
