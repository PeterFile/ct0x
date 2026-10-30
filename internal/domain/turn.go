package domain

import "time"

type TurnState string

const (
	TurnStateStarting        TurnState = "starting"
	TurnStateRunning         TurnState = "running"
	TurnStateCancelRequested TurnState = "cancel_requested"
	TurnStateUnknown         TurnState = "unknown"
	TurnStateNotStarted      TurnState = "not_started"
	TurnStateSucceeded       TurnState = "succeeded"
	TurnStateFailed          TurnState = "failed"
	TurnStateAborted         TurnState = "aborted"
	TurnStateTruncated       TurnState = "truncated"
)

type Turn struct {
	ID                string     `json:"id"`
	GroupID           string     `json:"group_id"`
	DeliveryID        string     `json:"delivery_id"`
	ConversationID    string     `json:"conversation_id"`
	MemberID          string     `json:"member_id"`
	ClaimToken        string     `json:"-"`
	State             TurnState  `json:"state"`
	NativeSessionID   string     `json:"native_session_id"`
	AcceptedAt        *time.Time `json:"accepted_at"`
	CancelRequestedAt *time.Time `json:"cancel_requested_at"`
	CandidateText     string     `json:"-"`
	CandidateTerminal TurnState  `json:"-"`
	ReplyMessageID    string     `json:"reply_message_id"`
	Reason            string     `json:"reason"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}
