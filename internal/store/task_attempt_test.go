package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func taskAttemptInput(task domain.Task) ActivateTaskAttemptInput {
	return ActivateTaskAttemptInput{GroupID: task.GroupID, TaskID: task.ID, ActorID: "user", ExpectedRevision: task.Revision, ExpectedActiveAttemptID: task.ActiveAttemptID}
}

func taskAttemptCounts(t *testing.T, s *Store, attempts, receipts int) {
	t.Helper()
	for query, want := range map[string]int{
		"SELECT count(*) FROM task_attempts":                                                attempts,
		"SELECT count(*) FROM mutation_idempotency WHERE operation='activate_task_attempt'": receipts,
	} {
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: %d, %v; want %d", query, got, err, want)
		}
	}
}

// Seed only valid text evidence through the frozen v5 constraints, not future APIs.
func taskAttemptDependencyState(t *testing.T, s *Store, task domain.Task, state domain.TaskStatus) {
	t.Helper()
	current, err := s.GetTask(ctx, task.GroupID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	attemptID, subID, stamp := "fixture-attempt-"+task.ID, "fixture-sub-"+task.ID, task.CreatedAt.Format(timestampLayout)
	if current.Status == domain.TaskStatusOpen {
		if _, err := tx.Exec(`INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,created_at) VALUES(?,?,?,?,'user',?)`, attemptID, task.ID, task.GroupID, task.Revision, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO task_submissions(id,task_id,group_id,task_revision,attempt_id,summary,submitted_by,created_at) VALUES(?,?,?,?,?,'evidence','user',?)`, subID, task.ID, task.GroupID, task.Revision, attemptID, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`UPDATE tasks SET status='submitted',active_attempt_id=?,current_submission_id=? WHERE id=?`, attemptID, subID, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	if state == domain.TaskStatusAccepted {
		if _, err := tx.Exec(`INSERT INTO task_review_decisions(id,submission_id,task_id,group_id,task_revision,outcome,reviewed_by,created_at) VALUES(?,?,?,?,?,'accepted','user',?)`, "fixture-review-"+task.ID, subID, task.ID, task.GroupID, task.Revision, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`UPDATE tasks SET status='accepted',active_attempt_id=NULL WHERE id=?`, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskAttemptHumanPersistsWithoutTurn(t *testing.T) {
	s, path := openTestStore(t)
	g := group(t, s)
	m := member(t, s, g.ID)
	create := taskInput(g.ID)
	create.AssigneeID, create.IdempotencyKey = m.ID, "shared-key"
	task, err := s.CreateTask(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE members SET status='left' WHERE id=?", m.ID); err != nil {
		t.Fatal(err)
	}
	input := taskAttemptInput(task)
	input.IdempotencyKey = "shared-key"
	attempt, err := s.ActivateTaskAttempt(ctx, input)
	if err != nil || attempt.ID == "" || attempt.ID == task.ID || attempt.TaskID != task.ID || attempt.GroupID != task.GroupID || attempt.TaskRevision != 1 || attempt.TurnID != "" || attempt.CreatedAt.IsZero() || attempt.CreatedAt.Location() != time.UTC {
		t.Fatalf("human activation: %+v, %v", attempt, err)
	}
	check := func(s *Store) {
		t.Helper()
		got, err := s.GetTaskAttempt(ctx, g.ID, task.ID, attempt.ID)
		if err != nil || !reflect.DeepEqual(got, attempt) {
			t.Fatalf("historical attempt: %+v, %v", got, err)
		}
		got, err = s.ActivateTaskAttempt(ctx, input)
		if err != nil || !reflect.DeepEqual(got, attempt) {
			t.Fatalf("durable activation replay: %+v, %v", got, err)
		}
		current, err := s.GetTask(ctx, g.ID, task.ID)
		if err != nil || current.Status != domain.TaskStatusOpen || current.Revision != 1 || current.ActiveAttemptID != attempt.ID || current.CurrentSubmissionID != "" {
			t.Fatalf("activation changed task delivery state: %+v, %v", current, err)
		}
		var noTurn bool
		if err := s.db.QueryRow("SELECT turn_id IS NULL FROM task_attempts WHERE id=?", attempt.ID).Scan(&noTurn); err != nil || !noTurn {
			t.Fatalf("manufactured Turn: %v, %v", noTurn, err)
		}
		queueCounts(t, s, 0, 0, 0)
		taskAttemptCounts(t, s, 1, 1)
	}
	check(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
}

func TestTaskAttemptFencesPayloadAndActorScopes(t *testing.T) {
	f, _, claim := queueClaimFixture(t)
	scope, err := f.s.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	task := createTestTask(t, f.s, f.groupID)
	input := taskAttemptInput(task)
	input.IdempotencyKey = "key"
	first, err := f.s.ActivateTaskAttempt(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ActivateTaskAttemptInput){
		func(i *ActivateTaskAttemptInput) { i.TaskID = "" }, func(i *ActivateTaskAttemptInput) { i.ExpectedRevision = 0 },
		func(i *ActivateTaskAttemptInput) { i.ExpectedActiveAttemptID = "different" },
	} {
		bad := input
		change(&bad)
		if _, err := f.s.ActivateTaskAttempt(ctx, bad); !errors.Is(err, ErrConflict) {
			t.Errorf("changed keyed payload: %v", err)
		}
	}
	stale := input
	stale.IdempotencyKey = ""
	if _, err := f.s.ActivateTaskAttempt(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale active pointer: %v", err)
	}
	stale.ExpectedActiveAttemptID, stale.ExpectedRevision = first.ID, 2
	if _, err := f.s.ActivateTaskAttempt(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	input.ActorID, input.MemberScope, input.ExpectedActiveAttemptID = scope.MemberID, &scope, first.ID
	second, err := f.s.ActivateTaskAttempt(ctx, input)
	if err != nil || second.ID == first.ID || second.TurnID != claim.TurnID {
		t.Fatalf("actor scope collision: %+v, %v", second, err)
	}
	input = taskAttemptInput(createTestTask(t, f.s, f.otherID))
	input.IdempotencyKey = "key"
	if third, err := f.s.ActivateTaskAttempt(ctx, input); err != nil || third.ID == first.ID {
		t.Fatalf("group scope collision: %+v, %v", third, err)
	}
	taskAttemptCounts(t, f.s, 3, 3)
	if got, err := f.s.GetTaskAttempt(ctx, f.groupID, task.ID, first.ID); err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("superseded history was lost: %+v, %v", got, err)
	}
}

func TestTaskAttemptRequiresEveryDependencyAccepted(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	a, b := createTestTask(t, s, g.ID), createTestTask(t, s, g.ID)
	create := taskInput(g.ID)
	create.DependencyIDs = []string{a.ID, b.ID}
	task, err := s.CreateTask(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	input := taskAttemptInput(task)
	input.IdempotencyKey = "readiness"
	blocked := func() {
		t.Helper()
		if _, err := s.ActivateTaskAttempt(ctx, input); !errors.Is(err, ErrConflict) {
			t.Fatalf("unaccepted dependency unlocked work: %v", err)
		}
	}
	blocked()
	taskAttemptDependencyState(t, s, a, domain.TaskStatusSubmitted)
	blocked()
	taskAttemptDependencyState(t, s, a, domain.TaskStatusAccepted)
	blocked()
	taskAttemptDependencyState(t, s, b, domain.TaskStatusAccepted)
	if _, err := s.ActivateTaskAttempt(ctx, input); err != nil {
		t.Fatal(err)
	}
	taskAttemptCounts(t, s, 3, 1)
	for _, state := range []domain.TaskStatus{domain.TaskStatusSubmitted, domain.TaskStatusAccepted} {
		closed := createTestTask(t, s, g.ID)
		taskAttemptDependencyState(t, s, closed, state)
		if _, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(closed)); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s task activated: %v", state, err)
		}
	}
}

func TestTaskAttemptCASFailureRollsBackReservation(t *testing.T) {
	s, _ := openTestStore(t)
	g := group(t, s)
	task := createTestTask(t, s, g.ID)
	if _, err := s.db.Exec(`CREATE TRIGGER force_task_attempt_cas_miss AFTER INSERT ON task_attempts
BEGIN UPDATE tasks SET revision=revision+1 WHERE id=NEW.task_id; END`); err != nil {
		t.Fatal(err)
	}
	input := taskAttemptInput(task)
	input.IdempotencyKey = "retry"
	if got, err := s.ActivateTaskAttempt(ctx, input); !errors.Is(err, ErrConflict) || got.ID != "" {
		t.Fatalf("CAS miss was not detected: %+v, %v", got, err)
	}
	taskAttemptCounts(t, s, 0, 0)
	if current, err := s.GetTask(ctx, g.ID, task.ID); err != nil || !reflect.DeepEqual(current, task) {
		t.Fatalf("failed CAS changed task: %+v, %v", current, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER force_task_attempt_cas_miss"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateTaskAttempt(ctx, input); err != nil {
		t.Fatal(err)
	}
	taskAttemptCounts(t, s, 1, 1)
}

func TestTaskAttemptConcurrentFencesAndReplays(t *testing.T) {
	for _, agent := range []bool{false, true} {
		for _, keyed := range []bool{false, true} {
			t.Run(fmt.Sprintf("agent=%t/key=%t", agent, keyed), func(t *testing.T) {
				f, _, claim := queueClaimFixture(t)
				task := createTestTask(t, f.s, f.groupID)
				input := taskAttemptInput(task)
				if agent {
					scope, err := f.s.BindMemberTool(ctx, claim)
					if err != nil {
						t.Fatal(err)
					}
					input.ActorID, input.MemberScope = scope.MemberID, &scope
				}
				if keyed {
					input.IdempotencyKey = "race"
				}
				// Deferred agent handles prove its first statement acquires the write fence.
				handles := []*Store{queueHandle(t, f.path, agent), queueHandle(t, f.path, agent)}
				type result struct {
					attempt domain.TaskAttempt
					err     error
				}
				results, start := make(chan result, 2), make(chan struct{})
				for _, s := range handles {
					go func() { <-start; a, err := s.ActivateTaskAttempt(ctx, input); results <- result{a, err} }()
				}
				close(start)
				var winner domain.TaskAttempt
				success, conflicts := 0, 0
				for range 2 {
					r := <-results
					if errors.Is(r.err, ErrConflict) {
						conflicts++
						continue
					}
					if r.err != nil {
						t.Fatal(r.err)
					}
					if winner.ID != "" && !reflect.DeepEqual(winner, r.attempt) {
						t.Fatal("replays diverged")
					}
					winner = r.attempt
					success++
				}
				if (keyed && (success != 2 || conflicts != 0)) || (!keyed && (success != 1 || conflicts != 1)) {
					t.Fatalf("unexpected race outcomes: %d successes, %d conflicts", success, conflicts)
				}
				keys := 0
				if keyed {
					keys = 1
				}
				taskAttemptCounts(t, f.s, 1, keys)
			})
		}
	}
}
