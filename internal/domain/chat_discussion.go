package domain

const (
	ChatDiscussionInvite  = "invite"
	ChatDiscussionReply   = "reply"
	ChatDiscussionResolve = "resolve"
)

// ChatDiscussionAction never selects the source, caller, or return route.
// Invite requires exactly one of RecipientID or CreateMember. ExpectedReply
// optionally refines its question and is limited to 1024 bytes as a JSON string,
// including quotes and escapes. When omitted, the invitation's exact saved
// Content is the reply requirement. Reply and resolve accept only Kind and Content.
type ChatDiscussionAction struct {
	Kind          string                      `json:"kind"`
	Content       string                      `json:"content"`
	RecipientID   string                      `json:"recipient_id,omitempty"`
	ExpectedReply string                      `json:"expected_reply,omitempty"`
	CreateMember  *ChatDiscussionCreateMember `json:"create_member,omitempty"`
}

// ChatDiscussionCreateMember describes a fallback participant, not its runtime
// configuration, ownership, workspace or grants.
type ChatDiscussionCreateMember struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Reason string `json:"reason"`
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

// ChatDiscussionWork is the admitted work bound to the receiving Turn of the
// current invitation. Assignment and worker are omitted until proved. These
// references do not indicate execution, submission, or acceptance.
type ChatDiscussionWork struct {
	TaskID            string `json:"task_id"`
	MandateID         string `json:"mandate_id"`
	DispatchRequestID string `json:"dispatch_request_id"`
	AssignmentID      string `json:"assignment_id,omitempty"`
	WorkerMemberID    string `json:"worker_member_id,omitempty"`
}

// ChatDiscussionContext is scoped to an arriving turn. A new action needs the
// current head and owner; an older arrival can still read its associated path.
// After a root answer, the owner can resolve but no invitation is active.
// Closed ends the ordinary path, including when WorkAdmitted transfers it to
// admitted work. Only a proven interruption at the current head has ClosureReason.
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
	WorkAdmitted              bool                    `json:"work_admitted,omitempty"`
	ContentOmitted            bool                    `json:"content_omitted,omitempty"`
	Events                    []ChatDiscussionReceipt `json:"events"`
}
