// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sylirre/git-archive/internal/archive"
	"github.com/sylirre/git-archive/internal/gitx"
	"github.com/sylirre/git-archive/internal/safe"
)

// VerifyOptions select what verify checks.
type VerifyOptions struct {
	Only   []string
	Quick  bool // connectivity only, skip hashing every object
	Remote bool // compare refs with upstream (ls-remote; no download)
}

// Verify checks the integrity of archived repositories.
func (e *Engine) Verify(ctx context.Context, opts VerifyOptions) (*Report, error) {
	rep := newReport("verify", e.Archive)
	tasks, pre := e.existingTargets(opts.Only, ActVerify, true)
	e.Reporter.Phase("Verifying", len(tasks))
	results := e.runPool(ctx, tasks, func(ctx context.Context, slot int, t *Task) *Result {
		return e.verifyOne(ctx, slot, t, opts)
	})
	now := e.now()
	e.Archive.Update(func(st *archive.State) {
		for _, r := range results {
			rs := st.Repos[r.ID]
			if rs == nil || r.Outcome == Interrupted {
				continue
			}
			rs.LastVerify, rs.VerifyError = now, ""
			if r.Outcome == Failed {
				rs.VerifyError = r.Error
			}
		}
	})
	err := e.Archive.SaveState()
	rep.finish(append(pre, results...))
	if err != nil {
		return rep, err
	}
	return rep, ctx.Err()
}

// existingTargets returns tasks for repositories on disk plus failures
// for repositories that should exist but do not.
func (e *Engine) existingTargets(only []string, act Action, reportMissing bool) ([]*Task, []*Result) {
	a := e.Archive
	onDisk, partials := scanRepos(a.Root, a.Config.Layout)
	var tasks []*Task
	var pre []*Result
	for _, p := range partials {
		e.Reporter.Message(Warn, "leftover partial clone (removed by the next sync or gc): "+p)
	}
	ids := map[string]bool{}
	for id := range onDisk {
		ids[id] = true
	}
	for id, rs := range a.State.Repos {
		if !rs.Cloned.IsZero() || !rs.LastSync.IsZero() {
			ids[id] = true
		}
	}
	for id := range ids {
		if !archive.MatchAny(only, id) {
			continue
		}
		dir, err := a.RepoDir(id)
		if err != nil {
			pre = append(pre, &Result{ID: id, Action: act, Outcome: Failed, Error: err.Error()})
			continue
		}
		if onDisk[id] == "" {
			if reportMissing {
				pre = append(pre, &Result{ID: id, Action: act, Outcome: Failed, Error: "repository is missing from disk"})
			}
			continue
		}
		t := &Task{ID: id, Dir: dir, Action: act}
		if rs := a.State.Repos[id]; rs != nil {
			t.Source = rs.Source
			t.Repo.CloneURL = rs.URL
			t.estimate = time.Duration(rs.Size)
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].estimate != tasks[j].estimate {
			return tasks[i].estimate > tasks[j].estimate
		}
		return tasks[i].ID < tasks[j].ID
	})
	return tasks, pre
}

func (e *Engine) verifyOne(ctx context.Context, slot int, t *Task, opts VerifyOptions) *Result {
	r := &Result{ID: t.ID, Action: ActVerify, Outcome: Verified}
	fail := func(format string, args ...any) *Result {
		r.Outcome = Failed
		r.Error = safe.TermLine(fmt.Sprintf(format, args...))
		return r
	}
	if url, _ := e.git(ctx, t.Dir, "config", "--get", "remote.origin.url"); t.Repo.CloneURL != "" && url != t.Repo.CloneURL {
		r.warn("remote URL is %q, expected %q", url, t.Repo.CloneURL)
	}
	hasRefs, _ := e.git(ctx, t.Dir, "for-each-ref", "--count=1", "--format=x")
	if _, err := e.git(ctx, t.Dir, "rev-parse", "-q", "--verify", "HEAD^{commit}"); err != nil && hasRefs != "" {
		r.warn("HEAD does not point to a commit")
	}

	e.Reporter.Step(slot, t, "fsck")
	args := []string{"fsck", "--progress", "--no-dangling", "--strict"}
	if opts.Quick {
		args = append(args, "--connectivity-only")
	}
	c := gitx.Cmd{Dir: t.Dir, GitDir: e.gitDir(t.Dir), Args: args,
		Progress: func(p gitx.Progress) { e.Reporter.Progress(slot, t, p) }}
	out, err := e.Git.Run(ctx, c)
	if err != nil {
		if ctx.Err() != nil {
			return fail("interrupted")
		}
		msg := shortErr(err)
		if lines := strings.Fields(string(out)); len(lines) > 0 {
			first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 4)
			msg = strings.Join(first[:min(len(first), 3)], " | ") + " | " + msg
		}
		return fail("fsck: %s", msg)
	}

	if !e.layoutBare() {
		status, err := e.git(ctx, t.Dir, "status", "--porcelain", "--untracked-files=no", "--ignore-submodules=all")
		if err != nil {
			r.warn("status: %s", shortErr(err))
		} else if status != "" {
			r.warn("%d tracked files modified in the working tree", strings.Count(status, "\n")+1)
		}
	}
	if opts.Remote {
		e.Reporter.Step(slot, t, "ls-remote")
		e.compareRemote(ctx, slot, t, r)
	}
	return r
}

// compareRemote reports refs that differ from upstream without fetching.
func (e *Engine) compareRemote(ctx context.Context, slot int, t *Task, r *Result) {
	var out string
	err := e.retry(ctx, slot, t, r, "ls-remote", func() error {
		var err error
		out, err = e.git(ctx, t.Dir, "ls-remote", "--heads", "--tags", "origin")
		return err
	})
	if err != nil {
		if gitx.IsNotFound(err) {
			r.warn("upstream repository not found")
		} else {
			r.warn("cannot reach upstream: %s", shortErr(err))
		}
		return
	}
	remote := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		oid, ref, ok := strings.Cut(line, "\t")
		if !ok || strings.HasSuffix(ref, "^{}") {
			continue
		}
		remote[ref] = oid
	}
	local, err := e.snapshot(ctx, t.Dir)
	if err != nil {
		r.warn("reading refs: %s", shortErr(err))
		return
	}
	norm := map[string]string{}
	for ref, oid := range local {
		norm["refs/"+archiveRefName(ref)] = oid
	}
	changed, deleted := 0, 0
	for ref, oid := range remote {
		if norm[ref] != oid {
			changed++
		}
	}
	for ref := range norm {
		if _, ok := remote[ref]; !ok {
			deleted++
		}
	}
	if changed+deleted == 0 {
		r.note("in sync with upstream")
		return
	}
	r.warn("differs from upstream: %d new or changed refs, %d deleted upstream (run a sync)", changed, deleted)
}

// GCOptions select what gc does.
type GCOptions struct {
	Only       []string
	Aggressive bool
}

// GC repacks repositories and removes leftover partial clones.
func (e *Engine) GC(ctx context.Context, opts GCOptions) (*Report, error) {
	rep := newReport("gc", e.Archive)
	_, partials := scanRepos(e.Archive.Root, e.Archive.Config.Layout)
	for _, p := range partials {
		if err := removePartial(e.Archive.Root, p); err != nil {
			rep.Warnings = append(rep.Warnings, err.Error())
		} else {
			e.Reporter.Message(Info, "removed leftover partial clone "+p)
		}
	}
	tasks, _ := e.existingTargets(opts.Only, ActGC, false)
	e.Reporter.Phase("Maintenance", len(tasks))
	results := e.runPool(ctx, tasks, func(ctx context.Context, slot int, t *Task) *Result {
		r := &Result{ID: t.ID, Action: ActGC, Outcome: Maintained}
		before := e.objectBytes(ctx, t.Dir)
		e.Reporter.Step(slot, t, "gc")
		args := []string{"gc", "--quiet"}
		if opts.Aggressive {
			args = append(args, "--aggressive")
		}
		if err := e.gitSilent(ctx, t.Dir, args...); err != nil {
			r.Outcome, r.Error = Failed, safe.TermLine(shortErr(err))
			return r
		}
		r.Freed = max(before-e.objectBytes(ctx, t.Dir), 0)
		return r
	})
	rep.finish(results)
	return rep, ctx.Err()
}

// objectBytes returns the size of the object store via count-objects.
func (e *Engine) objectBytes(ctx context.Context, dir string) int64 {
	out, err := e.git(ctx, dir, "count-objects", "-v")
	if err != nil {
		return 0
	}
	var total int64
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if ok && (k == "size" || k == "size-pack" || k == "size-garbage") {
			n, _ := strconv.ParseInt(v, 10, 64)
			total += n << 10
		}
	}
	return total
}

func removePartial(root, p string) error {
	if !safe.Within(root, p) || !strings.HasSuffix(p, archive.PartialSuffix) {
		return errors.New("refusing to remove " + p)
	}
	return removeAll(p)
}
