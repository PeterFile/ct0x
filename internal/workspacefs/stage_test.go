//go:build unix

package workspacefs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestStageCrossRootCannotAccessActivePayload(t *testing.T) {
	for _, failSync := range []bool{false, true} {
		t.Run(map[bool]string{false: "publish", true: "cleanup"}[failSync], func(t *testing.T) {
			r, dir := fixture(t)
			other, err := Open(dir, trustedInfo(t, dir))
			must(t, err)
			defer other.Close()
			must(t, os.WriteFile(filepath.Join(dir, "target"), []byte("old"), 0600))
			r.io.sync = func(file *os.File) error {
				must(t, file.Sync())
				info, err := file.Stat()
				must(t, err)
				if info.IsDir() {
					return nil
				}
				rel, err := filepath.Rel(dir, file.Name())
				must(t, err)
				rel = filepath.ToSlash(rel)
				top := strings.Split(rel, "/")[0]
				entries, err := other.List(context.Background(), ".")
				must(t, err)
				for _, entry := range entries {
					if entry.Name == top {
						t.Errorf("second Root listed active staging: %s", entry.Name)
					}
				}
				if _, err := other.List(context.Background(), top); !errors.Is(err, ErrPath) {
					t.Errorf("second Root List(stage) = %v, want ErrPath", err)
				}
				if _, err := other.Read(context.Background(), rel); !errors.Is(err, ErrPath) {
					t.Errorf("second Root Read(stage) = %v, want ErrPath", err)
				}
				receipt, err := other.Replace(context.Background(), rel, digest("requested"), "foreign")
				if !errors.Is(err, ErrPath) || receipt.Effect != EffectUnchanged {
					t.Errorf("second Root Replace(stage) = %+v, %v, want unchanged/ErrPath", receipt, err)
				}
				if _, err := other.Create(context.Background(), rel, "foreign"); !errors.Is(err, ErrPath) {
					t.Errorf("second Root Create(stage) = %v, want ErrPath", err)
				}
				if failSync {
					return syscall.EIO
				}
				return nil
			}
			receipt, err := r.Replace(context.Background(), "target", digest("old"), "requested")
			if failSync {
				if !errors.Is(err, syscall.EIO) || receipt.Effect != EffectUnchanged {
					t.Fatalf("staging failure = %+v, %v", receipt, err)
				}
				assertDisk(t, filepath.Join(dir, "target"), "old", 0600)
			} else {
				must(t, err)
				data, err := os.ReadFile(filepath.Join(dir, "target"))
				must(t, err)
				if string(data) != "requested" || digest(string(data)) != receipt.AfterSHA256 {
					t.Errorf("published %q with receipt %+v, want requested bytes", data, receipt)
				}
			}
			assertNoStage(t, dir)
		})
	}
}

func TestStagePayloadSwapIsNotPublishedOrDeleted(t *testing.T) {
	r, dir := fixture(t)
	must(t, os.WriteFile(filepath.Join(dir, "target"), []byte("old"), 0600))
	var substituted string
	r.io.sync = func(file *os.File) error {
		must(t, file.Sync())
		info, err := file.Stat()
		must(t, err)
		if info.IsDir() {
			return nil
		}
		substituted = file.Name()
		must(t, os.Rename(substituted, substituted+"-detached"))
		return os.WriteFile(substituted, []byte("foreign"), 0600)
	}
	receipt, err := r.Replace(context.Background(), "target", digest("old"), "requested")
	if err == nil || receipt.Effect != EffectUnchanged {
		t.Errorf("observed staging substitution = %+v, %v, want unchanged error", receipt, err)
	}
	data, readErr := os.ReadFile(filepath.Join(dir, "target"))
	must(t, readErr)
	if string(data) != "old" {
		t.Errorf("published substituted bytes: %q", data)
	}
	data, readErr = os.ReadFile(substituted)
	if readErr != nil || string(data) != "foreign" {
		t.Errorf("cleanup removed or changed substituted file: %q, %v", data, readErr)
	}
}

func TestStageCleanupKeepsPublicReplacement(t *testing.T) {
	r, dir := fixture(t)
	must(t, os.WriteFile(filepath.Join(dir, "target"), []byte("old"), 0600))
	var publicName string
	var foreign os.FileInfo
	r.io.sync = func(file *os.File) error {
		must(t, file.Sync())
		rel, err := filepath.Rel(dir, file.Name())
		must(t, err)
		publicName = filepath.Join(dir, strings.Split(filepath.ToSlash(rel), "/")[0])
		must(t, os.Rename(publicName, filepath.Join(dir, "detached-stage")))
		must(t, os.WriteFile(publicName, []byte("successful foreign file"), 0600))
		foreign = trustedInfo(t, publicName)
		return syscall.EIO
	}
	receipt, err := r.Replace(context.Background(), "target", digest("old"), "requested")
	if !errors.Is(err, syscall.EIO) || receipt.Effect != EffectUnchanged {
		t.Fatalf("staging failure = %+v, %v", receipt, err)
	}
	data, err := os.ReadFile(publicName)
	if err != nil || string(data) != "successful foreign file" {
		t.Fatalf("cleanup deleted public replacement: %q, %v", data, err)
	}
	if !os.SameFile(foreign, trustedInfo(t, publicName)) {
		t.Fatal("cleanup replaced the foreign inode")
	}
	assertDisk(t, filepath.Join(dir, "target"), "old", 0600)
}

func TestStageNamespaceIsReservedAndBounded(t *testing.T) {
	r, dir := fixture(t)
	private := filepath.Join(dir, ".workspacefs-staging")
	must(t, os.Mkdir(private, 0700))
	if handle, err := Open(private, trustedInfo(t, private)); !errors.Is(err, ErrPath) {
		if handle != nil {
			handle.Close()
		}
		t.Errorf("Open(private namespace) = %v, want ErrPath", err)
	}
	for _, name := range []string{".workspacefs-staging", ".workspacefs-staging/payload", "sub/.workspacefs-staging/payload"} {
		if _, err := r.Create(context.Background(), name, "x"); !errors.Is(err, ErrPath) {
			t.Errorf("Create(%q) = %v, want ErrPath", name, err)
		}
	}
	for i := range MaxEntries {
		must(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%03d", i)), []byte("x"), 0600))
	}
	entries, err := r.List(context.Background(), ".")
	must(t, err)
	if len(entries) != MaxEntries {
		t.Fatalf("public list = %d entries, want %d", len(entries), MaxEntries)
	}
	if got := trustedInfo(t, private).Mode().Perm(); got != 0700 {
		t.Fatalf("private namespace mode = %o", got)
	}
}

func TestStageRejectsNonPrivateNamespace(t *testing.T) {
	r, dir := fixture(t)
	private := filepath.Join(dir, ".workspacefs-staging")
	must(t, os.Mkdir(private, 0755))
	must(t, os.Chmod(private, 0755))
	receipt, err := r.Create(context.Background(), "target", "requested")
	if err == nil || receipt.Effect != EffectUnchanged {
		t.Errorf("non-private namespace accepted: %+v, %v", receipt, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "target")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("target changed using unsafe namespace: %v", err)
	}
	if got := trustedInfo(t, private).Mode().Perm(); got != 0755 {
		t.Fatalf("foreign namespace permissions changed: %o", got)
	}
}

func TestCreateEEXISTAfterActualPublishIsUnknown(t *testing.T) {
	r, dir := fixture(t)
	calls := 0
	r.io.publish = func(parent *os.Root, staged, name string, create bool) error {
		calls++
		must(t, publish(parent, staged, name, create))
		return os.ErrExist
	}
	receipt, err := r.Create(context.Background(), "target", "requested")
	if !errors.Is(err, os.ErrExist) || receipt.Stage != StagePublish || receipt.Effect != EffectUnknown {
		t.Errorf("actual Link then injected EEXIST = %+v, %v, want publish/unknown", receipt, err)
	}
	if calls != 1 {
		t.Fatalf("publication retried %d times", calls)
	}
	assertDisk(t, filepath.Join(dir, "target"), "requested", 0600)
	assertNoStage(t, dir)
}
