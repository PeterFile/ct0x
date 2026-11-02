package domain

// RequestWorkRequest is an explicit local-user command; the message is provenance only.
// Mode selects exactly one nested value. Actor and policy are never caller inputs.
type RequestWorkRequest struct {
	TopicID      string               `json:"topic_id"`
	MessageID    string               `json:"message_id"`
	Mode         RoutingDecisionKind  `json:"mode"`
	Reason       string               `json:"reason"`
	NewTask      *NewTaskRequest      `json:"new_task,omitempty"`
	ExistingTask *ExistingTaskRequest `json:"existing_task,omitempty"`
}

type NewTaskRequest struct {
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Acceptance    string   `json:"acceptance"`
	AssigneeID    string   `json:"assignee_id"`
	DependencyIDs []string `json:"dependency_ids"`
}

type ExistingTaskRequest struct {
	TaskID           string `json:"task_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

// TaskRef is historical identity, not the Task's current lifecycle projection.
type TaskRef struct {
	ID                  string `json:"id"`
	GroupID             string `json:"group_id"`
	RevisionAtAdmission int64  `json:"revision_at_admission"`
}

type AdmissionReceipt struct {
	Decision RoutingDecision `json:"decision"`
	Mandate  Mandate         `json:"mandate"`
	Task     TaskRef         `json:"task"`
}
