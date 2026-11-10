// Package registry persists the identity registry: the serial number of
// every entity the scenario has ever materialized, keyed by the entity's
// logical key ("host/dc-01/cluster-03/esx02"), plus the identity namespace
// the serials are hashed under.
//
// Identities are derived, never stored: a UUID is UUID v5 of (namespace,
// kind, serial). The registry makes serials stable across restarts and
// scenario edits: an entity keeps its serial for as long as it exists, a new
// entity gets the next serial, and a retired serial is never reused. So
// adding a cluster leaves every existing identity untouched, and removing
// an entity shifts nobody's: its serial and reserved MoRefs stay with it
// until it is retired explicitly.
//
// Changing the seed changes the namespace and therefore every identity; the
// registry refuses to load under a different seed unless the caller asks to
// regenerate, which is the one explicit way identities churn.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Version of the on-disk format.
const Version = 1

// ErrSeedChanged is returned by Open when the file was written under
// another seed and Regenerate was not requested.
var ErrSeedChanged = errors.New("registry: seed changed; start with -regenerate to mint new identities (every id changes)")

// Registry is the identity registry of one simulated vCenter.
type Registry struct {
	path  string
	dirty bool
	doc   document
	ns    uuid.UUID
}

type document struct {
	Version    int                `json:"version"`
	Seed       string             `json:"seed"`
	Namespace  string             `json:"namespace"`
	NextSerial int                `json:"nextSerial"`
	Entries    map[string]int     `json:"entries"`
	Ranges     map[int]MorefRange `json:"morefRanges,omitempty"`
	MorefHigh  int64              `json:"morefHigh,omitempty"`
}

// MorefRange reserves the vcsim counter interval used by one inventory step.
type MorefRange struct{ Start, End int64 }

// MorefRange returns the interval assigned to a serial.
func (r *Registry) MorefRange(serial int) (MorefRange, bool) {
	v, ok := r.doc.Ranges[serial]
	return v, ok
}
func (r *Registry) MorefHigh() int64 { return r.doc.MorefHigh }
func (r *Registry) SetMorefRange(serial int, v MorefRange) {
	if r.doc.Ranges == nil {
		r.doc.Ranges = make(map[int]MorefRange)
	}
	r.doc.Ranges[serial] = v
	if v.End > r.doc.MorefHigh {
		r.doc.MorefHigh = v.End
	}
	r.dirty = true
}

// Namespace derives the identity namespace of a seed.
func Namespace(seed string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("vcenter-api-simulator:"+seed))
}

// Open loads the registry at path, or starts an empty one for the seed if
// the file does not exist. A file written under another seed is an error
// unless regenerate is set. With regenerate the file is discarded whatever
// its seed: serials start over, and the identities change if the seed did
// (the namespace is the seed's, so the same seed reproduces the same ids).
func Open(path, seed string, regenerate bool) (*Registry, error) {
	r := &Registry{path: path}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &r.doc); err != nil {
			return nil, fmt.Errorf("registry: %s: %w", path, err)
		}
		if r.doc.Version != Version {
			return nil, fmt.Errorf("registry: %s: version %d, want %d", path, r.doc.Version, Version)
		}
		if r.doc.Seed != seed && !regenerate {
			return nil, fmt.Errorf("%w (file %s has seed %q, scenario has %q)", ErrSeedChanged, path, r.doc.Seed, seed)
		}
		if regenerate {
			r.doc = document{}
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	if r.doc.Entries == nil {
		r.doc = document{Version: Version, Seed: seed, Namespace: Namespace(seed).String(), NextSerial: 1, Entries: map[string]int{}}
		r.dirty = true
	}
	r.ns, err = uuid.Parse(r.doc.Namespace)
	if err != nil {
		return nil, fmt.Errorf("registry: %s: namespace: %w", path, err)
	}
	return r, nil
}

// InMemory is a registry that is never written; for tests and for running
// without a data directory.
func InMemory(seed string) *Registry {
	return &Registry{
		doc: document{Version: Version, Seed: seed, Namespace: Namespace(seed).String(), NextSerial: 1, Entries: map[string]int{}},
		ns:  Namespace(seed),
	}
}

// Seed is the seed the registry was created under.
func (r *Registry) Seed() string { return r.doc.Seed }

// Serial returns the serial of a logical key, allocating the next one on
// first sight.
func (r *Registry) Serial(key string) int {
	if n, ok := r.doc.Entries[key]; ok {
		return n
	}
	n := r.doc.NextSerial
	r.doc.NextSerial++
	r.doc.Entries[key] = n
	r.dirty = true
	return n
}

// Has reports whether a key has a serial.
func (r *Registry) Has(key string) bool {
	_, ok := r.doc.Entries[key]
	return ok
}

// Keys lists the registered keys, sorted.
func (r *Registry) Keys() []string {
	out := make([]string, 0, len(r.doc.Entries))
	for k := range r.doc.Entries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Len is the number of registered keys.
func (r *Registry) Len() int { return len(r.doc.Entries) }

// Dirty reports whether Save has something to write.
func (r *Registry) Dirty() bool { return r.dirty }

// Save writes the registry atomically (temp file and rename) if it changed.
func (r *Registry) Save() error {
	if !r.dirty || r.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(&r.doc, "", " ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return err
	}
	r.dirty = false
	return nil
}

// UUID is the identity of (kind, serial): UUID v5 under the namespace.
func (r *Registry) UUID(kind string, serial int) string {
	return uuid.NewSHA1(r.ns, []byte(fmt.Sprintf("%s:%d", kind, serial))).String()
}

// Hex returns n hex characters of identity for (kind, serial).
func (r *Registry) Hex(kind string, serial int, n int) string {
	sum := sha256.Sum256([]byte(r.ns.String() + "|" + kind + ":" + fmt.Sprint(serial)))
	return hex.EncodeToString(sum[:])[:n]
}

// Sub derives a 64-bit seed for per-entity variation from the namespace and
// a logical key, so each cluster's draws are independent of every other's.
func (r *Registry) Sub(key string) uint64 {
	sum := sha256.Sum256([]byte(r.ns.String() + "|vary|" + key))
	var v uint64
	for i := 0; i < 8; i++ {
		v = v<<8 | uint64(sum[i])
	}
	return v
}

// Clone returns a copy with the same path: allocations on it never reach
// the original, and it writes the file only if Save is called on it. The
// console previews a draft on a clone and saves the clone it applies.
func (r *Registry) Clone() *Registry {
	c := &Registry{path: r.path, dirty: true, doc: r.doc, ns: r.ns}
	c.doc.Ranges = make(map[int]MorefRange, len(r.doc.Ranges))
	for k, v := range r.doc.Ranges {
		c.doc.Ranges[k] = v
	}
	c.doc.Entries = make(map[string]int, len(r.doc.Entries))
	for k, v := range r.doc.Entries {
		c.doc.Entries[k] = v
	}
	return c
}

// Retire forgets every identity of a cluster (the cluster, its hosts,
// devices and VMs). The serials are never reused, so the next generation
// of the same logical entities gets new identities and the old ones stay
// retired. It returns how many keys it removed.
func (r *Registry) Retire(clusterKey string) int {
	n := 0
	for k := range r.doc.Entries {
		if strings.HasSuffix(k, "/"+clusterKey) || strings.Contains(k, "/"+clusterKey+"/") {
			delete(r.doc.Entries, k)
			n++
		}
	}
	if n > 0 {
		r.dirty = true
	}
	return n
}
