package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestProposeTaskDecisionRequestTransactionBoundary(t *testing.T) {
	isolateDecisionFixtureRegistry(t)
	f := chatSetup(t, "user", false)
	installDecisionRequestFixture(t, f.s)
	scope := chatAuthorityWorker(t, f).Member
	input := decisionTaskInput()
	const key = "Proposal/Exact:1"
	const fence = "UPDATE turns SET claim_token=claim_token"
	var claims int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM resource_claims WHERE turn_id=?`, scope.Claim.TurnID).Scan(&claims); err != nil || claims == 0 {
		t.Fatalf("fixture lacks live owner claims: %d, %v", claims, err)
	}
	before := messageSequenceSnapshot(t, f.s)

	checkFailure := func(t *testing.T, got domain.DecisionRequest, err error, phase string, confirmed bool, cause error) {
		t.Helper()
		requestDispatchZero(t, got, err, nil)
		unknown := errors.Is(err, ErrTaskWorkMutationUnknown)
		if unknown == confirmed || !errors.Is(err, cause) || unknown && err.Error() != ErrTaskWorkMutationUnknown.Error() {
			t.Errorf("%s: unknown=%t, error=%q; want unknown=%t with cause %v", phase, unknown, err, !confirmed, cause)
		}
		var evidence *taskWorkTransactionError
		if !errors.As(err, &evidence) || evidence.Phase() != phase || evidence.RollbackConfirmed() != confirmed || evidence.CommitOutcomeUnknown() != (phase == "commit") {
			t.Errorf("%s: wrong transaction evidence: %+v", phase, evidence)
		}
		t.Logf("phase=%s unknown=%t category_or_cause_preserved=%t", phase, unknown, errors.Is(err, cause))
	}

	for _, tc := range []struct {
		phase, trigger string
		category       error
	}{
		{"scope", `CREATE TRIGGER proposal_test_fault BEFORE UPDATE OF claim_token ON turns
		 BEGIN SELECT RAISE(ABORT,'proposal scope fault'); END`, ErrMemberToolDenied},
		// Fail the receipt readback after the real question, version and key inserts.
		{"mutation", `CREATE TRIGGER proposal_test_fault AFTER INSERT ON mutation_idempotency
		 WHEN NEW.operation='task_decision_request' BEGIN UPDATE decision_requests SET withdrawn_at=created_at; END`, ErrConflict},
	} {
		t.Run(tc.phase+"-rollback", func(t *testing.T) {
			coordinationDecideExec(t, f.s, tc.trigger)
			t.Cleanup(func() { coordinationDecideExec(t, f.s, "DROP TRIGGER proposal_test_fault") })
			for _, fault := range []struct {
				name string
				ack  error
			}{
				{"confirmed", nil},
				{"lost-ack", errors.New("private rollback acknowledgment lost")},
			} {
				t.Run(fault.name, func(t *testing.T) {
					gateway, observed := assignmentBoundaryStore(t, f.path, fault.ack)
					got, err := gateway.ProposeTaskDecisionRequest(ctx, scope, input, key)
					if len(observed.events) != 1 {
						t.Fatalf("expected one SQLite transaction, got %d", len(observed.events))
					}
					event := observed.events[0]
					if !strings.HasPrefix(event.firstSQL, fence) || event.rollbackCalls != 1 || event.rollbackErr != nil {
						t.Fatalf("did not observe the first-write scope fence and real SQLite rollback: %+v", event)
					}
					checkFailure(t, got, err, tc.phase, fault.ack == nil, tc.category)
					if fault.ack != nil && !errors.Is(err, fault.ack) {
						t.Errorf("rollback acknowledgment error was discarded: %v", err)
					}
					messageSequencePreserved(t, f.s, before)
				})
			}
		})
	}

	t.Run("commit-ack", func(t *testing.T) {
		loseAck := func(label string, cause error) {
			t.Helper()
			t.Log(label)
			gateway, observed := coordinationDecideWrapped(t, f.path, "ack-error", cause)
			got, err := gateway.ProposeTaskDecisionRequest(ctx, scope, input, key)
			if len(observed.begins) != 1 || len(observed.queries) == 0 || !strings.HasPrefix(observed.queries[0], fence) {
				t.Fatal("expected one transaction beginning with the real scope fence")
			}
			checkFailure(t, got, err, "commit", false, cause)
			if err := gateway.Close(); err != nil {
				t.Fatal(err)
			}
		}
		loseAck("first proposal", errors.New("private commit acknowledgment lost"))

		// Read the durable receipt from a separate handle before attempting any replay.
		observer := queueHandle(t, f.path, false)
		page, err := observer.ListPendingDecisionRequests(ctx, ListOptions{})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("lost first acknowledgment did not leave one pending question: %+v, %v", page, err)
		}
		want := page.Items[0]
		if want.ID == "" || want.Version != 1 || want.AuthorTurnID != scope.Claim.TurnID || !reflect.DeepEqual(want.DecisionQuestion, input.DecisionQuestion) {
			t.Fatalf("wrong durable question: %+v", want)
		}
		var record string
		if err := observer.db.QueryRow(`SELECT record_id FROM mutation_idempotency WHERE (operation,actor_id,group_id,idempotency_key)=('task_decision_request',?,?,?)`,
			scope.MemberID, scope.Claim.GroupID, key).Scan(&record); err != nil || record != decisionVersionRecord(want.ID, want.Version) {
			t.Fatalf("original key did not bind the durable version: %q, %v", record, err)
		}
		committed := messageSequenceSnapshot(t, observer)
		for _, table := range []string{"decision_requests", "decision_request_versions", "mutation_idempotency"} {
			if len(committed[table]) != len(before[table])+1 {
				t.Fatalf("expected exactly one new %s row", table)
			}
			delete(before, table)
		}
		messageSequencePreserved(t, observer, before)

		loseAck("same-key replay", fmt.Errorf("private replay commit acknowledgment lost: %w", ErrConflict))
		if replay, err := observer.ProposeTaskDecisionRequest(ctx, scope, input, key); err != nil || !reflect.DeepEqual(replay, want) {
			t.Fatalf("same-key reconciliation did not return the original receipt: %+v, %v", replay, err)
		}
		changed := input
		changed.ExpectedVersion = want.Version
		changed.Question += "请说明原因。"
		got, err := observer.ProposeTaskDecisionRequest(ctx, scope, changed, key)
		checkFailure(t, got, err, "mutation", true, ErrConflict)
		messageSequencePreserved(t, observer, committed)
		t.Log("exact replay preserved the original question, version, key and live claims")
	})
}
