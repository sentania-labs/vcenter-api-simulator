package vsanhealth

import (
	"math/rand/v2"
	"regexp"
	"sync"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	vim25types "github.com/vmware/govmomi/vim25/types"

	"github.com/sentania/vcenter-api-simulator/internal/perf"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// Knobs are the scenario's failure and latency settings for /vsanHealth:
// faults injected per entity family or per method, narrowed to clusters
// and bounded in time, and per-family response latency.
//
// A fault's From/To are snapped outward to five-minute bucket boundaries
// (From down, To up), so a window is active for every bucket it touches
// and a scheduled change becomes visible on a bucket boundary, as a
// scenario edit does. The clock is the Performance Service's, so a frozen
// test clock sees the same schedule the service does.
//
// Delays never run inside a method handler: vcsim holds a lock on the
// target managed object for the whole call, and the Performance Manager is
// one object for every cluster, so a sleep there would stall every
// cluster's queries. Hold runs in the HTTP handler before dispatch instead.
type Knobs struct {
	now func() time.Time

	cfg     sync.RWMutex // guards faults, latency and conds, which Set replaces live
	faults  []scenario.Fault
	latency scenario.Latency
	conds   []scenario.Condition

	mu  sync.Mutex
	rng *rand.Rand
}

// Set replaces the faults and latency while the service runs. In-flight
// requests finish under the settings they started with.
func (k *Knobs) Set(faults []scenario.Fault, latency scenario.Latency) {
	k.cfg.Lock()
	k.faults, k.latency = faults, latency
	k.cfg.Unlock()
}

// SetConditions replaces the console's conditions while the service runs.
func (k *Knobs) SetConditions(conds []scenario.Condition) {
	k.cfg.Lock()
	k.conds = append([]scenario.Condition(nil), conds...)
	k.cfg.Unlock()
}

// SDKDelay is the /sdk latency in force, if any: the scenario's setting
// plus every inventory slowness condition that is on.
func (k *Knobs) SDKDelay() *scenario.Delay {
	if k == nil {
		return nil
	}
	now := k.now().UTC()
	k.cfg.RLock()
	defer k.cfg.RUnlock()
	var d scenario.Delay
	if k.latency.SDK != nil {
		d = *k.latency.SDK
	}
	for _, c := range k.conds {
		if c.Kind == scenario.CondSlow && c.Which == scenario.WhichInv && c.ActiveAt(now) {
			d.FixedMs += c.FixedMs
			d.JitterMs += c.JitterMs
		}
	}
	if d.FixedMs == 0 && d.JitterMs == 0 {
		return nil
	}
	return &d
}

// NewKnobs builds the knobs from the scenario sections.
func NewKnobs(faults []scenario.Fault, latency scenario.Latency, now func() time.Time) *Knobs {
	return &Knobs{faults: faults, latency: latency, now: now, rng: rand.New(rand.NewPCG(1, 2))}
}

// FamilyFault returns the fault to inject for a wildcard query of family
// on cluster key, if one is active now.
func (k *Knobs) FamilyFault(family, cluster string) *scenario.Fault {
	if f := k.active(func(f scenario.Fault) bool { return f.Family == family }, cluster); f != nil {
		return f
	}
	return k.condFault(cluster, func(c scenario.Condition) bool {
		return c.Which == scenario.WhichPerf || (c.Which == scenario.WhichOne && c.Family == family)
	}, family, "")
}

// MethodFault returns the fault to inject for a method on cluster key, if
// one is active now.
func (k *Knobs) MethodFault(method, cluster string) *scenario.Fault {
	if f := k.active(func(f scenario.Fault) bool { return f.Method == method }, cluster); f != nil {
		return f
	}
	return k.condFault(cluster, func(c scenario.Condition) bool {
		return c.Which == scenario.WhichMethod && c.Method == method
	}, "", method)
}

// condFault is the fault of the first error, missing or timeout condition
// that is on for the cluster and matches. Its Cause names the condition.
func (k *Knobs) condFault(cluster string, match func(scenario.Condition) bool, family, method string) *scenario.Fault {
	if k == nil {
		return nil
	}
	now := k.now().UTC()
	k.cfg.RLock()
	defer k.cfg.RUnlock()
	for _, c := range k.conds {
		kind := c.FaultKind()
		if kind == "" || c.Kind == scenario.CondSlow || c.Kind == scenario.CondPerfDown {
			continue
		}
		if !match(c) || !c.Selects(cluster) || !c.ActiveAt(now) {
			continue
		}
		return &scenario.Fault{Family: family, Method: method, Fault: kind, Cause: c.ID}
	}
	return nil
}

func (k *Knobs) active(match func(scenario.Fault) bool, cluster string) *scenario.Fault {
	if k == nil {
		return nil
	}
	now := k.now().UTC()
	k.cfg.RLock()
	defer k.cfg.RUnlock()
	for i := range k.faults {
		f := &k.faults[i]
		if !match(*f) || !selects(f.Clusters, cluster) {
			continue
		}
		if f.From != nil && now.Before(f.From.UTC().Truncate(perf.Interval)) {
			continue
		}
		if f.To != nil && !now.Before(ceilBucket(f.To.UTC())) {
			continue
		}
		return f
	}
	return nil
}

// ceilBucket rounds up to the next bucket boundary (a boundary stays).
func ceilBucket(t time.Time) time.Time {
	b := t.Truncate(perf.Interval)
	if b.Equal(t) {
		return b
	}
	return b.Add(perf.Interval)
}

func selects(keys []string, cluster string) bool {
	if len(keys) == 0 {
		return true
	}
	for _, k := range keys {
		if k == cluster {
			return true
		}
	}
	return false
}

// delay is the family's latency: the per-family entry, else the perfQuery
// default, else zero.
func (k *Knobs) delay(family, cluster string) time.Duration {
	now := k.now().UTC()
	k.cfg.RLock()
	d, ok := k.latency.Families[family]
	if !ok && k.latency.PerfQuery != nil {
		d = *k.latency.PerfQuery
	}
	added := scenario.SlowFor(k.conds, family, cluster, now)
	d.FixedMs += added.FixedMs
	d.JitterMs += added.JitterMs
	k.cfg.RUnlock()
	if d.FixedMs == 0 && d.JitterMs == 0 {
		return 0
	}
	k.mu.Lock()
	r := k.rng.Float64()
	k.mu.Unlock()
	return d.Duration(r)
}

// TimeoutHold is how long a timeout fault withholds the answer: longer
// than the 120 s per-request timeout the pack sets (recon section 4.3).
// Tests shorten it.
var TimeoutHold = 125 * time.Second

// Fault renders an injected fault as the SOAP fault the live service would
// return for that condition. A timeout fault renders nothing: the hold
// happened in Hold, and the handler then answers normally, too late to
// matter.
func (k *Knobs) Fault(f *scenario.Fault, family string) *soap.Fault {
	switch f.Fault {
	case scenario.FaultInvalidArgument:
		return faultInvalidRef()
	case scenario.FaultNotSupported:
		return faultNotSupportedPMem()
	case scenario.FaultSystemError:
		return faultSystemError(family)
	}
	return nil
}

func faultSystemError(what string) *soap.Fault {
	return simulator.Fault("A general system error occurred: "+what, &vim25types.SystemError{Reason: what})
}

// Request shapes Hold reads out of a /vsanHealth body: the method, the
// wildcard family of a perf query, and the cluster or host the call
// targets.
var (
	methodRe  = regexp.MustCompile(`<(?:[A-Za-z0-9_]+:)?(VsanPerfQueryPerf|VsanPerfQueryNodeInformation|VsanClusterGetConfig|QueryVsanManagedDisks|VsanQueryVcClusterHealthSummary)\b`)
	familyRe  = regexp.MustCompile(`<(?:[A-Za-z0-9_]+:)?entityRefId[^>]*>([^<:]+):`)
	clusterRe = regexp.MustCompile(`<(?:[A-Za-z0-9_]+:)?cluster[^>]*>([^<]+)</`)
	hostRe    = regexp.MustCompile(`<(?:[A-Za-z0-9_]+:)?host[^>]*>([^<]+)</`)
)

// Target reads the entity family a perf query asks for and the cluster key a
// /vsanHealth call targets, for the request log. Either may be empty.
func (e *Env) Target(body []byte) (family, cluster string) {
	if f := familyRe.FindSubmatch(body); f != nil {
		family = string(f[1])
	}
	if c := clusterRe.FindSubmatch(body); c != nil {
		if cl := e.clusterByValue(string(c[1])); cl != nil {
			cluster = cl.Key
		}
	} else if h := hostRe.FindSubmatch(body); h != nil {
		if _, cl := e.Host(vim25types.ManagedObjectReference{Type: "HostSystem", Value: string(h[1])}); cl != nil {
			cluster = cl.Key
		}
	}
	return family, cluster
}

// Families lists every entity family a performance query names, in order.
func (e *Env) Families(body []byte) []string {
	var out []string
	for _, m := range familyRe.FindAllSubmatch(body, -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// Hold returns how long a /vsanHealth request must wait before dispatch:
// the family's latency for a perf query, plus TimeoutHold when a timeout
// fault is active for the family or method on the targeted cluster.
func (e *Env) Hold(body []byte) time.Duration {
	k := e.Knobs
	if k == nil {
		return 0
	}
	m := methodRe.FindSubmatch(body)
	if m == nil {
		return 0
	}
	method := string(m[1])
	family, cluster := e.Target(body)
	var hold time.Duration
	if method == "VsanPerfQueryPerf" {
		hold += k.delay(family, cluster)
		if cluster != "" {
			if f := k.FamilyFault(family, cluster); f != nil && f.Fault == scenario.FaultTimeout {
				hold += TimeoutHold
			}
		}
	} else if cluster != "" {
		if f := k.MethodFault(method, cluster); f != nil && f.Fault == scenario.FaultTimeout {
			hold += TimeoutHold
		}
	}
	return hold
}
