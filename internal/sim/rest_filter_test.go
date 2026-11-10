package sim_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

type restFilterEnv struct {
	*env
	token string
}

func newRestFilterEnv(t *testing.T) *restFilterEnv {
	t.Helper()
	sc := tenClusters(t)
	e := startWith(t, topology.Generate(sc, registry.InMemory(sc.Seed)), sim.Options{})
	if err := e.login("reader", "password"); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/session", nil)
	req.SetBasicAuth("reader", "password")
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var token string
	if err := json.NewDecoder(res.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}
	return &restFilterEnv{env: e, token: token}
}

func (e *restFilterEnv) get(t *testing.T, path string, query url.Values) (int, []byte) {
	t.Helper()
	u := e.srv.URL + "/api/vcenter/" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("vmware-api-session-id", e.token)
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var raw json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatalf("%s: %v", u, err)
	}
	return res.StatusCode, raw
}

func (e *restFilterEnv) ids(t *testing.T, path, key string, query url.Values) []string {
	t.Helper()
	status, body := e.get(t, path, query)
	if status != 200 {
		t.Fatalf("%s %v: status %d %s", path, query, status, body)
	}
	if string(body) == "null" {
		t.Fatalf("%s %v: null body, want []", path, query)
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, row := range rows {
		out = append(out, row[key].(string))
	}
	sort.Strings(out)
	return out
}

func values(v []types.ManagedObjectReference) []string {
	out := []string{}
	for _, r := range v {
		out = append(out, r.Value)
	}
	sort.Strings(out)
	return out
}

func retrieve[T any](t *testing.T, e *env, kind string, props []string) []T {
	t.Helper()
	ctx := context.Background()
	v, err := view.NewManager(e.client).CreateContainerView(ctx, e.client.ServiceContent.RootFolder, []string{kind}, true)
	if err != nil {
		t.Fatal(err)
	}
	var out []T
	if err := v.Retrieve(ctx, []string{kind}, props, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRESTListFilters(t *testing.T) {
	e := newRestFilterEnv(t)
	cl := e.sim.Clusters[0]
	other := e.sim.Clusters[1]

	dcs := retrieve[mo.Datacenter](t, e.env, "Datacenter", []string{"name", "parent", "hostFolder", "vmFolder", "datastoreFolder", "networkFolder"})
	if len(dcs) == 0 {
		t.Fatal("no datacenters")
	}
	hosts := retrieve[mo.HostSystem](t, e.env, "HostSystem", []string{"name", "parent"})
	vms := retrieve[mo.VirtualMachine](t, e.env, "VirtualMachine", []string{"name", "parent", "runtime.host", "resourcePool"})
	clusters := retrieve[mo.ClusterComputeResource](t, e.env, "ClusterComputeResource", []string{"name", "parent", "resourcePool"})
	folders := retrieve[mo.Folder](t, e.env, "Folder", []string{"name", "parent", "childType"})
	pools := retrieve[mo.ResourcePool](t, e.env, "ResourcePool", []string{"name", "parent"})
	stores := retrieve[mo.Datastore](t, e.env, "Datastore", []string{"name", "summary"})

	dcOf := func(ref types.ManagedObjectReference) string {
		for _, d := range dcs {
			for _, f := range []types.ManagedObjectReference{d.HostFolder, d.VmFolder, d.DatastoreFolder, d.NetworkFolder} {
				if f == ref {
					return d.Self.Value
				}
			}
		}
		return ""
	}
	var clusterDC string
	for _, c := range clusters {
		if c.Self == cl.Ref {
			clusterDC = dcOf(*c.Parent)
		}
	}
	if clusterDC == "" {
		t.Fatal("cluster datacenter not found")
	}

	var clusterHosts, otherHosts, hostVMs, clusterVMs, dcClusters, dcVMs []string
	var someHost types.ManagedObjectReference
	for _, h := range hosts {
		switch *h.Parent {
		case cl.Ref:
			clusterHosts = append(clusterHosts, h.Self.Value)
		case other.Ref:
			otherHosts = append(otherHosts, h.Self.Value)
		}
	}
	someHost = cl.Hosts[0]
	var vmFolder types.ManagedObjectReference
	var vmPool types.ManagedObjectReference
	for _, vm := range vms {
		if vm.Runtime.Host != nil && *vm.Runtime.Host == someHost {
			hostVMs = append(hostVMs, vm.Self.Value)
		}
		if vm.Runtime.Host != nil {
			for _, h := range hosts {
				if h.Self == *vm.Runtime.Host && *h.Parent == cl.Ref {
					clusterVMs = append(clusterVMs, vm.Self.Value)
				}
			}
		}
		if vm.Self == cl.VMs[0] {
			vmFolder = *vm.Parent
			vmPool = *vm.ResourcePool
		}
		if dcOf(*vm.Parent) == clusterDC {
			dcVMs = append(dcVMs, vm.Self.Value)
		}
	}
	for _, c := range clusters {
		if dcOf(*c.Parent) == clusterDC {
			dcClusters = append(dcClusters, c.Self.Value)
		}
	}
	for _, s := range []*[]string{&clusterHosts, &otherHosts, &hostVMs, &clusterVMs, &dcClusters, &dcVMs} {
		sort.Strings(*s)
	}
	if len(clusterHosts) == 0 || len(otherHosts) == 0 || len(hostVMs) == 0 || len(clusterVMs) <= len(hostVMs) || len(dcClusters) == 0 || len(dcVMs) == 0 {
		t.Fatalf("scenario too small for filter test: %d %d %d %d %d %d", len(clusterHosts), len(otherHosts), len(hostVMs), len(clusterVMs), len(dcClusters), len(dcVMs))
	}
	var hostName, clusterName, vmName string
	for _, h := range hosts {
		if h.Self == someHost {
			hostName = h.Name
		}
	}
	for _, c := range clusters {
		if c.Self == cl.Ref {
			clusterName = c.Name
		}
	}
	for _, vm := range vms {
		if vm.Self == cl.VMs[0] {
			vmName = vm.Name
		}
	}
	var vsanStore, hostFolder string
	for _, s := range stores {
		if s.Self == cl.Datastore {
			vsanStore = s.Summary.Type
		}
	}
	for _, f := range folders {
		if f.Self == dcs[0].HostFolder {
			hostFolder = f.Self.Value
		}
	}
	if vsanStore != "vsan" {
		t.Fatalf("SOAP datastore type = %q", vsanStore)
	}
	var poolParent types.ManagedObjectReference
	for _, p := range pools {
		if p.Self == vmPool && p.Parent != nil {
			poolParent = *p.Parent
		}
	}

	q := func(kv ...string) url.Values {
		v := url.Values{}
		for i := 0; i < len(kv); i += 2 {
			v.Add(kv[i], kv[i+1])
		}
		return v
	}
	one := func(s string) []string { return []string{s} }
	none := []string{}

	for _, tc := range []struct {
		name, path, key string
		query           url.Values
		want            []string
	}{
		{"datacenter by id", "datacenter", "datacenter", q("datacenters", dcs[0].Self.Value), one(dcs[0].Self.Value)},
		{"datacenter by prefixed name", "datacenter", "datacenter", q("filter.names", dcs[0].Name), one(dcs[0].Self.Value)},
		{"datacenter no match", "datacenter", "datacenter", q("names", "no-such-datacenter"), none},
		{"cluster by id", "cluster", "cluster", q("clusters", cl.Ref.Value), one(cl.Ref.Value)},
		{"cluster by name", "cluster", "cluster", q("filter.names", clusterName), one(cl.Ref.Value)},
		{"cluster by datacenter", "cluster", "cluster", q("datacenters", clusterDC), dcClusters},
		{"cluster no match", "cluster", "cluster", q("filter.clusters", "domain-c0"), none},
		{"host by cluster", "host", "host", q("clusters", cl.Ref.Value), clusterHosts},
		{"host by prefixed cluster", "host", "host", q("filter.clusters", other.Ref.Value), otherHosts},
		{"host by name", "host", "host", q("names", hostName), one(someHost.Value)},
		{"host connected", "host", "host", q("connection_states", "CONNECTED", "clusters", cl.Ref.Value), clusterHosts},
		{"host disconnected", "host", "host", q("connection_states", "DISCONNECTED"), none},
		{"host id and wrong name", "host", "host", q("hosts", someHost.Value, "names", "other"), none},
		{"host repeated ids", "host", "host", q("hosts", cl.Hosts[0].Value, "hosts", cl.Hosts[1].Value), values(cl.Hosts[:2])},
		{"datastore by id", "datastore", "datastore", q("datastores", cl.Datastore.Value), one(cl.Datastore.Value)},
		{"datastore type vsan", "datastore", "datastore", q("filter.types", "VSAN", "datastores", cl.Datastore.Value), one(cl.Datastore.Value)},
		{"datastore type nfs", "datastore", "datastore", q("types", "NFS"), none},
		{"vm by host", "vm", "vm", q("hosts", someHost.Value), hostVMs},
		{"vm by cluster", "vm", "vm", q("filter.clusters", cl.Ref.Value), clusterVMs},
		{"vm by datacenter", "vm", "vm", q("datacenters", clusterDC), dcVMs},
		{"vm by name", "vm", "vm", q("names", vmName, "hosts", someHost.Value), one(cl.VMs[0].Value)},
		{"vm by id", "vm", "vm", q("vms", cl.VMs[0].Value), one(cl.VMs[0].Value)},
		{"vm powered on", "vm", "vm", q("power_states", "POWERED_ON", "hosts", someHost.Value), hostVMs},
		{"vm powered off", "vm", "vm", q("filter.power_states", "POWERED_OFF"), none},
		{"vm folder", "vm", "vm", q("folders", vmFolder.Value, "hosts", someHost.Value), hostVMs},
		{"vm resource pool", "vm", "vm", q("resource_pools", vmPool.Value, "names", vmName), one(cl.VMs[0].Value)},
		{"vm other host excluded", "vm", "vm", q("hosts", otherHosts[0], "vms", cl.VMs[0].Value), none},
		{"vm no match", "vm", "vm", q("names", "no-such-vm"), none},
		{"folder by id", "folder", "folder", q("folders", hostFolder), one(hostFolder)},
		{"folder by type", "folder", "folder", q("type", "VIRTUAL_MACHINE", "folders", vmFolder.Value), one(vmFolder.Value)},
		{"folder by parent", "folder", "folder", q("filter.parent_folders", "no-such-folder"), none},
		{"resource pool by cluster", "resource-pool", "resource_pool", q("clusters", cl.Ref.Value, "resource_pools", vmPool.Value), one(vmPool.Value)},
		{"resource pool by parent", "resource-pool", "resource_pool", q("parent_resource_pools", poolParent.Value, "resource_pools", vmPool.Value), func() []string {
			if poolParent.Type == "ResourcePool" {
				return one(vmPool.Value)
			}
			return none
		}()},
		{"resource pool no match", "resource-pool", "resource_pool", q("filter.datacenters", "datacenter-0"), none},
		{"network by name no match", "network", "network", q("names", "no-such-network"), none},
		{"network by type no match", "network", "network", q("types", "OPAQUE_NETWORK"), none},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.ids(t, tc.path, tc.key, tc.query); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}

	t.Run("network types partition the inventory", func(t *testing.T) {
		all := e.ids(t, "network", "network", nil)
		std := e.ids(t, "network", "network", q("types", "STANDARD_PORTGROUP"))
		dvpg := e.ids(t, "network", "network", q("filter.types", "DISTRIBUTED_PORTGROUP"))
		if len(all) == 0 || len(std)+len(dvpg) != len(all) {
			t.Errorf("std %d + dvpg %d != all %d", len(std), len(dvpg), len(all))
		}
		byName := e.ids(t, "network", "network", q("names", "no-such-network"))
		if len(byName) != 0 {
			t.Errorf("name filter returned %v", byName)
		}
	})

	t.Run("unsupported parameters are invalid arguments", func(t *testing.T) {
		for _, tc := range []struct{ path, param string }{
			{"vm", "bogus"}, {"vm", "filter.bogus"}, {"host", "power_states"}, {"datastore", "clusters"},
			{"cluster", "hosts"}, {"datacenter", "clusters"}, {"network", "hosts"}, {"folder", "types"}, {"resource-pool", "hosts"},
		} {
			status, body := e.get(t, tc.path, q(tc.param, "x"))
			var got struct {
				ErrorType string `json:"error_type"`
				Messages  []struct {
					ID string `json:"id"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("%s?%s: %v", tc.path, tc.param, err)
			}
			if status != 400 || got.ErrorType != "INVALID_ARGUMENT" || len(got.Messages) != 1 || got.Messages[0].ID != "com.vmware.vapi.std.errors.invalid_argument" {
				t.Errorf("%s?%s: status %d body %s", tc.path, tc.param, status, body)
			}
		}
	})
}

func TestRESTListOrderIsDeterministic(t *testing.T) {
	e := newRestFilterEnv(t)
	for _, tc := range []struct{ path, key string }{{"resource-pool", "resource_pool"}, {"folder", "folder"}, {"datastore", "datastore"}, {"cluster", "cluster"}} {
		_, first := e.get(t, tc.path, nil)
		var rows []map[string]any
		if err := json.Unmarshal(first, &rows); err != nil {
			t.Fatal(err)
		}
		for i := 1; i < len(rows); i++ {
			a, b := rows[i-1], rows[i]
			an, bn := a["name"].(string), b["name"].(string)
			if an > bn || an == bn && a[tc.key].(string) >= b[tc.key].(string) {
				t.Errorf("%s rows %d,%d out of order: %v then %v", tc.path, i-1, i, a, b)
			}
		}
		dupes := false
		for i := 1; i < len(rows); i++ {
			dupes = dupes || rows[i-1]["name"] == rows[i]["name"]
		}
		if tc.path == "resource-pool" && !dupes {
			t.Errorf("%s: scenario has no repeated names, tie-break untested", tc.path)
		}
		for i := 0; i < 20; i++ {
			if _, again := e.get(t, tc.path, nil); string(again) != string(first) {
				t.Fatalf("%s: order changed between calls", tc.path)
			}
		}
	}
}

func TestRESTReadsRaceWithSOAPPowerChanges(t *testing.T) {
	e := newRestFilterEnv(t)
	ctx := context.Background()
	vm := object.NewVirtualMachine(e.client, e.sim.Clusters[0].VMs[0])
	var wg sync.WaitGroup
	done := make(chan struct{})
	for _, path := range []string{"vm", "host", "cluster", "datastore", "vm/" + e.sim.Clusters[0].VMs[0].Value} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/vcenter/"+path, nil)
					req.Header.Set("vmware-api-session-id", e.token)
					res, err := e.srv.Client().Do(req)
					if err != nil {
						t.Error(err)
						return
					}
					res.Body.Close()
					if res.StatusCode != 200 {
						t.Errorf("%s: status %d", path, res.StatusCode)
						return
					}
				}
			}
		}()
	}
	for i := 0; i < 5; i++ {
		for _, op := range []func(context.Context) (*object.Task, error){vm.PowerOff, vm.PowerOn} {
			task, err := op(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := task.Wait(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(done)
	wg.Wait()
}
