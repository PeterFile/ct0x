package store

import (
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/domain"
)

func taskSuccessorMigrationIndex(t *testing.T) int {
	t.Helper()
	index := slices.Index(migrations, taskSuccessorSchema)
	artifactRegistration := slices.Index(migrations, deliveryArtifactReadGrantSQL)
	if artifactRegistration < 0 || index <= artifactRegistration || slices.Contains(migrations[index+1:], taskSuccessorSchema) {
		t.Fatalf("want one task successor registration after the public artifact grant migration, got indices %d and %d", index, artifactRegistration)
	}
	return index
}

func TestMigrationsTaskSuccessorRegistration(t *testing.T) {
	taskSuccessorMigrationIndex(t)
}

func TestMigrationsTaskSuccessorUpgradePreservesAcceptedArtifact(t *testing.T) {
	index := taskSuccessorMigrationIndex(t)
	public := migrations
	t.Cleanup(func() { migrations = public })
	migrations = public[:index]

	before, path, original, _, input := taskSubmissionHuman(t)
	artifact := artifactPublishInput(original.GroupID)
	artifact.SourceTaskID = original.ID
	version := mustPublishArtifact(t, before, artifact)
	pin := domain.ArtifactVersionRef{ArtifactID: version.ArtifactID, VersionID: version.ID}
	input.ArtifactVersions = []domain.ArtifactVersionRef{pin}
	submission, err := before.SubmitTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	review, err := before.ReviewTask(ctx, taskReviewInput(submission))
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := before.GetTask(ctx, original.GroupID, original.ID)
	if err != nil || accepted.Status != domain.TaskStatusAccepted || accepted.CurrentSubmissionID != submission.ID {
		t.Fatalf("want recorded acceptance before the upgrade, got %+v, %v", accepted, err)
	}
	dependency := domain.TaskArtifactDependency{
		DependencyID: original.ID, SubmissionID: submission.ID, ArtifactID: version.ArtifactID,
		VersionID: version.ID, ReadyWhen: "accepted",
	}
	dependent := taskInput(original.GroupID)
	dependent.DependencyIDs = []string{original.ID}
	dependent.ArtifactDependencies = []domain.TaskArtifactDependency{dependency}
	consumer, err := before.CreateTask(ctx, dependent)
	if err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, before, index)
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	migrations = public
	after, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = after.Close() })
	messageSequenceLedger(t, after, len(public))

	gotTask, err := after.GetTask(ctx, original.GroupID, original.ID)
	if err != nil || !reflect.DeepEqual(gotTask, accepted) {
		t.Fatalf("accepted task changed on upgrade: %+v, %v", gotTask, err)
	}
	gotSubmission, err := after.GetTaskSubmission(ctx, original.GroupID, original.ID, submission.ID)
	if err != nil || !reflect.DeepEqual(gotSubmission, submission) {
		t.Fatalf("accepted submission changed on upgrade: %+v, %v", gotSubmission, err)
	}
	gotReview, err := after.GetTaskReview(ctx, original.GroupID, original.ID, review.ID)
	if err != nil || !reflect.DeepEqual(gotReview, review) {
		t.Fatalf("local-user review changed on upgrade: %+v, %v", gotReview, err)
	}
	pins, err := after.ListTaskArtifactDependencies(ctx, original.GroupID, consumer.ID, ListOptions{})
	if err != nil || !slices.Equal(pins.Items, []domain.TaskArtifactDependency{dependency}) {
		t.Fatalf("consumer pin changed on upgrade: %+v, %v", pins, err)
	}
	var successors int
	if err := after.db.QueryRowContext(ctx, "SELECT count(*) FROM task_successors").Scan(&successors); err != nil || successors != 0 {
		t.Fatalf("upgrade must install the relation without backfill: count=%d, err=%v", successors, err)
	}
	dispatchAssertFK(t, after)
	assertSQLiteQuickCheckOK(t, after)
}
