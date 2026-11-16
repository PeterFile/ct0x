package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestDispatchExecutionSingleBoundedReadSnapshot(t *testing.T) {
	f, r, scope := coordinationDecideScope(t)
	coordinationDecideExec(t, f.s, "PRAGMA journal_mode=WAL")
	old, err := f.s.GetDispatchExecution(ctx, r.GroupID, r.TaskID, r.ID)
	if err != nil || old.Decision != nil || old.CoordinatorTurn == nil {
		t.Fatalf("initial snapshot: %+v, %v", old, err)
	}
	reader, observed := coordinationDecideWrapped(t, f.path, "", nil)
	observed.afterAuth = func() { coordinationDecideAssign(t, f, scope, "during-read") }
	got, err := reader.GetDispatchExecution(ctx, r.GroupID, r.TaskID, r.ID)
	if err != nil || !reflect.DeepEqual(old, got) {
		t.Fatalf("mixed request/decision snapshot: %+v, %v", got, err)
	}
	fresh, err := f.s.GetDispatchExecution(ctx, r.GroupID, r.TaskID, r.ID)
	if err != nil || fresh.Decision == nil || fresh.Assignment == nil || fresh.WorkerTurn != nil {
		t.Fatalf("writer did not commit: %+v, %v", fresh, err)
	}
	if observed.afterAuth != nil || len(observed.begins) != 1 || !observed.begins[0].ReadOnly || len(observed.queries) > 12 {
		t.Fatalf("read is not one bounded snapshot: begins=%+v queries=%d", observed.begins, len(observed.queries))
	}
	// The assigned path must also avoid loading an unrelated Task/dependency list.
	assignedReader, assignedObserved := coordinationDecideWrapped(t, f.path, "", nil)
	assigned, err := assignedReader.GetDispatchExecution(ctx, r.GroupID, r.TaskID, r.ID)
	if err != nil || !reflect.DeepEqual(assigned, fresh) {
		t.Fatalf("assigned snapshot: %+v, %v", assigned, err)
	}
	legacy, err := f.s.GetAssignment(ctx, r.GroupID, fresh.Assignment.ID)
	if err != nil || legacy.Task.ID != r.TaskID || legacy.Task.CreatedAt.IsZero() {
		t.Fatalf("existing GetAssignment lost its Task projection: %+v, %v", legacy, err)
	}
	for _, queries := range [][]string{observed.queries, assignedObserved.queries} {
		if len(queries) > 12 {
			t.Fatalf("unbounded query count: %d", len(queries))
		}
		for _, query := range queries {
			if strings.Contains(query, "ORDER BY") || strings.Contains(query, "task_dependencies") || strings.Contains(query, "resource_claims") || strings.Contains(query, "candidate_text") || strings.Contains(query, "claim_token") {
				t.Fatalf("unnecessary scan or private Turn columns: %s", query)
			}
		}
	}
}

func TestDispatchExecutionRejectsDamagedSafeEvidenceWithoutMutation(t *testing.T) {
	for _, receipt := range []string{`{`, `null`, `{}`, `{"outcome":"PRIVATE-secret"}`, `{"outcome":"unknown","terminal":"PRIVATE-secret"}`} {
		t.Run(receipt, func(t *testing.T) {
			f, r, scope := coordinationDecideScope(t)
			coordinationDecideExec(t, f.s, "UPDATE turns SET receipt_json=? WHERE id=?", receipt, scope.Member.Claim.TurnID)
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.GetDispatchExecution(ctx, r.GroupID, r.TaskID, r.ID)
			if !errors.Is(err, ErrInvalid) || !reflect.DeepEqual(got, domain.DispatchExecution{}) || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("damaged evidence was exposed or guessed: %+v, %v", got, err)
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestDispatchExecutionRejectsUnrelatedTurnAndBinding(t *testing.T) {
	for _, damage := range []string{"turn-member", "turn-conversation", "delivery-turn", "binding"} {
		t.Run(damage, func(t *testing.T) {
			f, r, scope := coordinationDecideScope(t)
			decision := coordinationDecideAssign(t, f, scope, "damage")
			worker := coordinationDecideWorker(t, f, r, decision, true)
			// Counterfactual corruption bypasses only named identity guards/FKs in this private DB.
			coordinationDecideExec(t, f.s, "UPDATE turns SET state='succeeded' WHERE id=?", worker.Member.Claim.TurnID)
			coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_role_turns_update")
			coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=OFF")
			switch damage {
			case "turn-member":
				coordinationDecideExec(t, f.s, "UPDATE turns SET member_id=? WHERE id=?", f.coord, worker.Member.Claim.TurnID)
			case "turn-conversation":
				coordinationDecideExec(t, f.s, "UPDATE turns SET conversation_id=? WHERE id=?", scope.Member.ConversationID, worker.Member.Claim.TurnID)
			case "delivery-turn":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_dispatch_deliveries_identity")
				coordinationDecideExec(t, f.s, "UPDATE deliveries SET turn_id=? WHERE id=?", scope.Member.Claim.TurnID, worker.Provenance.DeliveryID)
			case "binding":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_attempts_no_update")
				coordinationDecideExec(t, f.s, "UPDATE coordination_assignment_attempts SET turn_id=? WHERE assignment_id=?", scope.Member.Claim.TurnID, decision.Assignment.ID)
			}
			coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=ON")
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.GetDispatchExecution(ctx, r.GroupID, r.TaskID, r.ID)
			if err == nil || !reflect.DeepEqual(got, domain.DispatchExecution{}) {
				t.Fatalf("unrelated identity escaped: %+v, %v", got, err)
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestDispatchExecutionCoordinatorUnknownReceiptAndReadErrors(t *testing.T) {
	f, r, scope := coordinationDecideScope(t)
	claim := scope.Member.Claim
	if _, err := f.s.RequestTurnCancel(ctx, r.GroupID, claim.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, NativeSessionID: "native-lead", ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	before := coordinationDecideSnapshot(t, f.s)
	got, err := f.s.GetDispatchExecution(ctx, r.GroupID, r.TaskID, r.ID)
	if err != nil || got.CoordinatorTurn.State != "unknown" || !got.CoordinatorTurn.CancelRequested || got.CoordinatorTurn.Receipt.Outcome != "unknown" || got.CoordinatorTurn.AcceptedAt != nil {
		t.Fatalf("coordinator evidence was invented: %+v, %v", got, err)
	}
	for _, bad := range []string{"", " padded", "bad\n", "bad\x00"} {
		got, err := f.s.GetDispatchExecution(ctx, r.GroupID, bad, r.ID)
		if !errors.Is(err, ErrInvalid) || !reflect.DeepEqual(got, domain.DispatchExecution{}) {
			t.Fatalf("invalid exact scope: %+v, %v", got, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	got, err = f.s.GetDispatchExecution(canceled, r.GroupID, r.TaskID, r.ID)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, domain.DispatchExecution{}) {
		t.Fatalf("cancellation became evidence: %+v, %v", got, err)
	}
	coordinationDecidePreserved(t, f.s, before)
}
