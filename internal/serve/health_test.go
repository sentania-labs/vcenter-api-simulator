package serve

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthDuringBuild(t *testing.T) {
	h := &Health{Versions: "<versions/>"}
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
	h.Install(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) }))
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
