package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"modernc.org/sqlite"
)

func TestCoordinationDecideReadHistoryReopenAndIsolation(t *testing.T) {
	for _, kind := range []string{"assign", "decline"} {
		t.Run(kind, func(t *testing.T) {
			f, r, scope := coordinationDecideScope(t)
			pending, err := f.s.GetDispatchDecision(ctx, r.GroupID, r.ID)
			requestDispatchZero(t, pending, err, ErrNotFound)
			input := CoordinatorDecisionInput{Kind: kind, Reason: " 原始理由 "}
			if kind == "assign" {
				input.WorkerMemberID = f.worker
			}
			receipt, err := f.s.DecideDispatch(ctx, scope, input, "read-history")
			if err != nil {
				t.Fatalf("decide: %v, cause=%v", err, errors.Unwrap(err))
			}
			var want domain.AssignmentView
			if kind == "assign" {
				want, err = f.s.GetAssignment(ctx, r.GroupID, receipt.Assignment.ID)
				if err != nil || want.Assignment != *receipt.Assignment || want.Decision != receipt.Decision || want.Request != r || want.Binding != nil || want.Task.ID != r.TaskID || want.Dispatch.Generation != 1 || want.Dispatch.Status != "pending" || want.Dispatch.NextAttemptAt != receipt.Decision.CreatedAt || want.Dispatch.UpdatedAt != receipt.Decision.CreatedAt {
					t.Fatalf("exact initial view: %+v, %v", want, err)
				}
			}
			other := group(t, f.s).ID
			coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.worker)
			if _, err := f.s.SetDispatchPolicy(ctx, r.GroupID, nil, 1, "clear"); err != nil {
				t.Fatal(err)
			}
			coordinationDecideExec(t, f.s, "UPDATE tasks SET description='current projection' WHERE id=?", r.TaskID)
			coordinationDecideExec(t, f.s, "UPDATE turns SET state='unknown' WHERE id=?", scope.Member.Claim.TurnID)
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.GetDispatchDecision(ctx, r.GroupID, r.ID)
			if err != nil || !reflect.DeepEqual(got, receipt) {
				t.Fatalf("historical decision revalidated eligibility: %+v, %v", got, err)
			}
			for _, groupID := range []string{other, r.GroupID} {
				id := "missing"
				if groupID == other {
					id = r.ID
				}
				got, err := f.s.GetDispatchDecision(ctx, groupID, id)
				requestDispatchZero(t, got, err, ErrNotFound)
			}
			if kind == "assign" {
				got, err := f.s.GetAssignment(ctx, r.GroupID, receipt.Assignment.ID)
				want.Task.Description = "current projection"
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("current projection/history: %+v, %v", got, err)
				}
				missing, err := f.s.GetAssignment(ctx, other, receipt.Assignment.ID)
				requestDispatchZero(t, missing, err, ErrNotFound)
			} else {
				data, err := json.Marshal(receipt)
				if err != nil || !strings.Contains(string(data), `"assignment":null`) || receipt.Decision.WorkerMemberID != "" || receipt.Decision.AssignmentID != "" {
					t.Fatalf("decline null shape: %s, %v", data, err)
				}
			}
			coordinationDecidePreserved(t, f.s, before)
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := queueHandle(t, f.path, false)
			got, err = reopened.GetDispatchDecision(ctx, r.GroupID, r.ID)
			if err != nil || !reflect.DeepEqual(got, receipt) {
				t.Fatalf("reopen: %+v, %v", got, err)
			}
			coordinationDecidePreserved(t, reopened, before)
		})
	}
}

func TestCoordinationDecideReadBindingAndErrorBoundaries(t *testing.T) {
	f, r, scope := coordinationDecideScope(t)
	receipt := coordinationDecideAssign(t, f, scope, "binding")
	worker := coordinationDecideWorker(t, f, r, receipt, true)
	before := coordinationDecideSnapshot(t, f.s)
	view, err := f.s.GetAssignment(ctx, r.GroupID, receipt.Assignment.ID)
	if err != nil || view.Binding == nil || view.Binding.AttemptID != worker.AttemptID || view.Binding.TurnID != worker.Member.Claim.TurnID || view.Binding.TaskRevision != r.TaskRevision || view.Task.ActiveAttemptID != worker.AttemptID || view.Dispatch.Delivery == nil {
		t.Fatalf("binding exact projection: %+v, %v", view, err)
	}
	coordinationDecidePreserved(t, f.s, before)
	for _, bad := range []string{"", " bad", "bad\n", "bad\x00", string([]byte{0xff})} {
		got, err := f.s.GetAssignment(ctx, r.GroupID, bad)
		requestDispatchZero(t, got, err, ErrInvalid)
		gotDecision, err := f.s.GetDispatchDecision(ctx, bad, r.ID)
		requestDispatchZero(t, gotDecision, err, ErrInvalid)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err := f.s.GetAssignment(cancelled, r.GroupID, receipt.Assignment.ID)
	requestDispatchZero(t, got, err, context.Canceled)
	// Counterfactual corruption: only immutable Binding UPDATE is bypassed; no missing row is fabricated.
	coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_attempts_no_update")
	coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=OFF")
	coordinationDecideExec(t, f.s, "UPDATE coordination_assignment_attempts SET task_revision=task_revision+1 WHERE assignment_id=?", receipt.Assignment.ID)
	coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=ON")
	got, err = f.s.GetAssignment(ctx, r.GroupID, receipt.Assignment.ID)
	requestDispatchZero(t, got, err, ErrInvalid)
	coordinationDecideExec(t, f.s, "DROP TABLE coordination_assignment_attempts")
	got, err = f.s.GetAssignment(ctx, r.GroupID, receipt.Assignment.ID)
	requestDispatchZero(t, got, err, nil)
	var sqlCause *sqlite.Error
	if !errors.As(err, &sqlCause) || errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), "coordination_assignment_attempts") {
		t.Fatalf("SQL cause lost or exposed: %v / %T", err, errors.Unwrap(err))
	}
}

func TestCoordinationDecideReadSingleSnapshot(t *testing.T) {
	for _, kind := range []string{"assignment", "decision"} {
		t.Run(kind, func(t *testing.T) {
			f, r, scope := coordinationDecideScope(t)
			coordinationDecideExec(t, f.s, "PRAGMA journal_mode=WAL")
			reader, observed := coordinationDecideWrapped(t, f.path, "", nil)
			if kind == "assignment" {
				receipt := coordinationDecideAssign(t, f, scope, "snapshot")
				old, err := f.s.GetAssignment(ctx, r.GroupID, receipt.Assignment.ID)
				if err != nil {
					t.Fatal(err)
				}
				observed.afterAuth = func() { coordinationDecideWorker(t, f, r, receipt, true) }
				got, err := reader.GetAssignment(ctx, r.GroupID, receipt.Assignment.ID)
				if err != nil || !reflect.DeepEqual(got, old) {
					t.Fatalf("mixed read snapshot: %+v, %v", got, err)
				}
				fresh, err := f.s.GetAssignment(ctx, r.GroupID, receipt.Assignment.ID)
				if err != nil || fresh.Binding == nil || fresh.Task.ActiveAttemptID == "" || fresh.Dispatch.Status != "enqueued" {
					t.Fatalf("writer did not actually commit between reader queries: %+v, %v", fresh, err)
				}
			} else {
				var created domain.DecisionReceipt
				observed.afterAuth = func() { created = coordinationDecideAssign(t, f, scope, "snapshot") }
				got, err := reader.GetDispatchDecision(ctx, r.GroupID, r.ID)
				requestDispatchZero(t, got, err, ErrNotFound)
				fresh, err := f.s.GetDispatchDecision(ctx, r.GroupID, r.ID)
				if err != nil || !reflect.DeepEqual(fresh, created) {
					t.Fatalf("writer did not commit a decision: %+v, %v", fresh, err)
				}
			}
			if observed.afterAuth != nil || len(observed.begins) != 1 || !observed.begins[0].ReadOnly || len(observed.queries) < 2 || !strings.Contains(observed.queries[0], "SELECT 1 FROM groups g") {
				t.Fatalf("read did not use one trusted snapshot: begins=%+v queries=%d", observed.begins, len(observed.queries))
			}
		})
	}
}

func TestCoordinationDecideReadCounterfactualMandatoryAndStrictTime(t *testing.T) {
	for _, fault := range []string{"intent-missing", "task-time", "decision-time", "decision-turn", "receipt-member"} {
		t.Run(fault, func(t *testing.T) {
			f, r, scope := coordinationDecideScope(t)
			receipt := coordinationDecideAssign(t, f, scope, "corruption")
			// Counterfactual fixtures explicitly bypass named immutable guards/FKs/CHECKs only on this private DB.
			switch fault {
			case "intent-missing":
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=OFF")
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_dispatch_no_delete")
				coordinationDecideExec(t, f.s, "DELETE FROM coordination_assignment_dispatch WHERE assignment_id=?", receipt.Assignment.ID)
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=ON")
			case "task-time":
				coordinationDecideExec(t, f.s, "UPDATE tasks SET created_at='0000-01-01T00:00:00.000000000Z' WHERE id=?", r.TaskID)
			case "decision-time":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_decisions_no_update")
				coordinationDecideExec(t, f.s, "PRAGMA ignore_check_constraints=ON")
				coordinationDecideExec(t, f.s, "UPDATE coordination_decisions SET created_at='2026-02-30T00:00:00.000000000Z' WHERE id=?", receipt.Decision.ID)
				coordinationDecideExec(t, f.s, "PRAGMA ignore_check_constraints=OFF")
			case "decision-turn":
				coordinationDecideExec(t, f.s, "UPDATE turns SET state='succeeded' WHERE id=?", scope.Member.Claim.TurnID)
				d := queueDelivery(t, f.s, r.GroupID, f.coord, r.TopicID)
				turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: d.ID, MaxActive: 8})
				if err != nil {
					t.Fatal(err)
				}
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_decisions_no_update")
				coordinationDecideExec(t, f.s, "UPDATE coordination_decisions SET coordinator_turn_id=? WHERE id=?", turn.ID, receipt.Decision.ID)
			case "receipt-member":
				worker := coordinationDecideWorker(t, f, r, receipt, false)
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_dispatch_update")
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=OFF")
				coordinationDecideExec(t, f.s, "UPDATE coordination_assignment_dispatch SET delivered_member_id=? WHERE assignment_id=?", f.coord, receipt.Assignment.ID)
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=ON")
				_ = worker
			}
			got, err := f.s.GetAssignment(ctx, r.GroupID, receipt.Assignment.ID)
			requestDispatchZero(t, got, err, ErrInvalid)
			if fault == "decision-time" || fault == "decision-turn" {
				decision, err := f.s.GetDispatchDecision(ctx, r.GroupID, r.ID)
				requestDispatchZero(t, decision, err, ErrInvalid)
			}
		})
	}
}
