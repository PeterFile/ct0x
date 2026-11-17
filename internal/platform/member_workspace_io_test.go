//go:build unix

package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/store"
	"agentworkspace/internal/workspacefs"
)

func memberWorkspaceDigest(text string) string    { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }
func memberWorkspaceExpected(hash string) *string { return &hash }

func TestMemberWorkspaceWriteInputBoundsAndConditions(t *testing.T) {
	valid := MemberWorkspaceWriteInput{Path: "report.txt", Text: "new", ExpectedSHA256: memberWorkspaceExpected(memberWorkspaceDigest("old"))}
	for _, input := range []MemberWorkspaceWriteInput{valid, {Path: "new.txt", Create: true, Text: ""},
		{Path: "new.txt", Create: true, Text: strings.Repeat("\x00", 8192)}} {
		if err := validateMemberWorkspaceWrite(input); err != nil {
			t.Fatalf("valid write rejected: %v", err)
		}
	}
	for _, input := range []MemberWorkspaceWriteInput{
		{Path: "report.txt", Text: "new"},
		{Path: "new.txt", Create: true, ExpectedSHA256: memberWorkspaceExpected("")},
		{Path: "new.txt", Create: true, Text: strings.Repeat("中", 2731)},
		{Path: "new.txt", Create: true, Text: strings.Repeat("a", 8193)},
		{Path: "new.txt", Create: true, Text: string([]byte{0xff})},
		{Path: "new.txt", ExpectedSHA256: memberWorkspaceExpected(strings.Repeat("A", 64))},
		{Path: strings.Repeat("<", 4096), Create: true, Text: strings.Repeat("\x00", 8192)},
	} {
		if err := validateMemberWorkspaceWrite(input); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("unsafe input accepted: %v", err)
		}
	}
	for _, name := range []string{"", "/absolute", "..", "../x", "a/../b", "./a", "a//b", "a/", "a\\b", "a\x00b", "a\nb", string([]byte{0xff}), strings.Repeat("x", 4097)} {
		if memberWorkspacePath(name, true) || memberWorkspacePath(name, false) {
			t.Fatalf("unsafe path accepted: %q", name)
		}
	}
	if !memberWorkspacePath(".", true) || memberWorkspacePath(".", false) {
		t.Fatal("root path is directory-only")
	}
}

func TestMemberWorkspaceArtifactInputUsesExactVersionCASWithoutSelectors(t *testing.T) {
	for _, input := range []MemberWorkspaceArtifactInput{{OperationID: "operation", Name: "报告"},
		{OperationID: "operation", ArtifactID: "artifact", ExpectedVersionID: "version"}} {
		if err := validateMemberWorkspaceArtifact(input); err != nil {
			t.Fatalf("valid publication rejected: %v", err)
		}
	}
	for _, input := range []MemberWorkspaceArtifactInput{{Name: "报告"}, {OperationID: "op"},
		{OperationID: "op", Name: "报告", ExpectedVersionID: "version"},
		{OperationID: "op", Name: "报告", ArtifactID: "artifact", ExpectedVersionID: "version"},
		{OperationID: "op", ArtifactID: "artifact"}, {OperationID: strings.Repeat("o", 257), Name: "报告"},
		{OperationID: "op", Name: strings.Repeat("中", 86)}, {OperationID: "op\u0085bad", Name: "报告"},
		{OperationID: "op", Name: string([]byte{0xff})}} {
		if err := validateMemberWorkspaceArtifact(input); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("invalid publication accepted: %v", err)
		}
	}
}

func TestMemberWorkspaceReadBudgetNeverTruncates(t *testing.T) {
	for _, text := range []string{strings.Repeat("a", 65536), strings.Repeat("\x00", 11000)} {
		if _, err := memberWorkspaceReadResult("report.txt", workspacefs.Snapshot{Text: text, SHA256: memberWorkspaceDigest(text)}); !errors.Is(err, ErrWorkspaceToolResultTooLarge) {
			t.Fatalf("encoded full result overflow was not rejected: %v", err)
		}
	}
	text := strings.Repeat("\x00", 10000)
	result, err := memberWorkspaceReadResult("report.txt", workspacefs.Snapshot{Text: text, SHA256: memberWorkspaceDigest(text)})
	if err != nil || result.Text != text {
		t.Fatal("bounded actual bytes were truncated or rejected")
	}
	wire, err := json.Marshal(result)
	if err != nil || len(wire)+1 > 65536 {
		t.Fatal("real JSON response exceeds HTTP budget")
	}
}

func TestMemberWorkspaceListFiltersSymlinksAndSpecialFiles(t *testing.T) {
	result, err := memberWorkspaceListResult([]workspacefs.Entry{
		{Name: "a.txt", Mode: 0600, Size: 3}, {Name: "dir", Mode: os.ModeDir | 0700},
		{Name: "outside", Mode: os.ModeSymlink | 0777}, {Name: "pipe", Mode: os.ModeNamedPipe | 0600},
	})
	if err != nil || len(result.Entries) != 2 || result.Entries[0].Kind != "file" || result.Entries[1].Kind != "directory" {
		t.Fatalf("unsafe directory projection: %+v %v", result, err)
	}
}

type memberWorkspaceIOFixture struct {
	path    string
	root    *workspacefs.Root
	blobs   *artifactfs.Store
	scratch *os.Root
}

func newMemberWorkspaceIO(t *testing.T) memberWorkspaceIOFixture {
	t.Helper()
	path := t.TempDir()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := workspacefs.Open(path, info)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	private, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { private.Close() })
	blobs, err := artifactfs.New(private, 65536)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	scratch, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { scratch.Close() })
	return memberWorkspaceIOFixture{path: path, root: root, blobs: blobs, scratch: scratch}
}

func TestMemberWorkspaceIOActualPreimageBlobAndReadback(t *testing.T) {
	f := newMemberWorkspaceIO(t)
	ctx := context.Background()
	before, after := "实际旧字节\x00\n", "实际新字节\n"
	if err := os.WriteFile(filepath.Join(f.path, "report.txt"), []byte(before), 0640); err != nil {
		t.Fatal(err)
	}
	preimage, err := f.root.Read(ctx, "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := memberWorkspaceSaveSnapshot(f.blobs, f.scratch, preimage)
	if err != nil || blob.Hash != memberWorkspaceDigest(before) || blob.Size != int64(len(before)) {
		t.Fatalf("actual backup: %+v %v", blob, err)
	}
	input := MemberWorkspaceWriteInput{Path: "report.txt", Text: after, ExpectedSHA256: memberWorkspaceExpected(preimage.SHA256)}
	receipt, observed, err := memberWorkspaceApply(ctx, f.root, input)
	if err != nil || receipt.Stage != workspacefs.StageDone || receipt.Effect != workspacefs.EffectApplied || observed.Text != after || observed.SHA256 != memberWorkspaceDigest(after) {
		t.Fatalf("actual write/readback not proven: %+v %+v %v", receipt, observed, err)
	}
	reader, err := f.blobs.Open(blob.Hash)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || string(data) != before {
		t.Fatal("immutable backup did not retain actual old bytes")
	}
	actual, err := os.ReadFile(filepath.Join(f.path, "report.txt"))
	if err != nil || string(actual) != after {
		t.Fatal("target bytes were not actually written")
	}
	dir, err := f.scratch.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := dir.ReadDir(-1)
	closeErr = dir.Close()
	if err != nil || closeErr != nil || len(entries) != 0 {
		t.Fatal("snapshot staging leaked")
	}
}

type changedWorkspaceReadback struct {
	*workspacefs.Root
	path string
}

func (r changedWorkspaceReadback) Read(ctx context.Context, name string) (workspacefs.Snapshot, error) {
	if err := os.WriteFile(filepath.Join(r.path, name), []byte("different actual bytes"), 0600); err != nil {
		return workspacefs.Snapshot{}, err
	}
	return r.Root.Read(ctx, name)
}

func TestMemberWorkspaceIOReadbackCannotUseIntendedHashAsObserved(t *testing.T) {
	f := newMemberWorkspaceIO(t)
	input := MemberWorkspaceWriteInput{Path: "report.txt", Create: true, Text: "intended bytes"}
	receipt, observed, err := memberWorkspaceApply(context.Background(), changedWorkspaceReadback{Root: f.root, path: f.path}, input)
	if !errors.Is(err, ErrWorkspaceOperationUnresolved) || observed.Text != "" || receipt.Effect != workspacefs.EffectApplied || receipt.AfterSHA256 != memberWorkspaceDigest(input.Text) {
		t.Fatalf("intended hash was accepted as actual readback: %+v %+v %v", receipt, observed, err)
	}
	actual, err := os.ReadFile(filepath.Join(f.path, input.Path))
	if err != nil || string(actual) != "different actual bytes" {
		t.Fatal("fault fixture did not change the real target")
	}
}

func TestMemberWorkspaceIOCreateConflictAndCancellationDoNotOverwrite(t *testing.T) {
	f := newMemberWorkspaceIO(t)
	input := MemberWorkspaceWriteInput{Path: "created.txt", Create: true, Text: "first"}
	_, observed, err := memberWorkspaceApply(context.Background(), f.root, input)
	if err != nil || observed.Text != "first" {
		t.Fatal("create did not verify real bytes")
	}
	input.Text = "second"
	receipt, _, err := memberWorkspaceApply(context.Background(), f.root, input)
	if err == nil || receipt.Effect != workspacefs.EffectUnchanged {
		t.Fatal("create overwrote existing bytes")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input = MemberWorkspaceWriteInput{Path: "created.txt", Text: "canceled", ExpectedSHA256: memberWorkspaceExpected(memberWorkspaceDigest("first"))}
	receipt, _, err = memberWorkspaceApply(ctx, f.root, input)
	if !errors.Is(err, context.Canceled) || receipt.Effect != workspacefs.EffectUnchanged {
		t.Fatal("pre-canceled IO lost actual unchanged result")
	}
	actual, err := os.ReadFile(filepath.Join(f.path, "created.txt"))
	if err != nil || string(actual) != "first" {
		t.Fatal("failed/canceled IO mutated the target")
	}
}

func TestMemberWorkspaceIORejectsForgedSnapshotAndEscapes(t *testing.T) {
	f := newMemberWorkspaceIO(t)
	if _, err := memberWorkspaceSaveSnapshot(f.blobs, f.scratch, workspacefs.Snapshot{Text: "real", SHA256: memberWorkspaceDigest("declared")}); err == nil {
		t.Fatal("declared hash substituted for actual backup bytes")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.path, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.root.Read(context.Background(), "link"); err == nil {
		t.Fatal("symlink disclosed external bytes")
	}
	input := MemberWorkspaceWriteInput{Path: "link", Text: "bad", ExpectedSHA256: memberWorkspaceExpected(memberWorkspaceDigest("secret"))}
	if _, _, err := memberWorkspaceApply(context.Background(), f.root, input); err == nil {
		t.Fatal("symlink target was written")
	}
	actual, err := os.ReadFile(outside)
	if err != nil || string(actual) != "secret" {
		t.Fatal("external target changed")
	}
}
