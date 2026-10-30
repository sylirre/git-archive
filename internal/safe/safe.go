// Package safe holds the small set of primitives that stand between data
// received from the network and the local host: validation of names that
// become filesystem paths, containment checks for joined paths, and
// neutralisation of text before it reaches the user's terminal.
//
// Everything that originates from a forge API, a git server or a
// repository must pass through one of these functions before it is used
// as a path component or printed.
package safe

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxNameLen bounds a single path component. Forges allow at most ~100
// characters; 255 is the common filesystem limit.
const MaxNameLen = 255

// Name validates a single path component received from a remote party
// (an owner, group or repository name). Accepted names consist of ASCII
// letters, digits, '.', '_' and '-', which is the intersection of what
// GitHub, GitLab and Gitea permit. The following are rejected even though
// they are made of permitted characters:
//
//   - "." and ".." (path traversal),
//   - ".git" in any letter case (would be mistaken for a git directory),
//   - a leading '-' (could be parsed as a command-line option),
//   - names longer than MaxNameLen.
func Name(s string) error {
	if s == "" {
		return fmt.Errorf("empty name")
	}
	if len(s) > MaxNameLen {
		return fmt.Errorf("name too long (%d bytes)", len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-'
		if !ok {
			return fmt.Errorf("name %q contains a disallowed character", Term(s))
		}
	}
	switch {
	case s == "." || s == "..":
		return fmt.Errorf("name %q is a relative path element", s)
	case strings.EqualFold(s, ".git"):
		return fmt.Errorf("name %q is reserved", s)
	case s[0] == '-':
		return fmt.Errorf("name %q starts with '-'", s)
	}
	return nil
}

// RelPath validates a slash-separated relative path made of components that
// each satisfy Name, such as "owner/repo" or "group/subgroup/project".
func RelPath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	for _, part := range strings.Split(p, "/") {
		if err := Name(part); err != nil {
			return err
		}
	}
	return nil
}

// Host validates a network host name with an optional ":port" suffix.
// Only lowercase DNS characters are accepted; callers lowercase first.
func Host(h string) error {
	if h == "" {
		return fmt.Errorf("empty host")
	}
	name, port, hasPort := strings.Cut(h, ":")
	if hasPort {
		if port == "" || len(port) > 5 {
			return fmt.Errorf("invalid port in host %q", Term(h))
		}
		for i := 0; i < len(port); i++ {
			if port[i] < '0' || port[i] > '9' {
				return fmt.Errorf("invalid port in host %q", Term(h))
			}
		}
	}
	if len(name) > 253 {
		return fmt.Errorf("host name too long")
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("invalid host %q", Term(h))
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("invalid host %q", Term(h))
			}
		}
	}
	return nil
}

// HostDir turns a validated host into a directory name. A port separator
// becomes '_' so that the result is a portable file name.
func HostDir(h string) string {
	return strings.ReplaceAll(h, ":", "_")
}

// Within reports whether path p, after cleaning, lies inside root (or is
// root itself). Both paths must be absolute.
func Within(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// Join joins validated relative path components below root and verifies
// that the result did not escape root. It is the only way the program
// derives a filesystem location from remote data.
func Join(root string, rel string) (string, error) {
	if err := RelPath(rel); err != nil {
		return "", err
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	if !Within(root, p) || filepath.Clean(p) == filepath.Clean(root) {
		return "", fmt.Errorf("path %q escapes the archive", Term(rel))
	}
	return p, nil
}

// Term makes untrusted text safe to write to a terminal. It removes the
// means by which a hostile server or repository could take control of the
// display: C0 and C1 control characters (which start escape sequences
// such as cursor movement, OSC 52 clipboard writes or title changes), DEL,
// Unicode bidirectional overrides (which can visually reorder text), and
// invalid UTF-8. Tabs become spaces; every other removed character is
// shown as '?', so that tampering stays visible.
func Term(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f || c >= 0x80 {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == utf8.RuneError && size <= 1:
			b.WriteByte('?')
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			b.WriteByte('?')
		case isBidiControl(r) || r == ' ' || r == ' ':
			b.WriteByte('?')
		case !unicode.IsPrint(r) && !unicode.IsSpace(r):
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TermLine is Term for text that must stay on a single line: line breaks
// are folded into " | " so that multi-line error messages remain readable.
func TermLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	s = strings.ReplaceAll(s, "\r", "\n")
	parts := strings.Split(s, "\n")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, Term(p))
		}
	}
	return strings.Join(out, " | ")
}

func isBidiControl(r rune) bool {
	return r == '؜' || r == '‎' || r == '‏' ||
		(r >= '‪' && r <= '‮') || (r >= '⁦' && r <= '⁩')
}
