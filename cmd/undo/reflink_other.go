//go:build !linux

package main

// probeReflink has no answer off Linux. undo does not run there, but gofmt and
// go vet do, on the macOS workstation these agents work from, and a build tag
// on the real probe alone leaves the call site undefined.
func probeReflink(dir string) (bool, error) {
	return false, nil
}
