package archive

import (
	"fmt"
	"path"
	"strings"
)

// Patterns select repositories by their archive ID ("host/owner/repo").
// The syntax is a forgiving variant of gitignore:
//
//   - A pattern is a slash-separated list of shell globs (*, ?, [...]),
//     each matched against one path component; "**" matches any number
//     of components.
//   - The pattern may match any contiguous run of components, so
//     "termux-packages", "termux/termux-packages" and
//     "github.com/termux/termux-packages" all select the same repository.
//     A leading "/" anchors the pattern at the host component.
//   - Matching a leading run of components selects everything below it:
//     "github.com/termux" selects every repository of that owner.
//   - In exclusion lists, "!pattern" re-includes; the last matching
//     pattern decides.

// ValidatePattern checks pattern syntax (without a leading '!').
func ValidatePattern(p string) error {
	body := strings.TrimPrefix(p, "/")
	if body == "" {
		return fmt.Errorf("empty pattern")
	}
	for _, r := range p {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("pattern %q contains whitespace or control characters", p)
		}
	}
	for _, c := range strings.Split(body, "/") {
		if c == "" {
			return fmt.Errorf("pattern %q has an empty component", p)
		}
		if _, err := path.Match(c, ""); err != nil {
			return fmt.Errorf("pattern %q: %v", p, err)
		}
	}
	return nil
}

// MatchPattern reports whether pattern selects the repository id.
func MatchPattern(pattern, id string) bool {
	anchored := strings.HasPrefix(pattern, "/")
	pc := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	ic := strings.Split(id, "/")
	for i := range ic {
		if anchored && i > 0 {
			break
		}
		for j := i + 1; j <= len(ic); j++ {
			if globComponents(pc, ic[i:j]) {
				return true
			}
		}
	}
	return false
}

func globComponents(pc, ic []string) bool {
	if len(pc) == 0 {
		return len(ic) == 0
	}
	if pc[0] == "**" {
		for k := 0; k <= len(ic); k++ {
			if globComponents(pc[1:], ic[k:]) {
				return true
			}
		}
		return false
	}
	if len(ic) == 0 {
		return false
	}
	ok, err := path.Match(pc[0], ic[0])
	return err == nil && ok && globComponents(pc[1:], ic[1:])
}

// Excluded evaluates an ordered exclusion list for id. It returns whether
// the repository is excluded and the deciding pattern.
func Excluded(patterns []string, id string) (bool, string) {
	excluded, by := false, ""
	for _, p := range patterns {
		neg := strings.HasPrefix(p, "!")
		if MatchPattern(strings.TrimPrefix(p, "!"), id) {
			excluded, by = !neg, p
		}
	}
	return excluded, by
}

// MatchAny reports whether any of the patterns selects id. An empty
// pattern list selects everything.
func MatchAny(patterns []string, id string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if MatchPattern(p, id) {
			return true
		}
	}
	return false
}
