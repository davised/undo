package main

import (
	"os"
	"runtime"
	"testing"
)

// The probe must answer for a real directory without reporting an error.
// Whether the answer is true or false depends on the filesystem under the
// test's temp dir, so the assertion is on the error, not on the verdict.
func TestProbeReflinkAnswersWithoutError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the probe only reports a real answer on linux")
	}
	if _, err := probeReflink(t.TempDir()); err != nil {
		t.Fatalf("probeReflink returned an error on a writable dir: %v", err)
	}
}

// A directory that does not exist is a probe failure, not "unsupported":
// answering false there would report a filesystem property we never measured.
func TestProbeReflinkErrorsOnMissingDir(t *testing.T) {
	if _, err := probeReflink("/nonexistent-undo-doctor-dir"); err == nil {
		t.Fatal("probeReflink reported no error for a directory that does not exist")
	}
}

// The probe leaves nothing behind: doctor runs it in the user's own directory.
func TestProbeReflinkCleansUp(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the probe only creates files on linux")
	}
	dir := t.TempDir()
	if _, err := probeReflink(dir); err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("probe left %d file(s) behind", len(ents))
	}
}
