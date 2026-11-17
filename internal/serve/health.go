package serve

import (
	"net/http"
	"sync/atomic"
)

type liveHandler struct{ handler http.Handler }

// Health binds before inventory construction. Liveness and SDK discovery never
// wait for the inventory lock; readiness becomes true only after installation.
type Health struct {
	Versions string
	next     atomic.Pointer[liveHandler]
}

func (h *Health) Install(next http.Handler) { h.next.Store(&liveHandler{next}) }
func (h *Health) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	next := h.next.Load()
	switch r.URL.Path {
	case "/healthz":
		w.WriteHeader(http.StatusOK)
	case "/readyz":
		if next == nil {
			http.Error(w, "inventory is building", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	case "/sdk/vimServiceVersions.xml":
		if next != nil {
			next.handler.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/xml; charset=UTF-8")
		_, _ = w.Write([]byte(h.Versions))
	default:
		if next == nil {
			http.Error(w, "inventory is building", http.StatusServiceUnavailable)
			return
		}
		next.handler.ServeHTTP(w, r)
	}
}
