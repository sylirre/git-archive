// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseProgress(t *testing.T) {
	cases := []struct {
		in   string
		want Progress
	}{
		{"Receiving objects:  12% (123/1000), 1.00 MiB | 500.00 KiB/s",
			Progress{Phase: "Receiving objects", Percent: 12, Current: 123, Total: 1000, Detail: "1.00 MiB | 500.00 KiB/s"}},
		{"Receiving objects: 100% (1000/1000), 5.00 MiB | 2.00 MiB/s, done.",
			Progress{Phase: "Receiving objects", Percent: 100, Current: 1000, Total: 1000, Detail: "5.00 MiB | 2.00 MiB/s", Done: true}},
		{"remote: Enumerating objects: 1234, done.",
			Progress{Phase: "Enumerating objects", Percent: -1, Current: 1234, Remote: true, Done: true}},
		{"remote: Compressing objects:  45% (9/20)        ",
			Progress{Phase: "Compressing objects", Percent: 45, Current: 9, Total: 20, Remote: true}},
		{"Resolving deltas: 100% (5/5), done.",
			Progress{Phase: "Resolving deltas", Percent: 100, Current: 5, Total: 5, Done: true}},
		{"Updating files:  30% (3/10)",
			Progress{Phase: "Updating files", Percent: 30, Current: 3, Total: 10}},
		{"Checking connectivity: 4096, done.",
			Progress{Phase: "Checking connectivity", Percent: -1, Current: 4096, Done: true}},
		{"Downloading LFS objects:  50% (1/2), 1.2 MB | 3.1 MB/s",
			Progress{Phase: "Downloading LFS objects", Percent: 50, Current: 1, Total: 2, Detail: "1.2 MB | 3.1 MB/s"}},
	}
	for _, c := range cases {
		got, ok := ParseProgress(c.in)
		if !ok || got != c.want {
			t.Errorf("ParseProgress(%q) = %+v, %v; want %+v", c.in, got, ok, c.want)
		}
	}
	for _, s := range []string{
		"Cloning into 'x'...",
		"remote: Total 12 (delta 0), reused 0 (delta 0), pack-reused 0",
		"fatal: repository 'https://x/y' not found",
		"error: object 1234: hasDotgit: contains '.git'",
		"warning: You appear to have cloned an empty repository.",
		"",
	} {
		if p, ok := ParseProgress(s); ok {
			t.Errorf("ParseProgress(%q) = %+v, want no match", s, p)
		}
	}
}

func TestScanLines(t *testing.T) {
	var got []string
	in := "a\rb\r\nc\n\n" + strings.Repeat("x", maxLine+100) + "\rtail"
	scanLines(strings.NewReader(in), func(s string) { got = append(got, s) })
	if len(got) != 5 || got[0] != "a" || got[1] != "b" || got[2] != "c" || len(got[3]) != maxLine || got[4] != "tail" {
		t.Fatalf("scanLines: %d lines, %q", len(got), got[:3])
	}
}

func TestCleanEnv(t *testing.T) {
	env := cleanEnv([]string{
		"PATH=/bin", "HOME=/home/u", "GIT_DIR=/evil", "GIT_WORK_TREE=/evil", "GIT_CONFIG_PARAMETERS='core.x=y'",
		"GIT_CONFIG_COUNT=1", "GIT_SSH_COMMAND=ssh -i k", "GIT_TRACE_PACKET=1", "LANG=ru_RU.UTF-8",
		"LC_MESSAGES=ru_RU.UTF-8", "SSH_ASKPASS=/usr/bin/ksshaskpass", "GIT_ASKPASS=/x", "GIT_TEMPLATE_DIR=/t", "noequals",
	})
	has := func(kv string) bool { return slices.Contains(env, kv) }
	for _, kv := range []string{"PATH=/bin", "HOME=/home/u", "GIT_SSH_COMMAND=ssh -i k", "GIT_TRACE_PACKET=1",
		"LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GIT_LFS_SKIP_SMUDGE=1"} {
		if !has(kv) {
			t.Errorf("env lacks %q", kv)
		}
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "LANG", "LC_MESSAGES",
			"SSH_ASKPASS", "GIT_TEMPLATE_DIR", "noequals":
			t.Errorf("env leaks %q", kv)
		case "GIT_ASKPASS":
			if kv != "GIT_ASKPASS=" {
				t.Errorf("env leaks %q", kv)
			}
		}
	}
}

func TestConfigArgs(t *testing.T) {
	args := configArgs([]string{"https"}, []Credential{{URLPrefix: "https://example.com", Token: "s3cret"}})
	joined := strings.Join(args, "\n")
	for _, want := range []string{"protocol.allow=never", "protocol.https.allow=always", "transfer.fsckObjects=true",
		"core.hooksPath=/dev/null", "fetch.fsck.zeroPaddedFilemode=ignore", "credential.https://example.com.helper="} {
		if !strings.Contains(joined, want) {
			t.Errorf("config args lack %q", want)
		}
	}
	if strings.Contains(joined, "s3cret") {
		t.Error("token leaked into command line")
	}
	if strings.Contains(joined, "protocol.file.allow") || strings.Contains(joined, "protocol.ext.allow") {
		t.Error("unexpected protocol allowed")
	}
	for i := 0; i < len(args); i += 2 {
		if args[i] != "-c" {
			t.Fatalf("args[%d] = %q, want -c", i, args[i])
		}
	}
}

func newRunner(t *testing.T, o Options) *Runner {
	t.Helper()
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func gitOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRunAndVersion(t *testing.T) {
	r := newRunner(t, Options{})
	v, err := r.Version(context.Background())
	if err != nil || v[0] < 2 {
		t.Fatalf("Version = %v, %v", v, err)
	}
	dir := t.TempDir()
	gitOK(t, dir, "init", "-q", "-b", "main", "repo")
	gitOK(t, filepath.Join(dir, "repo"), "commit", "-q", "--allow-empty", "-m", "x")
	out, err := r.Output(context.Background(), dir, filepath.Join(dir, "repo", ".git"), "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || out != "main" {
		t.Fatalf("rev-parse = %q, %v", out, err)
	}
	// Hooks must never run even if a repository installs them.
	hook := filepath.Join(dir, "repo", ".git", "hooks", "post-commit")
	marker := filepath.Join(dir, "hook-ran")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(context.Background(), Cmd{Dir: filepath.Join(dir, "repo"), GitDir: filepath.Join(dir, "repo", ".git"),
		WorkTree: filepath.Join(dir, "repo"),
		Args:     []string{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "y"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("repository hook was executed")
	}
}

func TestRunErrorSummary(t *testing.T) {
	r := newRunner(t, Options{})
	_, err := r.Run(context.Background(), Cmd{Dir: t.TempDir(), Args: []string{"rev-parse", "--verify", "nope"}})
	var ge *Error
	if !errors.As(err, &ge) || ge.Exit == 0 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(ge.Error(), "git rev-parse") || ge.Transient() {
		t.Errorf("Error() = %q transient=%v", ge.Error(), ge.Transient())
	}
}

func TestProtocolRestriction(t *testing.T) {
	r := newRunner(t, Options{Protocols: []string{"https"}})
	dir := t.TempDir()
	gitOK(t, dir, "init", "-q", "--bare", "up.git")
	for _, url := range []string{"file://" + filepath.Join(dir, "up.git"), "ext::sh -c touch% " + filepath.Join(dir, "pwned")} {
		_, err := r.Run(context.Background(), Cmd{Dir: dir, Args: []string{"clone", "-q", "--", url, filepath.Join(dir, "c")}})
		if err == nil {
			t.Fatalf("clone of %q succeeded although protocol is not allowed", url)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Fatal("ext:: transport executed a command")
	}
	r2 := newRunner(t, Options{Protocols: []string{"file"}})
	if _, err := r2.Run(context.Background(), Cmd{Dir: dir, Args: []string{"clone", "-q", "--", "file://" + filepath.Join(dir, "up.git"), filepath.Join(dir, "c2")}}); err != nil {
		t.Fatalf("clone with file allowed: %v", err)
	}
}

func fakeGit(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-git")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIdleTimeoutKillsProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-alive")
	// The fake git spawns a child that would create a marker file later
	// unless the whole process group is killed.
	git := fakeGit(t, "(sleep 2; touch "+marker+") &\necho 'Receiving objects:  1% (1/100)' >&2\nsleep 30")
	r := newRunner(t, Options{Git: git, IdleTimeout: 500 * time.Millisecond})
	r.killWait = 200 * time.Millisecond
	var progress []Progress
	start := time.Now()
	_, err := r.Run(context.Background(), Cmd{Args: []string{"clone"}, Progress: func(p Progress) { progress = append(progress, p) }})
	var ge *Error
	if !errors.As(err, &ge) || !ge.Timeout || !ge.Transient() {
		t.Fatalf("err = %#v, want timeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took %v", time.Since(start))
	}
	if len(progress) != 1 || progress[0].Percent != 1 {
		t.Errorf("progress = %+v", progress)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("child process survived the process group kill")
	}
}

func TestContextCancel(t *testing.T) {
	r := newRunner(t, Options{Git: fakeGit(t, "sleep 30")})
	r.killWait = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := r.Run(ctx, Cmd{Args: []string{"fetch"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestNoControllingTerminal(t *testing.T) {
	// ssh and friends open /dev/tty to prompt; in our session that fails.
	r := newRunner(t, Options{Git: fakeGit(t, "if (exec 3>/dev/tty) 2>/dev/null; then echo tty; else echo notty; fi")})
	out, err := r.Output(context.Background(), "", "", "x")
	if err != nil || out != "notty" {
		t.Fatalf("out = %q, %v", out, err)
	}
}

func TestOutputLimit(t *testing.T) {
	r := newRunner(t, Options{Git: fakeGit(t, "head -c 100000 /dev/zero")})
	_, err := r.Run(context.Background(), Cmd{Args: []string{"x"}, MaxOutput: 1000})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v", err)
	}
}

func TestTransientClassification(t *testing.T) {
	cases := map[string]bool{
		"fatal: unable to access 'https://x/': Could not resolve host: x":     true,
		"error: RPC failed; curl 92 HTTP/2 stream 5 was not closed cleanly":   true,
		"fatal: the remote end hung up unexpectedly":                          true,
		"error: The requested URL returned error: 502":                        true,
		"fatal: repository 'https://github.com/a/b/' not found":               false,
		"error: object 0123: hasDotgit: contains '.git'\nfatal: early EOF":    false,
		"fatal: fsck error in packed object\nfatal: the remote end hung up":   false,
		"fatal: could not read Username for 'https://github.com': terminal p": false,
	}
	for text, want := range cases {
		e := &Error{Args: []string{"fetch"}, Exit: 128, Stderr: strings.Split(text, "\n")}
		if got := e.Transient(); got != want {
			t.Errorf("Transient(%q) = %v, want %v", text, got, want)
		}
	}
	nf := &Error{Exit: 128, Stderr: []string{"remote: Repository not found.", "fatal: repository 'x' not found"}}
	if !IsNotFound(nf) {
		t.Error("IsNotFound = false")
	}
}

func TestParseVersion(t *testing.T) {
	v, err := parseVersion("git version 2.43.0")
	if err != nil || v != [3]int{2, 43, 0} {
		t.Errorf("parseVersion = %v, %v", v, err)
	}
	v, err = parseVersion("git version 2.39.5 (Apple Git-154)")
	if err != nil || v != [3]int{2, 39, 5} {
		t.Errorf("parseVersion = %v, %v", v, err)
	}
	if _, err := parseVersion("nonsense"); err == nil {
		t.Error("parseVersion accepted nonsense")
	}
}
