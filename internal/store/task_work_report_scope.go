package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"agentworkspace/internal/domain"
)

func taskWorkReportsSupportedInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var ready bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=?)`, taskWorkReportVersion).Scan(&ready)
	return ready, err
}

func requireTaskWorkReportsInTx(ctx context.Context, tx *sql.Tx) error {
	installed, err := taskWorkReportsSupportedInTx(ctx, tx)
	if err == nil && !installed {
		return ErrNotFound
	}
	return err
}

func taskWorkParent(f taskWorkFacts) domain.TaskWorkParent {
	a, r := f.v.a, f.v.r
	p := domain.TaskWorkParent{ThreadRef: domain.ThreadRef{GroupID: a.GroupID, TopicID: r.TopicID}, TaskID: a.TaskID,
		TaskRevision: r.TaskRevision, AssignmentID: a.ID, MandateID: r.MandateID, AuthorityGeneration: r.AuthorityGeneration, OwnerMemberID: a.WorkerMemberID}
	if d := f.dispatch.Delivery; d != nil {
		p.OriginalDeliveryID, p.OriginalMessageID = d.DeliveryID, d.MessageID
	}
	return p
}

// Initial role authority is reconstructed from the exact Assignment delivery;
// a raw typed marker must never fall through to the original-worker branch.
func taskWorkOwnerInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope) (taskWorkFacts, string, error) {
	var deliveryID string
	if err := tx.QueryRowContext(ctx, "SELECT t.delivery_id"+memberToolBinding+" AND t.member_id=? AND t.conversation_id=? AND c.topic_id=?",
		scope.Claim.GroupID, scope.Claim.TurnID, scope.Claim.Token, scope.MemberID, scope.ConversationID, scope.TopicID).Scan(&deliveryID); err != nil {
		return taskWorkFacts{}, "", coordinationRoleRelationError(err)
	}
	f, err := lookupTaskWorkDeliveryInTx(ctx, tx, scope.Claim.GroupID, deliveryID)
	if err == nil {
		attempt, err := taskWorkBoundOwnerInTx(ctx, tx, f, scope)
		return f, attempt, err
	}
	if !errors.Is(err, ErrNotFound) {
		return f, "", err
	}
	var assignmentID string
	if err := tx.QueryRowContext(ctx, `SELECT assignment_id FROM coordination_assignment_dispatch WHERE delivery_id=?`, deliveryID).Scan(&assignmentID); err != nil {
		return f, "", coordinationRoleRelationError(err)
	}
	f.v, f.dispatch, err = coordinationAssignmentLoad(ctx, tx, assignmentID)
	if err != nil {
		return f, "", coordinationRoleRelationError(err)
	}
	a, r, d := f.v.a, f.v.r, f.dispatch.Delivery
	if d == nil || f.dispatch.Status != domain.AssignmentDispatchStatusEnqueued ||
		a.GroupID != scope.Claim.GroupID || r.TopicID != scope.TopicID || a.WorkerMemberID != scope.MemberID ||
		d.DeliveryID != deliveryID || d.ConversationID != scope.ConversationID {
		return f, "", ErrMemberToolDenied
	}
	p := CoordinationRoleProvenance{Kind: CoordinationRoleWorker, GroupID: a.GroupID, TopicID: r.TopicID, MemberID: a.WorkerMemberID,
		ConversationID: d.ConversationID, DeliveryID: d.DeliveryID, MessageID: d.MessageID, RequestID: r.ID, TaskID: a.TaskID, TaskRevision: r.TaskRevision, AssignmentID: a.ID}
	if _, err := coordinationExecutionProvenanceInTx(ctx, tx, scope, p); err != nil {
		return f, "", err
	}
	attempt, err := coordinationRoleAttemptInTx(ctx, tx, scope, p)
	if err == nil && attempt == "" {
		err = ErrMemberToolDenied
	}
	f.d, f.turnID = taskWorkRecord(f.v, scope.MemberID, ""), scope.Claim.TurnID
	f.d.SourceKind = ""
	f.d.DeliveryID, f.d.MessageID, f.d.ConversationID = d.DeliveryID, d.MessageID, d.ConversationID
	return f, attempt, err
}

// The fact comes from the verified typed loader and the scope from a live claim.
func taskWorkBoundOwnerInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts, scope MemberToolScope) (string, error) {
	d := f.d
	if !d.SourceKind.IsOwnerContinuation() || f.turnID != scope.Claim.TurnID || d.OwnerMemberID != scope.MemberID ||
		d.MemberID != scope.MemberID || d.GroupID != scope.Claim.GroupID || d.ConversationID != scope.ConversationID || d.TopicID != scope.TopicID {
		return "", ErrMemberToolDenied
	}
	a, err := taskWorkBoundAttemptInTx(ctx, tx, d, "")
	if err == nil && (a.ID == "" || a.TurnID != scope.Claim.TurnID) {
		err = ErrMemberToolDenied
	}
	return a.ID, err
}

func (s *Store) taskWorkOwnerReadyInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts, attempt string) (taskInvestigationEvidence, error) {
	if !s.taskWorkExecution.Load() {
		return taskInvestigationEvidence{}, ErrMemberToolDenied
	}
	blocked, active, err := taskWorkCurrentInTx(ctx, tx, f)
	if err != nil {
		return taskInvestigationEvidence{}, err
	}
	if blocked != "" || active != attempt {
		return taskInvestigationEvidence{}, ErrConflict
	}
	return s.taskWorkInvestigationInTx(ctx, tx, f)
}

func taskWorkSubset(p domain.TaskInvestigationScope, in domain.DelegateWorkInput) bool {
	if in.AllowTopicMessages && !p.AllowTopicMessages {
		return false
	}
	for _, ref := range in.ArtifactVersions {
		if !slices.Contains(p.ArtifactVersions, ref) {
			return false
		}
	}
	return true
}

func (s *Store) taskWorkDelegationMaterialsInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts) (domain.TaskInvestigationScope, error) {
	evidence, err := s.taskWorkInvestigationInTx(ctx, tx, f)
	if err != nil {
		return domain.TaskInvestigationScope{}, err
	}
	p, d := evidence.TaskInvestigationScope, f.delegation
	// B2 verifies existence of every version in the parent allowlist.
	if d == nil || !taskWorkSubset(p, d.DelegateWorkInput) || p.MaxDelegations != d.MaxDelegations || d.Ordinal > p.MaxDelegations {
		return domain.TaskInvestigationScope{}, ErrMemberToolDenied
	}
	// A child sees its own question and grants, not the parent's full materials.
	p.Objective, p.AllowTopicMessages, p.ArtifactVersions, p.MaxDelegations = d.Question, d.AllowTopicMessages, d.ArtifactVersions, 0
	return p, nil
}

func (s *Store) taskWorkMaterialsForMemberInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope) (domain.TaskInvestigationScope, error) {
	f, err := taskWorkExecutionInTx(ctx, tx, scope)
	if err != nil {
		return domain.TaskInvestigationScope{}, err
	}
	if f.delegation != nil {
		blocked, _, err := s.taskWorkReadinessInTx(ctx, tx, f, scope.Claim.TurnID)
		if err != nil {
			return domain.TaskInvestigationScope{}, err
		}
		if blocked != "" {
			return domain.TaskInvestigationScope{}, ErrMemberToolDenied
		}
		return s.taskWorkDelegationMaterialsInTx(ctx, tx, f)
	}
	attempt, err := taskWorkBoundOwnerInTx(ctx, tx, f, scope)
	if err != nil {
		return domain.TaskInvestigationScope{}, err
	}
	evidence, err := s.taskWorkOwnerReadyInTx(ctx, tx, f, attempt)
	return evidence.TaskInvestigationScope, err
}
