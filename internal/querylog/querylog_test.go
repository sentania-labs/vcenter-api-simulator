package querylog

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogRedactsLoginAndRecordsResponse(t *testing.T) {
	var log bytes.Buffer
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("test"))
	}), &log)
	body := `<Envelope><Body><Login><userName>alice</userName><password>secret</password></Login></Body></Envelope>`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(body)))
	if strings.Contains(log.String(), "secret") || strings.Contains(log.String(), "alice") {
		t.Fatal("credentials included in log")
	}
	var got record
	if err := json.Unmarshal(log.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Operation != "Login" || got.Request != "" || got.Status != 418 || got.Bytes != 4 {
		t.Fatalf("unexpected record: %+v", got)
	}
}

func TestLogRedactsEverySessionManagerOperation(t *testing.T) {
	for _, op := range []string{"Login", "CloneSession", "ImpersonateUser", "AcquireGenericServiceTicket"} {
		var log bytes.Buffer
		h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), &log)
		body := `<Envelope><Body><` + op + ` xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><cloneTicket>TICKET</cloneTicket></` + op + `></Body></Envelope>`
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(body)))
		var got record
		if err := json.Unmarshal(log.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(log.String(), "TICKET") || got.Operation != op || got.Request != "" {
			t.Fatalf("%s leaked or misrecorded: %s", op, log.String())
		}
	}
}

func TestLogKeepsOtherRequestBodies(t *testing.T) {
	var log bytes.Buffer
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), &log)
	body := `<Envelope><Body><RetrievePropertiesEx xmlns="urn:vim25"><_this type="PropertyCollector">propertyCollector</_this></RetrievePropertiesEx></Body></Envelope>`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(body)))
	var got record
	if err := json.Unmarshal(log.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Operation != "RetrievePropertiesEx" || got.Request != body {
		t.Fatalf("unexpected record: %+v", got)
	}
}

func TestLogRejectsOversizedBody(t *testing.T) {
	var log bytes.Buffer
	called := false
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }), &log)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/sdk", strings.NewReader(strings.Repeat("x", maxBody+1))))
	if rec.Code != http.StatusRequestEntityTooLarge || called {
		t.Fatalf("code %d, downstream called %v", rec.Code, called)
	}
}
