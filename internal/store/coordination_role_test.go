package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func coordinationDecideScope(t *testing.T) (requestDispatchFixture, domain.DispatchRequest, CoordinationToolScope) {
	t.Helper()
	f, r, member := coordinationDecideFixture(t)
	view, err := f.s.GetDispatchRequest(ctx, r.GroupID, r.ID)
	if err != nil || view.State.Delivery == nil {
		t.Fatalf("request receipt: %+v, %v", view, err)
	}
	d := view.State.Delivery
	return f, r, CoordinationToolScope{Member: member, Provenance: CoordinationRoleProvenance{
		Kind: CoordinationRoleCoordinator, GroupID: r.GroupID, DeliveryID: d.DeliveryID, MessageID: d.MessageID,
		ConversationID: d.ConversationID, MemberID: r.CoordinatorMemberID, TopicID: r.TopicID, RequestID: r.ID, TaskID: r.TaskID, TaskRevision: r.TaskRevision}}
}

func coordinationDecideExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		t.Fatalf("fixture SQL failed: %v", err)
	}
}

func coordinationDecideSnapshot(t *testing.T, s *Store) map[string][][]any {
	t.Helper()
	snapshot := messageSequenceSnapshot(t, s)
	snapshot["sqlite_sequence"] = messageSequenceRows(t, s.db, "SELECT * FROM sqlite_sequence ORDER BY name")
	snapshot["schema_migrations"] = messageSequenceRows(t, s.db, "SELECT * FROM schema_migrations ORDER BY version")
	return snapshot
}

func coordinationDecidePreserved(t *testing.T, s *Store, before map[string][][]any) {
	t.Helper()
	if after := coordinationDecideSnapshot(t, s); !reflect.DeepEqual(before, after) {
		for table, rows := range before {
			if !reflect.DeepEqual(rows, after[table]) {
				t.Errorf("changed %s", table)
			}
		}
		t.Fatal("operation did not preserve the full typed snapshot/ledger/sequence/claims")
	}
}

func coordinationDecideAssign(t *testing.T, f requestDispatchFixture, scope CoordinationToolScope, key string) domain.DecisionReceipt {
	t.Helper()
	got, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: " 原始决定\x00\r\n "}, key)
	if err != nil {
		t.Fatalf("assign: %v, cause=%v", err, errors.Unwrap(err))
	}
	return got
}

// Current worker business fixtures use real B2 materialization and B3 binding.
func coordinationDecideWorker(t *testing.T, f requestDispatchFixture, r domain.DispatchRequest, receipt domain.DecisionReceipt, bind bool) CoordinationToolScope {
	t.Helper()
	materialized, err := f.s.MaterializeAssignment(ctx, receipt.Assignment.ID, 1, time.Now().UTC())
	if err != nil || materialized.Delivery == nil {
		t.Fatalf("worker materialization: %+v, %v", materialized, err)
	}
	d := materialized.Delivery
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: d.DeliveryID, MaxActive: 8, ResourceKeys: []string{"worker-resource"}})
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: r.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	scope := CoordinationToolScope{Member: member, Provenance: CoordinationRoleProvenance{Kind: CoordinationRoleWorker, GroupID: r.GroupID,
		DeliveryID: d.DeliveryID, MessageID: d.MessageID, ConversationID: d.ConversationID, MemberID: f.worker, TopicID: r.TopicID,
		RequestID: r.ID, TaskID: r.TaskID, TaskRevision: r.TaskRevision, AssignmentID: receipt.Assignment.ID}}
	if bind {
		binding, err := f.s.BindAssignmentAttempt(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		scope.AttemptID = binding.AttemptID
	}
	return scope
}

func coordinationDecideIdentity(t *testing.T, s *Store, scope CoordinationToolScope) (CoordinationRoleProvenance, error) {
	t.Helper()
	tx, err := s.beginMemberToolTx(ctx, scope.Member)
	if err != nil {
		return CoordinationRoleProvenance{}, err
	}
	defer tx.Rollback()
	if err := coordinationDecideLiveInTx(ctx, tx, scope.Member); err != nil {
		return CoordinationRoleProvenance{}, err
	}
	return coordinationExecutionProvenanceInTx(ctx, tx, scope.Member, scope.Provenance)
}

func TestCoordinationDecideRoleIdentityAndTrustedJSON(t *testing.T) {
	f, r, scope := coordinationDecideScope(t)
	if p, err := coordinationDecideIdentity(t, f.s, scope); err != nil || p != scope.Provenance {
		t.Fatalf("coordinator identity: %+v, %v", p, err)
	}
	for _, value := range []any{scope, scope.Provenance} {
		data, err := json.Marshal(value)
		if err != nil || string(data) != "{}" {
			t.Fatalf("identity serialized: %q, %v", data, err)
		}
	}
	var forged CoordinationToolScope
	if err := json.Unmarshal([]byte(`{"Member":{"MemberID":"forged"},"Provenance":{"Kind":"coordinator"},"AttemptID":"forged"}`), &forged); err != nil || forged != (CoordinationToolScope{}) {
		t.Fatalf("JSON selected identity: %+v, %v", forged, err)
	}
	value := reflect.ValueOf(scope.Provenance)
	for i := 0; i < value.NumField(); i++ {
		bad := scope
		field := reflect.ValueOf(&bad.Provenance).Elem().Field(i)
		if field.Kind() == reflect.String {
			field.SetString(field.String() + "-wrong")
		} else {
			field.SetInt(field.Int() + 1)
		}
		p, err := coordinationDecideIdentity(t, f.s, bad)
		requestDispatchZero(t, p, err, ErrMemberToolDenied)
	}
	receipt := coordinationDecideAssign(t, f, scope, "identity")
	worker := coordinationDecideWorker(t, f, r, receipt, false)
	if p, err := coordinationDecideIdentity(t, f.s, worker); err != nil || p != worker.Provenance {
		t.Fatalf("worker identity: %+v, %v", p, err)
	}
	got, err := f.s.DecideDispatch(ctx, worker, CoordinatorDecisionInput{Kind: "decline", Reason: "拒绝"}, "worker-command")
	requestDispatchZero(t, got, err, ErrMemberToolDenied)
}

func TestCoordinationDecideRoleScopeAndLifecycleDenials(t *testing.T) {
	f, _, scope := coordinationDecideScope(t)
	other := group(t, f.s).ID
	for name, change := range map[string]func(*CoordinationToolScope){
		"old-claim":    func(s *CoordinationToolScope) { s.Member.Claim.Token = "old-claim" },
		"cross-group":  func(s *CoordinationToolScope) { s.Member.Claim.GroupID = other },
		"member":       func(s *CoordinationToolScope) { s.Member.MemberID = f.worker },
		"conversation": func(s *CoordinationToolScope) { s.Member.ConversationID = "wrong" },
		"topic":        func(s *CoordinationToolScope) { s.Member.TopicID += " " },
		"attempt":      func(s *CoordinationToolScope) { s.AttemptID = "not-coordinator-authority" },
		"ordinary":     func(s *CoordinationToolScope) { s.Provenance.Kind = CoordinationRoleOrdinary },
		"zero-kind":    func(s *CoordinationToolScope) { s.Provenance.Kind = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := scope
			change(&bad)
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.DecideDispatch(ctx, bad, CoordinatorDecisionInput{Kind: "decline", Reason: "拒绝"}, "key")
			requestDispatchZero(t, got, err, ErrMemberToolDenied)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
	for _, state := range []string{"cancel_requested", "unknown", "succeeded"} {
		coordinationDecideExec(t, f.s, "UPDATE turns SET state=? WHERE id=?", state, scope.Member.Claim.TurnID)
		before := coordinationDecideSnapshot(t, f.s)
		got, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "decline", Reason: "拒绝"}, "key")
		requestDispatchZero(t, got, err, ErrMemberToolDenied)
		coordinationDecidePreserved(t, f.s, before)
	}
	coordinationDecideExec(t, f.s, "UPDATE turns SET state='starting',candidate_terminal='succeeded' WHERE id=?", scope.Member.Claim.TurnID)
	got, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "decline", Reason: "拒绝"}, "key")
	requestDispatchZero(t, got, err, ErrMemberToolDenied)
}

func TestCoordinationDecideRoleOrdinaryAndCounterfactualWorkerMismatch(t *testing.T) {
	f, r, scope := coordinationDecideScope(t)
	receipt := coordinationDecideAssign(t, f, scope, "assigned")
	other := member(t, f.s, r.GroupID).ID
	m, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: r.GroupID, TopicID: r.TopicID, ActorID: r.CoordinatorMemberID, Content: "普通聊天", RecipientIDs: []string{other}}, []string{other})
	if err != nil {
		t.Fatal(err)
	}
	d := m.Deliveries[0]
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: d.ID, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: r.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	bad := CoordinationToolScope{Member: bound, Provenance: CoordinationRoleProvenance{Kind: CoordinationRoleWorker, GroupID: r.GroupID, DeliveryID: d.ID,
		MessageID: m.ID, ConversationID: d.ConversationID, MemberID: other, TopicID: r.TopicID, RequestID: r.ID, TaskID: r.TaskID, TaskRevision: 1, AssignmentID: receipt.Assignment.ID}}
	p, err := coordinationDecideIdentity(t, f.s, bad)
	requestDispatchZero(t, p, err, ErrMemberToolDenied)
	// Counterfactual corruption: bypass only the intent UPDATE identity guard, not ordinary acceptance.
	coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_dispatch_update")
	stamp := time.Now().UTC().Format(timestampLayout)
	coordinationDecideExec(t, f.s, `UPDATE coordination_assignment_dispatch SET status='enqueued',generation=2,updated_at=?,message_id=?,delivery_id=?,conversation_id=?,delivered_member_id=?,materialized_from_generation=1,materialized_at=? WHERE assignment_id=?`, stamp, m.ID, d.ID, d.ConversationID, other, stamp, receipt.Assignment.ID)
	p, err = coordinationDecideIdentity(t, f.s, bad)
	requestDispatchZero(t, p, err, ErrMemberToolDenied)
}

func TestCoordinationDecideRoleCounterfactualDualAndBrokenBackpointer(t *testing.T) {
	for _, mode := range []string{"dual-broken-worker", "backpointer"} {
		t.Run(mode, func(t *testing.T) {
			f, r, scope := coordinationDecideScope(t)
			receipt := coordinationDecideAssign(t, f, scope, "assigned")
			// Counterfactual states bypass the named static identity guards to test defense in depth.
			if mode == "backpointer" {
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_dispatch_deliveries_identity")
				coordinationDecideExec(t, f.s, "UPDATE deliveries SET turn_id='' WHERE id=?", scope.Provenance.DeliveryID)
			} else {
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_dispatch_update")
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_receipt_source")
				stamp := time.Now().UTC().Format(timestampLayout)
				coordinationDecideExec(t, f.s, `UPDATE coordination_assignment_dispatch SET status='enqueued',generation=2,updated_at=?,message_id=?,delivery_id=?,conversation_id=?,delivered_member_id=?,materialized_from_generation=1,materialized_at=? WHERE assignment_id=?`, stamp, scope.Provenance.MessageID, scope.Provenance.DeliveryID, scope.Member.ConversationID, r.CoordinatorMemberID, stamp, receipt.Assignment.ID)
			}
			p, err := coordinationDecideIdentity(t, f.s, scope)
			requestDispatchZero(t, p, err, ErrMemberToolDenied)
		})
	}
}

func TestCoordinationDecideRoleImmediateLiveRecheckBeforeReplayOrConflict(t *testing.T) {
	for _, kind := range []string{"assign", "decline"} {
		for _, fault := range []string{"token", "cancel", "member"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				f, _, scope := coordinationDecideScope(t)
				input := CoordinatorDecisionInput{Kind: kind, Reason: "原始决定"}
				if kind == "assign" {
					input.WorkerMemberID = f.worker
				}
				original, err := f.s.DecideDispatch(ctx, scope, input, "replay")
				if err != nil {
					t.Fatalf("setup: %v, cause=%v", err, errors.Unwrap(err))
				}
				mutation := fmt.Sprintf("UPDATE turns SET claim_token='rotated' WHERE id='%s';", scope.Member.Claim.TurnID)
				if fault == "cancel" {
					mutation = fmt.Sprintf("UPDATE turns SET cancel_requested_at='%s' WHERE id='%s';", materializeAt.Format(timestampLayout), scope.Member.Claim.TurnID)
				}
				if fault == "member" {
					mutation = fmt.Sprintf("UPDATE members SET status='left' WHERE id='%s';", f.coord)
				}
				coordinationDecideExec(t, f.s, "CREATE TRIGGER decide_scope_fault AFTER UPDATE OF claim_token ON turns BEGIN "+mutation+" END")
				before := coordinationDecideSnapshot(t, f.s)
				for _, reason := range []string{input.Reason, "changed-payload"} {
					bad := input
					bad.Reason = reason
					got, err := f.s.DecideDispatch(ctx, scope, bad, "replay")
					requestDispatchZero(t, got, err, ErrMemberToolDenied)
					if strings.Contains(err.Error(), scope.Member.Claim.Token) || errors.Is(err, ErrConflict) {
						t.Fatal("scope failure disclosed a receipt/conflict/token")
					}
					coordinationDecidePreserved(t, f.s, before)
				}
				coordinationDecideExec(t, f.s, "DROP TRIGGER decide_scope_fault")
				got, err := f.s.DecideDispatch(ctx, scope, input, "replay")
				if err != nil || !reflect.DeepEqual(got, original) {
					t.Fatalf("original key no longer replays: %+v, %v", got, err)
				}
			})
		}
	}
}
