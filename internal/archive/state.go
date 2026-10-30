// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// State is machine-maintained bookkeeping in .git-archive/state.json. It
// is never required for correctness of the repositories themselves: if it
// is lost, the next sync adopts existing clones again.
type State struct {
	Version int                     `json:"version"`
	Repos   map[string]*RepoState   `json:"repos"`
	Sources map[string]*SourceState `json:"sources"`
}

// RepoState tracks one archived repository, keyed by its ID.
type RepoState struct {
	Source        string    `json:"source"`
	URL           string    `json:"url"`
	Wiki          bool      `json:"wiki,omitempty"`
	ForgeID       string    `json:"forgeId,omitempty"` // stable numeric id; detects renames
	DefaultBranch string    `json:"defaultBranch,omitempty"`
	Fork          bool      `json:"fork,omitempty"`
	Archived      bool      `json:"archived,omitempty"`
	Private       bool      `json:"private,omitempty"`
	Size          int64     `json:"size,omitempty"`
	FirstSeen     time.Time `json:"firstSeen,omitzero"`
	LastSeen      time.Time `json:"lastSeen,omitzero"` // last time the forge listed it
	Gone          time.Time `json:"gone,omitzero"`     // missing upstream since
	Absent        time.Time `json:"absent,omitzero"`   // optional repo (wiki) not found upstream, checked at
	Cloned        time.Time `json:"cloned,omitzero"`
	LastSync      time.Time `json:"lastSync,omitzero"` // last successful clone or update
	LastAttempt   time.Time `json:"lastAttempt,omitzero"`
	LastError     string    `json:"lastError,omitempty"`
	SyncSeconds   float64   `json:"syncSeconds,omitempty"` // duration of the last sync, for scheduling
	LastVerify    time.Time `json:"lastVerify,omitzero"`
	VerifyError   string    `json:"verifyError,omitempty"`
	Preserved     int       `json:"preserved,omitempty"` // refs saved under refs/archive/ so far
}

// SourceState tracks the last listing of a source.
type SourceState struct {
	LastList  time.Time `json:"lastList,omitzero"`
	ListError string    `json:"listError,omitempty"`
}

func newState() *State {
	return &State{Version: 1, Repos: map[string]*RepoState{}, Sources: map[string]*SourceState{}}
}

// loadState reads the state file. A missing file yields empty state; a
// corrupt one is moved aside (and reported) rather than aborting, because
// the state can be rebuilt from the repositories on disk.
func loadState(path string) (*State, string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return newState(), "", nil
	}
	if err != nil {
		return nil, "", err
	}
	s := newState()
	if err := json.Unmarshal(data, s); err != nil || s.Version != 1 {
		aside := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		if rerr := os.Rename(path, aside); rerr != nil {
			return nil, "", fmt.Errorf("state file %s is unreadable and cannot be moved aside: %v", path, rerr)
		}
		return newState(), fmt.Sprintf("state file was unreadable and has been moved to %s; it will be rebuilt", aside), nil
	}
	if s.Repos == nil {
		s.Repos = map[string]*RepoState{}
	}
	if s.Sources == nil {
		s.Sources = map[string]*SourceState{}
	}
	for id, r := range s.Repos {
		if r == nil {
			delete(s.Repos, id)
		}
	}
	return s, "", nil
}

// save writes the state atomically: a crash leaves either the old or the
// new file, never a truncated one.
func (s *State) save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o644)
}

// WriteFileAtomic writes data to a temporary file in the same directory,
// syncs it and renames it over path.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// Repo returns the state of id, creating it if needed.
func (s *State) Repo(id string) *RepoState {
	r := s.Repos[id]
	if r == nil {
		r = &RepoState{}
		s.Repos[id] = r
	}
	return r
}

// Source returns the state of a source, creating it if needed.
func (s *State) Source(name string) *SourceState {
	src := s.Sources[name]
	if src == nil {
		src = &SourceState{}
		s.Sources[name] = src
	}
	return src
}
