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
)

// Settings are the operator's choices; a saved choice overrides startup defaults.
type Settings struct {
	Full       bool `json:"full"`
	Unanswered bool `json:"unanswered"`
	Stdout     bool `json:"stdout"`
}
type State struct {
	Settings       Settings `json:"settings"`
	FullHeld       int      `json:"fullHeld"`
	UnansweredHeld int      `json:"unansweredHeld"`
	Path           string   `json:"path"`
	FileBytes      int64    `json:"fileBytes"`
	Error          string   `json:"error,omitempty"`
}

const capacity = 1000

type Capture struct {
	mu                 sync.Mutex
	settings           Settings
	settingsPath, path string
	stdout             io.Writer
	full, unanswered   []record
	lastError          string
}

// NewCapture loads persisted settings or uses the supplied startup defaults.
func NewCapture(dir, path string, defaults Settings, stdout io.Writer) (*Capture, error) {
	if path == "" {
		path = filepath.Join(dir, "query-log.jsonl")
	}
	c := &Capture{settings: defaults, settingsPath: filepath.Join(dir, "capture-settings.json"), path: path, stdout: stdout}
	data, err := os.ReadFile(c.settingsPath)
	if err == nil {
		if err = json.Unmarshal(data, &c.settings); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Fail at startup rather than silently dropping a requested persistent log.
	if c.settings.Full {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
	}
	return c, nil
}
func (c *Capture) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	var size int64
	if info, err := os.Stat(c.path); err == nil {
		size = info.Size()
	}
	return State{Settings: c.settings, FullHeld: len(c.full), UnansweredHeld: len(c.unanswered), Path: c.path, FileBytes: size, Error: c.lastError}
}
func (c *Capture) Set(s Settings) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.Full {
		f, err := os.OpenFile(c.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
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
		c.full = appendBounded(c.full, stored)
		f, err := os.OpenFile(c.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err == nil {
			err = json.NewEncoder(f).Encode(r)
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
		}
		if err != nil {
			c.lastError = "The full request log could not be written. Check its storage."
		} else {
			c.lastError = ""
		}
	}
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serve(w, r, next, c.save) })
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
	f, err := os.Open(c.path)
	c.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func (c *Capture) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.Truncate(c.path, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	c.full = nil
	c.unanswered = nil
	return nil
}
