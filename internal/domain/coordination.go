package domain

import "time"

// ThreadRef identifies one exact topic; an empty TopicID denotes the main topic.
type ThreadRef struct {
	GroupID string `json:"group_id"`
	TopicID string `json:"topic_id"`
}

type TriggerSourceKind string

const TriggerSourceLocalIM TriggerSourceKind = "local_im"

// Trigger is an immutable source reference, not content or authorization.
type Trigger struct {
	ID              string            `json:"id"`
	SourceKind      TriggerSourceKind `json:"source_kind"`
	GroupID         string            `json:"group_id"`
	TopicID         string            `json:"topic_id"`
	MessageID       string            `json:"message_id"`
	MessageSequence int64             `json:"message_sequence"`
	CreatedAt       time.Time         `json:"created_at"` // Capture time, not a source checkpoint.
}
