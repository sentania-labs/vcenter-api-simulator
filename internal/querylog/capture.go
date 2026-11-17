package querylog

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Settings are the operator's choices; a saved choice overrides startup defaults.
type Settings struct {
	Full       bool `json:"full"`
	Unanswered bool `json:"unanswered"`
	Stdout     bool `json:"stdout"`
	// Debug logs license-service requests, headers and a scrubbed
	// body, to stdout. On by default.
	Debug bool `json:"debug"`
}
type State struct {
	Settings       Settings `json:"settings"`
	FullHeld       int      `json:"fullHeld"`
	UnansweredHeld int      `json:"unansweredHeld"`
	// UnansweredKinds counts the different requests among the unanswered
	// ones (by path and operation).
	UnansweredKinds int `json:"unansweredKinds"`
	// FullSince is when full capture was last switched on, if it is on.
	FullSince *time.Time `json:"fullSince,omitempty"`
	// Path is the full log's file; the console does not show it.
	Path      string `json:"-"`
	FileBytes int64  `json:"fileBytes"`
	// TotalBytes is what the whole log takes (the active file and every
	// archive), and MaxBytes the cap it is kept inside.
	TotalBytes int64 `json:"totalBytes"`
	MaxBytes   int64 `json:"maxBytes"`
	// Files counts the log's files on disk.
	Files int    `json:"files"`
	Error string `json:"error,omitempty"`
}

const capacity = 1000

type Capture struct {
	mu                 sync.Mutex
	settings           Settings
	settingsPath, path string
	stdout             io.Writer
	full, unanswered   []record
	fullSince          time.Time
	lastError          string
	rot                *rotator
}

// NewCapture loads persisted settings or uses the supplied startup defaults.
// totalBytes is the capacity of the volume the log lives on (0 when unknown);
// it limits the log's total cap so the state files always keep their room.
// A zero in lim means the default for that one cap.
func NewCapture(dir, path string, defaults Settings, stdout io.Writer, totalBytes int64, limits Limits) (*Capture, error) {
	if path == "" {
		path = filepath.Join(dir, "query-log.jsonl")
	}
	c := &Capture{settings: defaults, settingsPath: filepath.Join(dir, "capture-settings.json"), path: path, stdout: stdout,
		rot: newRotator(path, LimitsFrom(limits.MaxFileBytes, limits.MaxFiles, int(limits.MaxTotal), totalBytes))}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(c.settingsPath)
	if err == nil {
		if err = json.Unmarshal(data, &c.settings); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Fail at startup rather than silently dropping a requested persistent log.
	if c.settings.Full {
		if err := c.rot.open(); err != nil {
			return nil, err
		}
		c.fullSince = time.Now()
	}
	return c, nil
}
func (c *Capture) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total int64
	files := 0
	if c.rot != nil {
		c.rot.scan()
		total, files = c.rot.total, len(c.rot.state)
	}
	kinds := map[string]bool{}
	for _, r := range c.unanswered {
		kinds[r.Method+" "+r.Path+" "+r.Operation] = true
	}
	st := State{Settings: c.settings, FullHeld: len(c.full), UnansweredHeld: len(c.unanswered), UnansweredKinds: len(kinds), Path: c.path,
		TotalBytes: total, MaxBytes: c.rot.lim.MaxTotal, Files: files, Error: c.lastError}
	if c.settings.Full {
		since := c.fullSince
		st.FullSince = &since
	}
	return st
}
func (c *Capture) Set(s Settings) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.Full {
		if err := c.rot.open(); err != nil {
			return err
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if !s.Full && c.settings.Full {
		// Turning full capture off is the one switch that discards: what it
		// captured goes with it. Unanswered requests are kept until deleted.
		if err = os.Truncate(c.path, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		c.full = nil
	}
	f, err := os.CreateTemp(filepath.Dir(c.settingsPath), ".capture-settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), c.settingsPath); err != nil {
		return err
	}
	if s.Full && !c.settings.Full {
		c.fullSince = time.Now()
	}
	c.settings = s
	return nil
}
func appendBounded(v []record, r record) []record {
	if len(v) == capacity {
		copy(v, v[1:])
		v = v[:capacity-1]
	}
	return append(v, r)
}
func (c *Capture) save(r record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stored := r
	if len(stored.Path) > 2048 {
		stored.Path = strings.Clone(stored.Path[:2048])
		stored.PathTruncated = true
	}
	if len(stored.Operation) > 256 {
		stored.Operation = strings.Clone(stored.Operation[:256])
	}
	if len(stored.ObjectType) > 256 {
		stored.ObjectType = strings.Clone(stored.ObjectType[:256])
	}
	if len(stored.Request) > maxRecordedBody {
		stored.Request = strings.Clone(stored.Request[:maxRecordedBody])
		stored.RequestTruncated = true
	}
	if len(stored.Properties) > 128 {
		stored.Properties = stored.Properties[:128]
		stored.PropertiesTruncated = true
	}
	if len(stored.Properties) > 0 {
		stored.Properties = append([]string(nil), stored.Properties...)
		for i, value := range stored.Properties {
			if len(value) > 256 {
				stored.Properties[i] = strings.Clone(value[:256])
				stored.PropertiesTruncated = true
			}
		}
	}
	if c.settings.Full {
		b, err := json.Marshal(r)
		if err == nil && int64(len(b)) > c.rot.lim.MaxTotal {
			// the operator gave a cap a full record cannot fit into
			if c.lastError == "" {
				c.lastError = "The log's size limit is smaller than one request. The full log was not written."
			}
		} else if err == nil {
			err = c.rot.put(append(b, '\n'))
		}
		if err != nil {
			c.lastError = "The full request log could not be written. Check its storage."
		} else {
			c.lastError = ""
		}
		c.full = appendBounded(c.full, stored)
	}
	// stdout is an install-time choice and does not depend on the console's switch
	if c.settings.Unanswered && r.Unanswered {
		c.unanswered = appendBounded(c.unanswered, stored)
		if c.settings.Stdout && c.stdout != nil {
			if err := json.NewEncoder(c.stdout).Encode(r); err != nil {
				c.lastError = "Unanswered requests could not be written to stdout."
			}
		}
	}
}
func (c *Capture) Wrap(next http.Handler) http.Handler {
	return c.debugWrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serve(w, r, next, c.capturing, c.save) }))
}
func (c *Capture) capturing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings.Full || c.settings.Unanswered
}
func (c *Capture) Records(unanswered bool) []record {
	c.mu.Lock()
	defer c.mu.Unlock()
	if unanswered {
		return append([]record{}, c.unanswered...)
	}
	return append([]record{}, c.full...)
}

// WriteJSONL downloads the retained full log or the bounded unanswered list.
// The full log is read across its rotated and gzipped files, oldest line first.
func (c *Capture) WriteJSONL(w io.Writer, unanswered bool) error {
	if unanswered {
		records := c.Records(true)
		enc := json.NewEncoder(w)
		for _, record := range records {
			if err := enc.Encode(record); err != nil {
				return err
			}
		}
		return nil
	}
	c.mu.Lock()
	lines := c.rot.readChronological()
	c.mu.Unlock()
	for _, line := range lines {
		if _, err := w.Write(line); err != nil {
			return err
		}
		if _, err := w.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	return nil
}

func (c *Capture) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.Truncate(c.path, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, f := range c.rot.state {
		if f.name != c.path {
			if err := removeFile(f.name); err != nil {
				return err
			}
		}
	}
	c.rot.scan()
	c.full = nil
	c.unanswered = nil
	return nil
}
