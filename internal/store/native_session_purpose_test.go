package store

import (
	"errors"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func TestNativeSessionIntakePurposeUsesPersistentCandidate(t *testing.T) {
	f := nativeFixture(t)
	_, generic, genericBinding := nativeClaim(t, f, f.groupID, f.a, "topic")
	nativeApply(t, f.s, generic, genericBinding, provider.OutcomeSucceeded, "reference")
	turn, current, binding := nativeClaim(t, f, f.groupID, f.a, "topic")
	binding.Purpose, binding.CandidateID = domain.ChatIntakePurpose, "not-a-persisted-candidate"
	binding.Profile.Tools = []string{"list_members", "read_channel", "read_history", "search_history", "send_message"}
	if got, err := f.s.SelectNativeSession(ctx, current, binding); !errors.Is(err, ErrMemberToolDenied) || got.Reference != nil {
		t.Fatalf("caller selected intake purpose without persistent provenance: %+v, %v", got, err)
	}
	candidate, err := f.s.CaptureChatCandidate(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	binding.CandidateID = candidate.ID
	binding.Profile.Authority = NativeAuthorityScope{ChatPolicyRevision: candidate.Policy.Revision, BindingRevision: candidate.BindingRevision, PolicyRevision: candidate.DispatchPolicyRevision}
	if got, err := f.s.SelectNativeSession(ctx, current, binding); err != nil || got.Reference != nil || got.SourceTurnID != "" {
		t.Fatalf("intake crossed generic purpose: %+v, %v", got, err)
	}
	nativeApply(t, f.s, current, binding, provider.OutcomeSucceeded, "reference")
	_, purpose, _, ref := nativeStored(t, f.s, turn.ID)
	if purpose != domain.ChatIntakePurpose || !ref.Valid {
		t.Fatal("persistent intake provenance did not qualify actual reference")
	}
	_, next, nextBinding := nativeClaim(t, f, f.groupID, f.a, "topic")
	nextCandidate, err := f.s.CaptureChatCandidate(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	nextBinding.Purpose, nextBinding.CandidateID, nextBinding.Profile = binding.Purpose, nextCandidate.ID, binding.Profile
	if got, err := f.s.SelectNativeSession(ctx, next, nextBinding); err != nil || got.Reference == nil || got.SourceTurnID != turn.ID {
		t.Fatalf("changing current candidate polluted compatible profile: %+v, %v", got, err)
	}
}

func TestNativeSessionRolePurposeUsesHistoricalProvenanceNotLiveRole(t *testing.T) {
	f, r := materializePending(t)
	nativeInstallSchema(t, f.s)
	materialized, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	if err != nil || materialized.Delivery == nil {
		t.Fatalf("materialize: %+v, %v", materialized, err)
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: materialized.Delivery.DeliveryID, ResourceKeys: []string{"decide-resource"}, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	memberScope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: r.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	binding := nativeBindingFixture(turn)
	binding.TopicID, binding.Purpose = r.TopicID, "coordinator"
	binding.Profile.ResourceKeys = []string{"decide-resource"}
	binding.Profile.Tools = []string{"decide_dispatch", "list_members", "read_channel", "read_history", "search_history"}
	binding.Profile.Authority = NativeAuthorityScope{RequestID: r.ID, TaskID: r.TaskID, TaskRevision: r.TaskRevision,
		MandateID: r.MandateID, AuthorityGeneration: r.AuthorityGeneration, BindingRevision: r.BindingRevision, PolicyRevision: r.PolicyRevision}
	if got, err := f.s.SelectNativeSession(ctx, memberScope, binding); err != nil || got.Reference != nil || got.SourceTurnID != "" {
		t.Fatalf("current coordinator provenance fence: %+v, %v", got, err)
	}
	p := CoordinationRoleProvenance{Kind: CoordinationRoleCoordinator, GroupID: r.GroupID, DeliveryID: turn.DeliveryID, MessageID: materialized.Delivery.MessageID,
		ConversationID: turn.ConversationID, MemberID: turn.MemberID, TopicID: r.TopicID, RequestID: r.ID, TaskID: r.TaskID, TaskRevision: r.TaskRevision}
	role := CoordinationToolScope{Member: memberScope, Provenance: p}
	decision := coordinationDecideAssign(t, f, role, "native-test-assignment")
	worker := coordinationDecideWorker(t, f, r, decision, true)
	// Business completion and live revocation must not invalidate historical
	// execution provenance or block truthful terminal settlement.
	if _, err := f.s.db.Exec("UPDATE members SET status='left',role='ordinary',model='changed' WHERE id=?", turn.MemberID); err != nil {
		t.Fatal(err)
	}
	nativeApply(t, f.s, memberScope, binding, provider.OutcomeSucceeded, "reference")
	_, purpose, _, ref := nativeStored(t, f.s, turn.ID)
	if purpose != "coordinator" || !ref.Valid {
		t.Fatal("historical role settlement incorrectly used live startup authorization")
	}
	workerTurn := queueTurn(t, f.s, worker.Member.Claim)
	workerBinding := nativeBindingFixture(workerTurn)
	workerBinding.TopicID, workerBinding.Purpose = r.TopicID, "worker"
	workerBinding.Profile.ResourceKeys = []string{"worker-resource"}
	workerBinding.Profile.Tools = []string{"read_channel", "read_history", "search_history", "submit_result"}
	workerBinding.Profile.Authority = binding.Profile.Authority
	workerBinding.Profile.Authority.AssignmentID = decision.Assignment.ID
	// Coordinator revocation affects the NEW worker selector, not old Apply.
	if got, err := f.s.SelectNativeSession(ctx, worker.Member, workerBinding); !errors.Is(err, ErrMemberToolDenied) || got.Reference != nil {
		t.Fatalf("new role selector ignored live revocation: %+v, %v", got, err)
	}
	nativeApply(t, f.s, worker.Member, workerBinding, provider.OutcomeSucceeded, "reference")
	_, purpose, _, ref = nativeStored(t, f.s, workerTurn.ID)
	if purpose != "worker" || !ref.Valid {
		t.Fatal("worker historical provenance was lost")
	}
}
