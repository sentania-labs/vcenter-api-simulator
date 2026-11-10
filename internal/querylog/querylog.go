// Package querylog records request metadata without credentials or headers.
package querylog

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"sync"
	"time"
)

const maxBody = 32 << 20

type logger struct {
	next http.Handler
	mu   sync.Mutex
	enc  *json.Encoder
}
type record struct {
	Time       time.Time `json:"time"`
	Path       string    `json:"path"`
	Method     string    `json:"method"`
	Operation  string    `json:"operation,omitempty"`
	Request    string    `json:"request,omitempty"`
	Status     int       `json:"status"`
	Bytes      int       `json:"bytes"`
	DurationMS float64   `json:"durationMs"`
}
type response struct {
	http.ResponseWriter
	status, bytes int
}

func (w *response) WriteHeader(n int) {
	if w.status == 0 {
		w.status = n
		w.ResponseWriter.WriteHeader(n)
	}
}
func (w *response) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, e := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, e
}

// Wrap emits one JSON line per request. SOAP bodies are deliberately omitted for
// any SessionManager operation (logins, clone tickets, impersonation) and for
// requests that name no target; the log
// contains no HTTP headers, cookies or passwords.
// Enable only in a controlled environment: other SOAP requests can contain
// inventory identifiers.
func Wrap(next http.Handler, dst io.Writer) http.Handler {
	return &logger{next: next, enc: json.NewEncoder(dst)}
}
func (l *logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var data []byte
	if r.Body != nil {
		var err error
		data, err = io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil {
			http.Error(w, "cannot read request body", http.StatusBadRequest)
			return
		}
		if len(data) > maxBody {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
	}
	op, target := "", ""
	dec := xml.NewDecoder(bytes.NewReader(data))
	inBody := false
	for {
		t, e := dec.Token()
		if e != nil {
			break
		}
		if s, ok := t.(xml.StartElement); ok {
			if inBody && op == "" {
				op = s.Name.Local
				continue
			}
			if op != "" && s.Name.Local == "_this" {
				for _, a := range s.Attr {
					if a.Name.Local == "type" {
						target = a.Value
					}
				}
				break
			}
			if s.Name.Local == "Body" {
				inBody = true
			}
		}
	}
	request := ""
	if target != "" && target != "SessionManager" {
		request = string(data)
	}
	rw := &response{ResponseWriter: w}
	l.next.ServeHTTP(rw, r)
	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.enc.Encode(record{Time: start.UTC(), Path: r.URL.Path, Method: r.Method, Operation: op, Request: request, Status: rw.status, Bytes: rw.bytes, DurationMS: float64(time.Since(start)) / float64(time.Millisecond)})
}
