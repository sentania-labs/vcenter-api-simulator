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

// powerSeries asks one host for every power counter over the interval and
// returns each counter's series by name, plus the counters discovery lists.
func powerSeries(t *testing.T, e *env, host types.ManagedObjectReference, interval int32, max int32) (map[string][]int64, map[string]bool) {
	t.Helper()
	ctx := context.Background()
	pm := *e.client.ServiceContent.PerfManager
	var perf mo.PerformanceManager
	if err := property.DefaultCollector(e.client).RetrieveOne(ctx, pm, []string{"perfCounter"}, &perf); err != nil {
		t.Fatal(err)
	}
	names := map[int32]string{}
	var ids []types.PerfMetricId
	for _, c := range perf.PerfCounter {
		if c.GroupInfo.GetElementDescription().Key == "power" {
			names[c.Key] = "power." + c.NameInfo.GetElementDescription().Key
			ids = append(ids, types.PerfMetricId{CounterId: c.Key})
		}
	}
	listed := map[string]bool{}
	avail, err := methods.QueryAvailablePerfMetric(ctx, e.client, &types.QueryAvailablePerfMetric{This: pm, Entity: host, IntervalId: interval})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range avail.Returnval {
		listed[names[id.CounterId]] = true
	}
	r, err := methods.QueryPerf(ctx, e.client, &types.QueryPerf{This: pm, QuerySpec: []types.PerfQuerySpec{{Entity: host, IntervalId: interval, MaxSample: max, MetricId: ids}}})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]int64{}
	for _, m := range r.Returnval {
		for _, v := range m.(*types.PerfEntityMetric).Value {
			s := v.(*types.PerfMetricIntSeries)
			out[names[s.Id.CounterId]] = s.Value
		}
	}
	return out, listed
}

func TestPowerHostInMaintenanceModeDrawsLess(t *testing.T) {
	e := start(t, "power-mm")
	ctx := context.Background()
	host := e.cl().Hosts[0]
	normal, _ := powerSeries(t, e, host, 20, 1)
	task, err := object.NewHostSystem(e.client, host).EnterMaintenanceMode(ctx, 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	for _, interval := range []int32{20, 300} {
		mm, listed := powerSeries(t, e, host, interval, 1)
		for name := range normal {
			if !listed[name] {
				t.Fatalf("interval %d: maintenance host no longer lists %s", interval, name)
			}
		}
		power := mm["power.power"][0]
		if power <= 0 || power >= normal["power.power"][0] {
			t.Fatalf("interval %d: maintenance power %d W, normal %d W: want reduced, not 0", interval, power, normal["power.power"][0])
		}
		if mm["power.capacity.usageVm"][0] != 0 || mm["power.capacity.usageIdle"][0] >= normal["power.capacity.usageIdle"][0] {
			t.Fatalf("interval %d: maintenance idle %d, VM %d", interval, mm["power.capacity.usageIdle"][0], mm["power.capacity.usageVm"][0])
		}
		want := power * 20
		if interval == 300 {
			want *= 15 // the energy of 15 samples, still adding up across the rollup
		}
		if mm["power.energy"][0] != want || mm["power.capacity.usage"][0] != power {
			t.Fatalf("interval %d: energy %d J, usage %d W for power %d W", interval, mm["power.energy"][0], mm["power.capacity.usage"][0], power)
		}
	}
}

func TestPowerCrashedHostHasNoSamples(t *testing.T) {
	e := start(t, "power-crash")
	ctx := context.Background()
	host := e.cl().Hosts[0]
	task, err := object.NewHostSystem(e.client, host).Disconnect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	for _, interval := range []int32{20, 300, 1800} {
		series, listed := powerSeries(t, e, host, interval, 2)
		for _, name := range powerCounterNames {
			if !listed[name] {
				t.Fatalf("interval %d: unreachable host does not list %s", interval, name)
			}
			values := series[name]
			if len(values) == 0 {
				t.Fatalf("interval %d: no series for %s", interval, name)
			}
			for _, v := range values {
				if v != -1 {
					t.Fatalf("interval %d: %s = %d, want -1 (no sample)", interval, name, v)
				}
			}
		}
	}
}
