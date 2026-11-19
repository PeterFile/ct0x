package domain

import "time"

// DecisionAnswerInput selects a label by its one-based index in this exact
// version, or supplies literal text. Zero OptionIndex means a literal answer.
type DecisionAnswerInput struct {
	Version     int64  `json:"version"`
	OptionIndex int    `json:"option_index,omitempty"`
	Text        string `json:"text,omitempty"`
}

// DecisionAnswerReceipt proves saving one human statement and queuing its wake.
// It does not prove the recipient handled it. Task fields are empty for a
// pre-Task Thread reply, which is itself the wake message.
type DecisionAnswerReceipt struct {
	ID        string `json:"id"`
	RequestID string `json:"request_id"`
	ThreadRef
	Version        int64     `json:"version"`
	ActorID        string    `json:"actor_id"`
	TaskID         string    `json:"task_id"`
	TaskRevision   int64     `json:"task_revision"`
	OptionIndex    int       `json:"option_index,omitempty"`
	Text           string    `json:"text"`
	ReplyMessageID string    `json:"reply_message_id"`
	WakeDeliveryID string    `json:"wake_delivery_id"`
	WakeMessageID  string    `json:"wake_message_id"`
	CreatedAt      time.Time `json:"created_at"`
}

// DecisionAnswerView separates the immutable saved receipt from explicit
// handling by the recipient. Without a persisted acknowledgement, Handling is nil.
type DecisionAnswerView struct {
	Receipt  DecisionAnswerReceipt   `json:"receipt"`
	Handling *DecisionAnswerHandling `json:"handling"`
}

// DecisionAnswerHandling identifies the Turn that acknowledged the answer.
// An Attempt exists only for the Task route.
type DecisionAnswerHandling struct {
	TurnID     string    `json:"turn_id"`
	AttemptID  string    `json:"attempt_id,omitempty"`
	ConsumedAt time.Time `json:"consumed_at"`
}
