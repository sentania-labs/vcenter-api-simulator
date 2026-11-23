package sim_test

import (
	"context"
	"testing"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

var powerCounterNames = []string{
	"power.power", "power.energy", "power.powerCap",
	"power.capacity.usable", "power.capacity.usage", "power.capacity.usagePct",
	"power.capacity.usageIdle", "power.capacity.usageSystem", "power.capacity.usageVm",
}

func TestPowerCountersAddUp(t *testing.T) {
	e := start(t, "power")
	ctx := context.Background()
	pm := *e.client.ServiceContent.PerfManager
	var perf mo.PerformanceManager
	if err := property.DefaultCollector(e.client).RetrieveOne(ctx, pm, []string{"perfCounter"}, &perf); err != nil {
		t.Fatal(err)
	}
	keys := map[string]int32{}
	for _, c := range perf.PerfCounter {
		keys[c.GroupInfo.GetElementDescription().Key+"."+c.NameInfo.GetElementDescription().Key] = c.Key
	}
	for _, name := range powerCounterNames {
		if keys[name] == 0 {
			t.Fatalf("counter %s is not in perfCounter", name)
		}
	}
	byLevel, err := methods.QueryPerfCounterByLevel(ctx, e.client, &types.QueryPerfCounterByLevel{This: pm, Level: 4})
	if err != nil {
		t.Fatal(err)
	}
	leveled := map[int32]bool{}
	for _, c := range byLevel.Returnval {
		leveled[c.Key] = true
	}
	for _, name := range powerCounterNames {
		if !leveled[keys[name]] {
			t.Fatalf("counter %s is missing from QueryPerfCounterByLevel(4)", name)
		}
	}

	advertised := func(ref types.ManagedObjectReference) map[int32]bool {
		r, err := methods.QueryAvailablePerfMetric(ctx, e.client, &types.QueryAvailablePerfMetric{This: pm, Entity: ref, IntervalId: 20})
		if err != nil {
			t.Fatal(err)
		}
		out := map[int32]bool{}
		for _, id := range r.Returnval {
			out[id.CounterId] = true
		}
		return out
	}
	host := e.cl().Hosts[0]
	onHost := advertised(host)
	for _, name := range powerCounterNames {
		if !onHost[keys[name]] {
			t.Fatalf("host does not advertise %s", name)
		}
	}
	if advertised(e.cl().Ref)[keys["power.power"]] {
		t.Fatal("cluster advertises power.power")
	}

	sample := func(ref types.ManagedObjectReference, names ...string) map[string]int64 {
		var ids []types.PerfMetricId
		for _, name := range names {
			ids = append(ids, types.PerfMetricId{CounterId: keys[name]})
		}
		r, err := methods.QueryPerf(ctx, e.client, &types.QueryPerf{This: pm, QuerySpec: []types.PerfQuerySpec{{Entity: ref, IntervalId: 20, MaxSample: 1, MetricId: ids}}})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, m := range r.Returnval {
			for _, v := range m.(*types.PerfEntityMetric).Value {
				s := v.(*types.PerfMetricIntSeries)
				for name, key := range keys {
					if key == s.Id.CounterId && len(s.Value) > 0 {
						out[name] = s.Value[len(s.Value)-1]
					}
				}
			}
		}
		return out
	}

	h := sample(host, powerCounterNames...)
	t.Logf("host power counters: %v", h)
	if h["power.power"] <= 0 || h["power.power"] != h["power.capacity.usage"] {
		t.Fatalf("host power %d and capacity usage %d", h["power.power"], h["power.capacity.usage"])
	}
	if got := h["power.capacity.usageIdle"] + h["power.capacity.usageSystem"] + h["power.capacity.usageVm"]; got != h["power.capacity.usage"] {
		t.Fatalf("idle + system + VM = %d, usage %d", got, h["power.capacity.usage"])
	}
	if h["power.energy"] != h["power.power"]*20 {
		t.Fatalf("energy %d J is not 20 s of %d W", h["power.energy"], h["power.power"])
	}
	if h["power.capacity.usagePct"] != h["power.capacity.usage"]*10000/h["power.capacity.usable"] {
		t.Fatalf("usagePct %d for %d of %d W", h["power.capacity.usagePct"], h["power.capacity.usage"], h["power.capacity.usable"])
	}

	// The VMs on the host add up to its VM power, give or take a watt of
	// rounding per VM.
	var vms []mo.VirtualMachine
	if err := property.DefaultCollector(e.client).Retrieve(ctx, e.cl().VMs, []string{"runtime.host", "runtime.powerState"}, &vms); err != nil {
		t.Fatal(err)
	}
	sum, count := int64(0), int64(0)
	for _, vm := range vms {
		if vm.Runtime.Host == nil || *vm.Runtime.Host != host || vm.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOn {
			continue
		}
		v := sample(vm.Self, "power.power", "power.energy")
		if v["power.energy"] != v["power.power"]*20 {
			t.Fatalf("VM %s energy %d J is not 20 s of %d W", vm.Self.Value, v["power.energy"], v["power.power"])
		}
		sum += v["power.power"]
		count++
	}
	if count == 0 {
		t.Fatal("no powered-on VM on the host")
	}
	if d := h["power.capacity.usageVm"] - sum; d < 0 || d > count {
		t.Fatalf("VMs draw %d W, host VM power %d W", sum, h["power.capacity.usageVm"])
	}
}
