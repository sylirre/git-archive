// Package gitx runs the system git binary in a hardened, observable way.
//
// Every invocation gets a sanitised environment, a fixed set of
// configuration overrides that close known attack surfaces of untrusted
// repositories, no controlling terminal, an inactivity watchdog and a
// process group that is torn down as a whole on cancellation. Progress
// reports on stderr are parsed and forwarded to a callback; the remaining
// stderr lines are retained for error reporting.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Credential supplies a token for every git URL starting with URLPrefix
// (for example "https://github.com"). The token is passed to git through
// the environment of a scoped credential helper, so it never appears in
// command lines, remote URLs or repository configuration.
type Credential struct {
	URLPrefix string
	Username  string
	Token     string
}

// Runner executes git commands. The zero value is not usable; call New.
type Runner struct {
	git      string
	env      []string
	cfg      []string
	idle     time.Duration
	killWait time.Duration
}

// Options configures a Runner.
type Options struct {
	// Git is the git executable; "git" (looked up in PATH) when empty.
	Git string
	// Protocols lists the transports git may use, e.g. "https", "ssh".
	// Everything else, notably "ext" and "file", is refused.
	Protocols []string
	// Credentials are offered to matching URLs.
	Credentials []Credential
	// IdleTimeout aborts a command that produces no output for this
	// long. Zero disables the watchdog.
	IdleTimeout time.Duration
	// Environ is the base environment; os.Environ() when nil.
	Environ []string
}

// New creates a Runner.
func New(o Options) (*Runner, error) {
	git := o.Git
	if git == "" {
		git = "git"
	}
	path, err := exec.LookPath(git)
	if err != nil {
		return nil, fmt.Errorf("git executable not found: %w", err)
	}
	environ := o.Environ
	if environ == nil {
		environ = os.Environ()
	}
	for _, p := range o.Protocols {
		if !regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`).MatchString(p) {
			return nil, fmt.Errorf("invalid protocol name %q", p)
		}
	}
	env := append(cleanEnv(environ), credentialEnv(o.Credentials)...)
	return &Runner{
		git:      path,
		env:      env,
		cfg:      configArgs(o.Protocols, o.Credentials),
		idle:     o.IdleTimeout,
		killWait: 3 * time.Second,
	}, nil
}

// Cmd describes one git invocation.
type Cmd struct {
	// Dir is the working directory.
	Dir string
	// GitDir and WorkTree are passed as --git-dir/--work-tree so that git
	// never discovers a repository on its own.
	GitDir   string
	WorkTree string
	Args     []string
	// Stdin is fed to the command; empty input when nil.
	Stdin io.Reader
	// Progress receives parsed progress reports; nil discards them.
	Progress func(Progress)
	// MaxOutput bounds captured stdout (default 64 MiB).
	MaxOutput int64
}

// Error describes a failed git command.
type Error struct {
	Args    []string // git subcommand and arguments (without -c options)
	Exit    int      // exit code, -1 when killed or not started
	Stderr  []string // last stderr lines that were not progress reports
	Timeout bool     // killed by the inactivity watchdog
	Err     error
}

func (e *Error) Error() string {
	cmd := "git"
	if len(e.Args) > 0 {
		cmd += " " + e.Args[0]
	}
	switch {
	case e.Timeout:
		return cmd + ": no progress for too long, aborted"
	case e.Exit < 0 && e.Err != nil:
		return cmd + ": " + e.Err.Error()
	}
	if s := e.Summary(); s != "" {
		return cmd + ": " + s
	}
	return fmt.Sprintf("%s: exit status %d", cmd, e.Exit)
}

func (e *Error) Unwrap() error { return e.Err }

// Summary returns the most informative stderr lines: up to three lines
// starting with "error:" or "fatal:", or else the last line. The result
// is raw; display code must sanitise it.
func (e *Error) Summary() string {
	var picked []string
	for _, l := range e.Stderr {
		t := strings.TrimSpace(strings.TrimPrefix(l, "remote: "))
		if strings.HasPrefix(t, "error:") || strings.HasPrefix(t, "fatal:") {
			picked = append(picked, t)
			if len(picked) == 3 {
				break
			}
		}
	}
	if len(picked) == 0 && len(e.Stderr) > 0 {
		picked = append(picked, strings.TrimSpace(e.Stderr[len(e.Stderr)-1]))
	}
	return strings.Join(picked, " | ")
}

var (
	transientRE = regexp.MustCompile(`(?i)could not resolve host|couldn't resolve host|name resolution|` +
		`timed out|connection reset|connection refused|connection closed|early eof|` +
		`remote end hung up|rpc failed|http/2 stream|gnutls|ssl_read|ssl_connect|tls connection|` +
		`unexpected disconnect|returned error: (5\d\d|429)|http (5\d\d|429)|error: (5\d\d|429)\b|` +
		`internal server error|bad gateway|service unavailable|gateway time-?out|too many requests|` +
		`transfer closed|recv failure|send failure|failed to connect|network is unreachable|` +
		`ssh_exchange_identification|kex_exchange_identification`)
	integrityRE = regexp.MustCompile(`(?i)fsck|hasdot|gitmodules|bad object|corrupt|duplicateentries|` +
		`invalid object|object .* is a .*, not a`)
)

// Transient reports whether retrying the command may succeed: network
// failures and server errors are transient, while missing repositories,
// authentication failures and integrity violations are not.
func (e *Error) Transient() bool {
	if e.Timeout {
		return true
	}
	text := strings.Join(e.Stderr, "\n")
	if integrityRE.MatchString(text) {
		return false
	}
	return transientRE.MatchString(text)
}

// IsNotFound reports whether the error says the remote repository does
// not exist (or is not accessible without credentials).
func IsNotFound(err error) bool {
	var ge *Error
	if !errors.As(err, &ge) {
		return false
	}
	text := strings.ToLower(strings.Join(ge.Stderr, "\n"))
	return strings.Contains(text, "not found") || strings.Contains(text, "does not appear to be a git repository") ||
		strings.Contains(text, "could not read username") || strings.Contains(text, "returned error: 404") ||
		strings.Contains(text, "authentication failed")
}

const stderrKeep = 40

// Run executes the command and returns its stdout.
func (r *Runner) Run(ctx context.Context, c Cmd) ([]byte, error) {
	args := make([]string, 0, len(r.cfg)+len(c.Args)+2)
	args = append(args, r.cfg...)
	if c.GitDir != "" {
		args = append(args, "--git-dir="+c.GitDir)
	}
	if c.WorkTree != "" {
		args = append(args, "--work-tree="+c.WorkTree)
	}
	args = append(args, c.Args...)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.git, args...)
	cmd.Dir = c.Dir
	cmd.Env = r.env
	cmd.Stdin = c.Stdin
	stopKill := setProcAttr(cmd, r.killWait)

	limit := c.MaxOutput
	if limit <= 0 {
		limit = 64 << 20
	}
	var activity atomic.Int64
	touch := func() { activity.Store(time.Now().UnixNano()) }
	touch()

	stdout := &limitedBuffer{limit: limit, touch: touch}
	cmd.Stdout = stdout
	stderrR, err := cmd.StderrPipe()
	if err != nil {
		return nil, &Error{Args: c.Args, Exit: -1, Err: err}
	}
	if err := cmd.Start(); err != nil {
		return nil, &Error{Args: c.Args, Exit: -1, Err: err}
	}

	var tail []string
	var wg sync.WaitGroup
	stderrDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stderrDone)
		scanLines(stderrR, func(line string) {
			touch()
			if p, ok := ParseProgress(line); ok {
				if c.Progress != nil {
					c.Progress(p)
				}
				return
			}
			if strings.TrimSpace(line) == "" {
				return
			}
			if len(tail) == stderrKeep {
				copy(tail, tail[1:])
				tail = tail[:stderrKeep-1]
			}
			tail = append(tail, line)
		})
	}()

	var timedOut atomic.Bool
	watchdogDone := make(chan struct{})
	if r.idle > 0 {
		go func() {
			t := time.NewTicker(min(r.idle/4, time.Second))
			defer t.Stop()
			for {
				select {
				case <-watchdogDone:
					return
				case <-t.C:
					if time.Since(time.Unix(0, activity.Load())) > r.idle {
						timedOut.Store(true)
						cancel()
						return
					}
				}
			}
		}()
	}

	// A descendant that escaped the process group could keep stderr open
	// forever; stop reading once a cancelled command had time to die.
	go func() {
		select {
		case <-stderrDone:
		case <-ctx.Done():
			select {
			case <-stderrDone:
			case <-time.After(r.killWait + 2*time.Second):
				stderrR.Close()
			}
		}
	}()

	wg.Wait() // stderr is drained before Wait closes the pipe
	err = cmd.Wait()
	stopKill()
	close(watchdogDone)

	if stdout.overflow {
		return nil, &Error{Args: c.Args, Exit: -1, Stderr: tail, Err: fmt.Errorf("output exceeds %d bytes", limit)}
	}
	if err != nil {
		ge := &Error{Args: c.Args, Exit: -1, Stderr: tail, Err: err, Timeout: timedOut.Load()}
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.Exited() {
			ge.Exit = ee.ExitCode()
		}
		if ctxErr := ctx.Err(); ctxErr != nil && !ge.Timeout {
			ge.Err = ctxErr
			ge.Exit = -1
		}
		return stdout.Bytes(), ge
	}
	return stdout.Bytes(), nil
}

// Output is a convenience wrapper returning trimmed stdout.
func (r *Runner) Output(ctx context.Context, dir, gitDir string, args ...string) (string, error) {
	out, err := r.Run(ctx, Cmd{Dir: dir, GitDir: gitDir, Args: args})
	return strings.TrimRight(string(out), "\n"), err
}

// Version returns git's version as major, minor, patch.
func (r *Runner) Version(ctx context.Context) ([3]int, error) {
	out, err := r.Output(ctx, "", "", "version")
	if err != nil {
		return [3]int{}, err
	}
	return parseVersion(out)
}

func parseVersion(s string) ([3]int, error) {
	m := regexp.MustCompile(`git version (\d+)\.(\d+)(?:\.(\d+))?`).FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, fmt.Errorf("unrecognised git version %q", s)
	}
	var v [3]int
	for i := range 3 {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, nil
}

// limitedBuffer deliberately does not embed bytes.Buffer: io.Copy would
// use the embedded ReadFrom and bypass the limit check in Write.
type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int64
	overflow bool
	touch    func()
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.touch()
	if int64(b.buf.Len()+len(p)) > b.limit {
		b.overflow = true
		return 0, errors.New("output limit exceeded")
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }
