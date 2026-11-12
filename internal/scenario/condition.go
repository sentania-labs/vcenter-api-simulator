package scenario

import (
	"fmt"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
)

// Condition kinds: what goes wrong.
const (
	CondSlow     = "slow"     // answers arrive later
	CondError    = "error"    // answers are an error
	CondMissing  = "missing"  // the simulator says the data does not exist
	CondTimeout  = "timeout"  // no answer at all
	CondPerfDown = "perfdown" // the vSAN performance service is switched off
)

// Which requests a condition applies to.
const (
	WhichPerf   = "perf"   // every vSAN performance query
	WhichOne    = "one"    // performance queries of one entity family
	WhichInv    = "inv"    // inventory requests on /sdk (slowness only)
	WhichMethod = "method" // one other /vsanHealth method, by name (scenario files only)
)

// Condition is one thing switched on to make the simulator misbehave on
// purpose: slowness, a fault, or the performance service of some clusters
// switched off. The console writes them; a scenario file may too.
type Condition struct {
	ID     int    `yaml:"id,omitempty" json:"id,omitempty"`
	Kind   string `yaml:"kind" json:"kind"`
	Which  string `yaml:"which,omitempty" json:"which,omitempty"`
	Family string `yaml:"family,omitempty" json:"family,omitempty"`
	Method string `yaml:"method,omitempty" json:"method,omitempty"`
	// Fault overrides the fault a missing or error condition answers with
	// (one of the Fault kinds); empty takes the kind's usual one.
	Fault    string   `yaml:"fault,omitempty" json:"fault,omitempty"`
	Clusters []string `yaml:"clusters,omitempty" json:"clusters,omitempty"`
	FixedMs  int      `yaml:"fixedMs,omitempty" json:"fixedMs,omitempty"`
	JitterMs int      `yaml:"jitterMs,omitempty" json:"jitterMs,omitempty"`
	// From is when it was switched on; To, when set, is when it turns
	// itself off.
	From *time.Time `yaml:"from,omitempty" json:"from,omitempty"`
	To   *time.Time `yaml:"to,omitempty" json:"to,omitempty"`
	// Snap widens the window to five-minute bucket boundaries, as the
	// windows of scenario-file fault rules are.
	Snap bool `yaml:"snap,omitempty" json:"snap,omitempty"`
}

// bucket is the Performance Service's interval; package perf imports this
// package, so the constant is repeated here.
const bucket = 5 * time.Minute

// ActiveAt reports whether the condition applies at t.
func (c Condition) ActiveAt(t time.Time) bool {
	return c.Started(t) && !c.Ended(t)
}

// Started reports whether the condition's window has opened at t.
func (c Condition) Started(t time.Time) bool {
	if c.From == nil {
		return true
	}
	from := c.From.UTC()
	if c.Snap {
		from = from.Truncate(bucket)
	}
	return !t.UTC().Before(from)
}

// Ended reports whether the condition has turned itself off at t.
func (c Condition) Ended(t time.Time) bool {
	if c.To == nil {
		return false
	}
	to := c.To.UTC()
	if c.Snap {
		if b := to.Truncate(bucket); !b.Equal(to) {
			to = b.Add(bucket)
		}
	}
	return !t.UTC().Before(to)
}

// Selects reports whether the condition applies to a cluster key; no
// clusters means every cluster.
func (c Condition) Selects(cluster string) bool {
	if len(c.Clusters) == 0 {
		return true
	}
	for _, k := range c.Clusters {
		if k == cluster {
			return true
		}
	}
	return false
}

// FaultKind is the Fault kind an error, missing or timeout condition
// answers with.
func (c Condition) FaultKind() string {
	if c.Fault != "" {
		return c.Fault
	}
	switch c.Kind {
	case CondError:
		return FaultSystemError
	case CondMissing:
		return FaultInvalidArgument
	case CondTimeout:
		return FaultTimeout
	}
	return ""
}

func (s *Scenario) validateConditions(cat *catalog.Catalog, generated map[string]bool) error {
	ids := map[int]bool{}
	for i, c := range s.Conditions {
		where := fmt.Sprintf("scenario: conditions[%d]", i)
		if c.ID != 0 {
			if ids[c.ID] {
				return fmt.Errorf("%s: id %d is used twice", where, c.ID)
			}
			ids[c.ID] = true
		}
		switch c.Kind {
		case CondSlow:
			if c.Which != WhichPerf && c.Which != WhichOne && c.Which != WhichInv {
				return fmt.Errorf("%s: slowness applies to perf, one or inv", where)
			}
			if c.FixedMs < 0 || c.JitterMs < 0 || c.FixedMs+c.JitterMs == 0 {
				return fmt.Errorf("%s: slowness needs a delay above zero", where)
			}
		case CondError, CondMissing, CondTimeout:
			if c.Which != WhichPerf && c.Which != WhichOne && c.Which != WhichMethod {
				return fmt.Errorf("%s: a fault applies to perf, one or method", where)
			}
		case CondPerfDown:
			if c.Which != "" || c.Family != "" || c.Method != "" {
				return fmt.Errorf("%s: perfdown takes only clusters", where)
			}
		default:
			return fmt.Errorf("%s: kind %q is not one of %s, %s, %s, %s, %s", where, c.Kind, CondSlow, CondError, CondMissing, CondTimeout, CondPerfDown)
		}
		if (c.Which == WhichOne) != (c.Family != "") {
			return fmt.Errorf("%s: family goes with which: one", where)
		}
		if c.Family != "" {
			if _, ok := cat.Families[c.Family]; !ok {
				return fmt.Errorf("%s: %q is not an entity family the service advertises", where, c.Family)
			}
		}
		if (c.Which == WhichMethod) != (c.Method != "") {
			return fmt.Errorf("%s: method goes with which: method", where)
		}
		if c.Method != "" && !FaultMethods[c.Method] {
			return fmt.Errorf("%s: method %q takes no injected fault", where, c.Method)
		}
		switch c.Fault {
		case "", FaultInvalidArgument, FaultNotSupported, FaultSystemError, FaultTimeout:
		default:
			return fmt.Errorf("%s: fault %q is not a fault kind", where, c.Fault)
		}
		for _, k := range c.Clusters {
			if !generated[k] {
				return fmt.Errorf("%s: %q is not a cluster the datacenters generate", where, k)
			}
		}
		if c.From != nil && c.To != nil && c.To.Before(*c.From) {
			return fmt.Errorf("%s: to is before from", where)
		}
	}
	return nil
}
