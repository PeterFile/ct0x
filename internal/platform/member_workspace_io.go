//go:build unix

package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/store"
	"agentworkspace/internal/workspacefs"
)

// MaxMemberWorkspaceWriteBytes leaves room for JSON escaping and receipt fields.
// The full encoded request and result must independently fit the HTTP budget.
const MaxMemberWorkspaceWriteBytes = 8192
const memberWorkspaceJSONBytes = 64 * 1024

var (
	ErrWorkspaceToolResultTooLarge  = errors.New("文件工具完整结果超过64 KiB限制，未返回部分内容")
	ErrWorkspaceOperationUnresolved = errors.New("文件操作结果未确认，须保留原执行和资源并先对账，不要重试或回滚")
	errMemberWorkspaceIO            = errors.New("工作目录实际文件读写或不可变快照保存失败")
)

// These inputs contain task-local data only. Root, role, grants and caller are
// private runtime values and are never selected through these fields.
type MemberWorkspacePathInput struct {
	Path string `json:"path"`
}

type MemberWorkspaceWriteInput struct {
	Path           string  `json:"path"`
	Create         bool    `json:"create"`
	Text           string  `json:"text"`
	ExpectedSHA256 *string `json:"expected_sha256,omitempty"`
}

type MemberWorkspaceArtifactInput struct {
	OperationID       string `json:"operation_id"`
	ArtifactID        string `json:"artifact_id,omitempty"`
	Name              string `json:"name,omitempty"`
	ExpectedVersionID string `json:"expected_version_id,omitempty"`
}

type MemberWorkspaceReadResult struct {
	Path   string `json:"path"`
	Text   string `json:"text"`
	SHA256 string `json:"sha256"`
}

type MemberWorkspaceEntry struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

type MemberWorkspaceListResult struct {
	Entries []MemberWorkspaceEntry `json:"entries"`
}

type MemberWorkspaceWriteResult struct {
	OperationID  string `json:"operation_id"`
	Outcome      string `json:"outcome"`
	BeforeSHA256 string `json:"before_sha256"`
	AfterSHA256  string `json:"after_sha256"`
	TextOmitted  bool   `json:"text_omitted"`
}

type MemberWorkspaceArtifactResult struct {
	ArtifactID string `json:"artifact_id"`
	VersionID  string `json:"version_id"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Applied    bool   `json:"applied"`
}

func memberWorkspaceHash(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }

func memberWorkspaceSHA256(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, r := range hash {
		if !(r >= 'a' && r <= 'f' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func memberWorkspacePath(name string, directory bool) bool {
	return len(name) <= 4096 && utf8.ValidString(name) && fs.ValidPath(name) &&
		!strings.ContainsRune(name, '\\') && !strings.ContainsFunc(name, unicode.IsControl) && (directory || name != ".")
}

func memberWorkspaceJSONBudget(value any) error {
	wire, err := json.Marshal(value)
	if err != nil || len(wire)+1 > memberWorkspaceJSONBytes {
		return ErrWorkspaceToolResultTooLarge
	}
	return nil
}

func validateMemberWorkspaceWrite(input MemberWorkspaceWriteInput) error {
	if !memberWorkspacePath(input.Path, false) || !utf8.ValidString(input.Text) || len(input.Text) > MaxMemberWorkspaceWriteBytes ||
		(input.ExpectedSHA256 == nil) != input.Create || input.ExpectedSHA256 != nil && !memberWorkspaceSHA256(*input.ExpectedSHA256) {
		return store.ErrInvalid
	}
	if memberWorkspaceJSONBudget(input) != nil {
		return store.ErrInvalid
	}
	return nil
}

func validateMemberWorkspaceArtifact(input MemberWorkspaceArtifactInput) error {
	for _, value := range []string{input.OperationID, input.ArtifactID, input.Name, input.ExpectedVersionID} {
		if len(value) > 256 || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
			return store.ErrInvalid
		}
	}
	if strings.TrimSpace(input.OperationID) == "" {
		return store.ErrInvalid
	}
	if input.ArtifactID == "" {
		if strings.TrimSpace(input.Name) == "" || input.ExpectedVersionID != "" {
			return store.ErrInvalid
		}
	} else if strings.TrimSpace(input.ArtifactID) == "" || input.Name != "" || strings.TrimSpace(input.ExpectedVersionID) == "" {
		return store.ErrInvalid
	}
	return nil
}

func memberWorkspaceReadResult(name string, observed workspacefs.Snapshot) (MemberWorkspaceReadResult, error) {
	result := MemberWorkspaceReadResult{Path: name, Text: observed.Text, SHA256: observed.SHA256}
	if err := memberWorkspaceJSONBudget(result); err != nil {
		return MemberWorkspaceReadResult{}, err
	}
	return result, nil
}

func memberWorkspaceListResult(entries []workspacefs.Entry) (MemberWorkspaceListResult, error) {
	result := MemberWorkspaceListResult{Entries: make([]MemberWorkspaceEntry, 0, len(entries))}
	for _, entry := range entries {
		kind := "file"
		if entry.Mode.IsDir() {
			kind = "directory"
		} else if !entry.Mode.IsRegular() {
			continue
		}
		result.Entries = append(result.Entries, MemberWorkspaceEntry{Name: entry.Name, Kind: kind, Size: entry.Size})
	}
	if err := memberWorkspaceJSONBudget(result); err != nil {
		return MemberWorkspaceListResult{}, err
	}
	return result, nil
}

// memberWorkspaceSaveSnapshot persists actual bytes already observed through
// workspacefs, never a model-declared hash. The trusted private scratch root and
// artifactfs owner must outlive this call and remain outside every source grant.
// A later failure can leave a safe immutable blob orphan, not a partial intent.
func memberWorkspaceSaveSnapshot(blobs *artifactfs.Store, scratch *os.Root, snapshot workspacefs.Snapshot) (blob artifactfs.Blob, err error) {
	if blobs == nil || scratch == nil || len(snapshot.Text) > workspacefs.MaxTextBytes || !utf8.ValidString(snapshot.Text) ||
		snapshot.SHA256 != memberWorkspaceHash(snapshot.Text) {
		return artifactfs.Blob{}, errMemberWorkspaceIO
	}
	name := ".workspace-snapshot-" + rand.Text()
	file, err := scratch.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return artifactfs.Blob{}, errMemberWorkspaceIO
	}
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		err = errors.Join(err, scratch.Remove(name))
		if err != nil {
			blob, err = artifactfs.Blob{}, errMemberWorkspaceIO
		}
	}()
	if n, writeErr := io.WriteString(file, snapshot.Text); writeErr != nil || n != len(snapshot.Text) {
		return artifactfs.Blob{}, errMemberWorkspaceIO
	}
	if err := file.Close(); err != nil {
		file = nil
		return artifactfs.Blob{}, errMemberWorkspaceIO
	}
	file = nil
	blob, err = blobs.Publish(scratch, name)
	if err != nil || blob.Hash != snapshot.SHA256 || blob.Size != int64(len(snapshot.Text)) {
		return artifactfs.Blob{}, errMemberWorkspaceIO
	}
	return blob, nil
}

// The concrete runtime implementation is always a pinned workspacefs.Root.
// Keeping the IO boundary narrow permits independent observed-byte verification.
type memberWorkspaceFileIO interface {
	Create(context.Context, string, string) (workspacefs.WriteReceipt, error)
	Replace(context.Context, string, string, string) (workspacefs.WriteReceipt, error)
	Read(context.Context, string) (workspacefs.Snapshot, error)
}

// The caller commits intent BEFORE this function and retains the original claim.
// AfterSHA256 in the native write receipt is intended, never observed evidence.
// Only an actual readback of the complete target bytes can prove the after state.
func memberWorkspaceApply(ctx context.Context, root memberWorkspaceFileIO, input MemberWorkspaceWriteInput) (workspacefs.WriteReceipt, workspacefs.Snapshot, error) {
	receipt := workspacefs.WriteReceipt{Stage: workspacefs.StageValidate, Effect: workspacefs.EffectUnchanged}
	if err := validateMemberWorkspaceWrite(input); err != nil {
		return receipt, workspacefs.Snapshot{}, err
	}
	if root == nil {
		return receipt, workspacefs.Snapshot{}, errMemberWorkspaceIO
	}
	var err error
	if input.Create {
		receipt, err = root.Create(ctx, input.Path, input.Text)
	} else {
		receipt, err = root.Replace(ctx, input.Path, *input.ExpectedSHA256, input.Text)
	}
	if err != nil {
		return receipt, workspacefs.Snapshot{}, err
	}
	if receipt.Stage != workspacefs.StageDone || receipt.Effect != workspacefs.EffectApplied {
		return receipt, workspacefs.Snapshot{}, ErrWorkspaceOperationUnresolved
	}
	observed, err := root.Read(ctx, input.Path)
	if err != nil || observed.SHA256 != memberWorkspaceHash(input.Text) || len(observed.Text) != len(input.Text) {
		return receipt, workspacefs.Snapshot{}, ErrWorkspaceOperationUnresolved
	}
	return receipt, observed, nil
}
