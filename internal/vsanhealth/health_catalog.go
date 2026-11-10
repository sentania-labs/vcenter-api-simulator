package vsanhealth

import (
	_ "embed"
	"encoding/json"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	vim "github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vsan/methods"
	vsan "github.com/vmware/govmomi/vsan/types"
)

//go:embed fixtures/health-config.json
var healthConfigJSON []byte

//go:embed fixtures/health-checks.json
var healthChecksJSON []byte

func (s *ClusterHealthSystem) VsanQueryAllSupportedHealthChecks(ctx *simulator.Context, req *vsan.VsanQueryAllSupportedHealthChecks) soap.HasFault {
	res := &vsan.VsanQueryAllSupportedHealthChecksResponse{}
	if err := json.Unmarshal(healthChecksJSON, &res.Returnval); err != nil {
		return &methods.VsanQueryAllSupportedHealthChecksBody{Fault_: simulator.Fault(err.Error(), &vim.SystemError{})}
	}
	return &methods.VsanQueryAllSupportedHealthChecksBody{Res: res}
}
func (s *ClusterHealthSystem) VsanHealthQueryVsanClusterHealthConfig(ctx *simulator.Context, req *vsan.VsanHealthQueryVsanClusterHealthConfig) soap.HasFault {
	res := &vsan.VsanHealthQueryVsanClusterHealthConfigResponse{}
	if err := json.Unmarshal(healthConfigJSON, &res.Returnval); err != nil {
		return &methods.VsanHealthQueryVsanClusterHealthConfigBody{Fault_: simulator.Fault(err.Error(), &vim.SystemError{})}
	}
	return &methods.VsanHealthQueryVsanClusterHealthConfigBody{Res: res}
}
func (s *ClusterHealthSystem) VsanHealthGetVsanClusterSilentChecks(ctx *simulator.Context, req *vsan.VsanHealthGetVsanClusterSilentChecks) soap.HasFault {
	return &methods.VsanHealthGetVsanClusterSilentChecksBody{Res: &vsan.VsanHealthGetVsanClusterSilentChecksResponse{}}
}
func (s *ClusterHealthSystem) VsanHealthQueryVsanClusterHealthCheckInterval(ctx *simulator.Context, req *vsan.VsanHealthQueryVsanClusterHealthCheckInterval) soap.HasFault {
	return &methods.VsanHealthQueryVsanClusterHealthCheckIntervalBody{Res: &vsan.VsanHealthQueryVsanClusterHealthCheckIntervalResponse{Returnval: 60}}
}
func (s *ClusterHealthSystem) VsanHealthQueryVsanProxyConfig(ctx *simulator.Context, req *vsan.VsanHealthQueryVsanProxyConfig) soap.HasFault {
	return &methods.VsanHealthQueryVsanProxyConfigBody{Res: &vsan.VsanHealthQueryVsanProxyConfigResponse{Returnval: vsan.VsanClusterTelemetryProxyConfig{Host: "localhost", Port: 1082, AutoDiscovered: vim.NewBool(true)}}}
}
func (s *ClusterHealthSystem) VsanHealthIsRebalanceRunning(ctx *simulator.Context, req *vsan.VsanHealthIsRebalanceRunning) soap.HasFault {
	return &methods.VsanHealthIsRebalanceRunningBody{Res: &vsan.VsanHealthIsRebalanceRunningResponse{Returnval: false}}
}
