package domain

import "time"

const (
	ChatActionTextSubmission    = "text_submission"
	ChatPreparationTopicSummary = "topic_summary"
	ChatIntakePurpose           = "chat_intake/v1"
)

// ChatPolicyRequest is local-user configuration, not a member tool grant.
// Scope is exactly one group/topic and one text-only action. The budget applies
// to the entire policy revision, not to each model invocation.
type ChatPolicyRequest struct {
	Enabled                   bool   `json:"enabled"`
	AllowPreparation          bool   `json:"allow_preparation"`
	PreauthorizationMessageID string `json:"preauthorization_message_id"`
	Action                    string `json:"action"`
	MaxAdmissions             int    `json:"max_admissions"`
	PreparationScopeKind      string `json:"preparation_scope_kind,omitempty"`
}

type ChatPolicy struct {
	ThreadRef
	ChatPolicyRequest
	Revision              int64  `json:"revision"`
	AuthorizedBy          string `json:"authorized_by"`
	PreauthorizationText  string `json:"preauthorization_text,omitempty"`
	PreparationSupported  bool   `json:"preparation_supported"`
	ConfigurationSourceID string `json:"configuration_source_id"`
}

// ChatCandidate is captured by the runtime before semantic assessment. Its
// identities, policy and generation cannot be selected by model arguments.
type ChatCandidate struct {
	ID string `json:"id"`
	ThreadRef
	MessageID              string     `json:"message_id"`
	SourceSenderID         string     `json:"source_sender_id"`
	MemberID               string     `json:"member_id"`
	TurnID                 string     `json:"turn_id"`
	Generation             int64      `json:"generation"`
	Policy                 ChatPolicy `json:"policy"`
	BindingRevision        int64      `json:"binding_revision"`
	DispatchPolicyRevision int64      `json:"dispatch_policy_revision"`
	Purpose                string     `json:"purpose"`
	SourceID               string     `json:"source_id"`
	SourceKind             string     `json:"source_kind"`
}

// ChatAssessment is a semantic proposal, never an authorization decision.
// request_work requires an actual user source; prepare_work requires enabled
// written preauthorization. discuss/clarify do not create tasks or dispatches.
type ChatAssessment struct {
	Disposition string `json:"disposition"`
	Action      string `json:"action,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Acceptance  string `json:"acceptance,omitempty"`
	Reason      string `json:"reason"`
	Question    string `json:"question,omitempty"`
}

// ChatAssessmentReceipt preserves the real assessor and source separately from
// local-user authority recorded by the legacy admission gate.
type ChatAssessmentReceipt struct {
	ID          string `json:"id"`
	CandidateID string `json:"candidate_id"`
	ThreadRef
	MessageID                 string    `json:"message_id"`
	SourceSenderID            string    `json:"source_sender_id"`
	AssessedBy                string    `json:"assessed_by"`
	TurnID                    string    `json:"turn_id"`
	PolicyRevision            int64     `json:"policy_revision"`
	CandidateGeneration       int64     `json:"candidate_generation"`
	Disposition               string    `json:"disposition"`
	Authority                 string    `json:"authority"`
	AuthorizationMessageID    string    `json:"authorization_message_id"`
	Action                    string    `json:"action"`
	TaskID                    string    `json:"task_id"`
	MandateID                 string    `json:"mandate_id"`
	DispatchRequestID         string    `json:"dispatch_request_id"`
	NoticeMessageID           string    `json:"notice_message_id"`
	CreatedAt                 time.Time `json:"created_at"`
	Purpose                   string    `json:"purpose"`
	SourceID                  string    `json:"source_id"`
	SourceKind                string    `json:"source_kind"`
	Outcome                   string    `json:"outcome"`
	ReasonCode                string    `json:"reason_code,omitempty"`
	Question                  string    `json:"question,omitempty"`
	AuthorityEvidenceVerified bool      `json:"authority_evidence_verified"`
}
