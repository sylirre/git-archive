// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sylirre/git-archive/internal/gitx"
	"github.com/sylirre/git-archive/internal/units"
)

// Repository layouts.
const (
	LayoutWorktree = "worktree" // regular clone with a checked-out default branch
	LayoutBare     = "bare"     // bare repository, no working tree
)

// Built-in defaults for archive-wide settings.
const (
	DefaultJobs    = 4
	DefaultTimeout = 10 * time.Minute
	DefaultRetries = 2
)

// Options are per-source settings. Each may be given in [archive] as a
// default for all sources, and overridden in a [source] section.
type Options struct {
	SkipForks    bool   // do not archive forks
	SkipArchived bool   // do not archive repositories marked archived upstream
	MaxSize      int64  // skip repositories larger than this (bytes, as reported by the API); 0 = no limit
	Wiki         bool   // also archive the wiki repository
	LFS          bool   // fetch Git LFS objects
	Metadata     bool   // save API metadata as JSON
	Preserve     bool   // keep rewritten or deleted refs under refs/archive/
	Protocol     string // clone over "https" or "ssh"
}

// DefaultOptions are used when neither the source nor [archive] sets a key.
var DefaultOptions = Options{Metadata: true, Preserve: true, Protocol: "https"}

// optionKeys maps lower-case keys (as git reports them) to the spelling
// written to the file.
var optionKeys = map[string]string{
	"skipforks":    "skipForks",
	"skiparchived": "skipArchived",
	"maxsize":      "maxSize",
	"wiki":         "wiki",
	"lfs":          "lfs",
	"metadata":     "metadata",
	"preserve":     "preserve",
	"protocol":     "protocol",
}

// OptionKey returns the canonical spelling of an option key, or "".
func OptionKey(k string) string { return optionKeys[strings.ToLower(k)] }

// Source is a configured origin of repositories: an organisation, user or
// group on a forge, or a single repository.
type Source struct {
	Name     string // section name and identity: "github.com/termux"
	URL      string // web or clone URL the source was added with
	Provider string // github, gitlab, gitea or git
	Type     string // owner or repo
	API      string // API base URL override (self-hosted, tests)
	Insecure bool   // allow plain http:// and git:// transports
	TokenEnv string // environment variable holding an API/git token
	Opts     map[string]string
}

// Config is the parsed .git-archive/config file.
type Config struct {
	path     string
	git      *gitx.Runner
	Version  int
	Layout   string
	Jobs     int
	Timeout  time.Duration
	Retries  int
	Excludes []string
	Defaults map[string]string // option defaults from [archive]
	Sources  []*Source
	Warnings []string // unknown keys and similar non-fatal problems
}

const configHeader = `# git-archive configuration
#
# This file uses git-config(1) syntax. It may be edited by hand; comments
# survive when git-archive modifies it. See git-archive(1) for all keys.
#
# [archive]  layout, jobs, timeout, retries, exclude (repeatable) and
#            defaults for the per-source options.
# [source "<host>/<path>"]
#            url, provider, type, api, insecure, tokenEnv and the options
#            skipForks, skipArchived, maxSize, wiki, lfs, metadata,
#            preserve, protocol.
`

// writeNewConfig creates the file with the header and basic settings.
func writeNewConfig(ctx context.Context, git *gitx.Runner, path, layout string) (*Config, error) {
	if err := os.WriteFile(path, []byte(configHeader), 0o644); err != nil {
		return nil, err
	}
	c := &Config{path: path, git: git}
	if err := c.set(ctx, "archive.version", "1"); err != nil {
		return nil, err
	}
	if err := c.set(ctx, "archive.layout", layout); err != nil {
		return nil, err
	}
	return LoadConfig(ctx, git, path)
}

// LoadConfig reads and validates a configuration file.
func LoadConfig(ctx context.Context, git *gitx.Runner, path string) (*Config, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	// --file implies --no-includes: include.path directives are not
	// followed, so the configuration cannot pull in other files.
	out, err := git.Run(ctx, gitx.Cmd{Args: []string{"config", "--file", path, "--null", "--list"}})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	c := &Config{
		path: path, git: git,
		Version: 1, Layout: LayoutWorktree, Jobs: DefaultJobs, Timeout: DefaultTimeout, Retries: DefaultRetries,
		Defaults: map[string]string{},
	}
	byName := map[string]*Source{}
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		key, val, hasVal := strings.Cut(string(rec), "\n")
		if !hasVal {
			val = "true" // "[archive] wiki" with no value means true, as in git
		}
		section, sub, name := splitKey(key)
		switch section {
		case "archive":
			if sub != "" {
				c.warn("unknown section [archive %q]", sub)
				continue
			}
			if err := c.setArchiveKey(name, val); err != nil {
				return nil, fmt.Errorf("%s: archive.%s: %w", path, name, err)
			}
		case "source":
			if sub == "" {
				c.warn("[source] section without a name")
				continue
			}
			s := byName[sub]
			if s == nil {
				s = &Source{Name: sub, Opts: map[string]string{}}
				byName[sub] = s
				c.Sources = append(c.Sources, s)
			}
			if err := c.setSourceKey(s, name, val); err != nil {
				return nil, fmt.Errorf("%s: source %q: %s: %w", path, sub, name, err)
			}
		default:
			c.warn("ignoring unknown key %q", key)
		}
	}
	if c.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported archive.version %d (this git-archive understands 1)", path, c.Version)
	}
	if _, err := ParseOptions(nil, c.Defaults); err != nil {
		return nil, fmt.Errorf("%s: [archive]: %w", path, err)
	}
	for _, s := range c.Sources {
		if s.URL == "" || s.Provider == "" || s.Type == "" {
			return nil, fmt.Errorf("%s: source %q needs url, provider and type", path, s.Name)
		}
		if _, err := c.Options(s); err != nil {
			return nil, fmt.Errorf("%s: source %q: %w", path, s.Name, err)
		}
	}
	return c, nil
}

func (c *Config) warn(format string, args ...any) {
	c.Warnings = append(c.Warnings, fmt.Sprintf(format, args...))
}

// splitKey splits "section.sub.section.name" at the first and last dot.
func splitKey(key string) (section, sub, name string) {
	first := strings.IndexByte(key, '.')
	last := strings.LastIndexByte(key, '.')
	if first < 0 {
		return key, "", ""
	}
	if first == last {
		return key[:first], "", key[first+1:]
	}
	return key[:first], key[first+1 : last], key[last+1:]
}

func (c *Config) setArchiveKey(name, val string) error {
	var err error
	switch name {
	case "version":
		c.Version, err = strconv.Atoi(val)
	case "layout":
		if val != LayoutWorktree && val != LayoutBare {
			return fmt.Errorf("must be %q or %q", LayoutWorktree, LayoutBare)
		}
		c.Layout = val
	case "jobs":
		c.Jobs, err = strconv.Atoi(val)
		if err == nil && (c.Jobs < 1 || c.Jobs > 256) {
			err = errors.New("must be between 1 and 256")
		}
	case "timeout":
		c.Timeout, err = units.ParseDuration(val)
	case "retries":
		c.Retries, err = strconv.Atoi(val)
		if err == nil && (c.Retries < 0 || c.Retries > 20) {
			err = errors.New("must be between 0 and 20")
		}
	case "exclude":
		if err = ValidatePattern(strings.TrimPrefix(val, "!")); err == nil {
			c.Excludes = append(c.Excludes, val)
		}
	default:
		if k := OptionKey(name); k != "" {
			c.Defaults[k] = val
		} else {
			c.warn("ignoring unknown key archive.%s", name)
		}
	}
	return err
}

func (c *Config) setSourceKey(s *Source, name, val string) error {
	var err error
	switch name {
	case "url":
		s.URL = val
	case "provider":
		s.Provider = val
	case "type":
		if val != "owner" && val != "repo" {
			return errors.New(`must be "owner" or "repo"`)
		}
		s.Type = val
	case "api":
		s.API = val
	case "insecure":
		s.Insecure, err = parseBool(val)
	case "tokenenv":
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(val) {
			return errors.New("must be an environment variable name")
		}
		s.TokenEnv = val
	default:
		if k := OptionKey(name); k != "" {
			s.Opts[k] = val
		} else {
			c.warn("ignoring unknown key source.%s.%s", s.Name, name)
		}
	}
	return err
}

// Options resolves the effective options of a source.
func (c *Config) Options(s *Source) (Options, error) {
	var opts map[string]string
	if s != nil {
		opts = s.Opts
	}
	return ParseOptions(c.Defaults, opts)
}

// ParseOptions applies the raw settings in layers (later layers win) on
// top of DefaultOptions.
func ParseOptions(layers ...map[string]string) (Options, error) {
	o := DefaultOptions
	for _, layer := range layers {
		for k, v := range layer {
			var err error
			switch k {
			case "skipForks":
				o.SkipForks, err = parseBool(v)
			case "skipArchived":
				o.SkipArchived, err = parseBool(v)
			case "maxSize":
				o.MaxSize, err = units.ParseSize(v)
			case "wiki":
				o.Wiki, err = parseBool(v)
			case "lfs":
				o.LFS, err = parseBool(v)
			case "metadata":
				o.Metadata, err = parseBool(v)
			case "preserve":
				o.Preserve, err = parseBool(v)
			case "protocol":
				if v != "https" && v != "ssh" {
					err = errors.New(`must be "https" or "ssh"`)
				}
				o.Protocol = v
			}
			if err != nil {
				return o, fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	return o, nil
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "true", "yes", "on", "1":
		return true, nil
	case "false", "no", "off", "0", "":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean %q", v)
}

// Source returns the source with the given name.
func (c *Config) Source(name string) *Source {
	for _, s := range c.Sources {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// Path returns the configuration file path.
func (c *Config) Path() string { return c.path }

// SaveSource writes s (creating or updating its section) and reloads.
func (c *Config) SaveSource(ctx context.Context, s *Source) error {
	prefix := "source." + s.Name + "."
	fields := []struct{ key, val string }{
		{"url", s.URL}, {"provider", s.Provider}, {"type", s.Type},
		{"api", s.API}, {"tokenEnv", s.TokenEnv},
	}
	if s.Insecure {
		fields = append(fields, struct{ key, val string }{"insecure", "true"})
	}
	for _, f := range fields {
		if f.val == "" {
			continue
		}
		if err := c.set(ctx, prefix+f.key, f.val); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(s.Opts))
	for k := range s.Opts {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if err := c.set(ctx, prefix+k, s.Opts[k]); err != nil {
			return err
		}
	}
	return c.reload(ctx)
}

// RemoveSource deletes a source section.
func (c *Config) RemoveSource(ctx context.Context, name string) error {
	if _, err := c.git.Run(ctx, gitx.Cmd{Args: []string{"config", "--file", c.path, "--remove-section", "source." + name}}); err != nil {
		return fmt.Errorf("removing source %q: %w", name, err)
	}
	return c.reload(ctx)
}

// AddExclude appends a pattern to the exclusion list. A previous
// occurrence of the same pattern is removed first, so that the new entry
// takes precedence over earlier negations.
func (c *Config) AddExclude(ctx context.Context, pattern string) error {
	if err := ValidatePattern(strings.TrimPrefix(pattern, "!")); err != nil {
		return err
	}
	if err := c.RemoveExclude(ctx, pattern); err != nil {
		return err
	}
	if _, err := c.git.Run(ctx, gitx.Cmd{Args: []string{"config", "--file", c.path, "--add", "archive.exclude", pattern}}); err != nil {
		return err
	}
	return c.reload(ctx)
}

// RemoveExclude removes every occurrence of exactly this pattern.
func (c *Config) RemoveExclude(ctx context.Context, pattern string) error {
	if !slices.Contains(c.Excludes, pattern) {
		return nil
	}
	re := "^" + regexp.QuoteMeta(pattern) + "$"
	if err := c.unset(ctx, "archive.exclude", re); err != nil {
		return err
	}
	return c.reload(ctx)
}

func (c *Config) set(ctx context.Context, key, val string) error {
	_, err := c.git.Run(ctx, gitx.Cmd{Args: []string{"config", "--file", c.path, "--replace-all", key, val}})
	if err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}
	return nil
}

func (c *Config) unset(ctx context.Context, key, valueRegex string) error {
	args := []string{"config", "--file", c.path, "--unset-all", key}
	if valueRegex != "" {
		args = append(args, valueRegex)
	}
	_, err := c.git.Run(ctx, gitx.Cmd{Args: args})
	var ge *gitx.Error
	if errors.As(err, &ge) && ge.Exit == 5 { // nothing to unset
		return nil
	}
	return err
}

func (c *Config) reload(ctx context.Context) error {
	n, err := LoadConfig(ctx, c.git, c.path)
	if err != nil {
		return err
	}
	*c = *n
	return nil
}
