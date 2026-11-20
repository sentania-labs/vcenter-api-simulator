package control

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// Compare complete membership and decoded values, including continuation pages,
// with an independent ContainerView walk over the very same live inventory.
// This oracle also passes with unmodified govmomi v0.56.2 (-mod=mod).
func TestTraversalSnapshotValues(t *testing.T) {
	clk := &clock{t: monday9}
	c := newCtlAt(t, labSize(t), "", "h", clk.now)
	srv := httptest.NewServer(c)
	defer srv.Close()
	client := activityClient(t, srv.URL)
	ctx := context.Background()
	pc := client.ServiceContent.PropertyCollector
	props := []types.PropertySpec{
		{Type: "HostSystem", PathSet: []string{"runtime.inMaintenanceMode", "runtime.connectionState", "runtime.powerState", "vm", "summary.quickStats", "summary.overallStatus"}},
		{Type: "VirtualMachine", PathSet: []string{"name", "runtime.host", "runtime.powerState", "runtime.connectionState", "summary.quickStats", "summary.runtime", "snapshot"}},
		{Type: "Datastore", PathSet: []string{"summary", "info"}},
		{Type: "ClusterComputeResource", PathSet: []string{"name", "host", "summary"}},
	}
	v, err := view.NewManager(client).CreateContainerView(ctx, client.ServiceContent.RootFolder, []string{"HostSystem", "VirtualMachine", "Datastore", "ClusterComputeResource"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Destroy(ctx)
	var roots []mo.Folder
	if err := property.DefaultCollector(client).Retrieve(ctx, []types.ManagedObjectReference{client.ServiceContent.RootFolder}, []string{"childEntity"}, &roots); err != nil {
		t.Fatal(err)
	}
	var objects []types.ObjectSpec
	for _, root := range roots[0].ChildEntity {
		objects = append(objects, types.ObjectSpec{Obj: root, Skip: types.NewBool(false), SelectSet: adapterTraversal()})
	}
	read := func(objects []types.ObjectSpec) map[types.ManagedObjectReference]map[string]any {
		t.Helper()
		r, err := methods.RetrievePropertiesEx(ctx, client, &types.RetrievePropertiesEx{This: pc, SpecSet: []types.PropertyFilterSpec{{PropSet: props, ObjectSet: objects}}, Options: types.RetrieveOptions{MaxObjects: 7}})
		if err != nil {
			t.Fatal(err)
		}
		out := map[types.ManagedObjectReference]map[string]any{}
		page := r.Returnval
		for page != nil {
			for _, o := range page.Objects {
				if _, exists := out[o.Obj]; exists {
					t.Fatalf("duplicate object %s", o.Obj)
				}
				values := map[string]any{}
				for _, p := range o.PropSet {
					values[p.Name] = p.Val
				}
				if len(o.MissingSet) > 0 {
					values["missingSet"] = o.MissingSet
				}
				out[o.Obj] = values
			}
			if page.Token == "" {
				break
			}
			next, err := methods.ContinueRetrievePropertiesEx(ctx, client, &types.ContinueRetrievePropertiesEx{This: pc, Token: page.Token})
			if err != nil {
				t.Fatal(err)
			}
			page = &next.Returnval
		}
		return out
	}
	viewObjects := []types.ObjectSpec{{Obj: v.Reference(), Skip: types.NewBool(true), SelectSet: []types.BaseSelectionSpec{&types.TraversalSpec{Type: "ContainerView", Path: "view", Skip: types.NewBool(false)}}}}
	want := snapshotWithoutReadTimestamp(t, read(viewObjects))
	got := snapshotWithoutReadTimestamp(t, read(objects))
	if len(want) == 0 {
		t.Fatal("empty inventory oracle")
	}
	if !reflect.DeepEqual(got, want) {
		for ref, values := range want {
			if !reflect.DeepEqual(got[ref], values) {
				t.Errorf("membership or values differ for %s: got %#v want %#v", ref, got[ref], values)
			}
		}
		for ref := range got {
			if _, ok := want[ref]; !ok {
				t.Errorf("unexpected object %s", ref)
			}
		}
	}
	stream := func(objects []types.ObjectSpec) map[types.ManagedObjectReference]map[string]any {
		t.Helper()
		created, err := methods.CreatePropertyCollector(ctx, client, &types.CreatePropertyCollector{This: pc})
		if err != nil {
			t.Fatal(err)
		}
		collector := created.Returnval
		defer methods.DestroyPropertyCollector(ctx, client, &types.DestroyPropertyCollector{This: collector})
		if _, err := methods.CreateFilter(ctx, client, &types.CreateFilter{This: collector, Spec: types.PropertyFilterSpec{PropSet: props, ObjectSet: objects}}); err != nil {
			t.Fatal(err)
		}
		out := map[types.ManagedObjectReference]map[string]any{}
		version := ""
		for {
			r, err := methods.WaitForUpdatesEx(ctx, client, &types.WaitForUpdatesEx{This: collector, Version: version, Options: &types.WaitOptions{MaxWaitSeconds: types.NewInt32(0)}})
			if err != nil {
				t.Fatal(err)
			}
			if r.Returnval == nil {
				break
			}
			version = r.Returnval.Version
			for _, f := range r.Returnval.FilterSet {
				for _, o := range f.ObjectSet {
					if _, exists := out[o.Obj]; exists {
						t.Fatalf("duplicate streamed object %s", o.Obj)
					}
					values := map[string]any{}
					for _, change := range o.ChangeSet {
						values[change.Name] = change.Val
					}
					if len(o.MissingSet) > 0 {
						values["missingSet"] = o.MissingSet
					}
					out[o.Obj] = values
				}
			}
			if r.Returnval.Truncated == nil || !*r.Returnval.Truncated {
				break
			}
		}
		return out
	}
	if got, want := snapshotWithoutReadTimestamp(t, stream(objects)), snapshotWithoutReadTimestamp(t, stream(viewObjects)); len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatal("streamed snapshot membership or values differ from ContainerView")
	}
	t.Logf("identical membership and property values for %d objects", len(want))
}

// snapshotWithoutReadTimestamp preserves membership, types, and all other values.
func snapshotWithoutReadTimestamp(t *testing.T, snapshot map[types.ManagedObjectReference]map[string]any) map[types.ManagedObjectReference]map[string]any {
	t.Helper()
	// Exclude only Datastore.info.timestamp: its value is the time of the read, so two snapshots cannot match on it.
	datastores := 0
	for ref, props := range snapshot {
		if ref.Type != "Datastore" {
			continue
		}
		value := reflect.ValueOf(props["info"])
		pointer := value.IsValid() && value.Kind() == reflect.Pointer
		if pointer {
			value = value.Elem()
		}
		if !value.IsValid() || value.Kind() != reflect.Struct {
			t.Fatalf("excluded path Datastore.info.timestamp must exist on %s in each snapshot", ref)
		}
		// Copy the decoded value while preserving whether SOAP returned a pointer.
		clone := reflect.New(value.Type())
		clone.Elem().Set(value)
		info, ok := clone.Interface().(types.BaseDatastoreInfo)
		if !ok || info.GetDatastoreInfo().Timestamp == nil {
			t.Fatalf("excluded path Datastore.info.timestamp must exist on %s in each snapshot", ref)
		}
		info.GetDatastoreInfo().Timestamp = nil
		if pointer {
			props["info"] = clone.Interface()
		} else {
			props["info"] = clone.Elem().Interface()
		}
		datastores++
	}
	if datastores == 0 {
		t.Fatal("excluded path Datastore.info.timestamp must exist in each snapshot")
	}
	return snapshot
}
