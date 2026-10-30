// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strings"

	"github.com/sylirre/git-archive/internal/engine"
	"github.com/sylirre/git-archive/internal/safe"
	"github.com/sylirre/git-archive/internal/units"
)

// Summary prints the end-of-run summary. In Quiet mode only failures are
// listed.
func (u *UI) Summary(rep *engine.Report) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.sty
	if rep.DryRun {
		u.plan(rep)
		return
	}
	c := rep.Counts
	if u.o.Mode != Quiet {
		var main []string
		add := func(list *[]string, n int, word string, color func(string) string) {
			if n > 0 {
				*list = append(*list, color(fmt.Sprintf("%d %s", n, word)))
			}
		}
		add(&main, c[engine.Cloned], "cloned", s.good)
		add(&main, c[engine.Updated], "updated", s.info)
		add(&main, c[engine.Unchanged], "unchanged", s.dim)
		add(&main, c[engine.Verified], "ok", s.good)
		add(&main, c[engine.Maintained], "maintained", s.good)
		add(&main, c[engine.Failed], "failed", s.bad)
		add(&main, c[engine.Interrupted], "interrupted", s.warn)
		var skipped []string
		add(&skipped, c[engine.Excluded], "excluded", s.dim)
		add(&skipped, c[engine.Filtered], "filtered", s.dim)
		add(&skipped, c[engine.Absent], "without wiki", s.dim)
		line := s.bold(verb(rep.Command)) + " in " + units.FormatDuration(rep.Duration)
		if len(main) > 0 {
			line += ": " + strings.Join(main, ", ")
		} else {
			line += ": nothing to do"
		}
		if len(skipped) > 0 {
			line += s.dim(" · skipped ") + strings.Join(skipped, ", ")
		}
		if n := c[engine.Gone]; n > 0 {
			line += s.dim(" · ") + s.warn(fmt.Sprintf("%d gone upstream (kept)", n))
		}
		if rep.Command == "gc" {
			var freed int64
			for _, r := range rep.Results {
				freed += r.Freed
			}
			line += s.dim(" · freed " + units.FormatBytes(freed))
		}
		u.emit(line)
	}
	var failed []*engine.Result
	for _, r := range rep.Results {
		if r.Outcome == engine.Failed {
			failed = append(failed, r)
		}
	}
	if len(failed) > 0 {
		u.emit(s.bad(fmt.Sprintf("Failed (%d):", len(failed))))
		for _, r := range failed {
			u.emit("  " + safe.Term(r.ID) + "  " + safe.TermLine(r.Error))
		}
	}
}

func verb(cmd string) string {
	switch cmd {
	case "verify":
		return "Verified"
	case "gc":
		return "Maintenance done"
	}
	return "Synced"
}

// plan prints the outcome of a dry run.
func (u *UI) plan(rep *engine.Report) {
	s := u.sty
	n := map[string]int{}
	u.emit(s.bold("Plan (dry run, nothing was changed):"))
	for _, r := range rep.Results {
		what := string(r.Action)
		switch r.Outcome {
		case engine.Planned:
		case engine.Gone:
			what = "keep"
		default:
			what = "skip"
		}
		n[what]++
		line := "  " + pad(what, 7) + " " + safe.Term(r.ID)
		if r.Reason != "" {
			line += s.dim("  " + safe.TermLine(r.Reason))
		}
		if what == "skip" && r.Reason == "" {
			line += s.dim("  " + string(r.Outcome))
		}
		u.emit(line)
	}
	u.emit(fmt.Sprintf("Would clone %d, update %d, skip %d; %d gone upstream.", n["clone"], n["update"], n["skip"], n["keep"]))
}
