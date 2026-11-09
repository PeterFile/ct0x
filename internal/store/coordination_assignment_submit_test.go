package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func assignmentBoundWorker(t *testing.T) (requestDispatchFixture, domain.DispatchRequest, CoordinationToolScope) {
	t.Helper()
	f, r, decision, _ := assignmentPending(t)
	worker := coordinationDecideWorker(t, f, r, decision, false)
	binding, err := f.s.BindAssignmentAttempt(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	worker.AttemptID = binding.AttemptID
	return f, r, worker
}

func TestSubmitAssignmentScopeRollbackCertainty(t *testing.T) {
	assignmentScopeRollback(t, true)
}

func TestSubmitAssignmentPointerRestoredAfterWrite(t *testing.T) {
	for _, kind := range []string{"keyed", "unkeyed"} {
		t.Run(kind, func(t *testing.T) {
			f, r, worker := assignmentBoundWorker(t)
			version := mustPublishArtifact(t, f.s, artifactPublishInput(r.GroupID))
			input := AssignmentResultInput{Summary: "成果证据", ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.ID}}}
			coordinationDecideExec(t, f.s, fmt.Sprintf(`CREATE TRIGGER submit_pointer_restore AFTER UPDATE OF current_submission_id ON tasks
 WHEN NEW.id='%s' AND NEW.status='submitted'
 BEGIN UPDATE tasks SET status='open',current_submission_id=NULL WHERE id=NEW.id; END`, r.TaskID))
			key := ""
			if kind == "keyed" {
				key = "pointer-restore"
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.SubmitAssignmentResult(ctx, worker, input, key)
			assignmentTransactionEvidence(t, "Submit pointer restore "+kind, err)
			if err == nil || !reflect.DeepEqual(got, domain.Submission{}) {
				t.Errorf("Task pointer restoration falsely returned successful Submit: %+v, %v", got, err)
			}
			var changed int
			if err := f.s.db.QueryRow("SELECT changes()").Scan(&changed); err != nil || changed != 1 {
				t.Errorf("expected the outer write to report one changed row: %d, %v", changed, err)
			}
			t.Logf("outer SQLite changes()=%d despite AFTER trigger restoring open/null current submission", changed)
			assignmentReopenPreserved(t, f.s, f.path, before)
		})
	}
}

func TestSubmitAssignmentResultReplayAndLocalReview(t *testing.T) {
	f, r, worker := assignmentBoundWorker(t)
	p := worker.Provenance
	view, err := f.s.GetAssignment(ctx, r.GroupID, p.AssignmentID)
	if err != nil || view.Binding == nil || view.Assignment.ID != p.AssignmentID || view.Request.ID != p.RequestID ||
		view.Assignment.TaskID != p.TaskID || view.Assignment.WorkerMemberID != p.MemberID || view.Dispatch.Delivery == nil {
		t.Fatalf("real source/binding projection: %+v, %v", view, err)
	}
	d := view.Dispatch.Delivery
	if view.Dispatch.Status != domain.AssignmentDispatchStatusEnqueued || d.DeliveryID != p.DeliveryID || d.MessageID != p.MessageID ||
		d.ConversationID != p.ConversationID || d.WorkerMemberID != p.MemberID || view.Binding.TurnID != worker.Member.Claim.TurnID ||
		view.Binding.AttemptID != worker.AttemptID || view.Binding.TaskRevision != p.TaskRevision {
		t.Fatalf("receipt/delivery/Turn/Attempt identities disagree: %+v", view)
	}
	turn, err := f.s.GetTurn(ctx, p.GroupID, worker.Member.Claim.TurnID)
	if err != nil || turn.DeliveryID != p.DeliveryID || turn.MemberID != p.MemberID || turn.ConversationID != p.ConversationID {
		t.Fatalf("real claimed worker Turn identity: %+v, %v", turn, err)
	}
	var backpointer, sender, topic string
	if err := f.s.db.QueryRow(`SELECT d.turn_id,m.sender_id,d.topic_id FROM deliveries d JOIN messages m ON m.id=d.message_id
 WHERE d.id=? AND d.message_id=? AND d.group_id=? AND d.recipient_id=? AND d.conversation_id=?`, p.DeliveryID, p.MessageID, p.GroupID, p.MemberID, p.ConversationID).Scan(&backpointer, &sender, &topic); err != nil ||
		backpointer != turn.ID || sender != view.Decision.CoordinatorMemberID || topic != p.TopicID {
		t.Fatalf("actual materialized delivery/message/backpointer: %q/%q/%q, %v", backpointer, sender, topic, err)
	}
	t.Log("real SQLite source chain: MaterializeAssignment -> ClaimTurn -> BindMemberTool -> BindAssignmentAttempt; no hand-inserted receipt")
	input := AssignmentResultInput{Summary: "工作结果"}
	before := coordinationDecideSnapshot(t, f.s)
	submission, err := f.s.SubmitAssignmentResult(ctx, worker, input, "result")
	if err != nil || submission.GroupID != r.GroupID || submission.TaskID != r.TaskID || submission.TaskRevision != r.TaskRevision || submission.AttemptID != worker.AttemptID || submission.SubmittedBy != worker.Member.MemberID || submission.Summary != input.Summary {
		t.Fatalf("bound submission: %+v, %v", submission, err)
	}
	task, err := f.s.GetTask(ctx, r.GroupID, r.TaskID)
	if err != nil || task.Status != domain.TaskStatusSubmitted || task.CurrentSubmissionID != submission.ID || task.ActiveAttemptID != worker.AttemptID {
		t.Fatalf("submission is not acceptance: %+v, %v", task, err)
	}
	after := coordinationDecideSnapshot(t, f.s)
	if !reflect.DeepEqual(before["resource_claims"], after["resource_claims"]) {
		t.Fatal("submission released physical claims")
	}
	replayed, err := f.s.SubmitAssignmentResult(ctx, worker, input, "result")
	if err != nil || !reflect.DeepEqual(replayed, submission) {
		t.Fatalf("same-key replay: %+v, %v", replayed, err)
	}
	coordinationDecidePreserved(t, f.s, after)
	legacy := SubmitTaskInput{GroupID: r.GroupID, TaskID: r.TaskID, ExpectedRevision: r.TaskRevision, AttemptID: worker.AttemptID, ActorID: worker.Member.MemberID, MemberScope: &worker.Member, Summary: input.Summary, IdempotencyKey: "result"}
	if got, err := f.s.SubmitTask(ctx, legacy); err != nil || !reflect.DeepEqual(got, submission) {
		t.Fatalf("original submit_task namespace changed: %+v, %v", got, err)
	}
	review := taskReviewInput(submission)
	review.ActorID = worker.Member.MemberID
	gotReview, err := f.s.ReviewTask(ctx, review)
	requestDispatchZero(t, gotReview, err, ErrMemberToolDenied)
	review.ActorID = "user"
	if _, err := f.s.ReviewTask(ctx, review); err != nil {
		t.Fatal(err)
	}
	acceptedTask, err := f.s.GetTask(ctx, r.GroupID, r.TaskID)
	if err != nil || acceptedTask.Status != domain.TaskStatusAccepted || acceptedTask.ActiveAttemptID != "" || acceptedTask.CurrentSubmissionID != submission.ID {
		t.Fatalf("local-user Review did not establish acceptance: %+v, %v", acceptedTask, err)
	}
	t.Log("same real source chain: bound SubmitAssignmentResult yielded submitted, then local-user Review yielded accepted; fixture, not provider/runtime E2E")
	accepted := coordinationDecideSnapshot(t, f.s)
	if !reflect.DeepEqual(after["resource_claims"], accepted["resource_claims"]) {
		t.Fatal("review released a running Turn's claims")
	}
}

func TestSubmitAssignmentReplayBeforeCurrentEligibility(t *testing.T) {
	for _, change := range []string{"policy", "binding", "authority", "accepted", "rejected", "pointer"} {
		t.Run(change, func(t *testing.T) {
			f, r, worker := assignmentBoundWorker(t)
			input := AssignmentResultInput{Summary: "原始结果"}
			original, err := f.s.SubmitAssignmentResult(ctx, worker, input, "history")
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "policy":
				if _, err := f.s.SetDispatchPolicy(ctx, r.GroupID, nil, 1, "policy"); err != nil {
					t.Fatal(err)
				}
			case "binding":
				if _, err := f.s.SetCoordinator(ctx, r.GroupID, f.coord, 1, "binding"); err != nil {
					t.Fatal(err)
				}
			case "authority":
				coordinationDecideExec(t, f.s, "UPDATE task_coordination SET authority_generation=2 WHERE task_id=?", r.TaskID)
			case "accepted", "rejected", "pointer":
				review := taskReviewInput(original)
				if change != "accepted" {
					review.Outcome = domain.ReviewOutcomeRejected
				}
				if _, err := f.s.ReviewTask(ctx, review); err != nil {
					t.Fatal(err)
				}
				if change == "pointer" {
					task, err := f.s.GetTask(ctx, r.GroupID, r.TaskID)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := dispatchHistoricalAttempt(t, f.s, taskAttemptInput(task)); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.SubmitAssignmentResult(ctx, worker, input, "history")
			if err != nil || !reflect.DeepEqual(got, original) {
				t.Fatalf("current eligibility displaced historical Submit replay: %+v, %v", got, err)
			}
			coordinationDecidePreserved(t, f.s, before)
			got, err = f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: input.Summary + " changed"}, "history")
			requestDispatchZero(t, got, err, ErrConflict)
			coordinationDecidePreserved(t, f.s, before)
			if change == "rejected" {
				binding, err := f.s.BindAssignmentAttempt(ctx, worker)
				requestDispatchZero(t, binding, err, ErrConflict)
				task, err := f.s.GetTask(ctx, r.GroupID, r.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				attempt, err := f.s.ActivateTaskAttempt(ctx, taskAttemptInput(task))
				requestDispatchZero(t, attempt, err, ErrConflict)
			}
		})
	}
}

func TestSubmitAssignmentBindingScopeAndCrossTurn(t *testing.T) {
	f, r, worker := assignmentBoundWorker(t)
	input := AssignmentResultInput{Summary: "原始结果"}
	if _, err := f.s.SubmitAssignmentResult(ctx, worker, input, "history"); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"attempt-empty", "attempt-other", "provenance", "group", "old-token", "topic"} {
		bad := worker
		switch field {
		case "attempt-empty":
			bad.AttemptID = ""
		case "attempt-other":
			bad.AttemptID = "other-attempt"
		case "provenance":
			bad.Provenance.AssignmentID = "other-assignment"
		case "group":
			bad.Member.Claim.GroupID = group(t, f.s).ID
		case "old-token":
			bad.Member.Claim.Token = "old-token"
		case "topic":
			bad.Member.TopicID += "wrong"
		}
		before := coordinationDecideSnapshot(t, f.s)
		got, err := f.s.SubmitAssignmentResult(ctx, bad, input, "history")
		requestDispatchZero(t, got, err, ErrMemberToolDenied)
		coordinationDecidePreserved(t, f.s, before)
	}
	queueEvent(t, f.s, worker.Member.Claim, provider.AcceptedEvent, "", "")
	unknown := queueReceipt(provider.OutcomeUnknown)
	unknown.OutputComplete = false
	if _, err := f.s.ApplyTurnReceipt(ctx, worker.Member.Claim, unknown); err != nil {
		t.Fatal(err)
	}
	before := coordinationDecideSnapshot(t, f.s)
	got, err := f.s.SubmitAssignmentResult(ctx, worker, input, "history")
	requestDispatchZero(t, got, err, ErrMemberToolDenied)
	coordinationDecidePreserved(t, f.s, before)
	other := member(t, f.s, r.GroupID).ID
	d := queueDelivery(t, f.s, r.GroupID, other, r.TopicID)
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: d.ID, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	otherScope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: r.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	bad := worker
	bad.Member = otherScope
	got, err = f.s.SubmitAssignmentResult(ctx, bad, input, "history")
	requestDispatchZero(t, got, err, ErrMemberToolDenied)
}

func TestSubmitAssignmentSecondTurnCannotReplay(t *testing.T) {
	f, _, worker := assignmentBoundWorker(t)
	input := AssignmentResultInput{Summary: "原始结果"}
	if _, err := f.s.SubmitAssignmentResult(ctx, worker, input, "history"); err != nil {
		t.Fatal(err)
	}
	second := assignmentSecondWorkerTurn(t, f.s, worker)
	before := coordinationDecideSnapshot(t, f.s)
	got, err := f.s.SubmitAssignmentResult(ctx, second, input, "history")
	requestDispatchZero(t, got, err, ErrMemberToolDenied)
	coordinationDecidePreserved(t, f.s, before)
	dispatchAssertFK(t, f.s)
}

func TestSubmitAssignmentReplayLiveScopeRecheck(t *testing.T) {
	f, _, worker := assignmentBoundWorker(t)
	input := AssignmentResultInput{Summary: "原始结果"}
	if _, err := f.s.SubmitAssignmentResult(ctx, worker, input, "history"); err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, f.s, fmt.Sprintf(`CREATE TRIGGER live_submit_drift AFTER UPDATE OF claim_token ON turns
 WHEN NEW.id='%s' BEGIN UPDATE turns SET claim_token='rotated' WHERE id=NEW.id; END`, worker.Member.Claim.TurnID))
	before := coordinationDecideSnapshot(t, f.s)
	recorder := assignmentQueryStore(t, f.path)
	got, err := recorder.s.SubmitAssignmentResult(ctx, worker, input, "history")
	requestDispatchZero(t, got, err, ErrMemberToolDenied)
	coordinationDecidePreserved(t, f.s, before)
	if len(recorder.queries) == 0 || !strings.HasPrefix(recorder.queries[0].query, "UPDATE turns SET claim_token=claim_token") {
		t.Fatal("live claim fence was not the first SQL")
	}
	for _, q := range recorder.queries {
		if strings.Contains(q.query, "mutation_idempotency") {
			t.Fatal("scope revoked by the first SQL reached historical key lookup")
		}
	}
}

func TestSubmitAssignmentSourceInvalidatesReplay(t *testing.T) {
	for _, fault := range []string{"backpointer", "dual-role", "missing-table"} {
		t.Run(fault, func(t *testing.T) {
			f, r, decision, coordinator := assignmentPending(t)
			worker := coordinationDecideWorker(t, f, r, decision, true)
			input := AssignmentResultInput{Summary: "原始结果"}
			if _, err := f.s.SubmitAssignmentResult(ctx, worker, input, "history"); err != nil {
				t.Fatal(err)
			}
			// Counterfactual negative source damage only, never a capability fixture.
			switch fault {
			case "backpointer":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_dispatch_deliveries_identity")
				coordinationDecideExec(t, f.s, "UPDATE deliveries SET turn_id='' WHERE id=?", worker.Provenance.DeliveryID)
			case "dual-role":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_dispatch_update")
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_receipt_source")
				p := coordinator.Provenance
				coordinationDecideExec(t, f.s, `UPDATE coordination_assignment_dispatch SET delivery_id=?,message_id=?,conversation_id=?,delivered_member_id=? WHERE assignment_id=?`, p.DeliveryID, p.MessageID, p.ConversationID, p.MemberID, decision.Assignment.ID)
				coordinator.AttemptID = worker.AttemptID
				worker = coordinator
				worker.Provenance.Kind, worker.Provenance.AssignmentID = CoordinationRoleWorker, decision.Assignment.ID
			case "missing-table":
				coordinationDecideExec(t, f.s, "DROP TABLE coordination_assignment_attempts")
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.SubmitAssignmentResult(ctx, worker, input, "history")
			requestDispatchZero(t, got, err, nil)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestSubmitAssignmentSubmissionAtomicity(t *testing.T) {
	assignmentSubmitWritePoint(t, "task_submissions", "INSERT")
}

func TestSubmitAssignmentArtifactAtomicity(t *testing.T) {
	assignmentSubmitWritePoint(t, "task_submission_artifacts", "INSERT")
}

func TestSubmitAssignmentPointerAtomicity(t *testing.T) {
	assignmentSubmitWritePoint(t, "tasks", "UPDATE OF current_submission_id")
}

func TestSubmitAssignmentMutationReceiptAtomicity(t *testing.T) {
	assignmentSubmitWritePoint(t, "mutation_idempotency", "INSERT")
}

func assignmentSubmitWritePoint(t *testing.T, table, operation string) {
	t.Helper()
	for _, fault := range []string{"ignore", "abort", "token", "cancel", "receipt-missing", "receipt-payload", "receipt-result", "commit"} {
		t.Run(fault, func(t *testing.T) {
			f, _, worker := assignmentBoundWorker(t)
			version := mustPublishArtifact(t, f.s, artifactPublishInput(f.groupID))
			input := AssignmentResultInput{Summary: "成果证据", ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.ID}}}
			timing, body := "AFTER", "SELECT RAISE(ABORT,'injected submit fault');"
			switch fault {
			case "ignore":
				timing, body = "BEFORE", "SELECT RAISE(IGNORE);"
			case "token":
				body = fmt.Sprintf("UPDATE turns SET claim_token='rotated' WHERE id='%s';", worker.Member.Claim.TurnID)
			case "cancel":
				body = fmt.Sprintf("UPDATE turns SET cancel_requested_at='%s' WHERE id='%s';", time.Now().UTC().Format(timestampLayout), worker.Member.Claim.TurnID)
			case "receipt-missing", "receipt-payload", "receipt-result":
				condition := fmt.Sprintf(" WHERE operation='submit_task' AND actor_id='%s' AND group_id='%s' AND idempotency_key='result';", worker.Member.MemberID, f.groupID)
				body = "DELETE FROM mutation_idempotency" + condition
				if fault == "receipt-payload" {
					body = "UPDATE mutation_idempotency SET payload='changed-payload'" + condition
				} else if fault == "receipt-result" {
					body = "UPDATE mutation_idempotency SET record_id='changed-result'" + condition
				}
			case "commit":
				coordinationDecideExec(t, f.s, "CREATE TABLE submit_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)")
				body = "INSERT INTO submit_commit_guard VALUES('missing');"
			}
			coordinationDecideExec(t, f.s, "CREATE TRIGGER submit_fault "+timing+" "+operation+" ON "+table+" BEGIN "+body+" END")
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.SubmitAssignmentResult(ctx, worker, input, "result")
			requestDispatchZero(t, got, err, nil)
			var failure *assignmentTransactionError
			if !errors.As(err, &failure) || failure.CommitOutcomeUnknown() != (fault == "commit") {
				t.Fatalf("submission outcome certainty lost: %v", err)
			}
			coordinationDecidePreserved(t, f.s, before)
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := queueHandle(t, f.path, false)
			coordinationDecidePreserved(t, reopened, before)
			dispatchAssertFK(t, reopened)
		})
	}
}

func TestSubmitAssignmentCommitUnknown(t *testing.T) {
	for _, mode := range []string{"refuse", "ack-error"} {
		t.Run(mode, func(t *testing.T) {
			f, r, worker := assignmentBoundWorker(t)
			second := queueHandle(t, f.path, false)
			coordinationDecideExec(t, second, "PRAGMA busy_timeout=0")
			lock, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, busy := second.db.BeginTx(ctx, nil)
			if busy == nil {
				t.Fatal("expected actual SQLite BUSY cause")
			}
			if err := lock.Rollback(); err != nil {
				t.Fatal(err)
			}
			wrapped, _ := coordinationDecideWrapped(t, f.path, mode, busy)
			before := coordinationDecideSnapshot(t, f.s)
			got, err := wrapped.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: "结果"}, "unknown")
			requestDispatchZero(t, got, err, busy)
			var failure *assignmentTransactionError
			if !errors.As(err, &failure) || !failure.CommitOutcomeUnknown() || failure.RollbackConfirmed() || failure.Phase() != "commit" {
				t.Fatalf("Submit Commit uncertainty lost: %v", err)
			}
			task, err := second.GetTask(ctx, r.GroupID, r.TaskID)
			if err != nil || (task.Status == domain.TaskStatusSubmitted) != (mode == "ack-error") {
				t.Fatalf("trusted Submit reconciliation: %+v, %v", task, err)
			}
			after := coordinationDecideSnapshot(t, f.s)
			if !reflect.DeepEqual(before["resource_claims"], after["resource_claims"]) || !reflect.DeepEqual(before["coordination_assignment_attempts"], after["coordination_assignment_attempts"]) {
				t.Fatal("unknown Submit released claims or rebound work")
			}
		})
	}
}
