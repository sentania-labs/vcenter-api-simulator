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
	faults  []scenario.Fault
	latency scenario.Latency
	now     func() time.Time

	mu  sync.Mutex
	rng *rand.Rand
}

// NewKnobs builds the knobs from the scenario sections.
func NewKnobs(faults []scenario.Fault, latency scenario.Latency, now func() time.Time) *Knobs {
	return &Knobs{faults: faults, latency: latency, now: now, rng: rand.New(rand.NewPCG(1, 2))}
}

// FamilyFault returns the fault to inject for a wildcard query of family
// on cluster key, if one is active now.
func (k *Knobs) FamilyFault(family, cluster string) *scenario.Fault {
	return k.active(func(f scenario.Fault) bool { return f.Family == family }, cluster)
}

// MethodFault returns the fault to inject for a method on cluster key, if
// one is active now.
func (k *Knobs) MethodFault(method, cluster string) *scenario.Fault {
	return k.active(func(f scenario.Fault) bool { return f.Method == method }, cluster)
}

func (k *Knobs) active(match func(scenario.Fault) bool, cluster string) *scenario.Fault {
	if k == nil {
		return nil
	}
	now := k.now().UTC()
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
func (k *Knobs) delay(family string) time.Duration {
	d, ok := k.latency.Families[family]
	if !ok {
		if k.latency.PerfQuery == nil {
			return 0
		}
		d = *k.latency.PerfQuery
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
	cluster := ""
	if c := clusterRe.FindSubmatch(body); c != nil {
		if cl := e.clusterByValue(string(c[1])); cl != nil {
			cluster = cl.Key
		}
	} else if h := hostRe.FindSubmatch(body); h != nil {
		if _, cl := e.Host(vim25types.ManagedObjectReference{Type: "HostSystem", Value: string(h[1])}); cl != nil {
			cluster = cl.Key
		}
	}
	var hold time.Duration
	if method == "VsanPerfQueryPerf" {
		family := ""
		if f := familyRe.FindSubmatch(body); f != nil {
			family = string(f[1])
		}
		hold += k.delay(family)
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
