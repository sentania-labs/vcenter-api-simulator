package sim

import (
	"github.com/vmware/govmomi/vim25/types"
)

// Host alarm defaults read from lab vCenter 9.1.1, 2026-11-17.
// Only definitions were read; no alarm or infrastructure state was changed.
var activityHostAlarms = []types.AlarmSpec{
	{Name: "vSphere HA host status", Description: "Default alarm to monitor health of a host as reported by vSphere HA", Enabled: true, Expression: &types.OrAlarmExpression{Expression: []types.BaseAlarmExpression{&types.EventAlarmExpression{EventType: "vim.event.EventEx", EventTypeId: "com.vmware.vc.HA.HostAgentErrorEvent", ObjectType: "vim.HostSystem", Status: "red"}, &types.EventAlarmExpression{EventType: "vim.event.EventEx", EventTypeId: "com.vmware.vc.HA.DasHostIsolatedEvent", ObjectType: "vim.HostSystem", Status: "red"}, &types.EventAlarmExpression{EventType: "vim.event.EventEx", EventTypeId: "com.vmware.vc.HA.HostPartitionedFromMasterEvent", ObjectType: "vim.HostSystem", Status: "red"}, &types.EventAlarmExpression{EventType: "vim.event.EventEx", EventTypeId: "com.vmware.vc.HA.DasHostFailedEvent", ObjectType: "vim.HostSystem", Status: "red"}, &types.EventAlarmExpression{EventType: "vim.event.HostNoHAEnabledPortGroupsEvent", EventTypeId: "vim.event.HostNoHAEnabledPortGroupsEvent", ObjectType: "vim.HostSystem", Status: "red"}, &types.EventAlarmExpression{EventType: "vim.event.EventEx", EventTypeId: "com.vmware.vc.HA.HostDasAgentHealthyEvent", ObjectType: "vim.HostSystem", Status: "green"}, &types.EventAlarmExpression{EventType: "vim.event.HostDasDisabledEvent", EventTypeId: "vim.event.HostDasDisabledEvent", ObjectType: "vim.HostSystem", Status: "green"}}}, Setting: &types.AlarmSetting{ToleranceRange: 0, ReportingFrequency: 300}},
	{Name: "Host connection and power state", Description: "Default alarm to monitor host connection and power state", Enabled: true, Expression: &types.AndAlarmExpression{Expression: []types.BaseAlarmExpression{&types.StateAlarmExpression{Operator: "isEqual", Type: "vim.HostSystem", StatePath: "runtime.connectionState", Red: "notResponding"}, &types.StateAlarmExpression{Operator: "isUnequal", Type: "vim.HostSystem", StatePath: "runtime.powerState", Red: "standBy"}}}, Setting: &types.AlarmSetting{ToleranceRange: 0, ReportingFrequency: 300}},
}
