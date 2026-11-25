package sim

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
func (s *Simulator) installStoragePerformance(_ map[string]*simulator.Datastore) {
	ctx := s.model.Service.Context
	base, ok := ctx.Map.Get(*s.model.ServiceContent.PerfManager).(*simulator.PerformanceManager)
	if !ok {
		return
	}
	p := &storagePerformance{PerformanceManager: base, seed: s.Topology.TelemetrySeed, now: s.Env.Now}
	if p.seed == "" {
		p.seed = s.Topology.Seed
	}
	p.PerfCounter = append([]types.PerfCounterInfo(nil), base.PerfCounter...)
	p.PerfCounter = append(p.PerfCounter, guestCounters()...)
	p.PerfCounter = append(p.PerfCounter, powerCounters(p.PerfCounter)...)
	p.HistoricalInterval = append([]types.PerfInterval(nil), base.HistoricalInterval...)
	for i := range p.HistoricalInterval {
		p.HistoricalInterval[i].Key = int32(i + 1)
		p.HistoricalInterval[i].Level = 4
	}
	ctx.Map.Put(p)
}

func counterInstances(c types.PerfCounterInfo, n *workloadNode, state *performanceState) []string {
	instances := []string{""}
	group := c.GroupInfo.GetElementDescription().Key
	switch group {
	case "cpu":
		if n.ref.Type == "VirtualMachine" || n.ref.Type == "HostSystem" {
			for i := 0; i < n.cpuInstances; i++ {
				instances = append(instances, strconv.Itoa(i))
			}
		}
	case "net":
		instances = append(instances, n.nics...)
	case "virtualDisk":
		instances = append(instances, n.disks...)
	case "disk":
		if n.ref.Type == "HostSystem" {
			instances = append(instances, n.disks...)
		}
	case "datastore":
		if n.ref.Type == "HostSystem" || n.ref.Type == "VirtualMachine" {
			for _, ref := range n.datastores {
				if ds := state.nodes[ref]; ds != nil {
					instances = append(instances, ds.identity)
				}
			}
		}
	}
	return instances
}

// intensiveCounter reports counters whose instance values equal the aggregate
// rather than partition it: percentages and non-summed time (latency) values.
func intensiveCounter(c types.PerfCounterInfo) bool {
	switch c.UnitInfo.GetElementDescription().Key {
	case "percent":
		return true
	case "millisecond", "microsecond":
		return c.RollupType != types.PerfSummaryTypeSummation
	}
	return false
}
func perfInterval(spec types.PerfQuerySpec) (int32, time.Duration, bool) {
	interval := spec.IntervalId
	if interval == 0 || interval == -1 {
		if spec.Entity.Type == "VirtualMachine" || spec.Entity.Type == "HostSystem" || spec.Entity.Type == "ResourcePool" {
			interval = 20
		} else {
			interval = 300
		}
	}
	switch interval {
	case 20:
		return interval, time.Hour, true
	case 300:
		return interval, 24 * time.Hour, true
	case 1800:
		return interval, 7 * 24 * time.Hour, true
	case 7200:
		return interval, 30 * 24 * time.Hour, true
	case 86400:
		return interval, 365 * 24 * time.Hour, true
	}
	return interval, 0, false
}
func sampleTimes(spec types.PerfQuerySpec, now time.Time) ([]time.Time, int32, bool) {
	interval, retention, ok := perfInterval(spec)
	if !ok || spec.MaxSample < 0 {
		return nil, interval, false
	}
	step := time.Duration(interval) * time.Second
	end := now.UTC().Truncate(step)
	if spec.EndTime != nil && spec.EndTime.Before(end) {
		end = spec.EndTime.UTC().Truncate(step)
	}
	start := now.Add(-retention)
	if spec.StartTime != nil {
		start = *spec.StartTime
	}
	if spec.EndTime != nil && spec.StartTime != nil && spec.EndTime.Before(*spec.StartTime) {
		return nil, interval, false
	}
	oldest := now.Add(-retention)
	if start.Before(oldest) {
		start = oldest
	}
	// vSphere startTime is exclusive; endTime is inclusive.
	first := start.UTC().Truncate(step).Add(step)
	if spec.MaxSample > 0 && int64(spec.MaxSample) < int64(end.Sub(first)/step)+1 {
		maxStart := end.Add(-time.Duration(spec.MaxSample-1) * step)
		if first.Before(maxStart) {
			first = maxStart
		}
	}
	var out []time.Time
	for t := first; !t.After(end); t = t.Add(step) {
		out = append(out, t)
	}
	return out, interval, true
}
func (p *storagePerformance) QueryAvailablePerfMetric(ctx *simulator.Context, req *types.QueryAvailablePerfMetric) soap.HasFault {
	state := readPerformanceSubset(ctx, &req.Entity)
	n := state.nodes[req.Entity]
	body := &methods.QueryAvailablePerfMetricBody{Res: &types.QueryAvailablePerfMetricResponse{}}
	_, _, validInterval := perfInterval(types.PerfQuerySpec{Entity: req.Entity, IntervalId: req.IntervalId})
	if !validInterval {
		body.Fault_ = simulator.Fault("Unsupported performance interval", &types.InvalidArgument{InvalidProperty: "intervalId"})
		return body
	}
	if n == nil {
		body.Fault_ = simulator.Fault("Unknown performance entity", &types.InvalidArgument{InvalidProperty: "entity"})
		return body
	}
	if req.IntervalId == 20 && n.ref.Type != "HostSystem" && n.ref.Type != "VirtualMachine" && n.ref.Type != "ResourcePool" {
		return body
	}
	if n.ref.Type == "VirtualMachine" && !n.active {
		return body
	}
	for _, c := range p.PerfCounter {
		if supportsCounter(c, n) {
			for _, instance := range counterInstances(c, n, state) {
				body.Res.Returnval = append(body.Res.Returnval, types.PerfMetricId{CounterId: c.Key, Instance: instance})
			}
		}
	}
	return body
}

type plannedMetric struct {
	counter      types.PerfCounterInfo
	id           types.PerfMetricId
	index, count int
	values       []int64
}
type plannedQuery struct {
	node    *workloadNode
	info    []types.PerfSampleInfo
	metrics []plannedMetric
	format  string
}
type sampleTask struct {
	query, index int
	last         bool
	ticks        int64
}

func (p *storagePerformance) query(ctx *simulator.Context, specs []types.PerfQuerySpec) ([]storagePerfEntity, *soap.Fault) {
	state := readPerformanceState(ctx)
	counters := map[int32]types.PerfCounterInfo{}
	for _, c := range p.PerfCounter {
		counters[c.Key] = c
	}
	plans := make([]plannedQuery, len(specs))
	tasks := map[int64][]sampleTask{}
	invalid := func(field string) ([]storagePerfEntity, *soap.Fault) {
		return nil, simulator.Fault("Invalid or excessive performance query", &types.InvalidArgument{InvalidProperty: field})
	}
	cells := 0
	var evaluations int64
	for qi, spec := range specs {
		n := state.nodes[spec.Entity]
		if n == nil {
			return invalid("entity")
		}
		if spec.Format != "" && spec.Format != "normal" && spec.Format != "csv" {
			return invalid("format")
		}
		times, interval, ok := sampleTimes(spec, p.now())
		if !ok {
			return invalid("intervalId")
		}
		if int64(len(times))*int64(interval/20)*int64(len(state.nodes)) > 100000000 {
			return invalid("querySpec")
		}
		plan := &plans[qi]
		plan.node = n
		plan.format = spec.Format
		if (n.ref.Type == "VirtualMachine" && !n.active) || (interval == 20 && n.ref.Type != "VirtualMachine" && n.ref.Type != "HostSystem" && n.ref.Type != "ResourcePool") {
			continue
		}
		for _, t := range times {
			plan.info = append(plan.info, types.PerfSampleInfo{Timestamp: t, Interval: interval})
		}
		requested := spec.MetricId
		if len(requested) == 0 {
			for _, c := range p.PerfCounter {
				if supportsCounter(c, n) {
					requested = append(requested, types.PerfMetricId{CounterId: c.Key})
				}
			}
		}
		seen := map[types.PerfMetricId]bool{}
		for _, id := range requested {
			c, ok := counters[id.CounterId]
			if !ok || !supportsCounter(c, n) {
				continue
			}
			instances := counterInstances(c, n, state)
			for index, instance := range instances {
				if id.Instance != "*" && id.Instance != instance {
					continue
				}
				mid := types.PerfMetricId{CounterId: id.CounterId, Instance: instance}
				if seen[mid] {
					continue
				}
				seen[mid] = true
				cells += len(times)
				if cells > 2000000 {
					return invalid("querySpec")
				}
				plan.metrics = append(plan.metrics, plannedMetric{counter: c, id: mid, index: index - 1, count: len(instances) - 1, values: make([]int64, len(times))})
			}
		}
		if len(plan.metrics) == 0 {
			continue
		}
		evaluations += int64(len(plan.metrics)) * int64(len(times)) * int64(interval/20)
		if evaluations > 30000000 {
			return invalid("querySpec")
		}
		for ti, t := range times {
			ticks := int64(interval / 20)
			for offset := ticks - 1; offset >= 0; offset-- {
				stamp := t.Unix() - offset*20
				tasks[stamp] = append(tasks[stamp], sampleTask{qi, ti, offset == 0, ticks})
			}
		}
	}
	// Bound adversarial queries before evaluating or allocating sample maps.
	// Clients can split wider historical requests into smaller windows.
	if int64(len(tasks))*int64(len(state.nodes)) > 100000000 {
		return invalid("querySpec")
	}
	stamps := make([]int64, 0, len(tasks))
	for stamp := range tasks {
		stamps = append(stamps, stamp)
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i] < stamps[j] })
	for _, stamp := range stamps {
		frame := state.sample(p.seed, time.Unix(stamp, 0))
		for _, task := range tasks[stamp] {
			plan := &plans[task.query]
			w := frame[plan.node.ref]
			for mi := range plan.metrics {
				m := &plan.metrics[mi]
				measurement := w
				datastoreInstance := m.id.Instance != "" && m.counter.GroupInfo.GetElementDescription().Key == "datastore"
				if datastoreInstance {
					for _, ref := range plan.node.datastores {
						if ds := state.nodes[ref]; ds != nil && ds.identity == m.id.Instance {
							measurement = frame[datastoreInstanceRef(plan.node.ref, ref)]
							break
						}
					}
				}
				if powerUnavailable(counterName(m.counter), plan.node) {
					m.values[task.index] = -1 // no sample, not 0 W or 0 J, whatever the rollup
					continue
				}
				v, _ := valueAt(m.counter, plan.node, measurement)
				if m.id.Instance != "" && m.count > 0 && !datastoreInstance {
					if !intensiveCounter(m.counter) {
						v = v*int64(m.index+1)/int64(m.count) - v*int64(m.index)/int64(m.count)
					}
				}
				dest := &m.values[task.index]
				first := stamp == plan.info[task.index].Timestamp.Unix()-(task.ticks-1)*20
				switch m.counter.RollupType {
				case types.PerfSummaryTypeLatest, types.PerfSummaryTypeNone:
					if task.last {
						*dest = v
					}
				case types.PerfSummaryTypeMinimum:
					if first || v < *dest {
						*dest = v
					}
				case types.PerfSummaryTypeMaximum:
					if first || v > *dest {
						*dest = v
					}
				default:
					*dest += v
				}
				if task.last && m.counter.RollupType == types.PerfSummaryTypeAverage {
					*dest /= task.ticks
				}
			}
		}
	}
	result := make([]storagePerfEntity, 0, len(plans))
	for _, plan := range plans {
		if plan.format == "csv" {
			m := &types.PerfEntityMetricCSV{PerfEntityMetricBase: types.PerfEntityMetricBase{Entity: plan.node.ref}}
			info := []string{}
			for _, s := range plan.info {
				info = append(info, itoa(int64(s.Interval)), s.Timestamp.UTC().Format(time.RFC3339))
			}
			m.SampleInfoCSV = strings.Join(info, ",")
			for _, metric := range plan.metrics {
				v := make([]string, len(metric.values))
				for i, n := range metric.values {
					v[i] = itoa(n)
				}
				m.Value = append(m.Value, types.PerfMetricSeriesCSV{PerfMetricSeries: types.PerfMetricSeries{Id: metric.id}, Value: strings.Join(v, ",")})
			}
			result = append(result, storagePerfEntity{metric: m})
		} else {
			m := &types.PerfEntityMetric{PerfEntityMetricBase: types.PerfEntityMetricBase{Entity: plan.node.ref}, SampleInfo: plan.info}
			for _, metric := range plan.metrics {
				m.Value = append(m.Value, &types.PerfMetricIntSeries{PerfMetricSeries: types.PerfMetricSeries{Id: metric.id}, Value: metric.values})
			}
			result = append(result, storagePerfEntity{metric: m})
		}
	}
	return result, nil
}
func (p *storagePerformance) QueryPerf(ctx *simulator.Context, req *types.QueryPerf) soap.HasFault {
	entries, fault := p.query(ctx, req.QuerySpec)
	return &storagePerfBody{Res: &storagePerfResponse{Returnval: entries}, Fault_: fault}
}

type compositePerfBody struct {
	Res    *compositePerfResponse `xml:"urn:vim25 QueryPerfCompositeResponse,omitempty"`
	Fault_ *soap.Fault            `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault,omitempty"`
}
type compositePerfResponse struct {
	Returnval compositePerfResult `xml:"returnval"`
}
type compositePerfResult struct {
	Entity   storagePerfEntity   `xml:"entity"`
	Children []storagePerfEntity `xml:"childEntity,omitempty"`
}

func (b *compositePerfBody) Fault() *soap.Fault { return b.Fault_ }
func (p *storagePerformance) QueryPerfComposite(ctx *simulator.Context, req *types.QueryPerfComposite) soap.HasFault {
	state := readPerformanceState(ctx)
	specs := []types.PerfQuerySpec{req.QuerySpec}
	var children []perfRef
	for ref, n := range state.nodes {
		matches := n.parent == req.QuerySpec.Entity
		switch req.QuerySpec.Entity.Type {
		case "ClusterComputeResource":
			matches = matches && ref.Type == "HostSystem"
		case "Datacenter":
			matches = matches && ref.Type == "ClusterComputeResource"
		case "HostSystem":
			matches = ref.Type == "VirtualMachine" && n.host == req.QuerySpec.Entity
		}
		if matches {
			children = append(children, ref)
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].String() < children[j].String() })
	for _, ref := range children {
		spec := req.QuerySpec
		spec.Entity = ref
		specs = append(specs, spec)
	}
	entries, fault := p.query(ctx, specs)
	body := &compositePerfBody{Fault_: fault}
	if fault == nil {
		body.Res = &compositePerfResponse{Returnval: compositePerfResult{Entity: entries[0], Children: entries[1:]}}
	}
	return body
}
