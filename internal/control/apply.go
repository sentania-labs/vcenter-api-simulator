package control

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// drainGrace is how long the replaced vCenter keeps answering requests that
// were already in flight at the swap.
var drainGrace = 30 * time.Second

// Failure is a change that was not made. It stays on every screen until it
// is hidden or tried again, and carries the change's own values so that
// "Try again" can reopen it filled in.
type Failure struct {
	At   time.Time `json:"at"`
	What string    `json:"what"`
	Why  string    `json:"why"`
	Fix  string    `json:"fix"`
	// Kind and Params are the task that failed. Passwords and keys are
	// left out of Params.
	Kind   string          `json:"kind"`
	Params json.RawMessage `json:"params"`
	// Route is the task screen to reopen.
	Route string `json:"route"`
}

// applyScenario makes next the served scenario. A change to clusters, hosts,
// storage or versions builds a new vCenter beside the running one and swaps
// them, which ends the sessions of connected tools; a change to slowness and
// faults alone swaps the knobs in place. On error nothing has changed.
// c.mu is held.
func (c *Controller) applyScenario(next *scenario.Scenario, freshRegistry bool) (rebuilt bool, err error) {
	old := c.applied()
	next = clone(next)
	dropVanishedHostVersions(next)
	if err := clone(next).Validate(); err != nil {
		return false, &Invalid{err.Error()}
	}
	if !freshRegistry && topologyForm(old.sc) == topologyForm(next) {
		nr := &running{version: old.version, at: old.at, sc: next, topo: old.topo, sim: old.sim}
		old.sim.Env.Knobs.Set(next.Faults, next.Latency)
		old.sim.Env.Knobs.SetConditions(next.Conditions)
		c.cur.Store(nr)
		c.syncConditions(nr)
		c.armExpiry()
		c.save()
		return false, nil
	}
	var newReg *registry.Registry
	if freshRegistry || next.Seed != old.sc.Seed {
		if newReg, err = registry.Open(c.regPath(), next.Seed, true); err != nil {
			return false, err
		}
	} else {
		newReg = c.reg.Clone()
	}
	nr, err := c.build(next, newReg, old.version+1)
	if err != nil {
		return false, err
	}
	if err := newReg.Save(); err != nil {
		nr.sim.Close()
		return false, err
	}
	c.reg = newReg
	c.cur.Store(nr)
	c.syncConditions(nr)
	c.armExpiry()
	c.drain(old.sim)
	c.save()
	return true, nil
}

func (c *Controller) regPath() string { return c.opts.DataDir + "/registry.json" }

// save writes the state file. A change that is already live is not undone
// or called a failure because it could not be saved: Home says that the
// state is unsaved until a later save succeeds.
func (c *Controller) save() {
	if err := c.persist(); err != nil {
		c.unsaved = err.Error()
		log.Printf("control: state not saved: %v", err)
		return
	}
	c.unsaved = ""
}

// whyFailed turns an apply error into the two sentences of the failure bar:
// why it failed and what to do.
func whyFailed(err error) (why, fix string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "FileFault") || strings.Contains(msg, "no space") || strings.Contains(msg, "quota"):
		return "The machine that runs the simulator is out of disk space.", "Nothing was changed. Free some space there, then try again."
	}
	msg = strings.TrimPrefix(msg, "scenario: ")
	return upper1(strings.TrimSuffix(msg, ".")) + ".", "Nothing was changed. Check the values, then try again."
}

// addHistory appends one line to the history. c.mu is held.
func (c *Controller) addHistory(text string) {
	c.timeline = append(c.timeline, TimelineEntry{At: c.now(), Text: text})
}

// SetPaused stops (true) or resumes (false) the simulated vCenter: while
// paused every connection is dropped, so a collection fails instead of
// reading empty data. Nothing is lost.
func (c *Controller) SetPaused(p bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused.Swap(p) == p {
		return
	}
	if p {
		c.pausedAt = c.now()
		c.addHistory("Simulator paused.")
	} else {
		c.addHistory("Simulator resumed.")
	}
	c.save()
}

// Paused reports whether the API is paused.
func (c *Controller) Paused() bool { return c.paused.Load() }

// DismissFailure hides the failure bar.
func (c *Controller) DismissFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failure = nil
}

// fail records a change that was not made. c.mu is held.
func (c *Controller) fail(text string, err error, kind string, params json.RawMessage, route string) {
	why, fix := whyFailed(err)
	c.failure = &Failure{At: c.now(), What: fmt.Sprintf("The change %q was not made.", text), Why: why, Fix: fix,
		Kind: kind, Params: scrub(kind, params), Route: route}
	c.addHistory(fmt.Sprintf("Failed: %s. %s Nothing was changed.", lower1(text), why))
	c.save()
}

// plainParams lists, by task kind, the values named like a secret that are
// not one and so are kept for "Try again". Every other task loses them all.
var plainParams = map[string]string{"replace-everything": "key"}

// scrub removes passwords and private keys from a task's values before they
// are kept for "Try again".
func scrub(kind string, params json.RawMessage) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(params, &m) != nil {
		return json.RawMessage("{}")
	}
	for _, k := range []string{"pw", "pw2", "cur", "key", "crt"} {
		if plainParams[kind] != k {
			delete(m, k)
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}
