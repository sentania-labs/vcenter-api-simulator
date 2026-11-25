package control

import (
	"strings"
	"testing"
)

// Changing the DNS domain renames hosts and keeps every object identity,
// says so before it is done, refuses what is not a DNS name, and is still
// in force after the simulator starts again with the same scenario file.
func TestChangingTheDomainRenamesAndKeepsIdentities(t *testing.T) {
	dir := t.TempDir()
	c := newCtl(t, small(t), dir, "t")
	before := identities(c)
	host := func(c *Controller) string { return c.applied().topo.Clusters()[0].Hosts[0].Name }
	if got := host(c); got != "esx01.cluster-01.dc-01.sim.invalid" {
		t.Fatalf("default host name %q", got)
	}

	wantContains(t, preview(t, c, "domain", nil, map[string]any{"domain": "lab.example.com"}),
		"A tool that is watching them sees the same objects with new names, not new objects.")
	for _, bad := range []string{"", "has space.com", "bad_name.com", "-a.com", "a..com", "a.com-", "http://a.com", strings.Repeat("a", 64) + ".com", strings.Repeat("a.", 130) + "com"} {
		r := do(t, c, "domain", nil, map[string]any{"domain": bad})
		if r.Outcome != nil || r.Errors["domain"] == "" {
			t.Errorf("%q was accepted: %+v", bad, r)
		}
	}
	// valid on its own, too long for the names the datacenter generates
	c.mu.Lock()
	long := clone(c.applied().sc)
	c.mu.Unlock()
	long.Datacenters[0].Name = strings.Repeat("d", 63)
	if err := long.CheckNamesFit(strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 62)); err == nil {
		t.Error("an overlong generated name was allowed")
	}
	if r := do(t, c, "domain", nil, map[string]any{"domain": "SIM.invalid."}); r.Outcome != nil {
		t.Error("the domain it already has was changed to itself")
	}

	must(t, c, "domain", nil, map[string]any{"domain": "Lab.Example.com"})
	if got := host(c); got != "esx01.cluster-01.dc-01.lab.example.com" {
		t.Errorf("host name %q", got)
	}
	after := identities(c)
	for k, id := range before {
		if after[k] != id {
			t.Errorf("%s changed identity: %s -> %s", k, id, after[k])
		}
	}
	if len(after) != len(before) {
		t.Errorf("%d identities became %d", len(before), len(after))
	}
	if got := c.Clusters().Domain; got != "lab.example.com" {
		t.Errorf("Clusters says the domain is %q", got)
	}
	c.Close()

	// restart from the same file: the saved console state wins
	c = newCtl(t, small(t), dir, "t")
	if got := host(c); got != "esx01.cluster-01.dc-01.lab.example.com" {
		t.Errorf("after a restart the host is %q", got)
	}
	// and back to the default
	must(t, c, "domain", nil, map[string]any{"domain": "sim.invalid"})
	if got := host(c); got != "esx01.cluster-01.dc-01.sim.invalid" {
		t.Errorf("back to the default: %q", got)
	}
}
