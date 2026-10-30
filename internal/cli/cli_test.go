package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sylirre/git-archive/internal/archive"
	"github.com/sylirre/git-archive/internal/engine"
	"github.com/sylirre/git-archive/internal/gitx"
	"github.com/sylirre/git-archive/internal/testutil"
)

type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, dir string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), args, Env{Stdout: &out, Stderr: &errb, Environ: testutil.GitEnv(), Dir: dir, NoSignals: true})
	return result{code, out.String(), errb.String()}
}

func (r result) must(t *testing.T, code int) result {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", r.code, code, r.stdout, r.stderr)
	}
	return r
}

func TestFlagParsing(t *testing.T) {
	var (
		b    bool
		s    string
		list []string
		n    int
	)
	fs := &flagSet{}
	fs.boolVar(&b, "quiet", 'q', "")
	fs.stringVar(&s, "archive-dir", 'C', "DIR", "")
	fs.listVar(&list, "exclude", 'x', "P", "")
	fs.intVar(&n, "jobs", 'j', "N", "")
	pos, err := fs.parse([]string{"a", "-qC", "dir", "--exclude=x", "-xy", "-j8", "b", "--", "--quiet", "-x"})
	if err != nil {
		t.Fatal(err)
	}
	if !b || s != "dir" || !reflect.DeepEqual(list, []string{"x", "y"}) || n != 8 ||
		!reflect.DeepEqual(pos, []string{"a", "b", "--quiet", "-x"}) {
		t.Errorf("b=%v s=%q list=%v n=%d pos=%v", b, s, list, n, pos)
	}
	for _, bad := range [][]string{{"--quite"}, {"--quiet=1"}, {"-C"}, {"--jobs", "x"}, {"-z"}} {
		if _, err := fs.parse(bad); err == nil {
			t.Errorf("parse(%v) succeeded", bad)
		}
	}
	if _, err := fs.parse([]string{"--quite"}); !strings.Contains(err.Error(), "did you mean --quiet") {
		t.Errorf("no suggestion: %v", err)
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		args          []string
		name          string
		before, after []string
	}{
		{nil, "sync", nil, nil},
		{[]string{"https://x/y"}, "sync", []string{"https://x/y"}, nil},
		{[]string{"-C", "verify", "list", "-q"}, "list", []string{"-C", "verify"}, []string{"-q"}},
		{[]string{"--provider", "github", "list"}, "list", []string{"--provider", "github"}, []string{}},
		{[]string{"-q", "--", "verify"}, "sync", []string{"-q", "--", "verify"}, nil},
		{[]string{"config", "--get", "x"}, "config", []string{}, []string{"--get", "x"}},
	}
	for _, c := range cases {
		name, before, after := splitCommand(c.args)
		if name != c.name || len(before) != len(c.before) || len(after) != len(c.after) {
			t.Errorf("splitCommand(%v) = %s %v %v", c.args, name, before, after)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	dir := t.TempDir()
	r := run(t, dir, "--frobnicate").must(t, ExitUsage)
	if !strings.Contains(r.stderr, "unknown option --frobnicate") || !strings.Contains(r.stderr, "git-archive help") {
		t.Errorf("stderr = %q", r.stderr)
	}
	run(t, dir, "completion").must(t, ExitUsage)
	run(t, dir, "completion", "tcsh").must(t, ExitUsage)
	run(t, dir, "help", "nope").must(t, ExitUsage)
	run(t, dir, "-q", "-v").must(t, ExitUsage)
	r = run(t, dir).must(t, ExitFailure)
	if !strings.Contains(r.stderr, "not a git-archive") {
		t.Errorf("stderr = %q", r.stderr)
	}
	r = run(t, dir, "termux").must(t, ExitUsage)
	if !strings.Contains(r.stderr, "cannot tell where") {
		t.Errorf("stderr = %q", r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, archive.MetaDir)); err == nil {
		t.Error("an archive was left behind after a failed first run")
	}
}

func TestHelpAndVersion(t *testing.T) {
	r := run(t, "", "--help").must(t, ExitOK)
	for _, want := range []string{"Usage:", "Commands:", "verify", "--archive-dir", "--skip-forks", "GITHUB_TOKEN"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	for _, c := range commands {
		r := run(t, "", "help", c.name).must(t, ExitOK)
		if !strings.Contains(r.stdout, "git-archive") {
			t.Errorf("help %s: %q", c.name, r.stdout)
		}
	}
	if r := run(t, "", "verify", "--help").must(t, ExitOK); !strings.Contains(r.stdout, "--remote") {
		t.Errorf("verify --help: %s", r.stdout)
	}
	if r := run(t, "", "--version").must(t, ExitOK); !strings.HasPrefix(r.stdout, "git-archive ") {
		t.Errorf("version: %q", r.stdout)
	}
}

func TestCompletionScriptsParse(t *testing.T) {
	for shell, check := range map[string][]string{"bash": {"bash", "-n"}, "zsh": {"zsh", "-n"}, "fish": {"fish", "--no-execute"}} {
		r := run(t, "", "completion", shell).must(t, ExitOK)
		if !strings.Contains(r.stdout, "archive-dir") || !strings.Contains(r.stdout, "verify") {
			t.Errorf("%s completion incomplete", shell)
		}
		if _, err := exec.LookPath(check[0]); err != nil {
			continue
		}
		f := filepath.Join(t.TempDir(), "c."+shell)
		os.WriteFile(f, []byte(r.stdout), 0o644)
		if out, err := exec.Command(check[0], append(check[1:], f)...).CombinedOutput(); err != nil {
			t.Errorf("%s rejects its completion script: %v\n%s", shell, err, out)
		}
	}
	// The bash script must actually complete.
	if _, err := exec.LookPath("bash"); err == nil {
		r := run(t, "", "completion", "bash")
		script := r.stdout + `
COMP_WORDS=(git-archive ver); COMP_CWORD=1; _git_archive; echo "${COMPREPLY[@]}"
COMP_WORDS=(git-archive verify --re); COMP_CWORD=2; _git_archive; echo "${COMPREPLY[@]}"
COMP_WORDS=(git-archive --prov); COMP_CWORD=1; _git_archive; echo "${COMPREPLY[@]}"
`
		out, err := exec.Command("bash", "-c", script).CombinedOutput()
		if err != nil || string(out) != "verify version\n--remote\n--provider\n" {
			t.Errorf("bash completion output %q, %v", out, err)
		}
	}
}

// forgeEnv is a fake GitHub with an organisation "org".
func forgeEnv(t *testing.T) (*testutil.Server, []string) {
	srv := testutil.NewServer(t)
	srv.NewRepo("org/alpha")
	srv.NewRepo("org/beta")
	srv.Meta("org/beta").Fork = true
	return srv, []string{"--provider", "github", "--insecure", "--api", srv.URL + "/api/v3"}
}

func TestEndToEnd(t *testing.T) {
	srv, flags := forgeEnv(t)
	dir := t.TempDir()
	id := func(name string) string { return srv.Host() + "/org/" + name }

	// Refuse to litter a non-empty directory.
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)
	r := run(t, dir, append(flags, srv.URL+"/org")...).must(t, ExitFailure)
	if !strings.Contains(r.stderr, "--init") {
		t.Errorf("stderr = %q", r.stderr)
	}

	r = run(t, dir, append(append(flags, "--init", "--skip-forks", "--json"), srv.URL+"/org")...).must(t, ExitOK)
	var rep engine.Report
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("JSON report: %v\n%s", err, r.stdout)
	}
	if rep.Counts[engine.Cloned] != 1 || rep.Counts[engine.Filtered] != 1 {
		t.Errorf("counts = %v", rep.Counts)
	}
	if !strings.Contains(r.stderr, "Added source") || !strings.Contains(r.stderr, "Synced in") {
		t.Errorf("stderr = %s", r.stderr)
	}

	// Quiet update prints nothing.
	if r := run(t, dir, "-q").must(t, ExitOK); r.stdout != "" || r.stderr != "" {
		t.Errorf("quiet output: %q %q", r.stdout, r.stderr)
	}

	// Exclude, list, include.
	run(t, dir, "exclude", "alpha").must(t, ExitOK)
	r = run(t, dir, "list", "--json").must(t, ExitOK)
	var entries []engine.Entry
	if err := json.Unmarshal([]byte(r.stdout), &entries); err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, e := range entries {
		status[e.ID] = e.Status
	}
	if status[id("alpha")] != "excluded" || status[id("beta")] != "filtered" {
		t.Errorf("status = %v", status)
	}
	run(t, dir, "include", "alpha").must(t, ExitOK)
	if r := run(t, dir, "config", "--get-all", "archive.exclude"); r.code == 0 {
		t.Errorf("exclusions left: %q", r.stdout)
	}
	r = run(t, dir, "list").must(t, ExitOK)
	if !strings.Contains(r.stdout, "ok") || !strings.Contains(r.stdout, id("alpha")) {
		t.Errorf("list:\n%s", r.stdout)
	}
	r = run(t, dir, "list", "--sources").must(t, ExitOK)
	if !strings.Contains(r.stdout, srv.Host()+"/org") || !strings.Contains(r.stdout, "skipForks=true") {
		t.Errorf("sources:\n%s", r.stdout)
	}

	// Verify, then corrupt and verify again.
	run(t, dir, "verify", "--quick").must(t, ExitOK)
	objects, _ := filepath.Glob(filepath.Join(dir, "*", "org", "alpha", ".git", "objects", "pack", "*.pack"))
	for _, p := range objects {
		os.Chmod(p, 0o644)
		os.WriteFile(p, []byte("garbage"), 0o644)
	}
	if len(objects) > 0 {
		run(t, dir, "verify").must(t, ExitFailure)
	}

	// Config edits are validated.
	run(t, dir, "config", "archive.jobs", "0").must(t, ExitFailure)
	run(t, dir, "config", "archive.jobs", "2").must(t, ExitOK)

	// Removing the source keeps repositories.
	run(t, dir, "remove", srv.URL+"/org").must(t, ExitOK)
	r = run(t, dir).must(t, ExitFailure)
	if !strings.Contains(r.stderr, "no sources") {
		t.Errorf("stderr = %q", r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, strings.ReplaceAll(srv.Host(), ":", "_"), "org", "alpha")); err != nil {
		t.Error("repository removed with its source")
	}
}

func TestDryRunCreatesNothing(t *testing.T) {
	srv, flags := forgeEnv(t)
	dir := filepath.Join(t.TempDir(), "new")
	r := run(t, dir, append(append(flags, "--dry-run"), srv.URL+"/org")...).must(t, ExitOK)
	if !strings.Contains(r.stderr, "Plan (dry run") || !strings.Contains(r.stderr, "Would clone 2") {
		t.Errorf("stderr = %s", r.stderr)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("dry run created the archive directory")
	}
}

func TestLockContention(t *testing.T) {
	srv, flags := forgeEnv(t)
	dir := t.TempDir()
	run(t, dir, append(flags, srv.URL+"/org/alpha")...).must(t, ExitOK)
	arc, err := archive.Open(context.Background(), mustRunner(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := arc.Lock(); err != nil {
		t.Fatal(err)
	}
	defer arc.Close()
	r := run(t, dir).must(t, ExitFailure)
	if !strings.Contains(r.stderr, "in use by another git-archive process") {
		t.Errorf("stderr = %q", r.stderr)
	}
	// Read-only commands still work.
	run(t, dir, "list").must(t, ExitOK)
}

func TestGitArchiveDirEnv(t *testing.T) {
	srv, flags := forgeEnv(t)
	dir := t.TempDir()
	run(t, dir, append(flags, srv.URL+"/org/alpha")...).must(t, ExitOK)
	var out, errb bytes.Buffer
	env := append(testutil.GitEnv(), "GIT_ARCHIVE_DIR="+dir)
	code := Run(context.Background(), []string{"list"}, Env{Stdout: &out, Stderr: &errb, Environ: env, Dir: t.TempDir(), NoSignals: true})
	if code != 0 || !strings.Contains(out.String(), "alpha") {
		t.Errorf("GIT_ARCHIVE_DIR ignored: %d %s %s", code, out.String(), errb.String())
	}
}

func mustRunner(t *testing.T) *gitx.Runner {
	t.Helper()
	r, err := gitx.New(gitx.Options{Environ: testutil.GitEnv()})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestCommittedCompletions keeps contrib/completion in sync with the
// generator; packagers install those files without running the binary
// (which may be cross-compiled). Regenerate with "make completions".
func TestCommittedCompletions(t *testing.T) {
	files := map[string]string{"bash": "git-archive.bash", "zsh": "_git-archive", "fish": "git-archive.fish"}
	for shell, name := range files {
		want, _ := completionScript(shell)
		got, err := os.ReadFile(filepath.Join("..", "..", "contrib", "completion", name))
		if err != nil || string(got) != want {
			t.Errorf("contrib/completion/%s is stale; run make completions", name)
		}
	}
}
