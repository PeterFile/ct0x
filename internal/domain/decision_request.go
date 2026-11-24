package domain

import "time"

type DecisionRequestKind string

const (
	DecisionRequestDecision      DecisionRequestKind = "decision"
	DecisionRequestAuthorization DecisionRequestKind = "authorization"
	DecisionRequestUnblock       DecisionRequestKind = "unblock"
	DecisionRequestClarification DecisionRequestKind = "clarification" // Set only by the native chat intake path.
)

type DecisionRelay string

const (
	DecisionRelayTaskOwner    DecisionRelay = "task_owner"
	DecisionRelayOriginThread DecisionRelay = "origin_thread"
)

// DecisionContext describes the question, not verified execution results.
// Choices are display labels in this version's order, not durable identifiers.
type DecisionContext struct {
	Facts          string   `json:"facts"`
	Recommendation string   `json:"recommendation"`
	Choices        []string `json:"choices,omitempty"`
}

type DecisionQuestion struct {
	DecisionContext
	Question      string                 `json:"question"`
	Authorization *DecisionAuthorization `json:"authorization,omitempty"`
}

// DecisionAuthorization describes proposed scope and impact. It grants neither.
type DecisionAuthorization struct {
	Scope  string `json:"scope"`
	Impact string `json:"impact"`
}

// TaskDecisionRequestInput deliberately contains no Task, actor or recipient.
// Kind is the issue slot: one per kind per exact work generation. Zero
// ExpectedVersion opens a slot or coalesces an identical question; a material
// amendment requires the exact current version. A nil expiry has no deadline.
type TaskDecisionRequestInput struct {
	Kind            DecisionRequestKind `json:"kind"`
	ExpectedVersion int64               `json:"expected_version"`
	ExpiresAt       *time.Time          `json:"expires_at,omitempty"`
	DecisionQuestion
}

type DecisionTaskRef struct {
	TaskID              string `json:"task_id"`
	TaskTitle           string `json:"task_title"`
	TaskRevision        int64  `json:"task_revision"`
	AssignmentID        string `json:"assignment_id"`
	MandateID           string `json:"mandate_id"`
	AuthorityGeneration int64  `json:"authority_generation"`
	OwnerMemberID       string `json:"owner_member_id"`
	OriginTriggerID     string `json:"origin_trigger_id"`
}

type DecisionChatProof struct {
	SourceID        string `json:"source_id"`
	CandidateID     string `json:"candidate_id"`
	AssessmentID    string `json:"assessment_id"`
	NoticeMessageID string `json:"notice_message_id"`
}

// DecisionRequest is a single current question version. ThreadRef is the real
// origin in both modes; exactly one of Task and Chat supplies the binding proof.
// Relay describes where a future answer belongs, not a delivery or grant.
type DecisionRequest struct {
	ID      string              `json:"id"`
	Version int64               `json:"version"`
	Kind    DecisionRequestKind `json:"kind"`
	ThreadRef
	GroupName       string             `json:"group_name"`
	RecipientID     string             `json:"recipient_id"`
	Relay           DecisionRelay      `json:"relay"`
	Task            *DecisionTaskRef   `json:"task,omitempty"`
	Chat            *DecisionChatProof `json:"chat,omitempty"`
	SourceMessageID string             `json:"source_message_id"`
	SourceSequence  int64              `json:"source_sequence"`
	AuthorMemberID  string             `json:"author_member_id"`
	AuthorTurnID    string             `json:"author_turn_id"`
	AuthorAttemptID string             `json:"author_attempt_id,omitempty"`
	DecisionQuestion
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}
