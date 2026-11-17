package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"agentworkspace/internal/domain"
)

func dispatchCapabilityFact(t *testing.T, f requestDispatchFixture) DispatchWorkerCapabilityFact {
	t.Helper()
	worker, err := f.s.GetMember(ctx, f.groupID, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	return DispatchWorkerCapabilityFact{GroupID: worker.GroupID, MemberID: worker.ID, ProviderID: worker.ProviderID, Model: worker.Model}
}

func TestCoordinationDecideCapabilitiesNewAssignment(t *testing.T) {
	for _, mode := range []string{"supported", "no-facts", "group", "member", "provider", "model", "empty-provider", "empty-model", "changed-provider", "changed-model", "tx-provider", "tx-model"} {
		t.Run(mode, func(t *testing.T) {
			f, _, scope := coordinationDecideScope(t)
			fact := dispatchCapabilityFact(t, f)
			facts := []DispatchWorkerCapabilityFact{fact}
			switch mode {
			case "no-facts":
				facts = nil
			case "group":
				facts[0].GroupID = group(t, f.s).ID
			case "member":
				facts[0].MemberID = f.coord
			case "provider":
				facts[0].ProviderID += "-other"
			case "model":
				facts[0].Model += "-other"
			case "empty-provider":
				facts[0].ProviderID = ""
			case "empty-model":
				facts[0].Model = ""
			case "changed-provider", "changed-model":
				column := "provider_id"
				if mode == "changed-model" {
					column = "model"
				}
				coordinationDecideExec(t, f.s, "UPDATE members SET "+column+"='changed' WHERE id=?", f.worker)
			case "tx-provider", "tx-model":
				column := "provider_id"
				if mode == "tx-model" {
					column = "model"
				}
				// A real SQL trigger changes current metadata within the command's
				// transaction. The final capability recheck must roll it all back.
				coordinationDecideExec(t, f.s, fmt.Sprintf("CREATE TRIGGER capability_change AFTER INSERT ON coordination_decisions BEGIN UPDATE members SET %s='changed' WHERE id='%s'; END", column, f.worker))
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.DecideDispatchWithCapabilities(ctx, scope, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: "能力采样不是许可"}, "capability", facts)
			if mode == "supported" {
				if err != nil || got.Assignment == nil || got.Assignment.WorkerMemberID != f.worker {
					t.Fatalf("supported server fact did not create assignment: %+v, %v", got, err)
				}
				return
			}
			requestDispatchZero(t, got, err, ErrConflict)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestCoordinationDecideCapabilitiesReplayBeforeNewQualification(t *testing.T) {
	f, _, scope := coordinationDecideScope(t)
	input := CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: "冻结原回执"}
	original, err := f.s.DecideDispatchWithCapabilities(ctx, scope, input, "original", []DispatchWorkerCapabilityFact{dispatchCapabilityFact(t, f)})
	if err != nil || original.Assignment == nil {
		t.Fatalf("original assignment: %+v, %v", original, err)
	}
	coordinationDecideExec(t, f.s, "UPDATE members SET provider_id='disabled-provider',model='changed-model' WHERE id=?", f.worker)
	before := coordinationDecideSnapshot(t, f.s)
	replayed, err := f.s.DecideDispatchWithCapabilities(ctx, scope, input, "original", nil)
	if err != nil || !reflect.DeepEqual(replayed, original) {
		t.Fatalf("capability invalidation replaced or hid original receipt: %+v, %v", replayed, err)
	}
	bad := input
	bad.Reason += "changed"
	got, err := f.s.DecideDispatchWithCapabilities(ctx, scope, bad, "original", nil)
	requestDispatchZero(t, got, err, ErrConflict)
	got, err = f.s.DecideDispatchWithCapabilities(ctx, scope, input, "new-key", nil)
	requestDispatchZero(t, got, err, ErrConflict)
	coordinationDecidePreserved(t, f.s, before)
}

func TestCoordinationDecideCapabilitiesPreserveAuthorityFences(t *testing.T) {
	for _, mode := range []string{"old-claim", "cross-group", "policy", "authority", "max-generation", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			f, r, scope := coordinationDecideScope(t)
			fact := dispatchCapabilityFact(t, f)
			want := ErrConflict
			switch mode {
			case "old-claim":
				scope.Member.Claim.Token += "stale"
				want = ErrMemberToolDenied
			case "cross-group":
				scope.Member.Claim.GroupID = group(t, f.s).ID
				want = ErrMemberToolDenied
			case "policy", "authority":
				materializeChangeEligibility(t, f, mode)
			case "max-generation":
				// Match the existing counter-boundary fixture, not a normal transition.
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_dispatch_state_update")
				coordinationDecideExec(t, f.s, "UPDATE coordination_dispatch_state SET generation=9223372036854775807 WHERE request_id=?", r.ID)
				want = ErrInvalid
			case "unknown":
				coordinationDecideExec(t, f.s, "UPDATE turns SET state='unknown' WHERE id=?", scope.Member.Claim.TurnID)
				want = ErrMemberToolDenied
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.DecideDispatchWithCapabilities(ctx, scope, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: "不得由能力扩权"}, "fenced", []DispatchWorkerCapabilityFact{fact})
			requestDispatchZero(t, got, err, want)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
	f, _, scope := coordinationDecideScope(t)
	declined, err := f.s.DecideDispatchWithCapabilities(ctx, scope, CoordinatorDecisionInput{Kind: "decline", Reason: "能力未知只拒绝不分配"}, "decline", nil)
	if err != nil || declined.Decision.Kind != domain.CoordinatorDecisionKindDecline || declined.Assignment != nil {
		t.Fatalf("decline incorrectly requires worker capability: %+v, %v", declined, err)
	}
}

func TestDispatchWorkerCapabilityFactCannotComeFromJSON(t *testing.T) {
	fact := DispatchWorkerCapabilityFact{GroupID: "group", MemberID: "member", ProviderID: "provider", Model: "model"}
	data, err := json.Marshal(fact)
	if err != nil || string(data) != "{}" {
		t.Fatalf("trusted capability facts serialized: %s, %v", data, err)
	}
	var forged DispatchWorkerCapabilityFact
	if err := json.Unmarshal([]byte(`{"group_id":"group","member_id":"member","provider_id":"provider","model":"model","ready":true}`), &forged); err != nil || forged != (DispatchWorkerCapabilityFact{}) {
		t.Fatalf("model JSON selected capability facts: %+v, %v", forged, err)
	}
}
