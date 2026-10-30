package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sylirre/git-archive/internal/archive"
	"github.com/sylirre/git-archive/internal/engine"
	"github.com/sylirre/git-archive/internal/forge"
	"github.com/sylirre/git-archive/internal/safe"
	"github.com/sylirre/git-archive/internal/ui"
	"github.com/sylirre/git-archive/internal/units"
)

type command struct {
	name    string
	aliases []string
	args    string
	summary string
	details string
	flags   func(a *app, fs *flagSet)
	run     func(a *app, ctx context.Context, args []string) (int, error)
	raw     bool // arguments after the command name are passed through unparsed
}

var commands []*command

func init() {
	commands = []*command{
		{name: "sync", args: "[SOURCE...]", summary: "add sources (if given) and clone or update all repositories",
			details: syncDetails, flags: syncFlags, run: (*app).cmdSync},
		{name: "add", args: "SOURCE...", summary: "add or reconfigure sources without syncing",
			flags: addFlags, run: (*app).cmdAdd},
		{name: "remove", aliases: []string{"rm"}, args: "SOURCE...", summary: "forget sources (their repositories stay on disk)",
			run: (*app).cmdRemove},
		{name: "exclude", args: "PATTERN...", summary: "stop cloning and updating matching repositories",
			details: patternDetails, run: (*app).cmdExclude},
		{name: "include", args: "PATTERN...", summary: "undo an exclusion", details: patternDetails, run: (*app).cmdInclude},
		{name: "list", aliases: []string{"ls", "status"}, args: "[PATTERN...]", summary: "show repositories and their status",
			flags: listFlags, run: (*app).cmdList},
		{name: "verify", args: "[PATTERN...]", summary: "check repository integrity (git fsck) and optionally upstream drift",
			flags: verifyFlags, run: (*app).cmdVerify},
		{name: "gc", args: "[PATTERN...]", summary: "repack repositories and remove leftovers of interrupted clones",
			flags: gcFlags, run: (*app).cmdGC},
		{name: "config", args: "[GIT-CONFIG-ARGS...]", summary: "read or edit the archive configuration",
			details: configDetails, run: (*app).cmdConfig, raw: true},
		{name: "completion", args: "bash|zsh|fish", summary: "print a shell completion script", run: (*app).cmdCompletion},
		{name: "help", args: "[COMMAND]", summary: "show help", run: (*app).cmdHelp},
		{name: "version", summary: "show version", run: (*app).cmdVersion},
	}
}

func lookupCommand(name string) *command {
	for _, c := range commands {
		if c.name == name {
			return c
		}
		for _, al := range c.aliases {
			if al == name {
				return c
			}
		}
	}
	return nil
}

func runFlags(a *app, fs *flagSet) {
	fs.intVar(&a.g.jobs, "jobs", 'j', "N", "repositories processed in parallel (default: archive.jobs, 4)")
	fs.stringVar(&a.g.timeout, "timeout", 0, "DURATION", "abort git after this long without output (default: archive.timeout, 10m)")
	fs.boolVar(&a.g.json, "json", 0, "print a JSON report on stdout")
}

func sourceOptionFlags(a *app, fs *flagSet) {
	s := &a.src
	fs.stringVar(&s.provider, "provider", 'p', "NAME", "provider of the following sources: github, gitlab, gitea or git")
	fs.boolVar(&s.insecure, "insecure", 0, "allow unencrypted http:// and git:// sources")
	fs.boolVar(&s.skipForks, "skip-forks", 0, "do not archive forks")
	fs.boolVar(&s.skipArchived, "skip-archived", 0, "do not archive repositories archived upstream")
	fs.stringVar(&s.maxSize, "max-size", 0, "SIZE", "skip repositories larger than SIZE (e.g. 2G)")
	fs.boolVar(&s.wiki, "wiki", 0, "also archive wikis")
	fs.boolVar(&s.lfs, "lfs", 0, "also fetch Git LFS objects (needs git-lfs)")
	fs.boolVar(&s.noMetadata, "no-metadata", 0, "do not save repository metadata snapshots")
	fs.boolVar(&s.noPreserve, "no-preserve", 0, "do not keep history rewritten upstream under refs/archive/")
	fs.boolVar(&s.ssh, "ssh", 0, "clone over ssh instead of https")
	fs.stringVar(&s.tokenEnv, "token-env", 0, "VAR", "environment variable holding the access token for these sources")
	fs.stringVar(&s.api, "api", 0, "URL", "API base URL (self-hosted instances with a non-standard location)")
	fs.listVar(&s.excludes, "exclude", 'x', "PATTERN", "exclude matching repositories")
	fs.stringVar(&s.layout, "layout", 0, "LAYOUT", "for a new archive: worktree (default) or bare")
	fs.boolVar(&s.init, "init", 0, "allow creating an archive in a non-empty directory")
}

func syncFlags(a *app, fs *flagSet) {
	sourceOptionFlags(a, fs)
	fs.listVar(&a.src.only, "only", 'o', "PATTERN", "sync only matching repositories")
	fs.boolVar(&a.src.dryRun, "dry-run", 'n', "show what would be done without changing anything")
	runFlags(a, fs)
}

func addFlags(a *app, fs *flagSet) { sourceOptionFlags(a, fs) }

func listFlags(a *app, fs *flagSet) {
	fs.boolVar(&a.g.json, "json", 0, "print JSON")
	fs.boolVar(&a.sources, "sources", 's', "list sources instead of repositories")
}

func verifyFlags(a *app, fs *flagSet) {
	fs.boolVar(&a.verify.Quick, "quick", 0, "check connectivity only (much faster, does not hash every object)")
	fs.boolVar(&a.verify.Remote, "remote", 'r', "compare refs with upstream (no download)")
	runFlags(a, fs)
}

func gcFlags(a *app, fs *flagSet) {
	fs.boolVar(&a.gc.Aggressive, "aggressive", 0, "pass --aggressive to git gc (slow, better compression)")
	runFlags(a, fs)
}

// openOrCreate opens the archive, creating it when sources are being
// added to a directory that is not an archive yet.
func (a *app) openOrCreate(ctx context.Context, create bool) (arc *archive.Archive, created bool, err error) {
	root := a.archiveDir()
	layout := a.src.layout
	if layout == "" {
		layout = archive.LayoutWorktree
	}
	if layout != archive.LayoutWorktree && layout != archive.LayoutBare {
		return nil, false, usageErr("--layout must be %q or %q", archive.LayoutWorktree, archive.LayoutBare)
	}
	if archive.IsArchive(root) {
		arc, err := a.openArchive(ctx)
		if err == nil && a.src.layout != "" && a.src.layout != arc.Config.Layout {
			return nil, false, fmt.Errorf("the archive uses the %s layout; it cannot be changed", arc.Config.Layout)
		}
		return arc, false, err
	}
	if !create {
		_, err := a.openArchive(ctx)
		return nil, false, err
	}
	if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 && !a.src.init {
		return nil, false, fmt.Errorf("%s is not empty and not an archive; pass --init to create the archive there anyway, or use -C DIR", root)
	}
	if a.src.dryRun {
		return archive.Scratch(root, layout), true, nil
	}
	git, err := a.bootstrapRunner()
	if err != nil {
		return nil, false, err
	}
	arc, err = archive.Create(ctx, git, root, layout)
	return arc, err == nil, err
}

// sourceOpts collects option flags into configuration values.
func (a *app) sourceOpts() (map[string]string, error) {
	s := a.src
	o := map[string]string{}
	set := func(cond bool, k, v string) {
		if cond {
			o[k] = v
		}
	}
	set(s.skipForks, "skipForks", "true")
	set(s.skipArchived, "skipArchived", "true")
	set(s.wiki, "wiki", "true")
	set(s.lfs, "lfs", "true")
	set(s.noMetadata, "metadata", "false")
	set(s.noPreserve, "preserve", "false")
	set(s.ssh, "protocol", "ssh")
	if s.maxSize != "" {
		if _, err := units.ParseSize(s.maxSize); err != nil {
			return nil, usageErr("--max-size: %v", err)
		}
		o["maxSize"] = s.maxSize
	}
	return o, nil
}

// addSources resolves and records sources and exclusions.
func (a *app) addSources(ctx context.Context, arc *archive.Archive, args []string, save bool) error {
	opts, err := a.sourceOpts()
	if err != nil {
		return err
	}
	if a.src.tokenEnv != "" && !validEnvName(a.src.tokenEnv) {
		return usageErr("--token-env must name an environment variable")
	}
	for _, arg := range args {
		spec, err := forge.ParseSpec(arg, a.src.provider, a.src.insecure)
		if err != nil {
			return usageErr("%v", err)
		}
		for _, n := range spec.Notes {
			a.ui.Message(engine.Info, safe.Term(arg)+": "+n)
		}
		spec.API, spec.TokenEnv = a.src.api, a.src.tokenEnv
		proto := opts["protocol"]
		client, err := forge.NewClient(spec, forge.Options{UserAgent: "git-archive/" + Version, Getenv: a.getenv, Protocol: proto})
		if err != nil {
			return err
		}
		resolved, err := client.Resolve(ctx, spec)
		if err != nil {
			return fmt.Errorf("cannot add %s: %w", safe.Term(arg), err)
		}
		src := arc.Config.Source(resolved.Name())
		verb := "Updated"
		if src == nil {
			verb = "Added"
			src = &archive.Source{Name: resolved.Name(), Opts: map[string]string{}}
		}
		src.URL, src.Provider, src.Type = resolved.URL, resolved.Provider, resolved.Type
		if a.src.api != "" {
			src.API = a.src.api
		}
		if a.src.tokenEnv != "" {
			src.TokenEnv = a.src.tokenEnv
		}
		src.Insecure = src.Insecure || a.src.insecure
		for k, v := range opts {
			src.Opts[k] = v
		}
		if save {
			err = arc.Config.SaveSource(ctx, src)
		} else {
			arc.Config.AddSourceInMemory(src)
		}
		if err != nil {
			return err
		}
		what := map[string]string{forge.TypeOwner: "every repository of this %s owner", forge.TypeRepo: "a single %s repository"}[resolved.Type]
		a.ui.Message(engine.Info, fmt.Sprintf("%s source %s: "+what, verb, safe.Term(resolved.Name()), resolved.Provider))
	}
	for _, p := range a.src.excludes {
		if err := archive.ValidatePattern(strings.TrimPrefix(p, "!")); err != nil {
			return usageErr("--exclude: %v", err)
		}
		if save {
			if err := arc.Config.AddExclude(ctx, p); err != nil {
				return err
			}
		} else {
			arc.Config.Excludes = append(arc.Config.Excludes, p)
		}
	}
	return nil
}

func validEnvName(s string) bool {
	for i, c := range s {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return s != ""
}

func (a *app) cmdSync(ctx context.Context, args []string) (int, error) {
	arc, created, err := a.openOrCreate(ctx, len(args) > 0)
	if err != nil {
		return ExitFailure, err
	}
	if !a.src.dryRun {
		if err := arc.Lock(); err != nil {
			return ExitFailure, err
		}
	}
	defer arc.Close()
	if a.g.jobs <= 0 {
		a.g.jobs = arc.Config.Jobs
	}
	a.newUI(a.g.jobs, true)
	a.reportConfig(arc)
	if err := a.addSources(ctx, arc, args, !a.src.dryRun); err != nil {
		if created && !a.src.dryRun && len(arc.Config.Sources) == 0 {
			// Do not leave an empty archive behind after a typo.
			_ = os.RemoveAll(filepath.Join(arc.Root, archive.MetaDir))
		}
		return ExitFailure, err
	}
	if len(arc.Config.Sources) == 0 {
		return ExitFailure, errors.New("no sources configured; add one with: git-archive URL")
	}
	eng, err := a.engine(ctx, arc)
	if err != nil {
		return ExitFailure, err
	}
	ctx, stop := a.withSignals(ctx)
	defer stop()
	rep, err := eng.Sync(ctx, engine.SyncOptions{Only: a.src.only, DryRun: a.src.dryRun})
	return a.finish(rep, err)
}

func (a *app) cmdAdd(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return ExitUsage, usageErr("add needs at least one SOURCE")
	}
	a.newUI(1, false)
	arc, _, err := a.openOrCreate(ctx, true)
	if err != nil {
		return ExitFailure, err
	}
	if err := arc.Lock(); err != nil {
		return ExitFailure, err
	}
	defer arc.Close()
	a.reportConfig(arc)
	if err := a.addSources(ctx, arc, args, true); err != nil {
		return ExitFailure, err
	}
	return ExitOK, nil
}

func (a *app) lockedArchive(ctx context.Context) (*archive.Archive, error) {
	arc, err := a.openArchive(ctx)
	if err != nil {
		return nil, err
	}
	if err := arc.Lock(); err != nil {
		return nil, err
	}
	return arc, nil
}

func (a *app) cmdRemove(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return ExitUsage, usageErr("remove needs at least one SOURCE")
	}
	a.newUI(1, false)
	arc, err := a.lockedArchive(ctx)
	if err != nil {
		return ExitFailure, err
	}
	defer arc.Close()
	for _, arg := range args {
		name := matchSource(arc.Config, arg)
		if arc.Config.Source(name) == nil {
			return ExitFailure, fmt.Errorf("no source %q (see: git-archive list --sources)", safe.Term(arg))
		}
		if err := arc.Config.RemoveSource(ctx, name); err != nil {
			return ExitFailure, err
		}
		n := 0
		for _, rs := range arc.State.Repos {
			if rs.Source == name {
				n++
			}
		}
		a.ui.Message(engine.Info, fmt.Sprintf("Removed source %s; its %d known %s on disk and no longer updated",
			name, n, pluralize(n, "repository stays", "repositories stay")))
	}
	return ExitOK, nil
}

// known returns the IDs of repositories in state or on disk.
func (a *app) known(arc *archive.Archive) []string {
	eng := &engine.Engine{Archive: arc}
	entries, _ := eng.Inventory(nil)
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

func (a *app) cmdExclude(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return ExitUsage, usageErr("exclude needs at least one PATTERN")
	}
	a.newUI(1, false)
	arc, err := a.lockedArchive(ctx)
	if err != nil {
		return ExitFailure, err
	}
	defer arc.Close()
	for _, p := range args {
		if err := archive.ValidatePattern(p); err != nil {
			return ExitUsage, usageErr("%v", err)
		}
		_ = arc.Config.RemoveExclude(ctx, "!"+p)
		if err := arc.Config.AddExclude(ctx, p); err != nil {
			return ExitFailure, err
		}
	}
	n := 0
	for _, id := range a.known(arc) {
		if ex, _ := archive.Excluded(arc.Config.Excludes, id); ex {
			n++
		}
	}
	a.ui.Message(engine.Info, fmt.Sprintf("%d known %s now excluded; existing copies are kept but no longer updated",
		n, pluralize(n, "repository is", "repositories are")))
	return ExitOK, nil
}

func (a *app) cmdInclude(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return ExitUsage, usageErr("include needs at least one PATTERN")
	}
	a.newUI(1, false)
	arc, err := a.lockedArchive(ctx)
	if err != nil {
		return ExitFailure, err
	}
	defer arc.Close()
	for _, p := range args {
		if err := archive.ValidatePattern(p); err != nil {
			return ExitUsage, usageErr("%v", err)
		}
		if err := arc.Config.RemoveExclude(ctx, p); err != nil {
			return ExitFailure, err
		}
		// If a broader pattern still excludes matching repositories, add
		// a negation so that this pattern is included again.
		still := 0
		for _, id := range a.known(arc) {
			if archive.MatchPattern(p, id) {
				if ex, _ := archive.Excluded(arc.Config.Excludes, id); ex {
					still++
				}
			}
		}
		if still > 0 {
			if err := arc.Config.AddExclude(ctx, "!"+p); err != nil {
				return ExitFailure, err
			}
		}
		a.ui.Message(engine.Info, fmt.Sprintf("Included %s; matching repositories are synced again", p))
	}
	return ExitOK, nil
}

func (a *app) cmdList(ctx context.Context, args []string) (int, error) {
	arc, err := a.openArchive(ctx)
	if err != nil {
		return ExitFailure, err
	}
	if a.sources {
		return a.listSources(arc)
	}
	eng := &engine.Engine{Archive: arc}
	entries, partials := eng.Inventory(args)
	if a.g.json {
		if entries == nil {
			entries = []engine.Entry{}
		}
		enc := json.NewEncoder(a.env.Stdout)
		enc.SetIndent("", "  ")
		return ExitOK, enc.Encode(entries)
	}
	tty := ui.IsTerminal(a.env.Stdout)
	color := ui.ColorEnabled(a.g.color, tty)
	tw := tabwriter.NewWriter(a.env.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tREPOSITORY\tLAST SYNC\tNOTE")
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Status]++
		if !a.g.verbose && (e.Status == "absent") {
			continue
		}
		last := "-"
		if !e.LastSync.IsZero() {
			last = e.LastSync.Local().Format("2006-01-02 15:04")
		}
		note := e.Detail
		if e.Preserved > 0 {
			note = strings.TrimSpace(note + fmt.Sprintf(" %d rewritten refs kept", e.Preserved))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", colorStatus(e.Status, color), safe.Term(e.ID), last, safe.TermLine(note))
	}
	tw.Flush()
	var parts []string
	for _, k := range []string{"ok", "failed", "corrupt", "missing", "pending", "gone", "excluded", "filtered", "orphaned", "unmanaged", "absent"} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	fmt.Fprintf(a.env.Stderr, "%d %s: %s\n", len(entries), pluralize(len(entries), "repository", "repositories"), strings.Join(parts, ", "))
	for _, p := range partials {
		fmt.Fprintf(a.env.Stderr, "leftover partial clone: %s (git-archive gc removes it)\n", safe.Term(p))
	}
	return ExitOK, nil
}

func colorStatus(s string, color bool) string {
	code := map[string]string{"ok": "32", "failed": "1;31", "corrupt": "1;31", "missing": "31", "gone": "33",
		"pending": "36", "excluded": "2", "filtered": "2", "orphaned": "33", "unmanaged": "2", "absent": "2"}[s]
	if !color || code == "" {
		return s
	}
	// Pad before colouring so tabwriter alignment is unaffected by the
	// invisible escape bytes (all status cells carry the same overhead).
	return "\x1b[" + code + "m" + fmt.Sprintf("%-9s", s) + "\x1b[0m"
}

func (a *app) listSources(arc *archive.Archive) (int, error) {
	type row struct {
		Name     string            `json:"name"`
		URL      string            `json:"url"`
		Provider string            `json:"provider"`
		Type     string            `json:"type"`
		Options  map[string]string `json:"options,omitempty"`
		LastList time.Time         `json:"lastList,omitzero"`
		Error    string            `json:"listError,omitempty"`
		Repos    int               `json:"repos"`
	}
	var rows []row
	for _, s := range arc.Config.Sources {
		r := row{Name: s.Name, URL: s.URL, Provider: s.Provider, Type: s.Type, Options: s.Opts}
		if st := arc.State.Sources[s.Name]; st != nil {
			r.LastList, r.Error = st.LastList, st.ListError
		}
		for _, rs := range arc.State.Repos {
			if rs.Source == s.Name {
				r.Repos++
			}
		}
		rows = append(rows, r)
	}
	if a.g.json {
		if rows == nil {
			rows = []row{}
		}
		enc := json.NewEncoder(a.env.Stdout)
		enc.SetIndent("", "  ")
		return ExitOK, enc.Encode(rows)
	}
	tw := tabwriter.NewWriter(a.env.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SOURCE\tPROVIDER\tTYPE\tREPOS\tOPTIONS\tLAST LISTED")
	for _, r := range rows {
		var opts []string
		for k, v := range r.Options {
			opts = append(opts, k+"="+v)
		}
		sort.Strings(opts)
		last := "-"
		if !r.LastList.IsZero() {
			last = r.LastList.Local().Format("2006-01-02 15:04")
		}
		if r.Error != "" {
			last += " (failed: " + r.Error + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", safe.Term(r.Name), r.Provider, r.Type, r.Repos, strings.Join(opts, " "), safe.TermLine(last))
	}
	tw.Flush()
	if len(arc.Config.Excludes) > 0 {
		fmt.Fprintf(a.env.Stdout, "\nexcluded: %s\n", safe.Term(strings.Join(arc.Config.Excludes, " ")))
	}
	return ExitOK, nil
}

func (a *app) cmdVerify(ctx context.Context, args []string) (int, error) {
	return a.runMaintenance(ctx, func(ctx context.Context, eng *engine.Engine) (*engine.Report, error) {
		o := a.verify
		o.Only = args
		return eng.Verify(ctx, o)
	})
}

func (a *app) cmdGC(ctx context.Context, args []string) (int, error) {
	return a.runMaintenance(ctx, func(ctx context.Context, eng *engine.Engine) (*engine.Report, error) {
		o := a.gc
		o.Only = args
		return eng.GC(ctx, o)
	})
}

func (a *app) runMaintenance(ctx context.Context, fn func(context.Context, *engine.Engine) (*engine.Report, error)) (int, error) {
	arc, err := a.lockedArchive(ctx)
	if err != nil {
		return ExitFailure, err
	}
	defer arc.Close()
	if a.g.jobs <= 0 {
		a.g.jobs = arc.Config.Jobs
	}
	a.newUI(a.g.jobs, true)
	a.reportConfig(arc)
	eng, err := a.engine(ctx, arc)
	if err != nil {
		return ExitFailure, err
	}
	ctx, stop := a.withSignals(ctx)
	defer stop()
	rep, err := fn(ctx, eng)
	return a.finish(rep, err)
}

func (a *app) cmdConfig(ctx context.Context, args []string) (int, error) {
	root := a.archiveDir()
	if !archive.IsArchive(root) {
		_, err := a.openArchive(ctx)
		return ExitFailure, err
	}
	path := filepath.Join(root, archive.MetaDir, "config")
	if len(args) == 0 {
		args = []string{"--list"}
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"config", "--file", path}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, a.env.Stdout, a.env.Stderr
	cmd.Env = a.env.Environ
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return ExitFailure, err
	}
	// Validate the result so that mistakes surface now, not in cron.
	if _, err := a.openArchive(ctx); err != nil {
		return ExitFailure, fmt.Errorf("the configuration is now invalid: %w", err)
	}
	return ExitOK, nil
}

func (a *app) cmdCompletion(_ context.Context, args []string) (int, error) {
	if len(args) != 1 {
		return ExitUsage, usageErr("completion needs one of: bash, zsh, fish")
	}
	script, err := completionScript(args[0])
	if err != nil {
		return ExitUsage, usageErr("%v", err)
	}
	_, err = io.WriteString(a.env.Stdout, script)
	return ExitOK, err
}

func (a *app) cmdHelp(_ context.Context, args []string) (int, error) {
	if len(args) == 0 {
		fmt.Fprint(a.env.Stdout, helpText(lookupCommand("sync"), nil))
		return ExitOK, nil
	}
	c := lookupCommand(args[0])
	if c == nil {
		return ExitUsage, usageErr("unknown command %q", args[0])
	}
	fs := &flagSet{}
	a.globalFlags(fs)
	if c.flags != nil {
		c.flags(a, fs)
	}
	fmt.Fprint(a.env.Stdout, helpText(c, fs))
	return ExitOK, nil
}

func (a *app) cmdVersion(context.Context, []string) (int, error) {
	fmt.Fprintf(a.env.Stdout, "git-archive %s\n", Version)
	return ExitOK, nil
}

func pluralize(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// matchSource finds the configured source named by arg: its name, its
// URL, or the URL in another spelling (scheme, trailing slash or .git).
func matchSource(cfg *archive.Config, arg string) string {
	norm := func(u string) string {
		if _, rest, ok := strings.Cut(u, "://"); ok {
			u = rest
		}
		u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
		host, path, _ := strings.Cut(u, "/")
		return strings.ToLower(host) + "/" + path
	}
	for _, s := range cfg.Sources {
		if arg == s.Name || arg == s.URL || norm(arg) == norm(s.URL) || norm(arg) == s.Name {
			return s.Name
		}
	}
	return arg
}
