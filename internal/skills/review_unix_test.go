//go:build linux || darwin

package skills

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSiblingSkillSymlinkIsNotAuthorized(t *testing.T) {
	root := t.TempDir()
	raw := skill("demo", "SIBLING_SECRET")
	put(t, root, "demo/SKILL.md", raw)
	c := catalog(t, []Root{{ID: "fixture", Path: root}}, DefaultLimits())
	id := list(t, c).Entries[0].ID
	// Matching metadata/hash must not authorize a sibling within the named root.
	put(t, root, "other/SKILL.md", raw)
	if err := os.Remove(filepath.Join(root, "demo", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../other/SKILL.md", filepath.Join(root, "demo", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	got := list(t, c)
	if len(got.Entries) != 0 || len(got.Problems) != 2 || got.Problems[0].Name != "demo" || !errors.Is(got.Problems[0].Err, ErrInvalid) {
		t.Fatalf("sibling skill loaded as local content: %+v", got)
	}
	if s, err := c.Select(id); s != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("selection followed sibling skill: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "demo", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	put(t, root, "demo/SKILL.md", raw)
	if err := os.Symlink("../other/SKILL.md", filepath.Join(root, "demo", "resource")); err != nil {
		t.Fatal(err)
	}
	if s, err := c.Select(id); s != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("selection followed sibling resource: %v", err)
	}
}

func TestFIFOSymlinkTargetsRejectWithoutWriter(t *testing.T) {
	for _, target := range []string{"candidate directory", "skill file"} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, "demo/SKILL.md", skill("demo", "body"))
			c := catalog(t, []Root{{ID: "fixture", Path: root}}, DefaultLimits())
			id := list(t, c).Entries[0].ID
			link := filepath.Join(root, "demo")
			fifo := filepath.Join(root, "pipe")
			if target == "skill file" {
				link = filepath.Join(root, "demo", "SKILL.md")
				fifo = filepath.Join(root, "demo", "pipe")
			}
			if err := os.RemoveAll(link); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("pipe", link); err != nil {
				t.Fatal(err)
			}
			got := list(t, c)
			if len(got.Entries) != 0 || len(got.Problems) != 1 || !errors.Is(got.Problems[0].Err, ErrInvalid) {
				t.Fatalf("FIFO link accepted: %+v", got)
			}
			if s, err := c.Select(id); s != nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("FIFO link selected: %v", err)
			}
		})
	}
}
