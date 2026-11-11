// Package credentials manages runtime vCenter users without exposing passwords.
package credentials

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	Name string `json:"name"`
	Role string `json:"role"`
}
type entry struct {
	Hash  string `json:"hash,omitempty"`
	Role  string `json:"role"`
	plain string
}
type Snapshot struct {
	AnyLogin bool `json:"anyLogin"`
	users    map[string]entry
}
type diskState struct {
	Source   string           `json:"source"`
	AnyLogin bool             `json:"anyLogin"`
	Users    map[string]entry `json:"users"`
}
type Store struct {
	mu           sync.RWMutex
	path, source string
	current      Snapshot
}

func New(dir, source string, baseline map[string]string, any bool, readonly []string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, "vcenter-users.json"), source: source, current: Snapshot{AnyLogin: any, users: map[string]entry{}}}
	for name, pw := range baseline {
		role := "administrator"
		for _, ro := range readonly {
			if ro == name {
				role = "read-only"
			}
		}
		s.current.users[name] = entry{plain: pw, Role: role}
	}
	b, err := os.ReadFile(s.path)
	if err == nil {
		var state diskState
		if err = json.Unmarshal(b, &state); err != nil {
			return nil, fmt.Errorf("credentials: invalid saved users: %w", err)
		}
		if state.Source == source {
			if state.Users == nil {
				return nil, errors.New("credentials: invalid saved users")
			}
			s.current = Snapshot{AnyLogin: state.AnyLogin, users: state.Users}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return s, nil
}
func (s *Store) Snapshot() Snapshot { s.mu.RLock(); defer s.mu.RUnlock(); return clone(s.current) }
func clone(in Snapshot) Snapshot {
	out := Snapshot{AnyLogin: in.AnyLogin, users: make(map[string]entry, len(in.users))}
	for n, e := range in.users {
		out.users[n] = e
	}
	return out
}
func (s *Store) Authenticate(name, password string) bool {
	if name == "" || password == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.current.AnyLogin {
		return true
	}
	e, ok := s.current.users[name]
	if !ok {
		return false
	}
	if e.Hash != "" {
		return bcrypt.CompareHashAndPassword([]byte(e.Hash), []byte(password)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(e.plain), []byte(password)) == 1
}
func (s *Store) Role(name string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.current.AnyLogin {
		return "administrator"
	}
	return s.current.users[name].Role
}
func (s *Store) Users() []User    { return s.Snapshot().Users() }
func (s Snapshot) Copy() Snapshot { return clone(s) }
func (s Snapshot) Users() []User {
	out := make([]User, 0, len(s.users))
	for n, e := range s.users {
		out = append(out, User{Name: n, Role: e.Role})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (s *Snapshot) Put(name, password, role string) error {
	if name == "" || password == "" {
		return errors.New("user name and password are required")
	}
	if role != "administrator" && role != "read-only" {
		return errors.New("role must be administrator or read-only")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return errors.New("password must be at most 72 bytes")
	}
	s.users[name] = entry{Hash: string(hash), Role: role}
	return nil
}
func (s *Snapshot) Delete(name string) error {
	if _, ok := s.users[name]; !ok {
		return errors.New("user not found")
	}
	if !s.AnyLogin && len(s.users) == 1 {
		return errors.New("Keep at least one vCenter user, or allow any username and password first.")
	}
	delete(s.users, name)
	return nil
}

// Commit persists before switching the active credentials; only hashes reach disk.
func (s *Store) Commit(snapshot Snapshot) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(snapshot)
	if !next.AnyLogin && len(next.users) == 0 {
		return nil, errors.New("Keep at least one vCenter user, or allow any username and password first.")
	}
	changed := []string{}
	if s.current.AnyLogin != next.AnyLogin {
		changed = append(changed, "*")
	}
	for name, old := range s.current.users {
		e, ok := next.users[name]
		if !ok || e != old || s.current.AnyLogin != next.AnyLogin {
			changed = append(changed, name)
		}
	}
	for name, e := range next.users {
		if e.Hash == "" {
			h, err := bcrypt.GenerateFromPassword([]byte(e.plain), bcrypt.DefaultCost)
			if err != nil {
				return nil, errors.New("could not protect a password")
			}
			e.Hash = string(h)
			e.plain = ""
			next.users[name] = e
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return nil, err
	}
	b, err := json.Marshal(diskState{Source: s.source, AnyLogin: next.AnyLogin, Users: next.users})
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".vcenter-users-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return nil, err
	}
	s.current = next
	return changed, nil
}

// Change describes a draft without exposing password material.
type Change struct {
	Name, Before, After, Effect string
	EndsSessions                bool
}

func (s *Store) Diff(next Snapshot) []Change {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Change
	if s.current.AnyLogin != next.AnyLogin {
		before, after := "Specific users", "Any non-empty login"
		if !next.AnyLogin {
			before, after = after, before
		}
		out = append(out, Change{Name: "Login policy", Before: before, After: after, Effect: "existing sessions end", EndsSessions: true})
	}
	names := map[string]bool{}
	for name := range s.current.users {
		names[name] = true
	}
	for name := range next.users {
		names[name] = true
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		old, was := s.current.users[name]
		updated, exists := next.users[name]
		switch {
		case !was:
			out = append(out, Change{Name: name, Before: "—", After: updated.Role, Effect: "user added"})
		case !exists:
			out = append(out, Change{Name: name, Before: old.Role, After: "removed", Effect: "user is signed out", EndsSessions: true})
		case old != updated:
			out = append(out, Change{Name: name, Before: old.Role, After: updated.Role, Effect: "credentials updated; user is signed out", EndsSessions: true})
		}
	}
	return out
}
