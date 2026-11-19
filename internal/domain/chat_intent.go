package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	ChatActionTextSubmission        = "text_submission"
	ChatActionWorkspaceChange       = "workspace_change"
	ChatActionReadOnlyInvestigation = "read_only_investigation"
	ChatPreparationTopicSummary     = "topic_summary"
	ChatIntakePurpose               = "chat_intake/v1"
	ChatDispositionContinueWork     = "continue_work"
)

// ChatPolicyRequest is local-user configuration, not a member tool grant.
// Scope is exactly one group/topic. Workspace changes require a separate opt-in
// and server-bound grants. The budget covers the entire policy revision.
type ChatPolicyRequest struct {
	Enabled                   bool                     `json:"enabled"`
	AllowPreparation          bool                     `json:"allow_preparation"`
	PreauthorizationMessageID string                   `json:"preauthorization_message_id"`
	Action                    string                   `json:"action"`
	MaxAdmissions             int                      `json:"max_admissions"`
	PreparationScopeKind      string                   `json:"preparation_scope_kind,omitempty"`
	AllowWorkspaceChanges     bool                     `json:"allow_workspace_changes,omitempty"`
	Investigation             *ChatInvestigationPolicy `json:"investigation,omitempty"`
}

// ChatInvestigationPolicy is a local-user allowlist, not a tool or delegation grant.
type ChatInvestigationPolicy struct {
	Objective          string                     `json:"objective"`
	AllowTopicMessages bool                       `json:"allow_topic_messages"`
	ArtifactVersions   []ArtifactVersionRef       `json:"artifact_versions"`
	MaxDelegations     int                        `json:"max_delegations"`
	ClueTerms          ChatInvestigationClueTerms `json:"clue_terms,omitzero"`
}

// ChatInvestigationClueTerms is omitted for legacy rules; null must not
// silently turn a configured boundary into the legacy investigation path.
type ChatInvestigationClueTerms []string

func (terms *ChatInvestigationClueTerms) UnmarshalJSON(data []byte) error {
	var phrases []string
	if err := json.Unmarshal(data, &phrases); err != nil {
		return err
	}
	if phrases == nil {
		return errors.New("只读调查线索词组必须为非空数组")
	}
	*terms = phrases
	return nil
}

// MatchesClue checks literal containment only. A caller previewing a delivery
// uses its persisted Message.Content; only Store can prove source and authority.
func (p ChatInvestigationPolicy) MatchesClue(body string) bool {
	if len(p.ClueTerms) == 0 {
		return false
	}
	for _, term := range p.ClueTerms {
		if term == "" || !strings.Contains(body, term) {
			return false
		}
	}
	return true
}

// TaskInvestigationScope is immutable admission evidence, not live permission.
type TaskInvestigationScope struct {
	ThreadRef
	TaskID                string `json:"task_id"`
	TaskRevision          int64  `json:"task_revision"`
	MandateID             string `json:"mandate_id"`
	PolicyRevision        int64  `json:"policy_revision"`
	ConfigurationSourceID string `json:"configuration_source_id"`
	ChatInvestigationPolicy
}

type ChatPolicy struct {
	ThreadRef
	ChatPolicyRequest
	Revision              int64  `json:"revision"`
	AuthorizedBy          string `json:"authorized_by"`
	PreauthorizationText  string `json:"preauthorization_text,omitempty"`
	PreparationSupported  bool   `json:"preparation_supported"`
	ConfigurationSourceID string `json:"configuration_source_id"`

	// WorkspaceChangesSupported describes Store admission support, not user
	// permission, provider file capabilities, or connected execution tools.
	WorkspaceChangesSupported bool `json:"workspace_changes_supported"`

	// Schema support and trusted process readiness are not policy grants.
	InvestigationSupported        bool `json:"investigation_supported"`
	InvestigationExecutionEnabled bool `json:"investigation_execution_enabled"`
}

// ChatCandidate is captured by the runtime before semantic assessment. Its
// identities, policy and generation cannot be selected by model arguments.
type ChatCandidate struct {
	ID string `json:"id"`
	ThreadRef
	MessageID              string            `json:"message_id"`
	SourceSenderID         string            `json:"source_sender_id"`
	MemberID               string            `json:"member_id"`
	TurnID                 string            `json:"turn_id"`
	Generation             int64             `json:"generation"`
	Policy                 ChatPolicy        `json:"policy"`
	BindingRevision        int64             `json:"binding_revision"`
	DispatchPolicyRevision int64             `json:"dispatch_policy_revision"`
	Purpose                string            `json:"purpose"`
	SourceID               string            `json:"source_id"`
	SourceKind             string            `json:"source_kind"`
	RepairSource           *ChatRepairSource `json:"repair_source,omitempty"`

	// Trusted issuance hint at capture time. A new assessment repeats all
	// checks under its own claim fence; this flag is not a persisted receipt.
	InvestigationClueMatched bool `json:"-"`

	// Advisory live same-topic hints, not a selection or proof of authority.
	// MessageID is the source to quote and the original reply destination.
	FollowupTaskIDs []string `json:"followup_task_ids,omitempty"`
}

// TaskWorkspaceScope records admission facts, not execution permission or a
// sandbox. Physical identity, role, capability and resource locks are rechecked
// by trusted execution wrappers. All same-resource eligible workers are kept.
type TaskWorkspaceScope struct {
	ThreadRef
	TaskID            string               `json:"task_id"`
	TaskRevision      int64                `json:"task_revision"`
	MandateID         string               `json:"mandate_id"`
	CandidateID       string               `json:"candidate_id"`
	AssessmentID      string               `json:"assessment_id"`
	DispatchRequestID string               `json:"dispatch_request_id"`
	PolicyRevision    int64                `json:"policy_revision"`
	ResourceKey       string               `json:"resource_key"`
	NodeID            string               `json:"node_id"`
	Grants            []TaskWorkspaceGrant `json:"grants"`
}

// TaskWorkspaceGrant preserves the original immutable grant and its owner.
// GrantSchemaVersion identifies the grant contract, not a mutable grant revision.
type TaskWorkspaceGrant struct {
	GrantID            string `json:"grant_id"`
	MemberID           string `json:"member_id"`
	Root               string `json:"root"`
	ResolvedRoot       string `json:"resolved_root"`
	GrantSchemaVersion int64  `json:"grant_schema_version"`
}

// ChatAssessment is a semantic proposal, never an authorization decision.
// request_work requires an actual user source; prepare_work requires enabled
// written preauthorization. discuss/clarify do not create tasks or dispatches.
type ChatAssessment struct {
	Disposition string                  `json:"disposition"`
	Action      string                  `json:"action,omitempty"`
	Title       string                  `json:"title,omitempty"`
	Description string                  `json:"description,omitempty"`
	Acceptance  string                  `json:"acceptance,omitempty"`
	Reason      string                  `json:"reason"`
	Question    string                  `json:"question,omitempty"`
	Repair      *ChatRepairAssessment   `json:"repair,omitempty"`
	Followup    *ChatFollowupAssessment `json:"followup,omitempty"`

	// Only for discuss: a verbatim restriction from the bound local-user message.
	DiscussionOnlyExcerpt *string `json:"discussion_only_excerpt,omitempty"`
}

// ChatFollowupAssessment proposes context for existing work, not new authority.
// TaskID, if specified, must occur in the source; Constraint must be a verbatim
// excerpt. An empty TaskID asks the Store to resolve work in the source topic.
type ChatFollowupAssessment struct {
	TaskID     string `json:"task_id,omitempty"`
	Constraint string `json:"constraint,omitempty"`
}

// ChatAssessmentReceipt preserves the real assessor and source separately from
// local-user authority recorded by the legacy admission gate. A source replay
// can return a different CandidateID: it is not an evaluation of the caller's
// candidate and must not schedule another discussion action.
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

	// Source limitation, not authorization or a policy change.
	DiscussionOnly        bool   `json:"discussion_only,omitempty"`
	DiscussionOnlyExcerpt string `json:"discussion_only_excerpt,omitempty"`

	WorkspaceScope    *TaskWorkspaceScope `json:"workspace_scope,omitempty"`
	RepairLink        *ChatRepairLink     `json:"repair_link,omitempty"`
	WorkInputID       string              `json:"work_input_id,omitempty"`
	WorkInputPending  bool                `json:"work_input_pending,omitempty"` // Recorded without a native wake by explicit dormant mode.
	WorkDeliveryID    string              `json:"work_delivery_id,omitempty"`
	WorkWakeMessageID string              `json:"work_wake_message_id,omitempty"`
}
