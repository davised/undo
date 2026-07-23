package main

import "testing"

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

func TestParseSelectionErrors(t *testing.T) {
	for _, input := range []string{"0", "7", "x", "", "2-1x"} {
		if _, err := parseSelection(input, 6); err == nil {
			t.Errorf("parseSelection(%q) should fail", input)
		}
	}
}
