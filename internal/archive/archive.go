// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

// Package archive models an archive directory: its configuration, its
// bookkeeping state, its lock, and the mapping from repository IDs to
// locations on disk.
//
// Layout of an archive:
//
//	<root>/.git-archive/config        sources, exclusions, settings
//	<root>/.git-archive/state.json    bookkeeping, rebuilt if lost
//	<root>/.git-archive/lock          held while a command modifies the archive
//	<root>/.git-archive/metadata/...  API metadata snapshots (JSON)
//	<root>/<host>/<owner>/<repo>      repositories (".git" suffix when bare)
package archive

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sylirre/git-archive/internal/gitx"
	"github.com/sylirre/git-archive/internal/safe"
)

// MetaDir is the name of the archive's metadata directory.
const MetaDir = ".git-archive"

// PartialSuffix marks a clone in progress. '~' cannot occur in validated
// names, so the suffix never collides with a real repository.
const PartialSuffix = "~partial"

// Archive is an opened archive directory.
type Archive struct {
	Root   string
	Config *Config
	State  *State
	// Notice is set when opening the archive required a repair that the
	// user should hear about (e.g. an unreadable state file).
	Notice string

	git    *gitx.Runner
	mu     sync.Mutex // guards State during concurrent updates
	unlock func()
}

// ErrNotArchive is returned by Open for directories without an archive.
var ErrNotArchive = errors.New("not a git-archive directory")

// IsArchive reports whether root contains an archive.
func IsArchive(root string) bool {
	_, err := os.Stat(filepath.Join(root, MetaDir, "config"))
	return err == nil
}

// Create initialises a new archive in root (which is created if needed).
func Create(ctx context.Context, git *gitx.Runner, root, layout string) (*Archive, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if IsArchive(root) {
		return nil, fmt.Errorf("%s already contains an archive", root)
	}
	if layout != LayoutWorktree && layout != LayoutBare {
		return nil, fmt.Errorf("unknown layout %q", layout)
	}
	if err := os.MkdirAll(filepath.Join(root, MetaDir), 0o755); err != nil {
		return nil, err
	}
	cfg, err := writeNewConfig(ctx, git, filepath.Join(root, MetaDir, "config"), layout)
	if err != nil {
		return nil, err
	}
	return &Archive{Root: root, Config: cfg, State: newState(), git: git}, nil
}

// Open loads an existing archive.
func Open(ctx context.Context, git *gitx.Runner, root string) (*Archive, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(root, MetaDir, "config")
	if _, err := os.Stat(cfgPath); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", root, ErrNotArchive)
	}
	cfg, err := LoadConfig(ctx, git, cfgPath)
	if err != nil {
		return nil, err
	}
	st, notice, err := loadState(filepath.Join(root, MetaDir, "state.json"))
	if err != nil {
		return nil, err
	}
	return &Archive{Root: root, Config: cfg, State: st, Notice: notice, git: git}, nil
}

// Lock takes the exclusive archive lock without waiting.
func (a *Archive) Lock() error {
	if a.unlock != nil {
		return nil
	}
	unlock, err := lockFile(filepath.Join(a.Root, MetaDir, "lock"))
	if err != nil {
		return err
	}
	a.unlock = unlock
	return nil
}

// Close releases the lock.
func (a *Archive) Close() {
	if a.unlock != nil {
		a.unlock()
		a.unlock = nil
	}
}

// Update runs fn with exclusive access to the state.
func (a *Archive) Update(fn func(*State)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(a.State)
}

// SaveState persists the state.
func (a *Archive) SaveState() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.State.save(filepath.Join(a.Root, MetaDir, "state.json"))
}

// SplitID splits "host/owner/repo" into host and path.
func SplitID(id string) (host, path string) {
	host, path, _ = strings.Cut(id, "/")
	return host, path
}

// RepoDir returns the location of the repository id.
func (a *Archive) RepoDir(id string) (string, error) {
	return RepoDir(a.Root, a.Config.Layout, id)
}

// RepoDir maps a repository ID to its directory below root.
func RepoDir(root, layout, id string) (string, error) {
	host, path := SplitID(id)
	if err := safe.Host(host); err != nil {
		return "", fmt.Errorf("repository %q: %w", safe.Term(id), err)
	}
	if layout == LayoutBare {
		path += ".git"
	}
	p, err := safe.Join(root, safe.HostDir(host)+"/"+path)
	if err != nil {
		return "", fmt.Errorf("repository %q: %w", safe.Term(id), err)
	}
	return p, nil
}

// MetadataPath returns where the metadata snapshot of id is stored.
func (a *Archive) MetadataPath(id string) (string, error) {
	host, path := SplitID(id)
	if err := safe.Host(host); err != nil {
		return "", err
	}
	return safe.Join(filepath.Join(a.Root, MetaDir, "metadata"), safe.HostDir(host)+"/"+path+".json")
}

// Scratch returns an in-memory archive rooted at root with default
// settings. Nothing is written; it serves dry runs for archives that do
// not exist yet.
func Scratch(root, layout string) *Archive {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	cfg := &Config{Version: 1, Layout: layout, Jobs: DefaultJobs, Timeout: DefaultTimeout, Retries: DefaultRetries,
		Defaults: map[string]string{}}
	return &Archive{Root: abs, Config: cfg, State: newState()}
}

// AddSourceInMemory adds or replaces a source without writing the
// configuration file (dry runs).
func (c *Config) AddSourceInMemory(s *Source) {
	for i, old := range c.Sources {
		if old.Name == s.Name {
			c.Sources[i] = s
			return
		}
	}
	c.Sources = append(c.Sources, s)
}
