package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sylirre/git-archive/internal/archive"
	"github.com/sylirre/git-archive/internal/gitx"
)

// gitDir returns the git directory of a repository location.
func (e *Engine) gitDir(dir string) string {
	if e.layoutBare() {
		return dir
	}
	return filepath.Join(dir, ".git")
}

// git runs a local git command in a repository and returns stdout.
func (e *Engine) git(ctx context.Context, dir string, args ...string) (string, error) {
	c := gitx.Cmd{Dir: dir, GitDir: e.gitDir(dir), Args: args}
	if !e.layoutBare() {
		c.WorkTree = dir
	}
	out, err := e.Git.Run(ctx, c)
	return strings.TrimRight(string(out), "\n"), err
}

// gitProgress runs a git command whose progress is reported to the UI.
func (e *Engine) gitProgress(ctx context.Context, slot int, t *Task, dir string, args ...string) error {
	c := gitx.Cmd{Dir: dir, GitDir: e.gitDir(dir), Args: args,
		Progress: func(p gitx.Progress) { e.Reporter.Progress(slot, t, p) }}
	if !e.layoutBare() {
		c.WorkTree = dir
	}
	_, err := e.Git.Run(ctx, c)
	return err
}

// fetchRefspecs map upstream branches and tags into the repository.
func (e *Engine) fetchRefspecs() []string {
	if e.layoutBare() {
		return []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}
	}
	return []string{"+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"}
}

// clone creates the repository in a partial directory next to its final
// location and renames it into place only when everything succeeded.
// An interrupted or failed clone therefore never looks like a valid
// repository, and leftovers are removed on the next attempt.
func (e *Engine) clone(ctx context.Context, slot int, t *Task, r *Result) error {
	if _, err := os.Lstat(t.Dir); err == nil {
		return fmt.Errorf("%s exists but is not a git repository; move it away to let git-archive clone there", t.Dir)
	}
	partial := t.Dir + archive.PartialSuffix
	parent := filepath.Dir(t.Dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(partial)

	args := []string{"clone", "--progress"}
	if e.layoutBare() {
		args = append(args, "--bare")
	}
	args = append(args, "--", t.Repo.CloneURL, partial)
	e.Reporter.Step(slot, t, "clone")
	err := e.retry(ctx, slot, t, r, "clone", func() error {
		if err := os.RemoveAll(partial); err != nil {
			return err
		}
		_, err := e.Git.Run(ctx, gitx.Cmd{Dir: parent, Args: args,
			Progress: func(p gitx.Progress) { e.Reporter.Progress(slot, t, p) }})
		return err
	})
	if err != nil {
		return err
	}
	// Record the refspecs so that plain "git fetch" run by a user inside
	// the archive behaves like git-archive (all branches and tags).
	if _, err := e.git(ctx, partial, "config", "--replace-all", "remote.origin.fetch", e.fetchRefspecs()[0]); err != nil {
		return err
	}
	if _, err := e.git(ctx, partial, "config", "--add", "remote.origin.fetch", e.fetchRefspecs()[1]); err != nil {
		return err
	}
	if err := e.syncLFS(ctx, slot, t, r, partial); err != nil {
		return err
	}
	if err := os.Rename(partial, t.Dir); err != nil {
		return fmt.Errorf("moving clone into place: %w", err)
	}
	return nil
}

// refSnapshot maps ref names to object IDs.
type refSnapshot map[string]string

func (e *Engine) snapshot(ctx context.Context, dir string) (refSnapshot, error) {
	patterns := []string{"refs/heads/", "refs/tags/"}
	if !e.layoutBare() {
		patterns = []string{"refs/remotes/origin/", "refs/tags/"}
	}
	out, err := e.git(ctx, dir, append([]string{"for-each-ref", "--format=%(objectname) %(refname)"}, patterns...)...)
	if err != nil {
		return nil, err
	}
	snap := refSnapshot{}
	for _, line := range strings.Split(out, "\n") {
		oid, ref, ok := strings.Cut(line, " ")
		if !ok || ref == "refs/remotes/origin/HEAD" {
			continue
		}
		snap[ref] = oid
	}
	return snap, nil
}

// archiveRefName maps a local ref to its name below refs/archive/<stamp>/,
// independent of layout: heads/<branch> or tags/<tag>.
func archiveRefName(ref string) string {
	switch {
	case strings.HasPrefix(ref, "refs/remotes/origin/"):
		return "heads/" + strings.TrimPrefix(ref, "refs/remotes/origin/")
	case strings.HasPrefix(ref, "refs/heads/"):
		return "heads/" + strings.TrimPrefix(ref, "refs/heads/")
	default:
		return strings.TrimPrefix(ref, "refs/")
	}
}

// update fetches upstream, preserves rewritten history and refreshes the
// working tree.
func (e *Engine) update(ctx context.Context, slot int, t *Task, r *Result) error {
	if !isRepo(t.Dir) {
		return fmt.Errorf("%s is not a git repository", t.Dir)
	}
	url, _ := e.git(ctx, t.Dir, "config", "--get", "remote.origin.url")
	if url != t.Repo.CloneURL {
		if url == "" {
			if _, err := e.git(ctx, t.Dir, "remote", "add", "origin", t.Repo.CloneURL); err != nil {
				return err
			}
		} else if _, err := e.git(ctx, t.Dir, "remote", "set-url", "origin", t.Repo.CloneURL); err != nil {
			return err
		}
		if url != "" {
			r.note("remote URL changed to %s", t.Repo.CloneURL)
		}
	}
	before, err := e.snapshot(ctx, t.Dir)
	if err != nil {
		return err
	}
	e.Reporter.Step(slot, t, "fetch")
	args := append([]string{"fetch", "--progress", "--prune", "--no-recurse-submodules", "origin"}, e.fetchRefspecs()...)
	if err := e.retry(ctx, slot, t, r, "fetch", func() error {
		return e.gitProgress(ctx, slot, t, t.Dir, args...)
	}); err != nil {
		return err
	}
	after, err := e.snapshot(ctx, t.Dir)
	if err != nil {
		return err
	}
	rewritten, err := e.diffRefs(ctx, t.Dir, before, after, &r.Refs)
	if err != nil {
		return err
	}
	stamp := e.now().UTC().Format("20060102T150405Z")
	if t.Opts.Preserve && len(rewritten) > 0 {
		if err := e.preserve(ctx, t.Dir, stamp, rewritten); err != nil {
			return fmt.Errorf("preserving rewritten refs: %w", err)
		}
		r.Preserved += len(rewritten)
	}
	if e.layoutBare() {
		e.updateBareHead(ctx, t, r)
	} else if err := e.updateWorktree(ctx, slot, t, r, stamp, rewritten); err != nil {
		return err
	}
	if r.Refs.Total() > 0 {
		if err := e.syncLFS(ctx, slot, t, r, t.Dir); err != nil {
			return err
		}
	}
	return nil
}

// diffRefs counts ref changes and returns the old values of refs whose
// history was rewritten (non-fast-forward, moved tags) or deleted.
func (e *Engine) diffRefs(ctx context.Context, dir string, before, after refSnapshot, c *RefChanges) (map[string]string, error) {
	lost := map[string]string{}
	for ref, old := range before {
		nu, ok := after[ref]
		switch {
		case !ok:
			c.Deleted++
			lost[ref] = old
		case nu == old:
		case strings.HasPrefix(ref, "refs/tags/"):
			c.Forced++ // a tag that moves is always a rewrite
			lost[ref] = old
		default:
			ff, err := e.isAncestor(ctx, dir, old, nu)
			if err != nil {
				return nil, err
			}
			if ff {
				c.Updated++
			} else {
				c.Forced++
				lost[ref] = old
			}
		}
	}
	for ref := range after {
		if _, ok := before[ref]; !ok {
			c.Added++
		}
	}
	return lost, nil
}

func (e *Engine) isAncestor(ctx context.Context, dir, a, b string) (bool, error) {
	_, err := e.git(ctx, dir, "merge-base", "--is-ancestor", a, b)
	var ge *gitx.Error
	if errors.As(err, &ge) && ge.Exit == 1 {
		return false, nil
	}
	return err == nil, err
}

// preserve stores old ref values below refs/archive/<stamp>/ so that the
// objects stay reachable (and survive gc) after upstream rewrote or
// deleted them.
func (e *Engine) preserve(ctx context.Context, dir, stamp string, refs map[string]string) error {
	names := make([]string, 0, len(refs))
	for ref := range refs {
		names = append(names, ref)
	}
	sort.Strings(names)
	var in strings.Builder
	for _, ref := range names {
		fmt.Fprintf(&in, "create refs/archive/%s/%s %s\n", stamp, archiveRefName(ref), refs[ref])
	}
	c := gitx.Cmd{Dir: dir, GitDir: e.gitDir(dir), Args: []string{"update-ref", "--stdin"}, Stdin: strings.NewReader(in.String())}
	_, err := e.Git.Run(ctx, c)
	return err
}

// updateBareHead points HEAD at the upstream default branch.
func (e *Engine) updateBareHead(ctx context.Context, t *Task, r *Result) {
	want := t.Repo.DefaultBranch
	if want == "" {
		return
	}
	cur, _ := e.git(ctx, t.Dir, "symbolic-ref", "-q", "HEAD")
	if cur == "refs/heads/"+want {
		return
	}
	if _, err := e.git(ctx, t.Dir, "rev-parse", "-q", "--verify", "refs/heads/"+want); err != nil {
		return
	}
	if _, err := e.git(ctx, t.Dir, "symbolic-ref", "HEAD", "refs/heads/"+want); err == nil {
		r.note("default branch is now %s", want)
	}
}

// updateWorktree moves the checked-out branch to the upstream default
// branch. Local modifications are never overwritten: the working tree is
// then left alone with a warning (the fetched history is safe in refs
// either way).
func (e *Engine) updateWorktree(ctx context.Context, slot int, t *Task, r *Result, stamp string, rewritten map[string]string) error {
	cur, _ := e.git(ctx, t.Dir, "symbolic-ref", "-q", "--short", "HEAD")
	target := t.Repo.DefaultBranch
	if target == "" {
		if ref, err := e.git(ctx, t.Dir, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil {
			target = strings.TrimPrefix(ref, "origin/")
		} else {
			target = cur
		}
	}
	if target == "" {
		return nil
	}
	remoteRef := "refs/remotes/origin/" + target
	remote, err := e.git(ctx, t.Dir, "rev-parse", "-q", "--verify", remoteRef+"^{commit}")
	if err != nil {
		// Empty repositories have no branch yet; anything else is odd.
		if has, _ := e.git(ctx, t.Dir, "for-each-ref", "--count=1", "--format=x", "refs/remotes/origin/"); has != "" {
			r.warn("upstream branch %s not found; working tree unchanged", target)
		}
		return nil
	}
	_, _ = e.git(ctx, t.Dir, "symbolic-ref", "refs/remotes/origin/HEAD", remoteRef)
	head, _ := e.git(ctx, t.Dir, "rev-parse", "-q", "--verify", "HEAD")
	if cur == target && head == remote {
		return nil
	}
	status, err := e.git(ctx, t.Dir, "status", "--porcelain", "--untracked-files=no", "--ignore-submodules=all")
	if err != nil {
		return err
	}
	if status != "" {
		r.warn("working tree has local modifications; checkout of %s skipped", target)
		return nil
	}
	alreadySaved := false
	for _, oid := range rewritten {
		alreadySaved = alreadySaved || oid == head
	}
	if head != "" && cur == target && t.Opts.Preserve && !alreadySaved {
		// Local commits (made by hand inside the archive) that upstream
		// does not have would be orphaned by the checkout below.
		ff, err := e.isAncestor(ctx, t.Dir, head, remote)
		if err != nil {
			return err
		}
		if !ff {
			if err := e.preserve(ctx, t.Dir, stamp, map[string]string{"refs/local/" + target: head}); err != nil {
				return err
			}
			r.Preserved++
		}
	}
	e.Reporter.Step(slot, t, "checkout")
	err = e.gitProgress(ctx, slot, t, t.Dir, "checkout", "--progress", "--no-recurse-submodules", "-B", target, remoteRef)
	if err != nil {
		r.warn("checkout of %s failed: %s", target, shortErr(err))
		return nil
	}
	if cur != target && cur != "" {
		r.note("default branch changed from %s to %s", cur, target)
	}
	return nil
}

// syncLFS downloads Git LFS objects for all refs when enabled.
func (e *Engine) syncLFS(ctx context.Context, slot int, t *Task, r *Result, dir string) error {
	if !t.Opts.LFS || t.Wiki {
		return nil
	}
	if !e.lfsAvailable(ctx) {
		r.warn("lfs is enabled but git-lfs is not installed")
		return nil
	}
	e.Reporter.Step(slot, t, "lfs fetch")
	if err := e.retry(ctx, slot, t, r, "lfs fetch", func() error {
		return e.gitProgress(ctx, slot, t, dir, "lfs", "fetch", "--all", "origin")
	}); err != nil {
		return fmt.Errorf("lfs fetch: %w", err)
	}
	if !e.layoutBare() {
		if err := e.gitProgress(ctx, slot, t, dir, "lfs", "checkout"); err != nil {
			r.warn("lfs checkout: %s", shortErr(err))
		}
	}
	return nil
}

func shortErr(err error) string {
	var ge *gitx.Error
	if errors.As(err, &ge) {
		if s := ge.Summary(); s != "" {
			return s
		}
	}
	return err.Error()
}

func isNotFound(err error) bool { return err != nil && gitx.IsNotFound(err) }

func removeAll(p string) error { return os.RemoveAll(p) }
