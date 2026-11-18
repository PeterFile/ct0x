package domain

import "time"

// TaskWorkInput is an immutable supplement to the original Mandate, not a Task
// revision, a delivery, or permission to execute. ReplyMessageID identifies the
// original user message in ThreadRef, not the platform's notice. SourceSequence
// is source commit order; CreatedAt and constraint revisions record routing
// order. Each changed quote supplements, rather than replaces, prior quotes.
type TaskWorkInput struct {
	ID string `json:"id"`
	ThreadRef
	TaskID                   string                    `json:"task_id"`
	TaskRevision             int64                     `json:"task_revision"`
	MandateID                string                    `json:"mandate_id"`
	AssignmentID             string                    `json:"assignment_id"`
	WorkerMemberID           string                    `json:"worker_member_id"`
	TriggerID                string                    `json:"trigger_id"`
	SourceID                 string                    `json:"source_id"`
	SourceSequence           int64                     `json:"source_sequence"`
	CandidateID              string                    `json:"candidate_id"`
	ReplyMessageID           string                    `json:"reply_message_id"`
	Content                  string                    `json:"content"` // Original user text, not new authority.
	NoticeMessageID          string                    `json:"notice_message_id"`
	BeforeConstraintRevision int64                     `json:"before_constraint_revision"`
	BeforeConstraint         string                    `json:"before_constraint"`
	AfterConstraintRevision  int64                     `json:"after_constraint_revision"`
	AfterConstraint          string                    `json:"after_constraint"`
	CreatedAt                time.Time                 `json:"created_at"`
	Consumption              *TaskWorkInputConsumption `json:"consumption,omitempty"`
	Return                   *TaskWorkInputReturn      `json:"return,omitempty"`
}

// TaskWorkInputConsumption records explicit handling by the exact Attempt and
// Turn. This is neither approval nor a fact inferred from delivery or task state.
type TaskWorkInputConsumption struct {
	TurnID     string    `json:"turn_id"`
	AttemptID  string    `json:"attempt_id"`
	ConsumedAt time.Time `json:"consumed_at"`
}

// TaskWorkInputReturn preserves an out-of-scope disposition separately from
// confirmed processing. It does not change any Mandate, Attempt or Turn.
type TaskWorkInputReturn struct {
	MemberID        string    `json:"member_id"`
	TurnID          string    `json:"turn_id"`
	AttemptID       string    `json:"attempt_id"`
	Reason          string    `json:"reason"`
	Question        string    `json:"question"`
	NoticeMessageID string    `json:"notice_message_id"`
	ReturnedAt      time.Time `json:"returned_at"`
}
