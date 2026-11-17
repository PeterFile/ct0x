//go:build unix

package workspacefs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func stageAliasInfo(t testing.TB, dir, alias string) os.FileInfo {
	t.Helper()
	aliasPath := filepath.Join(dir, alias)
	if _, err := os.Lstat(aliasPath); errors.Is(err, os.ErrNotExist) {
		// On case-sensitive volumes the same conservative policy applies to a
		// distinct directory. This lane's volume resolves these as one inode.
		must(t, os.Mkdir(aliasPath, 0700))
	}
	info := trustedInfo(t, aliasPath)
	t.Logf("alias %q same physical staging directory = %v", alias, os.SameFile(trustedInfo(t, filepath.Join(dir, stagingDirName)), info))
	return info
}

func TestStageCaseAliasRejectedSingleRoot(t *testing.T) {
	r, dir := fixture(t)
	ctx := context.Background()
	_, err := r.Create(ctx, "public", "base")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, stagingDirName, "payload"), []byte("private"), 0600))
	for _, alias := range []string{".WORKSPACEFS-STAGING", ".worKspacefs-staging", ".workſpacefs-staging"} {
		info := stageAliasInfo(t, dir, alias)
		name := path.Join(alias, "payload")
		if receipt, err := r.Create(ctx, path.Join(alias, "new"), "foreign"); !errors.Is(err, ErrPath) || receipt.Effect != EffectUnchanged {
			t.Errorf("Create via %q = %+v, %v, want unchanged/ErrPath", alias, receipt, err)
		}
		if snapshot, err := r.Read(ctx, name); !errors.Is(err, ErrPath) || snapshot != (Snapshot{}) {
			t.Errorf("Read via %q = %+v, %v, want ErrPath", alias, snapshot, err)
		}
		if receipt, err := r.Replace(ctx, name, digest("private"), "foreign"); !errors.Is(err, ErrPath) || receipt.Effect != EffectUnchanged {
			t.Errorf("Replace via %q = %+v, %v, want unchanged/ErrPath", alias, receipt, err)
		}
		if entries, err := r.List(ctx, alias); !errors.Is(err, ErrPath) || len(entries) != 0 {
			t.Errorf("List via %q = %v, %v, want ErrPath", alias, entries, err)
		}
		if handle, err := Open(filepath.Join(dir, alias), info); !errors.Is(err, ErrPath) {
			if handle != nil {
				must(t, handle.Close())
			}
			t.Errorf("Open via %q = %v, want ErrPath", alias, err)
		}
		if _, err := r.Create(ctx, path.Join("missing", alias, "new"), "x"); !errors.Is(err, ErrPath) {
			t.Errorf("nested reserved alias %q = %v, want ErrPath", alias, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, stagingDirName, "payload"))
	must(t, err)
	if string(data) != "private" {
		t.Errorf("public API changed private bytes to %q", data)
	}
}

func TestStageExistingCaseAliasHiddenAndBounded(t *testing.T) {
	r, dir := fixture(t)
	alias := strings.ToUpper(stagingDirName)
	must(t, os.Mkdir(filepath.Join(dir, alias), 0700))
	_, err := r.Create(context.Background(), "public", "ok")
	must(t, err)
	stageAliasInfo(t, dir, alias)
	entries, err := r.List(context.Background(), ".")
	must(t, err)
	if len(entries) != 1 || entries[0].Name != "public" {
		t.Errorf("preexisting case alias is visible: %+v", entries)
	}
	for i := range MaxEntries - 1 {
		must(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%03d", i)), []byte("x"), 0600))
	}
	entries, err = r.List(context.Background(), ".")
	if err != nil || len(entries) != MaxEntries {
		t.Errorf("200 public entries plus hidden aliases = %d entries, %v", len(entries), err)
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name, stagingDirName) {
			t.Errorf("case alias leaked in bounded list: %q", entry.Name)
		}
	}
	for _, name := range []string{"extra", "beyond-scan"} {
		must(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600))
		if partial, err := r.List(context.Background(), "."); !errors.Is(err, ErrTooManyEntries) || len(partial) != 0 {
			t.Errorf("oversized listing after %q returned a partial success: %d entries, %v", name, len(partial), err)
		}
	}
}

func TestStageCaseAliasBlockedAfterFinalCheck(t *testing.T) {
	r, dir := fixture(t)
	other, err := Open(dir, trustedInfo(t, dir))
	must(t, err)
	defer other.Close()
	must(t, os.WriteFile(filepath.Join(dir, "target"), []byte("old"), 0600))
	ctx := context.Background()
	calls := 0
	r.io.publish = func(parent *os.Root, staged, target string, create bool) error {
		calls++
		if create {
			t.Fatal("unexpected create")
		}
		aliasDir := strings.ToUpper(stagingDirName)
		alias := path.Join(aliasDir, path.Base(staged))
		info := stageAliasInfo(t, dir, aliasDir)
		if entries, err := other.List(ctx, aliasDir); !errors.Is(err, ErrPath) || len(entries) != 0 {
			t.Errorf("second Root List after final check = %v, %v, want ErrPath", entries, err)
		}
		if handle, err := Open(filepath.Join(dir, aliasDir), info); !errors.Is(err, ErrPath) {
			if handle != nil {
				must(t, handle.Close())
			}
			t.Errorf("second Root Open after final check = %v, want ErrPath", err)
		}
		if snapshot, err := other.Read(ctx, alias); !errors.Is(err, ErrPath) || snapshot != (Snapshot{}) {
			t.Errorf("second Root Read after final check = %+v, %v, want ErrPath", snapshot, err)
		}
		second, err := other.Replace(ctx, alias, digest("requested"), "foreign")
		if !errors.Is(err, ErrPath) || second.Effect != EffectUnchanged {
			t.Errorf("second Root Replace after final check = %+v, %v, want unchanged/ErrPath", second, err)
		}
		// The first call still performs the real publication, not a mock result.
		return publish(parent, staged, target, create)
	}
	receipt, err := r.Replace(ctx, "target", digest("old"), "requested")
	must(t, err)
	data, err := os.ReadFile(filepath.Join(dir, "target"))
	must(t, err)
	if calls != 1 || receipt.Stage != StageDone || receipt.Effect != EffectApplied || string(data) != "requested" || digest(string(data)) != receipt.AfterSHA256 {
		t.Errorf("final-check/publication sequence: calls=%d receipt=%+v actual=%q", calls, receipt, data)
	}
}
