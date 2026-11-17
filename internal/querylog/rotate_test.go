package querylog

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func soap(n int) string {
	return `<Envelope><Body><Fetch><_this type="HostSystem">host-` + string(rune('0'+n)) +
		`</_this><prop>config.product</prop></Fetch></Body></Envelope>`
}

// One file rotates into a gzip archive once it passes its own limit, and the
// archive is a complete copy: no line is torn or lost.
func TestRotationCompressesWithoutTearing(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCapture(dir, "", Settings{Full: true}, nil, 0, Limits{MaxFileBytes: 3000, MaxFiles: 3, MaxTotal: 30000})
	if err != nil {
		t.Fatal(err)
	}
	h := c.Wrap(http.NotFoundHandler())
	for i := 0; i < 30; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(soap(i%9))))
	}
	st := c.State()
	if st.Files < 2 {
		t.Fatalf("no rotation happened: %+v", st)
	}
	if st.TotalBytes > st.MaxBytes {
		t.Fatalf("total %d over cap %d", st.TotalBytes, st.MaxBytes)
	}
	var gz int
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl.gz") {
			gz++
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			zr, err := gzip.NewReader(bytes.NewReader(b))
			if err != nil {
				t.Fatalf("%s: %v", e.Name(), err)
			}
			raw, err := io.ReadAll(zr)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				if !json.Valid([]byte(line)) {
					t.Fatalf("torn line in %s: %q", e.Name(), line)
				}
			}
		}
	}
	if gz == 0 {
		t.Fatal("no gzip archive was written")
	}
	// the archives are as restrictive as the active file
	for _, name := range archives(t, dir) {
		st, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s has mode %o, want 600 like the active log", filepath.Base(name), perm)
		}
	}
}

// The total stays inside the cap even after many files, and the oldest are the
// ones gone.
func TestTotalCapShedsOldestFirst(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCapture(dir, "", Settings{Full: true}, nil, 0, Limits{MaxFileBytes: 200, MaxFiles: 5, MaxTotal: 900})
	if err != nil {
		t.Fatal(err)
	}
	h := c.Wrap(http.NotFoundHandler())
	for i := 0; i < 60; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(soap(i%9))))
	}
	st := c.State()
	if st.TotalBytes > st.MaxBytes {
		t.Fatalf("total %d over cap %d", st.TotalBytes, st.MaxBytes)
	}
	if st.Files > 5 {
		t.Fatalf("%d files, want at most 5", st.Files)
	}
}

// The volume's own capacity limits the cap, so a small existing claim is not
// filled by the log and the state files keep their room.
func TestVolumeCapacityLimitsTheCap(t *testing.T) {
	lim := LimitsFrom(0, 0, 0, 100<<20)
	if lim.MaxTotal > 100<<20-stateHeadroom || lim.MaxTotal <= 0 {
		t.Fatalf("cap %d not limited by a 100Mi volume", lim.MaxTotal)
	}
	if got := LimitsFrom(0, 0, 0, 0); got.MaxTotal != defaultMaxTotal {
		t.Fatalf("unknown volume: cap %d, want the default %d", got.MaxTotal, defaultMaxTotal)
	}
	if got := LimitsFrom(1<<20, 2, 1<<20, 0); got.MaxFileBytes != 1<<20 || got.MaxFiles != 2 {
		t.Fatalf("operator values ignored: %+v", got)
	}
}

// With a small cap the log sheds its oldest lines, but the state files still
// get written and requests are still answered.
func TestSmallCapKeepsStateFilesWritable(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCapture(dir, filepath.Join(dir, "query-log.jsonl"), Settings{Full: true, Unanswered: true}, nil, 0,
		Limits{MaxFileBytes: 8192, MaxFiles: 2, MaxTotal: 24576})
	if err != nil {
		t.Fatal(err)
	}
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<Envelope><Body><FetchResponse><returnval>x</returnval></FetchResponse></Body></Envelope>`))
	}))
	body := `<Envelope><Body><Fetch><_this type="HostSystem">host-1</_this><prop>` + strings.Repeat("y", 900) + `</prop></Fetch></Body></Envelope>`
	for i := 0; i < 40; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(body)))
	}
	st := c.State()
	if st.TotalBytes > st.MaxBytes {
		t.Fatalf("total %d over cap %d", st.TotalBytes, st.MaxBytes)
	}
	// the state files are intact and small
	for _, name := range []string{"query-log.jsonl"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if int64(len(b)) > st.MaxBytes {
			t.Fatalf("%s is %d bytes, over the cap", name, len(b))
		}
	}
	if err := c.Set(Settings{Full: true, Unanswered: true}); err != nil {
		t.Fatalf("state write failed under a small cap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "capture-settings.json")); err != nil {
		t.Fatalf("settings not written: %v", err)
	}
	// requests keep being answered
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/sdk", strings.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "returnval") {
		t.Fatalf("request not answered: %d %s", rec.Code, rec.Body.String())
	}
}

// Reading the log works across rotated and compressed files: every record it
// still holds comes back, oldest line first.
func TestWriteJSONLReadsAcrossRotatedFiles(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCapture(dir, "", Settings{Full: true}, nil, 0, Limits{MaxFileBytes: 3000, MaxFiles: 10, MaxTotal: 100000})
	if err != nil {
		t.Fatal(err)
	}
	h := c.Wrap(http.NotFoundHandler())
	for i := 0; i < 25; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(soap(i%9))))
	}
	var out bytes.Buffer
	if err := c.WriteJSONL(&out, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 25 {
		t.Fatalf("expected all 25 requests across the files, got %d", len(lines))
	}
	if archives, _ := filepath.Glob(filepath.Join(dir, "query-log.*.jsonl.gz")); len(archives) == 0 {
		t.Fatal("expected at least one compressed archive")
	}
	st := c.State()
	// every line is a record, and the newest line is the last request
	var last record
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("last line is not a record: %v", err)
	}
	if last.Operation != "Fetch" {
		t.Fatalf("last record: %+v", last)
	}
	// the lines run in the order they were written, across the rotated files
	var prev string
	for _, line := range lines {
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not a record: %v", len(out.String()), err)
		}
		if s := rec.Time.Format("2006-01-02T15:04:05.000000000"); prev != "" && s < prev {
			t.Fatalf("lines out of order: %s then %s", prev, s)
		} else {
			prev = s
		}
	}
	if int64(sumSizes(dir)) > st.MaxBytes {
		t.Fatalf("on-disk total over cap")
	}
}

// A cap a whole record cannot fit into is reported through State.Error, and it
// stays reported: the next request's reset must not clear it away.
func TestCapTooSmallReportsError(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCapture(dir, filepath.Join(dir, "query-log.jsonl"), Settings{Full: true}, nil, 0,
		Limits{MaxFileBytes: 512, MaxFiles: 2, MaxTotal: 500})
	if err != nil {
		t.Fatal(err)
	}
	h := c.Wrap(http.NotFoundHandler())
	body := `<Envelope><Body><Fetch><_this type="HostSystem">host-1</_this><prop>` +
		strings.Repeat("y", 900) + `</prop></Fetch></Body></Envelope>`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(body)))
	if st := c.State(); !strings.Contains(st.Error, "smaller than one request") {
		t.Fatalf("cap-too-small not reported: %q", st.Error)
	}
	// a following small request is written, and the cap error is what stays
	if err := c.rot.put([]byte(`{"time":"2026-01-01T00:00:00Z","path":"/sdk"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if st := c.State(); !strings.Contains(st.Error, "smaller than one request") {
		t.Fatalf("the cap error did not survive a later write: %q", st.Error)
	}
	if st := c.State(); st.TotalBytes == 0 {
		t.Fatal("the following request did not reach the log")
	}
	// the log file holds nothing but the cap is still respected
	if b, err := os.ReadFile(filepath.Join(dir, "query-log.jsonl")); err == nil && int64(len(b)) > 500 {
		t.Fatalf("log %d bytes over the 500 cap", len(b))
	}
}

// Concurrent requests all land, and the log stays readable.
func TestConcurrentWritesStayInsideCap(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCapture(dir, "", Settings{Full: true, Unanswered: true}, nil, 0, Limits{MaxFileBytes: 3000, MaxFiles: 3, MaxTotal: 12000})
	if err != nil {
		t.Fatal(err)
	}
	h := c.Wrap(http.NotFoundHandler())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for k := 0; k < 20; k++ {
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(soap(i%9))))
			}
		}(i)
	}
	wg.Wait()
	st := c.State()
	if st.TotalBytes > st.MaxBytes {
		t.Fatalf("total %d over cap %d", st.TotalBytes, st.MaxBytes)
	}
	var out bytes.Buffer
	if err := c.WriteJSONL(&out, false); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("torn line: %q", line)
		}
	}
}

// Clear empties the whole log, archives included.
func TestClearRemovesArchives(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCapture(dir, "", Settings{Full: true}, nil, 0, Limits{MaxFileBytes: 3000, MaxFiles: 3, MaxTotal: 12000})
	if err != nil {
		t.Fatal(err)
	}
	h := c.Wrap(http.NotFoundHandler())
	for i := 0; i < 20; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(soap(i%9))))
	}
	if err := c.Clear(); err != nil {
		t.Fatal(err)
	}
	st := c.State()
	if st.TotalBytes != 0 || st.Files != 1 {
		t.Fatalf("log not cleared: %+v", st)
	}
	empty, err := os.ReadFile(filepath.Join(dir, "query-log.jsonl"))
	if err != nil || len(empty) != 0 {
		t.Fatalf("active file not emptied: %q %v", empty, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".gz") {
			t.Fatalf("archive %s survived the clear", e.Name())
		}
	}
}

func sumSizes(dir string) (n int) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			n += int(info.Size())
		}
	}
	return n
}

// A log an earlier, unbounded run left over the caps is cut down when the
// capture opens it, not only after the next write.
func TestOpenTrimsALogAlreadyOverTheCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "query-log.jsonl")
	line := []byte(`{"time":"2026-01-01T00:00:00Z","method":"POST","path":"/sdk"}` + "\n")
	old := bytes.Repeat(line, 4000)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := NewCapture(dir, "", Settings{Full: true}, nil, 0, Limits{MaxFileBytes: 20000, MaxFiles: 3, MaxTotal: 40000})
	if err != nil {
		t.Fatal(err)
	}
	st := c.State()
	if st.TotalBytes > st.MaxBytes {
		t.Fatalf("log of %d bytes left over the %d cap after opening", st.TotalBytes, st.MaxBytes)
	}
	if st.TotalBytes == 0 {
		t.Fatal("the log was emptied instead of kept inside the cap")
	}
}

// A legacy log much larger than the reader's buffer comes back complete and in
// order through a download, and the decompressed file is not held as one block.
func TestDownloadStreamsALegacyLogLargerThanTheBuffer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "query-log.jsonl")
	const lines = 5000
	line := []byte(`{"time":"2026-01-01T00:00:00Z","method":"POST","path":"/sdk","prop":"` + strings.Repeat("z", 200) + `"}` + "\n")
	if err := os.WriteFile(path, bytes.Repeat(line, lines), 0o600); err != nil {
		t.Fatal(err)
	}
	// 1 MiB of legacy log, sixteen times the reader's 64 KiB buffer
	if n, err := os.Stat(path); err != nil || n.Size() < 64<<10 {
		t.Fatalf("legacy log is not larger than the buffer: %+v %v", n, err)
	}
	c, err := NewCapture(dir, "", Settings{Full: true}, nil, 0,
		Limits{MaxFileBytes: 1 << 20, MaxFiles: 4, MaxTotal: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "query-log.1.jsonl.gz"))
	if err != nil || info.Size() == 0 {
		t.Fatalf("the legacy log was not compressed into an archive on start: %v", err)
	}
	var out bytes.Buffer
	if err := c.WriteJSONL(&out, false); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(got) != lines {
		t.Fatalf("streamed %d lines, want %d", len(got), lines)
	}
	for i, l := range got {
		if !json.Valid([]byte(l)) {
			t.Fatalf("line %d is not a record: %.80s", i+1, l)
		}
	}
	// the newest line is still the last one after a later request
	h := c.Wrap(http.NotFoundHandler())
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/sdk", strings.NewReader(soap(3))))
	out.Reset()
	if err := c.WriteJSONL(&out, false); err != nil {
		t.Fatal(err)
	}
	got = strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(got) != lines+1 {
		t.Fatalf("after one more request: %d lines, want %d", len(got), lines+1)
	}
	var last record
	if err := json.Unmarshal([]byte(got[len(got)-1]), &last); err != nil {
		t.Fatalf("last line is not a record: %v", err)
	}
	if last.Path != "/sdk" {
		t.Fatalf("last record: %+v", last)
	}
}

func archives(t *testing.T, dir string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "query-log.*.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	return names
}
