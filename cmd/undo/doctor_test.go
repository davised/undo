package main

import (
	"math"
	"strings"
	"testing"
)

// effectiveMaxBytes must mirror max_bytes() in the shim: parse_ulong over
// UNDO_MAX_BYTES, and a zero result becomes the 256 MiB default. The point of
// these cases is that "256MiB" really means 256 BYTES, so doctor must not echo
// a unit word back as if it were honoured.
func TestEffectiveMaxBytesMirrorsTheShim(t *testing.T) {
	const def = uint64(268435456)
	cases := []struct {
		raw  string
		want uint64
	}{
		{"", def},
		{"0", def},
		{"4096", 4096},
		{"256MiB", 256},
		{"  512", 512},
		{"abc", def},
		{"999999999999999999999999999999", math.MaxUint64}, // saturates, no wrap
	}
	for _, c := range cases {
		if got := effectiveMaxBytes(c.raw); got != c.want {
			t.Errorf("effectiveMaxBytes(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// effectiveMaxStore must use envInt's rule: a base-10 integer > 0, else the
// 1 GiB default. A unit suffix or a negative number is silently discarded.
func TestEffectiveMaxStoreMirrorsEnvInt(t *testing.T) {
	const def = int64(1 << 30)
	cases := []struct {
		raw  string
		want int64
	}{
		{"", def},
		{"10GiB", def},
		{"-5", def},
		{"2048", 2048},
	}
	for _, c := range cases {
		if got := effectiveMaxStore(c.raw); got != c.want {
			t.Errorf("effectiveMaxStore(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// The warn decision for UNDO_MAX_BYTES: a plain positive number is taken at
// face value, anything the shim would read differently is a warn.
func TestMaxBytesMisreadDecision(t *testing.T) {
	cases := []struct {
		raw  string
		warn bool
	}{
		{"123", false},
		{"  123", false}, // leading space/tab is skipped by parse_ulong
		{"999999999999999999999999999999", false},
		{"256MiB", true},
		{"0", true},
		{"000", true},
		{"abc", true},
		{"  ", true},
		{"12x", true},
	}
	for _, c := range cases {
		if got := maxBytesMisread(c.raw); got != c.warn {
			t.Errorf("maxBytesMisread(%q) = %v, want %v", c.raw, got, c.warn)
		}
	}
}

// The warn decision for UNDO_MAX_STORE: envInt rejects a non-integer or a
// non-positive value, so doctor warns on exactly those.
func TestMaxStoreMisreadDecision(t *testing.T) {
	cases := []struct {
		raw  string
		warn bool
	}{
		{"2048", false},
		{"10GiB", true},
		{"-5", true},
		{"0", true},
	}
	for _, c := range cases {
		if got := maxStoreMisread(c.raw); got != c.warn {
			t.Errorf("maxStoreMisread(%q) = %v, want %v", c.raw, got, c.warn)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    uint64
		want string
	}{
		{256, "256 B"},
		{268435456, "256 MiB"},
		{1 << 30, "1 GiB"},
		{25 << 30, "25 GiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// captureReport returns a report func and a pointer to the lines it receives.
func captureReport() (func(checkState, string, string), *[]string) {
	var lines []string
	return func(state checkState, name, detail string) {
		lines = append(lines, name+": "+detail)
	}, &lines
}

// A fallback onto a same-filesystem store can still be a hardlink. Doctor must
// report the method the journal recorded, not assume a capped copy, and must
// not repeat the old "size-capped" claim.
func TestReportVolumeFallbackNamesTheRecordedMethod(t *testing.T) {
	t.Setenv("UNDO_MAX_BYTES", "")
	t.Setenv("UNDO_MAX_STORE", "")

	cases := []struct {
		method  string
		want    string
		notWant string
	}{
		{"link", "hardlinked", "size-capped"},
		{"copy", "copied", "size-capped"},
	}
	for _, c := range cases {
		report, lines := captureReport()
		reportVolume(report, "/tmp", volumeVerdict{Fallback: true, Method: c.method})
		joined := strings.Join(*lines, "\n")
		if !strings.Contains(joined, c.want) {
			t.Errorf("method %q: report %q does not mention %q", c.method, joined, c.want)
		}
		if strings.Contains(joined, c.notWant) {
			t.Errorf("method %q: report %q still says %q", c.method, joined, c.notWant)
		}
		if !strings.Contains(joined, "session store") {
			t.Errorf("method %q: report %q does not name the session store", c.method, joined)
		}
		if !strings.Contains(joined, c.method) {
			t.Errorf("method %q: report %q does not print the token", c.method, joined)
		}
	}
}
