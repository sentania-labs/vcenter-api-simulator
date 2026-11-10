// Package activity records what the simulator's clients ask of it: one entry
// per /sdk or /vsanHealth request, redacted (no headers, cookies, bodies or
// credentials are kept), in a bounded ring, with the aggregates the control
// console draws: requests per minute, per-method and per-family totals, the
// duration distribution, bytes, faults and the diagnosis of a collection
// that returned no metrics.
package activity

import (
	"bytes"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Results of a call.
const (
	ResultOK       = "ok"
	ResultInjected = "injected" // a fault the scenario asked for
	ResultFault    = "fault"    // any other SOAP fault or HTTP error
	ResultRefused  = "refused"  // the service is paused; the connection was dropped
	ResultDenied   = "denied"   // refused as not authenticated: a wrong login, or no session
)

// Call is one recorded request. Nothing in it identifies a credential.
type Call struct {
	Seq        uint64    `json:"seq"`
	Time       time.Time `json:"time"`
	Surface    string    `json:"surface"` // sdk | vsanHealth
	Method     string    `json:"method"`
	Family     string    `json:"family,omitempty"`
	Cluster    string    `json:"cluster,omitempty"`
	Client     string    `json:"client"`
	DurationMs float64   `json:"durationMs"`
	Bytes      int64     `json:"bytes"`
	Status     int       `json:"status"`
	Result     string    `json:"result"`
	Fault      string    `json:"fault,omitempty"`
	Message    string    `json:"message,omitempty"`
	// Entities and Empty count the entities a VsanPerfQueryPerf answer
	// carried and how many of them had no samples.
	Entities int `json:"entities,omitempty"`
	Empty    int `json:"empty,omitempty"`
	// Body is the request body of a /vsanHealth call (never /sdk, whose
	// Login carries a password), kept for the detail view and cut at
	// MaxBody bytes. It holds no cookie or header.
	Body string `json:"body,omitempty"`
}

// MaxBody bounds the kept request body.
const MaxBody = 2048

// Log is the bounded request log.
type Log struct {
	// EmptyKind says whether an entity family is one the service answers
	// empty on this cluster type; set once by the owner, read by Summarize.
	EmptyKind func(family string) bool

	mu    sync.Mutex
	ring  []Call
	next  int
	full  bool
	seq   uint64
	since time.Time

	total, injected, faults, refused, denied uint64
	bytes                                    uint64
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
	l.bytes += uint64(c.Bytes)
	switch c.Result {
	case ResultInjected:
		l.injected++
	case ResultFault:
		l.faults++
	case ResultRefused:
		l.refused++
	case ResultDenied:
		l.denied++
	}
	return c
}

// all returns the held calls, oldest first.
func (l *Log) all() []Call {
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

// Filter selects calls; empty fields match everything.
type Filter struct {
	Surface, Method, Family, Cluster, Result string
	AfterSeq                                 uint64 // only calls newer than this
	Limit                                    int
}

// Brief returns the call without its body, for list views.
func (c Call) Brief() Call { c.Body = ""; return c }

// Query returns matching calls, newest first, without request bodies.
func (l *Log) Query(f Filter) []Call {
	calls := l.all()
	if f.Limit <= 0 {
		f.Limit = 200
	}
	var out []Call
	for i := len(calls) - 1; i >= 0 && len(out) < f.Limit; i-- {
		c := calls[i]
		switch {
		case f.AfterSeq != 0 && c.Seq <= f.AfterSeq:
			return out
		case f.Surface != "" && c.Surface != f.Surface,
			f.Method != "" && c.Method != f.Method,
			f.Family != "" && c.Family != f.Family,
			f.Cluster != "" && c.Cluster != f.Cluster,
			f.Result != "" && c.Result != f.Result:
			continue
		}
		out = append(out, c.Brief())
	}
	return out
}

// Get returns one call by sequence number.
func (l *Log) Get(seq uint64) (Call, bool) {
	for _, c := range l.all() {
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
	Injected uint64    `json:"injected"`
	Faults   uint64    `json:"faults"`
	Refused  uint64    `json:"refused"`
	Denied   uint64    `json:"denied"`
	Bytes    uint64    `json:"bytes"`
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
	return Totals{Since: l.since, Requests: l.total, Injected: l.injected, Faults: l.faults,
		Refused: l.refused, Denied: l.denied, Bytes: l.bytes, Held: held}
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

// ---------------------------------------------------------------------------
// aggregates

// Summary is what the console draws from the log.
type Summary struct {
	Now      time.Time    `json:"now"`
	Totals   Totals       `json:"totals"`
	Minutes  []Minute     `json:"minutes"` // the last 60, oldest first
	Rate     float64      `json:"ratePerSec"`
	P95Ms    float64      `json:"p95Ms"`
	Faults   FaultSummary `json:"faults"`
	Methods  []MethodStat `json:"methods"`
	Families []FamilyStat `json:"families"`
	Duration []Bucket     `json:"duration"`
	Cycle    Cycle        `json:"lastCycle"`
	Clusters []string     `json:"clusters"`
}

// Minute is one bar of requests per minute.
type Minute struct {
	T          time.Time `json:"t"`
	Requests   int       `json:"requests"`
	Injected   int       `json:"injected"`
	Unexpected int       `json:"unexpected"`
	Bytes      int64     `json:"bytes"`
}

// FaultSummary splits the faults of the last hour.
type FaultSummary struct {
	Denied     int     `json:"denied"`
	Injected   int     `json:"injected"`
	Unexpected int     `json:"unexpected"`
	Empty      int     `json:"empty"`
	Percent    float64 `json:"percent"`
}

// MethodStat aggregates one operation.
type MethodStat struct {
	Surface    string  `json:"surface"`
	Method     string  `json:"method"`
	Calls      int     `json:"calls"`
	P50Ms      float64 `json:"p50Ms"`
	P95Ms      float64 `json:"p95Ms"`
	Injected   int     `json:"injected"`
	Unexpected int     `json:"unexpected"`
	Denied     int     `json:"denied"`
	Bytes      int64   `json:"bytes"`
}

// FamilyStat aggregates the perf queries of one entity family.
type FamilyStat struct {
	Family     string  `json:"family"`
	Calls      int     `json:"calls"`
	Entities   int     `json:"entities"`
	Empty      int     `json:"empty"`
	P95Ms      float64 `json:"p95Ms"`
	Injected   int     `json:"injected"`
	Unexpected int     `json:"unexpected"`
	Bytes      int64   `json:"bytes"`
}

// Bucket is one bar of the duration distribution.
type Bucket struct {
	Label  string  `json:"label"`
	UpToMs float64 `json:"upToMs"` // 0 for the open last bucket
	Count  int     `json:"count"`
}

var bounds = []float64{10, 50, 100, 250, 500, 1000, 5000}

// Cycle is the most recent collection burst: calls with gaps under a minute.
type Cycle struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Calls    int       `json:"calls"`
	Client   string    `json:"client"`
	Steps    []Step    `json:"steps"`
	Findings []Finding `json:"findings"`
	Series   int       `json:"entitiesWithData"`
	Empty    int       `json:"emptyEntities"`
}

// Step is one stage of a collection as the pack runs it.
type Step struct {
	Name       string  `json:"name"`
	Calls      int     `json:"calls"`
	P95Ms      float64 `json:"p95Ms"`
	Injected   int     `json:"injected"`
	Unexpected int     `json:"unexpected"`
	Denied     int     `json:"denied"`
	Empty      int     `json:"empty"`
	// Series counts the entities that carried samples (perf queries only).
	Series int `json:"series"`
}

// Finding is one line of the "why did this collection return nothing"
// diagnosis.
type Finding struct {
	Severity string `json:"severity"` // bad | warn | info | ok
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

// Summarize computes the aggregates at time now.
func (l *Log) Summarize(now time.Time) Summary {
	calls := l.all()
	s := Summary{Now: now, Totals: l.Totals(), Clusters: []string{}, Methods: []MethodStat{}, Families: []FamilyStat{}}
	hour := now.Add(-time.Hour)
	minutes := map[time.Time]*Minute{}
	for i := 59; i >= 0; i-- {
		t := now.Truncate(time.Minute).Add(-time.Duration(i) * time.Minute)
		minutes[t] = &Minute{T: t}
		s.Minutes = append(s.Minutes, Minute{})
	}
	type key struct{ surface, method string }
	ms := map[key]*MethodStat{}
	mdur := map[key][]float64{}
	fs := map[string]*FamilyStat{}
	fdur := map[string][]float64{}
	clusters := map[string]bool{}
	hist := make([]int, len(bounds)+1)
	var alldur []float64
	recent5 := 0
	for _, c := range calls {
		if c.Cluster != "" {
			clusters[c.Cluster] = true
		}
		if c.Time.Before(hour) {
			continue
		}
		if m, ok := minutes[c.Time.Truncate(time.Minute)]; ok {
			m.Requests++
			m.Bytes += c.Bytes
			switch c.Result {
			case ResultInjected:
				m.Injected++
			case ResultFault:
				m.Unexpected++
			}
		}
		if now.Sub(c.Time) < 5*time.Minute {
			recent5++
		}
		switch c.Result {
		case ResultInjected:
			s.Faults.Injected++
		case ResultFault:
			s.Faults.Unexpected++
		case ResultDenied:
			s.Faults.Denied++
		}
		s.Faults.Empty += c.Empty
		alldur = append(alldur, c.DurationMs)
		hist[bucketOf(c.DurationMs)]++
		k := key{c.Surface, c.Method}
		st := ms[k]
		if st == nil {
			st = &MethodStat{Surface: c.Surface, Method: c.Method}
			ms[k] = st
		}
		st.Calls++
		st.Bytes += c.Bytes
		mdur[k] = append(mdur[k], c.DurationMs)
		switch c.Result {
		case ResultInjected:
			st.Injected++
		case ResultFault:
			st.Unexpected++
		case ResultDenied:
			st.Denied++
		}
		if c.Family != "" {
			f := fs[c.Family]
			if f == nil {
				f = &FamilyStat{Family: c.Family}
				fs[c.Family] = f
			}
			f.Calls++
			f.Entities += c.Entities
			f.Empty += c.Empty
			f.Bytes += c.Bytes
			fdur[c.Family] = append(fdur[c.Family], c.DurationMs)
			switch c.Result {
			case ResultInjected:
				f.Injected++
			case ResultFault:
				f.Unexpected++
			}
		}
	}
	for i := range s.Minutes {
		t := now.Truncate(time.Minute).Add(-time.Duration(59-i) * time.Minute)
		s.Minutes[i] = *minutes[t]
	}
	s.Rate = float64(recent5) / 300
	s.P95Ms = percentile(alldur, 0.95)
	if n := len(alldur); n > 0 {
		s.Faults.Percent = 100 * float64(s.Faults.Injected+s.Faults.Unexpected+s.Faults.Denied) / float64(n)
	}
	for k, v := range ms {
		v.P50Ms, v.P95Ms = percentile(mdur[k], 0.5), percentile(mdur[k], 0.95)
		s.Methods = append(s.Methods, *v)
	}
	sort.Slice(s.Methods, func(i, j int) bool {
		if s.Methods[i].Calls != s.Methods[j].Calls {
			return s.Methods[i].Calls > s.Methods[j].Calls
		}
		return s.Methods[i].Method < s.Methods[j].Method
	})
	for k, v := range fs {
		v.P95Ms = percentile(fdur[k], 0.95)
		s.Families = append(s.Families, *v)
	}
	sort.Slice(s.Families, func(i, j int) bool {
		if s.Families[i].Entities != s.Families[j].Entities {
			return s.Families[i].Entities > s.Families[j].Entities
		}
		return s.Families[i].Family < s.Families[j].Family
	})
	for i, n := range hist {
		b := Bucket{Count: n}
		if i < len(bounds) {
			b.UpToMs = bounds[i]
			b.Label = "≤ " + trimMs(bounds[i])
		} else {
			b.Label = "> " + trimMs(bounds[len(bounds)-1])
		}
		s.Duration = append(s.Duration, b)
	}
	for c := range clusters {
		s.Clusters = append(s.Clusters, c)
	}
	sort.Strings(s.Clusters)
	s.Cycle = lastCycle(calls, l.EmptyKind)
	return s
}

func trimMs(v float64) string {
	if v >= 1000 {
		return itoa(int(v/1000)) + " s"
	}
	return itoa(int(v)) + " ms"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func bucketOf(ms float64) int {
	for i, b := range bounds {
		if ms <= b {
			return i
		}
	}
	return len(bounds)
}

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(float64(len(s)-1) * p)
	return s[i]
}

// cycleGap separates two collection cycles.
const cycleGap = 30 * time.Second

// stepOf names the collection stage a call belongs to.
// isPrelude is how a collection starts: the service content, then a login.
func isPrelude(c Call) bool { return c.Method == "RetrieveServiceContent" || isLogin(c) }

func isLogin(c Call) bool { return c.Method == "Login" || c.Method == "LoginByToken" }

func stepOf(c Call) string {
	switch c.Method {
	case "Login", "RetrieveServiceContent", "RetrieveContent", "Logout", "LoginByToken":
		return "Login · RetrieveContent"
	case "VsanPerfQueryPerf", "VsanPerfQueryNodeInformation", "VsanPerfGetSupportedEntityTypes",
		"VsanClusterGetConfig", "QueryVsanManagedDisks", "VsanQueryVcClusterHealthSummary":
		return c.Method
	}
	if c.Surface == "sdk" {
		return "Inventory · container views"
	}
	return c.Method
}

var stepOrder = map[string]int{
	"Login · RetrieveContent": 0, "Inventory · container views": 1,
	"VsanPerfGetSupportedEntityTypes": 2, "VsanClusterGetConfig": 3, "VsanPerfQueryNodeInformation": 4,
	"QueryVsanManagedDisks": 5, "VsanQueryVcClusterHealthSummary": 6, "VsanPerfQueryPerf": 7,
}

func lastCycle(calls []Call, emptyKind func(string) bool) Cycle {
	var cy Cycle
	if len(calls) == 0 {
		return cy
	}
	end := len(calls) - 1
	start := end
	for start > 0 && calls[start].Time.Sub(calls[start-1].Time) <= cycleGap {
		// a login right after performance queries starts the next collection
		if isPrelude(calls[start]) && calls[start-1].Method == "VsanPerfQueryPerf" {
			break
		}
		start--
	}
	cyc := calls[start : end+1]
	cy.Start, cy.End, cy.Calls = cyc[0].Time, cyc[len(cyc)-1].Time, len(cyc)
	clients := map[string]int{}
	type agg struct {
		Step
		dur []float64
	}
	steps := map[string]*agg{}
	loginFail, loginOK := 0, 0
	perfCalls, perfInjected, perfUnexpected, unauth := 0, 0, 0, 0
	faultedFamilies := map[string]bool{}
	emptyFamilies := map[string]bool{}
	anyData := map[string]bool{}
	for _, c := range cyc {
		clients[c.Client]++
		name := stepOf(c)
		a := steps[name]
		if a == nil {
			a = &agg{Step: Step{Name: name}}
			steps[name] = a
		}
		a.Calls++
		a.dur = append(a.dur, c.DurationMs)
		a.Empty += c.Empty
		if c.Method == "VsanPerfQueryPerf" {
			a.Series += c.Entities - c.Empty
		}
		switch c.Result {
		case ResultInjected:
			a.Injected++
		case ResultFault:
			a.Unexpected++
		case ResultDenied:
			a.Denied++
		}
		if isLogin(c) {
			if c.Result == ResultOK {
				loginOK++
			} else {
				loginFail++
			}
		}
		if c.Method == "VsanPerfQueryPerf" {
			perfCalls++
			cy.Series += c.Entities - c.Empty
			cy.Empty += c.Empty
			switch c.Result {
			case ResultInjected:
				perfInjected++
				faultedFamilies[c.Family] = true
			case ResultDenied:
				unauth++
			case ResultFault:
				perfUnexpected++
			}
			if c.Result == ResultOK {
				if c.Entities-c.Empty > 0 {
					anyData[c.Family] = true
				} else if c.Entities > 0 {
					emptyFamilies[c.Family] = true
				}
			}
		}
	}
	var onlyEmpty []string
	for f := range emptyFamilies {
		if !anyData[f] {
			onlyEmpty = append(onlyEmpty, f)
		}
	}
	sort.Strings(onlyEmpty)
	provisional := len(onlyEmpty) > 0 && emptyKind != nil
	for _, f := range onlyEmpty {
		if provisional && !emptyKind(f) {
			provisional = false
		}
	}
	best := 0
	for c, n := range clients {
		if n > best {
			cy.Client, best = c, n
		}
	}
	for _, a := range steps {
		a.P95Ms = percentile(a.dur, 0.95)
		cy.Steps = append(cy.Steps, a.Step)
	}
	sort.Slice(cy.Steps, func(i, j int) bool {
		oi, ok1 := stepOrder[cy.Steps[i].Name]
		oj, ok2 := stepOrder[cy.Steps[j].Name]
		if !ok1 {
			oi = 99
		}
		if !ok2 {
			oj = 99
		}
		if oi != oj {
			return oi < oj
		}
		return cy.Steps[i].Name < cy.Steps[j].Name
	})
	cy.Findings = diagnose(loginOK, loginFail, perfCalls, perfInjected, perfUnexpected, unauth, cy.Series, cy.Empty, faultedFamilies, onlyEmpty, provisional)
	return cy
}

func diagnose(loginOK, loginFail, perfCalls, injected, unexpected, unauth, withData, empty int, faulted map[string]bool, onlyEmpty []string, provisional bool) []Finding {
	var out []Finding
	switch {
	case loginFail > 0 && loginOK == 0:
		out = append(out, Finding{"bad", "Authentication failed",
			"Every login in the last cycle was refused (" + itoa(loginFail) + "). The collector never reached an inventory, so it asked for no metrics. Check the user the MP adapter is configured with against the scenario's auth users."})
	case loginFail > 0:
		out = append(out, Finding{"warn", "Some logins were refused", itoa(loginFail) + " of " + itoa(loginFail+loginOK) + " logins failed in the last cycle."})
	case loginOK > 0:
		out = append(out, Finding{"ok", "Authentication succeeded", itoa(loginOK) + " login(s) accepted in the last cycle."})
	}
	if perfCalls == 0 && loginOK > 0 {
		out = append(out, Finding{"bad", "Authenticated, but no performance queries followed",
			"The collector logged in and never called VsanPerfQueryPerf. It found no vSAN cluster in the inventory, or its adapter instance is not set to collect performance metrics."})
	}
	if injected > 0 {
		names := make([]string, 0, len(faulted))
		for f := range faulted {
			names = append(names, f)
		}
		sort.Strings(names)
		list := ""
		for i, n := range names {
			if i == 5 {
				list += ", …"
				break
			}
			if i > 0 {
				list += ", "
			}
			list += n
		}
		out = append(out, Finding{"warn", itoa(injected) + " performance queries answered with an injected fault",
			"Scenario fault rules caused these (" + list + "). The pack reports SystemError as a collection problem and treats InvalidArgument and NotSupported as 'kind absent'."})
	}
	if unexpected > 0 {
		out = append(out, Finding{"bad", itoa(unexpected) + " performance queries failed without an injected rule",
			"These faults did not come from a scenario rule. Open the request log filtered to Result = unexpected."})
	}
	if unauth > 0 {
		out = append(out, Finding{"bad", itoa(unauth) + " performance queries were refused as not authenticated",
			"They carried no valid session, so no rule or family was consulted. Fix the login first; everything below is a consequence."})
	}
	if perfCalls > 0 && withData == 0 && injected == 0 && unexpected == 0 && unauth == 0 {
		list := strings.Join(onlyEmpty[:min(len(onlyEmpty), 6)], ", ")
		if provisional {
			out = append(out, Finding{"bad", "Every performance query returned empty",
				"The collector asked only for " + list + ". The live service answers these provisional kinds (OSA, iSCSI target, PMem, file service) empty on an ESA cluster, so authentication was fine and there was nothing to collect. Point the adapter instance at the kinds an ESA cluster serves, or at a scope that includes them."})
		} else {
			out = append(out, Finding{"bad", "Every performance query returned empty",
				"The simulator answered, but no entity had samples" + func() string {
					if list != "" {
						return " (families: " + list + ")"
					}
					return ""
				}() + ". The cluster may have no hosts for the queried families."})
		}
	}
	if empty > 0 && withData > 0 {
		out = append(out, Finding{"info", itoa(empty) + " entities returned empty by design",
			"Provisional kinds (OSA, iSCSI, PMem, file service) answer empty on an ESA cluster, as the live service does."})
	}
	if withData > 0 {
		out = append(out, Finding{"ok", itoa(withData) + " entities returned samples", "Metrics were served in the last cycle."})
	}
	if len(out) == 0 {
		out = append(out, Finding{"info", "No collector traffic", "Nothing has called the simulator yet."})
	}
	return out
}
