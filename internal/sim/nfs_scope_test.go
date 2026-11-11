package sim_test

import (
	"context"
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
)

// A datastore scoped to one cluster must expose the same inaccessible host
// through both the datastore and the host storage system.
func TestNFSScopedFault(t *testing.T) {
	sc := scenario.Phase1("nfs-scope")
	sc.Datacenters[0].Clusters = 2
	sc.Datacenters[0].NFS = []scenario.NFSDatastore{{Name: "shared-nfs", RemoteHost: "nfs.example.test", RemotePath: "/exports/shared", Capacity: 1000, FreeSpace: 10, Clusters: []string{"dc-01/cluster-01"}, InaccessibleHosts: []string{"dc-01/cluster-01/esx02"}}}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	e := startWith(t, topology.Generate(sc, registry.InMemory(sc.Seed)), sim.Options{})
	if err := e.login("reader", "password"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pc := property.DefaultCollector(e.client)
	refs := e.sim.Clusters[0]
	var h mo.HostSystem
	if err := pc.RetrieveOne(ctx, refs.Hosts[1], []string{"datastore", "config.fileSystemVolume", "configManager.storageSystem"}, &h); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ref := range h.Datastore {
		var ds mo.Datastore
		if err := pc.RetrieveOne(ctx, ref, []string{"name", "host", "summary"}, &ds); err != nil {
			t.Fatal(err)
		}
		if ds.Name != "shared-nfs" {
			continue
		}
		found = true
		if len(ds.Host) != len(refs.Hosts) || !ds.Summary.Accessible || ds.Summary.FreeSpace != 10 {
			t.Fatalf("unexpected datastore state: %+v", ds.Summary)
		}
		for _, m := range ds.Host {
			if m.Key == refs.Hosts[1] && (m.MountInfo.Accessible == nil || *m.MountInfo.Accessible) {
				t.Fatal("fault not reflected in datastore mount")
			}
		}
	}
	if !found {
		t.Fatal("missing datastore")
	}
	var ss mo.HostStorageSystem
	if err := pc.RetrieveOne(ctx, *h.ConfigManager.StorageSystem, []string{"fileSystemVolumeInfo"}, &ss); err != nil {
		t.Fatal(err)
	}
	for _, m := range ss.FileSystemVolumeInfo.MountInfo {
		if m.Volume.GetHostFileSystemVolume().Name == "shared-nfs" {
			if m.MountInfo.Accessible == nil || *m.MountInfo.Accessible {
				t.Fatal("fault not reflected in storage system")
			}
			found = false
		}
	}
	if found {
		t.Fatal("storage system missing mount")
	}
	var other mo.HostSystem
	if err := pc.RetrieveOne(ctx, e.sim.Clusters[1].Hosts[0], []string{"datastore"}, &other); err != nil {
		t.Fatal(err)
	}
	for _, ref := range other.Datastore {
		var ds mo.Datastore
		if err := pc.RetrieveOne(ctx, ref, []string{"name"}, &ds); err != nil {
			t.Fatal(err)
		}
		if ds.Name == "shared-nfs" {
			t.Fatal("datastore mounted outside selected cluster")
		}
	}
}
