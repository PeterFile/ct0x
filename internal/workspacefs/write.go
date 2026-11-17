//go:build unix

package workspacefs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"unicode/utf8"
)

var ErrHash = errors.New("预期 SHA256 必须是小写十六进制")

type WriteStage string
type WriteEffect string

const (
	StageValidate WriteStage = "validate"
	StagePrepare  WriteStage = "prepare"
	StagePublish  WriteStage = "publish"
	StageSync     WriteStage = "sync"
	StageDone     WriteStage = "done"

	// EffectUnchanged means this call did not publish to the target name.
	EffectUnchanged WriteEffect = "unchanged"
	// EffectApplied means publication succeeded, not necessarily directory sync.
	EffectApplied WriteEffect = "applied"
	// EffectUnknown means publication was attempted but its effect is uncertain.
	EffectUnknown WriteEffect = "unknown"
)

// WriteReceipt is meaningful even on error. Hashes describe the actual checked
// preimage and intended new bytes, not a read-after-write or an Artifact version.
// Only StageDone with nil error confirms file and parent directory sync succeeded.
// StageSync/EffectApplied on error means publication is known, durability is not.
// Any error with EffectApplied or EffectUnknown requires caller reconciliation;
// do not release the original turn/resource gate or automatically retry/rollback.
type WriteReceipt struct {
	Stage        WriteStage
	Effect       WriteEffect
	BeforeSHA256 string
	AfterSHA256  string
}

// These two syscall boundaries are private, per-instance fault-injection seams.
// They never provide a caller callback or an alternative filesystem path.
type writeIO struct {
	publish func(parent *os.Root, staged, name string, create bool) error
	sync    func(*os.File) error
}

func publish(parent *os.Root, staged, name string, create bool) error {
	if create {
		// Link publishes complete staged bytes atomically without overwriting.
		return parent.Link(staged, name)
	}
	return parent.Rename(staged, name)
}

func validHash(hash string) bool {
	if len(hash) != sha256.Size*2 {
		return false
	}
	for _, c := range hash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func absent(parent *os.Root, name string) error {
	_, err := parent.Lstat(name)
	if err == nil {
		return os.ErrExist
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func removeStage(parent *os.Root, name string) error {
	err := parent.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Replace checks the actual preimage against expectedSHA256, then publishes a
// synced replacement inode, preserving its ordinary permission bits (0777).
// It rechecks bytes, inode and metadata immediately before publication. Rename
// has no expected-inode/hash CAS: external writes in the final gap are excluded
// by the caller's trust/locking contract, not by these observations.
func (r *Root) Replace(ctx context.Context, name, expectedSHA256, text string) (WriteReceipt, error) {
	return r.write(ctx, name, expectedSHA256, text, false)
}

// Create atomically publishes complete text with mode 0600 only if name is absent.
// Neither method creates parents. Once publication is attempted, cancellation is
// not used to interrupt finishing sync or to replace the actual operation result.
func (r *Root) Create(ctx context.Context, name, text string) (WriteReceipt, error) {
	return r.write(ctx, name, "", text, true)
}

func (r *Root) write(ctx context.Context, name, expected, text string, create bool) (receipt WriteReceipt, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	receipt = WriteReceipt{Stage: StageValidate, Effect: EffectUnchanged}
	defer func() {
		if err != nil {
			err = fmt.Errorf("写入工作目录文本失败（阶段 %s，效果 %s）: %w", receipt.Stage, receipt.Effect, err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if !validPath(name, false) {
		return receipt, ErrPath
	}
	if !create && !validHash(expected) {
		return receipt, ErrHash
	}
	if len(text) > MaxTextBytes {
		return receipt, ErrTooLarge
	}
	if !utf8.ValidString(text) {
		return receipt, ErrUTF8
	}
	receipt.AfterSHA256 = textHash(text)
	if err := r.checkRoot(); err != nil {
		return receipt, err
	}
	dirName, leaf := path.Dir(name), path.Base(name)
	parent, err := r.openDir(dirName)
	if err != nil {
		return receipt, err
	}
	defer parent.Close()
	var before os.FileInfo
	mode := os.FileMode(0600)
	if create {
		if err := absent(parent, leaf); err != nil {
			return receipt, err
		}
	} else {
		var observed Snapshot
		observed, before, err = readText(ctx, parent, leaf)
		if err != nil {
			return receipt, err
		}
		receipt.BeforeSHA256 = observed.SHA256
		if observed.SHA256 != expected {
			return receipt, ErrConflict
		}
		mode = observed.Mode
	}
	dir, err := parent.Open(".")
	if err != nil {
		return receipt, err
	}
	defer dir.Close()

	receipt.Stage = StagePrepare
	stage := ".workspacefs-" + rand.Text()
	out, err := parent.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return receipt, err
	}
	stageLive := true
	defer func() {
		if out != nil {
			err = errors.Join(err, out.Close())
		}
		if stageLive {
			err = errors.Join(err, removeStage(parent, stage))
		}
	}()
	n, err := io.WriteString(out, text)
	if err != nil {
		return receipt, err
	}
	if n != len(text) {
		return receipt, io.ErrShortWrite
	}
	if err := out.Chmod(mode); err != nil {
		return receipt, err
	}
	if err := r.io.sync(out); err != nil {
		return receipt, err
	}
	err = out.Close()
	out = nil
	if err != nil {
		return receipt, err
	}
	if err := r.verifyDir(dirName, parent); err != nil {
		return receipt, err
	}
	if create {
		if err := absent(parent, leaf); err != nil {
			return receipt, err
		}
	} else {
		current, info, err := readText(ctx, parent, leaf)
		if err != nil {
			return receipt, errors.Join(ErrConflict, err)
		}
		if current.SHA256 != expected || !sameFileState(before, info) {
			return receipt, ErrConflict
		}
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}

	receipt.Stage, receipt.Effect = StagePublish, EffectUnknown
	if err := r.io.publish(parent, stage, leaf, create); err != nil {
		// EEXIST from link is a definitive no-overwrite result. Other publication
		// failures are conservatively unknown, including remote filesystem errors.
		if create && errors.Is(err, os.ErrExist) {
			receipt.Effect = EffectUnchanged
		}
		return receipt, err
	}
	receipt.Stage, receipt.Effect = StageSync, EffectApplied
	stageLive = false
	var cleanupErr error
	if create {
		cleanupErr = removeStage(parent, stage)
	}
	// Even a canceled caller must receive the real post-publication outcome.
	if err := errors.Join(cleanupErr, r.io.sync(dir), r.verifyDir(dirName, parent)); err != nil {
		return receipt, err
	}
	receipt.Stage = StageDone
	return receipt, nil
}
