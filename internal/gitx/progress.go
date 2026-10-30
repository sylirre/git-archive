// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"bufio"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Progress is one progress report emitted by git (or git-lfs) on stderr.
type Progress struct {
	Phase   string // e.g. "Receiving objects", "Resolving deltas", "Updating files"
	Percent int    // 0..100, or -1 when git reports a plain counter
	Current int64
	Total   int64  // 0 when unknown
	Detail  string // throughput part, e.g. "12.00 MiB | 3.10 MiB/s"
	Remote  bool   // the server reported it ("remote: ...")
	Done    bool
}

// progressRE matches the two shapes of git progress lines:
//
//	Receiving objects:  45% (450/1000), 1.20 MiB | 2.00 MiB/s
//	remote: Enumerating objects: 1234, done.
var progressRE = regexp.MustCompile(
	`^(remote: )?([A-Z][A-Za-z ]{2,40}): +(?:(\d{1,3})% \((\d+)/(\d+)\)|(\d+))(?:, (.+))?$`)

// ParseProgress parses a single stderr line. ok is false for lines that
// are not progress reports.
func ParseProgress(line string) (p Progress, ok bool) {
	line = strings.TrimRight(line, " \t")
	for _, suffix := range []string{", done.", ", done"} {
		if rest, ok := strings.CutSuffix(line, suffix); ok {
			line, p.Done = rest, true
			break
		}
	}
	m := progressRE.FindStringSubmatch(line)
	if m == nil {
		return Progress{}, false
	}
	p.Remote = m[1] != ""
	p.Phase = strings.TrimSpace(m[2])
	if m[3] != "" {
		p.Percent, _ = strconv.Atoi(m[3])
		p.Current, _ = strconv.ParseInt(m[4], 10, 64)
		p.Total, _ = strconv.ParseInt(m[5], 10, 64)
		if p.Percent > 100 {
			p.Percent = 100
		}
	} else {
		p.Percent = -1
		p.Current, _ = strconv.ParseInt(m[6], 10, 64)
	}
	p.Detail = strings.TrimSpace(m[7])
	return p, true
}

// maxLine bounds the length of a single stderr line kept in memory.
// A server that sends an endless line cannot exhaust memory: the excess
// is discarded.
const maxLine = 16 << 10

// scanLines splits git's stderr stream on both '\r' (progress updates)
// and '\n', calling fn for each non-empty line.
func scanLines(r io.Reader, fn func(string)) {
	br := bufio.NewReaderSize(r, 32<<10)
	var buf []byte
	flush := func() {
		if len(buf) > 0 {
			fn(string(buf))
			buf = buf[:0]
		}
	}
	for {
		c, err := br.ReadByte()
		if err != nil {
			flush()
			return
		}
		switch c {
		case '\r', '\n':
			flush()
		default:
			if len(buf) < maxLine {
				buf = append(buf, c)
			}
		}
	}
}
