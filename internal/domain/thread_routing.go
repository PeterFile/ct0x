package domain

// ThreadLinks are verified references, not grants. A linked message can be in
// a different topic in this group; the link does not change either scope.
type ThreadLinks struct {
	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	RelatedTaskID    string `json:"related_task_id,omitempty"`
}

// ThreadAttribution is topical metadata, not read or execution authority.
// CorrectionMessageID is the last actual change, even after a move back to
// an earlier topic; it is empty for messages that have never moved.
type ThreadAttribution struct {
	Storage             ThreadRef
	Effective           ThreadRef
	CorrectionMessageID string
}

// ThreadCorrection records the outcome of a local-user metadata command.
// The containing message is the correction source, not a chat work command.
// A question denotes that no target was changed.
type ThreadCorrection struct {
	MessageID        string `json:"message_id,omitempty"`
	OriginalTopicID  string `json:"original_topic_id"`
	PreviousTopicID  string `json:"previous_topic_id"`
	EffectiveTopicID string `json:"effective_topic_id"`
	Question         string `json:"question,omitempty"`
}
