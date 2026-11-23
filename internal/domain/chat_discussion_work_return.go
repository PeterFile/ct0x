package domain

// ChatDiscussionWorkReturnReceipt is the immutable publication and routing
// reference for one TaskWorkHandback returning to its originating discussion.
// DeliveryID and ConversationID are empty if the original inviter had left.
// This is neither a Task/Turn receipt nor authority to start or continue work.
type ChatDiscussionWorkReturnReceipt struct {
	ThreadRef
	OriginMessageID         string
	InvitationMessageID     string
	HandbackID              string
	HandbackNoticeMessageID string
	MessageID               string
	SenderID                string
	RecipientID             string
	DeliveryID              string
	ConversationID          string
}
