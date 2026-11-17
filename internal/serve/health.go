package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"
)

// One probe may be outstanding. A stuck SOAP handler cannot accumulate workers.
// Two minutes tolerates transient load; Kubernetes adds its failure threshold.
const livenessGrace = 2 * time.Minute
const livenessInterval = 10 * time.Second

type liveHandler struct {
	public      http.Handler
	handler     http.Handler
	lastSuccess atomic.Int64
	lastAttempt atomic.Int64
	probing     atomic.Bool
}

func (h *liveHandler) live(now time.Time) bool {
	if now.Sub(time.Unix(0, h.lastAttempt.Load())) >= livenessInterval && h.probing.CompareAndSwap(false, true) {
		// Recheck after acquiring the slot: another caller may have completed
		// a fast probe between our initial timestamp read and the CAS.
		if now.Sub(time.Unix(0, h.lastAttempt.Load())) < livenessInterval {
			h.probing.Store(false)
			return now.Sub(time.Unix(0, h.lastSuccess.Load())) < livenessGrace
		}
		h.lastAttempt.Store(now.UnixNano())
		go func() {
			// net/http normally recovers handler panics. This internal call
			// needs the same protection; a panic is a failed probe.
			defer func() { _ = recover(); h.probing.Store(false) }()
			request := httptest.NewRequest(http.MethodPost, "http://localhost/sdk", strings.NewReader(`<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body><RetrieveServiceContent xmlns="urn:vim25"><_this type="ServiceInstance">ServiceInstance</_this></RetrieveServiceContent></soapenv:Body></soapenv:Envelope>`))
			request.Header.Set("Content-Type", "text/xml; charset=utf-8")
			request.Header.Set("SOAPAction", `"urn:vim25/6.0"`)
			response := httptest.NewRecorder()
			h.handler.ServeHTTP(response, request)
			if response.Code == http.StatusOK && strings.Contains(response.Body.String(), "RetrieveServiceContentResponse") {
				h.lastSuccess.Store(time.Now().UnixNano())
			}
		}()
	}
	return now.Sub(time.Unix(0, h.lastSuccess.Load())) < livenessGrace
}

// Health binds before inventory construction. Liveness and SDK discovery never
// wait for the inventory lock; readiness becomes true only after installation.
type Health struct {
	versions atomic.Pointer[string]
	next     atomic.Pointer[liveHandler]
}

// SetVersions replaces the discovery document served until the inventory exists.
func (h *Health) SetVersions(doc string) { h.versions.Store(&doc) }

// Install makes next the public handler. probe is the simulator beneath the
// pause, activity and capture layers: liveness asks it, so an operator pause
// does not restart the pod and the probe is not recorded as a client.
func (h *Health) Install(next, probe http.Handler) {
	live := &liveHandler{public: next, handler: probe}
	live.lastSuccess.Store(time.Now().UnixNano())
	h.next.Store(live)
}
func (h *Health) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	next := h.next.Load()
	switch r.URL.Path {
	case "/healthz":
		if next != nil && !next.live(time.Now()) {
			http.Error(w, "SOAP handler is not responding", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	case "/readyz":
		if next == nil {
			http.Error(w, "inventory is building", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	case "/sdk/vimServiceVersions.xml":
		if next != nil {
			next.public.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/xml; charset=UTF-8")
		if doc := h.versions.Load(); doc != nil {
			_, _ = w.Write([]byte(*doc))
		}
	default:
		if next == nil {
			http.Error(w, "inventory is building", http.StatusServiceUnavailable)
			return
		}
		next.public.ServeHTTP(w, r)
	}
}
