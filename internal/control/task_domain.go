package control

import (
	"encoding/json"
	"fmt"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// DNS domain: the domain generated host names, the hosts' own DNS settings
// and VM guest host names live in.

func init() {
	tasks["domain"] = taskDef{form: formDomain, plan: planDomain}
}

type domainParams struct {
	Domain string `json:"domain"`
}

func formDomain(c *Controller, _ map[string]string, _ Caller) (*Form, error) {
	cur := c.applied().sc.DNSDomain()
	return &Form{Title: "Change the DNS domain", Crumbs: [][2]string{{"Clusters", "/clusters"}},
		Intro:   fmt.Sprintf("Host names are made under %s now, for example esx01.cluster-01.dc-01.%s.", cur, cur),
		Values:  map[string]any{"domain": cur},
		Options: map[string]any{"current": cur, "default": scenario.DefaultDomain}}, nil
}

func planDomain(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in domainParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	old := c.applied()
	cur := old.sc.DNSDomain()
	p := &Plan{Go: "Change the DNS domain", dest: "/clusters"}
	next, err := scenario.NormalizeDomain(in.Domain)
	if err != nil {
		p.bad("domain", "%s.", upper1(err.Error()))
		p.Idle = "Type a DNS domain, for example lab.example.com, and this box will say what happens."
		return p, nil
	}
	if next == cur {
		p.bad("domain", "The DNS domain is already %s. Type a different one, or press Cancel.", cur)
		p.Idle = "Nothing is different yet. Type another domain and this box will say what happens."
		return p, nil
	}
	p.Sum = append(p.Sum,
		say("Host names, the hosts' own DNS and search domain, their time servers and VM guest host names change from %s to %s.", bold(cur), bold(next)),
		say("Hosts and VMs keep their ids. A tool that is watching them sees the same objects with new names, not new objects."),
		say("Tools that are signed in are signed out and sign in again by themselves."),
		note("Anything you typed that holds the old domain, such as an NFS server address, is left as you typed it."))
	p.text = fmt.Sprintf("Change the DNS domain from %s to %s", cur, next)
	p.apply = func() (*Outcome, error) {
		sc := clone(old.sc)
		sc.Domain = ""
		if next != scenario.DefaultDomain {
			sc.Domain = next
		}
		if _, err := c.applyScenario(sc, false); err != nil {
			return nil, err
		}
		return &Outcome{Done: fmt.Sprintf("Host names are now made under %s.", next)}, nil
	}
	return p, nil
}
