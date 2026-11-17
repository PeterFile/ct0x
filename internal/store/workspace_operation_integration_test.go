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
	"agentworkspace/internal/provider"
)

// This fixture admits real chat work, then uses the existing materialization,
// claim and Assignment/Attempt path. The directory/blob metadata is trusted
// fixture data only: it proves no physical IO, durability or CLI capability.
func workspaceOperationFixture(t *testing.T) (requestDispatchFixture, domain.DispatchRequest, CoordinationToolScope) {
	t.Helper()
	if len(migrations) < 21 {
		t.Fatal("real operation fixtures require registered F3 v20 and F4 v21; never fabricate a workspace scope")
	}
	f := setupMessages(t)
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: "workspace-topic"}
	var policy domain.ChatPolicyRequest
	if err := json.Unmarshal([]byte(`{"enabled":true,"action":"text_submission","max_admissions":8,"allow_workspace_changes":true}`), &policy); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetChatPolicy(ctx, thread, policy, 0, "workspace-policy"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetCoordinator(ctx, f.groupID, f.a, 0, "coordinator"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b}, 0, "dispatch"); err != nil {
		t.Fatal(err)
	}
	grant, err := f.s.AuthorizeWorkspaceGrant(ctx, AuthorizeWorkspaceGrantInput{GroupID: f.groupID, MemberID: f.b, Root: "/fixture/workspace", Scope: "read_write", IdempotencyKey: "write"}, func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
		return WorkspaceGrantDirectory{ResolvedRoot: "/fixture/workspace", ResourceKey: "workspace:fixture:physical"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := f.s.CreateLocalUserMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: thread.TopicID, ActorID: "user", Content: "按讨论约定修改目录中的文件，并提交可核对版本。", RecipientIDs: []string{f.a}}, []string{f.a})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	memberScope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := f.s.CaptureChatCandidate(ctx, memberScope)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := f.s.ApplyChatAssessment(ctx, memberScope, candidate.ID, domain.ChatAssessment{Disposition: "request_work", Action: "workspace_change", Title: "实际目录变更", Description: "按授权范围修改文件", Acceptance: "交付真实文件对应的冻结成果版本", Reason: "本机用户明确请求"}, "我会在获准目录内形成可核对成果。", "admit-workspace")
	if err != nil || admitted.TaskID == "" {
		t.Fatalf("real F3 admission: %+v, %v", admitted, err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, memberScope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted}); err != nil {
		t.Fatal(err)
	}
	view, err := f.s.GetDispatchRequest(ctx, f.groupID, admitted.DispatchRequestID)
	if err != nil {
		t.Fatal(err)
	}
	r := view.Request
	coordinator := coordinationDecideBindRequest(t, f.s, r)
	decision, err := f.s.DecideDispatch(ctx, coordinator, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.b, Reason: "分派已准入的目录工作"}, "assign-workspace")
	if err != nil || decision.Assignment == nil {
		t.Fatalf("workspace Assignment: %+v, %v", decision, err)
	}
	materialized, err := f.s.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
	if err != nil || materialized.Delivery == nil {
		t.Fatalf("workspace materialization: %+v, %v", materialized, err)
	}
	d := materialized.Delivery
	workerTurn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.DeliveryID, MaxActive: 8, ResourceKeys: []string{grant.ResourceKey}})
	if err != nil {
		t.Fatal(err)
	}
	workerMember, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: workerTurn.ID, Token: workerTurn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	role := CoordinationToolScope{Member: workerMember, Provenance: CoordinationRoleProvenance{Kind: CoordinationRoleWorker, GroupID: f.groupID, DeliveryID: d.DeliveryID, MessageID: d.MessageID, ConversationID: d.ConversationID, MemberID: f.b, TopicID: thread.TopicID, RequestID: r.ID, TaskID: r.TaskID, TaskRevision: r.TaskRevision, AssignmentID: decision.Assignment.ID}}
	binding, err := f.s.BindAssignmentAttempt(ctx, role)
	if err != nil {
		t.Fatal(err)
	}
	role.AttemptID = binding.AttemptID
	task, err := f.s.GetTask(ctx, f.groupID, r.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	return requestDispatchFixture{s: f.s, path: f.path, groupID: f.groupID, worker: f.b, coord: f.a, task: task}, r, role
}

func workspaceOperationBegin(t *testing.T, s *Store, role CoordinationToolScope, key string) WorkspaceOperation {
	t.Helper()
	o, created, err := s.BeginWorkspaceOperation(ctx, role, workspaceOperationInput(), workspaceOperationPreimage(), key)
	if err != nil || !created || o.ID == "" || o.Result != nil {
		t.Fatalf("new immutable pending intent: %+v created=%t err=%v", o, created, err)
	}
	return o
}
func workspaceOperationDone(t *testing.T, s *Store, role CoordinationToolScope, key string) WorkspaceOperation {
	t.Helper()
	o := workspaceOperationBegin(t, s, role, key)
	finished, err := s.FinishWorkspaceOperation(ctx, role, o.ID, WorkspaceOperationResult{Outcome: "done", ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize})
	if err != nil || finished.Result == nil || finished.Result.Outcome != "done" {
		t.Fatalf("known synced result: %+v, %v", finished, err)
	}
	return finished
}

func TestWorkspaceOperationIntentReceiptReplayAndNotice(t *testing.T) {
	f, _, role := workspaceOperationFixture(t)
	input, p := workspaceOperationInput(), workspaceOperationPreimage()
	if _, found, err := f.s.LookupWorkspaceOperation(ctx, role, input, "op"); err != nil || found {
		t.Fatalf("new key pre-IO lookup: %t,%v", found, err)
	}
	beforeClaims := messageSequenceTableRows(t, f.s, "resource_claims")
	o := workspaceOperationBegin(t, f.s, role, "op")
	afterBegin := coordinationDecideSnapshot(t, f.s)
	if o.Input != input || o.Preimage != p || o.TaskID != role.Provenance.TaskID || o.AttemptID != role.AttemptID || o.TurnID != role.Member.Claim.TurnID || o.MemberID != f.worker || o.ResourceKey != "workspace:fixture:physical" {
		t.Fatalf("unbound intent: %+v", o)
	}
	for _, lookup := range []bool{true, false} {
		var got WorkspaceOperation
		var flag bool
		var err error
		if lookup {
			got, flag, err = f.s.LookupWorkspaceOperation(ctx, role, input, "op")
		} else {
			got, flag, err = f.s.BeginWorkspaceOperation(ctx, role, input, p, "op")
			flag = !flag
		}
		if err != nil || !flag || !reflect.DeepEqual(got, o) {
			t.Fatalf("pending replay must never request IO: %+v,%t,%v", got, flag, err)
		}
	}
	changed := input
	changed.AfterHash = strings.Repeat("c", 64)
	if _, _, err := f.s.LookupWorkspaceOperation(ctx, role, changed, "op"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed intended bytes: %v", err)
	}
	changedP := p
	changedP.Size++
	if _, _, err := f.s.BeginWorkspaceOperation(ctx, role, input, changedP, "op"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed actual preimage metadata: %v", err)
	}
	if _, _, err := f.s.BeginWorkspaceOperation(ctx, role, input, p, "other"); !errors.Is(err, ErrConflict) {
		t.Fatalf("new key bypassed pending Task/Turn: %v", err)
	}
	coordinationDecidePreserved(t, f.s, afterBegin)
	result := WorkspaceOperationResult{Outcome: "done", ObservedHash: input.AfterHash, ObservedSize: input.AfterSize}
	done, err := f.s.FinishWorkspaceOperation(ctx, role, o.ID, result)
	if err != nil || done.Result == nil || *done.Result != result || done.FinishedAt == nil {
		t.Fatalf("terminal result: %+v,%v", done, err)
	}
	afterFinish := coordinationDecideSnapshot(t, f.s)
	for range 2 {
		got, err := f.s.FinishWorkspaceOperation(ctx, role, o.ID, result)
		if err != nil || !reflect.DeepEqual(got, done) {
			t.Fatalf("terminal replay: %+v,%v", got, err)
		}
	}
	if _, err := f.s.FinishWorkspaceOperation(ctx, role, o.ID, WorkspaceOperationResult{Outcome: "unknown", ErrorCode: "io_unknown"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("immutable terminal result changed: %v", err)
	}
	if got, created, err := f.s.BeginWorkspaceOperation(ctx, role, input, p, "op"); err != nil || created || !reflect.DeepEqual(got, done) {
		t.Fatalf("Begin replay lost original terminal result: %+v,%t,%v", got, created, err)
	}
	coordinationDecidePreserved(t, f.s, afterFinish)
	for _, notice := range []struct{ id, source, text string }{{o.NoticeMessageID, "workspace_begin:", "尚未确认"}, {done.FinishNoticeMessageID, "workspace_finish:", "同步"}} {
		m, err := f.s.GetMessage(ctx, f.groupID, notice.id)
		if err != nil || m.SourceKind != "platform" || m.SourceID != notice.source+o.ID || m.SenderID != f.worker || len(m.Deliveries) != 0 || !strings.Contains(m.Content, notice.text) {
			t.Fatalf("progress is not a public-only platform fact: %+v,%v", m, err)
		}
	}
	if !reflect.DeepEqual(beforeClaims, messageSequenceTableRows(t, f.s, "resource_claims")) {
		t.Fatal("intent/result/replay released resources")
	}
	for _, table := range []string{"workspace_operations", "workspace_operation_receipts"} {
		for _, query := range []string{"UPDATE " + table + " SET notice_message_id=notice_message_id", "DELETE FROM " + table, "INSERT OR REPLACE INTO " + table + " SELECT * FROM " + table} {
			if _, err := f.s.db.Exec(query); err == nil {
				t.Fatalf("SQL rewrote immutable evidence: %s", query)
			}
		}
	}
	dispatchAssertFK(t, f.s)
}

func TestWorkspaceOperationUnresolvedOutcomesAndRecovery(t *testing.T) {
	for _, outcome := range []string{"done", "unchanged", "unknown", "applied_sync_error"} {
		t.Run(outcome, func(t *testing.T) {
			f, _, role := workspaceOperationFixture(t)
			o := workspaceOperationBegin(t, f.s, role, "op")
			if pending, err := f.s.HasUnresolvedWorkspaceOperations(ctx, f.groupID, o.TurnID); err != nil || !pending {
				t.Fatalf("pending not unresolved: %t,%v", pending, err)
			}
			result := WorkspaceOperationResult{Outcome: outcome, ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize}
			switch outcome {
			case "unchanged":
				result.ObservedHash = o.Preimage.Hash
				result.ObservedSize = o.Preimage.Size
			case "unknown":
				result.ObservedHash = ""
				result.ObservedSize = 0
				result.ErrorCode = "io_unknown"
			case "applied_sync_error":
				result.ErrorCode = "sync_failed"
			}
			if _, err := f.s.FinishWorkspaceOperation(ctx, role, o.ID, result); err != nil {
				t.Fatal(err)
			}
			want := outcome == "unknown" || outcome == "applied_sync_error"
			if unresolved, err := f.s.HasUnresolvedWorkspaceOperations(ctx, f.groupID, o.TurnID); err != nil || unresolved != want {
				t.Fatalf("outcome classification %s: %t,%v", outcome, unresolved, err)
			}
			_, created, err := f.s.BeginWorkspaceOperation(ctx, role, workspaceOperationInput(), workspaceOperationPreimage(), "next")
			if want {
				if !errors.Is(err, ErrConflict) || created {
					t.Fatalf("unresolved result released new operation: %t,%v", created, err)
				}
			} else if err != nil || !created {
				t.Fatalf("resolved result blocked new key: %t,%v", created, err)
			}
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(f.path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if unresolved, err := reopened.HasUnresolvedWorkspaceOperations(ctx, f.groupID, o.TurnID); err != nil || !unresolved {
				t.Fatalf("reopen lost unresolved evidence: %t,%v", unresolved, err)
			}
			if _, err := reopened.RecoverInFlight(ctx); err != nil {
				t.Fatal(err)
			}
			turn, err := reopened.GetTurn(ctx, f.groupID, o.TurnID)
			if err != nil || turn.State != domain.TurnStateUnknown {
				t.Fatalf("existing recovery did not retain unknown: %+v,%v", turn, err)
			}
			if held, err := reopened.HasResourceClaim(ctx, o.ResourceKey); err != nil || !held {
				t.Fatalf("recovery released physical claim: %t,%v", held, err)
			}
			if _, err := reopened.FinishWorkspaceOperation(ctx, role, o.ID, result); !errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("old live role survived recovery: %v", err)
			}
		})
	}
}

func TestWorkspaceOperationReplayBeforeLaterEligibility(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			f, _, role := workspaceOperationFixture(t)
			var original WorkspaceOperation
			if terminal {
				original = workspaceOperationDone(t, f.s, role, "op")
			} else {
				original = workspaceOperationBegin(t, f.s, role, "op")
			}
			if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.worker}, 1, "new-policy"); err != nil {
				t.Fatal(err)
			}
			if got, found, err := f.s.LookupWorkspaceOperation(ctx, role, workspaceOperationInput(), "op"); err != nil || !found || !reflect.DeepEqual(got, original) {
				t.Fatalf("historical key consulted new eligibility: %+v,%t,%v", got, found, err)
			}
			if got, created, err := f.s.BeginWorkspaceOperation(ctx, role, workspaceOperationInput(), workspaceOperationPreimage(), "op"); err != nil || created || !reflect.DeepEqual(got, original) {
				t.Fatalf("original key replay: %+v,%t,%v", got, created, err)
			}
			if _, _, err := f.s.BeginWorkspaceOperation(ctx, role, workspaceOperationInput(), workspaceOperationPreimage(), "new"); err == nil {
				t.Fatal("new key ignored current eligibility")
			}
			if !terminal {
				if _, err := f.s.FinishWorkspaceOperation(ctx, role, original.ID, WorkspaceOperationResult{Outcome: "done", ObservedHash: original.Input.AfterHash, ObservedSize: original.Input.AfterSize}); err == nil {
					t.Fatal("Finish ignored invalidated current eligibility")
				}
			}
		})
	}
}

func TestWorkspaceOperationRevocationClaimAndAttemptFences(t *testing.T) {
	for _, change := range []string{"cancel", "left", "missing-claim", "wrong-claim-owner", "wrong-attempt", "empty-attempt", "cross-group", "wrong-task", "ordinary", "old-generation", "task-pointer"} {
		t.Run(change, func(t *testing.T) {
			f, _, role := workspaceOperationFixture(t)
			o := workspaceOperationBegin(t, f.s, role, "op")
			switch change {
			case "cancel":
				if _, err := f.s.RequestTurnCancel(ctx, f.groupID, o.TurnID); err != nil {
					t.Fatal(err)
				}
			case "left":
				coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.worker)
			case "missing-claim":
				coordinationDecideExec(t, f.s, "DELETE FROM resource_claims WHERE resource_key=?", o.ResourceKey)
			case "wrong-claim-owner":
				coordinationDecideExec(t, f.s, "UPDATE resource_claims SET turn_id=(SELECT id FROM turns WHERE member_id=? ORDER BY created_at DESC LIMIT 1) WHERE resource_key=?", f.coord, o.ResourceKey)
			case "wrong-attempt":
				role.AttemptID = "other-attempt"
			case "empty-attempt":
				role.AttemptID = ""
			case "cross-group":
				role.Member.Claim.GroupID = group(t, f.s).ID
			case "wrong-task":
				role.Provenance.TaskID = "other-task"
			case "ordinary":
				role.Provenance.Kind = CoordinationRoleOrdinary
			case "old-generation":
				role.Member.Claim.Token = "stale"
			case "task-pointer":
				coordinationDecideExec(t, f.s, "UPDATE tasks SET active_attempt_id=NULL WHERE id=?", o.TaskID)
			}
			before := coordinationDecideSnapshot(t, f.s)
			if _, err := f.s.FinishWorkspaceOperation(ctx, role, o.ID, WorkspaceOperationResult{Outcome: "done", ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize}); err == nil {
				t.Fatal("invalidated role/claim finalized an IO result")
			}
			if _, _, err := f.s.BeginWorkspaceOperation(ctx, role, workspaceOperationInput(), workspaceOperationPreimage(), "new"); err == nil {
				t.Fatal("invalidated role/claim began new IO")
			}
			coordinationDecidePreserved(t, f.s, before)
			if unresolved, err := f.s.HasUnresolvedWorkspaceOperations(ctx, f.groupID, o.TurnID); err != nil || !unresolved {
				t.Fatalf("failed Finish erased pending: %t,%v", unresolved, err)
			}
		})
	}
}

func TestWorkspaceOperationNoticeAndReceiptRollback(t *testing.T) {
	for _, phase := range []string{"begin-message", "begin-intent", "finish-message", "finish-receipt"} {
		t.Run(phase, func(t *testing.T) {
			f, _, role := workspaceOperationFixture(t)
			var o WorkspaceOperation
			if strings.HasPrefix(phase, "finish") {
				o = workspaceOperationBegin(t, f.s, role, "op")
			}
			table := "messages"
			if phase == "begin-intent" {
				table = "workspace_operations"
			}
			if phase == "finish-receipt" {
				table = "workspace_operation_receipts"
			}
			coordinationDecideExec(t, f.s, "CREATE TRIGGER f4_fail BEFORE INSERT ON "+table+" BEGIN SELECT RAISE(ABORT,'injected F4 storage failure'); END")
			before := coordinationDecideSnapshot(t, f.s)
			if o.ID == "" {
				if got, created, err := f.s.BeginWorkspaceOperation(ctx, role, workspaceOperationInput(), workspaceOperationPreimage(), "op"); err == nil || created || !reflect.DeepEqual(got, WorkspaceOperation{}) {
					t.Fatalf("failed Begin returned permission to IO: %+v,%t,%v", got, created, err)
				}
			} else {
				if got, err := f.s.FinishWorkspaceOperation(ctx, role, o.ID, WorkspaceOperationResult{Outcome: "done", ObservedHash: o.Input.AfterHash, ObservedSize: o.Input.AfterSize}); err == nil || !reflect.DeepEqual(got, WorkspaceOperation{}) {
					t.Fatalf("failed Finish claimed completion: %+v,%v", got, err)
				}
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}
