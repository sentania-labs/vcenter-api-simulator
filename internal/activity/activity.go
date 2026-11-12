// Package activity records what the simulator's clients ask of it: one entry
// per request to the simulated vCenter, redacted (no headers, cookies or
// credentials are kept), in a bounded ring, and says of each one why it was
// answered the way it was. The console's Requests screen, Home and the tool
// list are all drawn from this log.
package activity

import (
	"bytes"
	"html"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Reasons: why a request was answered the way it was. Exactly one applies
// to every call, and "unknown" alone means the simulator has no answer.
const (
	ReasonAnswered = "answered" // a full answer
	ReasonEmpty    = "empty"    // answered "there are none", as a real vCenter does here
	ReasonRefused  = "refused"  // refused on purpose, as a real vCenter refuses it too
	ReasonFault    = "fault"    // failed because of a fault the operator switched on
	ReasonPaused   = "paused"   // the operator paused the simulator; the connection was dropped
	ReasonUnknown  = "unknown"  // the simulator does not know how to answer this
	ReasonDenied   = "denied"   // a sign-in was refused, or the request was not signed in
)

// Surfaces: which part of the simulated vCenter a request reached.
const (
	SurfaceSDK   = "sdk"
	SurfaceVsan  = "vsanHealth"
	SurfaceREST  = "rest"
	SurfaceOther = "other"
)

// Call is one recorded request. Nothing in it identifies a credential.
type Call struct {
	Seq     uint64    `json:"seq"`
	Time    time.Time `json:"time"`
	Surface string    `json:"surface"`
	// Method is the name tools use for the request: the SOAP operation, or
	// the HTTP method and path.
	Method  string `json:"method"`
	Family  string `json:"family,omitempty"`
	Cluster string `json:"cluster,omitempty"`
	// Client is the address the request came from; Agent what the tool
	// calls itself (its User-Agent); User the account it is signed in as,
	// or the account a sign-in named.
	Client     string  `json:"client"`
	Agent      string  `json:"agent,omitempty"`
	User       string  `json:"user,omitempty"`
	DurationMs float64 `json:"durationMs"`
	Bytes      int64   `json:"bytes"`
	Status     int     `json:"status"`
	Reason     string  `json:"reason"`
	Fault      string  `json:"fault,omitempty"`
	Message    string  `json:"message,omitempty"`
	// Cause is the id of the switched-on condition that made the request
	// fail, when Reason is "fault".
	Cause int `json:"cause,omitempty"`
	// Entities and Empty count the entities a VsanPerfQueryPerf answer
	// carried and how many of them had no samples.
	Entities int `json:"entities,omitempty"`
	Empty    int `json:"empty,omitempty"`
	// Body is the redacted request body of a SOAP call, kept for the detail
	// view and cut at MaxBody bytes. Sign-in requests keep none.
	Body string `json:"body,omitempty"`
}

// MaxBody bounds the kept request body.
const MaxBody = 2048

// Log is the bounded request log.
type Log struct {
	mu    sync.Mutex
	ring  []Call
	next  int
	full  bool
	seq   uint64
	since time.Time
	total uint64
}

// DefaultSize is how many calls the console keeps: about five collection
// cycles of a 10-cluster scenario.
const DefaultSize = 20000

// New returns a log holding the last size calls.
func New(size int, now time.Time) *Log {
	if size <= 0 {
		size = DefaultSize
	}
	return &Log{ring: make([]Call, size), since: now}
}

// Add records a call and returns it with its sequence number.
func (l *Log) Add(c Call) Call {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	c.Seq = l.seq
	l.ring[l.next] = c
	l.next++
	if l.next == len(l.ring) {
		l.next, l.full = 0, true
	}
	l.total++
	return c
}

// All returns the held calls, oldest first.
func (l *Log) All() []Call {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.full {
		return append([]Call(nil), l.ring[:l.next]...)
	}
	out := make([]Call, 0, len(l.ring))
	out = append(out, l.ring[l.next:]...)
	out = append(out, l.ring[:l.next]...)
	return out
}

// Since returns the held calls at or after t, oldest first.
func (l *Log) Since(t time.Time) []Call {
	// The ring is in the order requests finished, and each is stamped with
	// when it started, so a slow one can sit behind quicker later ones.
	var out []Call
	for _, c := range l.All() {
		if !c.Time.Before(t) {
			out = append(out, c)
		}
	}
	return out
}

// Brief returns the call without its body, for list views.
func (c Call) Brief() Call { c.Body = ""; return c }

// Get returns one call by sequence number.
func (l *Log) Get(seq uint64) (Call, bool) {
	for _, c := range l.All() {
		if c.Seq == seq {
			return c, true
		}
	}
	return Call{}, false
}

// Totals are counts since the log started (not bounded by the ring).
type Totals struct {
	Since    time.Time `json:"since"`
	Requests uint64    `json:"requests"`
	Held     int       `json:"held"` // calls currently in the ring
}

// Totals returns the lifetime counters.
func (l *Log) Totals() Totals {
	l.mu.Lock()
	defer l.mu.Unlock()
	held := l.next
	if l.full {
		held = len(l.ring)
	}
	return Totals{Since: l.since, Requests: l.total, Held: held}
}

// ---------------------------------------------------------------------------
// classification: why a request was answered the way it was

// Facts are what the simulator knows about one answered request.
type Facts struct {
	Surface    string
	HTTPMethod string
	Path       string
	// Method is the SOAP operation, when the request was one.
	Method string
	Status int
	// FaultType is the SOAP fault's type, when the answer was a fault.
	FaultType string
	Faulted   bool
	// Injected: a fault the operator switched on produced the answer (or
	// withheld it). PerfOff: the operator switched the performance service
	// of the request's cluster off.
	Injected, PerfOff bool
	// Paused: the simulator was paused, so the connection was dropped.
	Paused bool
	// ObjectType is the type of the managed object the SOAP call was sent to.
	ObjectType      string
	Entities, Empty int
}

// inventoryTypes are managed objects that come and go with the inventory.
// A request to one that no longer exists is refused by a real vCenter in the
// same way, so it is not a gap in the simulator.
var inventoryTypes = map[string]bool{
	"ClusterComputeResource": true, "ComputeResource": true, "HostSystem": true, "VirtualMachine": true,
	"Datastore": true, "Datacenter": true, "Folder": true, "Network": true, "ResourcePool": true,
	"DistributedVirtualSwitch": true, "VmwareDistributedVirtualSwitch": true, "DistributedVirtualPortgroup": true,
	"ContainerView": true, "ListView": true, "InventoryView": true, "PropertyFilter": true, "Task": true,
	"HostDatastoreSystem": true, "HostNetworkSystem": true, "HostStorageSystem": true, "HostVsanSystem": true,
	"HostVsanInternalSystem": true, "HostConfigManager": true, "HttpNfcLease": true, "PropertyCollector": true,
	"EventHistoryCollector": true, "TaskHistoryCollector": true,
}

// deliberatePaths are requests the simulator turns away exactly as a real
// vCenter does: they were captured that way.
var deliberatePaths = map[string]bool{
	"/sdk": true, "/sdk/": true, "/vsanHealth": true, "/vsanHealth/": true, "/vsanHealth/vsanServiceVersions.xml": true,
}

// Classify says why a request was answered the way it was. The Requests
// screen's headline is counted from this, so the one rule that matters is
// that "unknown" is returned only for a request the simulator truly has no
// answer for: never for a refused sign-in, a deliberate refusal, or a fault
// the operator switched on.
func Classify(f Facts) string {
	switch {
	case f.Paused:
		return ReasonPaused
	case f.Injected:
		return ReasonFault
	case f.PerfOff && f.Faulted && f.FaultType == "NotFound":
		return ReasonFault
	case f.FaultType == "NotAuthenticated" || f.FaultType == "InvalidLogin":
		return ReasonDenied
	case f.Surface == SurfaceREST && f.Status == http.StatusUnauthorized:
		return ReasonDenied
	}
	if !f.Faulted && f.Status < 400 {
		if f.Method == "VsanPerfQueryPerf" && f.Empty >= f.Entities {
			return ReasonEmpty
		}
		return ReasonAnswered
	}
	if f.Faulted {
		switch f.Surface {
		case SurfaceVsan:
			return classifyVsanFault(f)
		default:
			return classifySDKFault(f)
		}
	}
	// an HTTP error without a SOAP fault
	switch {
	case deliberatePaths[f.Path] && f.HTTPMethod == http.MethodGet:
		return ReasonRefused
	case f.Status == http.StatusNotFound || f.Status == http.StatusMethodNotAllowed || f.Status == http.StatusNotImplemented:
		return ReasonUnknown
	case f.Surface == SurfaceREST && f.Status < 500:
		// a filter the tool sent that a real vCenter rejects as well
		return ReasonRefused
	case f.Surface == SurfaceREST:
		// the read-only REST surface answers a few paths with the error a
		// real vCenter gives a read-only session; it has no other 5xx
		return ReasonRefused
	}
	return ReasonUnknown
}

func classifyVsanFault(f Facts) string {
	switch f.FaultType {
	case "InvalidArgument", "NotSupported", "NotFound":
		// A performance query of a kind a real vCenter refuses or does not
		// know, a time range it rejects, or another method's bad argument:
		// each was captured from a real vCenter answering the same way.
		return ReasonRefused
	}
	// ManagedObjectNotFound here is a vSAN managed object the simulator does
	// not model, and any other fault was not one a rule asked for.
	return ReasonUnknown
}

func classifySDKFault(f Facts) string {
	switch f.FaultType {
	case "NoPermission":
		return ReasonRefused
	case "ManagedObjectNotFound":
		if inventoryTypes[f.ObjectType] {
			return ReasonRefused
		}
		return ReasonUnknown
	case "MethodNotFound", "InvalidRequest", "NotImplemented", "NotSupported", "InvalidProperty", "InvalidType", "":
		return ReasonUnknown
	}
	// Any other typed fault is vCenter's own answer to a request it
	// understood: a bad argument, an object in the wrong state.
	return ReasonRefused
}

// ---------------------------------------------------------------------------
// capture: reading what a handler wrote without keeping it

// Capture wraps a ResponseWriter to count bytes, remember the status, keep
// the head of the body (where a SOAP fault lives) and, for performance
// queries, tally the entities answered and how many of them were empty.
type Capture struct {
	http.ResponseWriter
	Status   int
	Bytes    int64
	Head     []byte
	Entities int
	Empty    int
	tally    bool
	tail     []byte
}

// NewCapture wraps w; tally enables the per-entity counts.
func NewCapture(w http.ResponseWriter, tally bool) *Capture {
	return &Capture{ResponseWriter: w, Status: http.StatusOK, tally: tally}
}

const headSize = 4096

var (
	entityTag = []byte("<returnval>")
	emptyTag  = []byte("<sampleInfo></sampleInfo>")
	overlap   = len(emptyTag) - 1
)

// WriteHeader records the status.
func (c *Capture) WriteHeader(code int) {
	c.Status = code
	c.ResponseWriter.WriteHeader(code)
}

// Write counts and forwards.
func (c *Capture) Write(p []byte) (int, error) {
	if room := headSize - len(c.Head); room > 0 {
		c.Head = append(c.Head, p[:min(room, len(p))]...)
	}
	if c.tally {
		// count across write boundaries: scan the carried tail plus p, minus
		// what the tail alone already counted, and carry the last overlap
		// bytes forward
		buf := make([]byte, 0, len(c.tail)+len(p))
		buf = append(append(buf, c.tail...), p...)
		c.Entities += bytes.Count(buf, entityTag) - bytes.Count(c.tail, entityTag)
		c.Empty += bytes.Count(buf, emptyTag) - bytes.Count(c.tail, emptyTag)
		c.tail = append([]byte(nil), buf[max(0, len(buf)-overlap):]...)
	}
	n, err := c.ResponseWriter.Write(p)
	c.Bytes += int64(n)
	return n, err
}

// Flush passes through.
func (c *Capture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

var (
	faultString = regexp.MustCompile(`<faultstring>([^<]*)</faultstring>`)
	faultType   = regexp.MustCompile(`xsi:type="([A-Za-z0-9_]+)"`)
)

// Fault reads the SOAP fault out of the kept head: its type and message.
func (c *Capture) Fault() (typ, msg string, ok bool) {
	m := faultString.FindSubmatch(c.Head)
	if m == nil {
		return "", "", false
	}
	msg = string(m[1])
	if t := faultType.FindSubmatch(c.Head); t != nil {
		typ = string(t[1])
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return typ, msg, true
}

// SDKMethod names the operation of a /sdk request body: the first element
// inside the SOAP Body.
var sdkMethodRe = regexp.MustCompile(`<(?:[A-Za-z0-9_]+:)?Body[^>]*>\s*<(?:[A-Za-z0-9_]+:)?([A-Za-z0-9_]+)`)

// SDKMethod returns the operation name, or "" when the body is not SOAP.
func SDKMethod(body []byte) string {
	if m := sdkMethodRe.FindSubmatch(body); m != nil {
		return string(m[1])
	}
	return ""
}

// ObjectType reads the type of the managed object a SOAP request is sent to
// (its _this element), or "".
var thisTypeRe = regexp.MustCompile(`<(?:[A-Za-z0-9_]+:)?_this[^>]*\btype="([A-Za-z0-9_]+)"`)

// ObjectType returns the _this type of a SOAP body.
func ObjectType(body []byte) string {
	if m := thisTypeRe.FindSubmatch(body); m != nil {
		return string(m[1])
	}
	return ""
}

// loginUserRe reads the account a Login request names. The password element
// next to it is never read.
var loginUserRe = regexp.MustCompile(`<(?:[A-Za-z0-9_]+:)?userName[^>]*>([^<]{1,256})</`)

// LoginUser returns the account name a SOAP Login body carries, or "".
func LoginUser(body []byte) string {
	if m := loginUserRe.FindSubmatch(body); m != nil {
		return strings.TrimSpace(html.UnescapeString(string(m[1])))
	}
	return ""
}
