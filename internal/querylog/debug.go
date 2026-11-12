package querylog

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// maxDebugBody bounds the scrubbed body written to the log.
const maxDebugBody = 64 << 10

// maxDebugScan bounds how much of a request body is scrubbed. Anything past it
// is dropped unseen, never logged.
const maxDebugScan = 1 << 20

// debugHeaders are the only headers whose values are logged.
var debugHeaders = []string{"Content-Type", "SOAPAction", "User-Agent", "Accept", "Content-Length"}

// scrubElements are elements whose content is replaced, matched by local name
// without regard to case or namespace prefix.
var scrubElements = map[string]bool{
	"security": true, "assertion": true, "binarysecuritytoken": true, "signaturevalue": true,
	"x509data": true, "x509certificate": true, "password": true, "username": true,
	"sessionid": true, "sessionkey": true, "sessioncookie": true, "vmware-api-session-id": true,
	"ticket": true, "token": true, "secret": true, "securitycontexttoken": true,
	"cookie": true, "authorization": true,
}

var (
	longRun    = regexp.MustCompile(`[A-Za-z0-9+/=_-]{65,}`)
	jsonSecret = regexp.MustCompile(`(?i)("[^"\\]*(?:password|passwd|secret|token|session|authorization|cookie|ticket|credential|assertion|username|user)[^"\\]*"\s*:\s*)("(?:[^"\\]|\\.)*")`)
	formSecret = regexp.MustCompile(`(?i)((?:^|[&;\s?])[\w.\-]*(?:password|passwd|secret|token|session|authorization|cookie|ticket|credential|assertion|username)[\w.\-]*=)([^&;\s]*)`)
)

func placeholder(kind string, n int) string { return fmt.Sprintf("[scrubbed:%s len=%d]", kind, n) }

func scrubRuns(s string) string {
	return longRun.ReplaceAllStringFunc(s, func(m string) string { return placeholder("run", len(m)) })
}

// ScrubBody removes credentials, tokens and long opaque runs from a request body
// and keeps element names, namespaces, attributes, operations and their order.
func ScrubBody(data []byte) string {
	if !utf8.Valid(data) {
		return placeholder("binary", len(data))
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n\xef\xbb\xbf")
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] != '<' {
		s := jsonSecret.ReplaceAllStringFunc(string(data), func(m string) string {
			p := jsonSecret.FindStringSubmatch(m)
			return p[1] + `"` + placeholder("value", len(p[2])-2) + `"`
		})
		s = formSecret.ReplaceAllStringFunc(s, func(m string) string {
			p := formSecret.FindStringSubmatch(m)
			if p[2] == "" {
				return m
			}
			return p[1] + placeholder("value", len(p[2]))
		})
		return scrubRuns(s)
	}
	return scrubRuns(scrubXML(data))
}

func localName(n xml.Name) string { return strings.ToLower(n.Local) }

func scrubXML(data []byte) string {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = false
	var out bytes.Buffer
	at := int64(0)
	for {
		before := d.InputOffset()
		tok, err := d.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Everything from the last good token on is unreadable: drop it.
			out.Write(data[at:before])
			out.WriteString(placeholder("unparsed", len(data)-int(before)))
			return out.String()
		}
		start, ok := tok.(xml.StartElement)
		if !ok || !scrubElements[localName(start.Name)] {
			continue
		}
		contentStart := d.InputOffset()
		depth := 1
		contentEnd := int64(-1)
		for depth > 0 {
			b := d.InputOffset()
			t, err := d.RawToken()
			if err != nil {
				// Unterminated sensitive element: the rest is its content.
				contentEnd = int64(len(data))
				depth = 0
				break
			}
			switch t.(type) {
			case xml.StartElement:
				depth++
			case xml.EndElement:
				depth--
				if depth == 0 {
					contentEnd = b
				}
			}
		}
		if contentEnd > contentStart {
			out.Write(data[at:contentStart])
			out.WriteString(placeholder(start.Name.Local, int(contentEnd-contentStart)))
			at = contentEnd
		}
		if contentEnd == int64(len(data)) {
			break
		}
	}
	out.Write(data[at:])
	return out.String()
}

func scrubQuery(raw string) string {
	if raw == "" {
		return ""
	}
	var parts []string
	for _, kv := range strings.Split(raw, "&") {
		k, v, has := strings.Cut(kv, "=")
		if has && v != "" {
			if dk, err := url.QueryUnescape(k); err != nil || sensitiveKey(dk) {
				v = placeholder("value", len(v))
			}
			parts = append(parts, k+"="+v)
			continue
		}
		parts = append(parts, kv)
	}
	return scrubRuns(strings.Join(parts, "&"))
}
func sensitiveKey(k string) bool {
	k = strings.ToLower(k)
	for _, w := range []string{"password", "passwd", "secret", "token", "session", "auth", "cookie", "ticket", "credential", "assertion", "key", "user"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

type debugRecord struct {
	Event         string            `json:"event"`
	Time          time.Time         `json:"time"`
	Reason        string            `json:"reason"`
	Method        string            `json:"method"`
	Path          string            `json:"path"`
	Query         string            `json:"query,omitempty"`
	Status        int               `json:"status"`
	Headers       map[string]string `json:"headers,omitempty"`
	HeaderNames   []string          `json:"headerNames,omitempty"`
	Body          string            `json:"body,omitempty"`
	BodyBytes     int               `json:"bodyBytes"`
	BodyTruncated bool              `json:"bodyTruncated,omitempty"`
	BodyCut       string            `json:"bodyNote,omitempty"`
}

func isLicenseService(p string) bool { return p == "/ls" || strings.HasPrefix(p, "/ls/") }

// debugReason says why a finished request is worth logging, or "".
func debugReason(path string, status int) string {
	switch {
	case isLicenseService(path):
		return "ls-sdk"
	case status == http.StatusNotFound, status == http.StatusMethodNotAllowed, status == http.StatusNotImplemented:
		return "unanswered"
	}
	return ""
}

func buildDebug(r *http.Request, body []byte, bodyCut bool, status int, reason string, now time.Time) debugRecord {
	rec := debugRecord{Event: "request-capture", Time: now.UTC(), Reason: reason, Method: r.Method,
		Path: scrubRuns(r.URL.Path), Query: scrubQuery(r.URL.RawQuery), Status: status, BodyBytes: len(body)}
	rec.Headers = map[string]string{}
	for _, h := range debugHeaders {
		if v := r.Header.Get(h); v != "" {
			rec.Headers[h] = scrubRuns(v)
		}
	}
	for name := range r.Header {
		rec.HeaderNames = append(rec.HeaderNames, name)
	}
	if _, ok := r.Header["Content-Length"]; !ok && r.ContentLength >= 0 {
		rec.Headers["Content-Length"] = fmt.Sprint(r.ContentLength)
	}
	sort.Strings(rec.HeaderNames)
	scrubbed := ScrubBody(body)
	if len(scrubbed) > maxDebugBody {
		cut := maxDebugBody
		for cut > 0 && !utf8.RuneStart(scrubbed[cut]) {
			cut--
		}
		scrubbed = scrubbed[:cut]
		rec.BodyTruncated = true
	}
	if bodyCut {
		rec.BodyTruncated = true
	}
	if rec.BodyTruncated {
		rec.BodyCut = fmt.Sprintf("body cut for the log at %d bytes", maxDebugBody)
	}
	rec.Body = strings.Clone(scrubbed)
	return rec
}

// debugWrap logs license-service and unanswered requests with headers and a
// scrubbed body to stdout.
func (c *Capture) debugWrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		on := c.settings.Debug && c.stdout != nil
		c.mu.Unlock()
		if !on {
			next.ServeHTTP(w, r)
			return
		}
		var body []byte
		cut := false
		if r.Body != nil {
			head, _ := io.ReadAll(io.LimitReader(r.Body, maxDebugScan+1))
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
			body = head
			if len(body) > maxDebugScan {
				body, cut = body[:maxDebugScan], true
			}
		}
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		if rw.status == 0 {
			rw.status = http.StatusOK
		}
		reason := debugReason(r.URL.Path, rw.status)
		if reason == "" {
			return
		}
		rec := buildDebug(r, body, cut, rw.status, reason, start)
		c.mu.Lock()
		defer c.mu.Unlock()
		enc := json.NewEncoder(c.stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(rec); err != nil {
			c.lastError = "Request debug capture could not be written to stdout."
		}
	})
}

// statusWriter records only the status; the request log needs nothing else.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(n int) {
	if w.status == 0 {
		w.status = n
	}
	w.ResponseWriter.WriteHeader(n)
}
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
