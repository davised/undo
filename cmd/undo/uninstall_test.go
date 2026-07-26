package main

import "testing"

func TestStripHookLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		n    int
	}{
		{
			name: "installer block with a PATH line",
			in: `export EDITOR=vim

# undo: revert what the last command did (undo.edaywalid.com)
export PATH="/home/u/.local/bin:$PATH"
source /home/u/.local/share/undo/undo.zsh
alias gs='git status'`,
			want: `export EDITOR=vim
alias gs='git status'`,
			n: 3,
		},
		{
			name: "block without a PATH line",
			in: `# undo: revert what the last command did (undo.edaywalid.com)
source /usr/share/undo/undo.bash
export FOO=1`,
			want: `export FOO=1`,
			n:    2,
		},
		{
			name: "hand-added source line, no marker",
			in: `alias ll='ls -la'
source /home/u/.local/share/undo/undo.fish`,
			want: `alias ll='ls -la'`,
			n:    1,
		},
		{
			name: "nothing of ours: an unrelated PATH must survive",
			in: `export PATH="/opt/tools/bin:$PATH"
alias ll='ls -la'`,
			want: `export PATH="/opt/tools/bin:$PATH"
alias ll='ls -la'`,
			n: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, n := stripHookLines(c.in)
			if got != c.want {
				t.Errorf("content:\n--- got ---\n%s\n--- want ---\n%s", got, c.want)
			}
			if n != c.n {
				t.Errorf("removed = %d, want %d", n, c.n)
			}
		})
	}
}
