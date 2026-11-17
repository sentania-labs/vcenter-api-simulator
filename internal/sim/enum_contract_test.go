package sim_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/types"
)

// Every value returned by a full collection must satisfy the vSphere enum
// schema. Optional zero-value fields are omitted on the wire; required and
// explicitly requested scalar enums must be valid. This catches empty enums
// throughout nested runtime/config/summary objects, not just one known field.
func TestFullCollectionHasValidEnums(t *testing.T) {
	e := start(t, "enum-contract")
	ctx := context.Background()
	v, err := view.NewManager(e.client).CreateContainerView(ctx, e.client.ServiceContent.RootFolder, []string{"VirtualMachine", "HostSystem", "ClusterComputeResource", "Datastore", "Datacenter", "ResourcePool"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Destroy(ctx)
	refs, err := v.Find(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var objects []types.ObjectContent
	if err := property.DefaultCollector(e.client).Retrieve(ctx, refs, nil, &objects); err != nil {
		t.Fatal(err)
	}
	if len(objects) != len(refs) || len(objects) == 0 {
		t.Fatalf("incomplete collection: %d objects for %d references", len(objects), len(refs))
	}
	failures := map[string]bool{}
	var scan func(reflect.Value, string)
	scan = func(v reflect.Value, path string) {
		if !v.IsValid() {
			return
		}
		if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
			if !v.IsNil() {
				scan(v.Elem(), path)
			}
			return
		}
		if v.CanInterface() {
			if enum, ok := v.Interface().(interface{ Strings() []string }); ok && v.Kind() == reflect.String {
				valid := false
				for _, choice := range enum.Strings() {
					if choice == v.String() {
						valid = true
						break
					}
				}
				if !valid {
					key := path + " (" + v.Type().Name() + ")"
					if !failures[key] {
						failures[key] = true
						t.Errorf("invalid emitted enum at %s: %q; allowed=%v", key, v.String(), enum.Strings())
					}
				}
				return
			}
		}
		switch v.Kind() {
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				f := v.Type().Field(i)
				if f.PkgPath != "" {
					continue
				}
				tag := f.Tag.Get("xml")
				if tag == "-" {
					continue
				}
				name := strings.Split(tag, ",")[0]
				if name == "" {
					name = f.Name
				}
				value := v.Field(i)
				if strings.Contains(tag, ",omitempty") && value.IsZero() {
					continue
				}
				scan(value, path+"."+name)
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				scan(v.Index(i), path+"[]")
			}
		}
	}
	for _, obj := range objects {
		for _, prop := range obj.PropSet {
			scan(reflect.ValueOf(prop.Val), obj.Obj.Type+"."+prop.Name)
		}
	}
}
