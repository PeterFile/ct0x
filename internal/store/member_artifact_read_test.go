package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

type memberArtifactReader interface {
	ReadArtifactForTool(context.Context, MemberToolScope, string, string) (domain.Artifact, domain.ArtifactVersion, error)
}

func memberArtifactAPI(t *testing.T, s *Store) memberArtifactReader {
	t.Helper()
	reader, ok := any(s).(memberArtifactReader)
	if !ok {
		t.Fatal("Store lacks member-scoped exact artifact/version reads")
	}
	return reader
}

func memberArtifactFixture(t *testing.T) (messageFixture, MemberToolScope, domain.Artifact, domain.ArtifactVersion) {
	t.Helper()
	f, scope := memberOperationFixture(t)
	a, v := seedArtifact(t, f.s, f.groupID, "artifact/opaque", "version/initial", "user")
	return f, scope, a, v
}

func memberArtifactRequireError(t *testing.T, a domain.Artifact, v domain.ArtifactVersion, err, want error) {
	t.Helper()
	if err == nil || want != nil && !errors.Is(err, want) || a != (domain.Artifact{}) || v != (domain.ArtifactVersion{}) {
		t.Fatalf("want zero metadata and %v; got %+v, %+v, %v", want, a, v, err)
	}
	if want == ErrMemberToolDenied {
		requireMemberToolDenied(t, err)
	}
}

func TestMemberArtifactExactMetadataAndReopen(t *testing.T) {
	f, scope, a, base := memberArtifactFixture(t)
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: "source-task-topic"}
	source := coordinationTriggerMessage(t, f.s, thread)
	admission, err := f.s.RequestWork(ctx, f.groupID, domain.RequestWorkRequest{
		TopicID: thread.TopicID, MessageID: source.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "制作共享报告",
		NewTask: &domain.NewTaskRequest{Title: "报告", Acceptance: "验证引用"},
	}, "artifact-source")
	if err != nil || admission.Mandate.TopicID == scope.TopicID {
		t.Fatalf("source task must belong to another topic in the same group: %+v, %v", admission, err)
	}
	head := base
	head.ID, head.AuthorID, head.SourceTaskID = "version/head", f.b, admission.Task.ID
	head.ExpectedVersionID, head.ObservedVersionID = base.ID, base.ID
	head.BlobHash, head.Size, head.CreatedAt = strings.Repeat("b", 64), 73, base.CreatedAt.Add(time.Second)
	if err := insertArtifactVersionRow(f.s.db, head); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE artifacts SET current_version_id=? WHERE id=?", head.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	a.CurrentVersionID = head.ID
	candidate := head
	candidate.ID, candidate.ObservedVersionID, candidate.Applied = "version/candidate", head.ID, false
	candidate.BlobHash, candidate.Size, candidate.CreatedAt = strings.Repeat("c", 64), 123, head.CreatedAt.Add(time.Second)
	if err := insertArtifactVersionRow(f.s.db, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE id=?", f.b); err != nil {
		t.Fatal(err)
	}
	counts := memberOperationCounts(t, f.s)
	check := func(s *Store) {
		t.Helper()
		reader := memberArtifactAPI(t, s)
		for _, want := range []domain.ArtifactVersion{base, head, candidate} {
			gotArtifact, gotVersion, err := reader.ReadArtifactForTool(ctx, scope, a.ID, want.ID)
			if err != nil || gotArtifact != a || gotVersion != want {
				t.Fatalf("exact durable metadata changed: %+v, %+v, %v; want %+v, %+v", gotArtifact, gotVersion, err, a, want)
			}
		}
		if task, err := s.GetTask(ctx, f.groupID, admission.Task.ID); err != nil || task.Status != domain.TaskStatusOpen || task.CurrentSubmissionID != "" {
			t.Fatalf("reading a publication submitted or accepted its source task: %+v, %v", task, err)
		}
		if memberOperationCounts(t, s) != counts {
			t.Fatal("metadata reads created members, messages, deliveries, receipts or turns")
		}
	}
	check(f.s) // A starting claim is authorized.
	queueEvent(t, f.s, scope.Claim, provider.AcceptedEvent, "", "")
	check(f.s) // A running claim is authorized too.
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	check(queueHandle(t, f.path, false))
}

func TestMemberArtifactExactTupleIsolation(t *testing.T) {
	f, scope, a, v := memberArtifactFixture(t)
	reader := memberArtifactAPI(t, f.s)
	other, otherVersion := seedArtifact(t, f.s, f.groupID, " artifact/另一个? ", " version/另一个? ", "user")
	foreign, foreignVersion := seedArtifact(t, f.s, f.otherID, "artifact/foreign", "version/foreign", "user")
	if gotArtifact, gotVersion, err := reader.ReadArtifactForTool(ctx, scope, other.ID, otherVersion.ID); err != nil || gotArtifact != other || gotVersion != otherVersion {
		t.Fatalf("opaque non-UUID IDs were restricted or normalized: %+v, %+v, %v", gotArtifact, gotVersion, err)
	}
	for _, tc := range []struct {
		name, artifactID, versionID string
		want                        error
	}{
		{"foreign-tuple", foreign.ID, foreignVersion.ID, ErrNotFound},
		{"foreign-version", a.ID, foreignVersion.ID, ErrNotFound},
		{"foreign-artifact", foreign.ID, v.ID, ErrNotFound},
		{"other-artifact-version", a.ID, otherVersion.ID, ErrNotFound},
		{"version-with-other-artifact", other.ID, v.ID, ErrNotFound},
		{"missing-artifact", "missing", v.ID, ErrNotFound},
		{"missing-version", a.ID, "missing", ErrNotFound},
		{"empty-version", a.ID, "", ErrInvalid},
		{"empty-artifact", "", v.ID, ErrInvalid},
		{"empty-tuple", "", "", ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, v, err := reader.ReadArtifactForTool(ctx, scope, tc.artifactID, tc.versionID)
			memberArtifactRequireError(t, a, v, err, tc.want)
		})
	}
}

func TestMemberArtifactInvalidScopeDeniedBeforeLookup(t *testing.T) {
	f, scope, a, v := memberArtifactFixture(t)
	reader := memberArtifactAPI(t, f.s)
	otherTopic, err := f.s.GetOrCreateConversation(ctx, f.groupID, f.a, "other-topic")
	if err != nil {
		t.Fatal(err)
	}
	otherMember, err := f.s.GetOrCreateConversation(ctx, f.groupID, f.b, scope.TopicID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		forge func(*MemberToolScope)
	}{
		{"zero-scope", func(s *MemberToolScope) { *s = MemberToolScope{} }},
		{"empty-token", func(s *MemberToolScope) { s.Claim.Token = "" }},
		{"wrong-token", func(s *MemberToolScope) { s.Claim.Token = "PRIVATE-TOKEN" }},
		{"empty-turn", func(s *MemberToolScope) { s.Claim.TurnID = "" }},
		{"missing-turn", func(s *MemberToolScope) { s.Claim.TurnID = "missing" }},
		{"empty-group", func(s *MemberToolScope) { s.Claim.GroupID = "" }},
		{"other-group", func(s *MemberToolScope) { s.Claim.GroupID = f.otherID }},
		{"empty-member", func(s *MemberToolScope) { s.MemberID = "" }},
		{"other-member", func(s *MemberToolScope) { s.MemberID = f.b }},
		{"empty-conversation", func(s *MemberToolScope) { s.ConversationID = "" }},
		{"other-conversation", func(s *MemberToolScope) { s.ConversationID = otherTopic.ID }},
		{"wrong-topic", func(s *MemberToolScope) { s.TopicID = otherTopic.TopicID }},
		{"empty-topic", func(s *MemberToolScope) { s.TopicID = "" }},
		{"other-topic-binding", func(s *MemberToolScope) { s.ConversationID, s.TopicID = otherTopic.ID, otherTopic.TopicID }},
		{"other-member-binding", func(s *MemberToolScope) { s.MemberID, s.ConversationID = f.b, otherMember.ID }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := scope
			tc.forge(&bad)
			for _, versionID := range []string{v.ID, "missing", ""} {
				a, v, err := reader.ReadArtifactForTool(ctx, bad, a.ID, versionID)
				memberArtifactRequireError(t, a, v, err, ErrMemberToolDenied)
			}
		})
	}
}

func TestMemberArtifactCommittedLifecycleInvalidatesPreflight(t *testing.T) {
	for _, transition := range []string{"cancel", "terminal", "recovery"} {
		t.Run(transition, func(t *testing.T) {
			f, scope, a, v := memberArtifactFixture(t)
			s := queueHandle(t, f.path, true)
			reader := memberArtifactAPI(t, s)
			if err := s.ValidateMemberTool(ctx, scope); err != nil {
				t.Fatal(err)
			}
			var err error
			switch transition {
			case "cancel":
				_, err = f.s.RequestTurnCancel(ctx, scope.Claim.GroupID, scope.Claim.TurnID)
			case "terminal":
				_, err = f.s.ApplyTurnReceipt(ctx, scope.Claim, queueReceipt(provider.OutcomeSucceeded))
			case "recovery":
				_, err = f.s.RecoverInFlight(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, versionID := range []string{v.ID, "missing", ""} {
				a, v, err := reader.ReadArtifactForTool(ctx, scope, a.ID, versionID)
				memberArtifactRequireError(t, a, v, err, ErrMemberToolDenied)
			}
		})
	}
}

func TestMemberArtifactFencePrecedesMetadataInSameTransaction(t *testing.T) {
	f, scope, a, base := memberArtifactFixture(t)
	reader := memberArtifactAPI(t, queueHandle(t, f.path, true))
	turnBefore := queueTurn(t, f.s, scope.Claim)
	// The requested row exists only after the operation's real scope fence runs.
	// A failed lookup must roll back both the row and the observed head change.
	if _, err := f.s.db.Exec(`CREATE TRIGGER member_artifact_fence AFTER UPDATE OF claim_token ON turns BEGIN
	 INSERT INTO artifact_versions(id,artifact_id,group_id,author_id,blob_hash,size,expected_version_id,observed_version_id,applied,created_at)
	 SELECT 'version/fenced',artifact_id,group_id,author_id,blob_hash,size+1,id,id,1,created_at
	 FROM artifact_versions WHERE id='version/initial';
	 UPDATE artifacts SET current_version_id='version/fenced' WHERE id='artifact/opaque'; END`); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	gotArtifact, gotVersion, err := reader.ReadArtifactForTool(bounded, scope, a.ID, "missing")
	memberArtifactRequireError(t, gotArtifact, gotVersion, err, ErrNotFound)
	if got, err := f.s.GetArtifact(ctx, f.groupID, a.ID); err != nil || got != a {
		t.Fatalf("failed read committed a separate preflight fence: %+v, %v", got, err)
	}
	if _, err := f.s.GetArtifactVersion(ctx, f.groupID, a.ID, "version/fenced"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed read committed the fence's version: %v", err)
	}
	want := base
	want.ID, want.Size, want.ExpectedVersionID, want.ObservedVersionID = "version/fenced", base.Size+1, base.ID, base.ID
	a.CurrentVersionID = want.ID
	gotArtifact, gotVersion, err = reader.ReadArtifactForTool(bounded, scope, a.ID, want.ID)
	if err != nil || gotArtifact != a || gotVersion != want {
		t.Fatalf("metadata was read before the fence or outside its transaction: %+v, %+v, %v", gotArtifact, gotVersion, err)
	}
	if queueTurn(t, f.s, scope.Claim) != turnBefore {
		t.Fatal("read changed the caller's turn")
	}
}

func TestMemberArtifactCommitFailureReturnsZero(t *testing.T) {
	f, scope, a, v := memberArtifactFixture(t)
	reader := memberArtifactAPI(t, f.s)
	if _, err := f.s.db.Exec(`CREATE TABLE member_artifact_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED);
	 CREATE TRIGGER member_artifact_commit_failure AFTER UPDATE OF claim_token ON turns BEGIN
	 INSERT INTO member_artifact_guard VALUES('PRIVATE-COMMIT-FAILURE'); END`); err != nil {
		t.Fatal(err)
	}
	a, v, err := reader.ReadArtifactForTool(ctx, scope, a.ID, v.ID)
	memberArtifactRequireError(t, a, v, err, nil)
	if strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("commit error leaked database details: %v", err)
	}
	var count int
	if err := f.s.db.QueryRow("SELECT count(*) FROM member_artifact_guard").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed commit left fence effects: %d, %v", count, err)
	}
}

func TestMemberArtifactMetadataScanFailuresReturnZero(t *testing.T) {
	f, scope, _, _ := memberArtifactFixture(t)
	reader := memberArtifactAPI(t, f.s)
	for _, invalid := range []string{"artifact", "version"} {
		t.Run(invalid, func(t *testing.T) {
			a, v := artifactValues(f.groupID, "artifact/bad-"+invalid, "version/bad-"+invalid, "user")
			artifactStamp, versionStamp := a.CreatedAt.Format(timestampLayout), v.CreatedAt.Format(timestampLayout)
			if invalid == "artifact" {
				artifactStamp = "PRIVATE-INVALID-TIMESTAMP"
			} else {
				versionStamp = "PRIVATE-INVALID-TIMESTAMP"
			}
			tx, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec(artifactInsertSQL, a.ID, a.GroupID, a.Name, a.CurrentVersionID, artifactStamp); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(artifactVersionInsertSQL, v.ID, v.ArtifactID, v.GroupID, v.SourceTaskID, v.AuthorID, v.BlobHash,
				v.Size, v.ExpectedVersionID, v.ObservedVersionID, v.Applied, versionStamp); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			a, v, err = reader.ReadArtifactForTool(ctx, scope, a.ID, v.ID)
			memberArtifactRequireError(t, a, v, err, nil)
			if strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("scan error leaked database details: %v", err)
			}
		})
	}
}

func TestMemberArtifactStorageAndContextFailuresReturnZero(t *testing.T) {
	f, scope, a, v := memberArtifactFixture(t)
	reader := memberArtifactAPI(t, f.s)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	gotArtifact, gotVersion, err := reader.ReadArtifactForTool(cancelled, scope, a.ID, v.ID)
	memberArtifactRequireError(t, gotArtifact, gotVersion, err, nil)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	gotArtifact, gotVersion, err = reader.ReadArtifactForTool(ctx, scope, a.ID, v.ID)
	memberArtifactRequireError(t, gotArtifact, gotVersion, err, nil)
}
