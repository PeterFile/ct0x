package domain

import "time"

// DispatchExecution is a current read-only sample, not authority or task acceptance.
type DispatchExecution struct {
	GroupID                  string                     `json:"group_id"`
	TaskID                   string                     `json:"task_id"`
	RequestID                string                     `json:"request_id"`
	DispatchStatus           DispatchStatus             `json:"dispatch_status"`
	CoordinatorTurn          *DispatchExecutionTurn     `json:"coordinator_turn"`
	Decision                 *DispatchExecutionDecision `json:"decision"`
	Assignment               *Assignment                `json:"assignment"`
	AssignmentDispatchStatus *AssignmentDispatchStatus  `json:"assignment_dispatch_status"`
	Binding                  *AssignmentAttemptBinding  `json:"binding"`
	WorkerTurn               *DispatchExecutionTurn     `json:"worker_turn"`
}

type DispatchExecutionDecision struct {
	ID                string                  `json:"id"`
	Kind              CoordinatorDecisionKind `json:"kind"`
	CoordinatorTurnID string                  `json:"coordinator_turn_id"`
	CreatedAt         time.Time               `json:"created_at"`
}

type DispatchExecutionTurn struct {
	ID                string                    `json:"id"`
	GroupID           string                    `json:"group_id"`
	DeliveryID        string                    `json:"delivery_id"`
	ConversationID    string                    `json:"conversation_id"`
	MemberID          string                    `json:"member_id"`
	State             TurnState                 `json:"state"`
	NativeSessionID   string                    `json:"native_session_id"`
	AcceptedAt        *time.Time                `json:"accepted_at"`
	CancelRequested   bool                      `json:"cancel_requested"`
	CancelRequestedAt *time.Time                `json:"cancel_requested_at"`
	CreatedAt         time.Time                 `json:"created_at"`
	UpdatedAt         time.Time                 `json:"updated_at"`
	Receipt           *DispatchExecutionReceipt `json:"receipt"`
}

// Receipt fields observe this invocation only. Null and false do not disprove execution.
// Native terminal evidence and direct-child reaping do not prove descendant shutdown.
type DispatchExecutionReceipt struct {
	Started           bool      `json:"started"`
	Accepted          bool      `json:"accepted"`
	Outcome           TurnState `json:"outcome"`
	Terminal          TurnState `json:"terminal"`
	DeliveryFailed    bool      `json:"delivery_failed"`
	NativeSessionID   string    `json:"native_session_id"`
	DirectChildReaped bool      `json:"direct_child_reaped"`
	OutputComplete    bool      `json:"output_complete"`
	ExitCode          int       `json:"exit_code"`
	LogsTruncated     bool      `json:"logs_truncated"`
}
