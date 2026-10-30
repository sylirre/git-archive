package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/sylirre/git-archive/internal/archive"
	"github.com/sylirre/git-archive/internal/forge"
	"github.com/sylirre/git-archive/internal/safe"
)

// SpecOf converts a configured source back into a forge spec.
func SpecOf(s *archive.Source) (forge.Spec, error) {
	provider := s.Provider
	spec, err := forge.ParseSpec(s.URL, provider, s.Insecure)
	if err != nil {
		return spec, fmt.Errorf("source %q: %w", s.Name, err)
	}
	if spec.Type == "" || s.Type != "" {
		spec.Type = s.Type
	}
	spec.API = s.API
	spec.TokenEnv = s.TokenEnv
	if spec.Name() != s.Name {
		return spec, fmt.Errorf("source %q: url %q does not match the section name", s.Name, safe.Term(s.URL))
	}
	return spec, nil
}

// candidate is a repository that some source wants archived.
type candidate struct {
	repo   forge.Repo
	source *archive.Source
	opts   archive.Options
	wiki   bool
	listed bool // confirmed by the forge in this run (false: from state)
}

// discovery is the outcome of listing all sources.
type discovery struct {
	cands   map[string]*candidate
	order   []string
	gone    map[string]bool // IDs confirmed missing upstream
	listed  map[string]bool // sources whose listing succeeded
	results []SourceResult
}

func (d *discovery) add(c *candidate) {
	if old, ok := d.cands[c.repo.ID]; ok {
		// A repository named explicitly wins over the same repository
		// found through an owner, so its options apply.
		if old.source.Type == forge.TypeRepo || c.source.Type != forge.TypeRepo {
			return
		}
	} else {
		d.order = append(d.order, c.repo.ID)
	}
	d.cands[c.repo.ID] = c
}

// discover lists every configured source in parallel.
func (e *Engine) discover(ctx context.Context) *discovery {
	cfg := e.Archive.Config
	d := &discovery{cands: map[string]*candidate{}, gone: map[string]bool{}, listed: map[string]bool{}}
	type out struct {
		repos   []forge.Repo
		skipped []string
		gone    bool
		err     error
	}
	outs := make([]out, len(cfg.Sources))
	sem := make(chan struct{}, e.jobs())
	var wg sync.WaitGroup
	for i, src := range cfg.Sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			repos, skipped, gone, err := e.listSource(ctx, src)
			outs[i] = out{repos, skipped, gone, err}
		}()
	}
	wg.Wait()

	for i, src := range cfg.Sources {
		o := outs[i]
		opts, _ := cfg.Options(src) // validated at load
		sr := SourceResult{Name: src.Name, Repos: len(o.repos), Skipped: o.skipped}
		switch {
		case o.gone:
			sr.Error = "not found upstream"
			d.listed[src.Name] = true
			// Single-repository source that vanished: mark below.
			if spec, err := SpecOf(src); err == nil {
				d.gone[spec.Name()] = true
			}
		case o.err != nil:
			sr.Error = safe.TermLine(o.err.Error())
			// Keep archiving what we already have from this source.
			o.repos = e.knownRepos(src)
		default:
			d.listed[src.Name] = true
		}
		d.results = append(d.results, sr)
		for _, r := range o.repos {
			c := &candidate{repo: r, source: src, opts: opts, listed: o.err == nil}
			d.add(c)
			if opts.Wiki && r.WikiURL != "" {
				w := r
				w.ID, w.Path, w.CloneURL, w.WikiURL = r.ID+".wiki", r.Path+".wiki", r.WikiURL, ""
				w.DefaultBranch, w.Size, w.Meta, w.Empty = "", -1, nil, false
				if r.ForgeID != "" {
					w.ForgeID = r.ForgeID + ":wiki"
				}
				d.add(&candidate{repo: w, source: src, opts: opts, wiki: true, listed: c.listed})
			}
		}
	}
	return d
}

// listSource returns the repositories of one source.
func (e *Engine) listSource(ctx context.Context, src *archive.Source) (repos []forge.Repo, skipped []string, gone bool, err error) {
	spec, err := SpecOf(src)
	if err != nil {
		return nil, nil, false, err
	}
	opts, _ := e.Archive.Config.Options(src)
	fo := e.ForgeOptions
	fo.Protocol = opts.Protocol
	newClient := e.NewClient
	if newClient == nil {
		newClient = forge.NewClient
	}
	client, err := newClient(spec, fo)
	if err != nil {
		return nil, nil, false, err
	}
	if spec.Type == forge.TypeOwner {
		l, err := client.List(ctx, spec)
		return l.Repos, l.Skipped, false, err
	}
	r, err := client.Get(ctx, spec)
	if errors.Is(err, forge.ErrNotFound) {
		return nil, nil, true, nil
	}
	if err != nil {
		// The API is unavailable, but git may still work.
		r = forge.Repo{ID: spec.Name(), Path: spec.Path, CloneURL: forge.CloneURL(spec, spec.Path, opts.Protocol), Size: -1}
		if st := e.Archive.State.Repos[spec.Name()]; st != nil {
			r.DefaultBranch, r.ForgeID = st.DefaultBranch, st.ForgeID
		}
		e.Reporter.Message(Warn, fmt.Sprintf("%s: metadata unavailable (%s); syncing with git only", src.Name, safe.TermLine(err.Error())))
		err = nil
	}
	// Explicit git transports (ssh, git, file) are kept as given.
	if spec.Scheme == "ssh" || spec.Scheme == "git" || spec.Scheme == "file" {
		r.CloneURL = spec.URL
		if r.WikiURL != "" {
			r.WikiURL = strings.TrimSuffix(spec.URL, ".git") + ".wiki.git"
		}
	}
	return []forge.Repo{r}, nil, false, err
}

// knownRepos reconstructs the repositories of a source from state, for
// use when its listing fails.
func (e *Engine) knownRepos(src *archive.Source) []forge.Repo {
	var out []forge.Repo
	for id, st := range e.Archive.State.Repos {
		if st.Source != src.Name || st.Wiki || !st.Gone.IsZero() || st.URL == "" {
			continue
		}
		_, path := archive.SplitID(id)
		r := forge.Repo{ID: id, Path: path, CloneURL: st.URL, DefaultBranch: st.DefaultBranch, ForgeID: st.ForgeID,
			Fork: st.Fork, Archived: st.Archived, Private: st.Private, Size: st.Size}
		if w := e.Archive.State.Repos[id+".wiki"]; w != nil && w.URL != "" {
			r.WikiURL = w.URL
		}
		out = append(out, r)
	}
	return out
}
