//go:build unix

package workspacefs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"
)

func TestStageNameCompatibilityPublicAccess(t *testing.T) {
	for _, alias := range []string{".workspacefs-ﬆaging", ".workspacefs-ﬅaging"} {
		t.Run(alias, func(t *testing.T) {
			r, dir := fixture(t)
			ctx := context.Background()
			must(t, os.Mkdir(filepath.Join(dir, "sub"), 0700))
			_, err := r.Create(ctx, "sub/public", "base")
			must(t, err)
			parent := filepath.Join(dir, "sub")
			info := stageAliasInfo(t, parent, alias)
			canonical := trustedInfo(t, filepath.Join(parent, stagingDirName))
			prefix := path.Join("sub", alias)
			if !os.SameFile(info, canonical) {
				// A distinct directory is public on a filesystem without this name
				// equivalence. Do not turn observed spellings into a blacklist.
				_, err := r.Create(ctx, path.Join(prefix, "payload"), "public")
				must(t, err)
				observed, err := r.Read(ctx, path.Join(prefix, "payload"))
				must(t, err)
				if observed.Text != "public" {
					t.Fatalf("distinct public directory read = %+v", observed)
				}
				_, err = r.Replace(ctx, path.Join(prefix, "payload"), observed.SHA256, "updated")
				must(t, err)
				handle, err := Open(filepath.Join(parent, alias), info)
				must(t, err)
				must(t, handle.Close())
				return
			}
			private := filepath.Join(parent, stagingDirName)
			must(t, os.WriteFile(filepath.Join(private, "payload"), []byte("private"), 0600))
			must(t, os.Mkdir(filepath.Join(private, "child"), 0700))
			if snapshot, err := r.Read(ctx, path.Join(prefix, "payload")); !errors.Is(err, ErrPath) || snapshot != (Snapshot{}) {
				t.Errorf("Read via physical namespace alias = %+v, %v", snapshot, err)
			}
			if receipt, err := r.Create(ctx, path.Join(prefix, "new"), "foreign"); !errors.Is(err, ErrPath) || receipt.Effect != EffectUnchanged {
				t.Errorf("Create via physical namespace alias = %+v, %v", receipt, err)
			}
			if receipt, err := r.Replace(ctx, path.Join(prefix, "payload"), digest("private"), "foreign"); !errors.Is(err, ErrPath) || receipt.Effect != EffectUnchanged {
				t.Errorf("Replace via physical namespace alias = %+v, %v", receipt, err)
			}
			if entries, err := r.List(ctx, prefix); !errors.Is(err, ErrPath) || len(entries) != 0 {
				t.Errorf("List via physical namespace alias = %v, %v", entries, err)
			}
			if _, err := r.Read(ctx, prefix); !errors.Is(err, ErrPath) {
				t.Errorf("Read namespace as final component = %v", err)
			}
			if receipt, err := r.Create(ctx, prefix, "foreign"); !errors.Is(err, ErrPath) || receipt.Effect != EffectUnchanged {
				t.Errorf("Create namespace as final component = %+v, %v", receipt, err)
			}
			if receipt, err := r.Replace(ctx, prefix, digest("private"), "foreign"); !errors.Is(err, ErrPath) || receipt.Effect != EffectUnchanged {
				t.Errorf("Replace namespace as final component = %+v, %v", receipt, err)
			}
			for _, rootPath := range []string{filepath.Join(parent, alias), filepath.Join(parent, alias, "child")} {
				if handle, err := Open(rootPath, trustedInfo(t, rootPath)); !errors.Is(err, ErrPath) {
					if handle != nil {
						must(t, handle.Close())
					}
					t.Errorf("Open with namespace component %q = %v", rootPath, err)
				}
			}
			shortcut := filepath.Join(dir, "shortcut")
			must(t, os.Symlink(filepath.Join(parent, alias), shortcut))
			child := filepath.Join(shortcut, "child")
			if handle, err := Open(child, trustedInfo(t, child)); !errors.Is(err, ErrPath) {
				if handle != nil {
					must(t, handle.Close())
				}
				t.Errorf("Open through trusted ancestor symlink into namespace = %v", err)
			}
			data, err := os.ReadFile(filepath.Join(private, "payload"))
			must(t, err)
			if string(data) != "private" {
				t.Errorf("public API changed private bytes: %q", data)
			}
		})
	}
}

func TestStageNameCompatibilityHiddenAndBounded(t *testing.T) {
	for _, alias := range []string{".workspacefs-ﬆaging", ".workspacefs-ﬅaging"} {
		t.Run(alias, func(t *testing.T) {
			r, dir := fixture(t)
			must(t, os.Mkdir(filepath.Join(dir, alias), 0700))
			_, err := r.Create(context.Background(), "public", "ok")
			must(t, err)
			canonical := trustedInfo(t, filepath.Join(dir, stagingDirName))
			same := os.SameFile(canonical, trustedInfo(t, filepath.Join(dir, alias)))
			t.Logf("preexisting name %q is physical namespace = %v", alias, same)
			want := 2
			if same {
				want = 1
			}
			entries, err := r.List(context.Background(), ".")
			must(t, err)
			if len(entries) != want {
				t.Errorf("preexisting physical namespace visibility: %+v, want %d public entries", entries, want)
			}
			for i := range MaxEntries - want {
				must(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%03d", i)), []byte("x"), 0600))
			}
			entries, err = r.List(context.Background(), ".")
			if err != nil || len(entries) != MaxEntries {
				t.Errorf("200 public entries = %d, %v", len(entries), err)
			}
			for _, entry := range entries {
				if os.SameFile(canonical, trustedInfo(t, filepath.Join(dir, entry.Name))) {
					t.Errorf("physical namespace leaked: %q", entry.Name)
				}
			}
			must(t, os.WriteFile(filepath.Join(dir, "extra"), []byte("x"), 0600))
			if partial, err := r.List(context.Background(), "."); !errors.Is(err, ErrTooManyEntries) || len(partial) != 0 {
				t.Errorf("oversized list returned partial data: %d, %v", len(partial), err)
			}
		})
	}
}

func TestStageNameCompatibilityBlockedAfterFinalCheck(t *testing.T) {
	for _, aliasDir := range []string{".workspacefs-ﬆaging", ".workspacefs-ﬅaging"} {
		t.Run(aliasDir, func(t *testing.T) {
			r, dir := fixture(t)
			other, err := Open(dir, trustedInfo(t, dir))
			must(t, err)
			defer other.Close()
			must(t, os.WriteFile(filepath.Join(dir, "target"), []byte("old"), 0600))
			ctx := context.Background()
			calls := 0
			r.io.publish = func(parent *os.Root, staged, target string, create bool) error {
				calls++
				info := stageAliasInfo(t, dir, aliasDir)
				if !os.SameFile(info, trustedInfo(t, filepath.Join(dir, stagingDirName))) {
					return publish(parent, staged, target, create)
				}
				alias := path.Join(aliasDir, path.Base(staged))
				if entries, err := other.List(ctx, aliasDir); !errors.Is(err, ErrPath) || len(entries) != 0 {
					t.Errorf("second Root List after final check = %v, %v", entries, err)
				}
				if handle, err := Open(filepath.Join(dir, aliasDir), info); !errors.Is(err, ErrPath) {
					if handle != nil {
						must(t, handle.Close())
					}
					t.Errorf("second Root Open after final check = %v", err)
				}
				if snapshot, err := other.Read(ctx, alias); !errors.Is(err, ErrPath) || snapshot != (Snapshot{}) {
					t.Errorf("second Root Read after final check = %+v, %v", snapshot, err)
				}
				second, err := other.Replace(ctx, alias, digest("requested"), "foreign")
				if !errors.Is(err, ErrPath) || second.Effect != EffectUnchanged {
					t.Errorf("second Root Replace after final check = %+v, %v", second, err)
				}
				return publish(parent, staged, target, create)
			}
			receipt, err := r.Replace(ctx, "target", digest("old"), "requested")
			must(t, err)
			data, err := os.ReadFile(filepath.Join(dir, "target"))
			must(t, err)
			if calls != 1 || receipt.Stage != StageDone || receipt.Effect != EffectApplied || string(data) != "requested" || digest(string(data)) != receipt.AfterSHA256 {
				t.Errorf("physical alias final publication: calls=%d receipt=%+v actual=%q", calls, receipt, data)
			}
		})
	}
}

func TestStageNameCompatibilityCreateBeforeNamespace(t *testing.T) {
	r, dir := fixture(t)
	const alias = ".workspacefs-ﬆaging"
	calls := 0
	r.io.publish = func(parent *os.Root, staged, target string, create bool) error {
		calls++
		return publish(parent, staged, target, create)
	}
	receipt, err := r.Create(context.Background(), alias, "public")
	canonical := trustedInfo(t, filepath.Join(dir, stagingDirName))
	created := trustedInfo(t, filepath.Join(dir, alias))
	if os.SameFile(canonical, created) {
		if !errors.Is(err, ErrPath) || receipt.Effect != EffectUnchanged || calls != 0 {
			t.Errorf("newly materialized namespace alias = %+v, %v, calls=%d", receipt, err, calls)
		}
		assertNoStage(t, dir)
	} else {
		must(t, err)
		if receipt.Effect != EffectApplied || calls != 1 {
			t.Fatalf("distinct public filename was rejected: %+v, calls=%d", receipt, calls)
		}
		assertDisk(t, filepath.Join(dir, alias), "public", 0600)
	}
}

func TestRootKeepsTrustedPublicAncestorSymlink(t *testing.T) {
	parent := t.TempDir()
	public := filepath.Join(parent, "public")
	must(t, os.MkdirAll(filepath.Join(public, "child"), 0700))
	must(t, os.WriteFile(filepath.Join(public, "child", "text"), []byte("public"), 0600))
	alias := filepath.Join(parent, "shortcut")
	must(t, os.Symlink(public, alias))
	rootPath := filepath.Join(alias, "child")
	r, err := Open(rootPath, trustedInfo(t, rootPath))
	must(t, err)
	defer r.Close()
	observed, err := r.Read(context.Background(), "text")
	must(t, err)
	if observed.Text != "public" {
		t.Fatalf("trusted ancestor compatibility read = %+v", observed)
	}
}
