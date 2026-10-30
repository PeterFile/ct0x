//go:build linux || darwin

package skills

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSpecialFilesAndDirectoryEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	put(t, outside, "SKILL.md", skill("escape", "OUTSIDE_SECRET"))
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "pipe"), 0700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "pipe", "SKILL.md")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	got := list(t, catalog(t, []Root{{ID: "test", Path: root}}, DefaultLimits()))
	if len(got.Entries) != 0 || len(got.Problems) != 2 {
		t.Fatalf("escape or special file accepted: %+v", got)
	}
	if c, err := Open([]Root{{ID: "fifo", Path: fifo}}, DefaultLimits()); err == nil {
		_ = c.Close()
		t.Fatal("FIFO accepted as root")
	}
	for _, problem := range got.Problems {
		if !errors.Is(problem.Err, ErrInvalid) {
			t.Fatalf("unclear filesystem problem: %v", problem.Err)
		}
	}
}
