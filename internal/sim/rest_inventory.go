package sim

import (
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

type restList struct {
	kind, idKey, idParam string
	params               []string
}

var restLists = map[string]restList{
	"datacenter":    {"Datacenter", "datacenter", "datacenters", []string{"names", "folders"}},
	"cluster":       {"ClusterComputeResource", "cluster", "clusters", []string{"names", "folders", "datacenters"}},
	"host":          {"HostSystem", "host", "hosts", []string{"names", "folders", "datacenters", "clusters", "connection_states"}},
	"datastore":     {"Datastore", "datastore", "datastores", []string{"names", "types", "folders", "datacenters"}},
	"network":       {"Network", "network", "networks", []string{"names", "types", "folders", "datacenters"}},
	"vm":            {"VirtualMachine", "vm", "vms", []string{"names", "folders", "datacenters", "hosts", "clusters", "resource_pools", "power_states"}},
	"folder":        {"Folder", "folder", "folders", []string{"names", "parent_folders", "datacenters", "type"}},
	"resource-pool": {"ResourcePool", "resource_pool", "resource_pools", []string{"names", "parent_resource_pools", "datacenters", "clusters"}},
}

type restRel struct {
	parent, host, pool *types.ManagedObjectReference
}

func (s *restSessions) lock(obj mo.Reference, f func()) {
	s.registry.WithLock(&simulator.Context{}, obj, f)
}

func restInvalidArgument(w http.ResponseWriter, param string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error_type": "INVALID_ARGUMENT", "messages": []map[string]any{{"id": "com.vmware.vapi.std.errors.invalid_argument", "default_message": "Unsupported filter parameter: " + param, "args": []string{param}}}})
}

// restFilters returns the requested filter values keyed by parameter name.
// Both the plain and the filter.-prefixed spelling are accepted. A false
// result means the request has already been answered with an error.
func restFilters(w http.ResponseWriter, r *http.Request, l restList) (map[string][]string, bool) {
	filters := map[string][]string{}
	for key, values := range r.URL.Query() {
		name := strings.TrimPrefix(key, "filter.")
		if name != l.idParam && !slices.Contains(l.params, name) {
			restInvalidArgument(w, key)
			return nil, false
		}
		filters[name] = append(filters[name], values...)
	}
	return filters, true
}

// ancestors returns ref and its parents, nearest first.
func (s *restSessions) ancestors(ref *types.ManagedObjectReference) []types.ManagedObjectReference {
	var out []types.ManagedObjectReference
	for i := 0; ref != nil && i < 32; i++ {
		e, ok := s.registry.Get(*ref).(mo.Entity)
		if !ok {
			break
		}
		out = append(out, *ref)
		var next *types.ManagedObjectReference
		s.lock(e, func() {
			if p := e.Entity().Parent; p != nil {
				v := *p
				next = &v
			}
		})
		ref = next
	}
	return out
}

func (s *restSessions) attributes(l restList, id string, row map[string]any, rel restRel) map[string][]string {
	a := map[string][]string{}
	add := func(k string, ref types.ManagedObjectReference) { a[k] = append(a[k], ref.Value) }
	for _, ref := range s.ancestors(rel.parent) {
		switch ref.Type {
		case "Folder":
			add("folders", ref)
		case "Datacenter":
			add("datacenters", ref)
		case "ClusterComputeResource":
			add("clusters", ref)
		}
	}
	if rel.parent != nil {
		switch rel.parent.Type {
		case "Folder":
			add("parent_folders", *rel.parent)
		case "ResourcePool":
			add("parent_resource_pools", *rel.parent)
		}
	}
	if rel.host != nil {
		add("hosts", *rel.host)
		for _, ref := range s.ancestors(rel.host) {
			if ref.Type == "ClusterComputeResource" {
				add("clusters", ref)
			}
		}
	}
	for _, ref := range s.ancestors(rel.pool) {
		add("resource_pools", ref)
	}
	a[l.idParam] = []string{id}
	a["names"] = []string{row["name"].(string)}
	for param, key := range map[string]string{"type": "type", "types": "type", "power_states": "power_state", "connection_states": "connection_state"} {
		if v, ok := row[key].(string); ok {
			a[param] = []string{v}
		}
	}
	return a
}

func restMatches(filters, attrs map[string][]string) bool {
	for param, want := range filters {
		if len(want) == 0 {
			continue
		}
		matched := false
		for _, v := range attrs[param] {
			if slices.Contains(want, v) {
				matched = true
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// inventory reads the same object registry as the SOAP service.
func (s *restSessions) inventory(w http.ResponseWriter, r *http.Request) bool {
	p := strings.TrimPrefix(r.URL.Path, "/api/vcenter/")
	if p == r.URL.Path {
		return false
	}
	l, ok := restLists[p]
	if !ok {
		if strings.HasPrefix(p, "vm/") && s.vmInventory(w, p) {
			return true
		}
		parts := strings.Split(p, "/")
		if len(parts) != 2 || (parts[0] != "cluster" && parts[0] != "datastore") {
			return false
		}
		obj := s.registry.Get(types.ManagedObjectReference{Type: restLists[parts[0]].kind, Value: parts[1]})
		var row map[string]any
		switch o := obj.(type) {
		case *simulator.ClusterComputeResource:
			s.lock(o, func() {
				row = map[string]any{"name": o.Name}
				if o.ResourcePool != nil {
					row["resource_pool"] = o.ResourcePool.Value
				}
			})
		case *simulator.Datastore:
			s.lock(o, func() {
				multiple := o.Summary.MultipleHostAccess != nil && *o.Summary.MultipleHostAccess
				row = map[string]any{"name": o.Name, "type": strings.ToUpper(o.Summary.Type), "free_space": o.Summary.FreeSpace, "accessible": o.Summary.Accessible, "multiple_host_access": multiple, "thin_provisioning_supported": o.Capability.PerFileThinProvisioningSupported}
			})
		default:
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(row)
		return true
	}
	filters, ok := restFilters(w, r, l)
	if !ok {
		return true
	}
	rows := make([]map[string]any, 0)
	objects := s.registry.All(l.kind)
	if p == "network" {
		objects = append(objects, s.registry.All("DistributedVirtualPortgroup")...)
	}
	for _, e := range objects {
		var row map[string]any
		var rel restRel
		s.lock(e, func() { row, rel = restRow(e) })
		if row == nil {
			continue
		}
		if len(filters) > 0 && !restMatches(filters, s.attributes(l, row[l.idKey].(string), row, rel)) {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i]["name"] != rows[j]["name"] {
			return rows[i]["name"].(string) < rows[j]["name"].(string)
		}
		return rows[i][l.idKey].(string) < rows[j][l.idKey].(string)
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
	return true
}

// restRow must be called with the object's lock held.
func restRow(e mo.Entity) (map[string]any, restRel) {
	entity := e.Entity()
	row := map[string]any{"name": entity.Name}
	var rel restRel
	if entity.Parent != nil {
		v := *entity.Parent
		rel.parent = &v
	}
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
		if o.Runtime.Host != nil {
			v := *o.Runtime.Host
			rel.host = &v
		}
		if o.ResourcePool != nil {
			v := *o.ResourcePool
			rel.pool = &v
		}
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
		return nil, rel
	}
	return row, rel
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
