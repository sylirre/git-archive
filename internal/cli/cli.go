// Package cli implements the git-archive command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sylirre/git-archive/internal/archive"
	"github.com/sylirre/git-archive/internal/engine"
	"github.com/sylirre/git-archive/internal/forge"
	"github.com/sylirre/git-archive/internal/gitx"
	"github.com/sylirre/git-archive/internal/safe"
	"github.com/sylirre/git-archive/internal/ui"
	"github.com/sylirre/git-archive/internal/units"
)

// Version is set at build time (-ldflags "-X .../cli.Version=...").
var Version = "0.1.0"

// Exit statuses.
const (
	ExitOK          = 0
	ExitFailure     = 1
	ExitUsage       = 2
	ExitInterrupted = 130
)

// minGit is the oldest git release git-archive is tested against.
var minGit = [3]int{2, 25, 0}

// Env carries the process environment, replaceable in tests.
type Env struct {
	Stdout  io.Writer
	Stderr  io.Writer
	Environ []string
	Dir     string // working directory; "" = os.Getwd()
	// NoSignals disables SIGINT/SIGTERM handling (tests).
	NoSignals bool
}

// Main runs the program and returns the exit status.
func Main(args []string) int {
	return Run(context.Background(), args, Env{Stdout: os.Stdout, Stderr: os.Stderr, Environ: os.Environ()})
}

type app struct {
	env     Env
	g       globals
	src     sourceFlags
	verify  engine.VerifyOptions
	gc      engine.GCOptions
	sources bool // list --sources
	ui      *ui.UI
	cmd     *command
	vars    map[string]string
}

type globals struct {
	dir     string
	quiet   bool
	verbose bool
	color   string
	jobs    int
	timeout string
	json    bool
	help    bool
	version bool
}

type sourceFlags struct {
	provider     string
	insecure     bool
	layout       string
	init         bool
	skipForks    bool
	skipArchived bool
	maxSize      string
	wiki         bool
	lfs          bool
	noMetadata   bool
	noPreserve   bool
	ssh          bool
	tokenEnv     string
	api          string
	excludes     []string
	only         []string
	dryRun       bool
}

// Run executes git-archive with args (without the program name).
func Run(ctx context.Context, args []string, env Env) int {
	a := &app{env: env, vars: map[string]string{}}
	for _, kv := range env.Environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			a.vars[k] = v
		}
	}
	code, err := a.run(ctx, args)
	if a.ui != nil {
		a.ui.Close()
	}
	if err != nil {
		var ue *UsageError
		if errors.As(err, &ue) {
			fmt.Fprintf(env.Stderr, "git-archive: %s\n", safe.Term(err.Error()))
			fmt.Fprintf(env.Stderr, "Try 'git-archive help%s' for more information.\n", a.cmdSuffix())
			return ExitUsage
		}
		if errors.Is(err, context.Canceled) {
			return ExitInterrupted
		}
		fmt.Fprintf(env.Stderr, "git-archive: %s\n", safe.Term(err.Error()))
		if code == ExitOK {
			code = ExitFailure
		}
	}
	return code
}

func (a *app) cmdSuffix() string {
	if a.cmd != nil && a.cmd.name != "sync" {
		return " " + a.cmd.name
	}
	return ""
}

func (a *app) getenv(k string) string { return a.vars[k] }

func (a *app) globalFlags(fs *flagSet) {
	fs.stringVar(&a.g.dir, "archive-dir", 'C', "DIR", "archive location (default: $GIT_ARCHIVE_DIR or the current directory)")
	fs.boolVar(&a.g.quiet, "quiet", 'q', "print only errors and warnings")
	fs.boolVar(&a.g.verbose, "verbose", 'v', "also report unchanged and skipped repositories")
	fs.funcVar("color", 0, "WHEN", "colorize output: auto, always or never", func(v string) error {
		if v != "auto" && v != "always" && v != "never" {
			return fmt.Errorf("--color must be auto, always or never")
		}
		a.g.color = v
		return nil
	})
	fs.boolVar(&a.g.help, "help", 'h', "show help")
	fs.boolVar(&a.g.version, "version", 0, "show version")
}

func (a *app) run(ctx context.Context, args []string) (int, error) {
	a.g.color = "auto"
	name, before, after := splitCommand(args)
	cmd := lookupCommand(name)
	if cmd == nil {
		return ExitUsage, usageErr("unknown command %q", name)
	}
	a.cmd = cmd
	fs := &flagSet{}
	a.globalFlags(fs)
	if cmd.flags != nil {
		cmd.flags(a, fs)
	}
	var pos []string
	var err error
	if cmd.raw {
		// Arguments after the command belong to another program.
		if pos, err = fs.parse(before); err == nil && len(pos) > 0 {
			err = usageErr("unexpected argument %q", pos[0])
		}
		pos = after
	} else {
		pos, err = fs.parse(append(before, after...))
	}
	if err != nil {
		return ExitUsage, err
	}
	if a.g.version {
		fmt.Fprintf(a.env.Stdout, "git-archive %s\n", Version)
		return ExitOK, nil
	}
	if a.g.help && !(cmd.raw && len(pos) > 0) {
		fmt.Fprint(a.env.Stdout, helpText(cmd, fs))
		return ExitOK, nil
	}
	if a.g.quiet && a.g.verbose {
		return ExitUsage, usageErr("--quiet and --verbose are mutually exclusive")
	}
	return cmd.run(a, ctx, pos)
}

// splitCommand finds the subcommand: the first argument that is not an
// option (or an option's value). Without one, the command is "sync".
// It returns the arguments before and after the command name.
func splitCommand(args []string) (name string, before, after []string) {
	valueFlags := map[string]bool{"-C": true, "--archive-dir": true, "--color": true, "--provider": true,
		"--layout": true, "--max-size": true, "--token-env": true, "--api": true, "--exclude": true, "--only": true,
		"-j": true, "--jobs": true, "--timeout": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") {
			if valueFlags[a] {
				i++
			}
			continue
		}
		if lookupCommand(a) != nil {
			return a, append([]string{}, args[:i]...), append([]string{}, args[i+1:]...)
		}
		break
	}
	return "sync", args, nil
}

// archiveDir resolves the archive location.
func (a *app) archiveDir() string {
	dir := a.g.dir
	if dir == "" {
		dir = a.getenv("GIT_ARCHIVE_DIR")
	}
	if dir == "" {
		dir = a.env.Dir
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if !filepath.IsAbs(dir) && a.env.Dir != "" {
		dir = filepath.Join(a.env.Dir, dir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// newUI creates the progress display on stderr. Short commands pass
// live=false: they print messages but never draw a progress block.
func (a *app) newUI(jobs int, live bool) *ui.UI {
	tty := ui.IsTerminal(a.env.Stderr) && a.getenv("TERM") != "dumb"
	mode := ui.Plain
	switch {
	case a.g.quiet:
		mode = ui.Quiet
	case tty && live:
		mode = ui.Live
	}
	a.ui = ui.New(ui.Options{Out: a.env.Stderr, Mode: mode, Color: ui.ColorEnabled(a.g.color, tty),
		Unicode: unicodeLocale(a.vars), Verbose: a.g.verbose, Jobs: jobs})
	return a.ui
}

func unicodeLocale(vars map[string]string) bool {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := strings.ToLower(vars[k]); v != "" {
			return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
		}
	}
	return false
}

// bootstrapRunner is a git runner for configuration access only.
func (a *app) bootstrapRunner() (*gitx.Runner, error) {
	return gitx.New(gitx.Options{Environ: a.env.Environ})
}

// runner builds the git runner for an archive: transports and
// credentials are derived from the configured sources.
func (a *app) runner(ctx context.Context, cfg *archive.Config) (*gitx.Runner, error) {
	protocols := map[string]bool{"https": true, "ssh": true}
	var creds []gitx.Credential
	seen := map[string]bool{}
	for _, s := range cfg.Sources {
		spec, err := engine.SpecOf(s)
		if err != nil {
			return nil, err
		}
		switch spec.Scheme {
		case "http", "git", "file":
			protocols[spec.Scheme] = true
		}
		tok := forge.Token(spec, a.getenv)
		if tok == "" || (spec.Scheme != "https" && spec.Scheme != "http") {
			continue
		}
		prefix := spec.Scheme + "://" + spec.Host
		if seen[prefix] {
			continue
		}
		seen[prefix] = true
		user := map[string]string{forge.GitHub: "x-access-token", forge.GitLab: "oauth2"}[spec.Provider]
		creds = append(creds, gitx.Credential{URLPrefix: prefix, Username: user, Token: tok})
	}
	timeout := cfg.Timeout
	if a.g.timeout != "" {
		d, err := units.ParseDuration(a.g.timeout)
		if err != nil {
			return nil, usageErr("--timeout: %v", err)
		}
		timeout = d
	}
	var list []string
	for p := range protocols {
		list = append(list, p)
	}
	r, err := gitx.New(gitx.Options{Protocols: list, Credentials: creds, IdleTimeout: timeout, Environ: a.env.Environ})
	if err != nil {
		return nil, err
	}
	v, err := r.Version(ctx)
	if err != nil {
		return nil, err
	}
	if v[0] < minGit[0] || v[0] == minGit[0] && v[1] < minGit[1] {
		a.ui.Message(engine.Warn, fmt.Sprintf("git %d.%d.%d is older than the tested minimum %d.%d; some protections may be missing",
			v[0], v[1], v[2], minGit[0], minGit[1]))
	}
	return r, nil
}

// engine builds an engine for an opened archive.
func (a *app) engine(ctx context.Context, arc *archive.Archive) (*engine.Engine, error) {
	r, err := a.runner(ctx, arc.Config)
	if err != nil {
		return nil, err
	}
	return &engine.Engine{
		Archive:      arc,
		Git:          r,
		Reporter:     a.ui,
		Jobs:         a.g.jobs,
		Retries:      arc.Config.Retries,
		ForgeOptions: forge.Options{UserAgent: "git-archive/" + Version, Getenv: a.getenv},
	}, nil
}

// withSignals cancels ctx on SIGINT/SIGTERM; a second signal exits
// immediately (after restoring the terminal).
func (a *app) withSignals(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	if a.env.NoSignals {
		return ctx, cancel
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-sigs:
		case <-done:
			return
		}
		a.ui.Message(engine.Warn, "interrupted: stopping git processes (press Ctrl-C again to quit immediately)")
		cancel()
		select {
		case <-sigs:
			a.ui.Close()
			os.Exit(ExitInterrupted)
		case <-done:
		}
	}()
	return ctx, func() {
		signal.Stop(sigs)
		close(done)
		cancel()
	}
}

// finish prints the summary and JSON report and derives the exit status.
func (a *app) finish(rep *engine.Report, err error) (int, error) {
	if rep != nil {
		a.ui.Summary(rep)
		if a.g.json {
			enc := json.NewEncoder(a.env.Stdout)
			enc.SetIndent("", "  ")
			if jerr := enc.Encode(rep); jerr != nil && err == nil {
				err = jerr
			}
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return ExitInterrupted, nil
	case err != nil:
		return ExitFailure, err
	case rep != nil && rep.Failed():
		return ExitFailure, nil
	}
	return ExitOK, nil
}

// openArchive opens an existing archive.
func (a *app) openArchive(ctx context.Context) (*archive.Archive, error) {
	git, err := a.bootstrapRunner()
	if err != nil {
		return nil, err
	}
	root := a.archiveDir()
	arc, err := archive.Open(ctx, git, root)
	if errors.Is(err, archive.ErrNotArchive) {
		return nil, fmt.Errorf("%s is not a git-archive (no %s/config); create one with: git-archive URL...", root, archive.MetaDir)
	}
	return arc, err
}

// reportConfig shows configuration warnings and repair notices.
func (a *app) reportConfig(arc *archive.Archive) {
	for _, w := range arc.Config.Warnings {
		a.ui.Message(engine.Warn, "config: "+w)
	}
	if arc.Notice != "" {
		a.ui.Message(engine.Warn, arc.Notice)
	}
}
