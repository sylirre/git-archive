// Package engine implements the archive operations — sync, verify, gc
// and inventory — on top of the archive model, the forge clients and the
// hardened git runner. Work on repositories runs in a bounded worker
// pool and is reported through the Reporter interface, which the
// terminal UI implements.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/sylirre/git-archive/internal/archive"
	"github.com/sylirre/git-archive/internal/forge"
	"github.com/sylirre/git-archive/internal/gitx"
)

// Outcome classifies the result of one task.
type Outcome string

// Outcomes.
const (
	Cloned      Outcome = "cloned"
	Updated     Outcome = "updated"
	Unchanged   Outcome = "unchanged"
	Failed      Outcome = "failed"
	Excluded    Outcome = "excluded"    // matched an exclusion pattern
	Filtered    Outcome = "filtered"    // fork/archived/size filter
	Gone        Outcome = "gone"        // no longer upstream; local copy kept
	Absent      Outcome = "absent"      // optional repository (wiki) does not exist
	Interrupted Outcome = "interrupted" // cancelled before or during work
	Planned     Outcome = "planned"     // dry run
	Verified    Outcome = "ok"          // verify passed
	Maintained  Outcome = "maintained"  // gc done
)

// Action is what a task does.
type Action string

// Actions.
const (
	ActClone  Action = "clone"
	ActUpdate Action = "update"
	ActSkip   Action = "skip"
	ActVerify Action = "verify"
	ActGC     Action = "gc"
)

// Task is one unit of work on one repository.
type Task struct {
	ID       string
	Dir      string
	Action   Action
	Repo     forge.Repo // as discovered (sync)
	Source   string
	Opts     archive.Options
	Wiki     bool   // optional wiki repository
	RenameOf string // previous ID when the repository was renamed upstream
	Reason   string // for skips
	estimate time.Duration
}

// RefChanges counts ref updates of a fetch.
type RefChanges struct {
	Added   int `json:"added"`
	Updated int `json:"updated"` // fast-forward
	Forced  int `json:"forced"`  // history rewritten
	Deleted int `json:"deleted"`
}

// Total is the number of changed refs.
func (c RefChanges) Total() int { return c.Added + c.Updated + c.Forced + c.Deleted }

// Result is the outcome of one task.
type Result struct {
	ID        string        `json:"id"`
	Action    Action        `json:"action"`
	Outcome   Outcome       `json:"outcome"`
	Reason    string        `json:"reason,omitempty"`
	Error     string        `json:"error,omitempty"`
	Refs      RefChanges    `json:"refs"`
	Preserved int           `json:"preserved,omitempty"`
	Warnings  []string      `json:"warnings,omitempty"`
	Notes     []string      `json:"notes,omitempty"`
	Attempts  int           `json:"attempts,omitempty"`
	Duration  time.Duration `json:"-"`
	Millis    int64         `json:"durationMs"`
	Freed     int64         `json:"freedBytes,omitempty"` // gc
	err       error
}

func (r *Result) warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

func (r *Result) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// Report summarises a command run.
type Report struct {
	Command  string          `json:"command"`
	Archive  string          `json:"archive"`
	Started  time.Time       `json:"started"`
	Millis   int64           `json:"durationMs"`
	DryRun   bool            `json:"dryRun,omitempty"`
	Sources  []SourceResult  `json:"sources,omitempty"`
	Results  []*Result       `json:"results"`
	Counts   map[Outcome]int `json:"counts"`
	Warnings []string        `json:"warnings,omitempty"`
	Duration time.Duration   `json:"-"`
}

// SourceResult reports the listing of one source.
type SourceResult struct {
	Name    string   `json:"name"`
	Repos   int      `json:"repos"`
	Error   string   `json:"error,omitempty"`
	Skipped []string `json:"skipped,omitempty"`
}

// Failed reports whether any task failed.
func (r *Report) Failed() bool { return r.Counts[Failed] > 0 }

// Level of a message.
type Level int

// Message levels.
const (
	Info Level = iota
	Warn
	Error
)

// Reporter receives progress. Implementations must be safe for
// concurrent use; slot identifies the worker (0..jobs-1).
type Reporter interface {
	Phase(title string, total int)
	Start(slot int, t *Task)
	Step(slot int, t *Task, step string)
	Progress(slot int, t *Task, p gitx.Progress)
	Done(slot int, t *Task, r *Result)
	Message(level Level, text string)
}

// Engine runs archive operations.
type Engine struct {
	Archive  *archive.Archive
	Git      *gitx.Runner
	Reporter Reporter
	Jobs     int
	Retries  int
	// Backoff is the first retry delay; later retries wait three times
	// longer each.
	Backoff time.Duration
	// NewClient creates forge clients (replaceable in tests).
	NewClient func(forge.Spec, forge.Options) (forge.Client, error)
	// ForgeOptions are passed to NewClient (HTTP client, user agent).
	ForgeOptions forge.Options
	Now          func() time.Time

	lfsOnce sync.Once
	lfsOK   bool
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) jobs() int {
	if e.Jobs > 0 {
		return e.Jobs
	}
	return e.Archive.Config.Jobs
}

func (e *Engine) layoutBare() bool { return e.Archive.Config.Layout == archive.LayoutBare }

// lfsAvailable reports whether "git lfs" works.
func (e *Engine) lfsAvailable(ctx context.Context) bool {
	e.lfsOnce.Do(func() {
		if _, err := exec.LookPath("git-lfs"); err != nil {
			return
		}
		_, err := e.Git.Output(ctx, "", "", "lfs", "version")
		e.lfsOK = err == nil
	})
	return e.lfsOK
}

// runPool executes tasks with the configured parallelism. Tasks are
// started in the given order; fn's results are collected in that order.
func (e *Engine) runPool(ctx context.Context, tasks []*Task, fn func(ctx context.Context, slot int, t *Task) *Result) []*Result {
	results := make([]*Result, len(tasks))
	next := make(chan int)
	var wg sync.WaitGroup
	n := min(e.jobs(), max(len(tasks), 1))
	for slot := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				t := tasks[i]
				if ctx.Err() != nil {
					results[i] = &Result{ID: t.ID, Action: t.Action, Outcome: Interrupted}
					continue
				}
				e.Reporter.Start(slot, t)
				start := time.Now()
				r := fn(ctx, slot, t)
				r.Duration = time.Since(start)
				r.Millis = r.Duration.Milliseconds()
				if r.Outcome == Failed && ctx.Err() != nil {
					r.Outcome, r.Error = Interrupted, ""
				}
				results[i] = r
				e.Reporter.Done(slot, t, r)
			}
		}()
	}
	for i := range tasks {
		next <- i
	}
	close(next)
	wg.Wait()
	return results
}

// retry runs a network operation, repeating it after transient failures.
func (e *Engine) retry(ctx context.Context, slot int, t *Task, r *Result, step string, op func() error) error {
	delay := e.Backoff
	if delay == 0 {
		delay = 5 * time.Second
	}
	for attempt := 0; ; attempt++ {
		r.Attempts++
		err := op()
		var ge *gitx.Error
		if err == nil || attempt >= e.Retries || ctx.Err() != nil || !errors.As(err, &ge) || !ge.Transient() {
			return err
		}
		e.Reporter.Step(slot, t, fmt.Sprintf("%s failed, retrying in %s", step, delay))
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
		delay *= 3
		e.Reporter.Step(slot, t, step)
	}
}

// stateSaver persists state periodically while a long command runs, so
// that an interruption (power loss, SIGKILL) loses little bookkeeping.
func (e *Engine) stateSaver(interval time.Duration) (stop func() error) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := e.Archive.SaveState(); err != nil {
					e.Reporter.Message(Warn, "saving state: "+err.Error())
				}
			}
		}
	}()
	return func() error {
		close(done)
		wg.Wait()
		return e.Archive.SaveState()
	}
}

func newReport(cmd string, a *archive.Archive) *Report {
	return &Report{Command: cmd, Archive: a.Root, Started: time.Now(), Counts: map[Outcome]int{}}
}

func (rep *Report) finish(results []*Result) {
	rep.Results = append(rep.Results, results...)
	sort.SliceStable(rep.Results, func(i, j int) bool { return rep.Results[i].ID < rep.Results[j].ID })
	for _, r := range rep.Results {
		if r.err != nil && r.Error == "" {
			r.Error = r.err.Error()
		}
		rep.Counts[r.Outcome]++
	}
	rep.Duration = time.Since(rep.Started)
	rep.Millis = rep.Duration.Milliseconds()
}
