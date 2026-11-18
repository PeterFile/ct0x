package domain

type TaskWorkSourceKind string

const (
	TaskWorkSourceFollowup   TaskWorkSourceKind = "followup"
	TaskWorkSourceReport     TaskWorkSourceKind = "report"
	TaskWorkSourceDelegation TaskWorkSourceKind = "delegation"
	// Handback is a non-waking return, never authority to continue or reopen a Task.
	TaskWorkSourceHandback TaskWorkSourceKind = "handback"
)

func (k TaskWorkSourceKind) IsOwnerContinuation() bool {
	return k == TaskWorkSourceFollowup || k == TaskWorkSourceReport
}

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
