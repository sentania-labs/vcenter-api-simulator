package control

import (
	"fmt"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
)

// ApplyResult says what Apply did.
type ApplyResult struct {
	Scheduled bool      `json:"scheduled"`
	At        time.Time `json:"at"`
	Version   int       `json:"version"`
	Message   string    `json:"message"`
}

// drainGrace is how long the replaced vCenter keeps answering requests that
// were already in flight at the swap.
var drainGrace = 30 * time.Second

// Apply lands the pending draft: when "now", immediately; otherwise (the
// default) at the next five-minute bucket boundary.
func (c *Controller) Apply(when string) (*ApplyResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.draft
	if d == nil {
		if c.scheduled != nil {
			return nil, invalid("a change is already scheduled for %s", c.scheduled.At.UTC().Format("15:04:05Z"))
		}
		return nil, ErrNoDraft
	}
	switch when {
	case "now":
		return c.applyLocked(d)
	case "", "boundary":
		at := nextBoundary(c.now())
		s := &Scheduled{Draft: d, At: at}
		s.timer = time.AfterFunc(at.Sub(c.now()), func() { c.fire(s) })
		c.scheduled, c.draft = s, nil
		return &ApplyResult{Scheduled: true, At: at, Version: c.applied().version + 1,
			Message: "Scheduled for the " + at.Format("15:04") + " bucket"}, nil
	}
	return nil, invalid("when must be %q or %q", "boundary", "now")
}

func (c *Controller) fire(s *Scheduled) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scheduled != s {
		return
	}
	c.scheduled = nil
	if _, err := c.applyLocked(s.Draft); err != nil {
		c.draft = s.Draft // give the change back to the operator; applyLocked recorded the failure
	}
}

// applyLocked builds and swaps. c.mu is held.
func (c *Controller) applyLocked(d *Draft) (*ApplyResult, error) {
	old := c.applied()
	res, err := c.applyOnce(d, old)
	if err != nil {
		// the running vCenter is untouched; say so where the operator will see it
		hint := ""
		if strings.Contains(err.Error(), "FileFault") || strings.Contains(err.Error(), "no space") || strings.Contains(err.Error(), "quota") {
			hint = " The new vCenter could not write its datastore files: check the free space under TMPDIR."
		}
		c.lastErr = fmt.Sprintf("The change failed to apply at %s and was not applied; v%d is still serving. %s%s", c.now().Format("15:04:05Z"), old.version, err.Error(), hint)
		c.lastErrAt = c.now()
		c.timeline = append(c.timeline, TimelineEntry{At: c.now(), Bucket: bucket(c.now()), Version: old.version, Area: "Service",
			Text: "Apply failed: v" + fmt.Sprint(old.version) + " still serving", History: "preserved", Identities: "unchanged"})
		_ = c.persist()
		return nil, err
	}
	return res, nil
}

// applyOnce builds and swaps; on error nothing has changed.
func (c *Controller) applyOnce(d *Draft, old *running) (*ApplyResult, error) {
	rv, err := c.review(d)
	if err != nil {
		return nil, err
	}
	topoChanged := rv.RebuildsVCenter
	next := old.version
	if topoChanged {
		next++ // the version counts topologies; a latency or fault edit is not one
	}
	var nr *running
	var newReg *registry.Registry
	if topoChanged {
		if d.Scenario.Seed != old.sc.Seed {
			newReg, err = registry.Open(c.regPath(), d.Scenario.Seed, true)
			if err != nil {
				return nil, err
			}
		} else {
			newReg = c.reg.Clone()
			for _, k := range d.Regenerate {
				newReg.Retire(k)
			}
			for _, k := range d.Scenario.Removed {
				if !old.sc.IsRemoved(k) {
					newReg.Retire(k)
				}
			}
		}
		nr, err = c.build(clone(d.Scenario), newReg, next)
		if err != nil {
			return nil, fmt.Errorf("apply: %w (the running topology is unchanged)", err)
		}
		if err := newReg.Save(); err != nil {
			nr.sim.Close()
			return nil, err
		}
		c.reg = newReg
	} else {
		// telemetry only: swap the knobs in place, keep the vCenter
		old.sim.Env.Knobs.Set(d.Scenario.Faults, d.Scenario.Latency)
		nr = &running{version: next, at: c.now(), sc: clone(d.Scenario), topo: old.topo, sim: old.sim}
	}
	c.cur.Store(nr)
	if topoChanged {
		go func(s interface{ Close() }) {
			time.Sleep(drainGrace)
			s.Close()
		}(old.sim)
	}
	c.scope = d.Scope
	c.draft, c.lastErr = nil, ""
	c.timeline = append(c.timeline, c.timelineEntry(rv, d, next))
	if err := c.persist(); err != nil {
		c.lastErr = "state not saved: " + err.Error()
	}
	return &ApplyResult{At: c.now(), Version: next, Message: fmt.Sprintf("Applied as v%d", next)}, nil
}

func (c *Controller) regPath() string { return c.opts.DataDir + "/registry.json" }

// timelineEntry summarizes an applied review in one line per area.
func (c *Controller) timelineEntry(rv *Review, d *Draft, version int) TimelineEntry {
	area := "Topology"
	if !rv.RebuildsVCenter {
		area = "Telemetry"
	}
	var parts []string
	for i, ch := range rv.Changes {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("+%d more", len(rv.Changes)-3+rv.More))
			break
		}
		switch {
		case ch.Area == "Telemetry" && ch.Before == "—":
			parts = append(parts, fmt.Sprintf("Added %s %s", strings.ToLower(ch.Object), ch.Field))
		case ch.Area == "Telemetry" && ch.After == "removed":
			parts = append(parts, fmt.Sprintf("Removed %s %s", strings.ToLower(ch.Object), ch.Field))
		case ch.Before == "—" || ch.After == "—":
			parts = append(parts, fmt.Sprintf("%s %s", ch.Object, strings.ToLower(ch.Effect)))
		default:
			parts = append(parts, fmt.Sprintf("%s %s %s → %s", ch.Object, ch.Field, ch.Before, ch.After))
		}
	}
	hist := "preserved"
	for _, ch := range rv.Changes {
		if strings.Contains(ch.Effect, "samples") || ch.Field == "Telemetry seed" {
			hist = "changed"
		}
	}
	ids := "unchanged"
	var added, removed int
	for _, r := range rv.Identities {
		if r.Label == "Performance entities" || r.Label == "Virtual disk identities" {
			continue
		}
		added += r.Added
		removed += r.Removed
	}
	if added > 0 || removed > 0 {
		ids = fmt.Sprintf("+%d / −%d", added, removed)
	}
	text := strings.Join(parts, " · ")
	if text == "" {
		text = "applied"
	}
	if rv.Origin != "edit" {
		text = strings.Replace(rv.Origin, "preset:", "Preset ", 1) + " · " + text
	}
	return TimelineEntry{At: c.now(), Bucket: bucket(c.now()), Version: version, Area: area, Text: text, History: hist, Identities: ids}
}

// SetPaused stops (true) or resumes (false) serving the API: while paused,
// connections to /sdk and /vsanHealth are dropped, so a collection fails
// instead of reading empty data. History and seeds are untouched.
func (c *Controller) SetPaused(p bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused.Swap(p) == p {
		return
	}
	c.pausedAt = c.now()
	text := "API service resumed"
	if p {
		text = "API service paused: connections refused"
	}
	c.timeline = append(c.timeline, TimelineEntry{At: c.now(), Bucket: bucket(c.now()), Version: c.applied().version,
		Area: "Service", Text: text, History: "preserved", Identities: "unchanged"})
	_ = c.persist()
}

// Paused reports whether the API is paused.
func (c *Controller) Paused() bool { return c.paused.Load() }

// DismissError clears the failure banner.
func (c *Controller) DismissError() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr = ""
}
