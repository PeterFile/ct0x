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
	"syscall"
	"unicode/utf8"
)

const stagingDirName = ".workspacefs-staging"

var (
	ErrHash  = errors.New("预期 SHA256 必须是小写十六进制")
	ErrStage = errors.New("私有暂存目录或文件身份无效或已改变")
)

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

func privateStageDir(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func openStaging(parent *os.Root) (*os.Root, error) {
	if err := parent.Mkdir(stagingDirName, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := parent.Lstat(stagingDirName)
	if err != nil || !privateStageDir(info) {
		return nil, errors.Join(ErrStage, err)
	}
	root, err := parent.OpenRoot(stagingDirName)
	if err != nil {
		return nil, errors.Join(ErrStage, err)
	}
	held, err := root.Stat(".")
	if err != nil || !privateStageDir(held) || !os.SameFile(info, held) {
		return nil, errors.Join(ErrStage, err, root.Close())
	}
	return root, nil
}

// Cleanup is relative to the pinned private directory, never the public name
// of that directory. Leave the namespace itself in place. The identity check is
// defense in depth, not unlink CAS; nonprotocol namespace writers are excluded.
func removeStage(root *os.Root, name string, owned os.FileInfo) error {
	current, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || owned == nil || !os.SameFile(owned, current) {
		return errors.Join(ErrStage, err)
	}
	return root.Remove(name)
}

func verifyStaging(parent, root *os.Root) error {
	current, err := parent.Lstat(stagingDirName)
	held, heldErr := root.Stat(".")
	if err != nil || heldErr != nil || !privateStageDir(current) || !privateStageDir(held) || !os.SameFile(current, held) {
		return errors.Join(ErrStage, err, heldErr)
	}
	return nil
}

// verifyStage reads the actual inode through the pre-held readable descriptor.
// Final permission bits may forbid a new path open; they do not revoke this fd.
// Path and metadata checks remain observations, not protection from excluded writers.
func verifyStage(ctx context.Context, root *os.Root, name string, file *os.File, owned os.FileInfo, hash string, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := root.Lstat(name)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || !info.Mode().IsRegular() {
		return ErrNotRegular
	}
	if !os.SameFile(owned, info) || !sameFileState(before, info) || info.Mode().Perm() != mode {
		return ErrStage
	}
	if info.Size() > MaxTextBytes {
		return ErrTooLarge
	}
	data, err := io.ReadAll(io.NewSectionReader(file, 0, MaxTextBytes+1))
	if err != nil {
		return err
	}
	if len(data) > MaxTextBytes {
		return ErrTooLarge
	}
	if !utf8.Valid(data) {
		return ErrUTF8
	}
	after, statErr := file.Stat()
	named, nameErr := root.Lstat(name)
	if statErr != nil || nameErr != nil {
		return errors.Join(ErrStage, statErr, nameErr)
	}
	if !sameFileState(info, after) || !sameFileState(info, named) || int64(len(data)) != info.Size() || textHash(string(data)) != hash {
		return ErrStage
	}
	return ctx.Err()
}

// Replace checks the actual preimage against expectedSHA256, then publishes a
// synced replacement inode, preserving its ordinary permission bits (0777).
// It rechecks bytes, inode and metadata immediately before publication. The
// private staging namespace must remain exclusive to this protocol (see Open).
// Neither staging checks nor target Rename provide inode/hash CAS; external
// writers in the final gap are excluded by that contract, not by observations.
func (r *Root) Replace(ctx context.Context, name, expectedSHA256, text string) (WriteReceipt, error) {
	return r.write(ctx, name, expectedSHA256, text, false)
}

// Create atomically publishes complete text with mode 0600 only if name is absent.
// Every error after a publication attempt, including EEXIST, remains unknown.
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
	if err := checkReservedTarget(parent, leaf); err != nil {
		return receipt, err
	}
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
	private, err := openStaging(parent)
	if err != nil {
		return receipt, err
	}
	defer private.Close()
	privateDir, err := private.Open(".")
	if err != nil {
		return receipt, err
	}
	defer privateDir.Close()
	stage := rand.Text()
	out, err := private.OpenFile(stage, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return receipt, err
	}
	owned, statErr := out.Stat()
	stageLive := true
	defer func() {
		if out != nil {
			err = errors.Join(err, out.Close())
		}
		if stageLive {
			err = errors.Join(err, removeStage(private, stage, owned))
		}
	}()
	if statErr != nil {
		return receipt, statErr
	}
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
	if err := r.verifyDir(dirName, parent); err != nil {
		return receipt, err
	}
	// This call may have created the canonical namespace since the first check.
	if err := checkReservedTarget(parent, leaf); err != nil {
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
	if err := verifyStage(ctx, private, stage, out, owned, receipt.AfterSHA256, mode); err != nil {
		return receipt, errors.Join(ErrStage, err)
	}
	err = out.Close()
	out = nil
	if err != nil {
		return receipt, err
	}
	if err := verifyStaging(parent, private); err != nil {
		return receipt, err
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}

	receipt.Stage, receipt.Effect = StagePublish, EffectUnknown
	if err := r.io.publish(parent, path.Join(stagingDirName, stage), leaf, create); err != nil {
		return receipt, err
	}
	receipt.Stage, receipt.Effect = StageSync, EffectApplied
	stageLive = false
	var cleanupErr error
	if create {
		cleanupErr = removeStage(private, stage, owned)
	}
	// Cross-directory publication needs both directories synced. Even a canceled
	// caller receives the real post-publication outcome, never a rollback claim.
	if err := errors.Join(cleanupErr, privateDir.Sync(), r.io.sync(dir), r.verifyDir(dirName, parent), verifyStaging(parent, private)); err != nil {
		return receipt, err
	}
	receipt.Stage = StageDone
	return receipt, nil
}
