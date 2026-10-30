// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sylirre/git-archive/internal/archive"
	"github.com/sylirre/git-archive/internal/safe"
	"github.com/sylirre/git-archive/internal/units"
)

// SyncOptions select what a sync does.
type SyncOptions struct {
	Only   []string // restrict to repositories matching these patterns
	DryRun bool     // plan only
}

// absentRecheck is how long a missing optional repository (a wiki that
// was never created) is remembered before it is probed again.
const absentRecheck = 7 * 24 * time.Hour

// Sync brings the archive up to date with its sources.
func (e *Engine) Sync(ctx context.Context, opts SyncOptions) (*Report, error) {
	rep := newReport("sync", e.Archive)
	rep.DryRun = opts.DryRun
	e.Reporter.Phase("Listing sources", len(e.Archive.Config.Sources))
	d := e.discover(ctx)
	rep.Sources = d.results
	for _, sr := range d.results {
		if sr.Error != "" && sr.Error != "not found upstream" {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: listing failed, syncing known repositories only: %s", sr.Name, sr.Error))
		}
		for _, s := range sr.Skipped {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: ignored a repository: %s", sr.Name, safe.TermLine(s)))
		}
	}
	for _, w := range rep.Warnings {
		e.Reporter.Message(Warn, w)
	}
	if err := ctx.Err(); err != nil {
		rep.finish(nil)
		return rep, err
	}

	tasks, skipped := e.plan(d, opts.Only)
	if opts.DryRun {
		for _, t := range tasks {
			skipped = append(skipped, &Result{ID: t.ID, Action: t.Action, Outcome: Planned, Reason: t.Reason})
		}
		rep.finish(skipped)
		return rep, nil
	}
	e.recordSkips(skipped, d)

	stop := e.stateSaver(5 * time.Second)
	e.Reporter.Phase("Syncing", len(tasks))
	results := e.runPool(ctx, tasks, e.syncOne)
	err := stop()
	rep.finish(append(skipped, results...))
	if err != nil {
		return rep, fmt.Errorf("saving state: %w", err)
	}
	return rep, ctx.Err()
}

// plan turns candidates into tasks, applying exclusions and filters and
// detecting renames and repositories gone upstream.
func (e *Engine) plan(d *discovery, only []string) (tasks []*Task, skipped []*Result) {
	cfg, st := e.Archive.Config, e.Archive.State
	now := e.now()

	// Renames: a new ID whose forge ID belongs to a known repository
	// that is no longer listed.
	byForgeID := map[string]string{}
	for id, rs := range st.Repos {
		if rs.ForgeID != "" && d.cands[id] == nil {
			byForgeID[rs.Source+"\x00"+rs.ForgeID] = id
		}
	}
	renamedFrom := map[string]string{}
	for _, id := range d.order {
		c := d.cands[id]
		if _, known := st.Repos[id]; known || c.repo.ForgeID == "" {
			continue
		}
		if old, ok := byForgeID[c.source.Name+"\x00"+c.repo.ForgeID]; ok {
			renamedFrom[id] = old
		}
	}
	renamedTo := map[string]bool{}
	for _, old := range renamedFrom {
		renamedTo[old] = true
	}

	// Gone: known repositories of a successfully listed source that are
	// no longer listed (and were not renamed).
	for id, rs := range st.Repos {
		if d.cands[id] != nil || renamedTo[id] || !archive.MatchAny(only, id) {
			continue
		}
		if !(d.listed[rs.Source] || d.gone[id]) || e.Archive.Config.Source(rs.Source) == nil {
			continue
		}
		if rs.Wiki {
			continue // wikis follow their repository; disabled wikis are just left alone
		}
		r := &Result{ID: id, Action: ActSkip, Outcome: Gone, Reason: "no longer available upstream"}
		if dir, err := e.Archive.RepoDir(id); err == nil && isRepo(dir) {
			r.Reason += "; local copy kept"
		}
		if rs.Gone.IsZero() {
			r.note("first noticed missing upstream")
		}
		skipped = append(skipped, r)
	}

	for _, id := range d.order {
		c := d.cands[id]
		if !archive.MatchAny(only, id) {
			continue
		}
		dir, err := e.Archive.RepoDir(id)
		if err != nil {
			skipped = append(skipped, &Result{ID: id, Action: ActSkip, Outcome: Failed, err: err})
			continue
		}
		t := &Task{ID: id, Dir: dir, Repo: c.repo, Source: c.source.Name, Opts: c.opts, Wiki: c.wiki, RenameOf: renamedFrom[id]}
		if reason := filterReason(cfg, c); reason != "" {
			out := Filtered
			if strings.Contains(reason, "excluded by pattern") {
				out = Excluded
			}
			skipped = append(skipped, &Result{ID: id, Action: ActSkip, Outcome: out, Reason: reason})
			continue
		}
		rs := st.Repos[id]
		if c.wiki && rs != nil && !rs.Absent.IsZero() && now.Sub(rs.Absent) < absentRecheck {
			skipped = append(skipped, &Result{ID: id, Action: ActSkip, Outcome: Absent, Reason: "no wiki upstream"})
			continue
		}
		t.Action = ActClone
		if isRepo(dir) || (t.RenameOf != "" && e.renameSourceExists(t.RenameOf)) {
			t.Action = ActUpdate
		}
		t.estimate = estimate(t, rs)
		tasks = append(tasks, t)
	}
	// Longest-processing-time-first: starting the biggest jobs early
	// minimises the total wall time of a parallel run, instead of ending
	// with one huge clone running alone.
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].estimate != tasks[j].estimate {
			return tasks[i].estimate > tasks[j].estimate
		}
		return tasks[i].ID < tasks[j].ID
	})
	return tasks, skipped
}

func (e *Engine) renameSourceExists(oldID string) bool {
	dir, err := e.Archive.RepoDir(oldID)
	return err == nil && isRepo(dir)
}

// filterReason explains why a candidate is not synced, or returns "".
func filterReason(cfg *archive.Config, c *candidate) string {
	if ex, by := archive.Excluded(cfg.Excludes, c.repo.ID); ex {
		return "excluded by pattern " + by
	}
	if parent, ok := strings.CutSuffix(c.repo.ID, ".wiki"); ok && c.wiki {
		// A wiki follows its repository.
		if ex, by := archive.Excluded(cfg.Excludes, parent); ex {
			return "repository excluded by pattern " + by
		}
	}
	o := c.opts
	switch {
	case c.repo.Fork && o.SkipForks:
		return "fork (skipForks)"
	case c.repo.Archived && o.SkipArchived:
		return "archived upstream (skipArchived)"
	case o.MaxSize > 0 && c.repo.Size > o.MaxSize:
		return fmt.Sprintf("size %s exceeds maxSize %s", units.FormatBytes(c.repo.Size), units.FormatBytes(o.MaxSize))
	}
	return ""
}

// estimate predicts a task's duration for scheduling.
func estimate(t *Task, rs *archive.RepoState) time.Duration {
	if rs != nil && rs.SyncSeconds > 0 && t.Action == ActUpdate {
		return time.Duration(rs.SyncSeconds * float64(time.Second))
	}
	if t.Repo.Size > 0 {
		const assumedRate = 4 << 20 // bytes per second
		return 2*time.Second + time.Duration(float64(t.Repo.Size)/assumedRate*float64(time.Second))
	}
	if t.Action == ActClone {
		return 10 * time.Second
	}
	return 3 * time.Second
}

// isRepo reports whether dir holds a git repository (bare or not).
func isRepo(dir string) bool {
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
		return true
	}
	_, errH := os.Stat(filepath.Join(dir, "HEAD"))
	fi, errO := os.Stat(filepath.Join(dir, "objects"))
	return errH == nil && errO == nil && fi.IsDir()
}

// recordSkips updates state for repositories that are not synced.
func (e *Engine) recordSkips(skipped []*Result, d *discovery) {
	now := e.now()
	e.Archive.Update(func(st *archive.State) {
		for name := range d.listed {
			src := st.Source(name)
			src.LastList, src.ListError = now, ""
		}
		for _, sr := range d.results {
			if sr.Error != "" && !d.listed[sr.Name] {
				st.Source(sr.Name).ListError = sr.Error
			}
		}
		for _, r := range skipped {
			switch r.Outcome {
			case Gone:
				if rs := st.Repos[r.ID]; rs != nil && rs.Gone.IsZero() {
					rs.Gone = now
				}
			case Excluded, Filtered, Absent:
				if c := d.cands[r.ID]; c != nil {
					e.rememberCandidate(st, c, now)
				}
			}
		}
	})
}

// rememberCandidate copies discovered facts into the state.
func (e *Engine) rememberCandidate(st *archive.State, c *candidate, now time.Time) *archive.RepoState {
	rs := st.Repo(c.repo.ID)
	rs.Source, rs.URL, rs.Wiki = c.source.Name, c.repo.CloneURL, c.wiki
	if c.repo.ForgeID != "" {
		rs.ForgeID = c.repo.ForgeID
	}
	if c.repo.DefaultBranch != "" {
		rs.DefaultBranch = c.repo.DefaultBranch
	}
	if c.listed {
		rs.Fork, rs.Archived, rs.Private = c.repo.Fork, c.repo.Archived, c.repo.Private
		if c.repo.Size >= 0 {
			rs.Size = c.repo.Size
		}
		if rs.FirstSeen.IsZero() {
			rs.FirstSeen = now
		}
		rs.LastSeen = now
		rs.Gone = time.Time{}
	}
	return rs
}

// syncOne clones or updates one repository and records the outcome.
func (e *Engine) syncOne(ctx context.Context, slot int, t *Task) *Result {
	r := &Result{ID: t.ID, Action: t.Action}
	start := time.Now()
	var err error
	if t.RenameOf != "" {
		err = e.moveRenamed(t, r)
	}
	if err == nil {
		switch t.Action {
		case ActClone:
			err = e.clone(ctx, slot, t, r)
		case ActUpdate:
			err = e.update(ctx, slot, t, r)
		}
	}
	absent := t.Wiki && t.Action == ActClone && isNotFound(err)
	switch {
	case absent:
		r.Outcome, r.Reason, err = Absent, "no wiki upstream", nil
	case err != nil && ctx.Err() != nil:
		r.Outcome = Interrupted // not the repository's fault; leave state alone
	case err != nil:
		r.Outcome, r.err = Failed, err
		r.Error = safe.TermLine(err.Error())
	case t.Action == ActClone:
		r.Outcome = Cloned
	case r.Refs.Total() > 0 || len(r.Notes) > 0:
		r.Outcome = Updated
	default:
		r.Outcome = Unchanged
	}
	if err == nil && !absent && t.Opts.Metadata && t.Repo.Meta != nil {
		if werr := e.writeMetadata(t); werr != nil {
			r.warn("metadata: %v", werr)
		}
	}
	now := e.now()
	e.Archive.Update(func(st *archive.State) {
		c := &candidate{repo: t.Repo, source: e.Archive.Config.Source(t.Source), opts: t.Opts, wiki: t.Wiki, listed: true}
		if c.source == nil {
			return
		}
		if r.Outcome == Interrupted {
			return
		}
		rs := e.rememberCandidate(st, c, now)
		rs.LastAttempt = now
		switch r.Outcome {
		case Failed:
			rs.LastError = r.Error
		case Absent:
			rs.Absent = now
		default:
			rs.LastError, rs.Absent = "", time.Time{}
			rs.LastSync = now
			rs.SyncSeconds = time.Since(start).Seconds()
			rs.Preserved += r.Preserved
			if r.Outcome == Cloned {
				rs.Cloned = now
			}
		}
		if r.Outcome == Absent && rs.Cloned.IsZero() && rs.LastSync.IsZero() {
			// Nothing on disk: keep state minimal.
			rs.Fork, rs.Archived, rs.Private = false, false, false
		}
	})
	return r
}

// moveRenamed moves the directory (and state) of a renamed repository.
func (e *Engine) moveRenamed(t *Task, r *Result) error {
	oldDir, err := e.Archive.RepoDir(t.RenameOf)
	if err != nil {
		return err
	}
	if isRepo(oldDir) {
		if _, err := os.Lstat(t.Dir); err == nil {
			return fmt.Errorf("renamed from %s, but %s already exists", t.RenameOf, t.Dir)
		}
		if err := os.MkdirAll(filepath.Dir(t.Dir), 0o755); err != nil {
			return err
		}
		if err := os.Rename(oldDir, t.Dir); err != nil {
			return fmt.Errorf("moving renamed repository: %w", err)
		}
	}
	e.Archive.Update(func(st *archive.State) {
		if old := st.Repos[t.RenameOf]; old != nil {
			st.Repos[t.ID] = old
			delete(st.Repos, t.RenameOf)
		}
	})
	if p, err := e.Archive.MetadataPath(t.RenameOf); err == nil {
		_ = os.Remove(p)
	}
	r.note("renamed upstream from %s", t.RenameOf)
	return nil
}

// writeMetadata saves the API metadata snapshot when it changed.
func (e *Engine) writeMetadata(t *Task) error {
	p, err := e.Archive.MetadataPath(t.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(t.Repo.Meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if old, err := os.ReadFile(p); err == nil && string(old) == string(data) {
		return nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return archive.WriteFileAtomic(p, data, 0o644)
}
