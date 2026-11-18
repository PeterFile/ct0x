package domain

const (
	ChatDiscussionInvite  = "invite"
	ChatDiscussionReply   = "reply"
	ChatDiscussionResolve = "resolve"
)

// ChatDiscussionAction never selects the source, caller, or return route.
// Invite requires a recipient; ExpectedReply optionally refines its question
// and is limited to 1024 bytes as a JSON string, including quotes and escapes.
// When omitted, the invitation's exact saved Content is the reply requirement.
type ChatDiscussionAction struct {
	Kind          string `json:"kind"`
	Content       string `json:"content"`
	RecipientID   string `json:"recipient_id,omitempty"`
	ExpectedReply string `json:"expected_reply,omitempty"`
}

// ChatDiscussionEvent records one step. SourceMessageID is the arriving message
// consumed by this step. InvitationMessageID is the new invitation on invite,
// the answered invitation on reply, or the root invitation on resolve (if any).
// RecipientID is the intended target; an empty DeliveryID means no wake was sent.
// An invite with no ExpectedReply uses the exact question at MessageID.
type ChatDiscussionEvent struct {
	Kind                      string `json:"kind"`
	OriginMessageID           string `json:"origin_message_id"`
	SourceMessageID           string `json:"source_message_id"`
	MessageID                 string `json:"message_id"`
	ActorID                   string `json:"actor_id"`
	RecipientID               string `json:"recipient_id,omitempty"`
	DeliveryID                string `json:"delivery_id,omitempty"`
	InvitationMessageID       string `json:"invitation_message_id,omitempty"`
	ParentInvitationMessageID string `json:"parent_invitation_message_id,omitempty"`
	ExpectedReply             string `json:"expected_reply,omitempty"`
}

// ChatDiscussionReceipt is the immutable publication snapshot, not live
// delivery state. GetMessage can read delivery progress separately.
// Tool write responses should project the persisted relation and reference IDs,
// not echo the Message body.
type ChatDiscussionReceipt struct {
	Message Message             `json:"message"`
	Event   ChatDiscussionEvent `json:"event"`
}

// ChatDiscussionContext is scoped to an arriving turn. A new action needs the
// current head and owner; an older arrival can still read its associated path.
// After a root answer, the owner can resolve but no invitation is active.
// Closed also covers a public-only return or a departed recipient's delivery
// known not to have started; only the latter has ClosureReason.
// When ContentOmitted is true, read full bodies via read_history using
// OriginMessageID and each event's MessageID. Independent reply requirements
// remain inline even when the bodies are omitted.
// Events retain the publication snapshots' routing facts, not live delivery state.
type ChatDiscussionContext struct {
	ThreadRef
	OriginMessageID           string                  `json:"origin_message_id"`
	OriginContent             string                  `json:"origin_content"`
	RootInviterID             string                  `json:"root_inviter_id"`
	IncomingMessageID         string                  `json:"incoming_message_id"`
	HeadMessageID             string                  `json:"head_message_id"`
	ActiveInvitationMessageID string                  `json:"active_invitation_message_id,omitempty"`
	ActiveOwnerID             string                  `json:"active_owner_id,omitempty"`
	Closed                    bool                    `json:"closed"`
	ClosureReason             string                  `json:"closure_reason,omitempty"`
	ContentOmitted            bool                    `json:"content_omitted,omitempty"`
	Events                    []ChatDiscussionReceipt `json:"events"`
}
