// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package safe

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestName(t *testing.T) {
	good := []string{"termux", "termux-packages", ".github", "a", "A_b.c-d", "x.git.bak", strings.Repeat("a", MaxNameLen)}
	for _, s := range good {
		if err := Name(s); err != nil {
			t.Errorf("Name(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{
		"", ".", "..", ".git", ".GIT", ".Git", "-rf", "--upload-pack=x",
		"a/b", `a\b`, "a b", "a\x00b", "a\nb", "\x1b[31m", "ü", "a:b", "~x", "$(id)",
		"a;b", "*", strings.Repeat("a", MaxNameLen+1),
	}
	for _, s := range bad {
		if err := Name(s); err == nil {
			t.Errorf("Name(%q) = nil, want error", s)
		}
	}
}

func TestRelPath(t *testing.T) {
	for _, s := range []string{"o/r", "group/sub/project", "x"} {
		if err := RelPath(s); err != nil {
			t.Errorf("RelPath(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", "/abs", "o/", "o//r", "o/../r", "o/./r", "../x", "o/.git", "o/r/"} {
		if err := RelPath(s); err == nil {
			t.Errorf("RelPath(%q) = nil, want error", s)
		}
	}
}

func TestHost(t *testing.T) {
	for _, h := range []string{"github.com", "gitlab.example.org", "127.0.0.1:8080", "localhost", "a-b.c"} {
		if err := Host(h); err != nil {
			t.Errorf("Host(%q) = %v", h, err)
		}
	}
	for _, h := range []string{"", "GitHub.com", "-a.com", "a-.com", "a..com", ".a", "a.com:", "a.com:x", "a.com:123456",
		"a_b.com", "user@host", "a.com/x", "[::1]", strings.Repeat("a", 64) + ".com"} {
		if err := Host(h); err == nil {
			t.Errorf("Host(%q) = nil, want error", h)
		}
	}
	if got := HostDir("example.com:8443"); got != "example.com_8443" {
		t.Errorf("HostDir = %q", got)
	}
}

func TestWithinAndJoin(t *testing.T) {
	root := filepath.FromSlash("/srv/archive")
	cases := map[string]bool{
		"/srv/archive":            true,
		"/srv/archive/a":          true,
		"/srv/archive/a/../b":     true,
		"/srv/archive/../x":       false,
		"/srv/archive2":           false,
		"/srv":                    false,
		"/srv/archive/..a/b":      true,
		"/srv/archive/a/../../..": false,
	}
	for p, want := range cases {
		if got := Within(root, filepath.FromSlash(p)); got != want {
			t.Errorf("Within(%q) = %v, want %v", p, got, want)
		}
	}
	p, err := Join(root, "github.com/termux/termux-app")
	if err != nil || p != filepath.FromSlash("/srv/archive/github.com/termux/termux-app") {
		t.Errorf("Join = %q, %v", p, err)
	}
	for _, rel := range []string{"../etc", "a/../../etc", "/etc", "", "a/./b"} {
		if _, err := Join(root, rel); err == nil {
			t.Errorf("Join(%q) succeeded", rel)
		}
	}
}

func TestTerm(t *testing.T) {
	cases := map[string]string{
		"plain text":                   "plain text",
		"tab\there":                    "tab here",
		"\x1b[2J\x1b[Hclear":           "?[2J?[Hclear",
		"osc\x1b]52;c;ZXZpbA==\x07":    "osc?]52;c;ZXZpbA==?",
		"c1\u009b31m":                  "c1?31m",
		"bidi‮gnp.exe":                 "bidi?gnp.exe",
		"isolate⁦x⁩":                   "isolate?x?",
		"bad utf8 \xff\xfe":            "bad utf8 ??",
		"del\x7f":                      "del?",
		"newline\nkept?":               "newline?kept?",
		"unicode Привет ✓":             "unicode Привет ✓",
		"zero​width":                   "zero?width",
		"line sep":                     "line?sep",
		"cr\roverwrite":                "cr?overwrite",
		"bell\a":                       "bell?",
		"backspace\b\b\b":              "backspace???",
		"nul\x00":                      "nul?",
		"hyperlink \x1b]8;;http://x\a": "hyperlink ?]8;;http://x?",
	}
	for in, want := range cases {
		if got := Term(in); got != want {
			t.Errorf("Term(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTermLine(t *testing.T) {
	got := TermLine("fatal: one\r\nremote: two\x1b[K\n\n  three  \r")
	want := "fatal: one | remote: two?[K | three"
	if got != want {
		t.Errorf("TermLine = %q, want %q", got, want)
	}
}

func FuzzTerm(f *testing.F) {
	f.Add("\x1b[31mred\x1b[0m")
	f.Add("‮\xff\x9b")
	f.Fuzz(func(t *testing.T, s string) {
		out := Term(s)
		for _, r := range out {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || isBidiControl(r) {
				t.Fatalf("Term(%q) leaked control rune %U", s, r)
			}
		}
	})
}

func FuzzName(f *testing.F) {
	f.Add("termux")
	f.Add("..")
	f.Fuzz(func(t *testing.T, s string) {
		if Name(s) != nil {
			return
		}
		root := filepath.FromSlash("/r")
		p := filepath.Join(root, s)
		if filepath.Dir(p) != root || strings.ContainsAny(s, "/\\\x00") {
			t.Fatalf("accepted name %q escapes or contains separators", s)
		}
	})
}
