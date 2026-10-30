// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylirre/git-archive/internal/archive"
	"github.com/sylirre/git-archive/internal/engine"
	"github.com/sylirre/git-archive/internal/gitx"
	"github.com/sylirre/git-archive/internal/safe"
	"github.com/sylirre/git-archive/internal/testutil"
)

type recorder struct {
	mu       sync.Mutex
	messages []string
	progress int
	done     []*engine.Result
}

func (r *recorder) Phase(string, int)              {}
func (r *recorder) Start(int, *engine.Task)        {}
func (r *recorder) Step(int, *engine.Task, string) {}
func (r *recorder) Progress(int, *engine.Task, gitx.Progress) {
	r.mu.Lock()
	r.progress++
	r.mu.Unlock()
}
func (r *recorder) Done(_ int, _ *engine.Task, res *engine.Result) {
	r.mu.Lock()
	r.done = append(r.done, res)
	r.mu.Unlock()
}
func (r *recorder) Message(_ engine.Level, text string) {
	r.mu.Lock()
	r.messages = append(r.messages, text)
	r.mu.Unlock()
}

type env struct {
	t     *testing.T
	srv   *testutil.Server
	git   *gitx.Runner
	a     *archive.Archive
	e     *engine.Engine
	rec   *recorder
	clock time.Time
}

func newEnv(t *testing.T, layout string) *env {
	t.Helper()
	v := &env{t: t, srv: testutil.NewServer(t), rec: &recorder{}, clock: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	var err error
	v.git, err = gitx.New(gitx.Options{Protocols: []string{"http"}, Environ: testutil.GitEnv(), IdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	v.a, err = archive.Create(context.Background(), v.git, t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	v.e = &engine.Engine{Archive: v.a, Git: v.git, Reporter: v.rec, Jobs: 3, Retries: 1, Backoff: 10 * time.Millisecond,
		Now: func() time.Time { return v.clock }}
	return v
}

func (v *env) addOwner(owner string, opts map[string]string) {
	v.t.Helper()
	if opts == nil {
		opts = map[string]string{}
	}
	src := &archive.Source{Name: v.srv.Host() + "/" + owner, URL: v.srv.URL + "/" + owner, Provider: "github",
		Type: "owner", API: v.srv.URL + "/api/v3", Insecure: true, Opts: opts}
	if err := v.a.Config.SaveSource(context.Background(), src); err != nil {
		v.t.Fatal(err)
	}
}

func (v *env) sync(only ...string) *engine.Report {
	v.t.Helper()
	v.clock = v.clock.Add(time.Minute)
	rep, err := v.e.Sync(context.Background(), engine.SyncOptions{Only: only})
	if err != nil {
		v.t.Fatalf("sync: %v", err)
	}
	return rep
}

func (v *env) id(key string) string { return v.srv.Host() + "/" + key }

func (v *env) dir(key string) string {
	d, err := v.a.RepoDir(v.id(key))
	if err != nil {
		v.t.Fatal(err)
	}
	return d
}

func (v *env) result(rep *engine.Report, key string) *engine.Result {
	v.t.Helper()
	for _, r := range rep.Results {
		if r.ID == v.id(key) {
			return r
		}
	}
	v.t.Fatalf("no result for %s in %+v", key, rep.Results)
	return nil
}

func (v *env) expect(rep *engine.Report, key string, want engine.Outcome) *engine.Result {
	v.t.Helper()
	r := v.result(rep, key)
	if r.Outcome != want {
		v.t.Fatalf("%s: outcome %s (%s %s), want %s", key, r.Outcome, r.Reason, r.Error, want)
	}
	return r
}

func (v *env) refs(key, pattern string) string {
	return testutil.Git(v.t, "", "--git-dir="+v.gitDir(key), "for-each-ref", "--format=%(refname) %(objectname)", pattern)
}

func (v *env) gitDir(key string) string {
	if v.a.Config.Layout == archive.LayoutBare {
		return v.dir(key)
	}
	return filepath.Join(v.dir(key), ".git")
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCloneUpdateUnchanged(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	app := v.srv.NewRepo("org/app")
	v.srv.NewRepo("org/lib")
	v.srv.Meta("org/app").Size = 10
	app.Tag("v1", "main")
	v.addOwner("org", nil)

	rep := v.sync()
	v.expect(rep, "org/app", engine.Cloned)
	v.expect(rep, "org/lib", engine.Cloned)
	if got := readFile(t, filepath.Join(v.dir("org/app"), "README")); got != "hello\n" {
		t.Errorf("README = %q", got)
	}
	if !strings.Contains(v.refs("org/app", "refs/tags"), "refs/tags/v1") {
		t.Error("tag not cloned")
	}
	if v.rec.progress == 0 {
		t.Error("no progress reported")
	}
	// Metadata snapshot and state.
	meta := readFile(t, filepath.Join(v.a.Root, archive.MetaDir, "metadata", safe.HostDir(v.srv.Host()), "org", "app.json"))
	if !strings.Contains(meta, `"description": "repository org/app"`) {
		t.Errorf("metadata = %s", meta)
	}
	st := v.a.State.Repos[v.id("org/app")]
	if st == nil || st.Cloned.IsZero() || st.ForgeID == "" || st.DefaultBranch != "main" {
		t.Fatalf("state = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(v.a.Root, archive.MetaDir, "state.json")); err != nil {
		t.Error("state not saved")
	}
	cfg := testutil.Git(t, v.dir("org/app"), "config", "--get-all", "remote.origin.fetch")
	if !strings.Contains(cfg, "+refs/tags/*:refs/tags/*") {
		t.Errorf("fetch refspecs = %q", cfg)
	}

	rep = v.sync()
	v.expect(rep, "org/app", engine.Unchanged)

	app.Commit("main", "README", "v2\n")
	app.Commit("dev", "DEV", "dev\n")
	rep = v.sync()
	r := v.expect(rep, "org/app", engine.Updated)
	if r.Refs.Updated != 1 || r.Refs.Added != 1 || r.Refs.Forced != 0 || r.Preserved != 0 {
		t.Errorf("refs = %+v preserved=%d", r.Refs, r.Preserved)
	}
	if got := readFile(t, filepath.Join(v.dir("org/app"), "README")); got != "v2\n" {
		t.Errorf("worktree not updated: %q", got)
	}
	v.expect(rep, "org/lib", engine.Unchanged)
}

func TestPreserveRewrittenHistory(t *testing.T) {
	for _, layout := range []string{archive.LayoutWorktree, archive.LayoutBare} {
		t.Run(layout, func(t *testing.T) {
			v := newEnv(t, layout)
			up := v.srv.NewRepo("org/r")
			up.Commit("main", "a", "1")
			featTip := up.Commit("feature", "f", "1")
			tagOld := up.Tag("v1", "main")
			v.addOwner("org", nil)
			v.sync()

			oldMain, newMain := up.Rewrite("main")
			up.DeleteBranch("feature")
			up.Commit("main", "b", "2")
			newMain = testutil.Git(t, up.Work, "rev-parse", "HEAD")
			up.Tag("v1", "main")
			rep := v.sync()
			r := v.expect(rep, "org/r", engine.Updated)
			if r.Refs.Forced != 2 || r.Refs.Deleted != 1 || r.Preserved != 3 {
				t.Fatalf("refs = %+v preserved = %d", r.Refs, r.Preserved)
			}
			archived := v.refs("org/r", "refs/archive")
			for _, want := range []string{"/heads/main " + oldMain, "/heads/feature " + featTip, "/tags/v1 " + tagOld} {
				if !strings.Contains(archived, want) {
					t.Errorf("refs/archive lacks %q:\n%s", want, archived)
				}
			}
			if strings.Contains(archived, "/local/") {
				t.Errorf("local branch preserved redundantly:\n%s", archived)
			}
			// The preserved objects must survive garbage collection.
			testutil.Git(t, "", "--git-dir="+v.gitDir("org/r"), "gc", "--prune=now", "-q")
			testutil.Git(t, "", "--git-dir="+v.gitDir("org/r"), "cat-file", "-e", oldMain+"^{commit}")
			if layout == archive.LayoutWorktree {
				head := testutil.Git(t, v.dir("org/r"), "rev-parse", "HEAD")
				if head != newMain {
					t.Errorf("worktree HEAD = %s, want %s", head, newMain)
				}
			}
			if st := v.a.State.Repos[v.id("org/r")]; st.Preserved != 3 {
				t.Errorf("state preserved = %d", st.Preserved)
			}
		})
	}
}

func TestPreserveDisabled(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	up := v.srv.NewRepo("org/r")
	v.addOwner("org", map[string]string{"preserve": "false"})
	v.sync()
	up.Rewrite("main")
	rep := v.sync()
	if r := v.expect(rep, "org/r", engine.Updated); r.Preserved != 0 || v.refs("org/r", "refs/archive") != "" {
		t.Errorf("preserved despite preserve=false: %+v", r)
	}
}

func TestDefaultBranchChange(t *testing.T) {
	for _, layout := range []string{archive.LayoutWorktree, archive.LayoutBare} {
		t.Run(layout, func(t *testing.T) {
			v := newEnv(t, layout)
			up := v.srv.NewRepo("org/r")
			v.addOwner("org", nil)
			v.sync()
			up.Commit("trunk", "T", "t")
			up.SetHead("trunk")
			up.DeleteBranch("main")
			rep := v.sync()
			r := v.expect(rep, "org/r", engine.Updated)
			if !strings.Contains(strings.Join(r.Notes, ";"), "trunk") {
				t.Errorf("notes = %v", r.Notes)
			}
			head := testutil.Git(t, "", "--git-dir="+v.gitDir("org/r"), "symbolic-ref", "HEAD")
			if head != "refs/heads/trunk" {
				t.Errorf("HEAD = %s", head)
			}
			if layout == archive.LayoutWorktree {
				if _, err := os.Stat(filepath.Join(v.dir("org/r"), "T")); err != nil {
					t.Error("worktree not switched to trunk")
				}
			}
		})
	}
}

func TestLocalModificationsAreKept(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	up := v.srv.NewRepo("org/r")
	v.addOwner("org", nil)
	v.sync()
	readme := filepath.Join(v.dir("org/r"), "README")
	os.WriteFile(readme, []byte("my notes\n"), 0o644)
	newTip := up.Commit("main", "README", "upstream\n")
	rep := v.sync()
	r := v.expect(rep, "org/r", engine.Updated)
	if len(r.Warnings) == 0 || !strings.Contains(r.Warnings[0], "local modifications") {
		t.Errorf("warnings = %v", r.Warnings)
	}
	if got := readFile(t, readme); got != "my notes\n" {
		t.Errorf("local change overwritten: %q", got)
	}
	if got := testutil.Git(t, v.dir("org/r"), "rev-parse", "origin/main"); got != newTip {
		t.Error("upstream history not fetched")
	}
}

func TestExclusionsAndFilters(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t, archive.LayoutWorktree)
	v.srv.NewRepo("org/keep")
	v.srv.NewRepo("org/skip-me")
	v.srv.NewRepo("org/fork")
	v.srv.NewRepo("org/old")
	v.srv.NewRepo("org/huge")
	v.srv.Meta("org/fork").Fork = true
	v.srv.Meta("org/old").Archived = true
	v.srv.Meta("org/huge").Size = 5 << 20 // KiB: 5 GiB
	v.addOwner("org", map[string]string{"skipForks": "true", "skipArchived": "true", "maxSize": "1G"})
	if err := v.a.Config.AddExclude(ctx, "skip-*"); err != nil {
		t.Fatal(err)
	}
	rep := v.sync()
	v.expect(rep, "org/keep", engine.Cloned)
	v.expect(rep, "org/skip-me", engine.Excluded)
	v.expect(rep, "org/fork", engine.Filtered)
	v.expect(rep, "org/old", engine.Filtered)
	if r := v.expect(rep, "org/huge", engine.Filtered); !strings.Contains(r.Reason, "maxSize") {
		t.Errorf("reason = %q", r.Reason)
	}
	for _, k := range []string{"org/skip-me", "org/fork", "org/old", "org/huge"} {
		if _, err := os.Stat(v.dir(k)); err == nil {
			t.Errorf("%s was cloned", k)
		}
	}
	// Excluding an archived repository stops updates but keeps it.
	if err := v.a.Config.AddExclude(ctx, "org/keep"); err != nil {
		t.Fatal(err)
	}
	rep = v.sync()
	v.expect(rep, "org/keep", engine.Excluded)
	if _, err := os.Stat(v.dir("org/keep")); err != nil {
		t.Error("excluded repository was removed")
	}
	// --only restricts a run.
	if err := v.a.Config.RemoveExclude(ctx, "org/keep"); err != nil {
		t.Fatal(err)
	}
	rep = v.sync("keep")
	if len(rep.Results) != 1 || rep.Results[0].ID != v.id("org/keep") {
		t.Errorf("--only results: %+v", rep.Results)
	}
}

func TestGoneAndRename(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	v.srv.NewRepo("org/doomed")
	v.srv.NewRepo("org/before")
	v.addOwner("org", nil)
	v.sync()
	marker := filepath.Join(v.gitDir("org/before"), "marker")
	os.WriteFile(marker, []byte("x"), 0o644)

	v.srv.Delete("org/doomed")
	v.srv.Rename("org/before", "org/after")
	rep := v.sync()
	g := v.expect(rep, "org/doomed", engine.Gone)
	if !strings.Contains(g.Reason, "kept") {
		t.Errorf("gone reason = %q", g.Reason)
	}
	if _, err := os.Stat(v.dir("org/doomed")); err != nil {
		t.Error("gone repository was deleted locally")
	}
	if v.a.State.Repos[v.id("org/doomed")].Gone.IsZero() {
		t.Error("gone not recorded")
	}
	r := v.expect(rep, "org/after", engine.Updated)
	if !strings.Contains(strings.Join(r.Notes, ";"), "renamed") {
		t.Errorf("notes = %v", r.Notes)
	}
	if _, err := os.Stat(filepath.Join(v.gitDir("org/after"), "marker")); err != nil {
		t.Error("renamed repository was cloned again instead of moved")
	}
	if _, ok := v.a.State.Repos[v.id("org/before")]; ok {
		t.Error("old ID still in state")
	}
	url := testutil.Git(t, v.dir("org/after"), "config", "remote.origin.url")
	if !strings.HasSuffix(url, "/org/after.git") {
		t.Errorf("remote url = %s", url)
	}
	// Back again: no longer gone.
	v.srv.NewRepo("org/doomed")
	rep = v.sync()
	if r := v.result(rep, "org/doomed"); r.Outcome != engine.Updated && r.Outcome != engine.Unchanged {
		t.Errorf("reappeared repository: %s %s", r.Outcome, r.Error)
	}
	if !v.a.State.Repos[v.id("org/doomed")].Gone.IsZero() {
		t.Error("gone flag not cleared")
	}
}

func TestListingFailureKeepsKnownRepositories(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	up := v.srv.NewRepo("org/r")
	v.addOwner("org", nil)
	v.sync()
	up.Commit("main", "x", "y")
	v.srv.FailAPI = 100
	rep := v.sync()
	v.expect(rep, "org/r", engine.Updated)
	if len(rep.Warnings) == 0 || rep.Sources[0].Error == "" {
		t.Errorf("listing failure not reported: %+v", rep.Sources)
	}
	if !v.a.State.Repos[v.id("org/r")].Gone.IsZero() {
		t.Error("repository marked gone after a failed listing")
	}
}

func TestWiki(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	v.srv.NewRepo("org/with")
	v.srv.NewRepo("org/with.wiki")
	v.srv.NewRepo("org/without")
	v.srv.Meta("org/with").HasWiki = true
	v.srv.Meta("org/without").HasWiki = true // enabled, but never created
	v.addOwner("org", map[string]string{"wiki": "true"})
	rep := v.sync()
	v.expect(rep, "org/with.wiki", engine.Cloned)
	v.expect(rep, "org/without.wiki", engine.Absent)
	if rep.Failed() {
		t.Error("absent wiki counted as failure")
	}
	rep = v.sync()
	if r := v.expect(rep, "org/without.wiki", engine.Absent); r.Action != engine.ActSkip {
		t.Error("absent wiki probed again within the recheck interval")
	}
	v.expect(rep, "org/with.wiki", engine.Unchanged)
}

func TestMaliciousRepositoryIsRejected(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	up := v.srv.NewRepo("org/evil")
	up.PlantMaliciousTree("main")
	v.addOwner("org", nil)
	rep := v.sync()
	r := v.expect(rep, "org/evil", engine.Failed)
	if !strings.Contains(r.Error, ".git") && !strings.Contains(strings.ToLower(r.Error), "fsck") {
		t.Errorf("error = %q", r.Error)
	}
	if r.Attempts != 1 {
		t.Errorf("integrity failure retried %d times", r.Attempts)
	}
	matches, _ := filepath.Glob(filepath.Join(v.a.Root, "*", "org", "*"))
	if len(matches) != 0 {
		t.Errorf("leftovers after rejected clone: %v", matches)
	}
}

func TestUnsafeNamesFromAPI(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	v.srv.NewRepo("org/fine")
	v.srv.Extra = []map[string]any{
		{"id": 1, "full_name": "org/../../escape", "default_branch": "main"},
		{"id": 2, "full_name": "other/hijack", "default_branch": "main"},
		{"id": 3, "full_name": "org/-oops", "default_branch": "main"},
	}
	v.addOwner("org", nil)
	rep := v.sync()
	v.expect(rep, "org/fine", engine.Cloned)
	if len(rep.Results) != 1 || len(rep.Sources[0].Skipped) != 3 {
		t.Fatalf("results=%d skipped=%v", len(rep.Results), rep.Sources[0].Skipped)
	}
	entries, _ := os.ReadDir(v.a.Root)
	for _, e := range entries {
		if e.Name() != archive.MetaDir && e.Name() != safe.HostDir(v.srv.Host()) {
			t.Errorf("unexpected entry in archive root: %s", e.Name())
		}
	}
}

func TestRetryAndPartialCleanup(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	v.srv.NewRepo("org/flaky")
	v.addOwner("org", nil)
	// Leftover of an interrupted earlier run.
	partial := v.dir("org/flaky") + archive.PartialSuffix
	os.MkdirAll(filepath.Join(partial, "junk"), 0o755)
	v.srv.FailGit["org/flaky"] = 1
	rep := v.sync()
	r := v.expect(rep, "org/flaky", engine.Cloned)
	if r.Attempts != 2 {
		t.Errorf("attempts = %d", r.Attempts)
	}
	if _, err := os.Stat(partial); err == nil {
		t.Error("partial directory left behind")
	}
	v.srv.FailGit["org/flaky"] = 10
	rep = v.sync()
	if r := v.expect(rep, "org/flaky", engine.Failed); r.Attempts != 2 || !strings.Contains(r.Error, "502") {
		t.Errorf("result = %+v", r)
	}
	if v.a.State.Repos[v.id("org/flaky")].LastError == "" {
		t.Error("error not recorded in state")
	}
}

func TestEmptyRepository(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	bare := v.srv.NewEmptyRepo("org/empty")
	v.addOwner("org", nil)
	v.expect(v.sync(), "org/empty", engine.Cloned)
	v.expect(v.sync(), "org/empty", engine.Unchanged)
	// First content arrives.
	work := filepath.Join(t.TempDir(), "w")
	testutil.Git(t, "", "clone", "-q", bare, work)
	os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644)
	testutil.Git(t, work, "add", "f")
	testutil.Git(t, work, "commit", "-q", "-m", "first")
	testutil.Git(t, work, "push", "-q", "origin", "HEAD:main")
	v.expect(v.sync(), "org/empty", engine.Updated)
	if _, err := os.Stat(filepath.Join(v.dir("org/empty"), "f")); err != nil {
		t.Error("worktree not populated after first push")
	}
}

func TestDryRun(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	v.srv.NewRepo("org/a")
	v.addOwner("org", nil)
	rep, err := v.e.Sync(context.Background(), engine.SyncOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if r := v.expect(rep, "org/a", engine.Planned); r.Action != engine.ActClone {
		t.Errorf("action = %s", r.Action)
	}
	if _, err := os.Stat(v.dir("org/a")); err == nil {
		t.Error("dry run cloned")
	}
}

func TestVerifyAndGC(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t, archive.LayoutWorktree)
	up := v.srv.NewRepo("org/good")
	v.srv.NewRepo("org/bad")
	v.addOwner("org", nil)
	v.sync()

	rep, err := v.e.Verify(ctx, engine.VerifyOptions{Remote: true})
	if err != nil {
		t.Fatal(err)
	}
	if r := v.expect(rep, "org/good", engine.Verified); !strings.Contains(strings.Join(r.Notes, ";"), "in sync") {
		t.Errorf("notes = %v warnings = %v", r.Notes, r.Warnings)
	}

	// Corrupt an object of org/bad: overwrite pack data.
	packs, _ := filepath.Glob(filepath.Join(v.gitDir("org/bad"), "objects", "pack", "*.pack"))
	loose, _ := filepath.Glob(filepath.Join(v.gitDir("org/bad"), "objects", "??", "*"))
	targets := append(packs, loose...)
	if len(targets) == 0 {
		t.Fatal("no objects to corrupt")
	}
	for _, p := range targets {
		os.Chmod(p, 0o644)
		data, _ := os.ReadFile(p)
		for i := len(data) / 2; i < len(data)/2+16 && i < len(data); i++ {
			data[i] ^= 0xff
		}
		os.WriteFile(p, data, 0o644)
	}
	up.Commit("main", "new", "x")
	os.WriteFile(filepath.Join(v.dir("org/good"), "README"), []byte("tampered"), 0o644)

	rep, err = v.e.Verify(ctx, engine.VerifyOptions{Remote: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Failed() {
		t.Fatal("corruption not detected")
	}
	v.expect(rep, "org/bad", engine.Failed)
	good := v.expect(rep, "org/good", engine.Verified)
	w := strings.Join(good.Warnings, ";")
	if !strings.Contains(w, "modified in the working tree") || !strings.Contains(w, "differs from upstream") {
		t.Errorf("warnings = %v", good.Warnings)
	}
	if v.a.State.Repos[v.id("org/bad")].VerifyError == "" {
		t.Error("verify error not recorded")
	}
	entries, _ := v.e.Inventory(nil)
	for _, en := range entries {
		if en.ID == v.id("org/bad") && en.Status != "corrupt" {
			t.Errorf("inventory status = %s", en.Status)
		}
	}

	os.MkdirAll(v.dir("org/x")+archive.PartialSuffix, 0o755)
	rep, err = v.e.GC(ctx, engine.GCOptions{Only: []string{"good"}})
	if err != nil {
		t.Fatal(err)
	}
	v.expect(rep, "org/good", engine.Maintained)
	if _, err := os.Stat(v.dir("org/x") + archive.PartialSuffix); err == nil {
		t.Error("gc kept a partial clone")
	}
}

func TestInventoryAndJSON(t *testing.T) {
	v := newEnv(t, archive.LayoutBare)
	v.srv.NewRepo("org/a")
	v.addOwner("org", nil)
	rep := v.sync()
	if _, err := json.Marshal(rep); err != nil {
		t.Fatal(err)
	}
	unmanaged := filepath.Join(v.a.Root, "example.com", "x", "y.git")
	testutil.Git(t, "", "init", "-q", "--bare", unmanaged)
	entries, partials := v.e.Inventory(nil)
	if len(entries) != 2 || len(partials) != 0 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[1].ID != "example.com/x/y" || entries[1].Status != "unmanaged" || entries[0].Status != "ok" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestLFSEnabled(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	up := v.srv.NewRepo("org/l")
	v.addOwner("org", map[string]string{"lfs": "true"})
	r := v.expect(v.sync(), "org/l", engine.Cloned)
	if _, err := exec.LookPath("git-lfs"); err != nil {
		if len(r.Warnings) == 0 {
			t.Error("missing git-lfs not reported")
		}
		return
	}
	if len(r.Warnings) != 0 {
		t.Errorf("warnings = %v", r.Warnings)
	}
	up.Commit("main", "x", "y")
	v.expect(v.sync(), "org/l", engine.Updated)
}

func TestWikiFollowsRepositoryExclusion(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	v.srv.NewRepo("org/r")
	v.srv.NewRepo("org/r.wiki")
	v.srv.Meta("org/r").HasWiki = true
	v.addOwner("org", map[string]string{"wiki": "true"})
	if err := v.a.Config.AddExclude(context.Background(), "org/r"); err != nil {
		t.Fatal(err)
	}
	rep := v.sync()
	v.expect(rep, "org/r", engine.Excluded)
	v.expect(rep, "org/r.wiki", engine.Excluded)
}

func TestPreserveStampCollision(t *testing.T) {
	v := newEnv(t, archive.LayoutBare)
	up := v.srv.NewRepo("org/r")
	v.addOwner("org", nil)
	v.sync()
	first, _ := up.Rewrite("main")
	v.sync()
	v.clock = v.clock.Add(-time.Minute) // the next sync reuses the same timestamp
	second, _ := up.Rewrite("main")
	r := v.expect(v.sync(), "org/r", engine.Updated)
	if r.Preserved != 1 {
		t.Fatalf("preserved = %d (%s)", r.Preserved, r.Error)
	}
	refs := v.refs("org/r", "refs/archive")
	if !strings.Contains(refs, first) || !strings.Contains(refs, second) || !strings.Contains(refs, "Z-1/heads/main") {
		t.Errorf("refs/archive:\n%s", refs)
	}
}

func TestInterruptedSyncRecordsNoFailure(t *testing.T) {
	v := newEnv(t, archive.LayoutWorktree)
	v.srv.NewRepo("org/r")
	v.addOwner("org", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := v.e.Sync(ctx, engine.SyncOptions{})
	if err == nil {
		t.Fatal("cancelled sync reported success")
	}
	for _, r := range rep.Results {
		if r.Outcome == engine.Failed {
			t.Errorf("%s failed: %s", r.ID, r.Error)
		}
	}
	if rs := v.a.State.Repos[v.id("org/r")]; rs != nil && rs.LastError != "" {
		t.Errorf("state error = %q", rs.LastError)
	}
}
