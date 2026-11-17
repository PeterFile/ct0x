//go:build unix

package workspacefs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func digest(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }

func assertDisk(t testing.TB, path, text string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	if string(data) != text || digest(string(data)) != digest(text) {
		t.Fatalf("disk bytes = %q, want %q", data, text)
	}
	if got := trustedInfo(t, path).Mode().Perm(); got != mode {
		t.Fatalf("disk mode = %o, want %o", got, mode)
	}
}

func TestReadObservation(t *testing.T) {
	r, path := fixture(t)
	must(t, os.Mkdir(filepath.Join(path, "sub"), 0700))
	file := filepath.Join(path, "sub", "文档.txt")
	must(t, os.WriteFile(file, []byte("hello 世界\n"), 0640))
	must(t, os.Chmod(file, 0640))
	got, err := r.Read(context.Background(), "sub/文档.txt")
	must(t, err)
	want := Snapshot{Text: "hello 世界\n", SHA256: digest("hello 世界\n"), Mode: 0640}
	if got != want {
		t.Fatalf("Read = %+v, want %+v", got, want)
	}
	assertDisk(t, file, want.Text, want.Mode)
}

func TestReadTextBounds(t *testing.T) {
	r, path := fixture(t)
	for _, tc := range []struct {
		name, text string
		want       error
	}{
		{"empty", "", nil},
		{"exact", strings.Repeat("界", MaxTextBytes/3) + "a", nil},
		{"over", strings.Repeat("x", MaxTextBytes+1), ErrTooLarge},
		{"invalid_utf8", "\xff", ErrUTF8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must(t, os.WriteFile(filepath.Join(path, tc.name), []byte(tc.text), 0600))
			got, err := r.Read(context.Background(), tc.name)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Read = %v, want %v", err, tc.want)
			}
			if tc.want == nil && (got.Text != tc.text || got.SHA256 != digest(tc.text)) {
				t.Fatalf("incorrect observation: %+v", got)
			}
			if tc.want != nil && got != (Snapshot{}) {
				t.Fatal("failed read exposed a partial observation")
			}
		})
	}
}

func TestReadRejectsPathsAndNonRegular(t *testing.T) {
	r, path := fixture(t)
	must(t, os.WriteFile(filepath.Join(path, "ok"), []byte("ok"), 0600))
	for _, name := range []string{"", ".", "../ok", "a/../ok", "/ok", "./ok", "a//ok", "ok/", "a\\ok", "a\x00b", "\xff"} {
		if _, err := r.Read(context.Background(), name); !errors.Is(err, ErrPath) {
			t.Errorf("Read(%q) = %v, want ErrPath", name, err)
		}
	}
	must(t, os.Mkdir(filepath.Join(path, "dir"), 0700))
	must(t, syscall.Mkfifo(filepath.Join(path, "pipe"), 0600))
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600))
	must(t, os.Symlink(outside, filepath.Join(path, "escape")))
	must(t, os.Symlink("ok", filepath.Join(path, "alias")))
	for _, name := range []string{"dir", "pipe", "alias", "escape/secret"} {
		if got, err := r.Read(context.Background(), name); err == nil || got != (Snapshot{}) {
			t.Errorf("unsafe Read(%q) = %+v, %v", name, got, err)
		}
	}
	if _, err := r.Read(context.Background(), "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing read = %v", err)
	}
}

func TestListBoundedDirectory(t *testing.T) {
	r, path := fixture(t)
	entries, err := r.List(context.Background(), ".")
	must(t, err)
	if len(entries) != 0 {
		t.Fatalf("empty directory = %v", entries)
	}
	for i := MaxEntries - 1; i >= 0; i-- {
		must(t, os.WriteFile(filepath.Join(path, fmt.Sprintf("%03d", i)), []byte("x"), 0600))
	}
	entries, err = r.List(context.Background(), ".")
	must(t, err)
	if len(entries) != MaxEntries {
		t.Fatalf("list size = %d", len(entries))
	}
	for i, entry := range entries {
		if entry.Name != fmt.Sprintf("%03d", i) || !entry.Mode.IsRegular() || entry.Size != 1 {
			t.Fatalf("entry %d = %+v", i, entry)
		}
	}
	must(t, os.Mkdir(filepath.Join(path, "extra"), 0700))
	if got, err := r.List(context.Background(), "."); !errors.Is(err, ErrTooManyEntries) || len(got) != 0 {
		t.Fatalf("oversized list = %v, %v", got, err)
	}
	must(t, os.Symlink(t.TempDir(), filepath.Join(path, "extra", "link")))
	entries, err = r.List(context.Background(), "extra")
	must(t, err)
	if len(entries) != 1 || entries[0].Mode&os.ModeSymlink == 0 {
		t.Fatalf("list followed symlink: %+v", entries)
	}
	for _, name := range []string{"", "../", "extra/link"} {
		if _, err := r.List(context.Background(), name); err == nil {
			t.Fatalf("unsafe List(%q) accepted", name)
		}
	}
}

func TestReadRootIdentityAndCancellation(t *testing.T) {
	for _, kind := range []string{"replaced", "handle_closed", "closed", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "root")
			must(t, os.Mkdir(path, 0700))
			must(t, os.WriteFile(filepath.Join(path, "file"), []byte("old"), 0600))
			r, err := Open(path, trustedInfo(t, path))
			must(t, err)
			defer r.Close()
			ctx := context.Background()
			want := ErrRootChanged
			switch kind {
			case "replaced":
				must(t, os.Rename(path, path+"-old"))
				must(t, os.Mkdir(path, 0700))
				must(t, os.WriteFile(filepath.Join(path, "file"), []byte("new"), 0600))
			case "handle_closed":
				must(t, r.root.Close())
			case "closed":
				must(t, r.Close())
				want = ErrClosed
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			if got, err := r.Read(ctx, "file"); !errors.Is(err, want) || got != (Snapshot{}) {
				t.Errorf("Read = %+v, %v, want %v", got, err, want)
			}
			if got, err := r.List(ctx, "."); !errors.Is(err, want) || len(got) != 0 {
				t.Errorf("List = %v, %v, want %v", got, err, want)
			}
		})
	}
}
