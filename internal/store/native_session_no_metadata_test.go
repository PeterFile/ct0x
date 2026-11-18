package store

import (
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestNativeSessionNoMetadataBarrierAtomicAndNeverUpgraded(t *testing.T) {
	f := nativeFixture(t)
	turn, scope, binding := nativeClaim(t, f, f.groupID, f.a, "topic")
	before := queueTurn(t, f.s, scope.Claim)
	if _, err := f.s.db.Exec(`CREATE TRIGGER native_no_metadata_fault BEFORE INSERT ON native_run_bindings
	 BEGIN SELECT RAISE(ABORT,'private barrier fault'); END`); err != nil {
		t.Fatal(err)
	}
	r := queueReceipt(provider.OutcomeUnknown)
	if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, r); err == nil {
		t.Fatal("no-metadata association failure did not roll back receipt")
	}
	if !reflect.DeepEqual(before, queueTurn(t, f.s, scope.Claim)) || queueReceiptJSON(t, f.s, scope.Claim) != "" {
		t.Fatal("no-metadata barrier failure partially committed")
	}
	if held, err := f.s.HasResourceClaim(ctx, "shared"); err != nil || !held {
		t.Fatal("failed no-metadata Apply released resources")
	}
	if _, err := f.s.db.Exec("DROP TRIGGER native_no_metadata_fault"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, r); err != nil {
		t.Fatal(err)
	}
	seq, purpose, savedBinding, ref := nativeStored(t, f.s, turn.ID)
	if purpose != "unknown" || savedBinding.Valid || ref.Valid {
		t.Fatal("unbound ordinary execution invented a qualified purpose/reference")
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, r); err != nil {
		t.Fatalf("repeated active unknown Apply: %v", err)
	}
	r.Outcome, r.Terminal = provider.OutcomeSucceeded, provider.OutcomeSucceeded
	r.NativeRef = nativeReference(t, binding)
	if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, r); err != nil {
		t.Fatal(err)
	}
	seq2, purpose2, binding2, ref2 := nativeStored(t, f.s, turn.ID)
	if seq2 != seq || purpose2 != purpose || binding2.Valid || ref2.Valid {
		t.Fatal("reconciliation replaced/upgraded the first no-metadata barrier")
	}
}

func TestNativeSessionNoMetadataIntakePurposeIsNotOrdinary(t *testing.T) {
	f := nativeFixture(t)
	turn, scope, _ := nativeClaim(t, f, f.groupID, f.a, "topic")
	if _, err := f.s.CaptureChatCandidate(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, scope.Claim, queueReceipt(provider.OutcomeSucceeded)); err != nil {
		t.Fatal(err)
	}
	_, purpose, binding, ref := nativeStored(t, f.s, turn.ID)
	if purpose != domain.ChatIntakePurpose || binding.Valid || ref.Valid {
		t.Fatal("no-metadata shortcut erased independently derived intake purpose")
	}
}

func TestNativeSessionNoMetadataRolePurposeIsNotOrdinary(t *testing.T) {
	f, request := materializePending(t)
	materialized, err := f.s.MaterializeDispatchRequest(ctx, request.ID, 1, materializeAt)
	if err != nil || materialized.Delivery == nil {
		t.Fatalf("materialize: %+v, %v", materialized, err)
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: request.GroupID, DeliveryID: materialized.Delivery.DeliveryID, ResourceKeys: []string{"decide-resource"}, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	memberScope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: request.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	provenance := CoordinationRoleProvenance{Kind: CoordinationRoleCoordinator, GroupID: request.GroupID, DeliveryID: turn.DeliveryID,
		MessageID: materialized.Delivery.MessageID, ConversationID: turn.ConversationID, MemberID: turn.MemberID, TopicID: request.TopicID,
		RequestID: request.ID, TaskID: request.TaskID, TaskRevision: request.TaskRevision}
	role := CoordinationToolScope{Member: memberScope, Provenance: provenance}
	decision := coordinationDecideAssign(t, f, role, "no-metadata-assignment")
	worker := coordinationDecideWorker(t, f, request, decision, true)
	for _, item := range []struct {
		scope   MemberToolScope
		purpose string
	}{{memberScope, "coordinator"}, {worker.Member, "worker"}} {
		if _, err := f.s.ApplyTurnReceipt(ctx, item.scope.Claim, queueReceipt(provider.OutcomeSucceeded)); err != nil {
			t.Fatal(err)
		}
		_, purpose, binding, ref := nativeStored(t, f.s, item.scope.Claim.TurnID)
		if purpose != item.purpose || binding.Valid || ref.Valid {
			t.Fatalf("no-metadata shortcut erased %s purpose: %s", item.purpose, purpose)
		}
	}
}
