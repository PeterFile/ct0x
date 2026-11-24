package platform

import (
	"context"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// Freeze only stable projection and route facts, not message text, status or
// permissions. A projection of a reply (which also names an invitation), a
// generic member command, and an unknown source are all insufficient.
type investigationInvitation struct {
	domain.ThreadRef
	messageID, sourceID, deliveryID, conversationID string
	projection                                      domain.MessageDiscussion
}

func investigationInvitationOf(m domain.Message) investigationInvitation {
	discussion := m.Discussion
	if m.SourceKind != "member" || !memberToolRuntimeText(m.SourceID) || discussion == nil || discussion.Kind != domain.ChatDiscussionInvite ||
		discussion.InvitationMessageID != m.ID || discussion.RootMessageID == m.ID || !memberToolRuntimeText(discussion.RootMessageID) ||
		discussion.InviterID != m.SenderID || m.SenderID == "user" || !memberToolRuntimeText(m.SenderID) || len(m.Deliveries) != 1 {
		return investigationInvitation{}
	}
	d := m.Deliveries[0]
	if d.MessageID != m.ID || d.RecipientID != discussion.AssigneeID || d.RecipientID == m.SenderID || d.RecipientID == "user" ||
		!memberToolRuntimeText(d.ID) || !memberToolRuntimeText(d.RecipientID) || !memberToolRuntimeText(d.ConversationID) {
		return investigationInvitation{}
	}
	return investigationInvitation{ThreadRef: domain.ThreadRef{GroupID: m.GroupID, TopicID: m.TopicID},
		messageID: m.ID, sourceID: m.SourceID, deliveryID: d.ID, conversationID: d.ConversationID, projection: *discussion}
}

func (i investigationInvitation) matchesSource(thread domain.ThreadRef, messageID, kind, sourceID, senderID, recipientID string) bool {
	return i.messageID != "" && i.ThreadRef == thread && i.messageID == messageID && kind == "member" && i.sourceID == sourceID &&
		i.projection.InviterID == senderID && i.projection.AssigneeID == recipientID
}

// After CaptureChatCandidate, bind the issued schema to that live, exact
// arrival. B will independently revalidate source, limits and policy when an
// assessment is applied; this proof is not an admission.
func (i investigationInvitation) checkCapture(ctx context.Context, db *store.Store, c domain.ChatCandidate, scope store.MemberToolScope) error {
	if !i.matchesSource(c.ThreadRef, c.MessageID, c.SourceKind, c.SourceID, c.SourceSenderID, c.MemberID) ||
		c.GroupID != scope.Claim.GroupID || c.TopicID != scope.TopicID || c.MemberID != scope.MemberID || c.TurnID != scope.Claim.TurnID ||
		c.Purpose != domain.ChatIntakePurpose || c.ID == "" || c.Generation <= 0 || c.RepairSource != nil || i.conversationID != scope.ConversationID {
		return store.ErrMemberToolDenied
	}
	m, err := db.GetMessage(ctx, c.GroupID, c.MessageID)
	if err != nil {
		return investigationReadError(ctx, err)
	}
	if investigationInvitationOf(m) != i {
		return store.ErrMemberToolDenied
	}
	d := m.Deliveries[0]
	turn, err := db.GetTurn(ctx, c.GroupID, c.TurnID)
	if err != nil {
		return investigationReadError(ctx, err)
	}
	if d.State != domain.DeliveryStateQueued || d.Error != "" || d.TurnID != c.TurnID ||
		turn.ID != c.TurnID || turn.ClaimToken != scope.Claim.Token || turn.DeliveryID != d.ID || turn.State != domain.TurnStateStarting ||
		turn.MemberID != scope.MemberID || turn.ConversationID != scope.ConversationID {
		return store.ErrMemberToolDenied
	}
	return ctx.Err()
}

// GetChatAdmission selects the exact persisted candidate. The DiscussionWork
// projection additionally proves that candidate's receive binding (including
// the Turn and unique delivery), original Mandate trigger, and stable typed
// admission references. Optional projected Assignment fields are not authority;
// the B ready/role/launch checks and write transactions prove live ownership.
func requireInvestigationInvitation(ctx context.Context, db *store.Store, a domain.ChatAssessmentReceipt) (investigationInvitation, error) {
	var empty investigationInvitation
	if a.SourceKind != "member" || a.Purpose != domain.ChatIntakePurpose || a.Outcome != "admitted" ||
		a.Disposition != "prepare_work" || a.Action != domain.ChatActionReadOnlyInvestigation || a.Authority != "written_preauthorization" ||
		!a.AuthorityEvidenceVerified || a.DiscussionOnly || a.DiscussionOnlyExcerpt != "" || a.WorkspaceScope != nil || a.RepairLink != nil ||
		a.CandidateID == "" || a.ID == "" || a.TurnID == "" {
		return empty, errInvestigationFirstRoot
	}
	m, err := db.GetMessage(ctx, a.GroupID, a.MessageID)
	if err != nil {
		return empty, investigationReadError(ctx, err)
	}
	i := investigationInvitationOf(m)
	w := m.DiscussionWork
	if !i.matchesSource(a.ThreadRef, a.MessageID, a.SourceKind, a.SourceID, a.SourceSenderID, a.AssessedBy) ||
		m.Deliveries[0].TurnID != a.TurnID || w == nil || w.TaskID != a.TaskID || w.MandateID != a.MandateID || w.DispatchRequestID != a.DispatchRequestID {
		return empty, errInvestigationFirstRoot
	}
	return i, ctx.Err()
}
