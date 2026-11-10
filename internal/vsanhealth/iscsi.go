package vsanhealth

import (
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	vim "github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vsan/methods"
	vsan "github.com/vmware/govmomi/vsan/types"
)

type IscsiSystem struct {
	vim.ManagedObjectReference
	env *Env
}

func iscsiDisabled() *soap.Fault {
	f := &vim.VsanFault{}
	f.FaultMessage = []vim.LocalizableMessage{{Key: "com.vmware.vsan.iscsi.msg.notEnabled", Message: "vSAN iSCSI target service is not enabled"}}
	return simulator.Fault("General vSAN error.", f)
}
func (s *IscsiSystem) VsanVitGetIscsiTargets(ctx *simulator.Context, req *vsan.VsanVitGetIscsiTargets) soap.HasFault {
	return &methods.VsanVitGetIscsiTargetsBody{Fault_: iscsiDisabled()}
}
func (s *IscsiSystem) VsanVitGetIscsiLUNs(ctx *simulator.Context, req *vsan.VsanVitGetIscsiLUNs) soap.HasFault {
	return &methods.VsanVitGetIscsiLUNsBody{Fault_: iscsiDisabled()}
}
func (s *IscsiSystem) VsanVitGetIscsiInitiatorGroups(ctx *simulator.Context, req *vsan.VsanVitGetIscsiInitiatorGroups) soap.HasFault {
	return &methods.VsanVitGetIscsiInitiatorGroupsBody{Fault_: iscsiDisabled()}
}
func (s *IscsiSystem) VsanVitGetHomeObject(ctx *simulator.Context, req *vsan.VsanVitGetHomeObject) soap.HasFault {
	return &methods.VsanVitGetHomeObjectBody{Fault_: iscsiDisabled()}
}
func (s *IscsiSystem) VsanVitQueryIscsiTargetServiceVersion(ctx *simulator.Context, req *vsan.VsanVitQueryIscsiTargetServiceVersion) soap.HasFault {
	return &methods.VsanVitQueryIscsiTargetServiceVersionBody{Res: &vsan.VsanVitQueryIscsiTargetServiceVersionResponse{Returnval: s.env.Catalog.About.Version}}
}
func (s *SpaceReportSystem) VsanQueryEntitySpaceUsage(ctx *simulator.Context, req *vsan.VsanQueryEntitySpaceUsage) soap.HasFault {
	return &methods.VsanQueryEntitySpaceUsageBody{Fault_: simulator.Fault("The operation is not supported on the object.", &vim.NotSupported{})}
}
