package platform

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// These trusted registry metadata fixtures do not publish or validate blob bytes.
func taskLifecyclePublishMetadata(t *testing.T, db *store.Store, input store.PublishArtifactInput) domain.ArtifactVersion {
	t.Helper()
	input.ActorID, input.BlobHash, input.Size = "user", strings.Repeat("a", 64), 42
	version, err := db.PublishArtifact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func TestTaskLifecycleLocalUserBindsActorAndForwardsImmutableEvidence(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	task, err := f.local.CreateTask(ctx, f.groupID, TaskInput{Title: "task", Acceptance: "criteria"}, "")
	if err != nil {
		t.Fatal(err)
	}
	activation := TaskAttemptInput{ExpectedRevision: 1, ExpectedActiveAttemptID: ""}
	attempt, err := f.local.ActivateTaskAttempt(ctx, f.groupID, task.ID, activation, "cycle")
	if err != nil || attempt.ID == "" || attempt.TaskID != task.ID || attempt.GroupID != f.groupID || attempt.TaskRevision != 1 || attempt.TurnID != "" {
		t.Fatalf("human attempt: %+v, %v", attempt, err)
	}
	source, err := f.local.CreateTask(ctx, f.groupID, TaskInput{Title: "source", Acceptance: "criteria"}, "")
	if err != nil {
		t.Fatal(err)
	}
	base := taskLifecyclePublishMetadata(t, f.db, store.PublishArtifactInput{GroupID: f.groupID, Name: "metadata fixture", SourceTaskID: source.ID})
	update := store.PublishArtifactInput{GroupID: f.groupID, ArtifactID: base.ArtifactID, ExpectedVersionID: base.ID, SourceTaskID: source.ID}
	applied := taskLifecyclePublishMetadata(t, f.db, update)
	candidate := taskLifecyclePublishMetadata(t, f.db, update)
	head, err := f.db.GetArtifact(ctx, f.groupID, base.ArtifactID)
	if err != nil || head.CurrentVersionID != applied.ID || !applied.Applied || candidate.Applied || base.SourceTaskID == task.ID || candidate.SourceTaskID != source.ID {
		t.Fatalf("historical/candidate evidence fixture: %+v, %+v, %v", head, candidate, err)
	}
	wantRefs := []domain.ArtifactVersionRef{{ArtifactID: base.ArtifactID, VersionID: base.ID}, {ArtifactID: base.ArtifactID, VersionID: candidate.ID}}
	slices.SortFunc(wantRefs, func(a, b domain.ArtifactVersionRef) int { return strings.Compare(a.VersionID, b.VersionID) })
	submit := TaskSubmissionInput{ExpectedRevision: 1, AttemptID: attempt.ID, Summary: " exact summary ", ArtifactVersions: []domain.ArtifactVersionRef{wantRefs[1], wantRefs[0], wantRefs[1]}}
	originalRefs := slices.Clone(submit.ArtifactVersions)
	submission, err := f.local.SubmitTask(ctx, f.groupID, task.ID, submit, "cycle")
	if err != nil || submission.SubmittedBy != "user" || submission.Summary != submit.Summary || submission.AttemptID != attempt.ID || !slices.Equal(submission.ArtifactVersions, wantRefs) || !slices.Equal(submit.ArtifactVersions, originalRefs) {
		t.Fatalf("human submission or input refs changed: %+v, %v", submission, err)
	}
	update.ExpectedVersionID = applied.ID
	later := taskLifecyclePublishMetadata(t, f.db, update)
	head, err = f.db.GetArtifact(ctx, f.groupID, base.ArtifactID)
	if err != nil || !later.Applied || head.CurrentVersionID != later.ID {
		t.Fatalf("head did not advance before review: %+v, %v", head, err)
	}
	review := TaskReviewInput{ExpectedRevision: 1, SubmissionID: submission.ID, Outcome: domain.ReviewOutcomeRejected, Reason: " exact reason "}
	decision, err := f.local.ReviewTask(ctx, f.groupID, task.ID, review, "cycle")
	if err != nil || decision.ReviewedBy != "user" || decision.Reason != review.Reason || decision.SubmissionID != submission.ID || decision.Outcome != review.Outcome || !slices.Equal(decision.ArtifactVersions, wantRefs) {
		t.Fatalf("human review: %+v, %v", decision, err)
	}
	// Store receipt replay proves exact forwarding and the fixed user/nil-scope identity.
	storedAttempt, err := f.db.ActivateTaskAttempt(ctx, store.ActivateTaskAttemptInput{
		GroupID: f.groupID, TaskID: task.ID, ActorID: "user", ExpectedRevision: 1, IdempotencyKey: "cycle",
	})
	if err != nil || !reflect.DeepEqual(storedAttempt, attempt) {
		t.Fatalf("attempt actor or fences changed: %+v, %v", storedAttempt, err)
	}
	storedSubmission, err := f.db.SubmitTask(ctx, store.SubmitTaskInput{
		GroupID: f.groupID, TaskID: task.ID, ActorID: "user", ExpectedRevision: 1,
		AttemptID: attempt.ID, Summary: submit.Summary, ArtifactVersions: submit.ArtifactVersions, IdempotencyKey: "cycle",
	})
	if err != nil || !reflect.DeepEqual(storedSubmission, submission) {
		t.Fatalf("submission actor or payload changed: %+v, %v", storedSubmission, err)
	}
	storedReview, err := f.db.ReviewTask(ctx, store.ReviewTaskInput{
		GroupID: f.groupID, TaskID: task.ID, ActorID: "user", ExpectedRevision: 1,
		SubmissionID: submission.ID, Outcome: review.Outcome, Reason: review.Reason, IdempotencyKey: "cycle",
	})
	if err != nil || !reflect.DeepEqual(storedReview, decision) {
		t.Fatalf("review actor or payload changed: %+v, %v", storedReview, err)
	}
	replayInput := submit
	replayInput.ArtifactVersions = []domain.ArtifactVersionRef{wantRefs[0], wantRefs[1], wantRefs[0]}
	if replay, err := f.local.SubmitTask(ctx, f.groupID, task.ID, replayInput, "cycle"); err != nil || !reflect.DeepEqual(replay, submission) {
		t.Fatalf("canonical replay after review/head change: %+v, %v", replay, err)
	}
	replayInput.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: later.ArtifactID, VersionID: later.ID}}
	if _, err := f.local.SubmitTask(ctx, f.groupID, task.ID, replayInput, "cycle"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed exact refs did not conflict: %v", err)
	}
	gotAttempt, err := f.local.GetTaskAttempt(ctx, f.groupID, task.ID, attempt.ID)
	if err != nil || !reflect.DeepEqual(gotAttempt, attempt) {
		t.Fatalf("attempt detail: %+v, %v", gotAttempt, err)
	}
	attempts, err := f.local.ListTaskAttempts(ctx, f.groupID, task.ID, store.ListOptions{Limit: 1})
	if err != nil || !reflect.DeepEqual(attempts.Items, []domain.TaskAttempt{attempt}) || attempts.NextCursor != nil {
		t.Fatalf("attempt list: %+v, %v", attempts, err)
	}
	gotSubmission, err := f.local.GetTaskSubmission(ctx, f.groupID, task.ID, submission.ID)
	if err != nil || !reflect.DeepEqual(gotSubmission, submission) {
		t.Fatalf("submission detail: %+v, %v", gotSubmission, err)
	}
	submissions, err := f.local.ListTaskSubmissions(ctx, f.groupID, task.ID, store.ListOptions{Limit: 1})
	if err != nil || !reflect.DeepEqual(submissions.Items, []domain.Submission{submission}) || submissions.NextCursor != nil {
		t.Fatalf("submission list: %+v, %v", submissions, err)
	}
	gotReview, err := f.local.GetTaskReview(ctx, f.groupID, task.ID, decision.ID)
	if err != nil || !reflect.DeepEqual(gotReview, decision) {
		t.Fatalf("review detail: %+v, %v", gotReview, err)
	}
	reviews, err := f.local.ListTaskReviews(ctx, f.groupID, task.ID, store.ListOptions{Limit: 1})
	if err != nil || !reflect.DeepEqual(reviews.Items, []domain.ReviewDecision{decision}) || reviews.NextCursor != nil {
		t.Fatalf("review list: %+v, %v", reviews, err)
	}
	for table, want := range map[string]int{"turns": 0, "deliveries": 0, "resource_claims": 0, "mutation_idempotency": 3} {
		var count int
		if err := f.raw.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count = %d, %v; want %d", table, count, err, want)
		}
	}
}

func TestTaskLifecycleLocalUserDelegatesErrorsAndArtifactRefs(t *testing.T) {
	f := setupLocalMessages(t)
	ctx := context.Background()
	task, err := f.local.CreateTask(ctx, f.groupID, TaskInput{Title: "task", Acceptance: "criteria"}, "")
	if err != nil {
		t.Fatal(err)
	}
	activation := TaskAttemptInput{ExpectedRevision: 1}
	attempt, err := f.local.ActivateTaskAttempt(ctx, f.groupID, task.ID, activation, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.local.ActivateTaskAttempt(ctx, f.groupID, task.ID, activation, "retry"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("empty active-attempt fence was ignored: %v", err)
	}
	activation.ExpectedActiveAttemptID = attempt.ID
	next, err := f.local.ActivateTaskAttempt(ctx, f.groupID, task.ID, activation, "retry")
	if err != nil || next.ID == attempt.ID {
		t.Fatalf("nonempty active-attempt fence was not forwarded: %+v, %v", next, err)
	}
	version := taskLifecyclePublishMetadata(t, f.db, store.PublishArtifactInput{GroupID: f.groupID, Name: "metadata fixture"})
	input := TaskSubmissionInput{ExpectedRevision: 1, AttemptID: next.ID, Summary: "text"}
	for _, test := range []struct {
		ref  domain.ArtifactVersionRef
		want error
	}{
		{domain.ArtifactVersionRef{ArtifactID: version.ArtifactID}, store.ErrInvalid},
		{domain.ArtifactVersionRef{ArtifactID: version.ArtifactID, VersionID: "missing"}, store.ErrNotFound},
	} {
		input.ArtifactVersions = []domain.ArtifactVersionRef{test.ref}
		if _, err := f.local.SubmitTask(ctx, f.groupID, task.ID, input, "refs"); !errors.Is(err, test.want) {
			t.Fatalf("ref %+v: %v, want %v", test.ref, err, test.want)
		}
		before, err := f.local.GetTask(ctx, f.groupID, task.ID)
		if err != nil || before.Status != domain.TaskStatusOpen || before.ActiveAttemptID != next.ID || before.CurrentSubmissionID != "" {
			t.Fatalf("invalid refs changed state: %+v, %v", before, err)
		}
	}
	for _, query := range []string{"SELECT count(*) FROM task_submissions", "SELECT count(*) FROM task_submission_artifacts", "SELECT count(*) FROM mutation_idempotency WHERE operation='submit_task'"} {
		var count int
		if err := f.raw.QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected refs persisted: %s = %d, %v", query, count, err)
		}
	}
	input.ArtifactVersions = []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.ID}}
	if submission, err := f.local.SubmitTask(ctx, f.groupID, task.ID, input, "refs"); err != nil || !slices.Equal(submission.ArtifactVersions, input.ArtifactVersions) {
		t.Fatalf("corrected refs poisoned receipt or were dropped: %+v, %v", submission, err)
	}
	input.Summary += "changed"
	if _, err := f.local.SubmitTask(ctx, f.groupID, task.ID, input, "refs"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed payload lost store conflict: %v", err)
	}
}
