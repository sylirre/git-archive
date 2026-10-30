package cli

import (
	"fmt"
	"strings"
)

// Completion scripts are generated from the command table, so they can
// never drift from the actual options.

func commandFlags(c *command) *flagSet {
	fs := &flagSet{}
	a := &app{}
	a.globalFlags(fs)
	if c.flags != nil {
		c.flags(a, fs)
	}
	return fs
}

func commandNames() []string {
	var names []string
	for _, c := range commands {
		names = append(names, c.name)
		names = append(names, c.aliases...)
	}
	return names
}

// valueWords lists fixed completions for option values.
var valueWords = map[string]string{
	"provider": "github gitlab gitea git",
	"layout":   "worktree bare",
	"color":    "auto always never",
}

func completionScript(shell string) (string, error) {
	switch shell {
	case "bash":
		return bashCompletion(), nil
	case "zsh":
		return zshCompletion(), nil
	case "fish":
		return fishCompletion(), nil
	}
	return "", fmt.Errorf("unsupported shell %q (bash, zsh or fish)", shell)
}

func bashCompletion() string {
	var b strings.Builder
	names := strings.Join(commandNames(), " ")
	b.WriteString("# bash completion for git-archive\n_git_archive() {\n")
	b.WriteString("    local cur=${COMP_WORDS[COMP_CWORD]} prev=${COMP_WORDS[COMP_CWORD-1]} cmd= w\n")
	fmt.Fprintf(&b, "    local commands=%q\n", names)
	b.WriteString("    for w in \"${COMP_WORDS[@]:1:COMP_CWORD-1}\"; do\n")
	b.WriteString("        case \" $commands \" in *\" $w \"*) cmd=$w; break ;; esac\n    done\n")
	b.WriteString("    case $prev in\n")
	b.WriteString("        -C|--archive-dir) COMPREPLY=($(compgen -d -- \"$cur\")); return ;;\n")
	for _, kv := range [][2]string{{"-p|--provider", "provider"}, {"--layout", "layout"}, {"--color", "color"}} {
		fmt.Fprintf(&b, "        %s) COMPREPLY=($(compgen -W %q -- \"$cur\")); return ;;\n", kv[0], valueWords[kv[1]])
	}
	b.WriteString("    esac\n    if [[ $cur == -* ]]; then\n        local opts\n        case $cmd in\n")
	for _, c := range commands {
		pat := strings.Join(append([]string{c.name}, c.aliases...), "|")
		if c.name == "sync" {
			pat += "|\"\""
		}
		fmt.Fprintf(&b, "            %s) opts=%q ;;\n", pat, strings.Join(commandFlags(c).longNames(), " "))
	}
	b.WriteString("        esac\n        COMPREPLY=($(compgen -W \"$opts\" -- \"$cur\"))\n        return\n    fi\n")
	b.WriteString("    case $cmd in\n")
	b.WriteString("        \"\") COMPREPLY=($(compgen -W \"$commands\" -- \"$cur\")) ;;\n")
	b.WriteString("        completion) COMPREPLY=($(compgen -W \"bash zsh fish\" -- \"$cur\")) ;;\n")
	b.WriteString("        help) COMPREPLY=($(compgen -W \"$commands\" -- \"$cur\")) ;;\n")
	b.WriteString("    esac\n}\ncomplete -F _git_archive git-archive\n")
	return b.String()
}

func zshEscape(s string) string {
	r := strings.NewReplacer("'", "'\\''", "[", "\\[", "]", "\\]", ":", "\\:")
	return r.Replace(s)
}

func zshSpecs(fs *flagSet) []string {
	var specs []string
	for _, f := range fs.flags {
		help := zshEscape(f.help)
		action := ""
		if f.arg != "" {
			switch {
			case f.long == "archive-dir":
				action = ":" + f.arg + ":_files -/"
			case valueWords[f.long] != "":
				action = ":" + f.arg + ":(" + valueWords[f.long] + ")"
			default:
				action = ":" + f.arg + ": "
			}
		}
		names := "--" + f.long
		if f.short != 0 {
			specs = append(specs, fmt.Sprintf("'(-%c --%s)'{-%c,--%s}'[%s]%s'", f.short, f.long, f.short, f.long, help, action))
			continue
		}
		specs = append(specs, fmt.Sprintf("'%s[%s]%s'", names, help, action))
	}
	return specs
}

func zshCompletion() string {
	var b strings.Builder
	b.WriteString("#compdef git-archive\n\n_git_archive() {\n  local -a cmds\n  cmds=(\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "    '%s:%s'\n", c.name, zshEscape(c.summary))
	}
	b.WriteString("  )\n  local i cmd\n  for (( i = 2; i < CURRENT; i++ )); do\n    case ${words[i]} in\n")
	fmt.Fprintf(&b, "      %s) cmd=${words[i]}; break ;;\n", strings.Join(commandNames(), "|"))
	b.WriteString("    esac\n  done\n")
	b.WriteString("  if [[ -z $cmd && ${words[CURRENT]} != -* ]]; then\n    _describe -t commands 'git-archive command' cmds\n  fi\n")
	b.WriteString("  case ${cmd:-sync} in\n")
	for _, c := range commands {
		pat := strings.Join(append([]string{c.name}, c.aliases...), "|")
		rest := "'*:argument: '"
		switch c.name {
		case "completion":
			rest = "'1:shell:(bash zsh fish)'"
		case "help":
			rest = "'1:command:(" + strings.Join(commandNames(), " ") + ")'"
		}
		fmt.Fprintf(&b, "    %s)\n      _arguments -s \\\n        %s \\\n        %s ;;\n", pat,
			strings.Join(zshSpecs(commandFlags(c)), " \\\n        "), rest)
	}
	b.WriteString("  esac\n}\n\n_git_archive \"$@\"\n")
	return b.String()
}

func fishCompletion() string {
	var b strings.Builder
	others := func(c *command) string {
		var n []string
		for _, x := range commands {
			if x != c {
				n = append(n, x.name)
				n = append(n, x.aliases...)
			}
		}
		return strings.Join(n, " ")
	}
	b.WriteString("# fish completion for git-archive\ncomplete -c git-archive -f\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "complete -c git-archive -n __fish_use_subcommand -a %s -d %s\n", c.name, fishQuote(c.summary))
	}
	for _, c := range commands {
		cond := "__fish_seen_subcommand_from " + strings.Join(append([]string{c.name}, c.aliases...), " ")
		if c.name == "sync" {
			cond = "not __fish_seen_subcommand_from " + others(c)
		}
		for _, f := range commandFlags(c).flags {
			line := fmt.Sprintf("complete -c git-archive -n '%s' -l %s", cond, f.long)
			if f.short != 0 {
				line += fmt.Sprintf(" -s %c", f.short)
			}
			if f.arg != "" {
				line += " -r"
				switch {
				case f.long == "archive-dir":
					line += " -a '(__fish_complete_directories)'"
				case valueWords[f.long] != "":
					line += fmt.Sprintf(" -a '%s'", valueWords[f.long])
				}
			}
			line += " -d " + fishQuote(f.help)
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("complete -c git-archive -n '__fish_seen_subcommand_from completion' -a 'bash zsh fish'\n")
	return b.String()
}

// fishQuote single-quotes s for fish, where only \\ and \' are special.
func fishQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
