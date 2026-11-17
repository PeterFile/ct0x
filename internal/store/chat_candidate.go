package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"

	"agentworkspace/internal/domain"
)

// CaptureChatCandidate runs after claim and before native execution/assessment.
// Rebinding a turn returns its original snapshot, never a fresh policy version.
func (s *Store) CaptureChatCandidate(ctx context.Context, scope MemberToolScope) (domain.ChatCandidate, error) {
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return domain.ChatCandidate{}, memberToolOperationError("绑定聊天评估失败", err)
	}
	defer tx.Rollback()
	var messageID string
	var generation int64
	// Any role receipt, even a damaged one, prevents ordinary intake authority.
	err = tx.QueryRowContext(ctx, `SELECT d.message_id,l.sequence FROM turns t JOIN deliveries d ON d.id=t.delivery_id
	 JOIN message_log l ON l.message_id=d.message_id WHERE t.id=?
	 AND NOT EXISTS(SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=d.id)
	 AND NOT EXISTS(SELECT 1 FROM coordination_assignment_dispatch WHERE delivery_id=d.id)`, scope.Claim.TurnID).Scan(&messageID, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ChatCandidate{}, ErrMemberToolDenied
	}
	if err != nil {
		return domain.ChatCandidate{}, memberToolOperationError("读取聊天来源失败", err)
	}
	var id string
	err = tx.QueryRowContext(ctx, "SELECT id FROM chat_candidates WHERE turn_id=?", scope.Claim.TurnID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		thread := domain.ThreadRef{GroupID: scope.Claim.GroupID, TopicID: scope.TopicID}
		var policy domain.ChatPolicy
		policy, err = chatPolicyInTx(ctx, tx, thread, 0)
		if err != nil {
			return domain.ChatCandidate{}, memberToolOperationError("读取聊天策略失败", err)
		}
		var binding, dispatchPolicy int64
		binding, dispatchPolicy, err = chatDispatchRevisionsInTx(ctx, tx, scope.Claim.GroupID)
		if err != nil {
			return domain.ChatCandidate{}, memberToolOperationError("读取分派策略失败", err)
		}
		id = rand.Text()
		err = coordinationDispatchWrite(ctx, tx, `INSERT INTO chat_candidates(id,group_id,topic_id,message_id,member_id,turn_id,generation,policy_revision,binding_revision,dispatch_policy_revision)
		 VALUES(?,?,?,?,?,?,?,nullif(?,0),?,?)`, id, thread.GroupID, thread.TopicID, messageID, scope.MemberID, scope.Claim.TurnID, generation, policy.Revision, binding, dispatchPolicy)
		if err == nil {
			kind, sourceID, sourceErr := messageSourceInTx(ctx, tx, messageID)
			if sourceErr != nil {
				return domain.ChatCandidate{}, memberToolOperationError("读取真实来源失败", sourceErr)
			}
			digest := sha256.Sum256([]byte(scope.Claim.Token))
			err = coordinationDispatchWrite(ctx, tx, `INSERT INTO chat_intake_executions(candidate_id,turn_id,purpose,claim_digest,source_id,source_kind) VALUES(?,?,?,?,?,?)`, id, scope.Claim.TurnID, domain.ChatIntakePurpose, digest[:], sourceID, kind)
			if err == nil {
				err = captureChatRepairCandidateInTx(ctx, tx, scope, id)
			}
		}
	}
	if err != nil {
		if errors.Is(err, ErrChatRepairSourceEncoding) {
			return domain.ChatCandidate{}, err
		}
		return domain.ChatCandidate{}, memberToolOperationError("保存聊天候选失败", err)
	}
	candidate, err := chatCandidateInTx(ctx, tx, scope, id)
	if err != nil {
		return domain.ChatCandidate{}, memberToolOperationError("读取聊天候选失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ChatCandidate{}, memberToolOperationError("保存聊天候选失败", err)
	}
	return candidate, nil
}

func chatCandidateInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, id string) (domain.ChatCandidate, error) {
	var c domain.ChatCandidate
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT c.id,c.group_id,c.topic_id,c.message_id,m.sender_id,c.member_id,c.turn_id,c.generation,
	 coalesce(c.policy_revision,0),c.binding_revision,c.dispatch_policy_revision,coalesce(e.purpose,''),coalesce(e.source_id,''),coalesce(e.source_kind,'unknown')
	 FROM chat_candidates c JOIN messages m ON m.id=c.message_id LEFT JOIN chat_intake_executions e ON e.candidate_id=c.id
	 WHERE c.id=? AND c.group_id=? AND c.topic_id=? AND c.member_id=? AND c.turn_id=?`,
		id, scope.Claim.GroupID, scope.TopicID, scope.MemberID, scope.Claim.TurnID).Scan(&c.ID, &c.GroupID, &c.TopicID, &c.MessageID, &c.SourceSenderID, &c.MemberID, &c.TurnID,
		&c.Generation, &revision, &c.BindingRevision, &c.DispatchPolicyRevision, &c.Purpose, &c.SourceID, &c.SourceKind)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ChatCandidate{}, ErrMemberToolDenied
	}
	if err != nil {
		return domain.ChatCandidate{}, err
	}
	c.Policy = domain.ChatPolicy{ThreadRef: c.ThreadRef}
	if revision != 0 {
		c.Policy, err = chatPolicyInTx(ctx, tx, c.ThreadRef, revision)
	} else {
		// Revision zero stays a disabled snapshot, never the current policy.
		c.Policy.WorkspaceChangesSupported, err = chatWorkspaceSupportedInTx(ctx, tx)
	}
	if err == nil {
		c.RepairSource, err = chatRepairCandidateInTx(ctx, tx, c.ID)
	}
	return c, err
}

func chatIntakePurposeInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, candidateID string) error {
	digest := sha256.Sum256([]byte(scope.Claim.Token))
	var valid int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM chat_intake_executions e JOIN chat_candidates c ON c.id=e.candidate_id
	 WHERE e.candidate_id=? AND e.turn_id=? AND e.purpose=? AND e.claim_digest=?
	 AND c.group_id=? AND c.topic_id=? AND c.member_id=? AND c.turn_id=e.turn_id`, candidateID, scope.Claim.TurnID, domain.ChatIntakePurpose, digest[:], scope.Claim.GroupID, scope.TopicID, scope.MemberID).Scan(&valid); err != nil {
		return ErrMemberToolDenied
	}
	return nil
}

func chatDispatchRevisionsInTx(ctx context.Context, tx *sql.Tx, groupID string) (binding, policy int64, err error) {
	err = tx.QueryRowContext(ctx, `SELECT
	 coalesce((SELECT revision FROM group_coordinator_bindings WHERE group_id=?),0),
	 coalesce((SELECT revision FROM group_dispatch_policies WHERE group_id=?),0)`, groupID, groupID).Scan(&binding, &policy)
	return
}
