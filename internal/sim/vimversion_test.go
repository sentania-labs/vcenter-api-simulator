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
	"github.com/vmware/govmomi/vim25/types"
)

// A vCenter leaves out members newer than the API version a client asks for.
// PowerCLI 13.3 asks for 8.0.3.0 and loses the vmfs volume of a datastore
// whose info carries the 9.0 sector members. A 9.x client keeps them, as
// the real 9.1.1 capture in fixtures/live/9.1.1 reports them.
func TestDatastoreInfoFollowsRequestedVersion(t *testing.T) {
	sc := scenario.Phase1("version-test")
	e := startWith(t, topology.Generate(sc, registry.InMemory(sc.Seed)), sim.Options{})
	if err := e.login("reader", "password"); err != nil {
		t.Fatal(err)
	}
	pc := property.DefaultCollector(e.client)
	ctx := context.Background()
	var hs mo.HostSystem
	if err := pc.RetrieveOne(ctx, e.sim.Clusters[0].Hosts[0], []string{"datastore"}, &hs); err != nil {
		t.Fatal(err)
	}
	local := func(version string) *types.VmfsDatastoreInfo {
		t.Helper()
		e.client.Version = version
		defer func() { e.client.Version = "" }()
		for _, ref := range hs.Datastore {
			var ds mo.Datastore
			if err := pc.RetrieveOne(ctx, ref, []string{"info"}, &ds); err != nil {
				t.Fatal(err)
			}
			if info, ok := ds.Info.(*types.VmfsDatastoreInfo); ok {
				return info
			}
		}
		t.Fatal("no local VMFS datastore")
		return nil
	}
	for _, version := range []string{"9.0.0.0", "9.1.0.0"} {
		info := local(version)
		if info.Vmfs == nil || info.LogicalSectorSize != 512 || info.PhysicalSectorSize != 4096 || len(info.SupportedVDiskFormats) != 1 || info.SupportedVDiskFormats[0] != "native_512" {
			t.Errorf("vim %s client: local VMFS info %+v", version, info.DatastoreInfo)
		}
	}
	for _, version := range []string{"8.0.3.0", "7.0.3.0"} {
		info := local(version)
		if info.Vmfs == nil || info.MaxFileSize == 0 || info.LogicalSectorSize != 0 || info.PhysicalSectorSize != 0 || len(info.SupportedVDiskFormats) != 0 {
			t.Errorf("vim %s client: local VMFS info %+v", version, info.DatastoreInfo)
		}
	}
}
