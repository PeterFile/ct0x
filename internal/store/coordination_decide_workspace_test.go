package store

import (
	"errors"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func workspaceDispatchScope(t *testing.T) (chatFixture, CoordinationToolScope, domain.TaskWorkspaceScope) {
	t.Helper()
	f := chatWorkspaceSetup(t, "user", false)
	chatWorkspaceAuthorize(t, f, f.groupID, f.b, t.TempDir(), "workspace:1:2", "read_write")
	admitted, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "只登记获准的目录工作。", "file-admission")
	if err != nil || admitted.WorkspaceScope == nil {
		t.Fatalf("admission: %+v %v", admitted, err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, f.scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	materialized, err := f.s.MaterializeDispatchRequest(ctx, admitted.DispatchRequestID, 1, time.Now().UTC())
	if err != nil || materialized.Delivery == nil {
		t.Fatalf("materialize: %+v %v", materialized, err)
	}
	state, err := f.s.ResolveCoordinationDelivery(ctx, f.groupID, materialized.Delivery.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: materialized.Delivery.DeliveryID, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	return f, CoordinationToolScope{Member: member, Provenance: state.Provenance}, *admitted.WorkspaceScope
}

func TestWorkspaceDispatchBindsExactProfileAndTaskScope(t *testing.T) {
	for _, mode := range []string{"supported", "profile", "task", "revision", "resource", "node", "grant"} {
		t.Run(mode, func(t *testing.T) {
			f, role, scope := workspaceDispatchScope(t)
			worker, err := f.s.GetMember(ctx, f.groupID, f.b)
			if err != nil {
				t.Fatal(err)
			}
			fact := DispatchWorkerCapabilityFact{GroupID: f.groupID, MemberID: f.b, ProviderID: worker.ProviderID, Model: worker.Model, RequiredProfile: DispatchWorkspaceWorkerProfile, TaskID: scope.TaskID, TaskRevision: scope.TaskRevision, ResourceKey: scope.ResourceKey, NodeID: scope.NodeID, GrantID: scope.Grants[0].GrantID}
			switch mode {
			case "profile":
				fact.RequiredProfile = "text_worker/v1"
			case "task":
				fact.TaskID += "other"
			case "revision":
				fact.TaskRevision++
			case "resource":
				fact.ResourceKey += "other"
			case "node":
				fact.NodeID = "remote"
			case "grant":
				fact.GrantID += "other"
			}
			got, err := f.s.DecideDispatchWithCapabilities(ctx, role, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "精确文件profile及Task范围"}, "assign", []DispatchWorkerCapabilityFact{fact})
			if mode == "supported" {
				if err != nil || got.Assignment == nil {
					t.Fatalf("scoped file fact denied: %+v %v", got, err)
				}
			} else if !errors.Is(err, ErrConflict) || got.Assignment != nil {
				t.Fatalf("mismatched file fact admitted: %+v %v", got, err)
			}
		})
	}
}

func TestWorkspaceDispatchRejectsTextOnlyFacts(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "with-capabilities", true: "legacy-entry"}[legacy], func(t *testing.T) {
			f, scope, _ := workspaceDispatchScope(t)
			worker, err := f.s.GetMember(ctx, f.groupID, f.b)
			if err != nil {
				t.Fatal(err)
			}
			input := CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "文本支持不能证明文件工具能力"}
			var got domain.DecisionReceipt
			if legacy {
				got, err = f.s.DecideDispatch(ctx, scope, input, "file-dispatch")
			} else {
				got, err = f.s.DecideDispatchWithCapabilities(ctx, scope, input, "file-dispatch", []DispatchWorkerCapabilityFact{{GroupID: worker.GroupID, MemberID: worker.ID, ProviderID: worker.ProviderID, Model: worker.Model}})
			}
			if !errors.Is(err, ErrConflict) || got.Assignment != nil {
				t.Fatalf("text facts authorized file assignment: %+v %v", got, err)
			}
		})
	}
}
