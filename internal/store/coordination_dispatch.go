package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

// RequestDispatch records a trusted local-user intent without waking any model.
func (s *Store) RequestDispatch(ctx context.Context, groupID, taskID string, expectedTaskRevision, expectedAuthorityGeneration int64, reason, key string) (domain.DispatchRequest, error) {
	if !validTriggerID(groupID) || !validTriggerID(taskID) || expectedTaskRevision < 1 || expectedAuthorityGeneration < 1 || strings.TrimSpace(reason) == "" || strings.TrimSpace(key) == "" {
		return domain.DispatchRequest{}, fmt.Errorf("%w：分派请求标识、预期版本、原因或幂等键无效", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.DispatchRequest{}, admissionError("请求分派失败", err)
	}
	defer tx.Rollback()
	// Current scope must precede even historical receipt/conflict disclosure.
	if err := requireMessageActor(ctx, tx, groupID, "user"); err != nil {
		return domain.DispatchRequest{}, admissionError("验证分派请求权限失败", err)
	}
	payload := mutationPayload(groupID, taskID, strconv.FormatInt(expectedTaskRevision, 10), strconv.FormatInt(expectedAuthorityGeneration, 10), reason)
	scope := mutationScope{operation: "request_dispatch", actorID: "user", groupID: groupID, key: key}
	id, replay, err := lookupMutation(ctx, tx, scope, payload)
	if err != nil {
		return domain.DispatchRequest{}, admissionError("读取分派请求幂等键失败", err)
	}
	var request domain.DispatchRequest
	if replay {
		request, err = coordinationDispatchRequestInTx(ctx, tx, groupID, id)
	} else {
		request, err = coordinationDispatchCreateInTx(ctx, tx, groupID, taskID, expectedTaskRevision, expectedAuthorityGeneration, reason)
		if err == nil {
			err = coordinationDispatchWrite(ctx, tx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id)
			 VALUES('request_dispatch','user',?,?,?,?)`, groupID, key, payload, request.ID)
		}
	}
	if err != nil {
		return domain.DispatchRequest{}, admissionError("请求分派失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.DispatchRequest{}, admissionError("保存分派请求失败", err)
	}
	return request, nil
}

func coordinationDispatchCreateInTx(ctx context.Context, tx *sql.Tx, groupID, taskID string, taskRevision, authorityGeneration int64, reason string) (domain.DispatchRequest, error) {
	task, err := taskInTx(ctx, tx, groupID, taskID)
	if err != nil {
		return domain.DispatchRequest{}, err
	}
	if task.Status != domain.TaskStatusOpen || task.Revision != taskRevision || task.ActiveAttemptID != "" || task.CurrentSubmissionID != "" {
		return domain.DispatchRequest{}, fmt.Errorf("%w：任务状态、版本或当前引用不允许分派", ErrConflict)
	}
	var topic, mandateID, decisionID string
	var generation int64
	// Match GetTaskCoordination's exact immutable pair, within this same transaction.
	err = tx.QueryRowContext(ctx, `SELECT c.topic_id,c.current_mandate_id,c.authority_generation,d.id
	 FROM task_coordination c JOIN mandates m ON m.id=c.current_mandate_id
	 AND m.task_id=c.task_id AND m.group_id=c.group_id AND m.topic_id=c.topic_id
	 JOIN routing_decisions d ON d.id=m.decision_id AND d.mandate_id=m.id
	 AND d.trigger_id=m.origin_trigger_id AND d.task_id=m.task_id AND d.group_id=m.group_id
	 AND d.topic_id=m.topic_id AND d.task_revision_at_admission=m.task_revision_at_admission
	 WHERE c.group_id=? AND c.task_id=?`, groupID, taskID).Scan(&topic, &mandateID, &generation, &decisionID)
	if err != nil {
		return domain.DispatchRequest{}, err
	}
	admission, err := admissionReceiptInTx(ctx, tx, groupID, decisionID)
	if err != nil {
		return domain.DispatchRequest{}, err
	}
	if generation != authorityGeneration || admission.Mandate.TaskRevisionAtAdmission != taskRevision {
		return domain.DispatchRequest{}, fmt.Errorf("%w：任务授权或准入版本已变化", ErrConflict)
	}
	binding, err := coordinatorBindingInTx(ctx, tx, groupID, 0)
	if err != nil {
		return domain.DispatchRequest{}, err
	}
	policy, err := coordinatorPolicyInTx(ctx, tx, groupID, 0)
	if err != nil {
		return domain.DispatchRequest{}, err
	}
	if policy.Revision < 1 || len(policy.AllowedWorkerIDs) == 0 {
		return domain.DispatchRequest{}, fmt.Errorf("%w：分派策略没有允许执行的成员", ErrConflict)
	}
	coordinatorActive, workerEligible, err := coordinationDispatchMembersInTx(ctx, tx, groupID, binding.MemberID, policy.Revision, task.AssigneeID, admission.Mandate.Scope.AssigneeID)
	if err != nil {
		return domain.DispatchRequest{}, err
	}
	if !coordinatorActive || !workerEligible {
		return domain.DispatchRequest{}, fmt.Errorf("%w：协调员不在群内或没有符合授权的执行成员", ErrConflict)
	}
	var unfinished bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM coordination_dispatch_state
	 WHERE task_id=? AND group_id=? AND status IN ('pending','held','enqueued'))`, taskID, groupID).Scan(&unfinished); err != nil {
		return domain.DispatchRequest{}, err
	}
	if unfinished {
		return domain.DispatchRequest{}, fmt.Errorf("%w：任务已有未结束的分派请求", ErrConflict)
	}
	stamp := time.Now().UTC().Format(timestampLayout)
	createdAt, err := coordinationDispatchTime(stamp)
	if err != nil {
		return domain.DispatchRequest{}, err
	}
	request := domain.DispatchRequest{
		ID: rand.Text(), GroupID: groupID, TopicID: topic, TaskID: taskID,
		TaskRevision: taskRevision, AuthorityGeneration: authorityGeneration, MandateID: mandateID,
		CoordinatorMemberID: binding.MemberID, BindingRevision: binding.Revision, PolicyRevision: policy.Revision,
		RequestedBy: "user", Reason: reason, CreatedAt: createdAt,
	}
	if err := coordinationDispatchWrite(ctx, tx, "INSERT INTO coordination_dispatch_requests("+coordinationDispatchRequestColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
		request.ID, groupID, topic, taskID, taskRevision, authorityGeneration, mandateID, binding.MemberID, binding.Revision, policy.Revision, "user", reason, stamp); err != nil {
		return domain.DispatchRequest{}, err
	}
	if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO coordination_dispatch_state
	 (request_id,task_id,group_id,generation,status,attempt_count,next_attempt_at,updated_at,last_error_code,held_reason)
	 VALUES(?,?,?,1,'pending',0,?,?,'','')`, request.ID, taskID, groupID, stamp, stamp); err != nil {
		return domain.DispatchRequest{}, err
	}
	return request, nil
}

// Membership facts are independent so callers can distinguish the reason for holding work.
func coordinationDispatchMembersInTx(ctx context.Context, tx *sql.Tx, groupID, coordinatorID string, policyRevision int64, taskAssigneeID, mandateAssigneeID string) (coordinatorActive, workerEligible bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT
	 EXISTS(SELECT 1 FROM members WHERE group_id=? AND id=? AND status='active'),
	 EXISTS(SELECT 1 FROM group_dispatch_policy_revisions p
	 JOIN json_each(p.allowed_worker_ids) a JOIN members w ON w.id=a.value AND w.group_id=p.group_id
	 WHERE p.group_id=? AND p.revision=? AND w.status='active' AND w.id<>?
	 AND (?='' OR w.id=?) AND (?='' OR w.id=?))`,
		groupID, coordinatorID, groupID, policyRevision, coordinatorID,
		taskAssigneeID, taskAssigneeID, mandateAssigneeID, mandateAssigneeID).Scan(&coordinatorActive, &workerEligible)
	return
}

func coordinationDispatchWrite(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w：分派请求记录未写入", ErrConflict)
	}
	return nil
}
