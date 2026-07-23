package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDecodesEscapes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal")
	content := "unlink\t/tmp/a%09b%0Ac%25d\t/bak/1\ncreate\t/tmp/x\nbogus\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (bogus line skipped)", len(entries))
	}
	if entries[0].Op != OpUnlink {
		t.Errorf("op = %q", entries[0].Op)
	}
	want := "/tmp/a\tb\nc%d"
	if entries[0].Fields[0] != want {
		t.Errorf("decoded path = %q, want %q", entries[0].Fields[0], want)
	}
	if entries[1].Op != OpCreate || entries[1].Fields[0] != "/tmp/x" {
		t.Errorf("second entry = %+v", entries[1])
	}
}

func TestReadMissingFile(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "nope"))
	if !os.IsNotExist(err) {
		t.Errorf("want not-exist error, got %v", err)
	}
}

func TestDescribe(t *testing.T) {
	cases := []struct {
		e    Entry
		want string
	}{
		{Entry{Op: OpUnlink, Fields: []string{"/a", "/b"}}, "deleted   /a"},
		{Entry{Op: OpRename, Fields: []string{"/a", "/b", "-"}}, "moved     /a -> /b"},
		{Entry{Op: OpChmod, Fields: []string{"/a", "644", "600"}}, "mode      /a (644 -> 600)"},
	}
	for _, c := range cases {
		if got := c.e.Describe(); got != c.want {
			t.Errorf("Describe(%v) = %q, want %q", c.e, got, c.want)
		}
	}
}
