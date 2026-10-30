package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// flag describes one option.
type flag struct {
	long  string
	short byte
	arg   string // metavariable; empty for boolean flags
	help  string
	set   func(string) error
}

// flagSet is an ordered collection of options.
type flagSet struct {
	flags []*flag
}

func (fs *flagSet) boolVar(p *bool, long string, short byte, help string) {
	fs.flags = append(fs.flags, &flag{long: long, short: short, help: help, set: func(string) error { *p = true; return nil }})
}

func (fs *flagSet) stringVar(p *string, long string, short byte, arg, help string) {
	fs.flags = append(fs.flags, &flag{long: long, short: short, arg: arg, help: help, set: func(v string) error { *p = v; return nil }})
}

func (fs *flagSet) listVar(p *[]string, long string, short byte, arg, help string) {
	fs.flags = append(fs.flags, &flag{long: long, short: short, arg: arg, help: help + " (repeatable)",
		set: func(v string) error { *p = append(*p, v); return nil }})
}

func (fs *flagSet) intVar(p *int, long string, short byte, arg, help string) {
	fs.flags = append(fs.flags, &flag{long: long, short: short, arg: arg, help: help, set: func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("--%s expects a number, got %q", long, v)
		}
		*p = n
		return nil
	}})
}

func (fs *flagSet) funcVar(long string, short byte, arg, help string, fn func(string) error) {
	fs.flags = append(fs.flags, &flag{long: long, short: short, arg: arg, help: help, set: fn})
}

func (fs *flagSet) lookupLong(name string) *flag {
	for _, f := range fs.flags {
		if f.long == name {
			return f
		}
	}
	return nil
}

func (fs *flagSet) lookupShort(c byte) *flag {
	for _, f := range fs.flags {
		if f.short != 0 && f.short == c {
			return f
		}
	}
	return nil
}

// UsageError is a command-line mistake (exit status 2).
type UsageError struct{ msg string }

func (e *UsageError) Error() string { return e.msg }

func usageErr(format string, args ...any) error { return &UsageError{fmt.Sprintf(format, args...)} }

// parse processes GNU-style arguments: options may appear anywhere,
// "--name=value", "--name value", "-x value", "-xvalue" and bundled
// boolean short options ("-qv") are accepted, and "--" ends options.
func (fs *flagSet) parse(args []string) ([]string, error) {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(pos, args[i+1:]...), nil
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a[2:], "=")
			f := fs.lookupLong(name)
			if f == nil {
				return nil, usageErr("unknown option --%s%s", name, fs.suggest(name))
			}
			if f.arg == "" {
				if hasVal {
					return nil, usageErr("option --%s takes no value", name)
				}
				if err := f.set(""); err != nil {
					return nil, err
				}
				continue
			}
			if !hasVal {
				if i+1 >= len(args) {
					return nil, usageErr("option --%s needs a value (%s)", name, f.arg)
				}
				i++
				val = args[i]
			}
			if err := f.set(val); err != nil {
				return nil, usageErr("%v", err)
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for j := 1; j < len(a); j++ {
				f := fs.lookupShort(a[j])
				if f == nil {
					return nil, usageErr("unknown option -%c", a[j])
				}
				if f.arg == "" {
					if err := f.set(""); err != nil {
						return nil, err
					}
					continue
				}
				val := a[j+1:]
				if val == "" {
					if i+1 >= len(args) {
						return nil, usageErr("option -%c needs a value (%s)", a[j], f.arg)
					}
					i++
					val = args[i]
				}
				if err := f.set(val); err != nil {
					return nil, usageErr("%v", err)
				}
				break
			}
		default:
			pos = append(pos, a)
		}
	}
	return pos, nil
}

// suggest proposes the closest known long option for a typo.
func (fs *flagSet) suggest(name string) string {
	best, bestD := "", 3
	for _, f := range fs.flags {
		if d := editDistance(name, f.long); d < bestD {
			best, bestD = f.long, d
		}
	}
	if best == "" {
		return ""
	}
	return " (did you mean --" + best + "?)"
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// usage formats the option table.
func (fs *flagSet) usage() string {
	var b strings.Builder
	rows := make([][2]string, 0, len(fs.flags))
	width := 0
	for _, f := range fs.flags {
		left := "    "
		if f.short != 0 {
			left = "-" + string(f.short) + ", "
		}
		left += "--" + f.long
		if f.arg != "" {
			left += " " + f.arg
		}
		rows = append(rows, [2]string{left, f.help})
		width = max(width, len(left))
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, r[0], r[1])
	}
	return b.String()
}

// longNames lists option names, for shell completion.
func (fs *flagSet) longNames() []string {
	var out []string
	for _, f := range fs.flags {
		out = append(out, "--"+f.long)
	}
	sort.Strings(out)
	return out
}
