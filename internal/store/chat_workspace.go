package store

import (
	"context"
	"database/sql"
	"errors"

	"agentworkspace/internal/domain"
)

// Derive authority only from current server configuration and immutable grants.
// ResourceKey is the recorded physical identity; path spelling is not identity.
func chatWorkspaceForCandidateInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate) (*domain.TaskWorkspaceScope, error) {
	rows, err := tx.QueryContext(ctx, `SELECT g.id,g.member_id,g.root,g.resolved_root,g.resource_key,g.node_id
	 FROM workspace_write_grants g JOIN members m ON m.group_id=g.group_id AND m.id=g.member_id
	 JOIN group_dispatch_policy_revisions p ON p.group_id=g.group_id AND p.revision=?
	 JOIN json_each(p.allowed_worker_ids) w ON w.type='text' AND w.value=g.member_id
	 JOIN group_coordinator_binding_revisions b ON b.group_id=g.group_id AND b.revision=?
	 WHERE g.group_id=? AND g.scope='read_write' AND g.node_id='local' AND g.authorized_by='user'
	 AND m.status='active' AND m.id<>b.member_id ORDER BY g.member_id`, c.DispatchPolicyRevision, c.BindingRevision, c.GroupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scope := &domain.TaskWorkspaceScope{ThreadRef: c.ThreadRef, CandidateID: c.ID, PolicyRevision: c.Policy.Revision, Grants: []domain.TaskWorkspaceGrant{}}
	ambiguous := false
	for rows.Next() {
		grant := domain.TaskWorkspaceGrant{GrantSchemaVersion: 19}
		var resource, node string
		if err := rows.Scan(&grant.GrantID, &grant.MemberID, &grant.Root, &grant.ResolvedRoot, &resource, &node); err != nil {
			return nil, err
		}
		if len(scope.Grants) != 0 && (scope.ResourceKey != resource || scope.NodeID != node) {
			ambiguous = true
		}
		scope.ResourceKey, scope.NodeID = resource, node
		scope.Grants = append(scope.Grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(scope.Grants) == 0 {
		return nil, chatDeny("workspace_scope_unavailable", "本群当前允许执行的活跃成员没有读写目录授权。", "是否先为允许执行的成员授权一个明确的读写目录？")
	}
	if ambiguous {
		return nil, chatDeny("workspace_scope_ambiguous", "本群当前配置包含多个不同的物理写目录，无法确定本次目标。", "本次工作应使用哪个已授权目录？")
	}
	return scope, nil
}

func insertChatWorkspaceScopeInTx(ctx context.Context, tx *sql.Tx, scope domain.TaskWorkspaceScope) error {
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO chat_workspace_scopes
	 (task_id,group_id,topic_id,task_revision,mandate_id,candidate_id,assessment_id,dispatch_request_id,policy_revision,resource_key,node_id)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?)`, scope.TaskID, scope.GroupID, scope.TopicID, scope.TaskRevision, scope.MandateID, scope.CandidateID, scope.AssessmentID, scope.DispatchRequestID, scope.PolicyRevision, scope.ResourceKey, scope.NodeID); err != nil {
		return err
	}
	for _, grant := range scope.Grants {
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO chat_workspace_scope_grants
		 (task_id,group_id,resource_key,node_id,grant_id,member_id,root,resolved_root,grant_schema_version) VALUES(?,?,?,?,?,?,?,?,?)`,
			scope.TaskID, scope.GroupID, scope.ResourceKey, scope.NodeID, grant.GrantID, grant.MemberID, grant.Root, grant.ResolvedRoot, grant.GrantSchemaVersion); err != nil {
			return err
		}
	}
	return nil
}

// GetTaskWorkspaceScope is a trusted historical read, never an authorization
// entry. It acquires no role, execution session, filesystem access or lock.
func (s *Store) GetTaskWorkspaceScope(ctx context.Context, groupID, taskID string) (domain.TaskWorkspaceScope, error) {
	if !validTriggerID(groupID) || !validTriggerID(taskID) {
		return domain.TaskWorkspaceScope{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.TaskWorkspaceScope{}, admissionError("读取任务目录范围失败", err)
	}
	defer tx.Rollback()
	if err := requireMessageActor(ctx, tx, groupID, "user"); err != nil {
		return domain.TaskWorkspaceScope{}, admissionError("读取任务目录范围失败", err)
	}
	scope, err := taskWorkspaceScopeInTx(ctx, tx, groupID, taskID)
	if err != nil {
		return domain.TaskWorkspaceScope{}, admissionError("读取任务目录范围失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.TaskWorkspaceScope{}, admissionError("读取任务目录范围失败", err)
	}
	return scope, nil
}

func taskWorkspaceScopeInTx(ctx context.Context, tx *sql.Tx, groupID, taskID string) (domain.TaskWorkspaceScope, error) {
	supported, err := chatWorkspaceSupportedInTx(ctx, tx)
	if err != nil {
		return domain.TaskWorkspaceScope{}, err
	}
	if !supported {
		return domain.TaskWorkspaceScope{}, ErrNotFound
	}
	var scope domain.TaskWorkspaceScope
	err = tx.QueryRowContext(ctx, `SELECT task_id,group_id,topic_id,task_revision,mandate_id,candidate_id,assessment_id,dispatch_request_id,policy_revision,resource_key,node_id
	 FROM chat_workspace_scopes WHERE group_id=? AND task_id=?`, groupID, taskID).Scan(&scope.TaskID, &scope.GroupID, &scope.TopicID, &scope.TaskRevision, &scope.MandateID, &scope.CandidateID, &scope.AssessmentID, &scope.DispatchRequestID, &scope.PolicyRevision, &scope.ResourceKey, &scope.NodeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return domain.TaskWorkspaceScope{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT grant_id,member_id,root,resolved_root,grant_schema_version
	 FROM chat_workspace_scope_grants WHERE task_id=? AND group_id=? ORDER BY member_id`, taskID, groupID)
	if err != nil {
		return domain.TaskWorkspaceScope{}, err
	}
	defer rows.Close()
	scope.Grants = []domain.TaskWorkspaceGrant{}
	for rows.Next() {
		var grant domain.TaskWorkspaceGrant
		if err := rows.Scan(&grant.GrantID, &grant.MemberID, &grant.Root, &grant.ResolvedRoot, &grant.GrantSchemaVersion); err != nil {
			return domain.TaskWorkspaceScope{}, err
		}
		scope.Grants = append(scope.Grants, grant)
	}
	if err := rows.Err(); err != nil {
		return domain.TaskWorkspaceScope{}, err
	}
	if len(scope.Grants) == 0 {
		return domain.TaskWorkspaceScope{}, ErrConflict
	}
	return scope, nil
}

// Callers first validate their bound role/session/claim in this same tx. This
// checks scope membership only; physical identity and resource locking are still
// mandatory, and this must never be exposed as a member authorization entry.
func taskWorkspaceGrantInTx(ctx context.Context, tx *sql.Tx, groupID, taskID, memberID string) (domain.TaskWorkspaceGrant, error) {
	scope, err := taskWorkspaceScopeInTx(ctx, tx, groupID, taskID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			err = ErrMemberToolDenied
		}
		return domain.TaskWorkspaceGrant{}, err
	}
	for _, grant := range scope.Grants {
		if grant.MemberID != memberID {
			continue
		}
		var valid bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_write_grants g
		 JOIN members m ON m.group_id=g.group_id AND m.id=g.member_id AND m.status='active'
		 WHERE g.id=? AND g.group_id=? AND g.member_id=? AND g.resource_key=? AND g.node_id=?
		 AND g.root=? AND g.resolved_root=? AND g.scope='read_write' AND g.authorized_by='user')`, grant.GrantID, groupID, memberID, scope.ResourceKey, scope.NodeID, grant.Root, grant.ResolvedRoot).Scan(&valid)
		if err != nil {
			return domain.TaskWorkspaceGrant{}, err
		}
		if valid {
			return grant, nil
		}
	}
	return domain.TaskWorkspaceGrant{}, ErrMemberToolDenied
}

// Compatibility follows migration history; damaged current tables are errors.
func chatWorkspaceSupportedInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var supported bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=20)").Scan(&supported)
	return supported, err
}
