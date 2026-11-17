//go:build darwin

package workspacefs

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestWriteReplaceReadableACLZeroMode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires a non-root owner for meaningful permission checks")
	}
	r, dir := fixture(t)
	file := filepath.Join(dir, "source")
	must(t, os.WriteFile(file, []byte("old"), 0600))
	// Remove only this fixture's ACLs so the new inode does not inherit a grant.
	for _, name := range []string{dir, file} {
		output, err := exec.Command("/bin/chmod", "-N", name).CombinedOutput()
		if err != nil {
			t.Fatalf("fixture ACL reset: %v: %s", err, output)
		}
	}
	must(t, os.Chmod(file, 0))
	username, err := exec.Command("/usr/bin/id", "-un").Output()
	must(t, err)
	grant := "user:" + strings.TrimSpace(string(username)) + " allow read"
	output, err := exec.Command("/bin/chmod", "+a", grant, file).CombinedOutput()
	if err != nil {
		t.Fatalf("fixture read ACL: %v: %s", err, output)
	}
	ctx := context.Background()
	before, err := r.Read(ctx, "source")
	must(t, err)
	if before.Text != "old" || before.Mode != 0 || before.SHA256 != digest("old") {
		t.Fatalf("ACL precondition = %+v", before)
	}
	t.Logf("actual preimage: euid=%d go=%s text=%q mode=%04o sha256=%s", os.Geteuid(), runtime.Version(), before.Text, before.Mode, before.SHA256)
	original := trustedInfo(t, file)
	old, err := os.Open(file)
	must(t, err)
	defer old.Close()
	var held, staged *os.File
	stageSyncs := 0
	r.io.sync = func(out *os.File) error {
		info, err := out.Stat()
		must(t, err)
		if !info.IsDir() {
			stageSyncs++
			staged = out
			if info.Mode().Perm() != 0 {
				t.Fatalf("stage mode before sync = %04o", info.Mode().Perm())
			}
			// A fresh path-based open really fails under the final ordinary mode.
			probe, err := os.Open(out.Name())
			if err == nil {
				probe.Close()
				t.Fatal("fixture stage unexpectedly readable by a new path open")
			}
			if !errors.Is(err, os.ErrPermission) {
				t.Fatalf("stage open precondition = %v", err)
			}
			t.Logf("actual final-mode stage path open: %v", err)
			fd, err := syscall.Dup(int(out.Fd()))
			must(t, err)
			held = os.NewFile(uintptr(fd), "held-stage-observation")
			t.Cleanup(func() { must(t, held.Close()) })
		}
		return out.Sync()
	}
	receipt, err := r.Replace(ctx, "source", before.SHA256, "new")
	t.Logf("Replace: receipt=%+v error=%v", receipt, err)
	if staged == nil || stageSyncs != 1 {
		t.Fatalf("stage sync count = %d", stageSyncs)
	}
	if _, closeErr := staged.Stat(); !errors.Is(closeErr, os.ErrClosed) {
		t.Errorf("production stage descriptor was not closed: %v", closeErr)
	}
	if err != nil {
		after, readErr := r.Read(ctx, "source")
		t.Logf("after failure: snapshot=%+v error=%v", after, readErr)
		if readErr != nil || after != before || !os.SameFile(original, trustedInfo(t, file)) {
			t.Error("failed preparation changed original bytes or inode")
		}
		assertNoStage(t, dir)
		t.Fatalf("readable ACL preimage must not require reopening the final-mode stage: %v", err)
	}
	if receipt != (WriteReceipt{Stage: StageDone, Effect: EffectApplied, BeforeSHA256: digest("old"), AfterSHA256: digest("new")}) {
		t.Fatalf("receipt = %+v", receipt)
	}
	final := trustedInfo(t, file)
	observed, err := held.Stat()
	must(t, err)
	data, err := io.ReadAll(io.NewSectionReader(held, 0, MaxTextBytes+1))
	must(t, err)
	if os.SameFile(original, final) || !os.SameFile(observed, final) || final.Mode().Perm() != 0 || string(data) != "new" || digest(string(data)) != receipt.AfterSHA256 {
		t.Fatalf("published inode observation: text=%q mode=%04o receipt=%+v", data, final.Mode().Perm(), receipt)
	}
	preimage, err := io.ReadAll(old)
	must(t, err)
	if string(preimage) != "old" {
		t.Fatalf("old inode changed: %q", preimage)
	}
	// Preserving ordinary mode is not ACL preservation or successful readback.
	if after, err := r.Read(ctx, "source"); !errors.Is(err, os.ErrPermission) || after != (Snapshot{}) {
		t.Fatalf("post-publication path read must remain honest: %+v, %v", after, err)
	} else {
		t.Logf("actual post-publication Read: %v", err)
	}
	assertNoStage(t, dir)
}

func TestWriteHeldStageValidation(t *testing.T) {
	for _, change := range []string{"bytes", "mode"} {
		t.Run(change, func(t *testing.T) {
			r, dir := fixture(t)
			file := filepath.Join(dir, "target")
			must(t, os.WriteFile(file, []byte("old"), 0600))
			var staged *os.File
			r.io.sync = func(out *os.File) error {
				staged = out
				if change == "bytes" {
					n, err := out.WriteAt([]byte("bad"), 0)
					must(t, err)
					if n != 3 {
						t.Fatalf("fixture short corruption write: %d", n)
					}
				} else {
					must(t, out.Chmod(0640))
				}
				return out.Sync()
			}
			receipt, err := r.Replace(context.Background(), "target", digest("old"), "new")
			if !errors.Is(err, ErrStage) || receipt.Stage != StagePrepare || receipt.Effect != EffectUnchanged {
				t.Fatalf("changed stage accepted: %+v, %v", receipt, err)
			}
			if staged == nil {
				t.Fatal("stage not observed")
			}
			if _, err := staged.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Errorf("failed stage descriptor was not closed: %v", err)
			}
			assertDisk(t, file, "old", 0600)
			assertNoStage(t, dir)
		})
	}
}
