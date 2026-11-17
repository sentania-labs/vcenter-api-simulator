package serve

import (
	"net/http"
	"sync/atomic"
)

type liveHandler struct{ handler http.Handler }

// Health binds before inventory construction. Liveness and SDK discovery never
// wait for the inventory lock; readiness becomes true only after installation.
type Health struct {
	versions atomic.Pointer[string]
	next     atomic.Pointer[liveHandler]
}

// SetVersions replaces the discovery document served until the inventory exists.
func (h *Health) SetVersions(doc string) { h.versions.Store(&doc) }

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
		if doc := h.versions.Load(); doc != nil {
			_, _ = w.Write([]byte(*doc))
		}
	default:
		if next == nil {
			http.Error(w, "inventory is building", http.StatusServiceUnavailable)
			return
		}
		next.handler.ServeHTTP(w, r)
	}
}
