// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylirre/git-archive/internal/engine"
	"github.com/sylirre/git-archive/internal/gitx"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func task(id string) *engine.Task { return &engine.Task{ID: id, Action: engine.ActClone} }

func TestPlainOutput(t *testing.T) {
	var out syncBuf
	u := New(Options{Out: &out, Mode: Plain, Jobs: 2})
	u.Phase("Syncing", 3)
	tk := task("github.com/o/a")
	u.Start(0, tk)
	u.Progress(0, tk, gitx.Progress{Phase: "Receiving objects", Percent: 50})
	u.Done(0, tk, &engine.Result{ID: tk.ID, Outcome: engine.Cloned, Duration: time.Second})
	u.Done(1, task("github.com/o/b"), &engine.Result{ID: "github.com/o/b", Outcome: engine.Unchanged})
	u.Done(1, task("github.com/o/c"), &engine.Result{ID: "github.com/o/c\x1b]52;c;x\x07", Outcome: engine.Failed,
		Error: "fatal: \x1b[2Jboom", Warnings: []string{"careful\x1b[31m"}})
	u.Message(engine.Warn, "listing failed")
	u.Summary(&engine.Report{Command: "sync", Counts: map[engine.Outcome]int{engine.Cloned: 1, engine.Unchanged: 1, engine.Failed: 1},
		Results: []*engine.Result{{ID: "github.com/o/c", Outcome: engine.Failed, Error: "boom"}}})
	u.Close()
	got := out.String()
	for _, want := range []string{"Syncing 3 repositories with 2 jobs", "cloned", "github.com/o/a", "failed",
		"warning: listing failed", "Synced in", "1 cloned", "1 failed", "Failed (1):"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "unchanged     github.com/o/b") || strings.Contains(got, "\x1b") {
		t.Errorf("unexpected content (unchanged line or escape):\n%q", got)
	}
}

func TestQuietOutput(t *testing.T) {
	var out syncBuf
	u := New(Options{Out: &out, Mode: Quiet})
	u.Phase("Syncing", 2)
	u.Done(0, task("a/b/c"), &engine.Result{ID: "a/b/c", Outcome: engine.Cloned})
	u.Message(engine.Info, "removed something")
	u.Summary(&engine.Report{Command: "sync", Counts: map[engine.Outcome]int{engine.Cloned: 1}})
	u.Close()
	if out.String() != "" {
		t.Errorf("quiet mode printed:\n%s", out.String())
	}
	u = New(Options{Out: &out, Mode: Quiet})
	u.Done(0, task("a/b/d"), &engine.Result{ID: "a/b/d", Outcome: engine.Failed, Error: "x"})
	u.Close()
	if !strings.Contains(out.String(), "failed") {
		t.Errorf("quiet mode hid a failure: %q", out.String())
	}
}

func TestLiveRedraw(t *testing.T) {
	var out syncBuf
	u := New(Options{Out: &out, Mode: Live, Jobs: 2, Color: true, Unicode: true, Width: func() int { return 60 },
		Interval: 5 * time.Millisecond})
	u.Phase("Syncing", 2)
	tk := task("github.com/owner/a-very-long-repository-name-that-needs-truncation")
	u.Start(1, tk)
	u.Progress(1, tk, gitx.Progress{Phase: "Receiving objects", Percent: 42, Detail: "1.0 MiB | 2.0 MiB/s"})
	time.Sleep(30 * time.Millisecond)
	u.Done(1, tk, &engine.Result{ID: tk.ID, Outcome: engine.Cloned})
	time.Sleep(30 * time.Millisecond)
	u.Close()
	got := out.String()
	if !strings.Contains(got, "\x1b[?25l") || !strings.HasSuffix(got, "\x1b[?25h") {
		t.Error("cursor not hidden and restored")
	}
	if !strings.Contains(got, "receiving objects") || !strings.Contains(got, " 42%") {
		t.Errorf("progress not drawn:\n%q", got)
	}
	// No line of the live block (they start with a space; scrolled log
	// lines do not) may exceed the width: wrapping would corrupt redraws.
	for _, frame := range strings.Split(got, "\x1b[J") {
		for _, line := range strings.Split(frame, "\n") {
			if line = stripControl(line); strings.HasPrefix(line, " ") && visibleLen(line) > 59 {
				t.Errorf("line too wide (%d): %q", visibleLen(line), line)
			}
		}
	}
}

func stripControl(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\x1b[?25l", "")
	s = strings.ReplaceAll(s, "\x1b[?25h", "")
	return s
}

func TestCutAndBar(t *testing.T) {
	s := style{color: true, unicode: true}
	colored := s.good("abcdef") + "ghij"
	if got := cut(colored, 4); visibleLen(got) != 4 || !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("cut = %q", got)
	}
	for _, f := range []float64{0, 0.33, 0.5, 1, 2, -1} {
		if n := visibleLen(s.bar(f, 10)); n != 10 {
			t.Errorf("bar(%v) width %d", f, n)
		}
		if n := visibleLen(style{}.bar(f, 10)); n != 12 {
			t.Errorf("ascii bar(%v) width %d", f, n)
		}
	}
	if truncate("abcdef", 4) != "abc…" || pad("ab", 4) != "ab  " {
		t.Error("truncate/pad")
	}
}

func TestDryRunPlan(t *testing.T) {
	var out syncBuf
	u := New(Options{Out: &out, Mode: Plain})
	u.Summary(&engine.Report{Command: "sync", DryRun: true, Results: []*engine.Result{
		{ID: "h/o/a", Action: engine.ActClone, Outcome: engine.Planned},
		{ID: "h/o/b", Action: engine.ActSkip, Outcome: engine.Excluded, Reason: "excluded by pattern b"},
		{ID: "h/o/c", Action: engine.ActSkip, Outcome: engine.Gone, Reason: "no longer available upstream"},
	}})
	got := out.String()
	for _, want := range []string{"clone   h/o/a", "skip    h/o/b", "keep    h/o/c", "Would clone 1, update 0, skip 1; 1 gone upstream."} {
		if !strings.Contains(got, want) {
			t.Errorf("plan lacks %q:\n%s", want, got)
		}
	}
}
