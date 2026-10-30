//go:build linux || darwin

package skills

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSelectionRejectsEscapingResourcesAndPathSwaps(t *testing.T) {
	outside := t.TempDir()
	put(t, outside, "SKILL.md", skill("demo", "OUTSIDE_SECRET"))
	for _, target := range []string{filepath.Join(outside, "SKILL.md"), "../outside.txt", "resource", "."} {
		root := t.TempDir()
		put(t, root, "demo/SKILL.md", skill("demo", "inside"))
		put(t, root, "outside.txt", "OUTSIDE_SECRET")
		if err := os.Symlink(target, filepath.Join(root, "demo", "resource")); err != nil {
			t.Fatal(err)
		}
		c := catalog(t, []Root{{ID: "test", Path: root}}, DefaultLimits())
		if s, err := c.Select(list(t, c).Entries[0].ID); s != nil || !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe supporting resource accepted: %q, %v", target, err)
		}
	}
	root := t.TempDir()
	put(t, root, "demo/SKILL.md", skill("demo", "inside"))
	c := catalog(t, []Root{{ID: "test", Path: root}}, DefaultLimits())
	id := list(t, c).Entries[0].ID
	if err := os.Rename(filepath.Join(root, "demo"), filepath.Join(root, "saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "demo")); err != nil {
		t.Fatal(err)
	}
	if s, err := c.Select(id); s != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("selection followed swapped skill directory: %v", err)
	}
	pipeRoot := t.TempDir()
	put(t, pipeRoot, "demo/SKILL.md", skill("demo", "inside"))
	if err := syscall.Mkfifo(filepath.Join(pipeRoot, "demo", "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	pc := catalog(t, []Root{{ID: "pipe", Path: pipeRoot}}, DefaultLimits())
	if s, err := pc.Select(list(t, pc).Entries[0].ID); s != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("special supporting file accepted: %v", err)
	}
}

func TestContainedResourceLinkIsCopied(t *testing.T) {
	root := t.TempDir()
	put(t, root, "demo/SKILL.md", skill("demo", "inside"))
	put(t, root, "demo/real.txt", "stable")
	if err := os.Symlink("real.txt", filepath.Join(root, "demo", "alias.txt")); err != nil {
		t.Fatal(err)
	}
	c := catalog(t, []Root{{ID: "test", Path: root}}, DefaultLimits())
	s := selectSkill(t, c, list(t, c).Entries[0].ID)
	if err := os.Remove(filepath.Join(root, "demo", "real.txt")); err != nil {
		t.Fatal(err)
	}
	if data, err := s.ReadResource("alias.txt"); err != nil || string(data) != "stable" {
		t.Fatalf("link read from live source: %q, %v", data, err)
	}
}

func TestConcurrentResourceSwapCannotEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	put(t, outside, "secret", "OUTSIDE_SECRET")
	put(t, root, "demo/SKILL.md", skill("demo", "inside"))
	put(t, root, "demo/reference.txt", "INSIDE")
	c := catalog(t, []Root{{ID: "test", Path: root}}, DefaultLimits())
	id := list(t, c).Entries[0].ID
	if data, err := selectSkill(t, c, id).ReadResource("reference.txt"); err != nil || string(data) != "INSIDE" {
		t.Fatalf("initial resource missing: %q, %v", data, err)
	}
	stop, done := make(chan struct{}), make(chan error, 1)
	go func() {
		tmp, target := filepath.Join(root, "demo", ".swap"), filepath.Join(root, "demo", "reference.txt")
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if err := os.Symlink(filepath.Join(outside, "secret"), tmp); err != nil {
				done <- err
				return
			}
			if err := os.Rename(tmp, target); err != nil {
				done <- err
				return
			}
			if err := os.WriteFile(tmp, []byte("INSIDE"), 0600); err != nil {
				done <- err
				return
			}
			if err := os.Rename(tmp, target); err != nil {
				done <- err
				return
			}
		}
	}()
	defer func() {
		close(stop)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for n := 0; n < 64; n++ {
		s, err := c.Select(id)
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("unexpected swap error: %v", err)
			}
			continue
		}
		for _, file := range s.Files() {
			data, err := s.ReadResource(file.Path)
			if err != nil || strings.Contains(string(data), "OUTSIDE_SECRET") {
				t.Fatalf("path swap escaped root: %q, %v", data, err)
			}
		}
	}
}
