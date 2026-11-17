package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

func chatWorkspacePolicyInput(t *testing.T, input domain.ChatPolicyRequest) domain.ChatPolicyRequest {
	t.Helper()
	input.AllowWorkspaceChanges = true
	return input
}

func chatWorkspaceNextCandidate(t *testing.T, f *chatFixture, trusted bool) {
	t.Helper()
	if _, err := f.s.ApplyTurnReceipt(ctx, f.scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	r := MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user", Content: "请按刚才约定在本群已授权目录落实文件变更，交付可核验的文件版本。", RecipientIDs: []string{f.a}}
	var message domain.Message
	var err error
	if trusted {
		message = chatLocalMessage(t, f.s, r, []string{f.a})
	} else {
		message, err = f.s.CreateMessage(ctx, r, []string{f.a})
		if err != nil {
			t.Fatal(err)
		}
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	f.scope, err = f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	f.candidate, err = f.api.CaptureChatCandidate(ctx, f.scope)
	if err != nil {
		t.Fatal(err)
	}
}

func chatWorkspaceSetup(t *testing.T, sender string, preparation bool) chatFixture {
	t.Helper()
	f := chatSetup(t, "user", preparation)
	input := f.policy.ChatPolicyRequest
	input.PreauthorizationMessageID = ""
	var err error
	f.policy, err = f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, chatWorkspacePolicyInput(t, input), 1, "workspace-policy")
	if err != nil {
		t.Fatal(err)
	}
	f.input.Action = "workspace_change"
	f.input.Title, f.input.Description, f.input.Acceptance = "更新已授权目录", "按讨论落实变更", "提交可核验的冻结文件版本"
	if sender == "user" {
		chatWorkspaceNextCandidate(t, &f, true)
		return f
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, f.scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	invitation := chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user", Content: "仅请提出建议", RecipientIDs: []string{f.b}}, []string{f.b})
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: invitation.Deliveries[0].ID, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	message, err := f.s.SendMessageForTool(ctx, peer, MemberToolMessageInput{Content: "成员都赞同写入目录", RecipientIDs: []string{f.a}}, "workspace-consensus")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, peer.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	turn, err = f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	f.scope, err = f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	f.candidate, err = f.api.CaptureChatCandidate(ctx, f.scope)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func chatWorkspaceAuthorize(t *testing.T, f chatFixture, groupID, memberID, root, resource, scope string) WorkspaceGrantReceipt {
	t.Helper()
	grant, err := f.s.AuthorizeWorkspaceGrant(ctx, AuthorizeWorkspaceGrantInput{GroupID: groupID, MemberID: memberID, Root: root, Scope: scope, IdempotencyKey: "workspace-" + memberID}, func([]WorkspaceGrantReceipt) (WorkspaceGrantDirectory, error) {
		return WorkspaceGrantDirectory{ResolvedRoot: root, ResourceKey: resource}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func TestChatWorkspaceAdmissionBindsServerScopeAndReplays(t *testing.T) {
	f := chatWorkspaceSetup(t, "user", false)
	grant := chatWorkspaceAuthorize(t, f, f.groupID, f.b, t.TempDir(), "workspace:1:2", "read_write")
	got, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "将按获准范围执行，尚未交付。", "workspace-admit")
	if err != nil || got.Outcome != "admitted" || got.TaskID == "" || got.SourceKind != "local_user_chat" || !got.AuthorityEvidenceVerified {
		t.Fatalf("explicit directory request not admitted: %+v %v", got, err)
	}
	scope := got.WorkspaceScope
	if scope == nil || scope.TaskID != got.TaskID || scope.GroupID != f.groupID || scope.TopicID != f.scope.TopicID || scope.ResourceKey != grant.ResourceKey || scope.NodeID != "local" || scope.TaskRevision != 1 || scope.PolicyRevision != 2 {
		t.Fatalf("missing server-derived scope: %+v", scope)
	}
	if len(scope.Grants) != 1 || scope.Grants[0] != (domain.TaskWorkspaceGrant{GrantID: grant.ID, MemberID: f.b, Root: grant.Root, ResolvedRoot: grant.ResolvedRoot, GrantSchemaVersion: 19}) {
		t.Fatalf("grant authority was not preserved: %+v", scope)
	}
	policy := f.policy.ChatPolicyRequest
	policy.Enabled, policy.PreauthorizationMessageID = false, ""
	if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, policy, 2, "disable-workspace"); err != nil {
		t.Fatal(err)
	}
	coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.b)
	before := messageSequenceSnapshot(t, f.s)
	for _, s := range []*Store{f.s, queueHandle(t, f.path, false)} {
		replay, err := s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "将按获准范围执行，尚未交付。", "workspace-admit")
		if err != nil || !reflect.DeepEqual(replay, got) {
			t.Fatalf("policy/member change reinterpreted original receipt: %+v %v", replay, err)
		}
	}
	changed := f.input
	changed.Description += " changed"
	if _, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, changed, "将按获准范围执行，尚未交付。", "workspace-admit"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different workspace payload did not conflict: %v", err)
	}
	messageSequencePreserved(t, f.s, before)
	dispatchAssertFK(t, f.s)
}

func TestChatWorkspaceNonAdmissionAuthorityAndResources(t *testing.T) {
	for _, scenario := range []string{"default-disabled", "policy-disabled", "read-only", "foreign-only", "ambiguous", "agent-consensus", "unproven-user", "prepare-work", "stale-policy", "stale-routing", "stale-source", "inactive-worker"} {
		t.Run(scenario, func(t *testing.T) {
			sender := "user"
			if scenario == "agent-consensus" {
				sender = "agent"
			}
			f := chatWorkspaceSetup(t, sender, true)
			want := "workspace_scope_unavailable"
			scope := "read_write"
			if scenario == "read-only" {
				scope = "read_only"
			}
			chatWorkspaceAuthorize(t, f, f.groupID, f.b, t.TempDir(), "workspace:1:2", scope)
			switch scenario {
			case "default-disabled", "policy-disabled", "stale-policy":
				input := f.policy.ChatPolicyRequest
				input.PreauthorizationMessageID = ""
				if scenario == "default-disabled" {
					input = domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 2}
				}
				if scenario == "policy-disabled" {
					input.Enabled = false
				}
				if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, input, 2, "new-policy"); err != nil {
					t.Fatal(err)
				}
				want = "stale_policy"
				if scenario == "default-disabled" {
					chatWorkspaceNextCandidate(t, &f, true)
					want = "workspace_changes_disabled"
				}
				if scenario == "policy-disabled" {
					chatWorkspaceNextCandidate(t, &f, true)
					want = "policy_disabled"
				}
			case "foreign-only":
				coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.b)
				other := member(t, f.s, f.groupID)
				if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{other.ID}, 1, "other-worker"); err != nil {
					t.Fatal(err)
				}
				foreign := member(t, f.s, f.otherID)
				chatWorkspaceAuthorize(t, f, f.otherID, foreign.ID, t.TempDir(), "workspace:foreign:secret", "read_write")
				chatWorkspaceNextCandidate(t, &f, true)
			case "ambiguous":
				other := member(t, f.s, f.groupID)
				chatWorkspaceAuthorize(t, f, f.groupID, other.ID, t.TempDir(), "workspace:3:4", "read_write")
				if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b, other.ID}, 1, "two-workers"); err != nil {
					t.Fatal(err)
				}
				chatWorkspaceNextCandidate(t, &f, true)
				want = "workspace_scope_ambiguous"
			case "agent-consensus":
				want = "user_request_required"
			case "unproven-user":
				chatWorkspaceNextCandidate(t, &f, false)
				want = "untrusted_source"
			case "prepare-work":
				f.input.Disposition = "prepare_work"
				f.input.Title, f.input.Description, f.input.Acceptance = "", "", ""
				want = "preparation_scope_mismatch"
			case "stale-routing":
				if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b}, 1, "new-routing"); err != nil {
					t.Fatal(err)
				}
				want = "stale_dispatch_configuration"
			case "stale-source":
				chatLocalMessage(t, f.s, MessageRequest{GroupID: f.groupID, TopicID: f.scope.TopicID, ActorID: "user", Content: "先停下，约束变了。"}, nil)
				want = "stale_source"
			case "inactive-worker":
				coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.b)
				want = "routing_unavailable"
			}
			before := messageSequenceSnapshot(t, f.s)
			r, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "不应越过当前范围。", "deny-workspace")
			chatAssertNotAdmitted(t, r, err, want)
			if (scenario == "policy-disabled" || scenario == "agent-consensus" || scenario == "unproven-user") && !strings.Contains(r.Question, "目录") {
				t.Fatalf("workspace denial asked about the wrong action: %s", r.Question)
			}
			after := messageSequenceSnapshot(t, f.s)
			for _, table := range []string{"tasks", "mandates", "coordination_dispatch_requests", "resource_claims"} {
				if !reflect.DeepEqual(before[table], after[table]) {
					t.Fatalf("non-admission changed %s", table)
				}
			}
			notice, err := f.s.GetMessage(ctx, f.groupID, r.NoticeMessageID)
			if err != nil || strings.Count(notice.Content, "？") != 1 || strings.Contains(notice.Content, "workspace:foreign:secret") {
				t.Fatalf("not one scoped question: %+v %v", notice, err)
			}
		})
	}
}

func TestChatWorkspaceSameRootRetainsAllEligibleWorkers(t *testing.T) {
	f := chatWorkspaceSetup(t, "user", false)
	root := t.TempDir()
	chatWorkspaceAuthorize(t, f, f.groupID, f.b, root, "workspace:1:2", "read_write")
	other := member(t, f.s, f.groupID)
	chatWorkspaceAuthorize(t, f, f.groupID, other.ID, root, "workspace:1:2", "read_write")
	if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b, other.ID}, 1, "shared-workers"); err != nil {
		t.Fatal(err)
	}
	chatWorkspaceNextCandidate(t, &f, true)
	r, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "共享目录由获准成员执行。", "shared-root")
	if err != nil || r.TaskID == "" {
		t.Fatalf("same physical root became ambiguous: %+v %v", r, err)
	}
	var count int
	if err := f.s.db.QueryRow("SELECT count(*) FROM chat_workspace_scope_grants WHERE task_id=?", r.TaskID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("eligible member set lost: %d %v", count, err)
	}
	task, err := f.s.GetTask(ctx, f.groupID, r.TaskID)
	if err != nil || task.AssigneeID != "" {
		t.Fatalf("scope arbitrarily selected an assignee: %+v %v", task, err)
	}
}

func TestChatWorkspacePhysicalIdentityIgnoresPathSpelling(t *testing.T) {
	for _, scenario := range []string{"same-path-different-resource", "different-path-same-resource"} {
		t.Run(scenario, func(t *testing.T) {
			samePhysical := scenario == "different-path-same-resource"
			f := chatWorkspaceSetup(t, "user", false)
			firstRoot, secondRoot := t.TempDir(), t.TempDir()
			secondResource := "workspace:3:4"
			if samePhysical {
				secondResource = "workspace:1:2"
			} else {
				secondRoot = firstRoot
			}
			chatWorkspaceAuthorize(t, f, f.groupID, f.b, firstRoot, "workspace:1:2", "read_write")
			other := member(t, f.s, f.groupID)
			chatWorkspaceAuthorize(t, f, f.groupID, other.ID, secondRoot, secondResource, "read_write")
			if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, []string{f.b, other.ID}, 1, "identity-workers"); err != nil {
				t.Fatal(err)
			}
			chatWorkspaceNextCandidate(t, &f, true)
			r, err := f.api.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "目录按物理身份绑定。", "physical-identity")
			if !samePhysical {
				chatAssertNotAdmitted(t, r, err, "workspace_scope_ambiguous")
				return
			}
			if err != nil || r.WorkspaceScope == nil || len(r.WorkspaceScope.Grants) != 2 {
				t.Fatalf("same identity aliases not retained: %+v %v", r, err)
			}
			roots := map[string]string{}
			for _, grant := range r.WorkspaceScope.Grants {
				roots[grant.MemberID] = grant.Root
			}
			if roots[f.b] != firstRoot || roots[other.ID] != secondRoot {
				t.Fatalf("original roots replaced: %v", roots)
			}
		})
	}
}

func TestChatWorkspaceConcurrentSameKeyScopeReplay(t *testing.T) {
	f := chatWorkspaceSetup(t, "user", false)
	chatWorkspaceAuthorize(t, f, f.groupID, f.b, t.TempDir(), "workspace:1:2", "read_write")
	type outcome struct {
		receipt domain.ChatAssessmentReceipt
		err     error
	}
	results := make(chan outcome, 2)
	for _, s := range []*Store{f.s, queueHandle(t, f.path, false)} {
		go func(s *Store) {
			r, err := s.ApplyChatAssessment(ctx, f.scope, f.candidate.ID, f.input, "同一目录请求。", "concurrent-workspace")
			results <- outcome{r, err}
		}(s)
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || !reflect.DeepEqual(first.receipt, second.receipt) || first.receipt.WorkspaceScope == nil {
		t.Fatalf("concurrent scope replay diverged: %+v %+v", first, second)
	}
	for _, table := range []string{"tasks", "mandates", "coordination_dispatch_requests", "chat_workspace_scopes", "chat_workspace_scope_grants"} {
		var count int
		if err := f.s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("duplicated %s: %d %v", table, count, err)
		}
	}
}

func TestChatWorkspacePolicyOptInPreservesLegacyPayload(t *testing.T) {
	f := chatSetup(t, "user", false)
	old := f.policy.ChatPolicyRequest
	encoded, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"enabled":true,"allow_preparation":false,"preauthorization_message_id":"","action":"text_submission","max_admissions":2}`
	if string(encoded) != want {
		t.Fatalf("legacy payload changed: %s", encoded)
	}
	var payload []byte
	if err := f.s.db.QueryRow("SELECT payload FROM mutation_idempotency WHERE operation='set_chat_policy' AND idempotency_key='policy'").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payload, mutationPayload(f.groupID, f.scope.TopicID, "0", want)) {
		t.Fatal("legacy policy mutation payload changed")
	}
	before := messageSequenceSnapshot(t, f.s)
	replay, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, old, 0, "policy")
	if err != nil || !reflect.DeepEqual(replay, f.policy) {
		t.Fatalf("legacy key replay: %+v %v", replay, err)
	}
	messageSequencePreserved(t, f.s, before)

	input := chatWorkspacePolicyInput(t, old)
	if _, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, input, 0, "policy"); !errors.Is(err, ErrConflict) {
		t.Fatalf("opt-in must change payload under original key: %v", err)
	}
	policy, err := f.api.SetChatPolicy(ctx, f.candidate.ThreadRef, input, 1, "allow-workspace")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(policy)
	if !strings.Contains(string(data), `"allow_workspace_changes":true`) || policy.Action != domain.ChatActionTextSubmission {
		t.Fatalf("explicit optional workspace capability not persisted: %s", data)
	}
	for _, s := range []*Store{f.s, queueHandle(t, f.path, false)} {
		current, err := s.GetChatPolicy(ctx, f.candidate.ThreadRef)
		if err != nil || !reflect.DeepEqual(current, policy) {
			t.Fatalf("workspace opt-in lost on read/reopen: %+v %v", current, err)
		}
	}
}
