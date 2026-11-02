package domain

import "time"

type DispatchStatus string

const (
	DispatchStatusPending  DispatchStatus = "pending"
	DispatchStatusEnqueued DispatchStatus = "enqueued"
	DispatchStatusHeld     DispatchStatus = "held"
)

// These codes describe local materialization failures, never model execution outcomes.
const (
	DispatchErrorSQLiteBusy   = "sqlite_busy"
	DispatchErrorSQLiteLocked = "sqlite_locked"

	DispatchHeldTaskStale           = "task_stale"
	DispatchHeldAuthorityStale      = "authority_stale"
	DispatchHeldBindingStale        = "binding_stale"
	DispatchHeldPolicyStale         = "policy_stale"
	DispatchHeldCoordinatorInactive = "coordinator_inactive"
	DispatchHeldNoEligibleWorker    = "no_eligible_worker"
	DispatchHeldPayloadTooLarge     = "payload_too_large"
	DispatchHeldPayloadNotEncodable = "payload_not_encodable"
)

// DispatchRequest captures immutable authority, not a live execution grant.
type DispatchRequest struct {
	ID                  string    `json:"id"`
	GroupID             string    `json:"group_id"`
	TopicID             string    `json:"topic_id"`
	TaskID              string    `json:"task_id"`
	TaskRevision        int64     `json:"task_revision"`
	AuthorityGeneration int64     `json:"authority_generation"`
	MandateID           string    `json:"mandate_id"`
	CoordinatorMemberID string    `json:"coordinator_member_id"`
	BindingRevision     int64     `json:"binding_revision"`
	PolicyRevision      int64     `json:"policy_revision"`
	RequestedBy         string    `json:"requested_by"` // Always the server-bound local user.
	Reason              string    `json:"reason"`
	CreatedAt           time.Time `json:"created_at"`
}

// DispatchState has no terminal status; held and enqueued retain the Task slot.
type DispatchState struct {
	RequestID     string                   `json:"request_id"`
	Generation    int64                    `json:"generation"`
	Status        DispatchStatus           `json:"status"`
	AttemptCount  int64                    `json:"attempt_count"`
	NextAttemptAt time.Time                `json:"next_attempt_at"`
	LastErrorCode string                   `json:"last_error_code"`
	HeldReason    string                   `json:"held_reason"`
	Delivery      *DispatchDeliveryReceipt `json:"delivery"`
}

type DispatchDeliveryReceipt struct {
	MessageID                  string    `json:"message_id"`
	DeliveryID                 string    `json:"delivery_id"`
	ConversationID             string    `json:"conversation_id"`
	CoordinatorMemberID        string    `json:"coordinator_member_id"`
	MaterializedFromGeneration int64     `json:"materialized_from_generation"`
	CreatedAt                  time.Time `json:"created_at"`
}

type DispatchView struct {
	Request DispatchRequest `json:"request"`
	State   DispatchState   `json:"state"`
}

type DispatchMaterialization struct {
	Disposition string                   `json:"disposition"`
	Generation  int64                    `json:"generation"`
	Delivery    *DispatchDeliveryReceipt `json:"delivery"`
	HeldReason  string                   `json:"held_reason"`
}
