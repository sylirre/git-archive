package units

import (
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	good := map[string]int64{"0": 0, "512": 512, "100k": 100 << 10, "1.5G": 3 << 29, "2GiB": 2 << 30, "10MB": 10 << 20, " 3m ": 3 << 20, "1t": 1 << 40}
	for in, want := range good {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-1", "x", "1x", "1e400", "NaN", "inf"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) accepted", in)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 30: "5.0 GiB"}
	for in, want := range cases {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestDurations(t *testing.T) {
	if d, err := ParseDuration("90"); err != nil || d != 90*time.Second {
		t.Errorf("ParseDuration(90) = %v, %v", d, err)
	}
	if d, err := ParseDuration("1h30m"); err != nil || d != 90*time.Minute {
		t.Errorf("ParseDuration(1h30m) = %v, %v", d, err)
	}
	if _, err := ParseDuration("-5s"); err == nil {
		t.Error("negative duration accepted")
	}
	cases := map[time.Duration]string{850 * time.Millisecond: "850ms", 12300 * time.Millisecond: "12.3s",
		245 * time.Second: "4m05s", 123 * time.Minute: "2h03m"}
	for in, want := range cases {
		if got := FormatDuration(in); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", in, got, want)
		}
	}
}
