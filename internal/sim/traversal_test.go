package sim_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/types"
)

func TestTraversalType(t *testing.T) {
	e := start(t, "traversal-types")
	ctx := context.Background()
	cluster := e.cl()
	vm := cluster.VMs[0]
	read := func(root types.ManagedObjectReference, selection ...types.BaseSelectionSpec) []types.ObjectContent {
		t.Helper()
		r, err := methods.RetrievePropertiesEx(ctx, e.client, &types.RetrievePropertiesEx{
			This: e.client.ServiceContent.PropertyCollector,
			SpecSet: []types.PropertyFilterSpec{{
				PropSet:   []types.PropertySpec{{Type: "ManagedEntity", PathSet: []string{"name"}}},
				ObjectSet: []types.ObjectSpec{{Obj: root, Skip: types.NewBool(true), SelectSet: selection}},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if r.Returnval == nil {
			return nil
		}
		return r.Returnval.Objects
	}
	traversal := func(typ, path string) *types.TraversalSpec {
		return &types.TraversalSpec{Type: typ, Path: path, Skip: types.NewBool(false)}
	}
	for _, typ := range []string{"ComputeResource", "ClusterComputeResource"} {
		got := read(cluster.Ref, traversal(typ, "host"))
		if len(got) != len(cluster.Hosts) {
			t.Fatalf("%s.host: got %d hosts", typ, len(got))
		}
	}
	// ManagedEntity is a transitive base of ClusterComputeResource as well as VM.
	for _, root := range []types.ManagedObjectReference{cluster.Ref, vm} {
		if got := read(root, traversal("ManagedEntity", "parent")); len(got) != 1 {
			t.Fatalf("%s superclass parent: %+v", root, got)
		}
	}
	for _, typ := range []string{"ComputeResource", "HostSystem", "NoSuchType"} {
		if got := read(vm, traversal(typ, "resourcePool")); len(got) != 0 {
			t.Fatalf("wrong type %s traversed VM.resourcePool: %+v", typ, got)
		}
	}
	if got := read(vm, traversal("VirtualMachine", "resourcePool")); len(got) != 1 {
		t.Fatalf("valid VM.resourcePool: %+v", got)
	}
	// A finite selection can revisit an object through a cycle. Duplicate paths
	// must still emit the object once, including when a named spec is reused.
	toVM := traversal("HostSystem", "vm")
	back := traversal("VirtualMachine", "runtime.host")
	back.Skip = types.NewBool(true)
	back.SelectSet = []types.BaseSelectionSpec{toVM}
	first := traversal("HostSystem", "vm")
	first.Name = "hostVMs"
	first.SelectSet = []types.BaseSelectionSpec{back}
	got := read(cluster.Hosts[0], first, &types.SelectionSpec{Name: "hostVMs"})
	want := read(cluster.Hosts[0], toVM)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cycle/duplicate changed values: got %+v want %+v", got, want)
	}
	// This recursive selection would loop VM -> pool -> VM if the wrong-type
	// ComputeResource edge were followed on a VM.
	falseEdge := traversal("ComputeResource", "resourcePool")
	falseEdge.Name = "crToRp"
	poolVM := traversal("ResourcePool", "vm")
	poolVM.SelectSet = []types.BaseSelectionSpec{&types.SelectionSpec{Name: "crToRp"}}
	falseEdge.SelectSet = []types.BaseSelectionSpec{poolVM}
	if got := read(vm, falseEdge); len(got) != 0 {
		t.Fatalf("wrong-type cycle followed: %+v", got)
	}
}
