package domain

type TaskWorkSourceKind string

const (
	TaskWorkSourceFollowup TaskWorkSourceKind = "followup"

	// Reserved for future report types; B1 neither writes nor resolves them.
	TaskWorkSourceReport     TaskWorkSourceKind = "report"
	TaskWorkSourceDelegation TaskWorkSourceKind = "delegation"
	TaskWorkSourceHandback   TaskWorkSourceKind = "handback"
)

// TaskWorkDelivery is internal provenance, not a transport DTO or grant.
// Blocked is a current startup observation, not part of the immutable identity.
type TaskWorkDelivery struct {
	SourceKind                                              TaskWorkSourceKind `json:"-"`
	SourceID, AssignmentID, TaskID, MandateID               string             `json:"-"`
	TaskRevision, AuthorityGeneration                       int64              `json:"-"`
	OwnerMemberID, MemberID, SenderMemberID                 string             `json:"-"`
	GroupID, TopicID, ConversationID, DeliveryID, MessageID string             `json:"-"`
	Blocked                                                 string             `json:"-"`
}
