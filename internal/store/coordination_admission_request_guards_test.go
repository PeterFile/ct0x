package store

import (
	"errors"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
)

func TestRequestWorkLinkGuardMatrix(t *testing.T) {
	for _, guard := range []string{"missing", "foreign", "revision", "active-attempt", "submitted", "accepted", "current-submission-only", "coordinated", "orphaned-mandate", "historical-mandate"} {
		t.Run(guard, func(t *testing.T) {
			s, _, thread, request := requestWorkFixture(t)
			task := createTestTask(t, s, thread.GroupID)
			expected := ErrConflict
			switch guard {
			case "missing":
				task.ID, expected = "missing", ErrNotFound
			case "foreign":
				task, expected = createTestTask(t, s, group(t, s).ID), ErrNotFound
			case "revision":
				task.Revision++
			case "active-attempt", "submitted", "accepted", "current-submission-only":
				attempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(task))
				if err != nil {
					t.Fatal(err)
				}
				if guard != "active-attempt" {
					submission, err := s.SubmitTask(ctx, taskSubmissionInput(attempt))
					if err != nil {
						t.Fatal(err)
					}
					if guard == "accepted" {
						if _, err := s.ReviewTask(ctx, taskReviewInput(submission)); err != nil {
							t.Fatal(err)
						}
					} else if guard == "current-submission-only" {
						// Deliberately corrupt only the CHECK invariant to independently test both pointer guards.
						// All identity FKs stay enabled; no frozen schema or lifecycle code is changed.
						if _, err := s.db.Exec("PRAGMA ignore_check_constraints=ON"); err != nil {
							t.Fatal(err)
						}
						if _, err := s.db.Exec("UPDATE tasks SET status='open',active_attempt_id=NULL WHERE id=?", task.ID); err != nil {
							t.Fatal(err)
						}
						if _, err := s.db.Exec("PRAGMA ignore_check_constraints=OFF"); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "coordinated", "orphaned-mandate", "historical-mandate":
				prior := coordinationAdmissionSource(t, s, thread)
				rows := coordinationAdmissionRows(task, prior)
				if guard != "coordinated" {
					rows.current = nil
				}
				if err := coordinationAdmissionCommit(s, rows); err != nil {
					t.Fatal(err)
				}
				if guard == "historical-mandate" {
					attempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(task))
					if err != nil {
						t.Fatal(err)
					}
					submission, err := s.SubmitTask(ctx, taskSubmissionInput(attempt))
					if err != nil {
						t.Fatal(err)
					}
					review := taskReviewInput(submission)
					review.Outcome = domain.ReviewOutcomeRejected
					if _, err := s.ReviewTask(ctx, review); err != nil {
						t.Fatal(err)
					}
					task, err = s.GetTask(ctx, task.GroupID, task.ID)
					if err != nil || task.Revision != 2 || task.Status != domain.TaskStatusOpen {
						t.Fatalf("historical fixture: %+v, %v", task, err)
					}
				}
			}
			request = requestWorkLink(request, task)
			before := messageSequenceSnapshot(t, s)
			got, err := s.RequestWork(ctx, thread.GroupID, request, "retryable")
			requestWorkRequireZero(t, got, err, expected)
			messageSequencePreserved(t, s, before)
			// Every guard failure leaves the on-demand source and key available.
			request = requestWorkLink(request, createTestTask(t, s, thread.GroupID))
			if _, err := s.RequestWork(ctx, thread.GroupID, request, "retryable"); err != nil {
				t.Fatalf("failed guard burned key/source: %v", err)
			}
		})
	}
}

func TestRequestWorkLinkAuthoritativeSnapshotAndPayload(t *testing.T) {
	s, _, thread, request := requestWorkFixture(t)
	assignee := member(t, s, thread.GroupID)
	a, b := createTestTask(t, s, thread.GroupID), createTestTask(t, s, thread.GroupID)
	input := taskInput(thread.GroupID)
	input.Title, input.Description, input.Acceptance = "权威任务标题", "权威描述", "权威验收"
	input.AssigneeID, input.DependencyIDs = assignee.ID, []string{b.ID, a.ID, a.ID}
	task, err := s.CreateTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	request = requestWorkLink(request, task)
	receipt, err := s.RequestWork(ctx, thread.GroupID, request, "link")
	if err != nil || receipt.Decision.Kind != domain.RoutingDecisionLinkExisting || receipt.Task.ID != task.ID ||
		!reflect.DeepEqual(receipt.Mandate.Scope, domain.TaskScope{Objective: task.Title, Description: task.Description, Acceptance: task.Acceptance, AssigneeID: task.AssigneeID, DependencyIDs: task.DependencyIDs}) {
		t.Fatalf("link replaced authoritative task fields: %+v, %v", receipt, err)
	}
	if got, err := s.GetTask(ctx, thread.GroupID, task.ID); err != nil || !reflect.DeepEqual(got, task) {
		t.Fatalf("link changed task: %+v, %v", got, err)
	}
	before := messageSequenceSnapshot(t, s)
	for _, change := range []func(*domain.ExistingTaskRequest){
		func(r *domain.ExistingTaskRequest) { r.ExpectedRevision++ },
		func(r *domain.ExistingTaskRequest) { r.TaskID = a.ID },
	} {
		changed, target := request, *request.ExistingTask
		changed.ExistingTask = &target
		change(&target)
		got, err := s.RequestWork(ctx, thread.GroupID, changed, "link")
		requestWorkRequireZero(t, got, err, ErrConflict)
		messageSequencePreserved(t, s, before)
	}
	requestWorkCounts(t, s, 1, 3, 2, 1)
	// Admission does not require the assignee to remain active for an existing Task.
	if _, err := s.db.Exec("UPDATE members SET status='left' WHERE id=?", assignee.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RequestWork(ctx, thread.GroupID, request, "link"); err != nil || !reflect.DeepEqual(got, receipt) {
		t.Fatalf("replay revalidated live task eligibility: %+v, %v", got, err)
	}
}

func TestRequestWorkNewTaskValidationRollsBackSourceAndKey(t *testing.T) {
	for _, failure := range []string{"missing-assignee", "left-assignee", "foreign-assignee", "missing-dependency", "foreign-dependency", "late-dependency"} {
		t.Run(failure, func(t *testing.T) {
			s, _, thread, request := requestWorkFixture(t)
			other := group(t, s)
			var deps []string
			switch failure {
			case "missing-assignee":
				request.NewTask.AssigneeID = "missing"
			case "left-assignee":
				request.NewTask.AssigneeID = member(t, s, thread.GroupID).ID
				if _, err := s.db.Exec("UPDATE members SET status='left' WHERE id=?", request.NewTask.AssigneeID); err != nil {
					t.Fatal(err)
				}
			case "foreign-assignee":
				request.NewTask.AssigneeID = member(t, s, other.ID).ID
			case "missing-dependency":
				deps = []string{"missing"}
			case "foreign-dependency":
				deps = []string{createTestTask(t, s, other.ID).ID}
			case "late-dependency":
				deps = []string{createTestTask(t, s, thread.GroupID).ID, createTestTask(t, s, thread.GroupID).ID}
				if _, err := s.db.Exec(`CREATE TRIGGER request_work_bad_dependency BEFORE INSERT ON task_dependencies
				 WHEN (SELECT count(*) FROM task_dependencies WHERE task_id=NEW.task_id)=1
				 BEGIN SELECT RAISE(ABORT,'注入后续依赖错误'); END`); err != nil {
					t.Fatal(err)
				}
			}
			request.NewTask.DependencyIDs = deps
			before := messageSequenceSnapshot(t, s)
			got, err := s.RequestWork(ctx, thread.GroupID, request, "retryable")
			expected := ErrNotFound
			if failure == "late-dependency" {
				expected = nil
			}
			requestWorkRequireZero(t, got, err, expected)
			messageSequencePreserved(t, s, before)
			request.NewTask.AssigneeID, request.NewTask.DependencyIDs = "", nil
			if _, err := s.RequestWork(ctx, thread.GroupID, request, "retryable"); err != nil {
				t.Fatalf("task validation burned source/key: %v", err)
			}
		})
	}
}

func TestRequestWorkOrdinaryCreateTaskIsNotAuthorization(t *testing.T) {
	s, _, thread, request := requestWorkFixture(t)
	input := taskInput(thread.GroupID)
	input.IdempotencyKey = "same-key"
	task, err := s.CreateTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	requestWorkCounts(t, s, 0, 1, 0, 0)
	request = requestWorkLink(request, task)
	receipt, err := s.RequestWork(ctx, thread.GroupID, request, "same-key")
	if err != nil || receipt.Task.ID != task.ID {
		t.Fatalf("create_task key leaked into request_work namespace: %+v, %v", receipt, err)
	}
	if got, err := s.CreateTask(ctx, input); err != nil || !reflect.DeepEqual(got, task) {
		t.Fatalf("admission broke original CreateTask receipt: %+v, %v", got, err)
	}
	requestWorkCounts(t, s, 1, 1, 0, 1)
	input.ActorID, input.Title = "missing", "changed"
	if _, err := s.CreateTask(ctx, input); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateTask lost access-before-replay semantics: %v", err)
	}
}
