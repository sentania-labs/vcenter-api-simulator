package sim

import (
	"fmt"
	"reflect"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// ESXi's template contains references to managers that vcsim never creates.
// Give each host its own typed objects, preserving the managers vcsim owns.
func installHostManagers(ctx *simulator.Context, host *simulator.HostSystem) error {
	managers := reflect.ValueOf(&host.ConfigManager).Elem()
	for i := 0; i < managers.NumField(); i++ {
		field := managers.Field(i)
		ref, ok := field.Interface().(*types.ManagedObjectReference)
		if !ok || ref == nil || ctx.Map.Get(*ref) != nil {
			continue
		}
		own := types.ManagedObjectReference{Type: ref.Type, Value: host.Self.Value + "-" + managers.Type().Field(i).Name}
		if !mo.IsManagedObjectType(own.Type) {
			return fmt.Errorf("unknown host manager type %q", own.Type)
		}
		value, err := mo.ObjectContentToType(types.ObjectContent{Obj: own}, true)
		if err != nil {
			return err
		}
		obj, ok := value.(mo.Reference)
		if !ok {
			return fmt.Errorf("host manager %q has no reference", own.Type)
		}
		switch m := obj.(type) {
		case *mo.HostImageConfigManager:
			obj = &hostImageManager{HostImageConfigManager: *m}
		case *mo.HostMemorySystem:
			m.ConsoleReservationInfo = &types.ServiceConsoleReservationInfo{Unreserved: host.Hardware.MemorySize}
			m.VirtualMachineReservationInfo = host.Config.VirtualMachineReservation
		case *mo.HostDateTimeSystem:
			if host.Config.DateTimeInfo != nil {
				m.DateTimeInfo = *host.Config.DateTimeInfo
			}
		case *mo.HostServiceSystem:
			if host.Config.Service != nil {
				m.ServiceInfo = *host.Config.Service
			}
		}
		ctx.Map.Put(obj)
		field.Set(reflect.ValueOf(&own))
	}
	return nil
}

// The adapter follows configManager.imageConfigManager to this read.
type hostImageManager struct{ mo.HostImageConfigManager }

func (m *hostImageManager) HostImageConfigGetAcceptance(_ *simulator.Context, _ *types.HostImageConfigGetAcceptance) soap.HasFault {
	return &methods.HostImageConfigGetAcceptanceBody{Res: &types.HostImageConfigGetAcceptanceResponse{Returnval: string(types.HostImageAcceptanceLevelVmware_accepted)}}
}
