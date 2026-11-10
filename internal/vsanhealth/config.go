package vsanhealth

import (
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	vim25types "github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vsan/methods"
	vsantypes "github.com/vmware/govmomi/vsan/types"
)

// ClusterConfigSystem replaces vcsim's VsanVcClusterConfigSystem so the
// cluster uuid on /vsanHealth is the one /sdk reports in
// configurationEx.vsanConfigInfo.defaultConfig.uuid (recon risk 1).
type ClusterConfigSystem struct {
	vim25types.ManagedObjectReference
	env *Env
}

// The OSA response carries the newer data-efficiency subtype, whose
// dedupStoreUuid is not represented by govmomi's base type.
type osaDataEfficiency struct {
	DedupEnabled   bool   `xml:"dedupEnabled"`
	DedupStoreUUID string `xml:"dedupStoreUuid"`
}

type osaConfig struct {
	vsantypes.VsanConfigInfoEx
	DataEfficiency osaDataEfficiency `xml:"dataEfficiencyConfig"`
}

type osaConfigResponse struct {
	Returnval osaConfig `xml:"returnval"`
}
type osaConfigBody struct {
	Res *osaConfigResponse `xml:"urn:vsan VsanClusterGetConfigResponse,omitempty"`
}

func (*osaConfigBody) Fault() *soap.Fault { return nil }

// VsanClusterGetConfig returns the ESA cluster configuration with the fields
// the capture shows (clusterconfig-domain-c6004.resp.xml) that govmomi's
// VsanConfigInfoEx can carry.
func (s *ClusterConfigSystem) VsanClusterGetConfig(ctx *simulator.Context, req *vsantypes.VsanClusterGetConfig) soap.HasFault {
	cl := s.env.Cluster(&req.Cluster)
	if fault := s.env.methodFault("VsanClusterGetConfig", cl); fault != nil {
		return &methods.VsanClusterGetConfigBody{Fault_: fault}
	}
	info := vsantypes.VsanConfigInfoEx{}
	if cl == nil {
		info.Enabled = vim25types.NewBool(false)
	} else {
		info.Enabled = vim25types.NewBool(true)
		info.DefaultConfig = &vim25types.VsanClusterConfigInfoHostDefaultInfo{Uuid: cl.VsanUUID}
		info.VsanEsaEnabled = vim25types.NewBool(cl.Spec.Architecture != "osa")
		info.DataEfficiencyConfig = &vsantypes.VsanDataEfficiencyConfig{
			DedupEnabled:       cl.Dedup,
			CompressionEnabled: vim25types.NewBool(cl.Compression),
		}
		info.ResyncIopsLimitConfig = &vsantypes.ResyncIopsInfo{ResyncIops: 0}
		info.IscsiConfig = &vsantypes.VsanIscsiTargetServiceConfig{Enabled: vim25types.NewBool(false)}
		info.DataEncryptionConfig = &vsantypes.VsanDataEncryptionConfig{EncryptionEnabled: false}
		info.ExtendedConfig = &vsantypes.VsanExtendedConfig{
			ObjectRepairTimer:          60,
			DisableSiteReadLocality:    vim25types.NewBool(false),
			EnableCustomizedSwapObject: vim25types.NewBool(true),
			LargeScaleClusterSupport:   vim25types.NewBool(false),
			ProactiveRebalanceInfo:     &vsantypes.VsanProactiveRebalanceInfo{Enabled: vim25types.NewBool(true), Threshold: 30},
			CapacityReservationInfo:    &vsantypes.VsanCapacityReservationInfo{HostRebuildThreshold: "Reported", VsanOpSpaceThreshold: "Enforced"},
		}
		info.PerfsvcConfig = &vsantypes.VsanPerfsvcConfig{Enabled: true}
		info.UnmapConfig = &vsantypes.VsanUnmapConfig{Enable: true}
		info.VumConfig = &vsantypes.VsanVumConfig{BaselinePreferenceType: "latestRelease"}
		info.FileServiceConfig = &vsantypes.VsanFileServiceConfig{Enabled: false}
		info.DataInTransitEncryptionConfig = &vsantypes.VsanDataInTransitEncryptionConfig{}
	}
	if cl != nil && cl.Spec.Architecture == "osa" {
		return &osaConfigBody{Res: &osaConfigResponse{Returnval: osaConfig{VsanConfigInfoEx: info}}}
	}
	return &methods.VsanClusterGetConfigBody{Res: &vsantypes.VsanClusterGetConfigResponse{Returnval: info}}
}
