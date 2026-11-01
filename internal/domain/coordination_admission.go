package domain

import "time"

// LocalUserExplicitPolicyRevision versions the fixed gate, not mutable policy state.
const LocalUserExplicitPolicyRevision = "local-user-explicit/v1"

type RoutingDecisionKind string

const (
	RoutingDecisionCreateNew    RoutingDecisionKind = "create_new"
	RoutingDecisionLinkExisting RoutingDecisionKind = "link_existing"
)

// RoutingDecision records admission, independently of deliverable ReviewDecision.
// IDs and timestamps are assigned by the trusted server writer.
type RoutingDecision struct {
	ID                      string              `json:"id"`
	TriggerID               string              `json:"trigger_id"`
	GroupID                 string              `json:"group_id"`
	TopicID                 string              `json:"topic_id"`
	Kind                    RoutingDecisionKind `json:"kind"`
	ActorID                 string              `json:"actor_id"`
	PolicyRevision          string              `json:"policy_revision"`
	Reason                  string              `json:"reason"`
	TaskID                  string              `json:"task_id"`
	TaskRevisionAtAdmission int64               `json:"task_revision_at_admission"`
	MandateID               string              `json:"mandate_id"`
	CreatedAt               time.Time           `json:"created_at"`
}

type MandateAuthorityKind string

const MandateAuthorityLocalUserExplicit MandateAuthorityKind = "local_user_explicit"

type MandateStrength string

const MandateStrengthExplicit MandateStrength = "explicit"

// TaskScope is immutable admission evidence, not the live Task or resource grants.
// Objective captures the authoritative Task title at admission.
type TaskScope struct {
	Objective     string   `json:"objective"`
	Description   string   `json:"description"`
	Acceptance    string   `json:"acceptance"`
	AssigneeID    string   `json:"assignee_id"`
	DependencyIDs []string `json:"dependency_ids"`
}

// Mandate is immutable authority evidence; future withdrawal changes its projection.
type Mandate struct {
	ID                      string               `json:"id"`
	DecisionID              string               `json:"decision_id"`
	OriginTriggerID         string               `json:"origin_trigger_id"`
	GroupID                 string               `json:"group_id"`
	TopicID                 string               `json:"topic_id"`
	TaskID                  string               `json:"task_id"`
	TaskRevisionAtAdmission int64                `json:"task_revision_at_admission"`
	AuthorityKind           MandateAuthorityKind `json:"authority_kind"`
	AuthorizedBy            string               `json:"authorized_by"`
	Strength                MandateStrength      `json:"strength"`
	PolicyRevision          string               `json:"policy_revision"`
	Scope                   TaskScope            `json:"scope"`
	CreatedAt               time.Time            `json:"created_at"`
}

// TaskCoordination projects current authority, never Task deliverable status.
// Future pointer transitions must CAS AuthorityGeneration and retain old Mandates.
type TaskCoordination struct {
	TaskID              string `json:"task_id"`
	GroupID             string `json:"group_id"`
	TopicID             string `json:"topic_id"`
	CurrentMandateID    string `json:"current_mandate_id"`
	AuthorityGeneration int64  `json:"authority_generation"`
}
