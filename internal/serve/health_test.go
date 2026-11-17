package serve

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthDuringBuild(t *testing.T) {
	h := &Health{}
	h.SetVersions("<versions/>")
	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{{"/healthz", 200, ""}, {"/readyz", 503, "inventory is building\n"}, {"/sdk/vimServiceVersions.xml", 200, "<versions/>"}, {"/sdk", 503, "inventory is building\n"}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.status || w.Body.String() != tc.body {
			t.Fatalf("%s: %d %q", tc.path, w.Code, w.Body.String())
		}
	}
	public := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) })
	h.Install(public, public)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/sdk", nil))
	if w.Code != 201 {
		t.Fatal(w.Code)
	}
}

func waitForProbe(t *testing.T, live *liveHandler) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for live.probing.Load() {
		if time.Now().After(deadline) {
			t.Fatal("probe did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

// Oracle: health stays responsive through temporary load, fails after a bounded
// SOAP stall, and recovers when the outstanding SOAP request completes.
func TestHealthSOAPStall(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	h := &Health{}
	soap := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		if r.Method != http.MethodPost || r.URL.Path != "/sdk" || r.Header.Get("SOAPAction") == "" {
			t.Error("not a SOAP probe")
		}
		_, _ = w.Write([]byte(`<RetrieveServiceContentResponse xmlns="urn:vim25"><returnval>2026-11-17T00:00:00Z</returnval></RetrieveServiceContentResponse>`))
	})
	h.Install(soap, soap)
	live := h.next.Load()
	released := false
	defer func() {
		if !released {
			close(release)
			waitForProbe(t, live)
		}
	}()
	probe := func(want int) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
		if w.Code != want {
			t.Fatalf("health = %d, want %d", w.Code, want)
		}
	}
	probe(200)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("SOAP probe not started")
	}
	live.lastSuccess.Store(time.Now().Add(-time.Minute).UnixNano())
	probe(200)
	live.lastSuccess.Store(time.Now().Add(-livenessGrace - time.Second).UnixNano())
	for i := 0; i < 20; i++ {
		probe(503)
	}
	if calls.Load() != 1 {
		t.Fatal("hung probes accumulated")
	}
	close(release)
	released = true
	waitForProbe(t, live)
	probe(200)
}

func TestHealthSOAPResult(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		healthy    bool
	}{
		{"SOAP success", `<RetrieveServiceContentResponse/>`, 200, true},
		{"SOAP fault", `<Fault/>`, 500, false},
		{"wrong route", `<html>ok</html>`, 200, false},
		{"handler panic", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Health{}
			probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 0 {
					panic("probe failure")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			h.Install(probe, probe)
			live := h.next.Load()
			live.lastSuccess.Store(time.Now().Add(-livenessGrace - time.Second).UnixNano())
			live.live(time.Now())
			waitForProbe(t, live)
			if got := live.live(time.Now()); got != tc.healthy {
				t.Fatalf("live=%v", got)
			}
		})
	}
}

// Oracle: an operator pause drops every public connection and the public
// handler records each request. Liveness must not depend on either, so a paused
// simulator stays alive and the probe never reaches the public handler.
func TestHealthProbeBypassesPublicHandler(t *testing.T) {
	var public atomic.Int32
	h := &Health{}
	h.Install(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		public.Add(1)
		panic(http.ErrAbortHandler)
	}), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<RetrieveServiceContentResponse/>`))
	}))
	live := h.next.Load()
	live.lastSuccess.Store(time.Now().Add(-livenessGrace - time.Second).UnixNano())
	live.live(time.Now())
	waitForProbe(t, live)
	if !live.live(time.Now()) || public.Load() != 0 {
		t.Fatalf("live probe reached the public handler %d times or failed", public.Load())
	}
}
