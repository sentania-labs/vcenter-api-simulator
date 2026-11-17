package sim

import (
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"
	"reflect"
	"testing"
	"time"
)

func workloadFixture() *performanceState {
	s := &performanceState{nodes: map[perfRef]*workloadNode{}}
	dc := perfRef{Type: "Datacenter", Value: "dc"}
	cl := perfRef{Type: "ClusterComputeResource", Value: "cl"}
	h := perfRef{Type: "HostSystem", Value: "host"}
	ds := perfRef{Type: "Datastore", Value: "ds"}
	s.nodes[dc] = &workloadNode{ref: dc}
	s.nodes[cl] = &workloadNode{ref: cl, parent: dc}
	s.nodes[h] = &workloadNode{ref: h, parent: cl, cpu: 8000, memory: 16000, cpus: 4, active: true}
	s.hosts = []perfRef{h}
	s.nodes[ds] = &workloadNode{ref: ds}
	for i := 0; i < 12; i++ {
		r := perfRef{Type: "VirtualMachine", Value: itoa(int64(i))}
		s.nodes[r] = &workloadNode{ref: r, host: h, identity: r.Value, cpu: 6000, cpus: 3, memory: 8000, active: true, datastores: []perfRef{ds}}
	}
	return s
}

// Independent invariants: conservation and physical capacity, including an
// overcommitted host. A per-entity random generator or unbounded demand fails.
func TestWorkloadConservesLoadAcrossDay(t *testing.T) {
	s := workloadFixture()
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	varied := map[int64]bool{}
	spikes := false
	for minute := 0; minute < 1440; minute += 5 {
		at := start.Add(time.Duration(minute) * time.Minute)
		frame := s.sample("test", at)
		if !reflect.DeepEqual(frame, s.sample("test", at)) {
			t.Fatal("repeat changed")
		}
		sum := workload{}
		for ref, w := range frame {
			if ref.Type == "VirtualMachine" {
				sum.add(w)
				if w.cpu > w.cpuCap || w.memory > w.memCap {
					t.Fatal("VM exceeds capacity")
				}
				if w.ready > 1000*w.vcpus {
					spikes = true
				}
			}
		}
		for _, kind := range []string{"HostSystem", "ClusterComputeResource", "Datacenter"} {
			for ref, w := range frame {
				if ref.Type != kind {
					continue
				}
				if w.cpu != sum.cpu || w.memory != sum.memory || w.read != sum.read || w.write != sum.write || w.rx != sum.rx || w.ready != sum.ready {
					t.Fatalf("%s does not conserve: %+v / %+v", kind, w, sum)
				}
				if w.cpu > w.cpuCap || w.memory > w.memCap {
					t.Fatal("physical capacity exceeded")
				}
			}
		}
		for _, ref := range s.hosts {
			var reads int64
			for store := range s.nodes {
				if store.Type == "Datastore" {
					reads += frame[datastoreInstanceRef(ref, store)].read
				}
			}
			if reads != frame[ref].read {
				t.Fatal("host/datastore instances lost I/O")
			}
		}
		varied[sum.cpu] = true
	}
	if len(varied) < 100 || !spikes {
		t.Fatalf("missing daily variation or contention: %d, %v", len(varied), spikes)
	}
}
func TestWorkloadFollowsPlacementAndPower(t *testing.T) {
	s := workloadFixture()
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	a := s.hosts[0]
	b := perfRef{Type: "HostSystem", Value: "host2"}
	clone := *s.nodes[a]
	clone.ref = b
	s.nodes[b] = &clone
	s.hosts = append(s.hosts, b)
	vm := perfRef{Type: "VirtualMachine", Value: "0"}
	before := s.sample("test", at)
	s.nodes[vm].host = b
	after := s.sample("test", at)
	if after[b].cpu == 0 || after[a].read != before[a].read-before[vm].read || after[b].read != after[vm].read {
		t.Fatal("load did not move")
	}
	s.nodes[vm].active = false
	off := s.sample("test", at)
	if off[vm].cpu != 0 || off[b].cpu != 0 || off[vm].read != 0 {
		t.Fatal("powered-off VM retained load")
	}
}

func TestPerformanceWindowBounds(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	past := now.Add(-10 * time.Minute)
	future := now.Add(time.Hour)
	spec := types.PerfQuerySpec{Entity: perfRef{Type: "HostSystem"}, IntervalId: 20, MaxSample: 3, StartTime: &past, EndTime: &future}
	times, interval, ok := sampleTimes(spec, now)
	if !ok || interval != 20 || len(times) != 3 || times[2] != now || times[0] != now.Add(-40*time.Second) {
		t.Fatal("window bounds", times)
	}
	spec.StartTime = &future
	spec.EndTime = &past
	if _, _, ok := sampleTimes(spec, now); ok {
		t.Fatal("reversed window accepted")
	}
	spec.StartTime = &past
	spec.EndTime = &now
	spec.IntervalId = 13
	if _, _, ok := sampleTimes(spec, now); ok {
		t.Fatal("unsupported interval accepted")
	}
	for _, seconds := range []int32{300, 1800, 7200, 86400} {
		spec.IntervalId = seconds
		spec.StartTime = nil
		spec.MaxSample = 1
		times, interval, ok := sampleTimes(spec, now)
		if !ok || len(times) != 1 || times[0].Unix()%int64(seconds) != 0 || interval != seconds {
			t.Fatal("historical interval", seconds, times)
		}
	}
}

func TestPerformanceStateReadsRuntime(t *testing.T) {
	sim, err := New(Options{Topology: topology.Phase1("runtime-workload")})
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()
	ctx := sim.model.Service.Context
	host := ctx.Map.Get(sim.Clusters[0].Hosts[0]).(*simulator.HostSystem)
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	initial := readPerformanceState(ctx).sample("runtime", at)
	if initial[host.Self].cpu == 0 {
		t.Fatal("host has no initial load")
	}
	ctx.Map.WithLock(ctx, host, func() { host.Runtime.InMaintenanceMode = true })
	frame := readPerformanceState(ctx).sample("runtime", at)
	if frame[host.Self].cpu != 0 || frame[host.Self].read != 0 {
		t.Fatal("maintenance retained VM load")
	}
	ctx.Map.WithLock(ctx, host, func() {
		host.Runtime.InMaintenanceMode = false
		host.Runtime.ConnectionState = types.HostSystemConnectionStateDisconnected
	})
	frame = readPerformanceState(ctx).sample("runtime", at)
	if frame[host.Self].cpu != 0 || frame[host.Self].rx != 0 {
		t.Fatal("disconnected host retained VM load")
	}
	ctx.Map.WithLock(ctx, host, func() { host.Runtime.ConnectionState = types.HostSystemConnectionStateConnected })
	var vm *simulator.VirtualMachine
	for _, obj := range ctx.Map.All("VirtualMachine") {
		candidate := obj.(*simulator.VirtualMachine)
		if candidate.Runtime.Host != nil && *candidate.Runtime.Host == host.Self {
			vm = candidate
			break
		}
	}
	if vm == nil {
		t.Fatal("missing VM")
	}
	target := sim.Clusters[0].Hosts[1]
	ctx.Map.WithLock(ctx, vm, func() { vm.Runtime.Host = &target })
	state := readPerformanceState(ctx)
	frame = state.sample("runtime", at)
	if state.nodes[vm.Self].host != target || frame[host.Self].cpu != 0 || frame[target].read <= initial[target].read {
		t.Fatal("runtime placement ignored")
	}
	ctx.Map.WithLock(ctx, vm, func() { vm.Runtime.PowerState = types.VirtualMachinePowerStatePoweredOff })
	state = readPerformanceState(ctx)
	frame = state.sample("runtime", at)
	if state.nodes[vm.Self].active || frame[vm.Self].cpu != 0 {
		t.Fatal("power state ignored")
	}
}
