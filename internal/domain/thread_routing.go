package domain

// ThreadLinks are verified references, not grants. A linked message can be in
// a different topic in this group; the link does not change either scope.
type ThreadLinks struct {
	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	RelatedTaskID    string `json:"related_task_id,omitempty"`
}

// ThreadAttribution separates the current topic for topical context from the
// immutable message/delivery/Turn scope. For E1a they always agree.
type ThreadAttribution struct {
	Storage   ThreadRef
	Effective ThreadRef
}
