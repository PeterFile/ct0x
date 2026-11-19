package domain

import "time"

// DecisionAnswerInput selects a label by its one-based index in this exact
// version, or supplies literal text. Zero OptionIndex means a literal answer.
type DecisionAnswerInput struct {
	Version     int64  `json:"version"`
	OptionIndex int    `json:"option_index,omitempty"`
	Text        string `json:"text,omitempty"`
}

// DecisionAnswerReceipt proves saving one human statement and queuing its
// owner wake. It is immutable; the WorkInput ACK separately proves handling.
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

// DecisionAnswerView separates the immutable saved receipt from explicit owner
// handling. Without a persisted acknowledgement, Handling is nil.
type DecisionAnswerView struct {
	Receipt  DecisionAnswerReceipt     `json:"receipt"`
	Handling *TaskWorkInputConsumption `json:"handling"`
}
