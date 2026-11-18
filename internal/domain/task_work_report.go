package domain

import "time"

const (
	MaxWorkQuestionBytes        = 4096
	MaxWorkReportBytes          = 8192
	MaxWorkReportsPerDelegation = 4 // The last slot is reserved for a waking report.
)

// DelegateWorkInput grants nothing when the optional material selectors are omitted.
type DelegateWorkInput struct {
	MemberID           string               `json:"member_id"`
	Question           string               `json:"question"`
	AllowTopicMessages bool                 `json:"allow_topic_messages,omitempty"`
	ArtifactVersions   []ArtifactVersionRef `json:"artifact_versions,omitempty"`
}

type TaskWorkReportKind string

const (
	TaskWorkReportProgress  TaskWorkReportKind = "progress"
	TaskWorkReportQuestion  TaskWorkReportKind = "question"
	TaskWorkReportBlocked   TaskWorkReportKind = "blocked"
	TaskWorkReportDelivered TaskWorkReportKind = "delivered"
)

type ReportWorkInput struct {
	Kind     TaskWorkReportKind `json:"kind"`
	Summary  string             `json:"summary"`
	AnswerTo string             `json:"answer_to,omitempty"`
}

// TaskWorkParent identifies the original assignment and authority, never a grant.
type TaskWorkParent struct {
	ThreadRef
	TaskID              string `json:"task_id"`
	TaskRevision        int64  `json:"task_revision"`
	AssignmentID        string `json:"assignment_id"`
	MandateID           string `json:"mandate_id"`
	AuthorityGeneration int64  `json:"authority_generation"`
	OwnerMemberID       string `json:"owner_member_id"`
	OriginalDeliveryID  string `json:"original_delivery_id"`
	OriginalMessageID   string `json:"original_message_id"`
}

type TaskWorkDelegation struct {
	TaskWorkParent
	DelegateWorkInput
	ID               string    `json:"id"`
	AuthorTurnID     string    `json:"author_turn_id"`
	AuthorAttemptID  string    `json:"author_attempt_id"`
	AuthorDeliveryID string    `json:"author_delivery_id"`
	AuthorMessageID  string    `json:"author_message_id"`
	MaxDelegations   int       `json:"max_delegations"`
	Ordinal          int       `json:"ordinal"`
	DeliveryID       string    `json:"delivery_id"`
	MessageID        string    `json:"message_id"`
	ConversationID   string    `json:"conversation_id"`
	CreatedAt        time.Time `json:"created_at"`
}

// TaskWorkReport is an immutable report plus wake references. SourceReplyMessageID
// is a projection: the native turn may publish its own separate reply later.
type TaskWorkReport struct {
	TaskWorkParent
	ReportWorkInput
	ID                   string    `json:"id"`
	DelegationID         string    `json:"delegation_id"`
	ActorID              string    `json:"actor_id"`
	SourceTurnID         string    `json:"source_turn_id"`
	SourceDeliveryID     string    `json:"source_delivery_id"`
	SourceMessageID      string    `json:"source_message_id"`
	SourceReplyMessageID string    `json:"source_reply_message_id,omitempty"`
	DeliveryID           string    `json:"delivery_id,omitempty"`
	MessageID            string    `json:"message_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

type HandbackWorkInput struct {
	Summary string `json:"summary"`
}

// TaskWorkHandback is a non-waking notice to the bound coordinator. It does not
// transfer ownership, change the assignment, or authorize further execution.
type TaskWorkHandback struct {
	TaskWorkParent
	HandbackWorkInput
	ID              string    `json:"id"`
	AuthorTurnID    string    `json:"author_turn_id"`
	AuthorAttemptID string    `json:"author_attempt_id"`
	CoordinatorID   string    `json:"coordinator_member_id"`
	NoticeMessageID string    `json:"notice_message_id"`
	CreatedAt       time.Time `json:"created_at"`
}
