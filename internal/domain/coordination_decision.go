package domain

import "time"

type CoordinatorDecisionKind string

const (
	CoordinatorDecisionKindAssign  CoordinatorDecisionKind = "assign"
	CoordinatorDecisionKindDecline CoordinatorDecisionKind = "decline"
	DispatchStatusAssigned         DispatchStatus          = "assigned"
	DispatchStatusDeclined         DispatchStatus          = "declined"
)

type CoordinatorDecision struct {
	ID                  string                  `json:"id"`
	RequestID           string                  `json:"request_id"`
	TaskID              string                  `json:"task_id"`
	GroupID             string                  `json:"group_id"`
	CoordinatorMemberID string                  `json:"coordinator_member_id"`
	CoordinatorTurnID   string                  `json:"coordinator_turn_id"`
	Kind                CoordinatorDecisionKind `json:"kind"`
	WorkerMemberID      string                  `json:"worker_member_id"`
	AssignmentID        string                  `json:"assignment_id"`
	Reason              string                  `json:"reason"`
	CreatedAt           time.Time               `json:"created_at"`
}

type DecisionReceipt struct {
	Decision   CoordinatorDecision `json:"decision"`
	Assignment *Assignment         `json:"assignment"`
}

type AssignmentView struct {
	Assignment Assignment                `json:"assignment"`
	Decision   CoordinatorDecision       `json:"decision"`
	Request    DispatchRequest           `json:"request"`
	Dispatch   AssignmentDispatchState   `json:"dispatch"`
	Binding    *AssignmentAttemptBinding `json:"binding"`
	Task       Task                      `json:"task"`
}
