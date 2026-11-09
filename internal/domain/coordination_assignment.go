package domain

import "time"

// Assignment is an immutable binding; authority snapshots come from its request.
// DecisionID is reserved opaque provenance until the decision schema is installed.
type Assignment struct {
	ID             string    `json:"id"`
	RequestID      string    `json:"request_id"`
	DecisionID     string    `json:"decision_id"`
	TaskID         string    `json:"task_id"`
	GroupID        string    `json:"group_id"`
	WorkerMemberID string    `json:"worker_member_id"`
	CreatedAt      time.Time `json:"created_at"`
}

type AssignmentDispatchStatus string

const (
	AssignmentDispatchStatusPending  AssignmentDispatchStatus = "pending"
	AssignmentDispatchStatusEnqueued AssignmentDispatchStatus = "enqueued"
	AssignmentDispatchStatusHeld     AssignmentDispatchStatus = "held"
)

// Other local failure classifications reuse the applicable Dispatch error codes.
const AssignmentHeldWorkerInactive = "worker_inactive"

// AssignmentDispatchState describes durable intent, never native execution state.
type AssignmentDispatchState struct {
	AssignmentID  string                     `json:"assignment_id"`
	Generation    int64                      `json:"generation"`
	Status        AssignmentDispatchStatus   `json:"status"`
	AttemptCount  int64                      `json:"attempt_count"`
	NextAttemptAt time.Time                  `json:"next_attempt_at"`
	UpdatedAt     time.Time                  `json:"updated_at"`
	LastErrorCode string                     `json:"last_error_code"`
	HeldReason    string                     `json:"held_reason"`
	Delivery      *AssignmentDeliveryReceipt `json:"delivery"`
}

type AssignmentDeliveryReceipt struct {
	MessageID                  string    `json:"message_id"`
	DeliveryID                 string    `json:"delivery_id"`
	ConversationID             string    `json:"conversation_id"`
	WorkerMemberID             string    `json:"worker_member_id"`
	MaterializedFromGeneration int64     `json:"materialized_from_generation"`
	CreatedAt                  time.Time `json:"created_at"`
}

// AssignmentAttemptBinding records identity, not an execution or acceptance result.
type AssignmentAttemptBinding struct {
	AssignmentID   string    `json:"assignment_id"`
	GroupID        string    `json:"group_id"`
	TaskID         string    `json:"task_id"`
	WorkerMemberID string    `json:"worker_member_id"`
	TurnID         string    `json:"turn_id"`
	AttemptID      string    `json:"attempt_id"`
	TaskRevision   int64     `json:"task_revision"`
	CreatedAt      time.Time `json:"created_at"`
}
