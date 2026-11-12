package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A task is one change the console can make: add a cluster, change a
// version, switch a fault on. Every task works the same way. Its screen asks
// a few questions that are already answered (Form); the simulator says in
// sentences what the answers would do (Plan); and the change is made when
// the button is pressed, or at a time later the same day (TaskRun). One
// piece of code plans and applies, so the screen cannot promise one thing
// and do another.

// Item is one sentence of a "What will happen" box. A note is said more
// quietly: what is kept, what is not touched.
type Item struct {
	T    string `json:"t"`
	Note bool   `json:"note,omitempty"`
}

func say(format string, a ...any) Item  { return Item{T: fmt.Sprintf(format, a...)} }
func note(format string, a ...any) Item { return Item{T: fmt.Sprintf(format, a...), Note: true} }

// Mark tags a row as "New" or "Changed" on the screen a task lands on.
type Mark struct {
	Type string `json:"type"` // cluster | host | storage | user
	ID   string `json:"id"`
	Tag  string `json:"tag"`
}

// Outcome is what a change did, in the words shown after it.
type Outcome struct {
	// Lead is the bold word the green line begins with; empty means "Done."
	Lead string `json:"lead,omitempty"`
	Done string `json:"done"`
	// Tone is ok, or warn when the change cut a tool off.
	Tone  string `json:"tone"`
	Dest  string `json:"dest"`
	Marks []Mark `json:"marks,omitempty"`
	// history is the line for the history when it differs from Done.
	history string
}

// Confirm is the box shown before a change that removes things is made
// right away.
type Confirm struct {
	Title string   `json:"title"`
	Body  []string `json:"body"`
	OK    string   `json:"ok"`
}

// Plan is what a task would do with the values given.
type Plan struct {
	// Errors are the values that need fixing, by field.
	Errors map[string]string `json:"errors,omitempty"`
	Sum    []Item            `json:"sum,omitempty"`
	// Idle replaces Sum while the values change nothing.
	Idle string `json:"idle,omitempty"`
	// Go is the button's label.
	Go string `json:"go"`
	// Extra carries what the form needs that only the simulator knows.
	Extra   map[string]any `json:"extra,omitempty"`
	Confirm *Confirm       `json:"confirm,omitempty"`

	text  string // the change in a few words, for the scheduled bar and the history
	dest  string // the screen to land on
	apply func() (*Outcome, error)
}

func (p *Plan) bad(field, format string, a ...any) {
	if p.Errors == nil {
		p.Errors = map[string]string{}
	}
	if _, dup := p.Errors[field]; !dup {
		p.Errors[field] = fmt.Sprintf(format, a...)
	}
}

// Form is a task's screen: its title, where it came from, and its
// questions already answered.
type Form struct {
	Kind    string         `json:"kind"`
	Title   string         `json:"title"`
	Intro   string         `json:"intro,omitempty"`
	Crumbs  [][2]string    `json:"crumbs"` // label and address; the last one is where Cancel goes
	Values  map[string]any `json:"values"`
	Options map[string]any `json:"options,omitempty"`
	// NoWhen: the change always happens when the button is pressed.
	NoWhen bool `json:"noWhen,omitempty"`
	// Danger: the button removes or replaces something, so it is red.
	Danger bool `json:"danger,omitempty"`
	// NowNote replaces the usual words under "Right away".
	NowNote string `json:"nowNote,omitempty"`
	// Mix: a tool that is collecting may see old and new for one round.
	Mix bool `json:"mix,omitempty"`
}

// Caller is who is using the console: the console session (which a console
// password change keeps signed in) and the host name the console was
// opened with.
type Caller struct {
	Session string
	Host    string
}

type taskDef struct {
	form func(c *Controller, q map[string]string, who Caller) (*Form, error)
	plan func(c *Controller, raw json.RawMessage, who Caller) (*Plan, error)
	// now: the task always happens when the button is pressed.
	now bool
}

var tasks = map[string]taskDef{}

// N is a number typed into a form: a JSON number or a numeric string. OK is
// false for an empty or non-numeric value.
type N struct {
	V  int
	OK bool
}

// UnmarshalJSON accepts 4, "4" and "".
func (n *N) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*n = N{}
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		*n = N{}
		return nil
	}
	*n = N{V: v, OK: true}
	return nil
}

// in reports whether the number was given and lies in [lo, hi].
func (n N) in(lo, hi int) bool { return n.OK && n.V >= lo && n.V <= hi }

func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return invalid("The form could not be read: %s", err.Error())
	}
	return nil
}

// TaskForm returns a task's screen.
func (c *Controller) TaskForm(kind string, q map[string]string, who Caller) (*Form, error) {
	t, ok := tasks[kind]
	if !ok {
		return nil, invalid("There is no such task.")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	f, err := t.form(c, q, who)
	if err != nil {
		return nil, err
	}
	f.Kind = kind
	return f, nil
}

// TaskPreview says what a task would do with the values given. It reports
// no errors: the form shows those only once the button is pressed.
func (c *Controller) TaskPreview(kind string, raw json.RawMessage, who Caller) (*Plan, error) {
	t, ok := tasks[kind]
	if !ok {
		return nil, invalid("There is no such task.")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p, err := t.plan(c, raw, who)
	if err != nil {
		return nil, err
	}
	p.Errors, p.Confirm = nil, nil
	return p, nil
}

// RunResult is the answer to pressing a task's button.
type RunResult struct {
	// Errors: nothing was done; these values need fixing.
	Errors map[string]string `json:"errors,omitempty"`
	// Confirm: nothing was done yet; ask this first, then run again
	// confirmed.
	Confirm *Confirm `json:"confirm,omitempty"`
	// Outcome: the change was made.
	Outcome *Outcome `json:"outcome,omitempty"`
	// Scheduled: the change will be made later.
	Scheduled *ScheduledView `json:"scheduled,omitempty"`
	Dest      string         `json:"dest,omitempty"`
	// Failed: the change was tried and not made; the failure bar says why.
	Failed bool `json:"failed,omitempty"`
}

// TaskRun makes a change now, or schedules it for at.
func (c *Controller) TaskRun(kind string, raw json.RawMessage, at *time.Time, confirmed bool, who Caller) (*RunResult, error) {
	t, ok := tasks[kind]
	if !ok {
		return nil, invalid("There is no such task.")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep()
	p, err := t.plan(c, raw, who)
	if err != nil {
		return nil, err
	}
	if len(p.Errors) > 0 {
		return &RunResult{Errors: p.Errors}, nil
	}
	if p.apply == nil {
		return &RunResult{Errors: map[string]string{"_": "Nothing has been changed yet. Change something, or press Cancel."}}, nil
	}
	if at != nil && t.now {
		return &RunResult{Errors: map[string]string{"at": "This change always happens when the button is pressed. It cannot be scheduled."}}, nil
	}
	if at != nil {
		now := c.now()
		if !at.After(now) || at.Sub(now) > 24*time.Hour {
			return &RunResult{Errors: map[string]string{"at": "That time has passed. Pick a later time, or choose Right away."}}, nil
		}
		c.seq++
		s := &Scheduled{ID: c.seq, At: at.UTC(), Text: p.text, kind: kind, params: raw, who: who}
		s.timer = time.AfterFunc(at.Sub(now), func() { c.fire(s.ID) })
		c.sched = append(c.sched, s)
		sort.SliceStable(c.sched, func(i, j int) bool { return c.sched[i].At.Before(c.sched[j].At) })
		c.addHistory(fmt.Sprintf("Scheduled for %s: %s.", timeToken(s.At), lower1(p.text)))
		c.save()
		v := s.view()
		return &RunResult{Scheduled: &v, Dest: p.dest}, nil
	}
	if p.Confirm != nil && !confirmed {
		return &RunResult{Confirm: p.Confirm}, nil
	}
	out, err := c.carryOut(kind, raw, p)
	var wrong *fieldError
	if errors.As(err, &wrong) {
		return &RunResult{Errors: map[string]string{wrong.field: wrong.msg}}, nil
	}
	if err != nil {
		return &RunResult{Failed: true, Dest: p.dest}, nil
	}
	return &RunResult{Outcome: out, Dest: out.Dest}, nil
}

// fieldError is a value that only turned out to be wrong when the change
// was tried (the current console password). It goes back to the form, under
// its field; it is not a change that failed.
type fieldError struct{ field, msg string }

func (e *fieldError) Error() string { return e.msg }

// carryOut applies a planned change and writes the history line, or records
// the failure. c.mu is held.
func (c *Controller) carryOut(kind string, raw json.RawMessage, p *Plan) (*Outcome, error) {
	out, err := p.apply()
	var wrong *fieldError
	if errors.As(err, &wrong) {
		return nil, err
	}
	if err != nil {
		c.fail(p.text, err, kind, raw, taskRoute(kind, raw))
		return nil, err
	}
	if out.Tone == "" {
		out.Tone = "ok"
	}
	if out.Dest == "" {
		out.Dest = p.dest
	}
	line := out.history
	if line == "" {
		line = out.Done
	}
	c.failure = nil
	c.addHistory(line)
	c.save()
	return out, nil
}

// taskRoute is the address of a task's screen with its subject, so that
// "Try again" can reopen it.
func taskRoute(kind string, raw json.RawMessage) string {
	var p struct {
		Cluster, ID, Name string
	}
	_ = json.Unmarshal(raw, &p)
	switch kind {
	case "add-cluster":
		return "/clusters/add"
	case "replace-everything":
		return "/clusters/build"
	case "add-hosts", "remove-hosts", "hardware":
		return "/cluster/" + p.Cluster + "/" + kind
	case "cluster-versions":
		return "/versions/cluster/" + p.Cluster
	case "vcenter-version":
		return "/versions/vcenter"
	case "add-iscsi", "add-nfs":
		return "/storage/" + kind
	case "storage-size":
		return "/storage/" + p.ID + "/change"
	case "storage-disconnect":
		return "/storage/" + p.ID + "/problem"
	case "add-user":
		return "/users/add"
	case "change-user":
		return "/users/edit/" + p.Name
	case "idle":
		return "/security/idle"
	case "new-certificate":
		return "/security/new-certificate"
	case "own-certificate":
		return "/security/own-certificate"
	case "fault":
		return "/faults/add"
	}
	return "/home"
}

// ---------------------------------------------------------------------------
// changes scheduled for later

// Scheduled is a change waiting for its time. It holds the task and its
// values, not a finished result: when the time comes the change is planned
// again against whatever is true then, so a change that no longer makes
// sense fails cleanly instead of undoing what was done in between.
type Scheduled struct {
	ID   int
	At   time.Time
	Text string

	kind   string
	params json.RawMessage
	who    Caller
	timer  *time.Timer
}

// ScheduledView is a scheduled change as the console shows it.
type ScheduledView struct {
	ID   int       `json:"id"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

func (s *Scheduled) view() ScheduledView { return ScheduledView{ID: s.ID, At: s.At, Text: s.Text} }

// take removes a scheduled change from the list. c.mu is held.
func (c *Controller) take(id int) *Scheduled {
	for i, s := range c.sched {
		if s.ID == id {
			c.sched = append(c.sched[:i], c.sched[i+1:]...)
			if s.timer != nil {
				s.timer.Stop()
			}
			return s
		}
	}
	return nil
}

// fire makes a scheduled change when its time has come.
func (c *Controller) fire(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.take(id); s != nil {
		_, _ = c.runScheduled(s)
	}
}

// runScheduled plans a scheduled change against what is true now and makes
// it. c.mu is held.
func (c *Controller) runScheduled(s *Scheduled) (*Outcome, error) {
	c.sweep()
	p, err := tasks[s.kind].plan(c, s.params, s.who)
	if err == nil && len(p.Errors) > 0 {
		// say the first thing that is wrong, in field order
		keys := make([]string, 0, len(p.Errors))
		for k := range p.Errors {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		err = invalid("%s", p.Errors[keys[0]])
	}
	if err == nil && p.apply == nil {
		err = invalid("By the time it was due there was nothing left to change")
	}
	if err != nil {
		c.fail(s.Text, err, s.kind, s.params, taskRoute(s.kind, s.params))
		return nil, err
	}
	p.text = s.Text
	return c.carryOut(s.kind, s.params, p)
}

// ScheduledNow makes a scheduled change at once.
func (c *Controller) ScheduledNow(id int) (*RunResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.take(id)
	if s == nil {
		return nil, invalid("That change is no longer scheduled. It may have happened already.")
	}
	out, err := c.runScheduled(s)
	if err != nil {
		return &RunResult{Failed: true}, nil
	}
	return &RunResult{Outcome: out}, nil
}

// ScheduledCancel drops a scheduled change and returns the words that say
// so.
func (c *Controller) ScheduledCancel(id int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.take(id)
	if s == nil {
		return "", invalid("That change is no longer scheduled. It may have happened already.")
	}
	c.addHistory(fmt.Sprintf("Cancelled the change scheduled for %s: %s.", timeToken(s.At), lower1(s.Text)))
	c.save()
	return fmt.Sprintf("**Cancelled.** %q will not happen. Nothing was changed.", s.Text), nil
}

func (c *Controller) scheduledViews() []ScheduledView {
	out := []ScheduledView{}
	for _, s := range c.sched {
		out = append(out, s.view())
	}
	return out
}
