package control

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/credentials"
)

// Users: the accounts tools sign in to the simulated vCenter with. They
// take effect when the button is pressed; changing or removing a user ends
// that user's sessions, and the tool list says so afterwards.

func init() {
	tasks["add-user"] = taskDef{form: formAddUser, plan: planAddUser}
	tasks["change-user"] = taskDef{form: formChangeUser, plan: planChangeUser}
}

// The console's two access levels and the roles they are stored as.
var (
	roleOf   = map[string]string{"full": "administrator", "read": "read-only"}
	accessOf = map[string]string{"administrator": "full", "read-only": "read"}
	access   = map[string]string{"full": "full access", "read": "read-only access"}
)

type userParams struct {
	Name string `json:"name"`
	PW   string `json:"pw"`
	Role string `json:"role"`
}

// passwordProblem says what is wrong with a new password, or "". tail is
// the way out the form offers.
func passwordProblem(pw, tail string) string {
	switch n := len([]rune(pw)); {
	case n == 0:
		return "Type a password of at least 8 characters, " + tail
	case n < credentials.MinPassword:
		return fmt.Sprintf("That is %d characters. Use at least %d, %s", n, credentials.MinPassword, tail)
	case len(pw) > 72:
		return "That password is too long. Use 72 characters or fewer."
	}
	return ""
}

// commitUsers makes a changed set of users the one the simulated vCenter
// checks, and ends the sessions of the users that changed. c.mu is held.
func (c *Controller) commitUsers(snapshot credentials.Snapshot) error {
	return c.applied().sim.UpdateCredentials(func() ([]string, error) { return c.auth.Commit(snapshot) })
}

// toolsAs lists the connected tools signed in as a user.
func (c *Controller) toolsAs(user string) []ToolView {
	var out []ToolView
	for _, t := range c.toolViews() {
		if t.Connected() && t.User == user {
			out = append(out, t)
		}
	}
	return out
}

func toolAddrs(ts []ToolView) string {
	var a []string
	for _, t := range ts {
		a = append(a, t.Addr)
	}
	return listAnd(a)
}

func formAddUser(c *Controller, _ map[string]string, _ Caller) (*Form, error) {
	snapshot := c.auth.Snapshot()
	name := ""
	for i := 1; ; i++ {
		if name = fmt.Sprintf("tool-%d@vsphere.local", i); !snapshot.Has(name) {
			break
		}
	}
	return &Form{Title: "Add a user", Crumbs: [][2]string{{"Users", "/users"}}, Intro: "Make an account for a tool to sign in with.",
		Values: map[string]any{"name": name, "pw": "", "role": "read"}}, nil
}

func planAddUser(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in userParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	p := &Plan{Go: "Add user", dest: "/users"}
	name := strings.TrimSpace(in.Name)
	snapshot := c.auth.Snapshot()
	switch {
	case name == "":
		p.bad("name", "Type a username, for example tool-1@vsphere.local.")
	case strings.ContainsAny(name, " \t\r\n"):
		p.bad("name", "A username cannot contain spaces.")
	case len(name) > 256:
		p.bad("name", "That username is too long. Use 256 characters or fewer.")
	case snapshot.Has(name):
		p.bad("name", "There is already a user called %s. Choose another name, or change that user instead.", name)
	}
	if msg := passwordProblem(in.PW, `or press "Make one for me".`); msg != "" {
		p.bad("pw", "%s", msg)
	}
	if _, ok := roleOf[in.Role]; !ok {
		in.Role = "read"
	}
	shown := name
	if shown == "" {
		shown = "The new user"
	}
	p.Sum = []Item{
		say("%s can sign in to the simulated vCenter with %s.", bold(shown), bold(access[in.Role])),
		say("Give your tool this username and password, with the address shown at the top of the screen."),
		note("Other users are not affected.")}
	if len(p.Errors) > 0 {
		return p, nil
	}
	p.text = fmt.Sprintf("Add user %s (%s)", name, access[in.Role])
	p.apply = func() (*Outcome, error) {
		next := c.auth.Snapshot()
		if next.Has(name) {
			return nil, invalid("There is already a user called %s", name)
		}
		if err := next.Put(name, in.PW, roleOf[in.Role]); err != nil {
			return nil, &Invalid{upper1(err.Error())}
		}
		next.SetOrigin(name, "console")
		if err := c.commitUsers(next); err != nil {
			return nil, err
		}
		c.tools.restore(name)
		c.forgetSignin(name)
		return &Outcome{Done: fmt.Sprintf("%s can now sign in with %s.", name, access[in.Role]), Marks: []Mark{{Type: "user", ID: name, Tag: "New"}}}, nil
	}
	return p, nil
}

func formChangeUser(c *Controller, q map[string]string, _ Caller) (*Form, error) {
	snapshot := c.auth.Snapshot()
	name := q["name"]
	if !snapshot.Has(name) {
		return nil, invalid("There is no user called %s. It may have been removed.", name)
	}
	role := accessOf[snapshot.Role(name)]
	opts := map[string]any{"role": role}
	if t, ok := c.lastSignin(name); ok {
		opts["last"] = t
	}
	if ts := c.toolsAs(name); len(ts) > 0 {
		opts["inUse"] = toolAddrs(ts)
	}
	return &Form{Title: "Change " + name, Crumbs: [][2]string{{"Users", "/users"}},
		Values: map[string]any{"name": name, "role": role, "pw": ""}, Options: opts}, nil
}

func planChangeUser(c *Controller, raw json.RawMessage, _ Caller) (*Plan, error) {
	var in userParams
	if err := decodeParams(raw, &in); err != nil {
		return nil, err
	}
	snapshot := c.auth.Snapshot()
	if !snapshot.Has(in.Name) {
		return nil, invalid("There is no user called %s. It may have been removed.", in.Name)
	}
	p := &Plan{Go: "Save changes", dest: "/users"}
	cur := accessOf[snapshot.Role(in.Name)]
	if _, ok := roleOf[in.Role]; !ok {
		in.Role = cur
	}
	newPW := in.PW != ""
	if newPW {
		if msg := passwordProblem(in.PW, "or leave it empty to keep the current password."); msg != "" {
			p.bad("pw", "%s", msg)
		}
	}
	if in.Role != cur {
		p.Sum = append(p.Sum, say("%s changes from %s to %s.", in.Name, access[cur], bold(access[in.Role])))
	}
	if newPW {
		p.Sum = append(p.Sum, say("Its password changes. Give the new one to whatever uses this account."))
	}
	if len(p.Sum) == 0 {
		p.Idle = "Nothing is different yet. Change the access or type a new password and this box will say what happens."
		p.bad("pw", "Nothing has been changed. Choose a different access level or type a new password, or press Cancel.")
		return p, nil
	}
	live := c.toolsAs(in.Name)
	if len(live) > 0 {
		after := " and signs in again by itself"
		if newPW {
			after = " and cannot sign in again until it is given the new password"
		}
		p.Sum = append(p.Sum, say("%s It is signed out%s.", bold(fmt.Sprintf("The tool at %s is signed in as this user right now.", toolAddrs(live))), after))
	} else {
		p.Sum = append(p.Sum, say("Nothing is signed in as this user at the moment."))
	}
	p.Sum = append(p.Sum, note("Other users are not affected."))
	if len(p.Errors) > 0 {
		return p, nil
	}
	p.text = "Change user " + in.Name
	p.apply = func() (*Outcome, error) {
		next := c.auth.Snapshot()
		if !next.Has(in.Name) {
			return nil, invalid("There is no user called %s any more", in.Name)
		}
		if newPW {
			if err := next.Put(in.Name, in.PW, roleOf[in.Role]); err != nil {
				return nil, &Invalid{upper1(err.Error())}
			}
			next.SetOrigin(in.Name, "console")
		} else if err := next.SetRole(in.Name, roleOf[in.Role]); err != nil {
			return nil, &Invalid{upper1(err.Error())}
		}
		if err := c.commitUsers(next); err != nil {
			return nil, err
		}
		var parts []string
		if in.Role != cur {
			parts = append(parts, "now has "+access[in.Role])
		}
		if newPW {
			parts = append(parts, "has a new password")
		}
		return &Outcome{Done: fmt.Sprintf("%s %s.", in.Name, strings.Join(parts, " and ")), Marks: []Mark{{Type: "user", ID: in.Name, Tag: "Changed"}}}, nil
	}
	return p, nil
}

// RemoveUser removes a user at once. A tool signed in as that user is
// signed out, and from then on every screen says why it is not connected.
func (c *Controller) RemoveUser(name string) (done, tone string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.auth.Snapshot()
	if !next.Has(name) {
		return "", "", invalid("There is no user called %s. It may have been removed already.", name)
	}
	if err := next.Delete(name); err != nil {
		return "", "", &Invalid{err.Error()}
	}
	live := c.toolsAs(name)
	if err := c.commitUsers(next); err != nil {
		return "", "", &Invalid{err.Error()}
	}
	c.tools.cut(name, ToolRemoved, c.now(), c.paused.Load(), c.pausedAt)
	c.tools.forget(name, c.now())
	c.forgetSignin(name)
	done, tone = fmt.Sprintf("%s was removed and can no longer sign in.", name), "ok"
	line := fmt.Sprintf("User %s removed.", name)
	if len(live) > 0 {
		done += fmt.Sprintf(` **The tool at %s was signed out** and is now being refused. Home lists this under "Needs your attention".`, toolAddrs(live))
		line += fmt.Sprintf(" The tool at %s was signed out.", toolAddrs(live))
		tone = "warn"
	}
	c.addHistory(line)
	c.save()
	return done, tone, nil
}

// SetAnyLogin turns "accept any username and password" on or off at once.
func (c *Controller) SetAnyLogin(on bool) (done, tone string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.auth.Snapshot()
	if next.AnyLogin == on {
		return "", "", invalid("It is already %s.", map[bool]string{true: "on", false: "off"}[on])
	}
	if !on && next.Len() == 0 {
		return "", "", invalid("Add a user before turning this off, or nothing could sign in.")
	}
	next.AnyLogin = on
	if err := c.commitUsers(next); err != nil {
		return "", "", &Invalid{err.Error()}
	}
	if on {
		c.addHistory("Started accepting any username and password.")
		c.save()
		return "**On.** Any username and password is accepted until you turn this off.", "warn", nil
	}
	c.addHistory("Stopped accepting any username and password.")
	c.save()
	return "**Done.** Only the users in the list can sign in now.", "ok", nil
}

// AccountNeeded reports whether the first vCenter account still has no
// password: no user exists and not every sign-in is accepted. Until the
// console's first-run step sets one, nothing can sign in.
func (c *Controller) AccountNeeded() bool {
	snapshot := c.auth.Snapshot()
	return !snapshot.AnyLogin && snapshot.Len() == 0
}

// SetupAccount gives the first vCenter account its password. It works only
// while AccountNeeded is true.
func (c *Controller) SetupAccount(password string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.auth.Snapshot()
	if next.AnyLogin || next.Len() > 0 {
		return invalid("The first account already has a password. Change it on the Users screen.")
	}
	if msg := passwordProblem(password, `or press "Make one for me".`); msg != "" {
		return &Invalid{msg}
	}
	if err := next.Put(DefaultUsername, password, "administrator"); err != nil {
		return &Invalid{upper1(err.Error())}
	}
	next.SetOrigin(DefaultUsername, "setup")
	if err := c.commitUsers(next); err != nil {
		return err
	}
	c.addHistory(fmt.Sprintf("The first account, %s, was given its password.", DefaultUsername))
	c.save()
	return nil
}

// ---------------------------------------------------------------------------
// the screen

// UserRow is one user on the Users screen.
type UserRow struct {
	Name string `json:"name"`
	Role string `json:"role"` // full | read
	// Last is when the user last signed in; nil means never.
	Last *time.Time `json:"last,omitempty"`
	// InUse lists the addresses of connected tools signed in as the user.
	InUse []string `json:"inUse,omitempty"`
}

// UsersView is the Users screen.
type UsersView struct {
	Users    []UserRow `json:"users"`
	AnyLogin bool      `json:"anyLogin"`
}

// Users lists who can sign in.
func (c *Controller) Users() UsersView {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := c.auth.Snapshot()
	v := UsersView{Users: []UserRow{}, AnyLogin: snapshot.AnyLogin}
	tools := c.toolViews()
	for _, u := range snapshot.Users() {
		row := UserRow{Name: u.Name, Role: accessOf[u.Role]}
		if t, ok := c.lastSignin(u.Name); ok {
			at := t
			row.Last = &at
		}
		for _, t := range tools {
			if t.Connected() && t.User == u.Name {
				row.InUse = append(row.InUse, t.Addr)
			}
		}
		v.Users = append(v.Users, row)
	}
	return v
}
