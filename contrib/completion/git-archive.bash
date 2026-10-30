# bash completion for git-archive
# SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
# SPDX-License-Identifier: Apache-2.0

_git_archive() {
    local cur=${COMP_WORDS[COMP_CWORD]} prev=${COMP_WORDS[COMP_CWORD-1]} cmd= w
    local commands="sync add remove rm exclude include list ls status verify gc config completion help version"
    for w in "${COMP_WORDS[@]:1:COMP_CWORD-1}"; do
        case " $commands " in *" $w "*) cmd=$w; break ;; esac
    done
    case $prev in
        -C|--archive-dir) COMPREPLY=($(compgen -d -- "$cur")); return ;;
        -p|--provider) COMPREPLY=($(compgen -W "github gitlab gitea git" -- "$cur")); return ;;
        --layout) COMPREPLY=($(compgen -W "worktree bare" -- "$cur")); return ;;
        --color) COMPREPLY=($(compgen -W "auto always never" -- "$cur")); return ;;
    esac
    if [[ $cur == -* ]]; then
        local opts
        case $cmd in
            sync|"") opts="--api --archive-dir --color --dry-run --exclude --help --init --insecure --jobs --json --layout --lfs --max-size --no-metadata --no-preserve --only --provider --quiet --skip-archived --skip-forks --ssh --timeout --token-env --verbose --version --wiki" ;;
            add) opts="--api --archive-dir --color --exclude --help --init --insecure --layout --lfs --max-size --no-metadata --no-preserve --provider --quiet --skip-archived --skip-forks --ssh --token-env --verbose --version --wiki" ;;
            remove|rm) opts="--archive-dir --color --help --quiet --verbose --version" ;;
            exclude) opts="--archive-dir --color --help --quiet --verbose --version" ;;
            include) opts="--archive-dir --color --help --quiet --verbose --version" ;;
            list|ls|status) opts="--archive-dir --color --help --json --quiet --sources --verbose --version" ;;
            verify) opts="--archive-dir --color --help --jobs --json --quick --quiet --remote --timeout --verbose --version" ;;
            gc) opts="--aggressive --archive-dir --color --help --jobs --json --quiet --timeout --verbose --version" ;;
            config) opts="--archive-dir --color --help --quiet --verbose --version" ;;
            completion) opts="--archive-dir --color --help --quiet --verbose --version" ;;
            help) opts="--archive-dir --color --help --quiet --verbose --version" ;;
            version) opts="--archive-dir --color --help --quiet --verbose --version" ;;
        esac
        COMPREPLY=($(compgen -W "$opts" -- "$cur"))
        return
    fi
    case $cmd in
        "") COMPREPLY=($(compgen -W "$commands" -- "$cur")) ;;
        completion) COMPREPLY=($(compgen -W "bash zsh fish" -- "$cur")) ;;
        help) COMPREPLY=($(compgen -W "$commands" -- "$cur")) ;;
    esac
}
complete -F _git_archive git-archive
