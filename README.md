# git-archive

Keep a complete, verifiable archive of git repositories: whole GitHub,
GitLab and Gitea/Forgejo organizations, users and groups, or single
repositories from any git server. They are cloned once, updated in
parallel with live progress, and never lost:

- repositories deleted upstream are **kept** and flagged as *gone*;
- branches and tags that upstream **force-pushes, moves or deletes are
  preserved** under `refs/archive/` before they are replaced;
- excluded repositories stay on disk but are no longer updated;
- `git-archive verify` checks integrity (`git fsck`) and, with `--remote`,
  how far each copy is behind upstream without downloading anything.

```console
$ mkdir archive && cd archive
$ git-archive http://github.com/termux
http://github.com/termux: using https:// instead of http://
Added source github.com/termux: every repository of this github owner
 ⠹ termux/termux-packages   receiving objects   37% ███████▋            412.3 MiB | 12.3 MiB/s
 ⠹ termux/termux-app        resolving deltas    82% ████████████████▍
 Syncing 57/124 ███████████████░░░░░░░░░░░  ✓ 54 cloned  ✗ 1 failed  2m13s
...
$ git-archive                  # later: update the archive in the current directory
$ git-archive -C ~/archive -q  # or elsewhere, silently (cron)
```

## Installation

Requirements: git ≥ 2.25 (older versions work with a warning), Go ≥ 1.24
to build. `git-lfs` is needed only for `--lfs`.

```sh
make
sudo make install            # PREFIX=/usr/local; DESTDIR is honoured
```

`make install` installs the binary, the man page `git-archive(1)` and
completions for bash, zsh and fish. The binary is static
(`CGO_ENABLED=0`) and cross-compiles to any platform Go supports,
including Android/Termux.

The name differs from git's built-in `git archive` (with a space), which
does something unrelated. Call this tool as `git-archive`.

## Usage

```
git-archive [OPTIONS] [SOURCE...]     add sources (if any) and sync
git-archive COMMAND [OPTIONS] [ARGS]
```

| Command | Purpose |
|---|---|
| `sync [SOURCE...]` | default command: add sources, clone new and update existing repositories |
| `add SOURCE...` | add sources or change their options without syncing |
| `remove SOURCE...` | forget sources; their repositories stay on disk |
| `exclude PATTERN...` | stop cloning/updating matching repositories |
| `include PATTERN...` | undo an exclusion |
| `list [PATTERN...]` | repositories and status (`--sources`: sources); offline |
| `verify [PATTERN...]` | `git fsck --strict`, remote URL, HEAD, modified worktrees; `--remote` compares with upstream |
| `gc [PATTERN...]` | `git gc` in parallel, removes leftovers of interrupted clones |
| `config [ARGS]` | `git config` on the archive configuration (`--edit`, `--list`, ...) |
| `completion SHELL` | completion script for bash, zsh or fish |

### Sources

| Form | Meaning |
|---|---|
| `https://github.com/termux` | every repository of an organization or user |
| `https://github.com/termux/termux-app` | one repository |
| `--provider github termux` | the same as the first line |
| `https://gitlab.com/group/subgroup` | GitLab group (with subgroups), user or project |
| `https://codeberg.org/forgejo` | Gitea/Forgejo owner or repository |
| `git@host:owner/repo.git`, `https://host/path/repo.git`, `/srv/git/x.git` | any single repository |

Several sources can be given at once. Self-hosted forges are recognised
by `gitlab.`/`gitea.`/`forgejo.` host names, or by `--provider`.

### Selection

Options given with sources are stored per source:

```sh
git-archive --skip-forks --skip-archived --max-size 2G https://github.com/termux
git-archive add --wiki --lfs https://github.com/termux   # change an existing source
git-archive exclude termux-packages 'termux/*-legacy'
git-archive include termux/termux-app                    # even if 'termux' is excluded
git-archive sync --only 'termux/termux-a*'               # sync a subset once
git-archive --dry-run                                    # plan only
```

Patterns are matched against IDs `host/owner/repo`. Each part is a
shell glob, `**` spans parts, and a pattern may match any part of the
ID: `termux-packages`, `termux/termux-packages` and
`github.com/termux/termux-packages` are equivalent. `github.com/termux`
selects everything below it, and `!pattern` re-includes.

### Tokens

Without a token, GitHub allows 60 API requests per hour, which is
enough for occasional runs over a few organizations. Tokens also give
access to private repositories:

| Variable | Used for |
|---|---|
| `GITHUB_TOKEN`, `GH_TOKEN` | github.com |
| `GITLAB_TOKEN` | gitlab.com |
| `CODEBERG_TOKEN`, `GITEA_TOKEN` | codeberg.org |
| `--token-env VAR` | any source, including self-hosted instances |

Tokens are never written to disk. git receives them from a credential
helper scoped to the source's host, so they do not appear in command
lines or `.git/config`. A github.com token is never sent to any other
host. Tip: `GITHUB_TOKEN=$(gh auth token) git-archive`.

## Archive layout

```
archive/
├── .git-archive/
│   ├── config                       sources, exclusions, settings (git-config syntax)
│   ├── state.json                   bookkeeping; rebuilt if lost
│   ├── metadata/github.com/termux/termux-app.json
│   └── lock
└── github.com/
    └── termux/
        ├── termux-app/              working tree of the default branch (layout "worktree")
        ├── termux-app.wiki/         with --wiki
        └── termux-packages/
```

With `--layout bare` (chosen when the archive is created), repositories
are bare (`termux-app.git`): about half the disk space, and no
repository content is ever written out as files.

In every repository:

- all upstream branches and tags are fetched (`--prune`, so the archive
  mirrors upstream exactly);
- `refs/archive/<UTC time>/{heads,tags}/<name>` keeps every tip that
  upstream rewrote or deleted, so the objects survive `git gc`;
- the working tree follows the upstream default branch (including
  renames such as `master` → `main`). Local modifications are never
  overwritten: the checkout is skipped with a warning.

Renamed repositories (detected by forge ID) are moved instead of cloned
again. Interrupted clones never look like repositories. They are built
in `<name>~partial` and renamed into place only on success.

## Configuration

`.git-archive/config` may be edited by hand or with `git-archive config`:

```ini
[archive]
	layout = worktree
	jobs = 8              # parallel repositories (default 4)
	timeout = 10m         # abort git after this long without output
	retries = 2           # retries of transient network failures
	exclude = termux-packages
	wiki = true           # any source option here is a default for all sources
[source "github.com/termux"]
	url = https://github.com/termux
	provider = github
	type = owner
	skipForks = true
	maxSize = 2G
```

Per-source options: `skipForks`, `skipArchived`, `maxSize`, `wiki`,
`lfs`, `metadata` (default true), `preserve` (default true), `protocol`
(`https` or `ssh`), `tokenEnv`, `api`, `insecure`.

## Scheduling

cron:

```cron
15 3 * * *  git-archive -C /srv/archive --quiet
```

systemd user units, enabled with
`systemctl --user enable --now git-archive.timer`:

```ini
# ~/.config/systemd/user/git-archive.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/git-archive -C %h/archive --quiet
```

```ini
# ~/.config/systemd/user/git-archive.timer
[Timer]
OnCalendar=daily
Persistent=true

[Install]
WantedBy=timers.target
```

Exit status 0 means success, 1 that a repository or the command
failed, 2 a usage error, and 130 an interruption. `--quiet` prints only
problems, so cron mails only when something needs attention. `--json`
writes a full report to standard output.

## Security model

Everything that comes from the network is treated as hostile.

- **Paths.** Owner and repository names from APIs must consist of
  `[A-Za-z0-9._-]`. They may not be `.`, `..` or `.git`, and may not
  start with `-`. Containment below the archive is checked again after
  joining. Listings that report repositories outside the requested
  owner are ignored.
- **Clone URLs** are built from validated parts, never taken from API
  responses. That stops a server from redirecting git to another host,
  a local path or a transport helper.
- **git is hardened** with command-line configuration, which neither
  repositories nor user files can override:
  - hooks and fsmonitor are disabled;
  - `transfer.fsckObjects` rejects malicious trees (entries named
    `.git` or `..`, hostile `.gitmodules`) before anything is stored;
  - `protocol.allow=never` is set, with only the transports the
    configured sources need allowed (`ext::` and `file://` are refused);
  - submodules are never followed, and LFS content is never fetched
    during checkout;
  - NTFS/HFS+ path aliases are rejected;
  - `safe.bareRepository=explicit` is set.
- **Isolation.** git runs in a new session without a controlling
  terminal, in its own process group, with credential prompts and
  askpass disabled. Nothing it spawns can prompt on or draw over your
  terminal, and the whole group is killed on interruption or after
  `timeout` without output. The environment is cleaned of variables
  like `GIT_DIR` that would redirect git.
- **Terminal output.** Every string from servers and repositories is
  stripped of control characters, ANSI escapes and bidi overrides
  before it is printed.
- **HTTP.** TLS ≥ 1.2, timeouts, a 32 MiB response limit, a JSON
  content-type check, and no redirects or pagination links that leave
  the API host.

When you browse a working-tree archive with your own git, also set
`git config --global safe.bareRepository explicit`.

## Development

```sh
make test     # all tests
make race     # with the race detector
make vet      # go vet + gofmt check
```

The integration tests run a real forge in-process: `git http-backend`
behind `net/http/cgi` serves smart HTTP, and a GitHub-compatible API is
generated from the served repositories. They cover clone and update,
force-push and tag preservation, which must survive
`gc --prune=now`, both layouts, default branch renames, local
modifications, exclusions and filters, upstream deletion and rename,
listing outages, wikis, empty repositories, injected 502 errors and
retries, a repository carrying a `.git` tree entry, hostile API names,
verify with corrupted packs, and the CLI end to end. Fuzz tests cover
name validation and terminal sanitization.

| Package | Role |
|---|---|
| `internal/safe` | validation of remote names and paths, terminal sanitization |
| `internal/gitx` | hardened git runner: environment, config overrides, progress parsing, watchdog, process groups |
| `internal/forge` | source parsing; GitHub, GitLab, Gitea clients; hardened HTTP |
| `internal/archive` | configuration, state, locking, patterns, layout |
| `internal/engine` | sync/verify/gc/inventory, scheduling, ref preservation |
| `internal/ui` | live progress, plain and quiet output, summaries |
| `internal/cli` | commands, argument parsing, help, completions |
