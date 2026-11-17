package querylog

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Defaults for the full query log. They fit well inside the chart's default
// state volume, together with the identity registry, the saved users, the
// capture settings and the certificate.
const (
	defaultMaxFileBytes = 4 << 20 // 4 MiB uncompressed per file
	defaultMaxFiles     = 4       // active file plus this many archives at most
	defaultMaxTotal     = 24 << 20
	// stateHeadroom is what the state files keep for themselves; the log's
	// total cap never grows past volume size minus this.
	stateHeadroom = 8 << 20
)

// Limits bounds the full query log on disk. A zero value means the default.
// The total cap never exceeds the space the volume actually has, so a small
// existing claim cannot be filled by the log.
type Limits struct {
	MaxFileBytes int64
	MaxFiles     int
	MaxTotal     int64
}

// LimitsFrom applies the operator's values over the defaults, then over the
// space the volume actually holds. totalBytes is the claim's capacity, or 0
// when it is unknown.
func LimitsFrom(fileBytes int64, files, total int, totalBytes int64) Limits {
	lim := Limits{MaxFileBytes: defaultMaxFileBytes, MaxFiles: defaultMaxFiles, MaxTotal: defaultMaxTotal}
	if fileBytes > 0 {
		lim.MaxFileBytes = fileBytes
	}
	if files > 0 {
		lim.MaxFiles = files
	}
	if total > 0 {
		lim.MaxTotal = int64(total)
	}
	if totalBytes > 0 {
		room := totalBytes - stateHeadroom
		if room < 1 {
			room = 1
		}
		if lim.MaxTotal <= 0 || lim.MaxTotal > room {
			lim.MaxTotal = room
		}
	}
	if lim.MaxTotal < lim.MaxFileBytes {
		// the active file alone has to fit inside the cap
		lim.MaxFileBytes = lim.MaxTotal
	}
	if lim.MaxFileBytes < 1 {
		lim.MaxFileBytes = defaultMaxFileBytes
	}
	if lim.MaxFiles < 1 {
		lim.MaxFiles = defaultMaxFiles
	}
	if lim.MaxTotal < 1 {
		lim.MaxTotal = defaultMaxTotal
	}
	return lim
}

// rot is one file of the log, newest first: its size and its generation. The
// active file has the highest generation so it sorts first.
type rot struct {
	name string
	size int64
	gen  int64
}

const activeGen = 1 << 62

// rotator writes the full log as one active file plus gzip archives:
// query-log.jsonl, then query-log.N.jsonl.gz with N counting down from the
// newest. It is used under the Capture's mutex, so writes never interleave.
type rotator struct {
	base string // <dir>/query-log, or the path as given when it has an odd suffix
	lim  Limits
	// active is the path of the file being appended to, as given.
	activePath string
	// state is the list of files, newest first, rebuilt after every write.
	state []rot
	total int64
	gen   int64
	// dropped counts the files the caps removed.
	dropped int
	// paused says the cap could not hold another file.
	paused bool
}

func newRotator(path string, lim Limits) *rotator {
	base := strings.TrimSuffix(path, ".jsonl")
	if base == "" {
		base = "query-log" // no directory: the file sits next to the other state
	}
	return &rotator{base: filepath.Clean(base), lim: lim, activePath: path}
}

// active is the file being appended to: the one the operator named.
func (r *rotator) active() string {
	if r.activePath != "" {
		return r.activePath
	}
	return r.base + ".jsonl"
}

// scan lists the log's files, newest first, and their combined size.
func (r *rotator) scan() {
	r.state, r.total = nil, 0
	dir := filepath.Dir(r.active())
	if info, err := os.Stat(r.active()); err == nil {
		r.state = append(r.state, rot{name: r.active(), size: info.Size(), gen: activeGen})
		r.total += info.Size()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := filepath.Base(r.base) + "."
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, ".jsonl.gz") {
			continue
		}
		k, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(n, prefix), ".jsonl.gz"))
		if err != nil || k < 1 || filepath.Join(dir, n) == r.active() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		r.state = append(r.state, rot{name: filepath.Join(dir, n), size: info.Size(), gen: int64(k)})
		r.total += info.Size()
	}
	sort.Slice(r.state, func(i, j int) bool { return r.state[i].gen > r.state[j].gen })
}

// put writes one encoded line and then applies the caps.
func (r *rotator) put(line []byte) error {
	f, err := os.OpenFile(r.active(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	n, werr := f.Write(line)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil || n != len(line) {
		if werr != nil {
			return werr
		}
		return io.ErrShortWrite
	}
	r.scan()
	return r.enforce()
}

// enforce rotates when the active file is full, then keeps the number of files
// and the total inside the caps by removing the oldest first.
func (r *rotator) enforce() error {
	// 1. the active file is full: make it the newest archive
	if len(r.state) > 0 && r.state[0].gen == activeGen && r.state[0].size >= r.lim.MaxFileBytes {
		if err := r.compress(r.state[0].name); err != nil {
			return err
		}
	}
	// 2. not too many files
	for len(r.state) > r.lim.MaxFiles {
		last := r.state[len(r.state)-1]
		if err := removeFile(last.name); err != nil {
			return err
		}
		r.total -= last.size
		if r.total < 0 {
			r.total = 0
		}
		r.state = r.state[:len(r.state)-1]
		r.dropped++
	}
	// 3. the total stays inside the cap, oldest first. The active file is
	// last to go, so the newest lines are what survives.
	for r.total > r.lim.MaxTotal && len(r.state) > 0 {
		last := r.state[len(r.state)-1]
		if err := removeFile(last.name); err != nil {
			return err
		}
		r.total -= last.size
		if r.total < 0 {
			r.total = 0
		}
		r.state = r.state[:len(r.state)-1]
		r.dropped++
	}
	// 4. the active file alone is over the cap. A second file now starts the
	// log again from empty, so nothing is lost that the cap can hold.
	if len(r.state) > 0 && r.state[0].size > r.lim.MaxTotal {
		if err := trim(r.state[0].name, r.lim.MaxTotal); err != nil {
			return err
		}
		r.scan()
		r.dropped++
	}
	if r.total > r.lim.MaxTotal {
		r.paused = true
	}
	return nil
}

// open makes sure the active file exists, so a later append starts from a
// known state. The highest generation already on disk becomes the base the
// next archive numbers from.
func (r *rotator) open() error {
	r.scan()
	for _, f := range r.state {
		if f.gen != activeGen && f.gen > r.gen {
			r.gen = f.gen
		}
	}
	f, err := os.OpenFile(r.active(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	return f.Close()
}

// compress turns the active file into the newest archive. The archive is
// complete before the active file is emptied, so no line can be lost between
// the two.
func (r *rotator) compress(active string) error {
	in, err := os.Open(active)
	if err != nil {
		return err
	}
	defer in.Close()
	name := r.base + "." + strconv.FormatInt(r.gen+1, 10) + ".jsonl.gz"
	out, err := os.Create(name)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	if _, err = io.Copy(zw, in); err == nil {
		err = zw.Close()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(name)
		return err
	}
	if err = os.Truncate(active, 0); err != nil {
		return err
	}
	r.gen++
	r.scan()
	return nil
}

func removeFile(name string) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// trim keeps only the last max bytes of a file, starting at a line boundary.
func trim(name string, max int64) error {
	b, err := os.ReadFile(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if int64(len(b)) <= max {
		return nil
	}
	tail := b[int64(len(b))-max:]
	if i := bytes.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	return os.WriteFile(name, tail, 0600)
}

// readAll decodes every record the log holds, newest file first, each file's
// lines as they were written. used is what the log takes on disk.
func (r *rotator) readAll() (out []record, used int64) {
	r.scan()
	for _, f := range r.state {
		lines, err := readAny(f.name)
		if err != nil {
			continue
		}
		for _, line := range lines {
			var rec record
			if json.Unmarshal(line, &rec) != nil {
				continue
			}
			out = append(out, rec)
		}
	}
	return out, r.total
}

// readChronological returns the log's lines as they were written, oldest
// first: the archives from the oldest up, then the active file.
func (r *rotator) readChronological() [][]byte {
	r.scan()
	var out [][]byte
	for i := len(r.state) - 1; i >= 0; i-- {
		lines, err := readAny(r.state[i].name)
		if err != nil {
			continue
		}
		out = append(out, lines...)
	}
	return out
}

// readAny returns a file's non-empty lines, transparently for gzip.
func readAny(name string) ([][]byte, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(name, ".gz") {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		if b, err = io.ReadAll(zr); err != nil {
			return nil, err
		}
	}
	var out [][]byte
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			out = append(out, line)
		}
	}
	return out, nil
}
