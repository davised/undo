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
	if runtime.GOOS != "linux" {
		t.Skip("the !linux twin reports no error for any directory")
	}
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

// A reported success must have actually moved the data. This holds on any
// filesystem: where cloning is unsupported the probe reports false and the
// assertion does not apply, and where it is supported the destination must
// contain what the source did. It is what separates the real ioctl from a
// function that returns true without calling it.
func TestTryCloneThatSucceedsMovedTheData(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the clone only happens on linux")
	}
	dir := t.TempDir()
	const body = "undo doctor clone payload\n"

	src, err := os.CreateTemp(dir, "src-")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.WriteString(body); err != nil {
		t.Fatal(err)
	}

	dst, err := os.CreateTemp(dir, "dst-")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	ok, err := tryClone(dst, src)
	if err != nil {
		t.Fatalf("tryClone: %v", err)
	}
	if !ok {
		t.Skip("this filesystem cannot clone; nothing to verify")
	}
	got, err := os.ReadFile(dst.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("clone reported success but dst holds %q, want %q", got, body)
	}
}
