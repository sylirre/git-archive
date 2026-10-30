// Package units parses and formats byte sizes and durations for humans.
package units

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseSize parses sizes such as "512", "100k", "1.5G" or "2GiB" into
// bytes. Suffixes are binary (k = 1024), matching git's conventions.
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	t = strings.TrimSuffix(strings.TrimSuffix(t, "b"), "i")
	mult := 1.0
	if n := len(t); n > 0 {
		switch t[n-1] {
		case 'k':
			mult = 1 << 10
		case 'm':
			mult = 1 << 20
		case 'g':
			mult = 1 << 30
		case 't':
			mult = 1 << 40
		}
		if mult != 1 {
			t = t[:n-1]
		}
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) || v*mult > math.MaxInt64/2 {
		return 0, fmt.Errorf("invalid size %q (examples: 500M, 2G)", s)
	}
	return int64(v * mult), nil
}

// FormatBytes renders n with a binary unit, e.g. "1.5 GiB".
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// ParseDuration accepts Go durations ("90s", "10m", "1h30m") and plain
// integers, which are seconds.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q (examples: 90s, 10m, 1h)", s)
	}
	return d, nil
}

// FormatDuration renders d compactly: "850ms", "12.3s", "4m05s", "2h03m".
func FormatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
