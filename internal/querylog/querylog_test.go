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
