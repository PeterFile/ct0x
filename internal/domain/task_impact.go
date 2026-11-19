package domain

import "time"

type TaskImpactKind string

const (
	TaskImpactConstraintConfirmed TaskImpactKind = "constraint_confirmed"
	TaskImpactWorkInputReturned   TaskImpactKind = "work_input_returned"
	TaskImpactStaleDelegation     TaskImpactKind = "stale_delegation"
	TaskImpactStaleWorkReport     TaskImpactKind = "stale_work_report"
	TaskImpactStaleSubmission     TaskImpactKind = "stale_submission"
	TaskImpactStalePin            TaskImpactKind = "stale_pinned_dependency"
	TaskImpactCancelRequested     TaskImpactKind = "cancel_requested"
)

// TaskImpactSource identifies the proven original user input and its admitted
// owner scope. SourceSequence is the original message's commit sequence (the
// WorkInput's SourceSequence), not the order of proposals or acknowledgements.
// It is not an authority to answer, replace a pin, or start work.
type TaskImpactSource struct {
	ThreadRef
	InputID             string `json:"input_id"`
	MessageID           string `json:"message_id"`
	SourceSequence      int64  `json:"source_sequence"`
	SourceID            string `json:"source_id"`
	TriggerID           string `json:"trigger_id"`
	TaskID              string `json:"task_id"`
	TaskRevision        int64  `json:"task_revision"`
	AssignmentID        string `json:"assignment_id"`
	MandateID           string `json:"mandate_id"`
	AuthorityGeneration int64  `json:"authority_generation"`
	OwnerMemberID       string `json:"owner_member_id"`
}

// Before and after are this WorkInput's original proposal snapshots, numbered
// by assessment persistence. Neither is an effective-constraint view or a
// replacement of another input. Acceptance is from the immutable Mandate.
type TaskImpactPlan struct {
	Acceptance               string `json:"acceptance"`
	BeforeConstraintRevision int64  `json:"before_constraint_revision"`
	BeforeConstraint         string `json:"before_constraint"`
	AfterConstraintRevision  int64  `json:"after_constraint_revision"`
	AfterConstraint          string `json:"after_constraint"`
	NoticeMessageID          string `json:"notice_message_id"`
}

type TaskImpactDelivery struct {
	DeliveryID string        `json:"delivery_id"`
	MessageID  string        `json:"message_id"`
	MemberID   string        `json:"member_id"`
	State      DeliveryState `json:"state"`
	AcceptedAt *time.Time    `json:"accepted_at,omitempty"`
}

// A pin remains immutable. ReviewID is the decision about that exact old
// Submission at impact time, not a choice about replacing the pin.
type TaskImpactPin struct {
	TaskArtifactDependency
	DependencyRevision   int64         `json:"dependency_revision"`
	ReviewID             string        `json:"review_id,omitempty"`
	ReviewOutcome        ReviewOutcome `json:"review_outcome,omitempty"`
	AffectedSubmissionID string        `json:"affected_submission_id,omitempty"`
}

type TaskImpactSubmission struct {
	SubmissionID string `json:"submission_id"`
	TaskID       string `json:"task_id"`
	TaskRevision int64  `json:"task_revision"`
}

// A concrete handoff is the persisted scoped work-input return, not a blocked
// Task. ScopeCurrent is for display only. F must recheck authority and decision
// state on any future write, and must not turn a reply into an Agent assessment.
type TaskImpactDecision struct {
	TaskWorkInputReturn
	ScopeCurrent bool `json:"scope_current"`
}

type TaskImpactCancellation struct {
	TurnID      string    `json:"turn_id"`
	RequestedAt time.Time `json:"requested_at"`
	State       TurnState `json:"state"`
}

// TaskImpact is one bounded history entry for the selected Task. Source may
// identify another Task in the same group. EvidenceID identifies an existing
// report, delegation, submission, pin prerequisite, or turn, according to Kind.
// CurrentTaskStatus is a separate live projection, never a success receipt.
type TaskImpact struct {
	ID                string                    `json:"id"`
	Kind              TaskImpactKind            `json:"kind"`
	TaskID            string                    `json:"task_id"`
	TaskRevision      int64                     `json:"task_revision"`
	CurrentTaskStatus TaskStatus                `json:"current_task_status"`
	CreatedAt         time.Time                 `json:"created_at"`
	Reason            string                    `json:"reason"`
	Source            TaskImpactSource          `json:"source"`
	EvidenceID        string                    `json:"evidence_id,omitempty"`
	Acknowledgement   *TaskWorkInputConsumption `json:"acknowledgement,omitempty"`
	Plan              *TaskImpactPlan           `json:"plan,omitempty"`
	Delivery          *TaskImpactDelivery       `json:"delivery,omitempty"`
	Pin               *TaskImpactPin            `json:"pin,omitempty"`
	Submission        *TaskImpactSubmission     `json:"submission,omitempty"`
	Decision          *TaskImpactDecision       `json:"decision,omitempty"`
	Cancellation      *TaskImpactCancellation   `json:"cancellation,omitempty"`
}
