// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests guard licensing metadata across the repository: they
// fail when a new source file lacks its SPDX header or when a new
// dependency is linked into the binary without a third-party notice.

const repoRoot = "../.."

func TestSPDXHeaders(t *testing.T) {
	want := []string{
		"SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>",
		"SPDX-License-Identifier: Apache-2.0",
	}
	extra := []string{"Makefile", ".gitignore", "go.mod", "doc/git-archive.1",
		"contrib/completion/git-archive.bash", "contrib/completion/_git-archive", "contrib/completion/git-archive.fish"}
	var files []string
	err := filepath.WalkDir(repoRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range extra {
		files = append(files, filepath.Join(repoRoot, f))
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Error(err)
			continue
		}
		head := strings.Join(strings.SplitN(string(data), "\n", 5)[:min(4, strings.Count(string(data), "\n"))], "\n")
		for _, w := range want {
			if !strings.Contains(head, w) {
				t.Errorf("%s: missing %q in the first lines", f, w)
			}
		}
	}
	lic, err := os.ReadFile(filepath.Join(repoRoot, "LICENSE"))
	if err != nil || !strings.Contains(string(lic), "Apache License\n                           Version 2.0, January 2004") {
		t.Errorf("LICENSE is not the Apache License 2.0 text (%v)", err)
	}
}

func TestThirdPartyNotices(t *testing.T) {
	notices, err := os.ReadFile(filepath.Join(repoRoot, "THIRD_PARTY_LICENSES"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	inRequire := false
	n := 0
	for sc := bufio.NewScanner(f); sc.Scan(); {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "require ("):
			inRequire = true
			continue
		case inRequire && line == ")":
			inRequire = false
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimPrefix(line, "require ")
		case !inRequire:
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		n++
		if entry := fields[0] + " " + fields[1]; !strings.Contains(string(notices), entry) {
			t.Errorf("THIRD_PARTY_LICENSES lacks %q; regenerate it after dependency changes", entry)
		}
	}
	if n == 0 {
		t.Error("no requirements parsed from go.mod")
	}
	if !strings.Contains(string(notices), "Go standard library and runtime") {
		t.Error("THIRD_PARTY_LICENSES lacks the Go runtime notice")
	}
}
