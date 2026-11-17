//go:build unix

package platform

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"agentworkspace/internal/artifactfs"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func newWorkspacePreparation(t *testing.T) (*rolePrepareFixture, domain.TaskWorkspaceScope, string) {
	t.Helper()
	f := &rolePrepareFixture{prepareFixture: newPrepareFixture(t)}
	f.coordinator = f.member
	f.enableRuntime(t)
	private := t.TempDir()
	prepareRoleOK(t, os.Chmod(private, 0700))
	owner, err := os.OpenRoot(private)
	prepareRoleOK(t, err)
	blobs, err := artifactfs.New(owner, 65536)
	prepareRoleOK(t, err)
	prepareRoleOK(t, owner.Close())
	t.Cleanup(func() { blobs.Close() })
	scratch := t.TempDir()
	prepareRoleOK(t, os.Chmod(scratch, 0700))
	prepareRoleOK(t, f.opts.MemberTools.EnableWorkspace(MemberWorkspaceOptions{Blobs: blobs, ScratchRoot: scratch, ProtectedRoots: []string{private, f.opts.PrivateWorkspaceRoot}}))
	t.Cleanup(func() { f.opts.MemberTools.Close() })
	ctx := context.Background()
	worker, err := f.s.CreateMember(ctx, store.CreateMemberInput{GroupID: f.group.ID, Name: "file worker", Role: "writer", ProviderID: f.member.ProviderID, Model: f.member.Model})
	prepareRoleOK(t, err)
	f.worker = worker
	_, err = f.s.SetCoordinator(ctx, f.group.ID, f.coordinator.ID, 0, "binding")
	prepareRoleOK(t, err)
	_, err = f.s.SetDispatchPolicy(ctx, f.group.ID, []string{worker.ID}, 0, "dispatch-policy")
	prepareRoleOK(t, err)
	root := t.TempDir()
	_, err = f.preparer(t).AuthorizeWorkspaceGrant(ctx, f.group.ID, worker.ID, WorkspaceGrantInput{Root: root, Scope: "read_write"}, "grant")
	prepareRoleOK(t, err)
	_, err = f.s.SetChatPolicy(ctx, domain.ThreadRef{GroupID: f.group.ID, TopicID: f.trigger.TopicID}, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, AllowWorkspaceChanges: true, MaxAdmissions: 2}, 0, "policy")
	prepareRoleOK(t, err)
	message, err := NewLocalUser(f.s).CreateMessage(ctx, f.group.ID, MessageInput{TopicID: f.trigger.TopicID, Content: "依讨论在已授权目录修改文件，提交真实冻结版本", RecipientIDs: []string{f.coordinator.ID}}, "source")
	prepareRoleOK(t, err)
	turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: message.Deliveries[0].ID, MaxActive: 1})
	prepareRoleOK(t, err)
	scope, err := f.s.BindMemberTool(ctx, store.TurnClaim{GroupID: f.group.ID, TurnID: turn.ID, Token: turn.ClaimToken})
	prepareRoleOK(t, err)
	candidate, err := f.s.CaptureChatCandidate(ctx, scope)
	prepareRoleOK(t, err)
	receipt, err := f.s.ApplyChatAssessment(ctx, scope, candidate.ID, domain.ChatAssessment{Disposition: "request_work", Action: domain.ChatActionWorkspaceChange, Title: "目录修改", Description: "仅实际授权目录", Acceptance: "真实版本", Reason: "实际用户请求"}, "登记获准工作。", "admit")
	prepareRoleOK(t, err)
	if receipt.WorkspaceScope == nil {
		t.Fatal("real workspace scope missing")
	}
	_, err = f.s.ApplyTurnReceipt(ctx, scope.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	prepareRoleOK(t, err)
	materialized, err := f.s.MaterializeDispatchRequest(ctx, receipt.DispatchRequestID, 1, time.Now().UTC())
	prepareRoleOK(t, err)
	if materialized.Delivery == nil {
		t.Fatal("coordinator delivery missing")
	}
	f.selectDelivery(t, materialized.Delivery.MessageID)
	return f, *receipt.WorkspaceScope, root
}

func TestWorkspacePreparationSamplesTaskBoundFileFacts(t *testing.T) {
	f, scope, _ := newWorkspacePreparation(t)
	plan := f.contextPlan(t, f.preparer(t))
	if len(plan.dispatchCapabilities) != 1 {
		t.Fatalf("file-capable candidate absent: %+v", plan.dispatchCapabilities)
	}
	fact := plan.dispatchCapabilities[0]
	if fact.RequiredProfile != store.DispatchWorkspaceWorkerProfile || fact.TaskID != scope.TaskID || fact.TaskRevision != scope.TaskRevision || fact.ResourceKey != scope.ResourceKey || fact.GrantID != scope.Grants[0].GrantID {
		t.Fatalf("text-only or unbound capability fact: %+v", fact)
	}
	// Physical identity changes invalidate the file sample, regardless of CWD.
	grant := scope.Grants[0]
	prepareRoleOK(t, os.Rename(grant.Root, grant.Root+"-old"))
	t.Cleanup(func() { os.RemoveAll(grant.Root + "-old") })
	prepareRoleOK(t, os.Mkdir(grant.Root, 0700))
	plan = f.contextPlan(t, f.preparer(t))
	if len(plan.dispatchCapabilities) != 0 {
		t.Fatal("replaced root kept file capability fact")
	}
}

func TestWorkspaceAdapterSamplingUsesExactVerifiedFileProfile(t *testing.T) {
	f := newRolePrepareFixture(t)
	f.enableRuntime(t)
	p := f.preparer(t)
	names := provider.WorkspaceWorkerToolNames()
	if _, _, err := p.sampleMemberToolMember(f.worker, names); err != nil {
		t.Fatalf("verified file profile not selected: %v", err)
	}
	selected := p.providers[f.worker.ProviderID]
	selected.Name = provider.Codex
	selected.Version = "0.162.1"
	p.enableCodexAppServer = true
	if _, _, err := p.sampleMemberToolAdapter(selected, names); !errors.Is(err, errMemberToolAdapterUnsupported) {
		t.Fatalf("Codex text evidence widened to files: %v", err)
	}
	for _, bad := range [][]string{names[:7], append(append([]string{}, names...), "send_message")} {
		selected.Name = provider.Pi
		selected.Version = "1.4.0"
		if _, _, err := p.sampleMemberToolAdapter(selected, bad); !errors.Is(err, errMemberToolAdapterUnsupported) {
			t.Fatalf("mixed file profile supported: %v", err)
		}
	}
}
