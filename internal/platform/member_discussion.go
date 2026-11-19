package platform

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

// MemberDiscussionInput selects only the action and, for an invite, an existing member.
type MemberDiscussionInput struct {
	Kind          string
	RecipientID   string
	ExpectedReply string
}

// SendDiscussion shares the send_message grant and passes its key through unchanged.
func (s *MemberTools) SendDiscussion(ctx context.Context, token string, input MemberDiscussionInput, content, key string) (domain.ChatDiscussionReceipt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.capabilities[sha256.Sum256([]byte(token))]
	if !entry.discussionRelay() || !slices.Contains(entry.grants.Operations, "send_message") {
		return domain.ChatDiscussionReceipt{}, store.ErrMemberToolDenied
	}
	// Keep revocation excluded through the write. Store checks the current claim,
	// origin and assessment and owns replay before new-action eligibility.
	receipt, err := s.db.ApplyChatDiscussion(ctx, entry.scope, entry.candidate.ID, domain.ChatDiscussionAction{
		Kind: input.Kind, Content: content, RecipientID: input.RecipientID, ExpectedReply: input.ExpectedReply,
	}, key)
	if errors.Is(err, store.ErrChatDiscussionMutationUnknown) {
		return receipt, err // Preserve evidence even if the cause looks like a denial.
	}
	return receipt, memberRoleError(ctx, err)
}

// ReadDiscussionContext omits relay context for ordinary, non-relay capabilities.
func (s *MemberTools) ReadDiscussionContext(ctx context.Context, token string) (*domain.ChatDiscussionContext, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, found := s.capabilities[sha256.Sum256([]byte(token))]
	if !found || !slices.Contains(entry.grants.Operations, "read_channel") {
		return nil, store.ErrMemberToolDenied
	}
	if !entry.discussionRelay() {
		return nil, nil
	}
	result, err := s.db.ReadChatDiscussionContext(ctx, entry.scope, entry.candidate.ID)
	return result, memberRoleError(ctx, err)
}

func (entry memberToolRegistration) discussionRelay() bool {
	return entry.purpose == domain.ChatIntakePurpose && entry.candidate != nil && entry.candidate.RepairSource == nil &&
		entry.intake.piDiscussionRelaySource != "" && entry.intake.piDiscussionRelaySource == entry.candidate.SourceID
}

func discussionRelaySource(trigger domain.Message) string {
	switch trigger.SourceKind {
	case "local_user_chat":
		if trigger.SenderID == "user" {
			return trigger.SourceID
		}
	case "member":
		if d := trigger.Discussion; d != nil && (d.Kind == domain.ChatDiscussionInvite || d.Kind == domain.ChatDiscussionReply) {
			return trigger.SourceID
		}
	}
	return ""
}
