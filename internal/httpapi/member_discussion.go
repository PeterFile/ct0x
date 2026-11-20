package httpapi

import (
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
)

// Exact nested names and presence matter. The enclosing transport rejects
// duplicate keys, and memberToolField rejects explicit null.
type memberDiscussionFields map[string]memberToolField[string]

func (f memberDiscussionFields) action() (platform.MemberDiscussionInput, bool) {
	for name := range f {
		switch name {
		case "kind", "recipient_id", "expected_reply":
		default:
			return platform.MemberDiscussionInput{}, false
		}
	}
	kind := f["kind"].value
	recipient, hasRecipient := f["recipient_id"]
	expected, hasExpected := f["expected_reply"]
	switch kind {
	case domain.ChatDiscussionInvite:
		if !hasRecipient || recipient.value == "" {
			return platform.MemberDiscussionInput{}, false
		}
	case domain.ChatDiscussionReply, domain.ChatDiscussionResolve:
		if hasRecipient || hasExpected {
			return platform.MemberDiscussionInput{}, false
		}
	default:
		return platform.MemberDiscussionInput{}, false
	}
	return platform.MemberDiscussionInput{Kind: kind, RecipientID: recipient.value, ExpectedReply: expected.value}, true
}

type memberDiscussionContext struct {
	OriginMessageID           string                   `json:"origin_message_id"`
	IncomingMessageID         string                   `json:"incoming_message_id"`
	HeadMessageID             string                   `json:"head_message_id"`
	RootInviterID             string                   `json:"root_inviter_id"`
	ActiveInvitationMessageID string                   `json:"active_invitation_message_id,omitempty"`
	ActiveOwnerID             string                   `json:"active_owner_id,omitempty"`
	Closed                    bool                     `json:"closed"`
	ClosureReason             string                   `json:"closure_reason,omitempty"`
	IncomingEvent             *memberDiscussionArrival `json:"incoming_event,omitempty"`
	ExpectedReply             string                   `json:"expected_reply,omitempty"`
}

type memberDiscussionArrival struct {
	Kind                      string `json:"kind"`
	SourceMessageID           string `json:"source_message_id"`
	InvitationMessageID       string `json:"invitation_message_id,omitempty"`
	ParentInvitationMessageID string `json:"parent_invitation_message_id,omitempty"`
}

// The Store caps the source context; project only live pointers and the
// arriving relationship. Use read_history for the bound message bodies.
func projectMemberDiscussion(c *domain.ChatDiscussionContext) *memberDiscussionContext {
	if c == nil {
		return nil
	}
	out := &memberDiscussionContext{
		OriginMessageID: c.OriginMessageID, IncomingMessageID: c.IncomingMessageID, HeadMessageID: c.HeadMessageID,
		RootInviterID: c.RootInviterID, ActiveInvitationMessageID: c.ActiveInvitationMessageID,
		ActiveOwnerID: c.ActiveOwnerID, Closed: c.Closed, ClosureReason: c.ClosureReason,
	}
	for _, r := range c.Events {
		e := r.Event
		if e.MessageID == c.IncomingMessageID {
			out.IncomingEvent = &memberDiscussionArrival{Kind: e.Kind, SourceMessageID: e.SourceMessageID,
				InvitationMessageID: e.InvitationMessageID, ParentInvitationMessageID: e.ParentInvitationMessageID}
		}
		if e.Kind == domain.ChatDiscussionInvite && e.MessageID == c.ActiveInvitationMessageID && e.ExpectedReply != r.Message.Content {
			out.ExpectedReply = e.ExpectedReply
		}
	}
	return out
}
