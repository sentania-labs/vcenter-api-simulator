package sim

import (
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

type perfRef = types.ManagedObjectReference

// performanceState is the only inventory input to the workload model. A request
// takes a fresh snapshot, so power, maintenance and placement changes take
// effect without a second inventory or a stored telemetry series.
type performanceState struct {
	nodes map[perfRef]*workloadNode
	hosts []perfRef
}
type workloadNode struct {
	ref, parent, host perfRef
	identity          string
	cpu, memory       int64 // MHz, MiB
	cpus              int
	cpuInstances      int // logical host processors, or VM vCPUs
	active            bool
	datastores        []perfRef
	nics, disks       []string
	capacity, used    int64 // datastore KiB
	// loadCPU and loadMem are how loaded Activity says a VM's cluster looks:
	// 1 is as built.
	loadCPU, loadMem float64
	boot              time.Time
}

func readPerformanceState(ctx *simulator.Context) *performanceState {
	return readPerformanceSubset(ctx, nil)
}

// Discovery reads just one provider and its host/datastore dependencies.
// Collection uses the identical reader for the full inventory.
func readPerformanceSubset(ctx *simulator.Context, selected *perfRef) *performanceState {
	s := &performanceState{nodes: map[perfRef]*workloadNode{}}
	for _, kind := range []string{"Datacenter", "ClusterComputeResource", "ResourcePool", "HostSystem", "Datastore", "VirtualMachine"} {
		var entities []mo.Entity
		if selected == nil {
			entities = ctx.Map.All(kind)
		} else if kind == selected.Type {
			if e, ok := ctx.Map.Get(*selected).(mo.Entity); ok {
				entities = []mo.Entity{e}
			}
		}
		for _, entity := range entities {
			n := &workloadNode{ref: entity.Reference(), active: true, loadCPU: 1, loadMem: 1}
			ctx.Map.WithLock(ctx, entity, func() {
				if entity.Entity().Parent != nil {
					n.parent = *entity.Entity().Parent
				}
				n.identity = entity.Entity().Name
				switch o := entity.(type) {
				case *simulator.HostSystem:
					n.active = o.Runtime.ConnectionState == types.HostSystemConnectionStateConnected && !o.Runtime.InMaintenanceMode && o.Runtime.PowerState == types.HostSystemPowerStatePoweredOn
					n.cpu = int64(o.Summary.Hardware.CpuMhz) * int64(o.Summary.Hardware.NumCpuCores)
					n.cpus = int(o.Summary.Hardware.NumCpuCores)
					n.cpuInstances = int(o.Summary.Hardware.NumCpuThreads)
					n.memory = o.Summary.Hardware.MemorySize / (1024 * 1024)
					n.datastores = append([]perfRef(nil), o.Datastore...)
					if o.Runtime.BootTime != nil {
						n.boot = *o.Runtime.BootTime
					}
					if o.Config != nil && o.Config.Network != nil {
						for _, nic := range o.Config.Network.Pnic {
							n.nics = append(n.nics, nic.Device)
						}
					}
					if o.Config != nil && o.Config.StorageDevice != nil {
						for _, lun := range o.Config.StorageDevice.ScsiLun {
							if d, ok := lun.(*types.HostScsiDisk); ok {
								n.disks = append(n.disks, d.CanonicalName)
							}
						}
					}
				case *simulator.VirtualMachine:
					if o.ResourcePool != nil {
						n.parent = *o.ResourcePool
					}
					n.active = o.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn && o.Runtime.ConnectionState == types.VirtualMachineConnectionStateConnected
					if o.Runtime.Host != nil {
						n.host = *o.Runtime.Host
					}
					n.cpus = int(o.Summary.Config.NumCpu)
					n.cpuInstances = n.cpus
					n.memory = int64(o.Summary.Config.MemorySizeMB)
					n.datastores = append([]perfRef(nil), o.Datastore...)
					if o.Runtime.BootTime != nil {
						n.boot = *o.Runtime.BootTime
					}
					if o.Config != nil {
						n.identity = o.Config.Uuid
						for _, device := range o.Config.Hardware.Device {
							if nic, ok := device.(types.BaseVirtualEthernetCard); ok {
								n.nics = append(n.nics, itoa(int64(nic.GetVirtualEthernetCard().Key)))
							}
							if d, ok := device.(*types.VirtualDisk); ok && d.UnitNumber != nil {
								bus := int32(0)
								for _, c := range o.Config.Hardware.Device {
									if ctrl, ok := c.(types.BaseVirtualController); ok && ctrl.GetVirtualController().Key == d.ControllerKey {
										bus = ctrl.GetVirtualController().BusNumber
									}
								}
								n.disks = append(n.disks, "scsi"+itoa(int64(bus))+":"+itoa(int64(*d.UnitNumber)))
							}
						}
					}
				case *simulator.Datastore:
					n.capacity = o.Summary.Capacity / 1024
					n.used = (o.Summary.Capacity - o.Summary.FreeSpace) / 1024
					n.identity = strings.TrimSuffix(strings.TrimPrefix(o.Summary.Url, "ds:///vmfs/volumes/"), "/")
				}
			})
			if kind == "HostSystem" {
				s.hosts = append(s.hosts, n.ref)
			}
			s.nodes[n.ref] = n
		}
	}
	if selected != nil {
		if n := s.nodes[*selected]; n != nil {
			deps := append([]perfRef(nil), n.datastores...)
			if n.host.Type != "" {
				deps = append(deps, n.host)
			}
			for _, ref := range deps {
				part := readPerformanceSubset(ctx, &ref)
				for r, node := range part.nodes {
					s.nodes[r] = node
				}
			}
		}
	}
	// Containers are projected through their real inventory ancestry. Folder
	// nodes need no measurements but connect clusters to their datacenters.
	for _, n := range s.nodes {
		if n.ref.Type == "ClusterComputeResource" {
			if dc := parentDatacenter(ctx, n.ref); dc != nil {
				n.parent = *dc
			}
		}
		if n.ref.Type == "VirtualMachine" {
			if h := s.nodes[n.host]; h != nil {
				n.cpu = int64(n.cpus) * h.cpu / int64(max(h.cpus, 1))
				n.active = n.active && h.active
				n.loadCPU, n.loadMem = loadOf(ctx, h.parent)
			} else {
				n.active = false
			}
		}
	}
	for _, ref := range s.hosts {
		h := s.nodes[ref]
		if c := s.nodes[h.parent]; c != nil && h.active {
			c.cpus += h.cpus
		}
	}
	for ref, c := range s.nodes {
		if ref.Type == "ClusterComputeResource" {
			if d := s.nodes[c.parent]; d != nil {
				d.cpus += c.cpus
			}
		}
	}
	sort.Slice(s.hosts, func(i, j int) bool { return s.hosts[i].Value < s.hosts[j].Value })
	return s
}

type workload struct {
	cpu, demand, memory, activeMemory int64
	read, write, rx, tx               int64 // KiB/s
	ready, wait, usedCPU              int64 // milliseconds per 20 seconds, summed over vCPUs
	latency                           int64 // microseconds, weighted by IOPS at aggregation
	ios                               int64
	cpuCap, memCap, vcpus             int64
}

func (w *workload) add(v workload) {
	w.cpu += v.cpu
	w.demand += v.demand
	w.memory += v.memory
	w.activeMemory += v.activeMemory
	w.read += v.read
	w.write += v.write
	w.rx += v.rx
	w.tx += v.tx
	w.ready += v.ready
	w.wait += v.wait
	w.usedCPU += v.usedCPU
	w.latency += v.latency
	w.ios += v.ios
	w.vcpus += v.vcpus
}
func seedNumber(seed, identity string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(seed))
	h.Write([]byte{0})
	h.Write([]byte(identity))
	return h.Sum64()
}

// VM demand follows a continuous daily and weekly curve. Cohorts range from
// idle to batch-heavy. A brief, seeded daily batch window supplies occasional
// ready-time and latency spikes without random or restart-dependent values.
func vmWorkload(seed string, n *workloadNode, t time.Time, cpuScale, memScale float64) workload {
	w := workload{cpuCap: n.cpu, memCap: n.memory}
	if !n.active {
		return w
	}
	h := seedNumber(seed, n.identity)
	seconds := float64(t.Unix())
	phase := float64(h % 86400)
	day := 0.65 + 0.25*math.Sin(2*math.Pi*(seconds-8*3600)/86400)
	week := 0.85 + 0.15*math.Cos(2*math.Pi*seconds/(7*86400))
	drift := 0.85 + 0.15*math.Sin(2*math.Pi*(seconds+phase)/2700)
	level := []float64{0.025, 0.18, 0.28, 0.42, 0.60, 0.80}[h%6]
	pulse := math.Pow(math.Max(0, math.Cos(2*math.Pi*(seconds+phase)/86400)), 120)
	base := math.Min(.95, level*day*week*drift+.25*pulse)
	if n.loadCPU == 0 {
		n.loadCPU = 1
	}
	if n.loadMem == 0 {
		n.loadMem = 1
	}
	// Activity's CPU dial scales what the VM uses; a busier place also
	// contends more, so ready time rises faster than usage does
	usage := math.Min(.95, base*n.loadCPU)
	readyPct := .002 + .008*usage + .09*pulse + .06*math.Max(0, n.loadCPU-1)*usage
	w.cpu = int64(float64(n.cpu) * usage * cpuScale)
	w.demand = min(n.cpu, int64(float64(w.cpu)*(1+readyPct)))
	w.memory = min(n.memory, int64(float64(n.memory)*(.23+.55*base)*memScale*n.loadMem))
	w.activeMemory = int64(float64(w.memory) * (.45 + .4*usage))
	w.read = int64((16 + float64(h%400) + usage*10000) * (1 + pulse))
	w.write = int64(float64(w.read) * (.3 + float64(h%70)/100))
	w.rx = int64(32 + usage*3000 + float64(h%300))
	w.tx = int64(float64(w.rx) * .6)
	w.vcpus = int64(n.cpus)
	w.usedCPU = int64(usage * cpuScale * 20000 * float64(n.cpus))
	w.ready = int64(readyPct * 20000 * float64(n.cpus))
	w.wait = max(0, 20000*int64(n.cpus)-w.usedCPU-w.ready)
	w.ios = max(1, (w.read+w.write)/32)
	w.latency = int64(900+4000*usage+35000*pulse) * w.ios
	return w
}

// sample computes each VM once and sums the same integer values into its host,
// cluster, datacenter and datastores. Capacity budgets prevent overcommit from
// manufacturing usage above physical capacity. The snapshot is request-local.
func (s *performanceState) sample(seed string, t time.Time) map[perfRef]workload {
	out := make(map[perfRef]workload, len(s.nodes))
	budgets := map[perfRef][2]int64{}
	for _, n := range s.nodes {
		if n.ref.Type == "VirtualMachine" && n.active {
			b := budgets[n.host]
			b[0] += n.cpu
			b[1] += n.memory
			budgets[n.host] = b
		}
	}
	for _, n := range s.nodes {
		if n.ref.Type != "VirtualMachine" {
			continue
		}
		cpuScale, memScale := 1.0, 1.0
		if h := s.nodes[n.host]; h != nil {
			b := budgets[n.host]
			cpuScale = math.Min(1, float64(h.cpu)/float64(max(1, b[0])))
			memScale = math.Min(1, float64(h.memory)/float64(max(1, b[1])))
		}
		w := vmWorkload(seed, n, t, cpuScale, memScale)
		out[n.ref] = w
		if n.active {
			h := out[n.host]
			h.add(w)
			out[n.host] = h
			for i, ref := range n.datastores {
				// Exact integer partition: all datastore shares sum to the VM's I/O.
				d := out[ref]
				share := w
				count := int64(len(n.datastores))
				index := int64(i)
				split := func(v int64) int64 { return v*(index+1)/count - v*index/count }
				share.read = split(w.read)
				share.write = split(w.write)
				share.ios = split(w.ios)
				share.latency = split(w.latency)
				d.add(share)
				out[ref] = d
				out[datastoreInstanceRef(n.ref, ref)] = share
				instance := datastoreInstanceRef(n.host, ref)
				hostStore := out[instance]
				hostStore.add(share)
				out[instance] = hostStore
			}
			// Resource pools may be nested; their capacity is demand entitlement.
			ref := n.parent
			for depth := 0; depth < 32 && ref.Type == "ResourcePool"; depth++ {
				pool := out[ref]
				pool.add(w)
				pool.cpuCap += n.cpu
				pool.memCap += n.memory
				out[ref] = pool
				if p := s.nodes[ref]; p != nil {
					ref = p.parent
				} else {
					break
				}
			}
		}
	}
	for _, ref := range s.hosts {
		n := s.nodes[ref]
		w := out[ref]
		w.cpuCap = n.cpu
		w.memCap = n.memory
		out[ref] = w
		if c := s.nodes[n.parent]; c != nil {
			cw := out[c.ref]
			cw.add(w)
			if n.active {
				cw.cpuCap += n.cpu
				cw.memCap += n.memory
			}
			out[c.ref] = cw
		}
	}
	for ref, n := range s.nodes {
		if ref.Type == "ClusterComputeResource" {
			w := out[ref]
			if n.parent.Type == "Datacenter" {
				d := out[n.parent]
				d.add(w)
				d.cpuCap += w.cpuCap
				d.memCap += w.memCap
				out[n.parent] = d
			}
		}
	}
	return out
}

func datastoreInstanceRef(parent, store perfRef) perfRef {
	return perfRef{Type: "DatastoreInstance", Value: parent.String() + "/" + store.Value}
}
