package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func taskArtifactV6Fixture(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v6.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys=ON; CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY);` + strings.Join(migrations[:6], "\n") + `INSERT INTO schema_migrations VALUES(1),(2),(3),(4),(5),(6);`); err != nil {
		t.Fatal(err)
	}
	return &Store{db: db}, path
}

func TestTaskArtifactV6UpgradePreservesTextHistoryAndClaims(t *testing.T) {
	old, path := taskArtifactV6Fixture(t)
	g, err := old.CreateGroup(ctx, keyedGroup())
	if err != nil {
		t.Fatal(err)
	}
	m := member(t, old, g.ID)
	messageInput := MessageRequest{GroupID: g.ID, ActorID: "user", Content: "legacy input", IdempotencyKey: "message"}
	message, err := old.CreateMessage(ctx, messageInput, []string{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := old.ClaimTurn(ctx, ClaimTurnInput{GroupID: g.ID, DeliveryID: message.Deliveries[0].ID, ResourceKeys: []string{"legacy-held"}, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	claim := TurnClaim{GroupID: g.ID, TurnID: turn.ID, Token: turn.ClaimToken}
	if _, err := old.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); err != nil {
		t.Fatal(err)
	}
	beforeTurn := queueTurn(t, old, claim)
	beforeMessage, err := old.GetMessage(ctx, g.ID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	create := taskInput(g.ID)
	create.AssigneeID, create.IdempotencyKey = m.ID, "legacy-task"
	task, err := old.CreateTask(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	activate := taskAttemptInput(task)
	activate.IdempotencyKey = "legacy-attempt"
	attempt, err := old.ActivateTaskAttempt(ctx, activate)
	if err != nil {
		t.Fatal(err)
	}
	submit := taskSubmissionInput(attempt)
	submission, err := old.SubmitTask(ctx, submit)
	if err != nil {
		t.Fatal(err)
	}
	beforeTask, err := old.GetTask(ctx, g.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	reviewedTask := createTestTask(t, old, g.ID)
	reviewedAttempt, err := old.ActivateTaskAttempt(ctx, taskAttemptInput(reviewedTask))
	if err != nil {
		t.Fatal(err)
	}
	reviewedInput := taskSubmissionInput(reviewedAttempt)
	reviewedInput.IdempotencyKey = "reviewed-submit"
	reviewedSubmission, err := old.SubmitTask(ctx, reviewedInput)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the frozen v6 review representation without calling v7 readers on v6.
	taskSubmissionRejectFixture(t, old, reviewedSubmission)
	legacyReview := domain.ReviewDecision{ID: "fixture-review-" + reviewedSubmission.ID, SubmissionID: reviewedSubmission.ID,
		TaskID: reviewedTask.ID, GroupID: g.ID, TaskRevision: 1, Outcome: domain.ReviewOutcomeRejected,
		ReviewedBy: "user", CreatedAt: reviewedSubmission.CreatedAt.Add(time.Minute), ArtifactVersions: []domain.ArtifactVersionRef{}}
	legacyReviewInput := taskReviewInput(reviewedSubmission)
	legacyReviewInput.Outcome, legacyReviewInput.Reason, legacyReviewInput.IdempotencyKey = domain.ReviewOutcomeRejected, "", "legacy-review"
	payload := mutationPayload(g.ID, reviewedTask.ID, reviewedSubmission.ID, "1", "rejected", "")
	if _, err := old.db.Exec(`INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id)
		VALUES('review_task','user',?,?,?,?)`, g.ID, legacyReviewInput.IdempotencyKey, payload, legacyReview.ID); err != nil {
		t.Fatal(err)
	}
	beforeReviewedTask, err := old.GetTask(ctx, g.ID, reviewedTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	publish := artifactPublishInput(g.ID)
	publish.SourceTaskID, publish.IdempotencyKey = task.ID, "legacy-artifact"
	version := mustPublishArtifact(t, old, publish)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var latest, count, v7 int
	if err := s.db.QueryRow("SELECT max(version),count(*),count(CASE WHEN version=7 THEN 1 END) FROM schema_migrations").Scan(&latest, &count, &v7); err != nil || latest != len(migrations) || count != len(migrations) || v7 != 1 {
		t.Fatalf("v7 ledger: %d/%d, v7=%d, %v", latest, count, v7, err)
	}
	for _, check := range []struct {
		want any
		read func() (any, error)
	}{
		{g, func() (any, error) { return s.CreateGroup(ctx, keyedGroup()) }},
		{beforeMessage, func() (any, error) { return s.CreateMessage(ctx, messageInput, nil) }},
		{beforeTurn, func() (any, error) { return s.GetTurn(ctx, g.ID, turn.ID) }},
		{beforeTask, func() (any, error) { return s.CreateTask(ctx, create) }},
		{attempt, func() (any, error) { return s.ActivateTaskAttempt(ctx, activate) }},
		{submission, func() (any, error) { return s.SubmitTask(ctx, submit) }},
		{submission, func() (any, error) { return s.GetTaskSubmission(ctx, g.ID, task.ID, submission.ID) }},
		{beforeReviewedTask, func() (any, error) { return s.GetTask(ctx, g.ID, reviewedTask.ID) }},
		{reviewedSubmission, func() (any, error) { return s.SubmitTask(ctx, reviewedInput) }},
		{legacyReview, func() (any, error) { return s.GetTaskReview(ctx, g.ID, reviewedTask.ID, legacyReview.ID) }},
		{legacyReview, func() (any, error) { return s.ReviewTask(ctx, legacyReviewInput) }},
		{version, func() (any, error) { return s.PublishArtifact(ctx, publish) }},
	} {
		if got, err := check.read(); err != nil || !reflect.DeepEqual(got, check.want) {
			t.Fatalf("v7 changed legacy value: %+v, %v; want %+v", got, err, check.want)
		}
	}
	taskArtifactLateInsert(t, s, submission, version)
	taskArtifactLateInsert(t, s, reviewedSubmission, version)
	page, err := s.ListTaskSubmissions(ctx, g.ID, task.ID, ListOptions{})
	if err != nil || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], submission) {
		t.Fatalf("legacy text list: %+v, %v", page, err)
	}
	decision, err := s.ReviewTask(ctx, taskReviewInput(submission))
	if err != nil || decision.ArtifactVersions == nil || len(decision.ArtifactVersions) != 0 {
		t.Fatalf("legacy text review: %+v, %v", decision, err)
	}
	var held string
	if err := s.db.QueryRow("SELECT turn_id FROM resource_claims WHERE resource_key='legacy-held'").Scan(&held); err != nil || held != turn.ID {
		t.Fatalf("v7 released unknown claim: %q, %v", held, err)
	}
	taskArtifactIntegrity(t, s)
}

func TestTaskArtifactV7LateMigrationFailureIsAtomic(t *testing.T) {
	old, path := taskArtifactV6Fixture(t)
	g := group(t, old)
	task := createTestTask(t, old, g.ID)
	if _, err := old.db.Exec("CREATE TABLE task_reviews_group_task_created_id(collision TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(old.db); err == nil {
		t.Fatal("v7 late collision unexpectedly succeeded")
	}
	var latest, count int
	if err := old.db.QueryRow("SELECT max(version),count(*) FROM schema_migrations").Scan(&latest, &count); err != nil || latest != 6 || count != 6 {
		t.Fatalf("migration ledger partially committed: %d/%d, %v", latest, count, err)
	}
	if err := old.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'task_submission_artifacts%' OR name IN ('task_attempts_group_task_created_id','task_submissions_group_task_created_id')`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("v7 DDL partially committed: %d, %v", count, err)
	}
	if got, err := old.GetTask(ctx, g.ID, task.ID); err != nil || !reflect.DeepEqual(got, task) {
		t.Fatalf("failed migration changed history: %+v, %v", got, err)
	}
	if _, err := old.db.Exec("DROP TABLE task_reviews_group_task_created_id"); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v7 int
	if err := s.db.QueryRow("SELECT max(version),count(*),count(CASE WHEN version=7 THEN 1 END) FROM schema_migrations").Scan(&latest, &count, &v7); err != nil || latest != len(migrations) || count != len(migrations) || v7 != 1 {
		t.Fatalf("migration retry: %d/%d, v7=%d, %v", latest, count, v7, err)
	}
	taskArtifactIntegrity(t, s)
}
