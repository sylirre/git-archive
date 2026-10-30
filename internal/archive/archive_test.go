package archive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylirre/git-archive/internal/gitx"
)

func TestMatchPattern(t *testing.T) {
	id := "github.com/termux/termux-packages"
	yes := []string{
		"termux-packages", "termux/termux-packages", "github.com/termux/termux-packages", "termux", "github.com",
		"github.com/termux", "*-packages", "termux/*", "*/termux/*", "**/termux-packages", "github.com/**",
		"/github.com/termux", "/github.com", "termux/term?x-*", "[st]ermux",
	}
	no := []string{
		"termux-app", "termux/termux-app", "gitlab.com/termux", "/termux", "packages", "termux/termux-packages/x",
		"github.com/termux/termux-packages/**/x", "sylirre/*",
	}
	for _, p := range yes {
		if !MatchPattern(p, id) {
			t.Errorf("MatchPattern(%q, %q) = false", p, id)
		}
	}
	for _, p := range no {
		if MatchPattern(p, id) {
			t.Errorf("MatchPattern(%q, %q) = true", p, id)
		}
	}
	if !MatchPattern("group/**/proj", "gitlab.com/group/a/b/proj") || !MatchPattern("group/**/proj", "gitlab.com/group/proj") {
		t.Error("** does not span components")
	}
}

func TestExcluded(t *testing.T) {
	list := []string{"termux", "!termux/termux-app", "*-legacy"}
	cases := map[string]bool{
		"github.com/termux/termux-packages": true,
		"github.com/termux/termux-app":      false,
		"github.com/sylirre/foo":            false,
		"github.com/sylirre/foo-legacy":     true,
		"github.com/termux/termux-legacy":   true,
	}
	for id, want := range cases {
		if got, _ := Excluded(list, id); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", id, got, want)
		}
	}
	if _, by := Excluded(list, "github.com/termux/termux-app"); by != "!termux/termux-app" {
		t.Errorf("deciding pattern = %q", by)
	}
}

func TestValidatePattern(t *testing.T) {
	for _, p := range []string{"a", "a/b", "/a", "*", "**/x", "[ab]c"} {
		if err := ValidatePattern(p); err != nil {
			t.Errorf("ValidatePattern(%q) = %v", p, err)
		}
	}
	for _, p := range []string{"", "/", "a//b", "a b", "a\tb", "[", "a/[x", "a\x1b"} {
		if err := ValidatePattern(p); err == nil {
			t.Errorf("ValidatePattern(%q) accepted", p)
		}
	}
}

func runner(t *testing.T) *gitx.Runner {
	t.Helper()
	r, err := gitx.New(gitx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCreateOpenAndConfigRoundTrip(t *testing.T) {
	ctx := context.Background()
	git := runner(t)
	root := t.TempDir()
	a, err := Create(ctx, git, root, LayoutBare)
	if err != nil {
		t.Fatal(err)
	}
	if !IsArchive(root) || a.Config.Layout != LayoutBare || a.Config.Jobs != DefaultJobs {
		t.Fatalf("unexpected new archive: %+v", a.Config)
	}
	if _, err := Create(ctx, git, root, LayoutBare); err == nil {
		t.Fatal("Create over an existing archive succeeded")
	}

	src := &Source{Name: "github.com/termux", URL: "https://github.com/termux", Provider: "github", Type: "owner",
		Opts: map[string]string{"skipForks": "true", "maxSize": "2G"}}
	if err := a.Config.SaveSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	if err := a.Config.SaveSource(ctx, &Source{Name: "example.com:8443/a/b", URL: "https://example.com:8443/a/b.git",
		Provider: "git", Type: "repo", Insecure: true, TokenEnv: "MY_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"termux-packages", "!termux-packages", "*-legacy"} {
		if err := a.Config.AddExclude(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Config.AddExclude(ctx, "termux-packages"); err != nil { // moves to the end
		t.Fatal(err)
	}
	if got := strings.Join(a.Config.Excludes, ","); got != "!termux-packages,*-legacy,termux-packages" {
		t.Errorf("excludes = %s", got)
	}
	if err := a.Config.RemoveExclude(ctx, "!termux-packages"); err != nil {
		t.Fatal(err)
	}

	// Hand edits with comments must survive programmatic changes.
	data, _ := os.ReadFile(a.Config.Path())
	data = append(data, []byte("# my note\n[archive]\n\tjobs = 7 ; inline\n\twiki\n")...)
	if err := os.WriteFile(a.Config.Path(), data, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := Open(ctx, git, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Config.AddExclude(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(b.Config.Path())
	if !strings.Contains(string(data), "# my note") || !strings.Contains(string(data), "git-archive configuration") {
		t.Errorf("comments lost:\n%s", data)
	}
	c := b.Config
	if c.Jobs != 7 || len(c.Sources) != 2 || strings.Join(c.Excludes, ",") != "*-legacy,termux-packages,x" {
		t.Fatalf("reloaded config: jobs=%d sources=%d excludes=%v", c.Jobs, len(c.Sources), c.Excludes)
	}
	s := c.Source("github.com/termux")
	o, err := c.Options(s)
	if err != nil || !o.SkipForks || o.MaxSize != 2<<30 || !o.Wiki || !o.Preserve || o.Protocol != "https" {
		t.Errorf("options = %+v, %v", o, err)
	}
	s2 := c.Source("example.com:8443/a/b")
	if s2 == nil || !s2.Insecure || s2.TokenEnv != "MY_TOKEN" || s2.Provider != "git" {
		t.Errorf("source 2 = %+v", s2)
	}
	if err := c.RemoveSource(ctx, "example.com:8443/a/b"); err != nil || len(c.Sources) != 1 {
		t.Errorf("RemoveSource: %v, %d sources", err, len(c.Sources))
	}
}

func TestConfigValidation(t *testing.T) {
	ctx := context.Background()
	git := runner(t)
	bad := map[string]string{
		"layout":   "[archive]\n\tlayout = fancy\n",
		"jobs":     "[archive]\n\tjobs = 0\n",
		"version":  "[archive]\n\tversion = 2\n",
		"bool":     "[archive]\n\twiki = maybe\n",
		"size":     "[source \"a.com/b\"]\n\turl = x\n\tprovider = git\n\ttype = repo\n\tmaxSize = big\n",
		"type":     "[source \"a.com/b\"]\n\ttype = org\n",
		"missing":  "[source \"a.com/b\"]\n\turl = x\n",
		"exclude":  "[archive]\n\texclude = \"a b\"\n",
		"tokenenv": "[source \"a.com/b\"]\n\ttokenEnv = \"$(id)\"\n",
		"protocol": "[archive]\n\tprotocol = ftp\n",
	}
	for name, content := range bad {
		p := filepath.Join(t.TempDir(), "config")
		os.WriteFile(p, []byte(content), 0o644)
		if _, err := LoadConfig(ctx, git, p); err == nil {
			t.Errorf("%s: invalid config accepted", name)
		}
	}
	p := filepath.Join(t.TempDir(), "config")
	os.WriteFile(p, []byte("[archive]\n\tfrobnicate = 1\n[include]\n\tpath = /etc/passwd\n"), 0o644)
	c, err := LoadConfig(ctx, git, p)
	if err != nil || len(c.Warnings) != 2 {
		t.Errorf("warnings = %v, err = %v", c.Warnings, err)
	}
}

func TestStateSaveLoadAndCorruption(t *testing.T) {
	ctx := context.Background()
	git := runner(t)
	root := t.TempDir()
	a, err := Create(ctx, git, root, LayoutWorktree)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	a.Update(func(s *State) {
		r := s.Repo("github.com/a/b")
		r.Source, r.URL, r.LastSync, r.Size = "github.com/a", "https://github.com/a/b.git", now, 42
		s.Source("github.com/a").LastList = now
	})
	if err := a.SaveState(); err != nil {
		t.Fatal(err)
	}
	b, err := Open(ctx, git, root)
	if err != nil {
		t.Fatal(err)
	}
	r := b.State.Repos["github.com/a/b"]
	if r == nil || !r.LastSync.Equal(now) || r.Size != 42 || !b.State.Sources["github.com/a"].LastList.Equal(now) {
		t.Fatalf("state round trip: %+v", r)
	}
	data, _ := os.ReadFile(filepath.Join(root, MetaDir, "state.json"))
	if strings.Contains(string(data), "0001-01-01") {
		t.Error("zero times are serialised")
	}
	os.WriteFile(filepath.Join(root, MetaDir, "state.json"), []byte("{garbage"), 0o644)
	c, err := Open(ctx, git, root)
	if err != nil || c.Notice == "" || len(c.State.Repos) != 0 {
		t.Fatalf("corrupt state: err=%v notice=%q", err, c.Notice)
	}
	matches, _ := filepath.Glob(filepath.Join(root, MetaDir, "state.json.corrupt-*"))
	if len(matches) != 1 {
		t.Errorf("corrupt state not moved aside: %v", matches)
	}
}

func TestLock(t *testing.T) {
	ctx := context.Background()
	git := runner(t)
	root := t.TempDir()
	a, err := Create(ctx, git, root, LayoutWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Lock(); err != nil {
		t.Fatal(err)
	}
	b, _ := Open(ctx, git, root)
	err = b.Lock()
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("second lock: %v", err)
	}
	a.Close()
	if err := b.Lock(); err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	b.Close()
}

func TestRepoDir(t *testing.T) {
	root := "/srv/a"
	cases := []struct {
		layout, id, want string
	}{
		{LayoutWorktree, "github.com/termux/termux-app", "/srv/a/github.com/termux/termux-app"},
		{LayoutBare, "github.com/termux/termux-app", "/srv/a/github.com/termux/termux-app.git"},
		{LayoutWorktree, "git.example.com:8443/g/s/p", "/srv/a/git.example.com_8443/g/s/p"},
	}
	for _, c := range cases {
		got, err := RepoDir(root, c.layout, c.id)
		if err != nil || got != filepath.FromSlash(c.want) {
			t.Errorf("RepoDir(%s, %s) = %q, %v", c.layout, c.id, got, err)
		}
	}
	for _, id := range []string{"github.com/../etc", "github.com", "GITHUB.COM/a/b", "github.com/a/.git", "../x/y"} {
		if _, err := RepoDir(root, LayoutWorktree, id); err == nil {
			t.Errorf("RepoDir(%q) accepted", id)
		}
	}
}
