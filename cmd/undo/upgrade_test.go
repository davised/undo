package main

import "testing"

func TestDetectInstall(t *testing.T) {
	cases := []struct {
		exe  string
		want installKind
	}{
		{"/home/u/.local/bin/undo", installSelf},
		{"/usr/bin/undo", installSystem},
		{"/usr/local/bin/undo", installSystem},
		{"/home/linuxbrew/.linuxbrew/bin/undo", installBrew},
		{"/opt/homebrew/bin/undo", installBrew},
	}
	for _, c := range cases {
		if got, _ := detectInstall(c.exe); got != c.want {
			t.Errorf("detectInstall(%q) = %v, want %v", c.exe, got, c.want)
		}
	}
}
