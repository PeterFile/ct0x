package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

type requestDispatchAPI interface {
	RequestDispatch(context.Context, string, string, int64, int64, string, string) (domain.DispatchRequest, error)
	GetDispatchRequest(context.Context, string, string) (domain.DispatchView, error)
}

func requestDispatchStore(t *testing.T, s *Store) requestDispatchAPI {
	t.Helper()
	api, ok := any(s).(requestDispatchAPI)
	if !ok {
		t.Fatal("Store lacks RequestDispatch/GetDispatchRequest behavior")
	}
	return api
}

type requestDispatchFixture struct {
	s                            *Store
	path, groupID, worker, coord string
	task                         domain.Task
	admission                    domain.AdmissionReceipt
}

func requestDispatchSetup(t *testing.T) requestDispatchFixture {
	t.Helper()
	s, path, thread, input := requestWorkFixture(t)
	coord, worker := member(t, s, thread.GroupID).ID, member(t, s, thread.GroupID).ID
	if _, err := s.SetCoordinator(ctx, thread.GroupID, coord, 0, "config"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDispatchPolicy(ctx, thread.GroupID, []string{worker}, 0, "config"); err != nil {
		t.Fatal(err)
	}
	admission, err := s.RequestWork(ctx, thread.GroupID, input, "admission")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.GetTask(ctx, thread.GroupID, admission.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return requestDispatchFixture{s, path, thread.GroupID, worker, coord, task, admission}
}

func requestDispatchZero(t *testing.T, got any, err, want error) {
	t.Helper()
	if err == nil || want != nil && !errors.Is(err, want) || !reflect.ValueOf(got).IsZero() {
		t.Fatalf("want zero result and %v; got %+v, %v", want, got, err)
	}
}

func requestDispatchOnlyRows(t *testing.T, before, after map[string][][]any) {
	t.Helper()
	for table, rows := range before {
		switch table {
		case "coordination_dispatch_requests", "coordination_dispatch_state":
			if len(after[table]) != len(rows)+1 {
				t.Errorf("%s did not gain exactly one row", table)
			}
		case "mutation_idempotency":
			if len(after[table]) != len(rows)+1 {
				t.Error("expected exactly one outer mutation receipt")
			}
			var prior [][]any
			for _, row := range after[table] {
				if row[0] != "request_dispatch" {
					prior = append(prior, row)
				} else if row[1] != "user" {
					t.Errorf("request actor is not user: %v", row)
				}
			}
			if !reflect.DeepEqual(prior, rows) {
				t.Error("unrelated mutation receipts changed")
			}
		default:
			if !reflect.DeepEqual(after[table], rows) {
				t.Errorf("dispatch changed unrelated table %s", table)
			}
		}
	}
}

func requestDispatchSavedRows(t *testing.T, s *Store, id string) dispatchSQLRows {
	t.Helper()
	var result dispatchSQLRows
	for _, row := range messageSequenceTableRows(t, s, "coordination_dispatch_requests") {
		if row[0] == id {
			result.request = row
		}
	}
	for _, row := range messageSequenceTableRows(t, s, "coordination_dispatch_state") {
		if row[0] == id {
			result.state = row
		}
	}
	if result.request == nil || result.state == nil {
		t.Fatal("request/state pair missing")
	}
	return result
}

func TestRequestDispatchImmutableRequestGetAndReopen(t *testing.T) {
	f := requestDispatchSetup(t)
	api := requestDispatchStore(t, f.s)
	// Capture current versions, not the initial configuration revisions.
	if _, err := f.s.SetCoordinator(ctx, f.groupID, f.coord, 1, "binding-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.worker}, 1, "policy-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE task_coordination SET authority_generation=7 WHERE task_id=?", f.task.ID); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	reason := " \t请协调此任务。\x00\r\n "
	request, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 7, reason, "config")
	if err != nil {
		t.Fatal(err)
	}
	want := domain.DispatchRequest{
		ID: request.ID, GroupID: f.groupID, TopicID: f.admission.Mandate.TopicID, TaskID: f.task.ID,
		TaskRevision: 1, AuthorityGeneration: 7, MandateID: f.admission.Mandate.ID,
		CoordinatorMemberID: f.coord, BindingRevision: 2, PolicyRevision: 2,
		RequestedBy: "user", Reason: reason, CreatedAt: request.CreatedAt,
	}
	if request != want || !validTriggerID(request.ID) || request.ID == f.task.ID || request.ID == want.MandateID || request.CreatedAt.IsZero() || request.CreatedAt.Location() != time.UTC {
		t.Fatalf("incorrect immutable snapshots: %+v; want %+v", request, want)
	}
	requestDispatchOnlyRows(t, before, messageSequenceSnapshot(t, f.s))
	wantView := domain.DispatchView{Request: request, State: domain.DispatchState{
		RequestID: request.ID, Generation: 1, Status: domain.DispatchStatusPending, NextAttemptAt: request.CreatedAt,
	}}
	committed := messageSequenceSnapshot(t, f.s)
	if got, err := api.GetDispatchRequest(ctx, f.groupID, request.ID); err != nil || !reflect.DeepEqual(got, wantView) {
		t.Fatalf("exact pending read: %+v, %v", got, err)
	}
	if got, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 7, reason, "config"); err != nil || got != request {
		t.Fatalf("same-key replay: %+v, %v", got, err)
	}
	messageSequencePreserved(t, f.s, committed)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	api = requestDispatchStore(t, reopened)
	if got, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 7, reason, "config"); err != nil || got != request {
		t.Fatalf("reopen replay: %+v, %v", got, err)
	}
	if got, err := api.GetDispatchRequest(ctx, f.groupID, request.ID); err != nil || !reflect.DeepEqual(got, wantView) {
		t.Fatalf("reopen view: %+v, %v", got, err)
	}
	messageSequencePreserved(t, reopened, committed)
	dispatchAssertFK(t, reopened)
}

func TestRequestDispatchPreservesOpaqueTopicAndLocalUser(t *testing.T) {
	for _, topic := range []string{"", " \n\t ", "line\ntopic", "nul\x00topic", string([]byte{0xff})} {
		t.Run(fmt.Sprintf("%q", topic), func(t *testing.T) {
			f := requestDispatchSetup(t)
			message, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: topic, ActorID: f.worker, Content: "来源作者不是本地用户"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			admission, err := f.s.RequestWork(ctx, f.groupID, domain.RequestWorkRequest{
				TopicID: topic, MessageID: message.ID, Mode: domain.RoutingDecisionCreateNew,
				Reason: "明确授权", NewTask: &domain.NewTaskRequest{Title: "目标", Acceptance: "验收"},
			}, "opaque-admission")
			if err != nil {
				t.Fatal(err)
			}
			api := requestDispatchStore(t, f.s)
			request, err := api.RequestDispatch(ctx, f.groupID, admission.Task.ID, 1, 1, " 原始理由 ", "opaque")
			if err != nil || request.TopicID != topic || request.Reason != " 原始理由 " || request.RequestedBy != "user" {
				t.Fatalf("opaque bytes/user binding: %+v, %v", request, err)
			}
			before := messageSequenceSnapshot(t, f.s)
			if view, err := api.GetDispatchRequest(ctx, f.groupID, request.ID); err != nil || view.Request != request {
				t.Fatalf("read changed opaque request: %+v, %v", view, err)
			}
			messageSequencePreserved(t, f.s, before)
		})
	}
}

func TestRequestDispatchOriginalReceiptPrecedesCurrentEligibility(t *testing.T) {
	for _, change := range []string{"active-attempt", "submitted", "accepted", "rejected", "members-left", "policy-changed", "binding-changed", "authority-changed"} {
		t.Run(change, func(t *testing.T) {
			f := requestDispatchSetup(t)
			api := requestDispatchStore(t, f.s)
			original, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "original")
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "active-attempt", "submitted", "accepted", "rejected":
				attempt, err := f.s.ActivateTaskAttempt(ctx, taskAttemptInput(f.task))
				if err != nil {
					t.Fatal(err)
				}
				if change != "active-attempt" {
					submission, err := f.s.SubmitTask(ctx, taskSubmissionInput(attempt))
					if err != nil {
						t.Fatal(err)
					}
					if change != "submitted" {
						review := taskReviewInput(submission)
						if change == "rejected" {
							review.Outcome = domain.ReviewOutcomeRejected
						}
						if _, err := f.s.ReviewTask(ctx, review); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "members-left":
				_, err = f.s.db.Exec("UPDATE members SET status='left' WHERE group_id=?", f.groupID)
			case "policy-changed":
				_, err = f.s.SetDispatchPolicy(ctx, f.groupID, nil, 1, "clear")
			case "binding-changed":
				_, err = f.s.SetCoordinator(ctx, f.groupID, member(t, f.s, f.groupID).ID, 1, "replace")
			case "authority-changed":
				_, err = f.s.db.Exec("UPDATE task_coordination SET authority_generation=2 WHERE task_id=?", f.task.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, f.s)
			if got, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "original"); err != nil || got != original {
				t.Fatalf("historical receipt revalidated current eligibility: %+v, %v", got, err)
			}
			if got, err := api.GetDispatchRequest(ctx, f.groupID, original.ID); err != nil || got.Request != original || got.State.Status != domain.DispatchStatusPending {
				t.Fatalf("Get used current business eligibility: %+v, %v", got, err)
			}
			messageSequencePreserved(t, f.s, before)
		})
	}
}
