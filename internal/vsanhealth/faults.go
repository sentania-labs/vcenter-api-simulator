package vsanhealth

import (
	"fmt"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	vim25types "github.com/vmware/govmomi/vim25/types"
)

// Fault shapes, each one as captured from vCenter 9.1.1 in
// fixtures/live/9.1.1 (file named per case). The management pack treats
// InvalidArgument and NotSupported as "feature absent" and any other fault as
// a collection problem, so the types here are load-bearing.

// faultInvalidRef: edge-domain-c6004-bogus_family and -no_wildcard_no_uuid,
// and the nine advertised-but-rejected families (perf-*-clom-disk etc.).
func faultInvalidRef() *soap.Fault {
	return simulator.Fault("A specified parameter was not correct: entityRefId",
		&vim25types.InvalidArgument{InvalidProperty: "entityRefId"})
}

// faultWildcardRange: edge-domain-c6004-no_times.
func faultWildcardRange(family string) *soap.Fault {
	f := &vim25types.InvalidArgument{}
	f.FaultMessage = []vim25types.LocalizableMessage{{
		Key: "com.vmware.vsan.perfsvc.fault.queryperf.wildcardqueryrange",
		Arg: []vim25types.KeyAnyValue{
			{Key: "entityType", Value: family},
			{Key: "duration", Value: int32(12)},
		},
		Message: fmt.Sprintf("Invalid Argument. For wildcard query on entity type %s, the startTime and endTime must be specified and the duration between startTime and endTime should be no more than 12 hours.", family),
	}}
	return simulator.Fault("A specified parameter was not correct: ", f)
}

// faultWildcardLimit: edge-domain-c6004-two_specs.
func faultWildcardLimit() *soap.Fault {
	f := &vim25types.InvalidArgument{}
	f.FaultMessage = []vim25types.LocalizableMessage{{
		Key:     "com.vmware.vsan.perfsvc.fault.queryperf.wildcardlimit",
		Message: "Invalid Argument. Only one wildcard query allowed in query specs.",
	}}
	return simulator.Fault("A specified parameter was not correct: ", f)
}

// faultNotSupportedPMem: perf-domain-c6004-cluster-pmem and -host-pmem.
func faultNotSupportedPMem() *soap.Fault {
	f := &vim25types.NotSupported{}
	f.FaultMessage = []vim25types.LocalizableMessage{{
		Key:     "com.vmware.vsan.perfsvc.fault.queryperf.pmemisunavailable",
		Message: "You can't query PMem performance data for this cluster, because no PMem storage is monitored by vSAN in this cluster.",
	}}
	return simulator.Fault("The operation is not supported on the object.", f)
}
