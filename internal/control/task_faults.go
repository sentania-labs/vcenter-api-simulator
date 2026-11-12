package control

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

func init() {
	tasks["fault"] = taskDef{form: formFault, plan: planFault}
}

type faultParams struct {
	Kind   string   `json:"kind"`
	Which  string   `json:"which"`
	One    string   `json:"one"`
	Ms     N        `json:"ms"`
	Jitter N        `json:"jitter"`
	Where  string   `json:"where"`
	Pick   []string `json:"pick"`
	// Until is off (until it is turned off), 15m, 1h or today. DayEnd is
	// the end of today on the viewer's clock, which the simulator does not
	// know.
	Until  string     `json:"until"`
	DayEnd *time.Time `json:"dayEnd"`
}

// untilWords are the "for how long" choices, as the summary says them.
var untilWords = map[string]string{"15m": "for 15 minutes", "1h": "for 1 hour", "today": "until the end of today"}

func formFault(c *Controller, q map[string]string, _ Caller) (*Form, error) {
	r := c.applied()
	v := map[string]any{"kind": scenario.CondSlow, "which": scenario.WhichPerf, "one": "vsan-pnic-net", "ms": 200, "jitter": 50, "where": "all", "pick": []string{}, "until": "off"}
	f := &Form{Title: "Set up slowness or a fault", Crumbs: [][2]string{{"Slowness and faults", "/faults"}},
		Intro: `Choose what goes wrong. The "What will happen" box says in plain words what you have set up.`, Values: v}
	for _, ready := range readyMades {
		if ready.Key != q["preset"] {
			continue
		}
		f.Title = ready.Name
		f.Intro = "Nothing is switched on yet. The boxes below are filled in for you: press Switch it on, or adjust them first."
		v["kind"] = ready.kind
		if ready.which != "" {
			v["which"] = ready.which
		}
		if ready.family != "" {
			v["one"] = ready.family
		}
		if ready.kind == scenario.CondPerfDown {
			v["where"], v["pick"] = "some", []string{r.topo.Clusters()[0].Key}
		}
	}
	var clusters [][2]string
	for _, cl := range r.topo.Clusters() {
		clusters = append(clusters, [2]string{cl.Key, clusterLabel(r.sc, cl.Key)})
	}
	var main, more [][2]string
	for _, fam := range mainFamilies {
		main = append(main, [2]string{fam, familyNames[fam]})
	}
	for fam, name := range familyNames {
		if _, known := c.cat.Families[fam]; known && !slices.Contains(mainFamilies, fam) {
			more = append(more, [2]string{fam, name})
		}
	}
	sort.Slice(more, func(i, j int) bool { return more[i][1] < more[j][1] })
	f.Options = map[string]any{"clusters": clusters, "main": main, "more": more, "kinds": len(c.cat.Families)}
	return f, nil
}

func planFault(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in faultParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	old := c.applied()
	p := &Plan{Go: "Switch it on", dest: "/faults"}
	cond := scenario.Condition{Kind: in.Kind}
	switch in.Kind {
	case scenario.CondSlow:
		if !in.Ms.in(1, 60000) {
			p.bad("ms", "Enter how much slower, from 1 to 60,000 milliseconds.")
		}
		if in.Jitter.OK && !in.Jitter.in(0, 60000) {
			p.bad("jitter", "Enter a variation from 0 to 60,000 milliseconds.")
		}
		cond.FixedMs, cond.JitterMs = in.Ms.V, in.Jitter.V
		cond.Which = in.Which
	case scenario.CondError, scenario.CondMissing, scenario.CondTimeout:
		cond.Which = in.Which
		if cond.Which == scenario.WhichInv {
			cond.Which = scenario.WhichPerf
		}
	case scenario.CondPerfDown:
	default:
		p.bad("kind", "Choose what should go wrong.")
		return p, nil
	}
	if cond.Kind != scenario.CondPerfDown {
		switch cond.Which {
		case scenario.WhichOne:
			if _, ok := c.cat.Families[in.One]; !ok {
				p.bad("one", "Choose a kind from the list.")
			}
			cond.Family = in.One
		case scenario.WhichInv, scenario.WhichPerf:
		default:
			cond.Which = scenario.WhichPerf
		}
	}
	everywhere := cond.Kind == scenario.CondSlow && cond.Which == scenario.WhichInv
	if !everywhere && in.Where == "some" {
		for _, k := range old.sc.ClusterKeys() {
			if slices.Contains(in.Pick, k) {
				cond.Clusters = append(cond.Clusters, k)
			}
		}
		if len(cond.Clusters) == 0 {
			p.bad("pick", `Tick at least one cluster, or choose "Every cluster".`)
		}
	}
	text := condText(old.sc, cond)
	if !everywhere && in.Where == "some" && len(cond.Clusters) == 0 {
		text = strings.TrimSuffix(strings.TrimSuffix(text, "."), ", on every cluster") + ", on the clusters you tick."
	}
	lasting := note("It stays on until you turn it off, here or on Home.")
	if words, ok := untilWords[in.Until]; ok {
		lasting = note("It turns itself off: %s. You can also turn it off sooner.", words)
	} else {
		in.Until = "off"
	}
	p.Sum = []Item{say("%s", bold(text)), say("%s", toolAdvice[cond.Kind]), lasting, note("Clusters, hosts and VMs are not changed.")}
	if len(p.Errors) > 0 {
		return p, nil
	}
	p.text = "Switch on: " + lower1(strings.TrimSuffix(text, "."))
	p.apply = func() (*Outcome, error) {
		now := c.now()
		cond.From = &now
		switch in.Until {
		case "15m":
			to := now.Add(15 * time.Minute)
			cond.To = &to
		case "1h":
			to := now.Add(time.Hour)
			cond.To = &to
		case "today":
			// the end of the viewer's day; without one, the next midnight UTC
			to := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
			if in.DayEnd != nil && in.DayEnd.After(now) && in.DayEnd.Sub(now) <= 24*time.Hour {
				to = in.DayEnd.UTC()
			}
			cond.To = &to
		}
		c.seq++
		cond.ID = c.seq
		next := clone(c.applied().sc)
		next.Conditions = append(next.Conditions, cond)
		if _, err := c.applyScenario(next, false); err != nil {
			return nil, err
		}
		return &Outcome{Done: fmt.Sprintf(`Switched on: %s It is in the "Switched on now" list, with a button to turn it off.`, lower1(text)),
			history: "Switched on: " + lower1(text)}, nil
	}
	return p, nil
}
