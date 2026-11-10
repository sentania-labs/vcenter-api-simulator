package sim

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// inventory reads the same object registry as the SOAP service.
func (s *restSessions) inventory(w http.ResponseWriter, r *http.Request) bool {
	p := strings.TrimPrefix(r.URL.Path, "/api/vcenter/")
	if p == r.URL.Path {
		return false
	}
	kinds := map[string]string{"datacenter": "Datacenter", "cluster": "ClusterComputeResource", "host": "HostSystem", "datastore": "Datastore", "network": "Network", "vm": "VirtualMachine", "folder": "Folder", "resource-pool": "ResourcePool"}
	kind, ok := kinds[p]
	if !ok {
		if strings.HasPrefix(p, "vm/") && s.vmInventory(w, p) {
			return true
		}
		parts := strings.Split(p, "/")
		if len(parts) != 2 || (parts[0] != "cluster" && parts[0] != "datastore") {
			return false
		}
		kind = kinds[parts[0]]
		obj := s.registry.Get(types.ManagedObjectReference{Type: kind, Value: parts[1]})
		var row map[string]any
		switch o := obj.(type) {
		case *simulator.ClusterComputeResource:
			row = map[string]any{"name": o.Name}
			if o.ResourcePool != nil {
				row["resource_pool"] = o.ResourcePool.Value
			}
		case *simulator.Datastore:
			multiple := o.Summary.MultipleHostAccess != nil && *o.Summary.MultipleHostAccess
			row = map[string]any{"name": o.Name, "type": strings.ToUpper(o.Summary.Type), "free_space": o.Summary.FreeSpace, "accessible": o.Summary.Accessible, "multiple_host_access": multiple, "thin_provisioning_supported": o.Capability.PerFileThinProvisioningSupported}
		default:
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(row)
		return true
	}
	rows := make([]map[string]any, 0)
	objects := s.registry.All(kind)
	if p == "network" {
		objects = append(objects, s.registry.All("DistributedVirtualPortgroup")...)
	}
	for _, e := range objects {
		entity := e.Entity()
		row := map[string]any{"name": entity.Name}
		switch o := e.(type) {
		case *simulator.Datacenter:
			row["datacenter"] = entity.Self.Value
		case *simulator.ClusterComputeResource:
			row["cluster"] = entity.Self.Value
			row["drs_enabled"] = false
			row["ha_enabled"] = false
			if cfg, ok := o.ConfigurationEx.(*types.ClusterConfigInfoEx); ok {
				row["drs_enabled"] = cfg.DrsConfig.Enabled != nil && *cfg.DrsConfig.Enabled
				row["ha_enabled"] = cfg.DasConfig.Enabled != nil && *cfg.DasConfig.Enabled
			}
		case *simulator.HostSystem:
			row["host"] = entity.Self.Value
			row["connection_state"] = restConnectionState(string(o.Runtime.ConnectionState))
			row["power_state"] = restPowerState(string(o.Runtime.PowerState))
			if o.Hardware != nil {
				row["host_uuid"] = o.Hardware.SystemInfo.Uuid
			}
		case *simulator.Datastore:
			row["datastore"] = entity.Self.Value
			row["type"] = strings.ToUpper(o.Summary.Type)
			row["free_space"] = o.Summary.FreeSpace
			row["capacity"] = o.Summary.Capacity
		case *mo.Network:
			row["network"] = entity.Self.Value
			row["type"] = "STANDARD_PORTGROUP"
		case *simulator.DistributedVirtualPortgroup:
			row["network"] = entity.Self.Value
			row["type"] = "DISTRIBUTED_PORTGROUP"
		case *simulator.VirtualMachine:
			row["vm"] = entity.Self.Value
			row["power_state"] = restPowerState(string(o.Runtime.PowerState))
			row["cpu_count"] = o.Summary.Config.NumCpu
			row["memory_size_MiB"] = o.Summary.Config.MemorySizeMB
		case *simulator.Folder:
			row["folder"] = entity.Self.Value
			typ := "DATACENTER"
			for _, childType := range o.ChildType {
				switch childType {
				case "HostSystem", "ComputeResource", "ClusterComputeResource":
					typ = "HOST"
				case "VirtualMachine":
					typ = "VIRTUAL_MACHINE"
				case "Datastore":
					typ = "DATASTORE"
				case "Network", "DistributedVirtualSwitch":
					typ = "NETWORK"
				}
			}
			row["type"] = typ
		case *simulator.ResourcePool:
			row["resource_pool"] = entity.Self.Value
		default:
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["name"].(string) < rows[j]["name"].(string) })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
	return true
}

func restPowerState(state string) string {
	switch state {
	case "poweredOn":
		return "POWERED_ON"
	case "poweredOff":
		return "POWERED_OFF"
	case "suspended":
		return "SUSPENDED"
	case "standBy":
		return "STANDBY"
	default:
		return strings.ToUpper(state)
	}
}

func restConnectionState(state string) string {
	switch state {
	case "notResponding":
		return "NOT_RESPONDING"
	case "disconnected":
		return "DISCONNECTED"
	default:
		return strings.ToUpper(state)
	}
}
