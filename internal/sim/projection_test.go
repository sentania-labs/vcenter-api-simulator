package sim_test

import (
	"net/http"
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/control"
	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// Each projected family is checked against the actual wildcard SOAP response,
// including OSA overrides, an ESA cluster, and an OSA cluster without a primary.
func TestMixedArchitectureProjectionMatchesSOAP(t *testing.T) {
	sc := scenario.Phase1("projection-osa")
	sc.Datacenters[0].Clusters = 3
	sc.Defaults.Architecture = "osa"
	sc.Defaults.Hosts = 3
	sc.Defaults.Host.Threads = 2
	sc.Defaults.Host.DomWorlds = scenario.IntPtr(2)
	offline := false
	sc.Clusters = []scenario.ClusterOverride{
		{Key: "dc-01/cluster-02", ClusterSpec: scenario.ClusterSpec{Architecture: "esa"}},
		{Key: "dc-01/cluster-03", ClusterSpec: scenario.ClusterSpec{StatsPrimary: &offline}},
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	topo := topology.Generate(sc, registry.InMemory(sc.Seed))
	e := startWith(t, topo, sim.Options{})
	if err := e.login("administrator@vsphere.local", "VMware1!"); err != nil {
		t.Fatal(err)
	}
	projection := control.Project(catalog.MustLoad(), topo)
	var allEntities, allSeries, emptyCalls int
	wantFamilies := map[string]control.ClusterLoad{}
	for _, cl := range e.sim.Clusters {
		var clusterEntities, clusterSeries int
		for _, f := range projection.Families {
			status, body := e.postPerf(cl.Ref, f.Family)
			if status == http.StatusInternalServerError {
				emptyCalls++
				continue
			}
			if status != http.StatusOK {
				t.Fatalf("%s %s: status %d", cl.Cluster.Key, f.Family, status)
			}
			got := parseShape(t, body)
			if len(got.labels) == 0 {
				emptyCalls++
				continue
			}
			clusterEntities += len(got.refs)
			clusterSeries += len(got.refs) * len(got.labels)
			want := wantFamilies[f.Family]
			want.Entities += len(got.refs)
			want.Series += len(got.refs) * len(got.labels)
			wantFamilies[f.Family] = want
		}
		want := projection.PerCluster[cl.Cluster.Key]
		if want.Entities != clusterEntities || want.Series != clusterSeries {
			t.Errorf("%s: projection %+v, SOAP entities=%d series=%d", cl.Cluster.Key, want, clusterEntities, clusterSeries)
		}
		allEntities += clusterEntities
		allSeries += clusterSeries
	}
	if projection.All.Entities != allEntities || projection.All.Series != allSeries || projection.EmptyCalls != emptyCalls {
		t.Errorf("total %+v empty calls %d; SOAP entities=%d series=%d empty calls=%d", projection.All, projection.EmptyCalls, allEntities, allSeries, emptyCalls)
	}
	for _, f := range projection.Families {
		want := wantFamilies[f.Family]
		if f.Objects != want.Entities || f.Series != want.Series {
			t.Errorf("%s: projection objects=%d series=%d, SOAP objects=%d series=%d", f.Family, f.Objects, f.Series, want.Entities, want.Series)
		}
		if f.Family == "disk-group" && (f.Objects != 3 || f.Series != 3*258 || f.Behaviour != "mixed") {
			t.Errorf("disk-group: %+v", f)
		}
		if f.Family == "vsan-esa-disk-layer" && (f.Objects != 3 || f.Behaviour != "mixed") {
			t.Errorf("ESA disk layer: %+v", f)
		}
	}
}
