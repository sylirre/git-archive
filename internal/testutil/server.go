// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides a self-contained forge for integration tests:
// bare repositories served over smart HTTP by git http-backend (through
// net/http/cgi), and a GitHub-compatible API describing them.
package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// GitEnv is a hermetic environment for git commands in tests: no user or
// system configuration, fixed identities.
func GitEnv() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"LC_ALL=C")
}

// Git runs git hermetically and returns trimmed output.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = GitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// RepoMeta is what the fake API reports about a repository.
type RepoMeta struct {
	ID       int64
	Fork     bool
	Archived bool
	HasWiki  bool
	Size     int64 // KiB
	Hidden   bool  // omitted from API responses (deleted upstream)
}

// Server is a fake forge.
type Server struct {
	t    testing.TB
	Root string // GIT_PROJECT_ROOT
	*httptest.Server

	mu      sync.Mutex
	meta    map[string]*RepoMeta // "owner/name" -> metadata
	nextID  int64
	FailGit map[string]int // "owner/name" -> remaining requests to answer with 502
	FailAPI int            // remaining API requests to answer with 500
	Extra   []map[string]any
}

// NewServer starts a fake forge.
func NewServer(t testing.TB) *Server {
	t.Helper()
	execPath := Git(t, "", "--exec-path")
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not available: %v", err)
	}
	s := &Server{t: t, Root: t.TempDir(), meta: map[string]*RepoMeta{}, nextID: 1000, FailGit: map[string]int{}}
	cgiH := &cgi.Handler{
		Path:   backend,
		Stderr: io.Discard,
		Env: append([]string{"GIT_PROJECT_ROOT=" + s.Root, "GIT_HTTP_EXPORT_ALL=1"},
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v3/") {
			s.api(w, r)
			return
		}
		if key := repoKey(r.URL.Path); key != "" {
			s.mu.Lock()
			n := s.FailGit[key]
			if n > 0 {
				s.FailGit[key] = n - 1
			}
			s.mu.Unlock()
			if n > 0 {
				http.Error(w, "flaky", http.StatusBadGateway)
				return
			}
		}
		cgiH.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func repoKey(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
}

// Host returns host:port of the server.
func (s *Server) Host() string { return strings.TrimPrefix(s.URL, "http://") }

// Meta returns the metadata of owner/name for modification.
func (s *Server) Meta(key string) *RepoMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.meta[key]
	if m == nil {
		s.nextID++
		m = &RepoMeta{ID: s.nextID}
		s.meta[key] = m
	}
	return m
}

// Rename moves a repository upstream, keeping its forge ID.
func (s *Server) Rename(from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(filepath.Join(s.Root, from+".git"), filepath.Join(s.Root, to+".git")); err != nil {
		s.t.Fatal(err)
	}
	s.meta[to] = s.meta[from]
	delete(s.meta, from)
}

// Delete removes a repository upstream.
func (s *Server) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(s.Root, key+".git")); err != nil {
		s.t.Fatal(err)
	}
	delete(s.meta, key)
}

type apiRepo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Fork     bool   `json:"fork"`
	Archived bool   `json:"archived"`
	Size     int64  `json:"size"`
	Default  string `json:"default_branch"`
	HasWiki  bool   `json:"has_wiki"`
	Desc     string `json:"description"`
}

func (s *Server) apiRepo(key string) (apiRepo, bool) {
	m := s.meta[key]
	dir := filepath.Join(s.Root, key+".git")
	if m == nil || m.Hidden {
		return apiRepo{}, false
	}
	if _, err := os.Stat(dir); err != nil {
		return apiRepo{}, false
	}
	head, _ := os.ReadFile(filepath.Join(dir, "HEAD"))
	branch := strings.TrimPrefix(strings.TrimSpace(string(head)), "ref: refs/heads/")
	return apiRepo{ID: m.ID, FullName: key, Fork: m.Fork, Archived: m.Archived, Size: m.Size, Default: branch,
		HasWiki: m.HasWiki, Desc: "repository " + key}, true
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if s.FailAPI > 0 {
		s.FailAPI--
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"boom"}`)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v3/"), "/")
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}
	switch {
	case len(parts) == 2 && parts[0] == "users":
		if _, err := os.Stat(filepath.Join(s.Root, parts[1])); err != nil {
			notFound()
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"login": parts[1], "type": "Organization"})
	case len(parts) == 3 && parts[0] == "orgs" && parts[2] == "repos":
		entries, _ := os.ReadDir(filepath.Join(s.Root, parts[1]))
		var out []any
		var keys []string
		for _, e := range entries {
			name := strings.TrimSuffix(e.Name(), ".git")
			if strings.HasSuffix(name, ".wiki") || name == e.Name() {
				continue
			}
			keys = append(keys, parts[1]+"/"+name)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ar, ok := s.apiRepo(k); ok {
				out = append(out, ar)
			}
		}
		for _, x := range s.Extra {
			out = append(out, x)
		}
		if out == nil {
			out = []any{}
		}
		json.NewEncoder(w).Encode(out)
	case len(parts) == 3 && parts[0] == "repos":
		ar, ok := s.apiRepo(parts[1] + "/" + parts[2])
		if !ok {
			notFound()
			return
		}
		json.NewEncoder(w).Encode(ar)
	default:
		notFound()
	}
}

// Upstream is a bare repository on the server with a scratch clone used
// to author commits.
type Upstream struct {
	t    testing.TB
	Bare string
	Work string
	n    int
}

// NewRepo creates owner/name with one commit on "main".
func (s *Server) NewRepo(key string) *Upstream {
	s.t.Helper()
	s.Meta(key)
	bare := filepath.Join(s.Root, key+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		s.t.Fatal(err)
	}
	Git(s.t, "", "init", "-q", "--bare", "-b", "main", bare)
	u := &Upstream{t: s.t, Bare: bare, Work: filepath.Join(s.t.TempDir(), "work")}
	Git(s.t, "", "init", "-q", "-b", "main", u.Work)
	Git(s.t, u.Work, "remote", "add", "origin", bare)
	u.Commit("main", "README", "hello\n")
	return u
}

// NewEmptyRepo creates owner/name without commits.
func (s *Server) NewEmptyRepo(key string) string {
	s.Meta(key)
	bare := filepath.Join(s.Root, key+".git")
	Git(s.t, "", "init", "-q", "--bare", "-b", "main", bare)
	return bare
}

// Commit adds a commit on branch (created from the current HEAD if
// needed) and pushes it. It returns the new commit ID.
func (u *Upstream) Commit(branch, file, content string) string {
	u.t.Helper()
	u.n++
	if out := Git(u.t, u.Work, "branch", "--list", branch); out == "" && u.n > 1 {
		Git(u.t, u.Work, "checkout", "-q", "-b", branch)
	} else if u.n > 1 {
		Git(u.t, u.Work, "checkout", "-q", branch)
	}
	if err := os.WriteFile(filepath.Join(u.Work, file), []byte(content), 0o644); err != nil {
		u.t.Fatal(err)
	}
	Git(u.t, u.Work, "add", file)
	Git(u.t, u.Work, "commit", "-q", "-m", fmt.Sprintf("commit %d", u.n))
	Git(u.t, u.Work, "push", "-q", "origin", branch)
	return Git(u.t, u.Work, "rev-parse", "HEAD")
}

// Rewrite replaces the tip of branch with a different commit and force
// pushes it. It returns the old and new commit IDs.
func (u *Upstream) Rewrite(branch string) (old, nu string) {
	u.t.Helper()
	Git(u.t, u.Work, "checkout", "-q", branch)
	old = Git(u.t, u.Work, "rev-parse", "HEAD")
	u.n++
	Git(u.t, u.Work, "commit", "-q", "--amend", "-m", fmt.Sprintf("rewritten %d", u.n))
	Git(u.t, u.Work, "push", "-q", "--force", "origin", branch)
	return old, Git(u.t, u.Work, "rev-parse", "HEAD")
}

// Tag creates or moves a lightweight tag and pushes it.
func (u *Upstream) Tag(name, rev string) string {
	u.t.Helper()
	Git(u.t, u.Work, "tag", "-f", name, rev)
	Git(u.t, u.Work, "push", "-q", "--force", "origin", "refs/tags/"+name)
	return Git(u.t, u.Work, "rev-parse", name)
}

// DeleteBranch removes a branch upstream.
func (u *Upstream) DeleteBranch(branch string) {
	u.t.Helper()
	Git(u.t, u.Work, "push", "-q", "origin", "--delete", branch)
}

// SetHead changes the upstream default branch.
func (u *Upstream) SetHead(branch string) {
	u.t.Helper()
	Git(u.t, "", "--git-dir="+u.Bare, "symbolic-ref", "HEAD", "refs/heads/"+branch)
}

// PlantMaliciousTree pushes a commit whose tree contains an entry named
// ".git" — the shape of several historical git vulnerabilities. It is
// written straight into the bare repository, bypassing client checks.
func (u *Upstream) PlantMaliciousTree(branch string) {
	u.t.Helper()
	cmd := exec.Command("git", "--git-dir="+u.Bare, "hash-object", "-w", "--stdin")
	cmd.Env = GitEnv()
	cmd.Stdin = strings.NewReader("[core]\n\tfsmonitor = touch pwned\n")
	out, err := cmd.Output()
	if err != nil {
		u.t.Fatal(err)
	}
	cfgBlob := strings.TrimSpace(string(out))
	inner := mktree(u.t, u.Bare, "100644 blob "+cfgBlob+"\tconfig\n")
	// git mktree refuses ".git"; build the raw tree object instead.
	raw := treeEntry("40000", ".git", inner) + treeEntry("100644", "README", cfgBlob)
	cmd = exec.Command("git", "--git-dir="+u.Bare, "hash-object", "-w", "-t", "tree", "--literally", "--stdin")
	cmd.Env = GitEnv()
	cmd.Stdin = strings.NewReader(raw)
	out, err = cmd.Output()
	if err != nil {
		u.t.Fatal(err)
	}
	tree := strings.TrimSpace(string(out))
	commit := Git(u.t, "", "--git-dir="+u.Bare, "commit-tree", tree, "-m", "evil")
	Git(u.t, "", "--git-dir="+u.Bare, "update-ref", "refs/heads/"+branch, commit)
}

func mktree(t testing.TB, gitDir, spec string) string {
	cmd := exec.Command("git", "--git-dir="+gitDir, "mktree")
	cmd.Env = GitEnv()
	cmd.Stdin = strings.NewReader(spec)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func treeEntry(mode, name, hexID string) string {
	var bin []byte
	for i := 0; i+1 < len(hexID); i += 2 {
		var b byte
		fmt.Sscanf(hexID[i:i+2], "%02x", &b)
		bin = append(bin, b)
	}
	return mode + " " + name + "\x00" + string(bin)
}
