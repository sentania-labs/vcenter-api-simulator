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

// scrubWords extends the query log's sensitive words with the SAML, X509,
// signature and user-name names a request body can carry.
var scrubWords = append(append([]string{}, sensitiveWords...), "security", "signature", "x509", "user")

func scrubName(n string) bool {
	n = strings.ToLower(n)
	for _, w := range scrubWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

var (
	longRun    = regexp.MustCompile(`[A-Za-z0-9+/=_-]{65,}`)
	jsonSecret = regexp.MustCompile(`(?i)("[^"\\]*(?:` + strings.Join(scrubWords, "|") + `)[^"\\]*"\s*:\s*)("(?:[^"\\]|\\.)*"|\{[^{}]*\}|\[[^\[\]]*\]|[^,}\]\s]+)`)
	formSecret = regexp.MustCompile(`(?i)((?:^|[&;\s?])[\w.\-]*(?:` + strings.Join(scrubWords, "|") + `)[\w.\-]*=)([^&;\s]*)`)
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
	switch trimmed[0] {
	case '<':
		return scrubRuns(scrubXML(data))
	case '{', '[':
		// A JSON-looking body that does not parse cannot be scrubbed
		// structurally, so none of it is logged.
		if out, ok := scrubJSON(trimmed); ok {
			return scrubRuns(out)
		}
		return placeholder("unparsed", len(data))
	}
	if bytes.IndexByte(trimmed, '=') >= 0 {
		return scrubRuns(scrubForm(string(data)))
	}
	return scrubRuns(scrubText(string(data)))
}

// scrubText is the fallback for bodies that are neither XML, JSON nor a form.
func scrubText(s string) string {
	s = jsonSecret.ReplaceAllStringFunc(s, func(m string) string {
		p := jsonSecret.FindStringSubmatch(m)
		return p[1] + `"` + placeholder("value", len(p[2])) + `"`
	})
	return formSecret.ReplaceAllStringFunc(s, func(m string) string {
		p := formSecret.FindStringSubmatch(m)
		if p[2] == "" {
			return m
		}
		return p[1] + placeholder("value", len(p[2]))
	})
}

// scrubForm scrubs a URL-encoded form field by field. A field whose decoded
// name is sensitive, or cannot be decoded, loses its whole value.
func scrubForm(s string) string {
	var out strings.Builder
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != '&' && s[i] != ';' {
			continue
		}
		field := s[start:i]
		if k, v, has := strings.Cut(field, "="); has {
			if dk, err := url.QueryUnescape(k); (err != nil || scrubName(dk) || sensitiveKey(dk)) && v != "" {
				field = k + "=" + placeholder("value", len(v))
			}
		}
		out.WriteString(field)
		if i < len(s) {
			out.WriteByte(s[i])
		}
		start = i + 1
	}
	return out.String()
}

// scrubJSON rewrites a JSON document keeping key order, replacing the whole
// value of any member whose decoded key is sensitive. ok is false when the
// body is not exactly one valid JSON value.
func scrubJSON(data []byte) (string, bool) {
	if !json.Valid(data) {
		return "", false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out bytes.Buffer
	if !jsonValue(dec, &out) {
		return "", false
	}
	return out.String(), true
}
func jsonString(out *bytes.Buffer, s string) {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out.Truncate(out.Len() - 1)
}
func jsonValue(dec *json.Decoder, out *bytes.Buffer) bool {
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	switch t := tok.(type) {
	case json.Delim:
		closer := byte(']')
		if t == '{' {
			closer = '}'
		}
		out.WriteByte(byte(t))
		for first := true; dec.More(); first = false {
			if !first {
				out.WriteByte(',')
			}
			if t == '{' {
				kt, err := dec.Token()
				key, ok := kt.(string)
				if err != nil || !ok {
					return false
				}
				jsonString(out, key)
				out.WriteByte(':')
				if scrubName(key) || sensitiveKey(key) {
					var raw json.RawMessage
					if dec.Decode(&raw) != nil {
						return false
					}
					out.WriteString(`"` + placeholder("value", len(raw)) + `"`)
					continue
				}
			}
			if !jsonValue(dec, out) {
				return false
			}
		}
		if _, err := dec.Token(); err != nil {
			return false
		}
		out.WriteByte(closer)
	case string:
		jsonString(out, t)
	case json.Number:
		out.WriteString(t.String())
	case bool:
		fmt.Fprint(out, t)
	case nil:
		out.WriteString("null")
	}
	return true
}

var attrPair = regexp.MustCompile(`([\w:.\-]+)(\s*=\s*)("[^"]*"|'[^']*')`)

func scrubAttrs(tag string) string {
	return attrPair.ReplaceAllStringFunc(tag, func(m string) string {
		p := attrPair.FindStringSubmatch(m)
		if strings.HasPrefix(strings.ToLower(p[1]), "xmlns") || !scrubName(p[1]) {
			return m
		}
		return p[1] + p[2] + `"` + placeholder("attr", len(p[3])-2) + `"`
	})
}

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
		if !ok {
			continue
		}
		contentStart := d.InputOffset()
		if tag := string(data[before:contentStart]); strings.Contains(tag, "=") {
			if clean := scrubAttrs(tag); clean != tag {
				out.Write(data[at:before])
				out.WriteString(clean)
				at = contentStart
			}
		}
		if !scrubName(start.Name.Local) {
			continue
		}
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
	return scrubName(k) || strings.Contains(k, "auth") || strings.Contains(k, "key")
}

type debugRecord struct {
	Event         string            `json:"event"`
	Time          time.Time         `json:"time"`
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

func buildDebug(r *http.Request, body []byte, bodyCut bool, status int, now time.Time) debugRecord {
	rec := debugRecord{Event: "request-capture", Time: now.UTC(), Method: r.Method,
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

// debugWrap logs license-service requests with headers and a scrubbed body to
// stdout.
func (c *Capture) debugWrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		on := c.settings.Debug && c.stdout != nil
		c.mu.Unlock()
		if !on || !isLicenseService(r.URL.Path) {
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
		rec := buildDebug(r, body, cut, rw.status, start)
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
