package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func workerArtifactPublish(t *testing.T, s *Store, role CoordinationToolScope, o WorkspaceOperation, key string) domain.ArtifactVersion {
	t.Helper()
	v, err := s.PublishWorkerArtifact(ctx, role, WorkerArtifactInput{OperationID: o.ID, Name: "真实工作成果"}, WorkerArtifactBlob{Hash: o.Input.AfterHash, Size: o.Input.AfterSize}, key)
	if err != nil || v.ID == "" {
		t.Fatalf("bound publication: %+v,%v", v, err)
	}
	return v
}
func workerArtifactValidate(t *testing.T, s *Store, role CoordinationToolScope, refs []domain.ArtifactVersionRef) error {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := memberToolScopeInTx(ctx, tx, role.Member); err != nil {
		return err
	}
	return workerArtifactPublicationsInTx(ctx, tx, SubmitTaskInput{GroupID: role.Provenance.GroupID, TaskID: role.Provenance.TaskID, ExpectedRevision: role.Provenance.TaskRevision, AttemptID: role.AttemptID, ActorID: role.Member.MemberID, MemberScope: &role.Member, Summary: "冻结成果", ArtifactVersions: refs})
}

func TestWorkerArtifactPublicationProvenanceReplayAndCAS(t *testing.T) {
	f, _, role := workspaceOperationFixture(t)
	o := workspaceOperationDone(t, f.s, role, "op")
	input := WorkerArtifactInput{OperationID: o.ID, Name: "真实工作成果"}
	blob := WorkerArtifactBlob{Hash: o.Input.AfterHash, Size: o.Input.AfterSize}
	if _, found, err := f.s.LookupWorkerArtifact(ctx, role, input, "pub"); err != nil || found {
		t.Fatalf("pre-copy lookup: %t,%v", found, err)
	}
	v := workerArtifactPublish(t, f.s, role, o, "pub")
	if v.AuthorID != f.worker || v.GroupID != f.groupID || v.SourceTaskID != o.TaskID || v.BlobHash != o.Input.AfterHash || v.Size != o.Input.AfterSize || !v.Applied {
		t.Fatalf("publication used untrusted identity/blob/source: %+v", v)
	}
	var linked string
	if err := f.s.db.QueryRow("SELECT operation_id FROM worker_artifact_publications WHERE version_id=?", v.ID).Scan(&linked); err != nil || linked != o.ID {
		t.Fatalf("missing operation→version audit: %q,%v", linked, err)
	}
	before := coordinationDecideSnapshot(t, f.s)
	if got, found, err := f.s.LookupWorkerArtifact(ctx, role, input, "pub"); err != nil || !found || got != v {
		t.Fatalf("publication pre-copy replay: %+v,%t,%v", got, found, err)
	}
	if got, err := f.s.PublishWorkerArtifact(ctx, role, input, blob, "pub"); err != nil || got != v {
		t.Fatalf("publication receipt replay: %+v,%v", got, err)
	}
	changed := input
	changed.Name = "不同成果"
	if _, _, err := f.s.LookupWorkerArtifact(ctx, role, changed, "pub"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed publication request: %v", err)
	}
	badBlob := blob
	badBlob.Hash = strings.Repeat("c", 64)
	if _, err := f.s.PublishWorkerArtifact(ctx, role, input, badBlob, "pub"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed trusted blob: %v", err)
	}
	coordinationDecidePreserved(t, f.s, before)
	update := WorkerArtifactInput{OperationID: o.ID, ArtifactID: v.ArtifactID, ExpectedVersionID: v.ID}
	advanced, err := f.s.PublishWorkerArtifact(ctx, role, update, blob, "advance")
	if err != nil || !advanced.Applied || advanced.ExpectedVersionID != v.ID {
		t.Fatalf("existing version CAS: %+v,%v", advanced, err)
	}
	stale, err := f.s.PublishWorkerArtifact(ctx, role, update, blob, "stale")
	if err != nil || stale.Applied || stale.ObservedVersionID != advanced.ID || stale.ExpectedVersionID != v.ID {
		t.Fatalf("stale base lost immutable candidate semantics: %+v,%v", stale, err)
	}
	if got, err := f.s.PublishWorkerArtifact(ctx, role, update, blob, "stale"); err != nil || got != stale {
		t.Fatalf("stale candidate replay: %+v,%v", got, err)
	}
	if err := workerArtifactValidate(t, f.s, role, []domain.ArtifactVersionRef{{ArtifactID: v.ArtifactID, VersionID: v.ID}, {ArtifactID: stale.ArtifactID, VersionID: stale.ID}}); err != nil {
		t.Fatalf("valid immutable exact-version provenance rejected: %v", err)
	}
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.worker}, 1, "later-policy"); err != nil {
		t.Fatal(err)
	}
	if got, found, err := f.s.LookupWorkerArtifact(ctx, role, input, "pub"); err != nil || !found || got != v {
		t.Fatalf("historical publication replay consulted new eligibility: %+v,%t,%v", got, found, err)
	}
	for _, q := range []string{"UPDATE worker_artifact_publications SET operation_id=operation_id", "DELETE FROM worker_artifact_publications", "INSERT OR REPLACE INTO worker_artifact_publications SELECT * FROM worker_artifact_publications"} {
		if _, err := f.s.db.Exec(q); err == nil {
			t.Fatalf("SQL changed publication proof: %s", q)
		}
	}
	dispatchAssertFK(t, f.s)
}

func TestWorkerArtifactRequiresOwnKnownSyncedOperationAndActualBlob(t *testing.T) {
	for _, outcome := range []string{"pending", "unknown", "applied_sync_error", "unchanged", "wrong-hash", "wrong-size", "bare-hash", "foreign-operation"} {
		t.Run(outcome, func(t *testing.T) {
			f, _, role := workspaceOperationFixture(t)
			o := workspaceOperationBegin(t, f.s, role, "op")
			result := WorkspaceOperationResult{Outcome: "done", ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize}
			switch outcome {
			case "unknown":
				result = WorkspaceOperationResult{Outcome: "unknown", ErrorCode: "io_unknown"}
			case "applied_sync_error":
				result.Outcome = outcome
				result.ErrorCode = "sync_failed"
			case "unchanged":
				result = WorkspaceOperationResult{Outcome: "unchanged", ObservedHash: o.Preimage.Hash, ObservedSize: o.Preimage.Size}
			}
			if outcome != "pending" {
				if _, err := f.s.FinishWorkspaceOperation(ctx, role, o.ID, result); err != nil {
					t.Fatal(err)
				}
			}
			input := WorkerArtifactInput{OperationID: o.ID, Name: "伪造候选"}
			blob := WorkerArtifactBlob{Hash: o.Input.AfterHash, Size: o.Input.AfterSize}
			switch outcome {
			case "wrong-hash":
				blob.Hash = strings.Repeat("c", 64)
			case "wrong-size":
				blob.Size++
			case "bare-hash":
				input.OperationID = blob.Hash
			case "foreign-operation":
				other, _, otherRole := workspaceOperationFixture(t)
				foreign := workspaceOperationDone(t, other.s, otherRole, "foreign")
				input.OperationID = foreign.ID
			}
			before := coordinationDecideSnapshot(t, f.s)
			if got, err := f.s.PublishWorkerArtifact(ctx, role, input, blob, "pub"); err == nil || got != (domain.ArtifactVersion{}) {
				t.Fatalf("unproven operation/blob published: %+v,%v", got, err)
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestWorkerArtifactPathDoesNotSkipLatestPendingIntent(t *testing.T) {
	f, _, role := workspaceOperationFixture(t)
	o := workspaceOperationDone(t, f.s, role, "op")
	input := WorkerArtifactInput{RelativePath: o.Input.RelativePath, Name: "按相对路径发布"}
	blob := WorkerArtifactBlob{Hash: o.Input.AfterHash, Size: o.Input.AfterSize}
	if _, err := f.s.PublishWorkerArtifact(ctx, role, input, blob, "by-path"); err != nil {
		t.Fatal(err)
	}
	workspaceOperationBegin(t, f.s, role, "newer")
	if _, err := f.s.PublishWorkerArtifact(ctx, role, input, blob, "blocked-path"); err == nil {
		t.Fatal("path silently selected earlier done while latest intent pending")
	}
	if _, err := f.s.PublishWorkerArtifact(ctx, role, WorkerArtifactInput{OperationID: o.ID, Name: "旧操作"}, blob, "blocked-id"); err == nil {
		t.Fatal("explicit ID bypassed unresolved Task/Turn")
	}
}

func TestWorkerArtifactSubmissionRejectsUnrelatedTrustedVersions(t *testing.T) {
	f, _, role := workspaceOperationFixture(t)
	o := workspaceOperationDone(t, f.s, role, "op")
	v := workerArtifactPublish(t, f.s, role, o, "pub")
	if err := workerArtifactValidate(t, f.s, role, []domain.ArtifactVersionRef{{ArtifactID: v.ArtifactID, VersionID: v.ID}}); err != nil {
		t.Fatal(err)
	}
	bare, err := f.s.PublishArtifact(ctx, PublishArtifactInput{GroupID: f.groupID, SourceTaskID: o.TaskID, Name: "可信human但不是worker操作", BlobHash: o.Input.AfterHash, Size: o.Input.AfterSize, ActorID: "user", IdempotencyKey: "human"})
	if err != nil {
		t.Fatal(err)
	}
	for _, refs := range [][]domain.ArtifactVersionRef{nil, {{ArtifactID: bare.ArtifactID, VersionID: bare.ID}}, {{ArtifactID: v.ArtifactID, VersionID: bare.ID}}, {{ArtifactID: bare.ArtifactID, VersionID: v.ID}}} {
		if err := workerArtifactValidate(t, f.s, role, refs); err == nil {
			t.Fatalf("unrelated trusted artifact/version became execution proof: %+v", refs)
		}
	}
	workspaceOperationBegin(t, f.s, role, "unfinished")
	if err := workerArtifactValidate(t, f.s, role, []domain.ArtifactVersionRef{{ArtifactID: v.ArtifactID, VersionID: v.ID}}); err == nil {
		t.Fatal("submission validator ignored unresolved workspace effect")
	}
}

func TestWorkerArtifactCrossGroupVersionAndCallerIsolation(t *testing.T) {
	f, _, role := workspaceOperationFixture(t)
	o := workspaceOperationDone(t, f.s, role, "op")
	other := group(t, f.s)
	foreign, err := f.s.PublishArtifact(ctx, PublishArtifactInput{GroupID: other.ID, ActorID: "user", Name: "外群成果", BlobHash: o.Input.AfterHash, Size: o.Input.AfterSize})
	if err != nil {
		t.Fatal(err)
	}
	before := coordinationDecideSnapshot(t, f.s)
	if _, err := f.s.PublishWorkerArtifact(ctx, role, WorkerArtifactInput{OperationID: o.ID, ArtifactID: foreign.ArtifactID, ExpectedVersionID: foreign.ID}, WorkerArtifactBlob{Hash: o.Input.AfterHash, Size: o.Input.AfterSize}, "foreign-version"); err == nil {
		t.Fatal("worker appended a foreign group's ArtifactVersion")
	}
	if err := workerArtifactValidate(t, f.s, role, []domain.ArtifactVersionRef{{ArtifactID: foreign.ArtifactID, VersionID: foreign.ID}}); err == nil {
		t.Fatal("foreign group's exact version became submission proof")
	}
	bad := role
	bad.Member.Claim.GroupID = other.ID
	if _, err := f.s.PublishWorkerArtifact(ctx, bad, WorkerArtifactInput{OperationID: o.ID, Name: "伪造身份"}, WorkerArtifactBlob{Hash: o.Input.AfterHash, Size: o.Input.AfterSize}, "foreign-caller"); !errors.Is(err, ErrMemberToolDenied) {
		t.Fatalf("cross-group scope bypassed first SQL: %v", err)
	}
	coordinationDecidePreserved(t, f.s, before)
}

func TestWorkerArtifactSQLFailureIsAtomic(t *testing.T) {
	for _, table := range []string{"artifacts", "artifact_versions", "worker_artifact_publications"} {
		t.Run(table, func(t *testing.T) {
			f, _, role := workspaceOperationFixture(t)
			o := workspaceOperationDone(t, f.s, role, "op")
			coordinationDecideExec(t, f.s, "CREATE TRIGGER f4_publish_fail BEFORE INSERT ON "+table+" BEGIN SELECT RAISE(ABORT,'injected F4 publish failure'); END")
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.PublishWorkerArtifact(ctx, role, WorkerArtifactInput{OperationID: o.ID, Name: "可安全遗留blob"}, WorkerArtifactBlob{Hash: o.Input.AfterHash, Size: o.Input.AfterSize}, "pub")
			if err == nil || !reflect.DeepEqual(got, domain.ArtifactVersion{}) {
				t.Fatalf("SQL failure claimed publication: %+v,%v", got, err)
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}
