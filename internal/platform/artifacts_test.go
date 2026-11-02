//go:build unix

package platform

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

type artifactsFixture struct {
	db               *store.Store
	blobs            *artifactfs.Store
	service          *Artifacts
	raw              *sql.DB
	source, private  *os.Root
	dbPath           string
	groupID, otherID string
	maxBytes         int64
}

func artifactsMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newArtifactsFixture(t *testing.T, maxBytes int64) *artifactsFixture {
	t.Helper()
	f := &artifactsFixture{dbPath: filepath.Join(t.TempDir(), "workspace.db"), maxBytes: maxBytes}
	var err error
	f.db, err = store.Open(f.dbPath)
	artifactsMust(t, err)
	t.Cleanup(func() { artifactsMust(t, f.db.Close()) })
	f.source, err = os.OpenRoot(t.TempDir())
	artifactsMust(t, err)
	t.Cleanup(func() { artifactsMust(t, f.source.Close()) })
	f.private, err = os.OpenRoot(t.TempDir())
	artifactsMust(t, err)
	t.Cleanup(func() { artifactsMust(t, f.private.Close()) })
	f.blobs, err = artifactfs.New(f.private, maxBytes)
	artifactsMust(t, err)
	t.Cleanup(func() { artifactsMust(t, f.blobs.Close()) })
	f.service, err = NewArtifacts(f.db, f.blobs)
	artifactsMust(t, err)
	f.raw, err = sql.Open("sqlite", f.dbPath)
	artifactsMust(t, err)
	t.Cleanup(func() { artifactsMust(t, f.raw.Close()) })
	local := NewLocalUser(f.db)
	group, err := local.CreateGroup(context.Background(), GroupInput{Name: "artifacts"}, "")
	artifactsMust(t, err)
	other, err := local.CreateGroup(context.Background(), GroupInput{Name: "other"}, "")
	artifactsMust(t, err)
	f.groupID, f.otherID = group.ID, other.ID
	return f
}

func (f *artifactsFixture) publish(t *testing.T, input PublishArtifactInput, data, key string) domain.ArtifactVersion {
	t.Helper()
	artifactsMust(t, f.source.WriteFile("input", []byte(data), 0600))
	v, err := f.service.PublishArtifact(context.Background(), f.groupID, input, f.source, "input", key)
	artifactsMust(t, err)
	return v
}

func (f *artifactsFixture) counts(t *testing.T, artifacts, versions, receipts int) {
	t.Helper()
	for query, want := range map[string]int{
		"SELECT count(*) FROM artifacts":                                               artifacts,
		"SELECT count(*) FROM artifact_versions":                                       versions,
		"SELECT count(*) FROM mutation_idempotency WHERE operation='publish_artifact'": receipts,
	} {
		var got int
		if err := f.raw.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: %d, %v; want %d", query, got, err, want)
		}
	}
}

func artifactsDigest(data string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(data))) }

func artifactsRead(t *testing.T, reader io.ReadCloser, want string) {
	t.Helper()
	if reader == nil {
		t.Fatal("successful content has no reader")
	}
	data, err := io.ReadAll(reader)
	closeErr := reader.Close()
	artifactsMust(t, errors.Join(err, closeErr))
	if string(data) != want {
		t.Fatalf("content = %q, want %q", data, want)
	}
}

func (f *artifactsFixture) content(t *testing.T, want domain.ArtifactVersion, data string) {
	t.Helper()
	got, err := f.service.OpenArtifactVersion(context.Background(), want.GroupID, want.ArtifactID, want.ID)
	artifactsMust(t, err)
	if !reflect.DeepEqual(got.Version, want) || got.Artifact.ID != want.ArtifactID || got.Artifact.GroupID != want.GroupID {
		if got.Reader != nil {
			got.Reader.Close()
		}
		t.Fatalf("content metadata = %+v, want %+v", got, want)
	}
	encoded, err := json.Marshal(got)
	artifactsMust(t, err)
	var fields map[string]json.RawMessage
	artifactsMust(t, json.Unmarshal(encoded, &fields))
	if len(fields) != 2 || fields["artifact"] == nil || fields["version"] == nil {
		got.Reader.Close()
		t.Fatalf("reader escaped metadata JSON: %s", encoded)
	}
	artifactsRead(t, got.Reader, data)
}

func artifactsNoReader(t *testing.T, got ArtifactContent, err, want error) {
	t.Helper()
	if got.Reader != nil {
		got.Reader.Close()
		t.Fatal("failed open exposed a reader")
	}
	if !errors.Is(err, want) {
		t.Fatalf("open error = %v, want %v", err, want)
	}
}

func (f *artifactsFixture) reopen(t *testing.T) {
	t.Helper()
	artifactsMust(t, f.blobs.Close())
	artifactsMust(t, f.db.Close())
	var err error
	f.db, err = store.Open(f.dbPath)
	artifactsMust(t, err)
	f.blobs, err = artifactfs.New(f.private, f.maxBytes)
	artifactsMust(t, err)
	f.service, err = NewArtifacts(f.db, f.blobs)
	artifactsMust(t, err)
}

func TestArtifactsConstructorRejectsMissingDependencies(t *testing.T) {
	f := newArtifactsFixture(t, 64)
	for _, deps := range []struct {
		db    *store.Store
		blobs *artifactfs.Store
	}{{nil, f.blobs}, {f.db, nil}, {nil, nil}} {
		if got, err := NewArtifacts(deps.db, deps.blobs); got != nil || !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("constructor = %v, %v; want nil, ErrInvalid", got, err)
		}
	}
	// Rejected construction must not close either supplied dependency.
	v := f.publish(t, PublishArtifactInput{Name: "still usable"}, "bytes", "")
	f.content(t, v, "bytes")
}

func TestArtifactsPublishesActualBytesAndBindsUser(t *testing.T) {
	f := newArtifactsFixture(t, 256)
	ctx := context.Background()
	task, err := NewLocalUser(f.db).CreateTask(ctx, f.groupID, TaskInput{Title: "source", Acceptance: "report"}, "")
	artifactsMust(t, err)
	var input PublishArtifactInput
	payload := fmt.Sprintf(`{"name":" 报告.md ","source_task_id":%q,"actor_id":"not-user","group_id":%q,"blob_hash":"forged","size":999,"source_root":"/private","grants":["all"],"capability":"forged","claim":"forged"}`, task.ID, f.otherID)
	artifactsMust(t, json.Unmarshal([]byte(payload), &input))
	encoded, err := json.Marshal(input)
	artifactsMust(t, err)
	var fields map[string]json.RawMessage
	artifactsMust(t, json.Unmarshal(encoded, &fields))
	if len(fields) != 4 || fields["artifact_id"] == nil || fields["name"] == nil || fields["source_task_id"] == nil || fields["expected_version_id"] == nil {
		t.Fatalf("publication DTO is not narrow: %s", encoded)
	}
	data := "真实复制的报告\x00\n"
	v := f.publish(t, input, data, "publish")
	if v.ID == "" || v.ArtifactID == "" || v.AuthorID != "user" || v.GroupID != f.groupID || v.SourceTaskID != task.ID ||
		v.BlobHash != artifactsDigest(data) || v.Size != int64(len(data)) || !v.Applied || v.ID == v.BlobHash {
		t.Fatalf("untrusted or fabricated publication receipt: %+v", v)
	}
	a, err := f.service.GetArtifact(ctx, f.groupID, v.ArtifactID)
	if err != nil || a.Name != input.Name || a.CurrentVersionID != v.ID {
		t.Fatalf("artifact text/head: %+v, %v", a, err)
	}
	stored, err := f.service.GetArtifactVersion(ctx, f.groupID, v.ArtifactID, v.ID)
	if err != nil || !reflect.DeepEqual(stored, v) {
		t.Fatalf("version not persisted: %+v, %v", stored, err)
	}
	// Exact Store replay verifies the bound actor, key, metadata and real blob receipt.
	replay, err := f.db.PublishArtifact(ctx, store.PublishArtifactInput{
		GroupID: f.groupID, Name: input.Name, SourceTaskID: task.ID, BlobHash: v.BlobHash, Size: v.Size,
		ActorID: "user", IdempotencyKey: "publish",
	})
	if err != nil || !reflect.DeepEqual(replay, v) {
		t.Fatalf("bound publication differs from persisted receipt: %+v, %v", replay, err)
	}
	artifactsMust(t, f.source.WriteFile("input", []byte("edited"), 0600))
	artifactsMust(t, f.source.Remove("input"))
	f.content(t, v, data)
	if got, err := f.db.GetTask(ctx, f.groupID, task.ID); err != nil || !reflect.DeepEqual(got, task) {
		t.Fatalf("publication changed source task: %+v, %v", got, err)
	}
	f.counts(t, 1, 1, 1)
}

func TestArtifactsStableReplayAndDurableCandidates(t *testing.T) {
	f := newArtifactsFixture(t, 64)
	ctx := context.Background()
	create := PublishArtifactInput{Name: "report"}
	base := f.publish(t, create, "base", "create")
	update := PublishArtifactInput{ArtifactID: base.ArtifactID, ExpectedVersionID: base.ID}
	head := f.publish(t, update, "head", "head")
	candidate := f.publish(t, update, "candidate", "candidate")
	if !head.Applied || head.ObservedVersionID != base.ID || candidate.Applied || candidate.ExpectedVersionID != base.ID || candidate.ObservedVersionID != head.ID {
		t.Fatalf("CAS evidence: head=%+v candidate=%+v", head, candidate)
	}
	later := f.publish(t, PublishArtifactInput{ArtifactID: base.ArtifactID, ExpectedVersionID: head.ID}, "later", "later")
	for _, retry := range []struct {
		input PublishArtifactInput
		data  string
		key   string
		want  domain.ArtifactVersion
	}{{create, "base", "create", base}, {update, "head", "head", head}, {update, "candidate", "candidate", candidate}} {
		if got := f.publish(t, retry.input, retry.data, retry.key); !reflect.DeepEqual(got, retry.want) {
			t.Fatalf("replay followed mutable head: %+v, want %+v", got, retry.want)
		}
	}
	artifactsMust(t, f.source.WriteFile("input", []byte("changed bytes"), 0600))
	if got, err := f.service.PublishArtifact(ctx, f.groupID, create, f.source, "input", "create"); !errors.Is(err, store.ErrConflict) || got.ID != "" {
		t.Fatalf("changed bytes reused key: %+v, %v", got, err)
	}
	artifactsMust(t, f.source.Remove("input"))
	if got, err := f.service.PublishArtifact(ctx, f.groupID, create, f.source, "input", "create"); !errors.Is(err, os.ErrNotExist) || got.ID != "" {
		t.Fatalf("missing source fabricated a pre-copy replay: %+v, %v", got, err)
	}
	check := func() {
		t.Helper()
		a, err := f.service.GetArtifact(ctx, f.groupID, base.ArtifactID)
		if err != nil || a.CurrentVersionID != later.ID {
			t.Fatalf("retry/candidate changed head: %+v, %v", a, err)
		}
		for _, pair := range []struct {
			v    domain.ArtifactVersion
			data string
		}{{base, "base"}, {head, "head"}, {candidate, "candidate"}, {later, "later"}} {
			f.content(t, pair.v, pair.data)
		}
		f.counts(t, 1, 4, 4)
	}
	check()
	f.reopen(t)
	check()
}

func TestArtifactsMetadataDelegatesBoundedScope(t *testing.T) {
	f := newArtifactsFixture(t, 64)
	ctx := context.Background()
	base := f.publish(t, PublishArtifactInput{Name: "first"}, "first", "")
	f.publish(t, PublishArtifactInput{ArtifactID: base.ArtifactID, ExpectedVersionID: base.ID}, "second", "")
	neighbor := f.publish(t, PublishArtifactInput{Name: "neighbor"}, "neighbor", "")
	for _, options := range []store.ListOptions{{Limit: 1}, {Limit: 1, Before: &store.Cursor{CreatedAt: neighbor.CreatedAt, ID: neighbor.ArtifactID}}} {
		got, err := f.service.ListArtifacts(ctx, f.groupID, options)
		want, wantErr := f.db.ListArtifacts(ctx, f.groupID, options)
		if err != nil || wantErr != nil || !reflect.DeepEqual(got, want) || len(got.Items) != 1 {
			t.Fatalf("bounded artifacts: %+v, %v; Store: %+v, %v", got, err, want, wantErr)
		}
	}
	options := store.ListOptions{Limit: 1}
	got, err := f.service.ListArtifactVersions(ctx, f.groupID, base.ArtifactID, options)
	want, wantErr := f.db.ListArtifactVersions(ctx, f.groupID, base.ArtifactID, options)
	if err != nil || wantErr != nil || !reflect.DeepEqual(got, want) || len(got.Items) != 1 || got.NextCursor == nil {
		t.Fatalf("bounded versions: %+v, %v; Store: %+v, %v", got, err, want, wantErr)
	}
	empty, err := f.service.ListArtifacts(ctx, f.otherID, store.ListOptions{})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != nil {
		t.Fatalf("isolated empty group: %+v, %v", empty, err)
	}
	for _, scope := range [][3]string{{f.otherID, base.ArtifactID, base.ID}, {f.groupID, neighbor.ArtifactID, base.ID}} {
		if _, err := f.service.GetArtifactVersion(ctx, scope[0], scope[1], scope[2]); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("version scope %v: %v", scope, err)
		}
	}
	if _, err := f.service.GetArtifact(ctx, f.otherID, base.ArtifactID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("artifact group scope: %v", err)
	}
	if _, err := f.service.ListArtifactVersions(ctx, f.otherID, base.ArtifactID, store.ListOptions{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("version list scope: %v", err)
	}
	if _, err := f.service.ListArtifacts(ctx, "missing", store.ListOptions{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing group list: %v", err)
	}
	if _, err := f.service.ListArtifacts(ctx, f.groupID, store.ListOptions{Limit: 201}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("artifact list limit: %v", err)
	}
	if _, err := f.service.ListArtifactVersions(ctx, f.groupID, base.ArtifactID, store.ListOptions{Limit: 201}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("version list limit: %v", err)
	}
}

func TestArtifactsDelegatesMetadataAndTaskValidation(t *testing.T) {
	f := newArtifactsFixture(t, 64)
	ctx := context.Background()
	local := NewLocalUser(f.db)
	task, err := local.CreateTask(ctx, f.otherID, TaskInput{Title: "foreign", Acceptance: "report"}, "")
	artifactsMust(t, err)
	ownTask, err := local.CreateTask(ctx, f.groupID, TaskInput{Title: "local", Acceptance: "report"}, "")
	artifactsMust(t, err)
	artifactsMust(t, f.source.WriteFile("input", []byte("bytes"), 0600))
	for _, tc := range []struct {
		name  string
		input PublishArtifactInput
		want  error
	}{
		{"blank text", PublishArtifactInput{Name: " \t"}, store.ErrInvalid},
		{"foreign task", PublishArtifactInput{Name: "report", SourceTaskID: task.ID}, store.ErrNotFound},
		{"exact task identity", PublishArtifactInput{Name: "report", SourceTaskID: " " + ownTask.ID}, store.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := f.service.PublishArtifact(ctx, f.groupID, tc.input, f.source, "input", "retry"); !errors.Is(err, tc.want) || got.ID != "" {
				t.Fatalf("validation = %+v, %v; want %v", got, err, tc.want)
			}
		})
	}
	f.counts(t, 0, 0, 0)
	f.publish(t, PublishArtifactInput{Name: "fixed"}, "bytes", "retry")
	f.counts(t, 1, 1, 1)
}

func TestArtifactsChecksContextAndGroupBeforeCopy(t *testing.T) {
	f := newArtifactsFixture(t, 64)
	artifactsMust(t, f.blobs.Close())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := PublishArtifactInput{Name: "report"}
	if got, err := f.service.PublishArtifact(ctx, f.groupID, input, nil, "missing", "retry"); !errors.Is(err, context.Canceled) || got.ID != "" {
		t.Fatalf("canceled before IO: %+v, %v", got, err)
	}
	if got, err := f.service.PublishArtifact(context.Background(), "missing", input, nil, "missing", "retry"); !errors.Is(err, store.ErrNotFound) || got.ID != "" {
		t.Fatalf("group check did not precede blob/source IO: %+v, %v", got, err)
	}
	if _, err := f.service.PublishArtifact(context.Background(), f.groupID, input, f.source, "input", "retry"); !errors.Is(err, artifactfs.ErrClosed) {
		t.Fatalf("valid group did not reach blobstore: %v", err)
	}
	got, err := f.service.OpenArtifactVersion(ctx, f.groupID, "missing", "missing")
	artifactsNoReader(t, got, err, context.Canceled)
	f.counts(t, 0, 0, 0)
}

// Observe real filesystem state to cancel a standard context without timing or product hooks.
type artifactsCancelWhenContext struct {
	context.Context
	ready  func() bool
	cancel context.CancelFunc
}

func (c artifactsCancelWhenContext) Err() error {
	if c.ready() {
		c.cancel()
	}
	return c.Context.Err()
}

func TestArtifactsCancellationAfterCopySkipsRegistry(t *testing.T) {
	f := newArtifactsFixture(t, 64)
	data := "copied before cancellation"
	artifactsMust(t, f.source.WriteFile("input", []byte(data), 0600))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	copyCtx := artifactsCancelWhenContext{ctx, func() bool {
		_, err := f.private.Stat(filepath.Join("blobs", artifactsDigest(data)))
		return err == nil
	}, cancel}
	input := PublishArtifactInput{Name: "report"}
	got, err := f.service.PublishArtifact(copyCtx, f.groupID, input, f.source, "input", "retry")
	if !errors.Is(err, context.Canceled) || got.ID != "" {
		t.Fatalf("copy-time cancellation registered metadata: %+v, %v", got, err)
	}
	f.counts(t, 0, 0, 0)
	reader, err := f.blobs.Open(artifactsDigest(data))
	artifactsMust(t, err)
	artifactsRead(t, reader, data)
	f.publish(t, input, data, "retry")
	f.counts(t, 1, 1, 1)
}

func TestArtifactsCancellationAfterOpenClosesReader(t *testing.T) {
	f := newArtifactsFixture(t, 64)
	v := f.publish(t, PublishArtifactInput{Name: "report"}, "bytes", "")
	info, err := f.private.Stat(filepath.Join("blobs", v.BlobHash))
	artifactsMust(t, err)
	blobStat := info.Sys().(*syscall.Stat_t)
	hasOpenBlob := func() bool {
		dir, err := os.Open("/dev/fd")
		artifactsMust(t, err)
		// Read names only; FD directory entries may disappear before stat.
		names, readErr := dir.Readdirnames(-1)
		artifactsMust(t, errors.Join(readErr, dir.Close()))
		for _, name := range names {
			fd, err := strconv.Atoi(name)
			if err != nil {
				continue
			}
			var stat syscall.Stat_t
			// fstat works on both Linux and macOS; /dev/fd path stat does not.
			if syscall.Fstat(fd, &stat) == nil && stat.Dev == blobStat.Dev && stat.Ino == blobStat.Ino {
				return true
			}
		}
		return false
	}
	if hasOpenBlob() {
		t.Fatal("fixture already has a blob reader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	openCtx := artifactsCancelWhenContext{ctx, hasOpenBlob, cancel}
	got, err := f.service.OpenArtifactVersion(openCtx, f.groupID, v.ArtifactID, v.ID)
	artifactsNoReader(t, got, err, context.Canceled)
	if hasOpenBlob() {
		t.Fatal("cancellation leaked the verified blob reader")
	}
	f.content(t, v, "bytes")
}

func TestArtifactsSourceBoundariesDoNotRegisterMetadata(t *testing.T) {
	f := newArtifactsFixture(t, 4)
	artifactsMust(t, f.source.WriteFile("input", []byte("large"), 0600))
	for _, tc := range []struct {
		name string
		want error
	}{{"../input", artifactfs.ErrPath}, {"input", artifactfs.ErrTooLarge}} {
		if got, err := f.service.PublishArtifact(context.Background(), f.groupID, PublishArtifactInput{Name: "report"}, f.source, tc.name, "retry"); !errors.Is(err, tc.want) || got.ID != "" {
			t.Fatalf("source %q: %+v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	f.counts(t, 0, 0, 0)
	entries, err := os.ReadDir(filepath.Join(f.private.Name(), "blobs"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed copy left blobs/staging: %v, %v", entries, err)
	}
}

func TestArtifactsOpenChecksExactScopeBeforeBlobs(t *testing.T) {
	f := newArtifactsFixture(t, 64)
	base := f.publish(t, PublishArtifactInput{Name: "first"}, "first", "")
	neighbor := f.publish(t, PublishArtifactInput{Name: "neighbor"}, "neighbor", "")
	artifactsMust(t, f.blobs.Close())
	for _, scope := range [][3]string{
		{f.otherID, base.ArtifactID, base.ID},
		{f.groupID, neighbor.ArtifactID, base.ID},
		{f.groupID, base.ArtifactID, neighbor.ID},
		{f.groupID, base.ArtifactID, "missing"},
	} {
		got, err := f.service.OpenArtifactVersion(context.Background(), scope[0], scope[1], scope[2])
		artifactsNoReader(t, got, err, store.ErrNotFound)
	}
	got, err := f.service.OpenArtifactVersion(context.Background(), f.groupID, base.ArtifactID, base.ID)
	artifactsNoReader(t, got, err, artifactfs.ErrClosed)
}

func TestArtifactsOpenRejectsMissingOrCorruptBytes(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%t", corrupt), func(t *testing.T) {
			f := newArtifactsFixture(t, 64)
			v := f.publish(t, PublishArtifactInput{Name: "report"}, "good", "publish")
			name := filepath.Join("blobs", v.BlobHash)
			artifactsMust(t, f.private.Remove(name))
			want := os.ErrNotExist
			if corrupt {
				artifactsMust(t, f.private.WriteFile(name, []byte("evil"), 0600))
				want = artifactfs.ErrCorrupt
			}
			got, err := f.service.OpenArtifactVersion(context.Background(), f.groupID, v.ArtifactID, v.ID)
			artifactsNoReader(t, got, err, want)
			stored, err := f.db.GetArtifactVersion(context.Background(), f.groupID, v.ArtifactID, v.ID)
			if err != nil || !reflect.DeepEqual(stored, v) {
				t.Fatalf("unavailable bytes repaired/removed metadata: %+v, %v", stored, err)
			}
			f.counts(t, 1, 1, 1)
		})
	}
}

func TestArtifactsDBFailureLeavesVerifiedOrphanAndReusableKey(t *testing.T) {
	f := newArtifactsFixture(t, 64)
	_, err := f.raw.Exec(`CREATE TRIGGER artifacts_service_fail_version BEFORE INSERT ON artifact_versions
 BEGIN SELECT RAISE(ABORT, 'injected artifact version failure'); END`)
	artifactsMust(t, err)
	data := "safe immutable orphan"
	artifactsMust(t, f.source.WriteFile("input", []byte(data), 0600))
	input := PublishArtifactInput{Name: "report"}
	got, err := f.service.PublishArtifact(context.Background(), f.groupID, input, f.source, "input", "retry")
	if err == nil || !strings.Contains(err.Error(), "injected artifact version failure") || got.ID != "" {
		t.Fatalf("injected insertion failure: %+v, %v", got, err)
	}
	f.counts(t, 0, 0, 0)
	reader, err := f.blobs.Open(artifactsDigest(data))
	artifactsMust(t, err)
	artifactsRead(t, reader, data)
	info, err := f.private.Stat(filepath.Join("blobs", artifactsDigest(data)))
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatalf("orphan is not immutable: %v, %v", info, err)
	}
	_, err = f.raw.Exec("DROP TRIGGER artifacts_service_fail_version")
	artifactsMust(t, err)
	v := f.publish(t, input, data, "retry")
	f.content(t, v, data)
	f.counts(t, 1, 1, 1)
}

func TestArtifactsResourcesAndReadersRemainCallerOwned(t *testing.T) {
	f := newArtifactsFixture(t, 64)
	v := f.publish(t, PublishArtifactInput{Name: "report"}, "bytes", "")
	got, err := f.service.OpenArtifactVersion(context.Background(), f.groupID, v.ArtifactID, v.ID)
	artifactsMust(t, err)
	if got.Artifact.Name != "report" {
		got.Reader.Close()
		t.Fatalf("content lost stable artifact name: %+v", got.Artifact)
	}
	_, err = f.db.GetGroup(context.Background(), f.groupID)
	artifactsMust(t, err)
	reader, err := f.blobs.Open(v.BlobHash)
	artifactsMust(t, err)
	artifactsRead(t, reader, "bytes")
	_, err = f.source.Stat("input")
	artifactsMust(t, err)
	// Only the caller closes dependencies; its previously returned reader survives.
	artifactsMust(t, f.db.Close())
	artifactsMust(t, f.blobs.Close())
	artifactsRead(t, got.Reader, "bytes")
	_, err = f.source.Stat("input")
	artifactsMust(t, err)
	_, err = f.private.Stat("blobs")
	artifactsMust(t, err)
}
