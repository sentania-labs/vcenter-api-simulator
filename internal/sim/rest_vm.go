package sim

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"
)

func (s *restSessions) vmInventory(w http.ResponseWriter, path string) bool {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return false
	}
	vm, ok := s.registry.Get(types.ManagedObjectReference{Type: "VirtualMachine", Value: parts[1]}).(*simulator.VirtualMachine)
	if !ok {
		return false
	}
	var body any
	s.lock(vm, func() { body = restVMBody(vm, strings.Join(parts[2:], "/")) })
	if body == nil {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
	return true
}

// restVMBody must be called with the VM's lock held. It returns nil for an
// unknown sub-resource.
func restVMBody(vm *simulator.VirtualMachine, sub string) any {
	if vm.Config == nil {
		return nil
	}
	hardware := map[string]any{"upgrade_policy": "NEVER", "upgrade_status": "NONE", "version": strings.ToUpper(strings.ReplaceAll(vm.Config.Version, "-", "_"))}
	disks := map[string]any{}
	diskList := make([]map[string]string, 0)
	nics := map[string]any{}
	controllers := map[string]any{}
	for _, raw := range vm.Config.Hardware.Device {
		dev := raw.GetVirtualDevice()
		key := fmt.Sprint(dev.Key)
		switch d := raw.(type) {
		case *types.VirtualDisk:
			diskList = append(diskList, map[string]string{"disk": key})
			row := map[string]any{"type": "SCSI", "capacity": d.CapacityInKB * 1024, "label": fmt.Sprintf("Hard disk %d", len(disks)+1)}
			if d.UnitNumber != nil {
				row["scsi"] = map[string]any{"bus": 0, "unit": *d.UnitNumber}
			}
			if b, ok := d.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
				row["backing"] = map[string]any{"type": "VMDK_FILE", "vmdk_file": b.FileName}
			}
			disks[key] = row
		case types.BaseVirtualSCSIController:
			c := d.GetVirtualSCSIController()
			controllers[key] = map[string]any{"label": fmt.Sprintf("SCSI controller %d", c.BusNumber), "type": "PVSCSI", "sharing": "NONE", "scsi": map[string]any{"bus": c.BusNumber, "unit": c.ScsiCtlrUnitNumber}}
		case types.BaseVirtualEthernetCard:
			card := d.GetVirtualEthernetCard()
			row := map[string]any{"label": fmt.Sprintf("Network adapter %d", len(nics)+1), "mac_address": card.MacAddress, "mac_type": strings.ToUpper(card.AddressType), "type": "VMXNET3"}
			if b, ok := card.Backing.(*types.VirtualEthernetCardDistributedVirtualPortBackingInfo); ok {
				row["backing"] = map[string]any{"type": "DISTRIBUTED_PORTGROUP", "network": b.Port.PortgroupKey, "distributed_switch_uuid": b.Port.SwitchUuid}
			}
			if card.Connectable != nil {
				row["start_connected"] = card.Connectable.StartConnected
				row["allow_guest_control"] = card.Connectable.AllowGuestControl
				if card.Connectable.Connected {
					row["state"] = "CONNECTED"
				} else {
					row["state"] = "NOT_CONNECTED"
				}
			}
			nics[key] = row
		}
	}
	switch sub {
	case "hardware":
		return hardware
	case "hardware/disk":
		return diskList
	case "guest/identity":
		if vm.Guest == nil {
			return nil
		}
		return map[string]any{"name": "OTHER", "family": "OTHER", "full_name": map[string]any{"id": "vmsg.guestos.otherGuest.label", "default_message": vm.Guest.GuestFullName, "args": []string{}}, "host_name": vm.Guest.HostName, "ip_address": vm.Guest.IpAddress}
	case "":
		return map[string]any{"name": vm.Name, "identity": map[string]any{"name": vm.Name, "instance_uuid": vm.Config.InstanceUuid, "bios_uuid": vm.Config.Uuid}, "power_state": restPowerState(string(vm.Runtime.PowerState)), "instant_clone_frozen": false, "hardware": hardware, "guest_OS": "OTHER", "cpu": map[string]any{"count": vm.Config.Hardware.NumCPU, "cores_per_socket": vm.Config.Hardware.NumCoresPerSocket, "hot_add_enabled": false, "hot_remove_enabled": false}, "memory": map[string]any{"size_MiB": vm.Config.Hardware.MemoryMB, "hot_add_enabled": false, "hot_add_increment_size_MiB": 0, "hot_add_limit_MiB": vm.Config.Hardware.MemoryMB}, "disks": disks, "nics": nics, "scsi_adapters": controllers, "cdroms": map[string]any{}, "floppies": map[string]any{}, "parallel_ports": map[string]any{}, "serial_ports": map[string]any{}, "sata_adapters": map[string]any{}, "nvme_adapters": map[string]any{}, "boot": map[string]any{"type": "BIOS", "delay": 0, "retry": false, "retry_delay": 0, "enter_setup_mode": false, "efi_legacy_boot": false, "network_protocol": "IPV4"}, "boot_devices": []any{}}
	default:
		return nil
	}
}
