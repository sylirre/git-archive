// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sylirre/git-archive/internal/engine"
	"github.com/sylirre/git-archive/internal/safe"
	"github.com/sylirre/git-archive/internal/units"
)

// redraw replaces the live block with pending log lines followed by a
// fresh block (when live is true). mu is held.
//
// The block is drawn without a trailing newline and every line is cut
// to the terminal width, so the cursor position — and hence the number
// of lines to erase next time — is always known exactly.
func (u *UI) redraw(live bool) {
	var b strings.Builder
	if u.drawn > 0 {
		b.WriteString("\r")
		if u.drawn > 1 {
			fmt.Fprintf(&b, "\x1b[%dA", u.drawn-1)
		}
		b.WriteString("\x1b[J")
	}
	for _, l := range u.pending {
		b.WriteString(l)
		b.WriteString("\n")
	}
	u.pending = u.pending[:0]
	u.drawn = 0
	if live {
		lines := u.block(u.o.Width())
		b.WriteString(strings.Join(lines, "\n"))
		u.drawn = len(lines)
	}
	io.WriteString(u.o.Out, b.String())
}

// block renders the live lines: one per busy worker and a status line.
func (u *UI) block(width int) []string {
	s := u.sty
	spin := s.spinner(u.frame)
	var lines []string
	nameW := 0
	for _, sl := range u.slots {
		if sl.task != nil {
			nameW = max(nameW, utf8.RuneCountInString(shortName(sl.task.ID)))
		}
	}
	nameW = min(nameW, max(16, width/3))
	for _, sl := range u.slots {
		if sl.task == nil {
			continue
		}
		name := pad(truncate(safe.Term(shortName(sl.task.ID)), nameW), nameW)
		line := " " + s.info(spin) + " " + name + "  " + u.slotStatus(sl, width-nameW-6)
		lines = append(lines, cut(line, width-1))
	}
	lines = append(lines, cut(u.statusLine(width), width-1))
	return lines
}

// slotStatus renders a worker's step and git progress.
func (u *UI) slotStatus(sl slot, width int) string {
	s := u.sty
	elapsed := time.Since(sl.since)
	if !sl.hasProg {
		return s.dim(fmt.Sprintf("%s… %s", sl.step, clock(elapsed)))
	}
	p := sl.prog
	phase := strings.ToLower(p.Phase)
	if p.Remote {
		phase = "remote: " + phase
	}
	phase = pad(truncate(phase, 22), 22)
	if p.Percent < 0 {
		return phase + " " + fmt.Sprintf("%d", p.Current)
	}
	detail := ""
	if p.Detail != "" {
		detail = "  " + s.dim(safe.Term(p.Detail))
	}
	// Reserve room for the throughput text up front so the bar does not
	// jump when git starts reporting it.
	barW := max(min(width-22-6-28, 30), 8)
	return phase + " " + fmt.Sprintf("%3d%%", p.Percent) + " " + s.bar(float64(p.Percent)/100, barW) + detail
}

// statusLine renders the phase, overall bar, counts and elapsed time.
func (u *UI) statusLine(width int) string {
	s := u.sty
	elapsed := clock(time.Since(u.start))
	if u.total == 0 || u.phase == "Listing sources" {
		return " " + s.bold(u.phase+"…") + "  " + s.dim(elapsed)
	}
	counts := u.countSummary()
	frac := float64(u.done) / float64(u.total)
	head := fmt.Sprintf(" %s %d/%d ", s.bold(u.phase), u.done, u.total)
	barW := max(min(width-visibleLen(head)-visibleLen(counts)-len(elapsed)-6, 40), 10)
	return head + s.bar(frac, barW) + "  " + counts + "  " + s.dim(elapsed)
}

// countSummary lists the non-zero outcome counters.
func (u *UI) countSummary() string {
	s := u.sty
	var parts []string
	add := func(o engine.Outcome, color func(string) string, sym string) {
		if n := u.count[o]; n > 0 {
			parts = append(parts, color(fmt.Sprintf("%s %d %s", s.sym(sym), n, o)))
		}
	}
	add(engine.Cloned, s.good, "ok")
	add(engine.Updated, s.info, "upd")
	add(engine.Unchanged, s.dim, "same")
	add(engine.Verified, s.good, "ok")
	add(engine.Maintained, s.good, "ok")
	add(engine.Failed, s.bad, "fail")
	return strings.Join(parts, "  ")
}

// shortName drops the host from an ID for compact display.
func shortName(id string) string {
	if _, rest, ok := strings.Cut(id, "/"); ok {
		return rest
	}
	return id
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

func pad(s string, n int) string {
	if k := utf8.RuneCountInString(s); k < n {
		return s + strings.Repeat(" ", n-k)
	}
	return s
}

// visibleLen counts runes outside ANSI escape sequences.
func visibleLen(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			if r >= '@' && r <= '~' && r != '[' {
				esc = false
			}
		case r == '\x1b':
			esc = true
		default:
			n++
		}
	}
	return n
}

// cut truncates s to width visible runes, keeping escape sequences
// intact and resetting attributes at the end.
func cut(s string, width int) string {
	if visibleLen(s) <= width {
		return s
	}
	var b strings.Builder
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			b.WriteRune(r)
			if r >= '@' && r <= '~' && r != '[' {
				esc = false
			}
		case r == '\x1b':
			esc = true
			b.WriteRune(r)
		default:
			if n >= width {
				continue
			}
			b.WriteRune(r)
			n++
		}
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

// clock renders an elapsed time with second resolution: "7s", "4m05s".
func clock(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return units.FormatDuration(d)
}
