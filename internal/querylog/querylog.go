// Package querylog captures client requests without recording credentials.
package querylog

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxBody = 32 << 20
const maxRecordedBody = 64 << 10

type record struct {
	Time                time.Time `json:"time"`
	Path                string    `json:"path"`
	Method              string    `json:"method"`
	Operation           string    `json:"operation,omitempty"`
	ObjectType          string    `json:"objectType,omitempty"`
	Properties          []string  `json:"properties,omitempty"`
	Request             string    `json:"request,omitempty"`
	RequestTruncated    bool      `json:"requestTruncated,omitempty"`
	PropertiesTruncated bool      `json:"propertiesTruncated,omitempty"`
	PathTruncated       bool      `json:"pathTruncated,omitempty"`
	Fault               string    `json:"fault,omitempty"`
	Unanswered          bool      `json:"unanswered,omitempty"`
	Status              int       `json:"status"`
	Bytes               int       `json:"bytes"`
	DurationMS          float64   `json:"durationMs"`
}

type response struct {
	http.ResponseWriter
	status, bytes int
	head          []byte
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
	if len(w.head) < 256<<10 {
		w.head = append(w.head, b[:min(len(b), (256<<10)-len(w.head))]...)
	}
	n, e := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, e
}
func (w *response) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type logger struct {
	next http.Handler
	mu   sync.Mutex
	enc  *json.Encoder
}

// Wrap writes a redacted JSON line for every request, including missing paths.
func Wrap(next http.Handler, dst io.Writer) http.Handler {
	return &logger{next: next, enc: json.NewEncoder(dst)}
}
func (l *logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	serve(w, r, l.next, func(rec record) { l.mu.Lock(); defer l.mu.Unlock(); _ = l.enc.Encode(rec) })
}
func serve(w http.ResponseWriter, r *http.Request, next http.Handler, save func(record)) {
	start := time.Now()
	rw := &response{ResponseWriter: w}
	rec := record{Time: start.UTC(), Path: r.URL.Path, Method: r.Method}
	defer func() {
		if rw.status == 0 {
			rw.status = http.StatusOK
		}
		rec.Status = rw.status
		rec.Bytes = rw.bytes
		rec.DurationMS = float64(time.Since(start)) / float64(time.Millisecond)
		rec.Fault = fault(rw.head)
		rec.Unanswered = rw.status == 404 || rw.status == 405 || isUnansweredFault(rec.Fault) || (rw.status >= 500 && rec.Fault == "")
		save(rec)
	}()
	if r.Body != nil {
		data, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil {
			http.Error(rw, "cannot read request body", http.StatusBadRequest)
			return
		}
		if len(data) > maxBody {
			http.Error(rw, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		rec.Operation, rec.ObjectType, rec.Properties, rec.Request = inspect(data)
	}
	next.ServeHTTP(rw, r)
}

// sensitive recognizes credential-bearing elements and attributes regardless
// of case and namespace. Unknown or malformed bodies are omitted entirely.
func sensitive(s string) bool {
	s = strings.ToLower(s)
	for _, word := range []string{"password", "passwd", "secret", "token", "cookie", "ticket", "credential", "authorization", "session", "hash", "assertion", "privatekey", "licensekey", "signedentitlement"} {
		if strings.Contains(s, word) {
			return true
		}
	}
	return false
}
func inspect(data []byte) (op, target string, props []string, body string) {
	if len(data) == 0 {
		return
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	var spans [][2]int64
	var depth int
	inBody := false
	opDepth := 0
	propDepth := 0
	var current string
	isSOAP := false
	sessionOp := false
	sessionTarget := false
	for {
		before := d.InputOffset()
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return op, target, props, ""
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Local == "Envelope" {
				isSOAP = true
			}
			if t.Name.Local == "Body" {
				inBody = true
				continue
			}
			if inBody && op == "" {
				op = t.Name.Local
				opDepth = depth
				low := strings.ToLower(op)
				sessionOp = strings.Contains(low, "login") || strings.Contains(low, "session") || strings.Contains(low, "ticket") || strings.Contains(low, "token")
			}
			if inBody && t.Name.Local == "propSet" {
				propDepth = depth
			}
			if inBody && t.Name.Local == "_this" {
				for _, a := range t.Attr {
					if a.Name.Local == "type" {
						target = a.Value
						if a.Value == "SessionManager" {
							sessionTarget = true
						}
					}
				}
			}
			redact := t.Name.Local == "Header" || sensitive(t.Name.Local)
			for _, a := range t.Attr {
				if sensitive(a.Name.Local) || (a.Name.Local == "type" && sensitive(a.Value)) {
					redact = true
				}
			}
			if redact {
				if err := d.Skip(); err != nil {
					return op, target, props, ""
				}
				spans = append(spans, [2]int64{before, d.InputOffset()})
				depth--
				continue
			}
			current = t.Name.Local
		case xml.CharData:
			if inBody && current == "type" && propDepth > 0 {
				if v := strings.TrimSpace(string(t)); v != "" {
					target = v
				}
			}
			if inBody && (current == "pathSet" || (op == "Fetch" && current == "prop") || ((op == "QueryOptions" || op == "FindExtension") && (current == "name" || current == "extensionKey"))) {
				v := strings.TrimSpace(string(t))
				if v != "" {
					props = append(props, v)
				}
			}
		case xml.EndElement:
			if depth == propDepth {
				propDepth = 0
			}
			if depth == opDepth {
				inBody = false
			}
			depth--
			current = ""
		}
	}
	if !isSOAP || op == "" || sessionTarget || sessionOp {
		return op, target, props, ""
	}
	var out bytes.Buffer
	at := int64(0)
	for _, s := range spans {
		out.Write(data[at:s[0]])
		out.WriteString("<redacted/>")
		at = s[1]
	}
	out.Write(data[at:])
	return op, target, props, out.String()
}

// Redact returns a safe SOAP request body, or an empty string when the body
// cannot be parsed safely or belongs to a session operation.
func Redact(data []byte) string { _, _, _, body := inspect(data); return body }

func isUnansweredFault(f string) bool {
	switch f {
	case "NotAuthenticated", "MethodNotFound", "InvalidRequest", "NotSupported", "InvalidProperty", "ManagedObjectNotFound":
		return true
	}
	return false
}
func fault(data []byte) string {
	d := xml.NewDecoder(bytes.NewReader(data))
	inDetail := false
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "detail" {
				inDetail = true
				continue
			}
			name := strings.TrimSuffix(t.Name.Local, "Fault")
			for _, a := range t.Attr {
				if a.Name.Local == "type" {
					name = a.Value
					if i := strings.LastIndex(name, ":"); i >= 0 {
						name = name[i+1:]
					}
					name = strings.TrimSuffix(name, "Fault")
					if isUnansweredFault(name) {
						return name
					}
				}
			}
			if inDetail || isUnansweredFault(name) {
				return name
			}
		case xml.EndElement:
			if t.Name.Local == "detail" {
				inDetail = false
			}
		}
	}
	return ""
}
