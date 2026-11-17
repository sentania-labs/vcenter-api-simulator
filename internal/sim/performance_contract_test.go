package sim_test

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func TestPerformanceDayAndRollup(t *testing.T) {
	e := start(t, "performance-day")
	ctx := context.Background()
	pm := *e.client.ServiceContent.PerfManager
	refs := append([]types.ManagedObjectReference(nil), e.cl().VMs...)
	refs = append(refs, e.cl().Hosts...)
	refs = append(refs, e.cl().Ref, firstOf(t, e, "Datacenter"))
	end := captureTime.Truncate(5 * time.Minute)
	begin := end.Add(-23 * time.Hour)
	specs := []types.PerfQuerySpec{}
	for _, ref := range refs {
		specs = append(specs, types.PerfQuerySpec{Entity: ref, IntervalId: 300, StartTime: &begin, EndTime: &end, MetricId: []types.PerfMetricId{{CounterId: 6}, {CounterId: 98}, {CounterId: 125}, {CounterId: 143}, {CounterId: 12}}})
	}
	request := &types.QueryPerf{This: pm, QuerySpec: specs}
	result, err := methods.QueryPerf(ctx, e.client, request)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := methods.QueryPerf(ctx, e.client, request)
	if err != nil || !reflect.DeepEqual(result, repeat) {
		t.Fatal("non-deterministic query", err)
	}
	byRef := map[types.ManagedObjectReference]*types.PerfEntityMetric{}
	for _, entry := range result.Returnval {
		m := entry.(*types.PerfEntityMetric)
		byRef[m.Entity] = m
		if len(m.SampleInfo) != 276 {
			t.Fatalf("timestamps: %d", len(m.SampleInfo))
		}
		for i, s := range m.SampleInfo {
			if s.Interval != 300 || s.Timestamp != begin.Add(time.Duration(i+1)*5*time.Minute) {
				t.Fatal("wrong grid or timestamp order")
			}
		}
	}
	for _, parent := range []types.ManagedObjectReference{e.cl().Ref, refs[len(refs)-1]} {
		m := byRef[parent]
		for counter := range m.Value {
			series := m.Value[counter].(*types.PerfMetricIntSeries)
			varied := map[int64]bool{}
			for tick, v := range series.Value {
				var sum int64
				for _, ref := range e.cl().Hosts {
					sum += byRef[ref].Value[counter].(*types.PerfMetricIntSeries).Value[tick]
				}
				if math.Abs(float64(sum-v)) > float64(len(e.cl().Hosts)) {
					t.Fatal("rollup lost load", parent, counter, sum, v)
				}
				varied[v] = true
			}
			if len(varied) < 10 {
				t.Fatal("flat metric", parent, counter)
			}
		}
	}
	// Historical CPU average is the average of its fifteen realtime values;
	// ready summation is their sum. This catches wrong units and fake rollups.
	start := end.Add(-5 * time.Minute)
	spec := types.PerfQuerySpec{Entity: e.cl().VMs[0], StartTime: &start, EndTime: &end, IntervalId: 20, MetricId: []types.PerfMetricId{{CounterId: 6}, {CounterId: 12}}}
	rt, err := methods.QueryPerf(ctx, e.client, &types.QueryPerf{This: pm, QuerySpec: []types.PerfQuerySpec{spec}})
	if err != nil {
		t.Fatal(err)
	}
	real := rt.Returnval[0].(*types.PerfEntityMetric)
	if len(real.SampleInfo) != 15 {
		t.Fatal("realtime window", len(real.SampleInfo))
	}
	hist := byRef[spec.Entity]
	for i, col := range []int{0, 4} {
		var sum int64
		for _, v := range real.Value[i].(*types.PerfMetricIntSeries).Value {
			sum += v
		}
		if i == 0 {
			sum /= 15
		}
		v := hist.Value[col].(*types.PerfMetricIntSeries).Value[275]
		if v != sum {
			t.Fatal("historical rollup", v, sum)
		}
	}
}

func TestPerformanceDiscoveryInstancesAndQuickStats(t *testing.T) {
	e := start(t, "performance-discovery")
	ctx := context.Background()
	pm := *e.client.ServiceContent.PerfManager
	vm := e.cl().VMs[0]
	host := e.cl().Hosts[0]
	available, err := methods.QueryAvailablePerfMetric(ctx, e.client, &types.QueryAvailablePerfMetric{This: pm, Entity: host, IntervalId: 20})
	if err != nil {
		t.Fatal(err)
	}
	advertised := map[types.PerfMetricId]bool{}
	for _, id := range available.Returnval {
		advertised[id] = true
		if id.CounterId == 10042 {
			t.Fatal("unknown upstream counter leaked")
		}
	}
	var hardware mo.HostSystem
	if err := property.DefaultCollector(e.client).RetrieveOne(ctx, host, []string{"summary.hardware"}, &hardware); err != nil {
		t.Fatal(err)
	}
	cpuInstances := 0
	for id := range advertised {
		if id.CounterId == 6 && id.Instance != "" {
			cpuInstances++
		}
	}
	if cpuInstances != int(hardware.Summary.Hardware.NumCpuThreads) {
		t.Fatalf("CPU instances %d do not match logical processors %d", cpuInstances, hardware.Summary.Hardware.NumCpuThreads)
	}
	r, err := methods.QueryPerf(ctx, e.client, &types.QueryPerf{This: pm, QuerySpec: []types.PerfQuerySpec{{Entity: host, IntervalId: 20, MaxSample: 1, MetricId: []types.PerfMetricId{{CounterId: 143, Instance: "*"}}}, {Entity: vm, IntervalId: 20, MaxSample: 1, MetricId: []types.PerfMetricId{{CounterId: 6}, {CounterId: 20003}}}}})
	if err != nil {
		t.Fatal(err)
	}
	h := r.Returnval[0].(*types.PerfEntityMetric)
	var sum, aggregate int64
	for _, series := range h.Value {
		s := series.(*types.PerfMetricIntSeries)
		if !advertised[s.Id] || s.Id.Instance == "*" {
			t.Fatal("unadvertised instance", s.Id)
		}
		if s.Id.Instance == "" {
			aggregate = s.Value[0]
		} else {
			sum += s.Value[0]
		}
	}
	if sum != aggregate || aggregate == 0 {
		t.Fatal("NIC instances do not conserve", sum, aggregate)
	}
	var summary mo.VirtualMachine
	if err := property.DefaultCollector(e.client).RetrieveOne(ctx, vm, []string{"summary.quickStats", "runtime"}, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Runtime.FaultToleranceState != types.VirtualMachineFaultToleranceStateNotConfigured {
		t.Fatal("invalid runtime fault tolerance enum")
	}
	v := r.Returnval[1].(*types.PerfEntityMetric)
	if int64(summary.Summary.QuickStats.OverallCpuUsage) != v.Value[0].(*types.PerfMetricIntSeries).Value[0] {
		t.Fatal("quickStats disagrees")
	}
	if v.Value[1].(*types.PerfMetricIntSeries).Value[0] <= 0 {
		t.Fatal("guest memory absent")
	}
	composite, err := methods.QueryPerfComposite(ctx, e.client, &types.QueryPerfComposite{This: pm, QuerySpec: types.PerfQuerySpec{Entity: e.cl().Ref, IntervalId: 300, MaxSample: 1, MetricId: []types.PerfMetricId{{CounterId: 6}}}})
	if err != nil || len(composite.Returnval.ChildEntity) != len(e.cl().Hosts) {
		t.Fatal("composite missing children", err)
	}
}
