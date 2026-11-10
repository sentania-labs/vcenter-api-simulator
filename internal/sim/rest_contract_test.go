package sim_test

import (
	"context"
	"encoding/json"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"net/http"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

func TestCapturedRESTInventoryContract(t *testing.T) {
	data, err := os.ReadFile("fixtures/rest-inventory-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string][]string
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	sc := tenClusters(t)
	e := startWith(t, topology.Generate(sc, registry.InMemory(sc.Seed)), sim.Options{})
	client := e.srv.Client()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/session", nil)
	req.SetBasicAuth("reader", "password")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var token string
	if err := json.NewDecoder(res.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	cl := e.sim.Clusters[0]
	paths := map[string]string{"datacenter": "datacenter", "cluster": "cluster", "host": "host", "datastore": "datastore", "network": "network", "vm": "vm", "folder": "folder", "resource-pool": "resource-pool", "cluster-detail": "cluster/" + cl.Ref.Value, "datastore-detail": "datastore/" + cl.Datastore.Value, "vm-detail": "vm/" + cl.VMs[0].Value, "vm-hardware": "vm/" + cl.VMs[0].Value + "/hardware", "vm-disk": "vm/" + cl.VMs[0].Value + "/hardware/disk", "guest-identity": "vm/" + cl.VMs[0].Value + "/guest/identity"}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/vcenter/"+path, nil)
			req.Header.Set("vmware-api-session-id", token)
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatalf("status %d", res.StatusCode)
			}
			var body any
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if rows, ok := body.([]any); ok {
				if len(rows) == 0 {
					t.Fatal("empty inventory")
				}
				body = rows[0]
			}
			row, ok := body.(map[string]any)
			if !ok {
				t.Fatalf("unexpected body %T", body)
			}
			var keys []string
			for key := range row {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(keys, fixture[name]) {
				t.Errorf("keys = %v, captured %v", keys, fixture[name])
			}
		})
	}
}

func TestRESTClusterConfigurationMatchesSOAP(t *testing.T) {
	sc := tenClusters(t)
	e := startWith(t, topology.Generate(sc, registry.InMemory(sc.Seed)), sim.Options{})
	if err := e.login("reader", "password"); err != nil {
		t.Fatal(err)
	}
	var cl mo.ClusterComputeResource
	if err := property.DefaultCollector(e.client).RetrieveOne(context.Background(), e.sim.Clusters[0].Ref, []string{"configurationEx"}, &cl); err != nil {
		t.Fatal(err)
	}
	cfg, ok := cl.ConfigurationEx.(*types.ClusterConfigInfoEx)
	if !ok {
		t.Fatal("missing SOAP cluster configuration")
	}
	client := e.srv.Client()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/session", nil)
	req.SetBasicAuth("reader", "password")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var token string
	if err := json.NewDecoder(res.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	req, _ = http.NewRequest(http.MethodGet, e.srv.URL+"/api/vcenter/cluster", nil)
	req.Header.Set("vmware-api-session-id", token)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var rows []struct {
		Cluster string `json:"cluster"`
		HA      bool   `json:"ha_enabled"`
		DRS     bool   `json:"drs_enabled"`
	}
	if err := json.NewDecoder(res.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Cluster == e.sim.Clusters[0].Ref.Value {
			if row.HA != (cfg.DasConfig.Enabled != nil && *cfg.DasConfig.Enabled) || row.DRS != (cfg.DrsConfig.Enabled != nil && *cfg.DrsConfig.Enabled) {
				t.Fatalf("REST cluster settings disagree with SOAP: %+v", row)
			}
			return
		}
	}
	t.Fatal("cluster missing")
}
