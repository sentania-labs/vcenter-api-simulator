package sim_test

import (
	"context"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// The counter catalog defines provisioned cores as virtual processors. A pool
// must count running VMs in every descendant pool, and usage must agree with
// its MHz utilization. Exercise both through SOAP, including power changes.
func TestResourcePoolCoreCountersIncludeNestedRunningVMs(t *testing.T) {
	e := start(t, "pool-core-counters")
	ctx := context.Background()
	root := object.NewResourcePool(e.client, firstOf(t, e, "ResourcePool"))
	var props mo.ResourcePool
	if err := property.DefaultCollector(e.client).RetrieveOne(ctx, root.Reference(), []string{"config"}, &props); err != nil {
		t.Fatal(err)
	}
	parent, err := root.Create(ctx, "parent", props.Config)
	if err != nil {
		t.Fatal(err)
	}
	child, err := parent.Create(ctx, "nested", props.Config)
	if err != nil {
		t.Fatal(err)
	}
	wait := func(task *object.Task, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := task.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var parentCPUs, childCPUs int64
	for i, ref := range e.cl().VMs {
		vm := object.NewVirtualMachine(e.client, ref)
		// Enough processors to exercise nonzero integer core usage as well as MHz.
		wait(vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{NumCPUs: 16}))
		pool := child.Reference()
		if i == 0 {
			pool = parent.Reference()
		} else {
			childCPUs += 16
		}
		parentCPUs += 16
		wait(vm.Relocate(ctx, types.VirtualMachineRelocateSpec{Pool: &pool}, types.VirtualMachineMovePriorityDefaultPriority))
	}
	check := func(pool *object.ResourcePool, wantCPUs int64) int64 {
		t.Helper()
		result, err := methods.QueryPerf(ctx, e.client, &types.QueryPerf{
			This: *e.client.ServiceContent.PerfManager,
			QuerySpec: []types.PerfQuerySpec{{Entity: pool.Reference(), IntervalId: 20, MaxSample: 1,
				MetricId: []types.PerfMetricId{{CounterId: 6}, {CounterId: 15}, {CounterId: 20}, {CounterId: 21}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		values := map[int32]int64{}
		for _, series := range result.Returnval[0].(*types.PerfEntityMetric).Value {
			v := series.(*types.PerfMetricIntSeries)
			if len(v.Value) != 1 {
				t.Fatalf("counter %d has %d samples", v.Id.CounterId, len(v.Value))
			}
			values[v.Id.CounterId] = v.Value[0]
		}
		if len(values) != 4 || values[20] != wantCPUs {
			t.Fatalf("%s: expected %d running vCPUs, got counters %v", pool.Reference(), wantCPUs, values)
		}
		if wantCPUs == 0 {
			if values[6] != 0 || values[15] != 0 || values[21] != 0 {
				t.Fatalf("empty pool retained CPU load: %v", values)
			}
			return 0
		}
		if values[6] <= 0 || values[15] <= 0 {
			t.Fatalf("running pool has no MHz usage/capacity: %v", values)
		}
		wantUsage := values[6] * wantCPUs / values[15]
		if values[21] != wantUsage {
			t.Fatalf("%s: expected %d used cores from MHz utilization, got %v", pool.Reference(), wantUsage, values)
		}
		return values[21]
	}
	if check(parent, parentCPUs) == 0 || check(child, childCPUs) == 0 {
		t.Fatal("fixture must exercise nonzero core usage before powering VMs off")
	}
	// Leave one direct VM running, but power off every nested VM.
	for _, ref := range e.cl().VMs[1:] {
		wait(object.NewVirtualMachine(e.client, ref).PowerOff(ctx))
	}
	check(parent, parentCPUs-childCPUs)
	check(child, 0)
}
