package platform_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/scheduler"
	"agentworkspace/internal/store"
)

type discussionReturnFixture struct {
	*boundWorkReportFixture
	inviter, receiver domain.Member
	origin, invite    domain.Message
}

func newDiscussionReturnFixture(t *testing.T) *discussionReturnFixture {
	t.Helper()
	ctx := t.Context()
	f := &discussionReturnFixture{boundWorkReportFixture: newBoundWorkReportFixture(t)}
	f.inviter = ownerFixtureMember(t, f.boundWorkReportFixture, f.group, "first inviter")
	f.receiver = ownerFixtureMember(t, f.boundWorkReportFixture, f.group, "investigation intake")
	// A second eligible worker makes accidental #208 delegation visible.
	_, err := f.db.SetDispatchPolicy(ctx, f.group, []string{f.worker.ID, f.inviter.ID}, 1, "return-workers")
	roleOK(t, err)
	f.policy, err = f.engine.SetChatPolicy(ctx, domain.ThreadRef{GroupID: f.group, TopicID: "discussion-return"},
		domain.ChatPolicyRequest{Enabled: true, Action: domain.ChatActionTextSubmission, MaxAdmissions: 1,
			Investigation: &domain.ChatInvestigationPolicy{Objective: "核对本话题的资料", AllowTopicMessages: true, MaxDelegations: 1}}, 0, "return-policy")
	roleOK(t, err)
	f.engine.EnableInvestigationExecution()
	f.engine.EnableTaskWorkExecution()
	f.origin, err = f.local.CreateMessage(ctx, f.group, platform.MessageInput{TopicID: f.policy.TopicID,
		Content: "请先和同伴核对本话题的调查范围。", RecipientIDs: []string{f.inviter.ID}}, "origin")
	roleOK(t, err)
	// C1 owns model-facing apply/read. Seed the exact persisted A path via its
	// public Store operations, with the worker as the *second* inviter.
	invite := func(source domain.Message, recipient, key string) domain.Message {
		scope, c := f.capture(t, source)
		_, err := f.engine.AssessChatIntent(ctx, scope, c.ID, domain.ChatAssessment{Disposition: "discuss", Reason: "请同伴核对"}, "请同伴核对。", key)
		roleOK(t, err)
		a, err := f.db.ApplyChatDiscussion(ctx, scope, c.ID, domain.ChatDiscussionAction{
			Kind: domain.ChatDiscussionInvite, RecipientID: recipient, Content: "请核对调查的未决问题。"}, key+"-invite")
		roleOK(t, err)
		f.settleWork(t, scope, true)
		return a.Message
	}
	f.invite = invite(invite(f.origin, f.worker.ID, "first"), f.receiver.ID, "forward")
	return f
}

func (f *discussionReturnFixture) capture(t *testing.T, m domain.Message) (store.MemberToolScope, domain.ChatCandidate) {
	t.Helper()
	scope := f.claimWork(t, m.Deliveries[0].ID)
	c, err := f.engine.CaptureChatCandidate(t.Context(), scope)
	roleOK(t, err)
	f.acceptWork(t, scope)
	return scope, c
}

func (f *discussionReturnFixture) admit(t *testing.T) domain.ChatAssessmentReceipt {
	t.Helper()
	scope, c := f.capture(t, f.invite)
	a, err := f.engine.AssessChatIntent(t.Context(), scope, c.ID, domain.ChatAssessment{
		Disposition: "prepare_work", Action: domain.ChatActionReadOnlyInvestigation, Reason: "按明确配置的范围调查"}, "已记录调查范围。", "admit")
	roleOK(t, err)
	if a.Outcome != "admitted" || a.SourceKind != "member" || a.MessageID != f.invite.ID {
		t.Fatalf("fixture did not admit from the exact invitation: %s/%s", a.Outcome, a.ReasonCode)
	}
	f.settleWork(t, scope, true)
	return a
}

func queuedFromMessage(m domain.Message) store.PendingDelivery {
	return store.PendingDelivery{GroupID: m.GroupID, SenderID: m.SenderID, Delivery: m.Deliveries[0]}
}

func (f *discussionReturnFixture) savedReturn(t *testing.T, handbackID string) (domain.ChatDiscussionWorkReturnReceipt, domain.Message) {
	t.Helper()
	var id string
	// Fixture only: locate the delivery, then use the public typed routing read.
	roleOK(t, f.raw.QueryRow(`SELECT delivery_id FROM chat_discussion_work_returns WHERE handback_id=?`, handbackID).Scan(&id))
	r, err := f.db.ResolveChatDiscussionWorkReturnDelivery(t.Context(), f.group, id)
	roleOK(t, err)
	m, err := f.db.GetMessage(t.Context(), f.group, r.MessageID)
	roleOK(t, err)
	if r.RecipientID != f.inviter.ID || r.OriginMessageID != f.origin.ID || r.InvitationMessageID != f.invite.ID ||
		len(m.Deliveries) != 1 || m.SourceKind != "unknown" || m.SourceID != "" || m.Discussion != nil {
		t.Fatal("fixture lacks a typed return to the exact original inviter")
	}
	return r, m
}

func (f *discussionReturnFixture) seedReturn(t *testing.T) (domain.ChatDiscussionWorkReturnReceipt, domain.Message) {
	t.Helper()
	ctx := t.Context()
	a := f.admit(t)
	decision := f.decideInvestigationFixture(t, a, "assign")
	assigned, err := f.db.MaterializeAssignment(ctx, decision.Assignment.ID, 1, a.CreatedAt)
	roleOK(t, err)
	initial := f.claim(t, assigned.Delivery.DeliveryID)
	scope, err := f.db.BindMemberTool(ctx, initial.claim)
	roleOK(t, err)
	owner := f.bindInvestigationOwner(t, store.CoordinationToolScope{Member: scope, Provenance: initial.provenance})
	f.acceptWork(t, scope)
	h, err := f.engine.HandbackWork(ctx, owner.Member, domain.HandbackWorkInput{Summary: "材料不足，暂不能完成调查。"}, "seed-return")
	roleOK(t, err)
	f.settleWork(t, scope, true)
	return f.savedReturn(t, h.ID)
}

func TestDiscussionWorkReturnPreparerNoFallback(t *testing.T) {
	f := newDiscussionReturnFixture(t)
	ctx := t.Context()
	r, message := f.seedReturn(t)

	private, cli := t.TempDir(), filepath.Join(t.TempDir(), "metadata-only-pi")
	roleOK(t, os.WriteFile(cli, nil, 0700))
	prep, err := platform.NewPreparer(f.db, platform.PrepareOptions{PrivateWorkspaceRoot: private,
		Providers: []provider.Provider{{ID: f.worker.ProviderID, Name: provider.Pi, Version: "1.4.0", Installation: provider.Installed, Path: cli}}})
	roleOK(t, err)
	// Same text, sender and topic are not routing proof. The generic projector
	// labels both messages unknown in this fixture.
	ordinary, err := f.db.CreateMessage(ctx, store.MessageRequest{GroupID: f.group, TopicID: r.TopicID,
		ActorID: f.worker.ID, Content: message.Content, RecipientIDs: []string{f.inviter.ID}}, []string{f.inviter.ID})
	roleOK(t, err)
	if p, err := prep.Prepare(ctx, queuedFromMessage(ordinary)); err != nil || p.Executor == nil {
		t.Fatalf("ordinary control did not keep its normal path: %v", err)
	}
	if p, err := prep.Prepare(ctx, queuedFromMessage(message)); p.Executor != nil || !errors.Is(err, scheduler.ErrNotReady) {
		t.Errorf("typed route fell back to ordinary preparation without opt-in: executor=%T error=%v", p.Executor, err)
	}
	// Damage only the exact A relation's root evidence, leaving the return
	// message and its ordinary queue identity untouched.
	_, err = f.raw.Exec(`DROP TRIGGER message_command_sources_delete`)
	roleOK(t, err)
	_, err = f.raw.Exec(`DELETE FROM message_command_sources WHERE message_id=?`, f.origin.ID)
	roleOK(t, err)
	if _, err := f.db.ResolveChatDiscussionWorkReturnDelivery(ctx, f.group, r.DeliveryID); !errors.Is(err, store.ErrMemberToolDenied) {
		t.Fatalf("fixture did not damage the typed read: %v", err)
	}
	if p, err := prep.Prepare(ctx, queuedFromMessage(message)); p.Executor != nil || !errors.Is(err, store.ErrMemberToolDenied) {
		t.Errorf("a damaged return entered ordinary preparation: executor=%T error=%v", p.Executor, err)
	}
	// Also distinguish a stopped resolver from marker absence.
	_, err = f.raw.Exec(`DELETE FROM schema_migrations WHERE version=31`)
	roleOK(t, err)
	if p, err := prep.Prepare(ctx, queuedFromMessage(message)); p.Executor != nil || !errors.Is(err, store.ErrConflict) {
		t.Errorf("schema error fell back to ordinary preparation: executor=%T error=%v", p.Executor, err)
	}
	f.expectReturnQueued(t, r)
}

func (f *discussionReturnFixture) expectReturnQueued(t *testing.T, r domain.ChatDiscussionWorkReturnReceipt) {
	t.Helper()
	m, err := f.db.GetMessage(t.Context(), r.GroupID, r.MessageID)
	roleOK(t, err)
	if len(m.Deliveries) != 1 || m.Deliveries[0].ID != r.DeliveryID || m.Deliveries[0].State != domain.DeliveryStateQueued || m.Deliveries[0].TurnID != "" {
		t.Fatal("probing the typed return changed the queue")
	}
}
