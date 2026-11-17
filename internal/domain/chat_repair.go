package domain

import "time"

const (
	ChatRepairReviewRequest     = "review_request"
	ChatRepairOriginalAgreement = "original_agreement"
	ChatRepairChatRequest       = "chat_request"
)

// ChatRepairAssessment proposes intent only; the server resolves all scope and
// formal Review/command relationships in the bound execution transaction.
type ChatRepairAssessment struct {
	ReviewID      string `json:"review_id"`
	Authorization string `json:"authorization"`
}

// ChatRepairLink is immutable provenance for a new Task, not a renewed parent
// Assignment or an execution capability. Artifact versions are concrete bases.
type ChatRepairLink struct {
	ID string `json:"id"`
	ThreadRef
	RepairTaskID                string               `json:"repair_task_id"`
	ParentTaskID                string               `json:"parent_task_id"`
	ParentSubmissionID          string               `json:"parent_submission_id"`
	ParentReviewID              string               `json:"parent_review_id"`
	ReviewedTaskRevision        int64                `json:"reviewed_task_revision"`
	ParentMandateID             string               `json:"parent_mandate_id"`
	CandidateID                 string               `json:"candidate_id"`
	AssessmentID                string               `json:"assessment_id"`
	MandateID                   string               `json:"mandate_id"`
	DispatchRequestID           string               `json:"dispatch_request_id"`
	PolicyRevision              int64                `json:"policy_revision"`
	Authorization               string               `json:"authorization"`
	AuthorizationSourceKind     string               `json:"authorization_source_kind"`
	AuthorizationSourceID       string               `json:"authorization_source_id"`
	AuthorizationMessageID      string               `json:"authorization_message_id,omitempty"`
	AuthorizationPolicyRevision int64                `json:"authorization_policy_revision"`
	ArtifactVersions            []ArtifactVersionRef `json:"artifact_versions"`
	CreatedAt                   time.Time            `json:"created_at"`
}

// ChatRepairSource is server-read Review context, not permission to admit or
// execute repair work. Parent authority and frozen versions remain historical;
// admission must separately check intent, current Policy, grants and execution.
type ChatRepairSource struct {
	ThreadRef
	ParentTaskID            string                  `json:"parent_task_id"`
	ParentSubmissionID      string                  `json:"parent_submission_id"`
	ParentSubmissionSummary string                  `json:"parent_submission_summary"`
	ParentReviewID          string                  `json:"parent_review_id"`
	ReviewedTaskRevision    int64                   `json:"reviewed_task_revision"`
	ParentMandate           Mandate                 `json:"parent_mandate"`
	ParentAdmission         ChatAssessmentReceipt   `json:"parent_admission"`
	ReviewReason            string                  `json:"review_reason"`
	ArtifactVersions        []ArtifactVersionRef    `json:"artifact_versions"`
	OriginalAuthorization   ChatRepairAuthorization `json:"original_authorization"`
}

// ChatRepairAuthorization carries complete actual user evidence, not a model's
// generated Task/Mandate projection. Content is never permission by itself.
type ChatRepairAuthorization struct {
	SourceKind     string `json:"source_kind"`
	SourceID       string `json:"source_id"`
	MessageID      string `json:"message_id,omitempty"`
	PolicyRevision int64  `json:"policy_revision"`
	Content        string `json:"content"`
	ScopeKind      string `json:"scope_kind,omitempty"`
}
