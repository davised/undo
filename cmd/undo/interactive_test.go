package main

import (
	"strings"
	"testing"
)

func TestParseSelection(t *testing.T) {
	sel, err := parseSelection("1,3-5", 6)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]bool{0: true, 2: true, 3: true, 4: true}
	if len(sel) != len(want) {
		t.Fatalf("got %v, want %v", sel, want)
	}
	for k := range want {
		if !sel[k] {
			t.Errorf("missing index %d in %v", k, sel)
		}
	}
}

func TestArmedEnvDropsAnExistingShim(t *testing.T) {
	// a shell with the hook installed already exports our shim; loading a
	// second copy makes both intercept and journal each other's writes
	base := []string{
		"LD_PRELOAD=/usr/lib/undo/libundo.so:/opt/other.so",
		"UNDO_SESSION=/stale/session",
		"PATH=/bin",
	}
	got := armedEnv(base, "/home/u/.local/lib/undo/libundo.so", "/new/session")

	var preload, sess string
	for _, kv := range got {
		if v, ok := strings.CutPrefix(kv, "LD_PRELOAD="); ok {
			preload = v
		}
		if v, ok := strings.CutPrefix(kv, "UNDO_SESSION="); ok {
			sess = v
		}
	}
	if strings.Count(preload, "libundo.so") != 1 {
		t.Errorf("shim listed %d times in %q, want exactly 1",
			strings.Count(preload, "libundo.so"), preload)
	}
	if !strings.Contains(preload, "/opt/other.so") {
		t.Errorf("unrelated preload was dropped: %q", preload)
	}
	if sess != "/new/session" {
		t.Errorf("session = %q, want the new one", sess)
	}
}

func TestParseSelectionErrors(t *testing.T) {
	for _, input := range []string{"0", "7", "x", "", "2-1x"} {
		if _, err := parseSelection(input, 6); err == nil {
			t.Errorf("parseSelection(%q) should fail", input)
		}
	}
}
