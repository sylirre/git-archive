// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

// Package forge discovers repositories through the APIs of code hosting
// platforms (GitHub, GitLab, Gitea/Forgejo) and describes single
// repositories on any git server.
//
// All data returned by a forge is treated as hostile: names are validated
// before they can become paths, repositories outside the requested owner
// are dropped, clone URLs are constructed locally from validated parts
// rather than taken from the response, and HTTP responses are bounded in
// size, time and redirect targets.
package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/sylirre/git-archive/internal/safe"
)

// ErrNotFound means the owner or repository does not exist (or is not
// visible with the available credentials).
var ErrNotFound = errors.New("not found")

// Repo is a repository discovered on a forge.
type Repo struct {
	ID            string // archive ID: host/path
	Path          string // owner/repo or group/sub/project
	CloneURL      string
	WikiURL       string // set when the forge reports an enabled wiki
	DefaultBranch string
	ForgeID       string // stable numeric ID, survives renames
	Fork          bool
	Archived      bool
	Private       bool
	Empty         bool
	Size          int64 // bytes as reported by the forge; -1 when unknown
	Meta          *Metadata
}

// Metadata is the provider-independent snapshot saved next to the archive.
type Metadata struct {
	Provider      string   `json:"provider"`
	ID            string   `json:"id"`
	FullName      string   `json:"fullName"`
	Description   string   `json:"description,omitempty"`
	Homepage      string   `json:"homepage,omitempty"`
	WebURL        string   `json:"webUrl,omitempty"`
	Language      string   `json:"language,omitempty"`
	License       string   `json:"license,omitempty"`
	Topics        []string `json:"topics,omitempty"`
	Stars         int      `json:"stars"`
	Forks         int      `json:"forks"`
	Fork          bool     `json:"fork"`
	Archived      bool     `json:"archived"`
	Private       bool     `json:"private"`
	DefaultBranch string   `json:"defaultBranch,omitempty"`
	HasWiki       bool     `json:"hasWiki"`
	Size          int64    `json:"size,omitempty"`
	Created       string   `json:"created,omitempty"`
	Updated       string   `json:"updated,omitempty"`
	Pushed        string   `json:"pushed,omitempty"`
}

// Client talks to one kind of forge.
type Client interface {
	// Resolve settles the type of the spec (owner or repository) and the
	// canonical spelling of its path.
	Resolve(ctx context.Context, s Spec) (Spec, error)
	// List returns all repositories of an owner. Entries the forge
	// returned but that failed validation are reported in Listing.Skipped.
	List(ctx context.Context, s Spec) (Listing, error)
	// Get returns a single repository.
	Get(ctx context.Context, s Spec) (Repo, error)
}

// Listing is the result of listing an owner.
type Listing struct {
	Repos   []Repo
	Skipped []string // reasons for entries that were rejected
	seen    map[string]bool
}

// add appends r unless its ID was already seen (pages can shift while a
// listing is in progress, repeating entries).
func (l *Listing) add(r Repo) {
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	if l.seen[r.ID] {
		return
	}
	l.seen[r.ID] = true
	l.Repos = append(l.Repos, r)
}

// Options configures client construction.
type Options struct {
	HTTP      *http.Client // nil: a hardened default client
	UserAgent string
	Getenv    func(string) string // nil: os.Getenv
	Protocol  string              // "https" (default) or "ssh" clone URLs
}

// NewClient returns the client for the spec's provider.
func NewClient(s Spec, o Options) (Client, error) {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if s.Provider == Git {
		return generic{}, nil
	}
	api, err := newAPI(s, o)
	if err != nil {
		return nil, err
	}
	switch s.Provider {
	case GitHub:
		return &github{api: api}, nil
	case GitLab:
		return &gitlab{api: api}, nil
	case Gitea:
		return &gitea{api: api}, nil
	}
	return nil, fmt.Errorf("unknown provider %q", s.Provider)
}

// tokenVars are the conventional environment variables per provider.
// They are only honoured for the provider's public instance: a token for
// github.com must never be sent to some other host the user typed.
var tokenVars = map[string][]string{
	"github.com":   {"GITHUB_TOKEN", "GH_TOKEN"},
	"gitlab.com":   {"GITLAB_TOKEN"},
	"codeberg.org": {"CODEBERG_TOKEN", "GITEA_TOKEN"},
}

// Token returns the API/git token for a spec: from its TokenEnv variable
// if configured, else from the conventional variables of public forges.
func Token(s Spec, getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if s.TokenEnv != "" {
		return strings.TrimSpace(getenv(s.TokenEnv))
	}
	for _, v := range tokenVars[s.Host] {
		if t := strings.TrimSpace(getenv(v)); t != "" {
			return t
		}
	}
	return ""
}

// CloneURL builds the clone URL for a repository path on the spec's host.
// URLs are always assembled from validated components; URLs suggested by
// the API are ignored so a hostile server cannot point git elsewhere
// (another host, a local path, or a transport helper like ext::).
func CloneURL(s Spec, path, protocol string) string {
	if protocol == "ssh" {
		host, port, hasPort := strings.Cut(s.Host, ":")
		if hasPort {
			return "ssh://git@" + host + ":" + port + "/" + path + ".git"
		}
		return "git@" + host + ":" + path + ".git"
	}
	scheme := "https"
	if s.Scheme == "http" && s.Insecure {
		scheme = "http"
	}
	return scheme + "://" + s.Host + "/" + path + ".git"
}

// newRepo validates a repository path reported by the API and fills in
// the derived fields. owner restricts the accepted paths: a listing for
// owner "termux" may only contain "termux/..." paths.
func newRepo(s Spec, owner, path string, protocol string) (Repo, error) {
	if err := safe.RelPath(path); err != nil {
		return Repo{}, fmt.Errorf("forge returned an unsafe repository path: %w", err)
	}
	if owner != "" && !strings.HasPrefix(strings.ToLower(path), strings.ToLower(owner)+"/") {
		return Repo{}, fmt.Errorf("forge returned repository %q outside of %q", safe.Term(path), safe.Term(owner))
	}
	return Repo{
		ID:       s.Host + "/" + path,
		Path:     path,
		CloneURL: CloneURL(s, path, protocol),
		Size:     -1,
	}, nil
}

// wikiURL derives the wiki clone URL from a repository clone URL.
func wikiURL(clone string) string {
	return strings.TrimSuffix(clone, ".git") + ".wiki.git"
}

// generic handles single repositories on servers without a known API.
type generic struct{}

func (generic) Resolve(_ context.Context, s Spec) (Spec, error) {
	s.Type = TypeRepo
	return s, nil
}

func (generic) List(context.Context, Spec) (Listing, error) {
	return Listing{}, errors.New("plain git servers cannot list repositories; add each repository URL")
}

func (generic) Get(_ context.Context, s Spec) (Repo, error) {
	return Repo{ID: s.Name(), Path: s.Path, CloneURL: s.URL, Size: -1}, nil
}

func checkPath(p string) error {
	if err := safe.RelPath(p); err != nil {
		return fmt.Errorf("forge returned an unsafe path: %w", err)
	}
	return nil
}

// canonicalURL is the URL recorded for a resolved spec: the web URL for
// http(s) sources, and the URL exactly as given for git transports (an
// ssh URL must keep its user, e.g. "git@").
func canonicalURL(s Spec) string {
	if s.Scheme == "https" || s.Scheme == "http" {
		return s.Scheme + "://" + s.Host + "/" + s.Path
	}
	return s.URL
}
