// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"math"
	"os"
	"strings"
)

// style holds colour and glyph choices.
type style struct {
	color, unicode bool
}

func newStyle(color, unicode bool) style { return style{color, unicode} }

func (s style) wrap(code, text string) string {
	if !s.color || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s style) good(t string) string { return s.wrap("32", t) }
func (s style) info(t string) string { return s.wrap("36", t) }
func (s style) warn(t string) string { return s.wrap("33", t) }
func (s style) bad(t string) string  { return s.wrap("1;31", t) }
func (s style) dim(t string) string  { return s.wrap("2", t) }
func (s style) bold(t string) string { return s.wrap("1", t) }

var glyphs = map[string][2]string{
	"ok":   {"✓", "+"},
	"upd":  {"↻", "~"},
	"same": {"·", "."},
	"fail": {"✗", "!"},
	"warn": {"⚠", "!"},
	"gone": {"⊘", "-"},
	"skip": {"·", "."},
}

func (s style) sym(name string) string {
	g := glyphs[name]
	if s.unicode {
		return g[0]
	}
	return g[1]
}

func (s style) spinner(frame int) string {
	if s.unicode {
		frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
		return string(frames[frame%len(frames)])
	}
	return string(`|/-\`[frame%4])
}

// bar draws a progress bar of width cells for fraction f in [0,1].
func (s style) bar(f float64, width int) string {
	f = math.Max(0, math.Min(1, f))
	if !s.unicode {
		n := int(f * float64(width))
		return "[" + strings.Repeat("#", n) + strings.Repeat("-", width-n) + "]"
	}
	eighths := int(math.Round(f * float64(width*8)))
	full, part := eighths/8, eighths%8
	var b strings.Builder
	b.WriteString(strings.Repeat("█", full))
	if full < width {
		if part > 0 {
			b.WriteRune([]rune(" ▏▎▍▌▋▊▉")[part])
			full++
		}
	}
	filled := b.String()
	return s.info(filled) + s.dim(strings.Repeat("░", width-full))
}

// DetectUnicode guesses whether the terminal renders UTF-8.
func DetectUnicode() bool {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(k); v != "" {
			v = strings.ToLower(v)
			return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
		}
	}
	return false
}

// ColorEnabled applies the --color setting and the NO_COLOR convention.
func ColorEnabled(setting string, isTTY bool) bool {
	switch setting {
	case "always":
		return true
	case "never":
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	return isTTY && os.Getenv("TERM") != "dumb"
}
