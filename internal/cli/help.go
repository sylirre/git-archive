package cli

import (
	"fmt"
	"strings"
)

const overview = `git-archive keeps an archive of git repositories: whole organizations,
users and groups, or single repositories, cloned once and updated on
every run. Nothing is ever deleted: repositories removed upstream are
kept and flagged, and history rewritten by force-pushes is preserved
under refs/archive/.

Usage:
  git-archive [OPTIONS] [SOURCE...]     add sources (if any) and sync
  git-archive COMMAND [OPTIONS] [ARGS]

Examples:
  git-archive https://github.com/termux            create an archive here
  git-archive --provider github termux sylirre     several sources at once
  git-archive                                      update the archive here
  git-archive -C ~/archive --quiet                 update quietly (cron)
  git-archive exclude termux/termux-packages       stop updating one repo
  git-archive verify --remote                      fsck + compare upstream

Commands:
`

const syncDetails = `Sources:
  https://github.com/termux                all repositories of an organization or user
  https://github.com/termux/termux-app     a single repository
  --provider github termux                 the same as the first line
  https://gitlab.com/group/subgroup        GitLab groups (nested), users, projects
  https://codeberg.org/forgejo             Gitea and Forgejo owners and repositories
  git@host:owner/repo.git                  any git repository over ssh
  https://git.example.org/path/repo.git    any git repository over https

Running without SOURCE updates the archive in the current directory (or
-C DIR, or $GIT_ARCHIVE_DIR). Giving sources to a directory that is not
an archive yet creates one. Options such as --skip-forks are stored
with the sources named in the same invocation; repeat "git-archive add
--skip-forks SOURCE" to change an existing source, or edit the
configuration ("git-archive config --edit").

Tokens (raise API rate limits, give access to private repositories):
  GITHUB_TOKEN or GH_TOKEN      github.com
  GITLAB_TOKEN                  gitlab.com
  CODEBERG_TOKEN or GITEA_TOKEN codeberg.org
  --token-env VAR               any source, including self-hosted instances
Tokens are never written to disk; git receives them through a
credential helper scoped to the source's host.
`

const patternDetails = `Patterns are matched against repository IDs (host/owner/repo) with
shell globs (*, ?, [...]); "**" spans path components. A pattern may
match any part of the ID, so these all select the same repository:
  termux-packages   termux/termux-packages   github.com/termux/termux-packages
Matching a leading part selects everything below it ("github.com/termux").
Excluded repositories that were already archived stay on disk but are
no longer updated. "include" removes an exclusion or, when a broader
pattern still applies, adds a negated pattern ("!termux/termux-app").
`

const configDetails = `Arguments are passed to "git config --file .git-archive/config", e.g.:
  git-archive config --edit                    open the file in $EDITOR
  git-archive config archive.jobs 8            set a value
  git-archive config source.github.com/termux.wiki true

[archive] keys: layout, jobs, timeout, retries, exclude (repeatable);
per-source options (as defaults or in [source "NAME"]): skipForks,
skipArchived, maxSize, wiki, lfs, metadata, preserve, protocol.
`

const footer = `
Exit status: 0 success, 1 a repository or command failed, 2 usage error,
130 interrupted. See git-archive(1) for details.
`

// helpText renders help for a command; for the default command it
// includes the overview and the command list.
func helpText(c *command, fs *flagSet) string {
	var b strings.Builder
	if c == nil || c.name == "sync" {
		b.WriteString(overview)
		for _, cmd := range commands {
			name := cmd.name
			if cmd.args != "" {
				name += " " + cmd.args
			}
			fmt.Fprintf(&b, "  %-30s %s\n", name, cmd.summary)
		}
		b.WriteString("\n")
		if c == nil {
			c = lookupCommand("sync")
		}
		b.WriteString(c.details)
	} else {
		fmt.Fprintf(&b, "Usage: git-archive %s [OPTIONS] %s\n\n%s.\n", c.name, c.args, upperFirst(c.summary))
		if len(c.aliases) > 0 {
			fmt.Fprintf(&b, "Aliases: %s\n", strings.Join(c.aliases, ", "))
		}
		if c.details != "" {
			b.WriteString("\n" + c.details)
		}
	}
	if fs == nil {
		fs = &flagSet{}
		a := &app{}
		a.globalFlags(fs)
		if c.flags != nil {
			c.flags(a, fs)
		}
	}
	b.WriteString("\nOptions:\n")
	b.WriteString(fs.usage())
	b.WriteString(footer)
	return b.String()
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
