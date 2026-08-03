//go:build !linux

package main

import "os"

// tryClone has no answer off Linux -- the twin mirrors the linux signature so the
// package builds and vets everywhere. It is never a broken probe: off Linux undo
// does not run, and the linux twin alone decides what a real filesystem supports.
func tryClone(dst, src *os.File) (bool, error) { return false, nil }

// probeReflink has no answer off Linux. undo does not run there, but gofmt and
// go vet do, on the macOS workstation these agents work from, and a build tag
// on the real probe alone leaves the call site undefined.
func probeReflink(dir string) (bool, error) {
	return false, nil
}
