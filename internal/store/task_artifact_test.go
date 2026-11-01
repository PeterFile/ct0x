package store

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func taskArtifactNext(t *testing.T, s *Store, base domain.ArtifactVersion, expected string) domain.ArtifactVersion {
	t.Helper()
	input := artifactPublishInput(base.GroupID)
	input.ArtifactID, input.Name, input.ExpectedVersionID, input.SourceTaskID = base.ArtifactID, "", expected, base.SourceTaskID
	input.BlobHash = strings.Repeat("b", 64)
	return mustPublishArtifact(t, s, input)
}

func TestTaskArtifactConcreteEvidenceSurvivesHeadsReviewsAndReopen(t *testing.T) {
	s, path, task, _, input := taskSubmissionHuman(t)
	source := createTestTask(t, s, task.GroupID)
	publish := artifactPublishInput(task.GroupID)
	publish.SourceTaskID = source.ID
	base := mustPublishArtifact(t, s, publish)
	applied := taskArtifactNext(t, s, base, base.ID)
	candidate := taskArtifactNext(t, s, base, base.ID)
	other := mustPublishArtifact(t, s, artifactPublishInput(task.GroupID))
	if candidate.Applied || !applied.Applied || base.ID == base.BlobHash || base.SourceTaskID == task.ID {
		t.Fatal("fixture lacks historical/conflict/independent-task evidence")
	}
	input.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: base.ArtifactID, VersionID: candidate.ID}, {ArtifactID: other.ArtifactID, VersionID: other.ID}, {ArtifactID: base.ArtifactID, VersionID: base.ID}, {ArtifactID: base.ArtifactID, VersionID: candidate.ID}}
	original := slices.Clone(input.ArtifactVersions)
	first, err := s.SubmitTask(ctx, input)
	if err != nil || len(first.ArtifactVersions) != 3 || !slices.Equal(input.ArtifactVersions, original) {
		t.Fatalf("concrete canonical evidence: %+v, %v", first, err)
	}
	reordered := input
	reordered.ArtifactVersions = slices.Clone(first.ArtifactVersions)
	slices.Reverse(reordered.ArtifactVersions)
	if replay, err := s.SubmitTask(ctx, reordered); err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("canonical receipt changed: %+v, %v", replay, err)
	}
	before, err := s.GetTask(ctx, task.GroupID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	later := taskArtifactNext(t, s, base, applied.ID)
	if after, err := s.GetTask(ctx, task.GroupID, task.ID); err != nil || !reflect.DeepEqual(after, before) || after.Status != domain.TaskStatusSubmitted {
		t.Fatalf("head advancement accepted task: %+v, %v", after, err)
	}
	taskArtifactLateInsert(t, s, first, applied)
	reject := taskReviewInput(first)
	reject.Outcome, reject.IdempotencyKey = domain.ReviewOutcomeRejected, "reject"
	rejected, err := s.ReviewTask(ctx, reject)
	if err != nil || !slices.Equal(rejected.ArtifactVersions, first.ArtifactVersions) {
		t.Fatalf("rejection replaced authoritative refs: %+v, %v", rejected, err)
	}
	current, err := s.GetTask(ctx, task.GroupID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(current))
	if err != nil {
		t.Fatal(err)
	}
	taskArtifactLateInsert(t, s, first, applied)
	next := taskSubmissionInput(attempt)
	next.IdempotencyKey = "second"
	next.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: base.ArtifactID, VersionID: applied.ID}}
	second, err := s.SubmitTask(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	accept := taskReviewInput(second)
	accept.IdempotencyKey = "accept"
	accepted, err := s.ReviewTask(ctx, accept)
	if err != nil || !slices.Equal(accepted.ArtifactVersions, second.ArtifactVersions) {
		t.Fatalf("acceptance replaced evidence: %+v, %v", accepted, err)
	}
	final := taskArtifactNext(t, s, base, later.ID)
	taskArtifactLateInsert(t, s, second, final)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, want := range []domain.Submission{first, second} {
		if got, err := reopened.GetTaskSubmission(ctx, task.GroupID, task.ID, want.ID); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("stored refs followed head: %+v, %v", got, err)
		}
	}
	for _, want := range []domain.ReviewDecision{rejected, accepted} {
		if got, err := reopened.GetTaskReview(ctx, task.GroupID, task.ID, want.ID); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("decision refs drifted: %+v, %v", got, err)
		}
	}
	if replay, err := reopened.SubmitTask(ctx, input); err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("old submission receipt drifted: %+v, %v", replay, err)
	}
	if replay, err := reopened.ReviewTask(ctx, reject); err != nil || !reflect.DeepEqual(replay, rejected) {
		t.Fatalf("old decision receipt drifted: %+v, %v", replay, err)
	}
	if current, err := reopened.GetTask(ctx, task.GroupID, task.ID); err != nil || current.Status != domain.TaskStatusAccepted || current.Revision != 2 || current.CurrentSubmissionID != second.ID {
		t.Fatalf("head/replay changed accepted task: %+v, %v", current, err)
	}
	taskArtifactCounts(t, reopened, 2, 4, 2)
	queueCounts(t, reopened, 0, 0, 0)
}

func TestTaskArtifactInvalidReferencesRollBack(t *testing.T) {
	s, _, task, _, input := taskSubmissionHuman(t)
	local := mustPublishArtifact(t, s, artifactPublishInput(task.GroupID))
	other := mustPublishArtifact(t, s, artifactPublishInput(task.GroupID))
	foreignGroup := group(t, s)
	foreign := mustPublishArtifact(t, s, artifactPublishInput(foreignGroup.ID))
	for _, test := range []struct {
		ref  domain.ArtifactVersionRef
		want error
	}{
		{domain.ArtifactVersionRef{ArtifactID: local.ArtifactID}, ErrInvalid},
		{domain.ArtifactVersionRef{VersionID: local.ID}, ErrInvalid},
		{domain.ArtifactVersionRef{ArtifactID: "missing", VersionID: local.ID}, ErrNotFound},
		{domain.ArtifactVersionRef{ArtifactID: local.ArtifactID, VersionID: "missing"}, ErrNotFound},
		{domain.ArtifactVersionRef{ArtifactID: foreign.ArtifactID, VersionID: foreign.ID}, ErrNotFound},
		{domain.ArtifactVersionRef{ArtifactID: local.ArtifactID, VersionID: other.ID}, ErrNotFound},
		{domain.ArtifactVersionRef{ArtifactID: local.ArtifactID, VersionID: local.BlobHash}, ErrNotFound},
	} {
		bad := input
		bad.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: local.ArtifactID, VersionID: local.ID}, test.ref}
		if got, err := s.SubmitTask(ctx, bad); !errors.Is(err, test.want) || got.ID != "" {
			t.Fatalf("invalid concrete ref %+v: %+v, %v", test.ref, got, err)
		}
		taskArtifactCounts(t, s, 0, 0, 0)
	}
	input.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: local.ArtifactID, VersionID: local.ID}}
	if _, err := s.SubmitTask(ctx, input); err != nil {
		t.Fatal(err)
	}
	bad := input
	bad.ArtifactVersions = []domain.ArtifactVersionRef{{}}
	if _, err := s.SubmitTask(ctx, bad); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed invalid refs bypassed payload conflict: %v", err)
	}
}

func TestTaskArtifactMidTransactionFailureRollsBackEverything(t *testing.T) {
	s, _, task, _, input := taskSubmissionHuman(t)
	v := mustPublishArtifact(t, s, artifactPublishInput(task.GroupID))
	input.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: v.ArtifactID, VersionID: v.ID}}
	before, err := s.GetTask(ctx, task.GroupID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_after_task_submit AFTER UPDATE OF status ON tasks WHEN NEW.status='submitted'
 BEGIN SELECT RAISE(ABORT,'故意中断提交'); END`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SubmitTask(ctx, input); err == nil || got.ID != "" {
		t.Fatalf("failed transaction returned evidence: %+v, %v", got, err)
	}
	taskArtifactCounts(t, s, 0, 0, 0)
	if after, err := s.GetTask(ctx, task.GroupID, task.ID); err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("partial refs/task persisted: %+v, %v", after, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_after_task_submit"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitTask(ctx, input); err != nil {
		t.Fatal(err)
	}
	taskArtifactCounts(t, s, 1, 1, 1)
}

func TestTaskArtifactUnknownExecutionAndHeadAdvanceKeepResources(t *testing.T) {
	f, scope, task, _, input := taskSubmissionAgent(t)
	version := mustPublishArtifact(t, f.s, artifactPublishInput(task.GroupID))
	input.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.ID}}
	submission, err := f.s.SubmitTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); err != nil {
		t.Fatal(err)
	}
	before := queueTurn(t, f.s, scope.Claim)
	taskArtifactNext(t, f.s, version, version.ID)
	if current, err := f.s.GetTask(ctx, task.GroupID, task.ID); err != nil || current.Status != domain.TaskStatusSubmitted {
		t.Fatalf("publication accepted task: %+v, %v", current, err)
	}
	decision, err := f.s.ReviewTask(ctx, taskReviewInput(submission))
	if err != nil || !slices.Equal(decision.ArtifactVersions, submission.ArtifactVersions) {
		t.Fatalf("unknown-run evidence changed: %+v, %v", decision, err)
	}
	if after := queueTurn(t, f.s, scope.Claim); !reflect.DeepEqual(after, before) {
		t.Fatal("artifact/review reconciled unknown execution")
	}
	queueCounts(t, f.s, 1, 1, 1)
}
