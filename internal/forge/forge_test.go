package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseSpec(t *testing.T) {
	cases := []struct {
		arg, provider string
		insecure      bool
		want          Spec // Notes ignored
	}{
		{"http://github.com/termux", "", false,
			Spec{Provider: GitHub, Scheme: "https", Host: "github.com", Path: "termux", Type: TypeOwner, URL: "https://github.com/termux"}},
		{"https://github.com/termux/termux-packages/", "", false,
			Spec{Provider: GitHub, Scheme: "https", Host: "github.com", Path: "termux/termux-packages", Type: TypeRepo, URL: "https://github.com/termux/termux-packages"}},
		{"https://GitHub.com/termux/termux-app.git", "", false,
			Spec{Provider: GitHub, Scheme: "https", Host: "github.com", Path: "termux/termux-app", Type: TypeRepo, URL: "https://github.com/termux/termux-app"}},
		{"termux", GitHub, false,
			Spec{Provider: GitHub, Scheme: "https", Host: "github.com", Path: "termux", Type: TypeOwner, URL: "https://github.com/termux"}},
		{"termux/termux-app", GitHub, false,
			Spec{Provider: GitHub, Scheme: "https", Host: "github.com", Path: "termux/termux-app", Type: TypeRepo, URL: "https://github.com/termux/termux-app"}},
		{"github.com/sylirre", "", false,
			Spec{Provider: GitHub, Scheme: "https", Host: "github.com", Path: "sylirre", Type: TypeOwner, URL: "https://github.com/sylirre"}},
		{"https://gitlab.com/group/sub/proj/-/tree/main", "", false,
			Spec{Provider: GitLab, Scheme: "https", Host: "gitlab.com", Path: "group/sub/proj", URL: "https://gitlab.com/group/sub/proj"}},
		{"forgejo", Gitea, false,
			Spec{Provider: Gitea, Scheme: "https", Host: "codeberg.org", Path: "forgejo", Type: TypeOwner, URL: "https://codeberg.org/forgejo"}},
		{"https://git.example.com/org", Gitea, false,
			Spec{Provider: Gitea, Scheme: "https", Host: "git.example.com", Path: "org", Type: TypeOwner, URL: "https://git.example.com/org"}},
		{"https://git.kernel.org/pub/scm/git/git.git", "", false,
			Spec{Provider: Git, Scheme: "https", Host: "git.kernel.org", Path: "pub/scm/git/git", Type: TypeRepo, URL: "https://git.kernel.org/pub/scm/git/git.git"}},
		{"git@github.com:termux/termux-app.git", "", false,
			Spec{Provider: GitHub, Scheme: "ssh", Host: "github.com", Path: "termux/termux-app", Type: TypeRepo, URL: "git@github.com:termux/termux-app.git"}},
		{"ssh://git@example.com:2222/a/b.git", "", false,
			Spec{Provider: Git, Scheme: "ssh", Host: "example.com:2222", Path: "a/b", Type: TypeRepo, URL: "ssh://git@example.com:2222/a/b.git"}},
		{"http://127.0.0.1:8080/org", GitHub, true,
			Spec{Provider: GitHub, Scheme: "http", Host: "127.0.0.1:8080", Path: "org", Type: TypeOwner, URL: "http://127.0.0.1:8080/org", Insecure: true}},
		{"git://example.org/x/y.git", "", true,
			Spec{Provider: Git, Scheme: "git", Host: "example.org", Path: "x/y", Type: TypeRepo, URL: "git://example.org/x/y.git", Insecure: true}},
		{"example.com:8443/a/b", "", false,
			Spec{Provider: Git, Scheme: "https", Host: "example.com:8443", Path: "a/b", Type: TypeRepo, URL: "https://example.com:8443/a/b"}},
	}
	for _, c := range cases {
		got, err := ParseSpec(c.arg, c.provider, c.insecure)
		got.Notes = nil
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseSpec(%q, %q) =\n  %+v, %v\nwant\n  %+v", c.arg, c.provider, got, err, c.want)
		}
	}
	bad := []struct{ arg, provider string }{
		{"", ""},
		{"termux", ""},                              // no host
		{"http://example.com/a/b", ""},              // plaintext to unknown host
		{"git://example.com/a/b", ""},               // insecure transport
		{"https://user:pw@github.com/termux", ""},   // credentials in URL
		{"https://github.com/termux?tab=repos", ""}, // query
		{"https://github.com/a/b/tree/main", ""},    // inside a repository
		{"https://github.com/../etc", ""},           // traversal
		{"https://github.com/a/.git", ""},           // reserved
		{"https://github.com/-rf", ""},              // option-like
		{"ext::sh -c id", ""},                       // transport helper
		{"ext::sh", ""},                             // transport helper without spaces
		{"ftp://example.com/a/b", ""},               // scheme
		{"https://unknown.example/owner", ""},       // owner on unknown host
		{"termux", "bitbucket"},                     // unknown provider
		{"https://github.com/a\x1b[31m", ""},        // control characters
		{"/nonexistent/path/for/test", ""},          // local path missing
		{"https://exa_mple.com/a/b", ""},            // bad host
		{"-oProxyCommand=evil@host:a/b", ""},        // ssh option injection
		{"https://github.com/%2e%2e/x", ""},         // encoded traversal
		{"https://github.com/a%2Fb/c", ""},          // encoded slash
	}
	for _, b := range bad {
		if s, err := ParseSpec(b.arg, b.provider, false); err == nil {
			t.Errorf("ParseSpec(%q, %q) accepted: %+v", b.arg, b.provider, s)
		}
	}
	s, _ := ParseSpec("http://github.com/termux", "", false)
	if len(s.Notes) == 0 {
		t.Error("http upgrade not noted")
	}
}

func TestToken(t *testing.T) {
	env := map[string]string{"GITHUB_TOKEN": "gh", "GITLAB_TOKEN": "gl", "MINE": " m "}
	getenv := func(k string) string { return env[k] }
	if tok := Token(Spec{Host: "github.com"}, getenv); tok != "gh" {
		t.Errorf("github token = %q", tok)
	}
	if tok := Token(Spec{Host: "github.example.com", Provider: GitHub}, getenv); tok != "" {
		t.Errorf("github.com token offered to another host: %q", tok)
	}
	if tok := Token(Spec{Host: "github.example.com", TokenEnv: "MINE"}, getenv); tok != "m" {
		t.Errorf("tokenEnv token = %q", tok)
	}
}

func TestCloneURL(t *testing.T) {
	s := Spec{Host: "github.com", Scheme: "https"}
	if u := CloneURL(s, "a/b", "https"); u != "https://github.com/a/b.git" {
		t.Error(u)
	}
	if u := CloneURL(s, "a/b", "ssh"); u != "git@github.com:a/b.git" {
		t.Error(u)
	}
	if u := CloneURL(Spec{Host: "h:2222"}, "a/b", "ssh"); u != "ssh://git@h:2222/a/b.git" {
		t.Error(u)
	}
	if u := CloneURL(Spec{Host: "h", Scheme: "http"}, "a/b", "https"); u != "https://h/a/b.git" {
		t.Error("http used without insecure:", u)
	}
}

// fakeForge serves canned JSON per request path (and query when the key
// contains '?').
type fakeForge struct {
	t      *testing.T
	routes map[string]func(w http.ResponseWriter, r *http.Request)
	hits   atomic.Int32
	auth   atomic.Value
}

func (f *fakeForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	f.auth.Store(r.Header.Get("Authorization"))
	key := r.URL.EscapedPath()
	if h, ok := f.routes[key+"?"+r.URL.RawQuery]; ok {
		h(w, r)
		return
	}
	if h, ok := f.routes[key]; ok {
		h(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(404)
	fmt.Fprint(w, `{"message":"Not Found"}`)
}

func jsonReply(body string, hdr ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], strings.ReplaceAll(hdr[i+1], "HOST", r.Host))
		}
		fmt.Fprint(w, body)
	}
}

func newFake(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request)) (*fakeForge, *httptest.Server) {
	f := &fakeForge{t: t, routes: routes}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func spec(t *testing.T, srv *httptest.Server, provider, path, apiPrefix string) Spec {
	t.Helper()
	s, err := ParseSpec(srv.URL+"/"+path, provider, true)
	if err != nil {
		t.Fatal(err)
	}
	s.API = srv.URL + apiPrefix
	return s
}

func client(t *testing.T, s Spec, env map[string]string) Client {
	t.Helper()
	c, err := NewClient(s, Options{Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	if a := apiOf(c); a != nil {
		a.sleep = func(context.Context, time.Duration) error { return nil }
	}
	return c
}

func apiOf(c Client) *api {
	switch v := c.(type) {
	case *github:
		return v.api
	case *gitlab:
		return v.api
	case *gitea:
		return v.api
	}
	return nil
}

func TestGitHubListPaginationAndValidation(t *testing.T) {
	f, srv := newFake(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v3/users/Termux": jsonReply(`{"login":"termux","type":"Organization"}`),
		"/api/v3/orgs/termux/repos?per_page=100&type=all": jsonReply(`[
			{"id":1,"full_name":"termux/termux-app","default_branch":"master","size":2048,"has_wiki":true,
			 "description":"App \u001b[31mred","license":{"spdx_id":"GPL-3.0"},"topics":["android"],"stargazers_count":5},
			{"id":2,"full_name":"termux/../../etc","default_branch":"main"},
			{"id":3,"full_name":"evil/elsewhere","default_branch":"main"}
		]`, "Link", `<http://HOST/api/v3/orgs/termux/repos?page=2&per_page=100&type=all>; rel="next", <http://HOST/x>; rel="last"`),
		"/api/v3/orgs/termux/repos?page=2&per_page=100&type=all": jsonReply(`[
			{"id":4,"full_name":"termux/termux-packages","fork":true,"archived":true,"default_branch":"master"},
			{"id":1,"full_name":"termux/termux-app","default_branch":"master"}
		]`),
	})
	s := spec(t, srv, GitHub, "Termux", "/api/v3")
	c := client(t, s, map[string]string{"GITHUB_TOKEN": "secret"})
	l, err := c.List(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Repos) != 2 || len(l.Skipped) != 2 {
		t.Fatalf("repos=%+v skipped=%v", l.Repos, l.Skipped)
	}
	app := l.Repos[0]
	host := strings.TrimPrefix(srv.URL, "http://")
	if app.ID != host+"/termux/termux-app" || app.CloneURL != srv.URL+"/termux/termux-app.git" ||
		app.WikiURL != srv.URL+"/termux/termux-app.wiki.git" || app.Size != 2<<20 || app.ForgeID != "1" ||
		app.Meta.License != "GPL-3.0" || app.Meta.Stars != 5 {
		t.Errorf("app = %+v meta=%+v", app, app.Meta)
	}
	if pk := l.Repos[1]; !pk.Fork || !pk.Archived {
		t.Errorf("packages = %+v", pk)
	}
	// The GITHUB_TOKEN variable belongs to github.com only.
	if a := f.auth.Load(); a != "" {
		t.Errorf("token sent to a non-github.com host: %q", a)
	}
}

func TestGitHubTokenAndResolve(t *testing.T) {
	f, srv := newFake(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v3/repos/Termux/Termux-App": jsonReply(`{"id":9,"full_name":"termux/termux-app","default_branch":"master"}`),
	})
	s := spec(t, srv, GitHub, "Termux/Termux-App", "/api/v3")
	s.TokenEnv = "TOK"
	c := client(t, s, map[string]string{"TOK": "abc"})
	r, err := c.Resolve(context.Background(), s)
	if err != nil || r.Path != "termux/termux-app" || r.Type != TypeRepo {
		t.Fatalf("Resolve = %+v, %v", r, err)
	}
	if a := f.auth.Load(); a != "Bearer abc" {
		t.Errorf("Authorization = %q", a)
	}
	s2 := spec(t, srv, GitHub, "nobody", "/api/v3")
	if _, err := client(t, s2, nil).Resolve(context.Background(), s2); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRedirectAndPaginationOriginChecks(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached foreign host: %s (auth %q)", r.URL, r.Header.Get("Authorization"))
	}))
	defer other.Close()
	_, srv := newFake(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v3/users/redir": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
		},
		"/api/v3/users/pages":                               jsonReply(`{"login":"pages","type":"User"}`),
		"/api/v3/users/pages/repos?per_page=100&type=owner": jsonReply(`[]`, "Link", "<"+other.URL+`/next>; rel="next"`),
	})
	s := spec(t, srv, GitHub, "redir", "/api/v3")
	s.TokenEnv = "T"
	if _, err := client(t, s, map[string]string{"T": "x"}).Resolve(context.Background(), s); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("redirect err = %v", err)
	}
	s2 := spec(t, srv, GitHub, "pages", "/api/v3")
	if _, err := client(t, s2, nil).List(context.Background(), s2); err == nil || !strings.Contains(err.Error(), "pagination") {
		t.Errorf("pagination err = %v", err)
	}
}

func TestRateLimitAndRetries(t *testing.T) {
	var calls atomic.Int32
	_, srv := newFake(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v3/users/limited": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
		},
		"/api/v3/users/flaky": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if calls.Add(1) < 3 {
				w.WriteHeader(502)
				return
			}
			fmt.Fprint(w, `{"login":"flaky","type":"User"}`)
		},
		"/api/v3/users/big": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"login":"`+strings.Repeat("a", maxBody)+`"}`)
		},
		"/api/v3/users/html": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html>captive portal</html>`)
		},
	})
	s := spec(t, srv, GitHub, "limited", "/api/v3")
	_, err := client(t, s, nil).Resolve(context.Background(), s)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Reset.IsZero() || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("rate limit err = %v", err)
	}
	s = spec(t, srv, GitHub, "flaky", "/api/v3")
	if _, err := client(t, s, nil).Resolve(context.Background(), s); err != nil || calls.Load() != 3 {
		t.Errorf("retry: err=%v calls=%d", err, calls.Load())
	}
	s = spec(t, srv, GitHub, "big", "/api/v3")
	if _, err := client(t, s, nil).Resolve(context.Background(), s); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Errorf("size limit err = %v", err)
	}
	s = spec(t, srv, GitHub, "html", "/api/v3")
	if _, err := client(t, s, nil).Resolve(context.Background(), s); err == nil || !strings.Contains(err.Error(), "content type") {
		t.Errorf("content type err = %v", err)
	}
}

func TestGitLab(t *testing.T) {
	_, srv := newFake(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v4/groups/grp%2Fsub":       jsonReply(`{"id":7,"full_path":"grp/sub"}`),
		"/api/v4/projects/grp%2Fsub%2Fp": jsonReply(`{"id":70,"path_with_namespace":"grp/sub/p","default_branch":"main","visibility":"public","wiki_access_level":"enabled"}`),
		"/api/v4/groups/7/projects?include_subgroups=true&order_by=id&per_page=100&sort=asc&with_shared=false": jsonReply(`[
			{"id":70,"path_with_namespace":"grp/sub/p","default_branch":"main","visibility":"private","forked_from_project":{"id":1},
			 "statistics":{"repository_size":1000},"tag_list":["t"]},
			{"id":71,"path_with_namespace":"grp/sub/deeper/q","empty_repo":true,"wiki_enabled":true},
			{"id":72,"path_with_namespace":"other/shared"}
		]`),
		"/api/v4/users?username=alice":                               jsonReply(`[{"id":5,"username":"alice"}]`),
		"/api/v4/users/5/projects?order_by=id&per_page=100&sort=asc": jsonReply(`[{"id":80,"path_with_namespace":"alice/dots"}]`),
	})
	ctx := context.Background()
	s := spec(t, srv, GitLab, "grp/sub", "/api/v4")
	c := client(t, s, nil)
	r, err := c.Resolve(ctx, s)
	if err != nil || r.Type != TypeOwner || r.Path != "grp/sub" {
		t.Fatalf("Resolve group = %+v, %v", r, err)
	}
	l, err := c.List(ctx, r)
	if err != nil || len(l.Repos) != 2 || len(l.Skipped) != 1 {
		t.Fatalf("List = %+v, %v", l, err)
	}
	p := l.Repos[0]
	if !p.Fork || !p.Private || p.Size != 1000 || p.WikiURL != "" || p.Meta.Topics[0] != "t" {
		t.Errorf("project = %+v", p)
	}
	if q := l.Repos[1]; !q.Empty || q.WikiURL == "" {
		t.Errorf("deeper project = %+v", q)
	}
	s = spec(t, srv, GitLab, "grp/sub/p", "/api/v4")
	r, err = c.Resolve(ctx, s)
	if err != nil || r.Type != TypeRepo {
		t.Errorf("Resolve project = %+v, %v", r, err)
	}
	s = spec(t, srv, GitLab, "alice", "/api/v4")
	if l, err := c.List(ctx, s); err != nil || len(l.Repos) != 1 {
		t.Errorf("user list = %+v, %v", l, err)
	}
}

func TestGitea(t *testing.T) {
	_, srv := newFake(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/users/bob":                       jsonReply(`{"login":"bob"}`),
		"/api/v1/users/bob/repos?limit=50&page=1": jsonReply(`[` + strings.TrimSuffix(strings.Repeat(`{"id":1,"full_name":"bob/r1","size":1},`, 1), ",") + `,` + giteaPage(2, 50) + `]`),
		"/api/v1/users/bob/repos?limit=50&page=2": jsonReply(`[{"id":99,"full_name":"bob/last","has_wiki":true,"empty":true}]`),
	})
	s := spec(t, srv, Gitea, "bob", "/api/v1")
	c := client(t, s, nil)
	l, err := c.List(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Repos) != 51 || l.Repos[50].Path != "bob/last" || !l.Repos[50].Empty || l.Repos[50].WikiURL == "" {
		t.Fatalf("got %d repos, last %+v", len(l.Repos), l.Repos[len(l.Repos)-1])
	}
}

func giteaPage(start, n int) string {
	var parts []string
	for i := start; i < start+n-1; i++ {
		parts = append(parts, fmt.Sprintf(`{"id":%d,"full_name":"bob/r%d"}`, i, i))
	}
	return strings.Join(parts, ",")
}

func TestGeneric(t *testing.T) {
	s, err := ParseSpec("https://git.example.com/a/b.git", "", false)
	if err != nil {
		t.Fatal(err)
	}
	c := client(t, s, nil)
	r, err := c.Get(context.Background(), s)
	if err != nil || r.ID != "git.example.com/a/b" || r.CloneURL != "https://git.example.com/a/b.git" {
		t.Errorf("Get = %+v, %v", r, err)
	}
	if _, err := c.List(context.Background(), s); err == nil {
		t.Error("generic List succeeded")
	}
}
