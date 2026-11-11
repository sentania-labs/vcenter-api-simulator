package control

import (
	"context"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
)

// The console says how many hosts have no working path to a LUN, and the
// simulator serves each host's mount as accessible or not. Both follow the
// scenario's per-host path overrides, so they must name the same hosts.
func TestConsoleAndServedAPIAgreeOnHostSpecificPaths(t *testing.T) {
	sc := scenario.Phase1("iscsi-agreement")
	cluster := "dc-01/cluster-01"
	sc.Datacenters[0].ISCSI = []scenario.ISCSIStorage{{
		Cluster:  cluster,
		Adapters: []scenario.ISCSIAdapter{{Name: "software", Kind: "software"}, {Name: "hardware", Kind: "hardware"}},
		Targets:  []scenario.ISCSITarget{{Name: "array", Address: "array.example.test", IQN: "iqn.2026-01.test.example:array"}},
		LUNs: []scenario.ISCSILUN{{Name: "shared-vmfs", Target: "array", Number: 1, Capacity: 1 << 30, FreeSpace: 1 << 20, Paths: []scenario.ISCSIPath{
			{Adapter: "software", State: "active"},
			{Adapter: "hardware", State: "dead"},
			// esx02: both paths dead, so no working path
			{Adapter: "software", Host: cluster + "/esx02", State: "dead"},
			// esx03: software dead, but its own hardware override is standby
			{Adapter: "software", Host: cluster + "/esx03", State: "dead"},
			{Adapter: "hardware", Host: cluster + "/esx03", State: "standby"},
		}}},
	}}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	c := newCtl(t, sc, "", "t")
	view, err := c.Cluster(cluster)
	if err != nil {
		t.Fatal(err)
	}
	var console string
	for _, row := range view.Datastores {
		if row.Name == "shared-vmfs" {
			console = row.State
		}
	}
	if console != "1 host has no working path" {
		t.Fatalf("console state %q", console)
	}

	s, err := sim.New(sim.Options{Topology: c.applied().topo, Now: func() time.Time { return time.Unix(1_700_000_000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s.Handler)
	t.Cleanup(srv.Close)
	u, _ := soap.ParseURL(srv.URL + "/sdk")
	u.User = nil
	ctx := context.Background()
	client, err := vim25.NewClient(ctx, soap.NewClient(u, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.NewManager(client).Login(ctx, url.UserPassword("reader", "password")); err != nil {
		t.Fatal(err)
	}
	pc := property.DefaultCollector(client)
	names := map[string]string{}
	var store mo.Datastore
	for _, href := range s.Clusters[0].Hosts {
		var h mo.HostSystem
		if err := pc.RetrieveOne(ctx, href, []string{"name", "datastore"}, &h); err != nil {
			t.Fatal(err)
		}
		names[href.Value] = h.Name
		for _, dref := range h.Datastore {
			var ds mo.Datastore
			if err := pc.RetrieveOne(ctx, dref, []string{"name", "host"}, &ds); err != nil {
				t.Fatal(err)
			}
			if ds.Name == "shared-vmfs" {
				store = ds
			}
		}
	}
	var down []string
	for _, m := range store.Host {
		if m.MountInfo.Accessible != nil && !*m.MountInfo.Accessible {
			down = append(down, names[m.Key.Value])
		}
	}
	if len(down) != 1 || down[0] != "esx02.cluster-01.dc-01.sim.invalid" {
		t.Errorf("served API reports inaccessible mounts on %v, console reports %q", down, console)
	}
}

func TestStorageRowsSayWhichDatastoresAnswerPerformanceQueries(t *testing.T) {
	sc := scenario.Phase1("storage-perf-rows")
	cluster := "dc-01/cluster-01"
	sc.Datacenters[0].NFS = []scenario.NFSDatastore{{Name: "shared-nfs", RemoteHost: "nfs.example.test", RemotePath: "/shared", Capacity: 1000, FreeSpace: 500}}
	sc.Datacenters[0].ISCSI = []scenario.ISCSIStorage{{
		Cluster:  cluster,
		Adapters: []scenario.ISCSIAdapter{{Name: "software", Kind: "software"}},
		Targets:  []scenario.ISCSITarget{{Name: "array", Address: "array.example.test", IQN: "iqn.2026-01.test.example:array"}},
		LUNs:     []scenario.ISCSILUN{{Name: "shared-vmfs", Target: "array", Number: 1, Capacity: 1 << 30, FreeSpace: 1 << 20, Paths: []scenario.ISCSIPath{{Adapter: "software", State: "active"}}}},
	}}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	view, err := newCtl(t, sc, "", "t").Cluster(cluster)
	if err != nil {
		t.Fatal(err)
	}
	perf := map[string]string{}
	for _, row := range view.Datastores {
		perf[row.Type] = row.Perf
	}
	want := "queried: capacity and used space, I/O counters read zero"
	if perf["NFS"] != want || perf["VMFS · iSCSI"] != want || perf["vSAN ESA"] != "queried" || perf["VMFS · local"] != "inventory only" {
		t.Errorf("performance column by datastore type: %v", perf)
	}
}
