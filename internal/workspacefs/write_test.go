//go:build unix

package workspacefs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func assertNoStage(t testing.TB, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	must(t, err)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".workspacefs-") {
			t.Fatalf("private staging file leaked: %s", entry.Name())
		}
	}
}

func TestWriteReplaceActualBytesAndMode(t *testing.T) {
	r, path := fixture(t)
	must(t, os.Mkdir(filepath.Join(path, "sub"), 0700))
	file := filepath.Join(path, "sub", "file")
	must(t, os.WriteFile(file, []byte("old\n"), 0750))
	must(t, os.Chmod(file, 0750))
	before := trustedInfo(t, file)
	old, err := os.Open(file)
	must(t, err)
	defer old.Close()
	receipt, err := r.Replace(context.Background(), "sub/file", digest("old\n"), "新内容\n")
	must(t, err)
	want := WriteReceipt{Stage: StageDone, Effect: EffectApplied, BeforeSHA256: digest("old\n"), AfterSHA256: digest("新内容\n")}
	if receipt != want {
		t.Fatalf("receipt = %+v, want %+v", receipt, want)
	}
	assertDisk(t, file, "新内容\n", 0750)
	if os.SameFile(before, trustedInfo(t, file)) {
		t.Fatal("replacement changed the original inode in place")
	}
	preimage, err := io.ReadAll(old)
	must(t, err)
	if string(preimage) != "old\n" {
		t.Fatalf("old descriptor lost preimage: %q", preimage)
	}
	got, err := r.Read(context.Background(), "sub/file")
	must(t, err)
	if got.SHA256 != receipt.AfterSHA256 || got.Text != "新内容\n" || got.Mode != 0750 {
		t.Fatalf("read-after-write = %+v", got)
	}
	assertNoStage(t, filepath.Dir(file))
}

func TestWriteCreateNoOverwrite(t *testing.T) {
	r, path := fixture(t)
	file := filepath.Join(path, "new")
	receipt, err := r.Create(context.Background(), "new", "created")
	must(t, err)
	if receipt.Stage != StageDone || receipt.Effect != EffectApplied || receipt.BeforeSHA256 != "" || receipt.AfterSHA256 != digest("created") {
		t.Fatalf("create receipt = %+v", receipt)
	}
	assertDisk(t, file, "created", 0600)
	before := trustedInfo(t, file)
	receipt, err = r.Create(context.Background(), "new", "overwrite")
	if !errors.Is(err, os.ErrExist) || receipt.Effect != EffectUnchanged {
		t.Fatalf("duplicate create = %+v, %v", receipt, err)
	}
	assertDisk(t, file, "created", 0600)
	if !os.SameFile(before, trustedInfo(t, file)) {
		t.Fatal("duplicate create replaced the inode")
	}
	if receipt, err := r.Create(context.Background(), "missing/new", "x"); !errors.Is(err, os.ErrNotExist) || receipt.Effect != EffectUnchanged {
		t.Fatalf("implicit mkdir = %+v, %v", receipt, err)
	}
	assertNoStage(t, path)
}

func TestWriteExpectedHashConflict(t *testing.T) {
	r, path := fixture(t)
	file := filepath.Join(path, "file")
	must(t, os.WriteFile(file, []byte("external change"), 0640))
	before := trustedInfo(t, file)
	for _, tc := range []struct {
		hash string
		want error
	}{
		{digest("old"), ErrConflict},
		{"", ErrHash},
		{strings.Repeat("A", 64), ErrHash},
		{strings.Repeat("g", 64), ErrHash},
		{strings.Repeat("a", 63), ErrHash},
	} {
		receipt, err := r.Replace(context.Background(), "file", tc.hash, "new")
		if !errors.Is(err, tc.want) || receipt.Effect != EffectUnchanged {
			t.Fatalf("Replace(%q) = %+v, %v", tc.hash, receipt, err)
		}
		assertDisk(t, file, "external change", before.Mode().Perm())
		if !os.SameFile(before, trustedInfo(t, file)) {
			t.Fatal("conflict changed the inode")
		}
	}
	assertNoStage(t, path)
}

func TestWriteBoundsAndUnsafeTargets(t *testing.T) {
	r, path := fixture(t)
	must(t, os.WriteFile(filepath.Join(path, "file"), []byte("old"), 0600))
	for _, tc := range []struct {
		name, text string
		want       error
	}{
		{"", "x", ErrPath},
		{".", "x", ErrPath},
		{"../file", "x", ErrPath},
		{"a/../file", "x", ErrPath},
		{"/file", "x", ErrPath},
		{"file", "\xff", ErrUTF8},
		{"file", strings.Repeat("x", MaxTextBytes+1), ErrTooLarge},
	} {
		for _, create := range []bool{false, true} {
			var receipt WriteReceipt
			var err error
			if create {
				receipt, err = r.Create(context.Background(), tc.name, tc.text)
			} else {
				receipt, err = r.Replace(context.Background(), tc.name, digest("old"), tc.text)
			}
			if !errors.Is(err, tc.want) || receipt.Effect != EffectUnchanged {
				t.Fatalf("write %q create=%v = %+v, %v", tc.name, create, receipt, err)
			}
		}
	}
	must(t, os.Mkdir(filepath.Join(path, "dir"), 0700))
	must(t, syscall.Mkfifo(filepath.Join(path, "pipe"), 0600))
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "file"), []byte("outside"), 0600))
	must(t, os.Symlink(outside, filepath.Join(path, "escape")))
	must(t, os.Symlink(filepath.Join(outside, "file"), filepath.Join(path, "link")))
	must(t, os.Symlink("file", filepath.Join(path, "inside-link")))
	for _, name := range []string{"dir", "pipe", "link", "inside-link", "escape/file"} {
		before := trustedInfo(t, filepath.Join(path, name))
		receipt, err := r.Replace(context.Background(), name, digest("old"), "new")
		if err == nil || receipt.Effect != EffectUnchanged {
			t.Fatalf("unsafe replace %s = %+v, %v", name, receipt, err)
		}
		if !os.SameFile(before, trustedInfo(t, filepath.Join(path, name))) {
			t.Fatalf("unsafe target changed: %s", name)
		}
		if receipt, err := r.Create(context.Background(), name, "new"); err == nil || receipt.Effect != EffectUnchanged {
			t.Fatalf("unsafe create %s = %+v, %v", name, receipt, err)
		}
	}
	assertDisk(t, filepath.Join(path, "file"), "old", 0600)
	assertDisk(t, filepath.Join(outside, "file"), "outside", 0600)
	if _, err := r.Create(context.Background(), "escape/new", "x"); err == nil {
		t.Fatal("created through external symlink")
	}
	if _, err := os.Lstat(filepath.Join(outside, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside target = %v", err)
	}
	for _, text := range []string{strings.Repeat("x", MaxTextBytes), ""} {
		receipt, err := r.Replace(context.Background(), "file", digest("old"), text)
		must(t, err)
		assertDisk(t, filepath.Join(path, "file"), text, 0600)
		_, err = r.Replace(context.Background(), "file", receipt.AfterSHA256, "old")
		must(t, err)
	}
	assertNoStage(t, path)
}

func TestWriteRechecksPreimage(t *testing.T) {
	for _, change := range []string{"content", "same_bytes_new_inode"} {
		t.Run(change, func(t *testing.T) {
			r, path := fixture(t)
			file := filepath.Join(path, "file")
			must(t, os.WriteFile(file, []byte("old"), 0600))
			want := "old"
			r.io.sync = func(staged *os.File) error {
				must(t, staged.Sync())
				if change == "content" {
					want = "external"
					return os.WriteFile(file, []byte(want), 0600)
				}
				replacement := filepath.Join(path, "external")
				must(t, os.WriteFile(replacement, []byte("old"), 0600))
				return os.Rename(replacement, file)
			}
			receipt, err := r.Replace(context.Background(), "file", digest("old"), "new")
			if !errors.Is(err, ErrConflict) || receipt.Effect != EffectUnchanged {
				t.Fatalf("raced replace = %+v, %v", receipt, err)
			}
			assertDisk(t, file, want, 0600)
			assertNoStage(t, path)
		})
	}
}

func TestWriteCreateRaceNoOverwrite(t *testing.T) {
	r, path := fixture(t)
	r.io.publish = func(parent *os.Root, staged, name string, create bool) error {
		must(t, os.WriteFile(filepath.Join(path, "new"), []byte("competitor"), 0600))
		return publish(parent, staged, name, create)
	}
	receipt, err := r.Create(context.Background(), "new", "ours")
	if !errors.Is(err, os.ErrExist) || receipt.Effect != EffectUnchanged || receipt.Stage != StagePublish {
		t.Fatalf("create race = %+v, %v", receipt, err)
	}
	assertDisk(t, filepath.Join(path, "new"), "competitor", 0600)
	assertNoStage(t, path)
}

func TestWriteIdentityChanges(t *testing.T) {
	for _, kind := range []string{"root_before", "root_during", "parent_during", "handle_closed"} {
		t.Run(kind, func(t *testing.T) {
			r, path := fixture(t)
			must(t, os.Mkdir(filepath.Join(path, "sub"), 0700))
			file := filepath.Join(path, "sub", "file")
			must(t, os.WriteFile(file, []byte("old"), 0600))
			change := func() {
				if kind == "parent_during" {
					must(t, os.Rename(filepath.Join(path, "sub"), filepath.Join(path, "old-sub")))
					must(t, os.Mkdir(filepath.Join(path, "sub"), 0700))
					return
				}
				must(t, os.Rename(path, path+"-old"))
				must(t, os.Mkdir(path, 0700))
			}
			want := ErrRootChanged
			if kind == "parent_during" {
				want = ErrConflict
			}
			if kind == "root_before" {
				change()
			} else if kind == "handle_closed" {
				must(t, r.root.Close())
			} else {
				r.io.sync = func(staged *os.File) error {
					must(t, staged.Sync())
					change()
					return nil
				}
			}
			receipt, err := r.Replace(context.Background(), "sub/file", digest("old"), "new")
			if !errors.Is(err, want) || receipt.Effect != EffectUnchanged {
				t.Fatalf("identity change = %+v, %v, want %v", receipt, err, want)
			}
			if kind == "root_before" || kind == "root_during" {
				assertDisk(t, filepath.Join(path+"-old", "sub", "file"), "old", 0600)
				assertNoStage(t, filepath.Join(path+"-old", "sub"))
			} else if kind == "parent_during" {
				assertDisk(t, filepath.Join(path, "old-sub", "file"), "old", 0600)
				assertNoStage(t, filepath.Join(path, "old-sub"))
			} else {
				assertDisk(t, file, "old", 0600)
			}
		})
	}
}

func TestWriteCancellationBoundary(t *testing.T) {
	for _, when := range []string{"before", "staged", "published"} {
		t.Run(when, func(t *testing.T) {
			r, path := fixture(t)
			file := filepath.Join(path, "file")
			must(t, os.WriteFile(file, []byte("old"), 0600))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch when {
			case "before":
				cancel()
			case "staged":
				r.io.sync = func(staged *os.File) error {
					err := staged.Sync()
					cancel()
					return err
				}
			case "published":
				r.io.publish = func(parent *os.Root, staged, name string, create bool) error {
					err := publish(parent, staged, name, create)
					cancel()
					return err
				}
			}
			receipt, err := r.Replace(ctx, "file", digest("old"), "new")
			if when == "published" {
				must(t, err)
				if receipt.Effect != EffectApplied || receipt.Stage != StageDone {
					t.Fatalf("published cancellation lost success: %+v", receipt)
				}
				assertDisk(t, file, "new", 0600)
			} else {
				if !errors.Is(err, context.Canceled) || receipt.Effect != EffectUnchanged {
					t.Fatalf("prepublish cancellation = %+v, %v", receipt, err)
				}
				assertDisk(t, file, "old", 0600)
			}
			assertNoStage(t, path)
		})
	}
}

func TestWriteErrorEffects(t *testing.T) {
	for _, when := range []string{"staging_sync", "publish_unknown", "directory_sync"} {
		t.Run(when, func(t *testing.T) {
			r, path := fixture(t)
			file := filepath.Join(path, "file")
			must(t, os.WriteFile(file, []byte("old"), 0600))
			wantEffect, wantStage := EffectUnchanged, StagePrepare
			calls := 0
			switch when {
			case "staging_sync":
				r.io.sync = func(staged *os.File) error {
					must(t, staged.Sync())
					return syscall.EIO
				}
			case "publish_unknown":
				wantEffect, wantStage = EffectUnknown, StagePublish
				r.io.publish = func(parent *os.Root, staged, name string, create bool) error {
					calls++
					must(t, publish(parent, staged, name, create))
					return syscall.EIO
				}
			case "directory_sync":
				wantEffect, wantStage = EffectApplied, StageSync
				r.io.sync = func(file *os.File) error {
					info, err := file.Stat()
					must(t, err)
					must(t, file.Sync())
					if info.IsDir() {
						calls++
						return syscall.EIO
					}
					return nil
				}
			}
			receipt, err := r.Replace(context.Background(), "file", digest("old"), "new")
			if !errors.Is(err, syscall.EIO) || receipt.Effect != wantEffect || receipt.Stage != wantStage {
				t.Fatalf("write error = %+v, %v, want %s/%s", receipt, err, wantStage, wantEffect)
			}
			want := "new"
			if when == "staging_sync" {
				want = "old"
			} else if calls != 1 {
				t.Fatalf("boundary retried %d times", calls)
			}
			assertDisk(t, file, want, 0600)
			assertNoStage(t, path)
		})
	}
}

func TestWriteSerializesOneRoot(t *testing.T) {
	r, path := fixture(t)
	must(t, os.WriteFile(filepath.Join(path, "file"), []byte("old"), 0600))
	start, results := make(chan struct{}), make(chan error, 2)
	for _, text := range []string{"first", "second"} {
		go func() {
			<-start
			_, err := r.Replace(context.Background(), "file", digest("old"), text)
			results <- err
		}()
	}
	close(start)
	succeeded, conflicted := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrConflict) {
			conflicted++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("success=%d conflict=%d", succeeded, conflicted)
	}
	got, err := r.Read(context.Background(), "file")
	must(t, err)
	if got.Text != "first" && got.Text != "second" {
		t.Fatalf("unexpected final bytes: %q", got.Text)
	}
	assertNoStage(t, path)
}
