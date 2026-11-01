//go:build unix

package artifactfs

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func fixture(t testing.TB, limit int64) (*Store, *os.Root, string) {
	t.Helper()
	source, err := os.OpenRoot(t.TempDir())
	must(t, err)
	t.Cleanup(func() { must(t, source.Close()) })
	private, err := os.OpenRoot(t.TempDir())
	must(t, err)
	dir := filepath.Join(private.Name(), "blobs")
	s, err := New(private, limit)
	must(t, err)
	must(t, private.Close()) // New must own an independent handle.
	t.Cleanup(func() { must(t, s.Close()) })
	return s, source, dir
}

func digest(data string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(data))) }

func assertFiles(t testing.TB, dir string, want int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, err)
	if len(entries) != want {
		t.Fatalf("store entries = %v, want %d", entries, want)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("staging file leaked: %s", entry.Name())
		}
	}
}

func assertRead(t testing.TB, s *Store, hash, want string) {
	t.Helper()
	r, err := s.Open(hash)
	must(t, err)
	data, err := io.ReadAll(r)
	must(t, err)
	must(t, r.Close())
	if string(data) != want {
		t.Fatalf("read = %q, want %q", data, want)
	}
}

func TestSizeBoundariesAndPrivateModes(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		limit      int64
		wantErr    error
	}{{"empty", "", 0, nil}, {"exact", "data", 4, nil}, {"over", "data!", 4, ErrTooLarge}} {
		t.Run(tc.name, func(t *testing.T) {
			s, src, dir := fixture(t, tc.limit)
			must(t, src.WriteFile("input", []byte(tc.data), 0600))
			blob, err := s.Publish(src, "input")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Publish = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if blob != (Blob{}) {
					t.Fatalf("failed publish returned %+v", blob)
				}
				assertFiles(t, dir, 0)
				return
			}
			if blob != (Blob{digest(tc.data), int64(len(tc.data))}) {
				t.Fatalf("unexpected blob: %+v", blob)
			}
			assertRead(t, s, blob.Hash, tc.data)
			assertFiles(t, dir, 1)
			for path, mode := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, blob.Hash): 0400} {
				info, err := os.Stat(path)
				must(t, err)
				if info.Mode().Perm() != mode {
					t.Errorf("%s mode = %o, want %o", path, info.Mode().Perm(), mode)
				}
			}
		})
	}
}

func TestRejectUnauthorizedPaths(t *testing.T) {
	s, src, dir := fixture(t, 32)
	must(t, src.WriteFile("ok", []byte("good"), 0600))
	for _, name := range []string{"", "../ok", "a/../ok", "/ok", filepath.Join(src.Name(), "ok")} {
		if _, err := s.Publish(src, name); !errors.Is(err, ErrPath) {
			t.Errorf("Publish(%q) = %v, want ErrPath", name, err)
		}
	}
	if _, err := s.Publish(nil, "ok"); !errors.Is(err, ErrPath) {
		t.Errorf("nil source = %v, want ErrPath", err)
	}
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600))
	rel, err := filepath.Rel(src.Name(), outside)
	must(t, err)
	must(t, src.Symlink(rel, "escape"))
	must(t, src.Symlink(filepath.Join(src.Name(), "ok"), "absolute"))
	for _, name := range []string{"escape/secret", "absolute"} {
		if _, err := s.Publish(src, name); err == nil {
			t.Errorf("symlink escape accepted: %s", name)
		}
	}
	assertFiles(t, dir, 0)
}

func TestContainedRelativeSymlink(t *testing.T) {
	s, src, _ := fixture(t, 32)
	must(t, src.Mkdir("sub", 0700))
	must(t, src.WriteFile("ok", []byte("good"), 0600))
	must(t, src.Symlink("../ok", "sub/alias"))
	blob, err := s.Publish(src, "sub/alias")
	must(t, err)
	assertRead(t, s, blob.Hash, "good")
}

func TestRejectNonRegularInputs(t *testing.T) {
	s, src, dir := fixture(t, 32)
	must(t, src.Mkdir("directory", 0700))
	must(t, syscall.Mkfifo(filepath.Join(src.Name(), "pipe"), 0600))
	for _, name := range []string{"directory", "pipe"} {
		if _, err := s.Publish(src, name); !errors.Is(err, ErrNotRegular) {
			t.Errorf("Publish(%q) = %v, want ErrNotRegular", name, err)
		}
	}
	assertFiles(t, dir, 0)
}

func TestImmutableCopyAcrossSourceMutation(t *testing.T) {
	s, src, dir := fixture(t, 32)
	must(t, src.WriteFile("input", []byte("first"), 0600))
	blob, err := s.Publish(src, "input")
	must(t, err)
	sourceInfo, err := src.Stat("input")
	must(t, err)
	blobInfo, err := os.Stat(filepath.Join(dir, blob.Hash))
	must(t, err)
	if os.SameFile(sourceInfo, blobInfo) {
		t.Fatal("source was hardlinked instead of copied")
	}
	must(t, src.WriteFile("input", []byte("edited"), 0600))
	assertRead(t, s, blob.Hash, "first")
	must(t, src.Remove("input"))
	assertRead(t, s, blob.Hash, "first")
	must(t, src.WriteFile("input", []byte("replacement"), 0600))
	assertRead(t, s, blob.Hash, "first")
	next, err := s.Publish(src, "input")
	must(t, err)
	if next.Hash == blob.Hash {
		t.Fatal("replacement reused old digest")
	}
	assertRead(t, s, next.Hash, "replacement")
}

func TestConcurrentDuplicatePublication(t *testing.T) {
	s, src, dir := fixture(t, 32)
	must(t, src.WriteFile("input", []byte("same"), 0600))
	root, err := os.OpenRoot(filepath.Dir(dir))
	must(t, err)
	other, err := New(root, 32)
	must(t, err)
	must(t, root.Close())
	defer other.Close()
	start, results := make(chan struct{}), make(chan error, 16)
	for i := 0; i < cap(results); i++ {
		go func(i int) {
			<-start
			store := []*Store{s, other}[i%2]
			blob, err := store.Publish(src, "input")
			if err == nil && blob != (Blob{digest("same"), 4}) {
				err = fmt.Errorf("unexpected blob: %+v", blob)
			}
			results <- err
		}(i)
	}
	close(start)
	for i := 0; i < cap(results); i++ {
		must(t, <-results)
	}
	assertFiles(t, dir, 1)
	before, err := os.Stat(filepath.Join(dir, digest("same")))
	must(t, err)
	_, err = s.Publish(src, "input")
	must(t, err)
	after, err := os.Stat(filepath.Join(dir, digest("same")))
	must(t, err)
	if !os.SameFile(before, after) {
		t.Fatal("duplicate publication overwrote the existing inode")
	}
	assertRead(t, other, digest("same"), "same")
}

func TestCorruptExistingDigestIsNeverAcceptedOrOverwritten(t *testing.T) {
	for _, kind := range []string{"content", "oversized", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			s, src, dir := fixture(t, 16)
			must(t, src.WriteFile("input", []byte("good"), 0600))
			blob, err := s.Publish(src, "input")
			must(t, err)
			path := filepath.Join(dir, blob.Hash)
			must(t, os.Remove(path))
			switch kind {
			case "content":
				must(t, os.WriteFile(path, []byte("evil"), 0600))
			case "oversized":
				must(t, os.WriteFile(path, []byte(strings.Repeat("x", 17)), 0600))
			case "directory":
				must(t, os.Mkdir(path, 0700))
			case "symlink":
				must(t, os.Symlink(filepath.Join(src.Name(), "input"), path))
			}
			before, err := os.Lstat(path)
			must(t, err)
			reader, err := s.Open(blob.Hash)
			if reader != nil {
				reader.Close()
				t.Fatal("corrupt bytes exposed to reader")
			}
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Open = %v, want ErrCorrupt", err)
			}
			if _, err := s.Publish(src, "input"); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Publish = %v, want ErrCorrupt", err)
			}
			after, err := os.Lstat(path)
			must(t, err)
			if !os.SameFile(before, after) {
				t.Fatal("corrupt digest overwritten")
			}
			assertFiles(t, dir, 1)
		})
	}
}

func TestHashValidationAndCloseLifecycle(t *testing.T) {
	s, src, _ := fixture(t, 32)
	for _, hash := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64), strings.Repeat("A", 64), "../" + digest("good")} {
		if _, err := s.Open(hash); !errors.Is(err, ErrHash) {
			t.Errorf("Open(%q) = %v, want ErrHash", hash, err)
		}
	}
	if _, err := s.Open(digest("missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing digest = %v, want os.ErrNotExist", err)
	}
	must(t, src.WriteFile("input", []byte("good"), 0600))
	blob, err := s.Publish(src, "input")
	must(t, err)
	r, err := s.Open(blob.Hash)
	must(t, err)
	must(t, s.Close())
	must(t, s.Close())
	if _, err := s.Open(blob.Hash); !errors.Is(err, ErrClosed) {
		t.Errorf("Open after Close = %v", err)
	}
	if _, err := s.Publish(src, "input"); !errors.Is(err, ErrClosed) {
		t.Errorf("Publish after Close = %v", err)
	}
	data, err := io.ReadAll(r)
	must(t, err)
	must(t, r.Close())
	if string(data) != "good" {
		t.Fatalf("reader did not survive Close: %q", data)
	}
	_, err = src.Stat("input")
	must(t, err) // Source handles remain caller-owned.
}

func TestInvalidConfiguration(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	must(t, err)
	defer root.Close()
	for _, limit := range []int64{-1, 1<<63 - 1} {
		if s, err := New(root, limit); err == nil {
			s.Close()
			t.Errorf("invalid limit accepted: %d", limit)
		}
	}
	if s, err := New(nil, 1); err == nil {
		s.Close()
		t.Error("nil private root accepted")
	}
}

func TestFailedWriteCleanup(t *testing.T) {
	if os.Getenv("ARTIFACTFS_WRITE_FAILURE_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestFailedWriteCleanup$", "-test.v", "-test.timeout=20s")
		cmd.Env = append(os.Environ(), "ARTIFACTFS_WRITE_FAILURE_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("write failure subprocess: %v\n%s", err, out)
		}
		t.Logf("%s", out)
		return
	}
	s, src, dir := fixture(t, 4096)
	must(t, src.WriteFile("input", []byte(strings.Repeat("x", 4096)), 0600))
	var limit syscall.Rlimit
	must(t, syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit))
	limit.Cur = 512
	signal.Ignore(syscall.SIGXFSZ)
	must(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit))
	blob, err := s.Publish(src, "input")
	if !errors.Is(err, syscall.EFBIG) || blob != (Blob{}) {
		t.Fatalf("partial write = %+v, %v; want EFBIG and no blob", blob, err)
	}
	assertFiles(t, dir, 0)
}
