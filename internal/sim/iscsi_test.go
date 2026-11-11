package sim_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func TestISCSISharedLUNAndHostFailure(t *testing.T) {
	sc := scenario.Phase1("iscsi-test")
	sc.Datacenters[0].ISCSI = []scenario.ISCSIStorage{{Cluster: "dc-01/cluster-01", Adapters: []scenario.ISCSIAdapter{{Name: "software", Kind: "software"}, {Name: "hardware", Kind: "hardware"}}, Targets: []scenario.ISCSITarget{{Name: "array", Address: "array.example.test", IQN: "iqn.2026-01.test.example:array"}}, LUNs: []scenario.ISCSILUN{{Name: "shared-vmfs", Target: "array", Number: 1, Capacity: 1024 * 1024 * 1024, FreeSpace: 1024, Paths: []scenario.ISCSIPath{{Adapter: "software", State: "active"}, {Adapter: "hardware", State: "standby"}, {Adapter: "software", Host: "dc-01/cluster-01/esx02", State: "dead"}, {Adapter: "hardware", Host: "dc-01/cluster-01/esx02", State: "dead"}}}}}}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	e := startWith(t, topology.Generate(sc, registry.InMemory(sc.Seed)), sim.Options{})
	if err := e.login("reader", "password"); err != nil {
		t.Fatal(err)
	}
	pc := property.DefaultCollector(e.client)
	ctx := context.Background()
	refs := e.sim.Clusters[0]
	var datastoreID types.ManagedObjectReference
	for i, href := range refs.Hosts {
		var h mo.HostSystem
		if err := pc.RetrieveOne(ctx, href, []string{"config.storageDevice", "configManager.storageSystem", "datastore"}, &h); err != nil {
			t.Fatal(err)
		}
		var sys mo.HostStorageSystem
		if err := pc.RetrieveOne(ctx, *h.ConfigManager.StorageSystem, []string{"storageDeviceInfo", "fileSystemVolumeInfo"}, &sys); err != nil {
			t.Fatal(err)
		}
		if sys.StorageDeviceInfo == nil {
			t.Fatal("missing host storage")
		}
		var adapters, disks int
		var canonical string
		for _, x := range sys.StorageDeviceInfo.HostBusAdapter {
			if a, ok := x.(*types.HostInternetScsiHba); ok {
				adapters++
				if len(a.ConfiguredSendTarget) != 1 || len(a.ConfiguredStaticTarget) != 1 {
					t.Fatal("targets missing")
				}
			}
		}
		for _, x := range sys.StorageDeviceInfo.ScsiLun {
			if d, ok := x.(*types.HostScsiDisk); ok && d.Vendor == "SIMULATR" {
				disks++
				canonical = d.CanonicalName
				want := "ok"
				if i == 1 {
					want = "lostCommunication"
				}
				if len(d.OperationalState) != 1 || d.OperationalState[0] != want {
					t.Fatalf("LUN state %v", d.OperationalState)
				}
			}
		}
		if adapters != 2 || disks != 1 || sys.StorageDeviceInfo.MultipathInfo == nil || len(sys.StorageDeviceInfo.MultipathInfo.Lun) != 1 {
			t.Fatalf("adapter/lun/path inventory %d/%d", adapters, disks)
		}
		mp := sys.StorageDeviceInfo.MultipathInfo.Lun[0]
		if len(mp.Path) != 2 {
			t.Fatal("missing paths")
		}
		found := false
		for _, ref := range h.Datastore {
			var ds mo.Datastore
			if err := pc.RetrieveOne(ctx, ref, []string{"name", "info", "host", "summary"}, &ds); err != nil {
				t.Fatal(err)
			}
			if ds.Name != "shared-vmfs" {
				continue
			}
			found = true
			if i == 0 {
				datastoreID = ref
			} else if datastoreID != ref {
				t.Fatal("datastore is not shared")
			}
			info := ds.Info.(*types.VmfsDatastoreInfo)
			if len(info.Vmfs.Extent) != 1 || info.Vmfs.Extent[0].DiskName != canonical || ds.Summary.FreeSpace != 1024 || len(ds.Host) != len(refs.Hosts) {
				t.Fatal("datastore does not match LUN")
			}
			for _, mount := range ds.Host {
				if mount.Key == href && (mount.MountInfo.Accessible == nil || *mount.MountInfo.Accessible != (i != 1)) {
					t.Fatal("host accessibility does not match paths")
				}
			}
		}
		if !found {
			t.Fatal("shared datastore missing")
		}
	}
	available, err := methods.QueryAvailablePerfMetric(ctx, e.client.Client, &types.QueryAvailablePerfMetric{This: *e.client.ServiceContent.PerfManager, Entity: datastoreID, IntervalId: 300})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int32]bool{}
	for _, m := range available.Returnval {
		ids[m.CounterId] = true
	}
	for _, id := range []int32{136, 137, 178, 179, 281, 282, 283, 284} {
		if !ids[id] {
			t.Fatalf("missing storage counter %d", id)
		}
	}
	samples, err := methods.QueryPerf(ctx, e.client.Client, &types.QueryPerf{This: *e.client.ServiceContent.PerfManager, QuerySpec: []types.PerfQuerySpec{{Entity: datastoreID, IntervalId: 300, MaxSample: 1, MetricId: []types.PerfMetricId{{CounterId: 283}, {CounterId: 281}, {CounterId: 136}}}}})
	if err != nil {
		t.Fatal(err)
	}
	metric := samples.Returnval[0].(*types.PerfEntityMetric)
	want := []int64{1024 * 1024, (1024*1024*1024 - 1024) / 1024, 0}
	for i, series := range metric.Value {
		s := series.(*types.PerfMetricIntSeries)
		if len(s.Value) != 1 || s.Value[0] != want[i] {
			t.Fatalf("counter %d values %v, want %d", i, s.Value, want[i])
		}
	}
	csv, err := methods.QueryPerf(ctx, e.client.Client, &types.QueryPerf{This: *e.client.ServiceContent.PerfManager, QuerySpec: []types.PerfQuerySpec{{Entity: datastoreID, IntervalId: 300, MaxSample: 1, Format: "csv", MetricId: []types.PerfMetricId{{CounterId: 136}, {CounterId: 283}}}}})
	if err != nil {
		t.Fatal(err)
	}
	values := csv.Returnval[0].(*types.PerfEntityMetricCSV).Value
	if len(values) != 2 || values[0].Value != "0" || values[1].Value != "1048576" {
		t.Fatalf("CSV samples: %+v", values)
	}
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/session", nil)
	req.SetBasicAuth("reader", "password")
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var token string
	if err = json.NewDecoder(res.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	req, _ = http.NewRequest(http.MethodGet, e.srv.URL+"/api/vcenter/datastore/"+datastoreID.Value, nil)
	req.Header.Set("vmware-api-session-id", token)
	res, err = e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var detail struct {
		Type       string `json:"type"`
		Free       int64  `json:"free_space"`
		Accessible bool   `json:"accessible"`
		Multiple   bool   `json:"multiple_host_access"`
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("REST datastore status %s", res.Status)
	}
	if err = json.NewDecoder(res.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if detail.Type != "VMFS" || detail.Free != 1024 || !detail.Accessible || !detail.Multiple {
		t.Fatalf("REST datastore differs from SOAP: %+v", detail)
	}
}
