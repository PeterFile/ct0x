//go:build unix

package workspacefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func trustedInfo(t testing.TB, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	must(t, err)
	return info
}

func fixture(t testing.TB) (*Root, string) {
	t.Helper()
	path := t.TempDir()
	r, err := Open(path, trustedInfo(t, path))
	must(t, err)
	t.Cleanup(func() { must(t, r.Close()) })
	return r, path
}

func TestRootBinding(t *testing.T) {
	path := t.TempDir()
	identity := trustedInfo(t, path)
	r, err := Open(path, identity)
	must(t, err)
	must(t, r.Close())
	must(t, r.Close())

	other := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	must(t, os.Symlink(path, alias))
	for _, tc := range []struct {
		name, path string
		identity   os.FileInfo
	}{
		{"missing_identity", path, nil},
		{"different_identity", path, trustedInfo(t, other)},
		{"relative_root", ".", identity},
		{"symlink_root", alias, identity},
		{"symlink_root_trailing_slash", alias + "/", identity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Open(tc.path, tc.identity)
			if got != nil {
				got.Close()
				t.Fatal("invalid root returned a handle")
			}
			if !errors.Is(err, ErrRootChanged) {
				t.Fatalf("Open = %v, want ErrRootChanged", err)
			}
		})
	}
}
