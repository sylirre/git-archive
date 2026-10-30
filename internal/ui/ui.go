// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

// Package ui renders engine progress for humans.
//
// Three modes exist: Live redraws a small block at the bottom of a
// terminal — one line per busy worker with git's progress, plus an
// overall bar — while finished repositories scroll above it; Plain
// prints one line per notable event (for logs, pipes and cron); Quiet
// prints only problems. All text that originates outside the program is
// passed through safe.Term before it is written.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/sylirre/git-archive/internal/engine"
	"github.com/sylirre/git-archive/internal/gitx"
	"github.com/sylirre/git-archive/internal/safe"
	"github.com/sylirre/git-archive/internal/units"
)

// Mode selects how much is printed and how.
type Mode int

// Modes.
const (
	Live Mode = iota
	Plain
	Quiet
)

// Options configure a UI.
type Options struct {
	Out     io.Writer
	Mode    Mode
	Color   bool
	Unicode bool
	Verbose bool
	Jobs    int
	// Width returns the terminal width; nil uses the width of Out when
	// it is a terminal, else 100.
	Width func() int
	// Interval between redraws in Live mode.
	Interval time.Duration
}

// UI implements engine.Reporter.
type UI struct {
	o     Options
	sty   style
	mu    sync.Mutex
	slots []slot
	phase string
	total int
	done  int
	count map[engine.Outcome]int
	start time.Time
	frame int

	pending []string // log lines not yet printed (Live)
	drawn   int      // lines of the live block currently on screen
	stop    chan struct{}
	stopped chan struct{}
	closed  bool
}

type slot struct {
	task    *engine.Task
	step    string
	prog    gitx.Progress
	hasProg bool
	since   time.Time
}

// New creates a UI and, in Live mode, starts the redraw loop.
func New(o Options) *UI {
	if o.Out == nil {
		o.Out = os.Stderr
	}
	if o.Jobs < 1 {
		o.Jobs = 1
	}
	if o.Width == nil {
		o.Width = widthOf(o.Out)
	}
	if o.Interval == 0 {
		o.Interval = 100 * time.Millisecond
	}
	u := &UI{o: o, sty: newStyle(o.Color, o.Unicode), slots: make([]slot, o.Jobs), count: map[engine.Outcome]int{},
		start: time.Now(), stop: make(chan struct{}), stopped: make(chan struct{})}
	if o.Mode == Live {
		io.WriteString(o.Out, "\x1b[?25l") // hide cursor
		go u.loop()
	} else {
		close(u.stopped)
	}
	return u
}

func widthOf(w io.Writer) func() int {
	f, ok := w.(*os.File)
	return func() int {
		if ok {
			if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 20 {
				return w
			}
		}
		return 100
	}
}

// IsTerminal reports whether w is an interactive terminal.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// Close stops the redraw loop and restores the cursor. It is safe to
// call more than once.
func (u *UI) Close() {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return
	}
	u.closed = true
	u.mu.Unlock()
	if u.o.Mode == Live {
		close(u.stop)
		<-u.stopped
		u.mu.Lock()
		u.redraw(false)
		io.WriteString(u.o.Out, "\x1b[?25h")
		u.mu.Unlock()
	}
}

func (u *UI) loop() {
	defer close(u.stopped)
	t := time.NewTicker(u.o.Interval)
	defer t.Stop()
	for {
		select {
		case <-u.stop:
			return
		case <-t.C:
			u.mu.Lock()
			u.frame++
			u.redraw(true)
			u.mu.Unlock()
		}
	}
}

// Phase implements engine.Reporter.
func (u *UI) Phase(title string, total int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.phase, u.total, u.done = title, total, 0
	if u.o.Mode == Plain && total > 0 && title != "Listing sources" {
		u.emit(u.sty.dim(fmt.Sprintf("%s %d %s with %d %s", title, total, plural(total, "repository", "repositories"),
			min(u.o.Jobs, total), plural(min(u.o.Jobs, total), "job", "jobs"))))
	}
}

// Start implements engine.Reporter.
func (u *UI) Start(i int, t *engine.Task) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if i < len(u.slots) {
		u.slots[i] = slot{task: t, since: time.Now(), step: string(t.Action)}
	}
}

// Step implements engine.Reporter.
func (u *UI) Step(i int, t *engine.Task, step string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if i < len(u.slots) {
		u.slots[i].step, u.slots[i].hasProg = step, false
	}
	if strings.Contains(step, "retrying") && u.o.Mode != Quiet {
		u.emit(u.sty.warn(u.sty.sym("warn")) + " " + safe.Term(t.ID) + ": " + safe.Term(step))
	}
}

// Progress implements engine.Reporter.
func (u *UI) Progress(i int, t *engine.Task, p gitx.Progress) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if i < len(u.slots) {
		u.slots[i].prog, u.slots[i].hasProg = p, true
	}
}

// Done implements engine.Reporter.
func (u *UI) Done(i int, t *engine.Task, r *engine.Result) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if i < len(u.slots) {
		u.slots[i] = slot{}
	}
	u.done++
	u.count[r.Outcome]++
	for _, line := range u.resultLines(r) {
		u.emit(line)
	}
}

// Message implements engine.Reporter.
func (u *UI) Message(level engine.Level, text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	text = safe.TermLine(text)
	switch level {
	case engine.Error:
		u.emit(u.sty.bad("error:") + " " + text)
	case engine.Warn:
		u.emit(u.sty.warn("warning:") + " " + text)
	default:
		if u.o.Mode != Quiet {
			u.emit(text)
		}
	}
}

// Printf prints a line through the UI (above the live block).
func (u *UI) Printf(format string, args ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.emit(fmt.Sprintf(format, args...))
}

// emit queues (Live) or writes (Plain, Quiet) one log line. mu is held.
func (u *UI) emit(line string) {
	if u.o.Mode == Live && !u.closed {
		u.pending = append(u.pending, line)
		return
	}
	io.WriteString(u.o.Out, line+"\n")
}

// resultLines formats a finished task for the log.
func (u *UI) resultLines(r *engine.Result) []string {
	id := safe.Term(r.ID)
	s := u.sty
	var lines []string
	label := func(sym, word string, color func(string) string) string {
		return color(s.sym(sym)+" "+pad(word, 10)) + " " + id
	}
	notable := u.o.Verbose
	var line string
	switch r.Outcome {
	case engine.Cloned:
		line, notable = label("ok", "cloned", s.good)+s.dim("  "+units.FormatDuration(r.Duration)), notable || u.o.Mode != Quiet
	case engine.Updated:
		line, notable = label("upd", "updated", s.info)+s.dim("  "+refSummary(r)), notable || u.o.Mode != Quiet
	case engine.Unchanged:
		line = label("same", "unchanged", s.dim)
	case engine.Failed:
		line, notable = label("fail", "failed", s.bad)+"  "+safe.TermLine(r.Error), true
	case engine.Gone:
		line = label("gone", "gone", s.warn) + s.dim("  "+r.Reason)
		notable = notable || (len(r.Notes) > 0 && u.o.Mode != Quiet)
	case engine.Verified:
		line = label("ok", "ok", s.good)
	case engine.Maintained:
		line = label("ok", "gc", s.good) + s.dim("  freed "+units.FormatBytes(r.Freed))
	case engine.Interrupted:
		line = label("skip", "interrupted", s.dim)
	default:
		line = label("skip", string(r.Outcome), s.dim) + s.dim("  "+safe.Term(r.Reason))
	}
	if notable {
		lines = append(lines, line)
	}
	if notable && r.Outcome != engine.Gone {
		for _, n := range r.Notes {
			lines = append(lines, "    "+s.dim(safe.TermLine(n)))
		}
	}
	for _, w := range r.Warnings {
		lines = append(lines, s.warn(s.sym("warn")+" warning")+" "+id+": "+safe.TermLine(w))
	}
	return lines
}

func refSummary(r *engine.Result) string {
	var parts []string
	c := r.Refs
	if c.Added > 0 {
		parts = append(parts, fmt.Sprintf("%d new", c.Added))
	}
	if c.Updated > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", c.Updated))
	}
	if c.Forced > 0 {
		parts = append(parts, fmt.Sprintf("%d rewritten", c.Forced))
	}
	if c.Deleted > 0 {
		parts = append(parts, fmt.Sprintf("%d deleted", c.Deleted))
	}
	s := strings.Join(parts, ", ")
	if r.Preserved > 0 {
		s += fmt.Sprintf(" (old tips kept in refs/archive: %d)", r.Preserved)
	}
	if s == "" {
		s = "working tree"
	}
	return s + "  " + units.FormatDuration(r.Duration)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
