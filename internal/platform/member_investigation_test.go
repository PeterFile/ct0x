//go:build unix

package platform

import (
	"errors"
	"io"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

type investigationTestCloseHook struct {
	io.ReadCloser
	afterClose func() error
}

func (r investigationTestCloseHook) Close() error {
	return errors.Join(r.ReadCloser.Close(), r.afterClose())
}

func TestInvestigationArtifactPolicyRevocationAfterClose(t *testing.T) {
	ctx := t.Context()
	f := newArtifactsFixture(t, 32<<10)
	member := func(name string) domain.Member {
		m, err := f.db.CreateMember(ctx, store.CreateMemberInput{GroupID: f.groupID, Name: name, Role: "fixture", ProviderID: "pi", Model: "fixture/model"})
		artifactsMust(t, err)
		return m
	}
	lead, worker := member("coordinator"), member("worker")
	_, err := f.db.SetCoordinator(ctx, f.groupID, lead.ID, 0, "bind")
	artifactsMust(t, err)
	_, err = f.db.SetDispatchPolicy(ctx, f.groupID, []string{worker.ID}, 0, "dispatch-policy")
	artifactsMust(t, err)
	version := f.publish(t, PublishArtifactInput{Name: "material"}, "granted immutable bytes", "publish")
	thread := domain.ThreadRef{GroupID: f.groupID, TopicID: "investigation"}
	policy, err := f.db.SetChatPolicy(ctx, thread, domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1,
		Investigation: &domain.ChatInvestigationPolicy{Objective: "核对材料", ArtifactVersions: []domain.ArtifactVersionRef{{ArtifactID: version.ArtifactID, VersionID: version.ID}}}}, 0, "policy")
	artifactsMust(t, err)
	f.db.EnableInvestigationExecution()
	root, err := NewLocalUser(f.db).CreateMessage(ctx, f.groupID, MessageInput{TopicID: thread.TopicID, Content: "按配置核对材料", RecipientIDs: []string{lead.ID}}, "root")
	artifactsMust(t, err)
	claim := func(delivery string) store.MemberToolScope {
		turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: delivery, MaxActive: 1})
		artifactsMust(t, err)
		scope, err := f.db.BindMemberTool(ctx, store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
		artifactsMust(t, err)
		return scope
	}
	intake := claim(root.Deliveries[0].ID)
	candidate, err := f.db.CaptureChatCandidate(ctx, intake)
	artifactsMust(t, err)
	admission, err := f.db.ApplyChatAssessment(ctx, intake, candidate.ID, domain.ChatAssessment{
		Disposition: "prepare_work", Action: domain.ChatActionReadOnlyInvestigation, Reason: "仅检查核准材料",
	}, "按策略调查。", "admit")
	artifactsMust(t, err)
	if admission.Outcome != "admitted" {
		t.Fatal("expected a real investigation admission")
	}
	_, err = f.db.ApplyTurnReceipt(ctx, intake.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1})
	artifactsMust(t, err)
	dispatch, err := f.db.MaterializeDispatchRequest(ctx, admission.DispatchRequestID, 1, time.Now().UTC())
	artifactsMust(t, err)
	if dispatch.Delivery == nil {
		t.Fatal("expected a materialized coordinator delivery")
	}
	state, err := f.db.ResolveCoordinationDelivery(ctx, f.groupID, dispatch.Delivery.DeliveryID)
	artifactsMust(t, err)
	role := store.CoordinationToolScope{Member: claim(dispatch.Delivery.DeliveryID), Provenance: state.Provenance}
	saved, err := f.db.GetTaskInvestigationScope(ctx, f.groupID, admission.TaskID)
	artifactsMust(t, err)
	grant, err := NewMemberArtifactGrant(f.service)
	artifactsMust(t, err)
	closed := 0
	original := grant.open
	grant.open = func(hash string) (io.ReadCloser, error) {
		reader, err := original(hash)
		artifactsMust(t, err)
		return investigationTestCloseHook{ReadCloser: reader, afterClose: func() error {
			closed++
			changed := policy.ChatPolicyRequest
			changed.Enabled = false
			_, err := f.db.SetChatPolicy(ctx, thread, changed, policy.Revision, "revoke-policy")
			artifactsMust(t, err)
			// The claim is still valid. Only the post-I/O investigation fence
			// can detect this policy-only change and suppress the close failure.
			artifactsMust(t, f.db.ValidateMemberTool(ctx, role.Member))
			return errors.New("synthetic blob close failure")
		}}, nil
	}
	tools := NewMemberTools(f.db)
	token, err := tools.issueInvestigationRole(ctx, role, &investigationRolePreparation{
		investigationPreparationSettings: investigationPreparationSettings{providerID: lead.ProviderID, artifacts: grant},
		scope:                            saved, request: provider.ExecuteRequest{Model: lead.Model},
	}, nil)
	artifactsMust(t, err)
	t.Cleanup(func() { tools.Revoke(token) })
	if _, err := tools.ReadInvestigationChannel(ctx, token, store.ListOptions{}); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatal("artifact-only registration also granted topic material")
	}
	fragment, err := tools.ReadInvestigationArtifact(ctx, token, MemberArtifactReadInput{ArtifactID: version.ArtifactID, VersionID: version.ID})
	if fragment != (InvestigationArtifactFragment{}) || !errors.Is(err, store.ErrMemberToolDenied) || closed != 1 {
		t.Fatalf("post-I/O revocation did not deny the read: empty=%t closed=%d, err=%v", fragment == (InvestigationArtifactFragment{}), closed, err)
	}
}
