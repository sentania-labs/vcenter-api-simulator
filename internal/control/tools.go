package control

import (
	"sort"
	"sync"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/activity"
)

// The toolbook is the one place that knows which tools are connected: which
// address, what the tool calls itself, which account it signed in with, when
// it last asked for something and, if it is not connected, since when and
// why. Home, Users, Requests and every "what will happen" box read it, so
// they cannot disagree.
//
// A tool is whatever signs in from one address under one name (its
// User-Agent). "Connected" is an inference from what it did: it signed in,
// it was answered within the last quarter of an hour, and nothing has cut it
// off since.

// quietAfter is how long a tool may ask for nothing before it counts as not
// connected. Tools that collect every five minutes stay well inside it.
const quietAfter = 15 * time.Minute

// Tool states.
const (
	ToolConnected = "connected"
	ToolPaused    = "paused"    // the simulator was paused under it
	ToolRemoved   = "removed"   // its account was removed
	ToolCert      = "cert"      // the certificate was replaced and it has not come back
	ToolDenied    = "denied"    // its last sign-in was refused
	ToolSignedOut = "signedout" // its session ended and it has not signed in again
	ToolLeft      = "left"      // it signed itself out and has not signed in again
	ToolQuiet     = "quiet"     // it has asked for nothing for a while
)

type tool struct {
	addr, agent string
	user        string    // the account it last signed in as
	signedIn    bool      // it has signed in at least once
	last        time.Time // its last request of any kind
	lastOK      time.Time // its last request made while signed in that the simulator served
	denied      time.Time // its last refused sign-in
	unsigned    time.Time // the first request refused for not being signed in, since it was last served
	left        time.Time // when it signed itself out, while it has not been served as signed in since
	deniedUser  string
	deniedCount int
	gone        time.Time // when it was cut off, with why
	why         string
}

type toolbook struct {
	mu    sync.Mutex
	tools map[string]*tool
	// removed remembers users that were removed, so a sign-in refused for
	// that reason can say so.
	removed map[string]time.Time
}

func newToolbook() *toolbook {
	return &toolbook{tools: map[string]*tool{}, removed: map[string]time.Time{}}
}

// forget notes that a user was removed; restore that it exists again.
func (b *toolbook) forget(user string, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removed[user] = at
}

func (b *toolbook) restore(user string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.removed, user)
}

// wasRemoved reports whether a user was removed from the console.
func (b *toolbook) wasRemoved(user string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.removed[user]
	return ok
}

func toolKey(addr, agent string) string { return addr + "|" + agent }

// isSignOut reports whether a request is a tool signing itself out of the
// simulated vCenter.
func isSignOut(call activity.Call) bool {
	switch call.Surface {
	case activity.SurfaceSDK:
		return call.Method == "Logout"
	case activity.SurfaceREST:
		return call.Method == "DELETE /api/session"
	}
	return false
}

// note files one request. It returns the tool's address when the request
// shows a tool that had been cut off is back.
func (b *toolbook) note(call activity.Call, signIn bool) (reconnected string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := toolKey(call.Client, call.Agent)
	t := b.tools[key]
	if t == nil {
		// only a sign-in, refused or not, makes an entry: a stray request
		// that never signs in is not a tool
		if !signIn {
			return ""
		}
		t = &tool{addr: call.Client, agent: call.Agent}
		b.tools[key] = t
	}
	t.last = call.Time
	served := call.Reason != activity.ReasonDenied && call.Reason != activity.ReasonPaused
	switch {
	case signIn && call.Reason == activity.ReasonDenied:
		t.denied, t.deniedUser = call.Time, call.User
		t.deniedCount++
	case signIn && served:
		t.signedIn, t.user = true, call.User
	case call.Reason == activity.ReasonDenied && !t.unsigned.After(t.lastOK):
		// its session is gone and it is still asking: until it signs in
		// again it is not connected, whatever it sends
		t.unsigned = call.Time
	}
	if served && call.User != "" && t.signedIn && isSignOut(call) {
		// Signing out is the tool leaving, not the tool asking for
		// something: from here it is not connected until it signs in again
		// (or is served on another session it still holds).
		if call.Time.After(t.left) {
			t.left = call.Time
		}
		return ""
	}
	if served && (call.User != "" || signIn) && t.signedIn {
		if call.Time.After(t.left) {
			t.left = time.Time{}
		}
		if t.why != "" {
			if !call.Time.After(t.gone) {
				// it was already on its way when the tool was cut off: an
				// answer to it is not the tool coming back
				return ""
			}
			t.why, t.gone = "", time.Time{}
			reconnected = t.addr
		}
		if call.Time.After(t.lastOK) {
			t.lastOK = call.Time
		}
	}
	if len(b.tools) > 200 {
		// forget what has not been seen for a day, and strangers that never
		// signed in after an hour
		for k, old := range b.tools {
			idle := call.Time.Sub(old.last)
			if idle > 24*time.Hour || (!old.signedIn && idle > time.Hour) {
				delete(b.tools, k)
			}
		}
	}
	return reconnected
}

// cut marks the connected tools signed in as user (every tool when user is
// "") as cut off at at, for why. It returns their addresses.
func (b *toolbook) cut(user, why string, at time.Time, paused bool, pausedAt time.Time) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, t := range b.tools {
		if !t.signedIn || (user != "" && t.user != user) {
			continue
		}
		if st, _ := t.state(at, paused, pausedAt); st != ToolConnected && st != ToolPaused {
			continue
		}
		t.why, t.gone = why, at
		out = append(out, t.addr)
	}
	sort.Strings(out)
	return out
}

// state says whether the tool is connected at now and, if not, why and
// since when.
func (t *tool) state(now time.Time, paused bool, pausedAt time.Time) (string, time.Time) {
	switch {
	case t.why != "":
		return t.why, t.gone
	case t.denied.After(t.lastOK):
		return ToolDenied, t.denied
	case t.unsigned.After(t.lastOK):
		return ToolSignedOut, t.unsigned
	case !t.left.IsZero():
		return ToolLeft, t.left
	case paused:
		if pausedAt.Sub(t.lastOK) <= quietAfter {
			return ToolPaused, pausedAt
		}
		return ToolQuiet, t.lastOK
	case now.Sub(t.lastOK) > quietAfter:
		return ToolQuiet, t.lastOK
	}
	return ToolConnected, time.Time{}
}

// ToolView is one tool as every screen shows it.
type ToolView struct {
	Addr  string `json:"addr"`
	Agent string `json:"agent"`
	User  string `json:"user"`
	// Last is when it last asked for something that was served.
	Last  time.Time `json:"last"`
	State string    `json:"state"`
	// Since is when it stopped being connected.
	Since *time.Time `json:"since,omitempty"`
}

// Connected reports whether the tool is connected.
func (v ToolView) Connected() bool { return v.State == ToolConnected }

// list returns every tool that has signed in, connected ones first, then
// the most recently seen.
func (b *toolbook) list(now time.Time, paused bool, pausedAt time.Time) []ToolView {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []ToolView
	for _, t := range b.tools {
		if !t.signedIn {
			continue
		}
		st, since := t.state(now, paused, pausedAt)
		v := ToolView{Addr: t.addr, Agent: t.agent, User: t.user, Last: t.lastOK, State: st}
		if st != ToolConnected {
			s := since
			v.Since = &s
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Connected() != out[j].Connected() {
			return out[i].Connected()
		}
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].Addr+out[i].Agent < out[j].Addr+out[j].Agent
	})
	return out
}

// known reports whether an address and name belong to a tool that has
// signed in.
func (b *toolbook) known(addr, agent string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.tools[toolKey(addr, agent)]
	return t != nil && t.signedIn
}
