package domain

import "time"

// ProjectExperienceOrigin binds the selection to D's stable ownership and the
// original persisted Group/Topic. All message references use that exact scope.
type ProjectExperienceOrigin struct {
	WorkspaceID string `json:"workspace_id"`
	ProjectID   string `json:"project_id"`
	ChannelID   string `json:"channel_id"`
	ThreadRef
}

// ProjectExperienceOutcome is one particular ArtifactVersion cited by one
// accepted Submission. It is not the head, a grant, or a Task status observation.
type ProjectExperienceOutcome struct {
	TaskID       string `json:"task_id"`
	ReviewID     string `json:"review_id"`
	SubmissionID string `json:"submission_id"`
	ArtifactVersionRef
}

// ProjectExperience is the immutable selection receipt. Confirmation is either
// a postmortem selected with real local-user message evidence or a formal Review.
type ProjectExperience struct {
	ID                    string                    `json:"id"`
	Origin                ProjectExperienceOrigin   `json:"origin"`
	Problem               string                    `json:"problem"`
	Decision              string                    `json:"decision"`
	Component             string                    `json:"component"`
	ApplicableVersion     string                    `json:"applicable_version"`
	Conditions            string                    `json:"conditions,omitempty"`
	OriginalMessageID     string                    `json:"original_message_id"`
	ConfirmationMessageID string                    `json:"confirmation_message_id,omitempty"`
	AcceptedOutcome       *ProjectExperienceOutcome `json:"accepted_outcome,omitempty"`
	ConfirmedBy           string                    `json:"confirmed_by"`
	ConfirmedAt           time.Time                 `json:"confirmed_at"`
	SelectedAt            time.Time                 `json:"selected_at"`
}

// ProjectExperienceRetirement is a separate immutable receipt. Its message and
// optional replacement outcome use the original experience's scope.
type ProjectExperienceRetirement struct {
	ExperienceID       string                    `json:"experience_id"`
	MessageID          string                    `json:"message_id"`
	ReplacementVersion string                    `json:"replacement_version,omitempty"`
	ReplacementOutcome *ProjectExperienceOutcome `json:"replacement_outcome,omitempty"`
	RetiredBy          string                    `json:"retired_by"`
	RetiredAt          time.Time                 `json:"retired_at"`
}

type ProjectExperienceStatus string

const (
	ProjectExperienceActive  ProjectExperienceStatus = "active"
	ProjectExperienceStale   ProjectExperienceStatus = "stale"
	ProjectExperienceRetired ProjectExperienceStatus = "retired"
)

type ProjectExperienceHit struct {
	ProjectExperience
	Status     ProjectExperienceStatus      `json:"status"`
	Retirement *ProjectExperienceRetirement `json:"retirement,omitempty"`
}
