package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentworkspace/internal/domain"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const admissionDecisionColumns = "id,trigger_id,group_id,topic_id,kind,actor_id,policy_revision,reason,task_id,task_revision_at_admission,mandate_id,created_at"
const admissionMandateColumns = "id,decision_id,origin_trigger_id,group_id,topic_id,task_id,task_revision_at_admission,authority_kind,authorized_by,strength,policy_revision,objective,description,acceptance,assignee_id,dependency_ids,created_at"

// RequestWork is trusted server-local persistence. No input can select its bound user actor.
func (s *Store) RequestWork(ctx context.Context, groupID string, request domain.RequestWorkRequest, key string) (domain.AdmissionReceipt, error) {
	payload, err := requestWorkPayload(groupID, request, key)
	if err != nil {
		return domain.AdmissionReceipt{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.AdmissionReceipt{}, admissionError("请求开展工作失败", err)
	}
	defer tx.Rollback()
	// Current group access and exact source scope precede any receipt/conflict disclosure.
	if err := requireMessageActor(ctx, tx, groupID, "user"); err != nil {
		return domain.AdmissionReceipt{}, admissionError("验证工作请求权限失败", err)
	}
	trigger, err := recordLocalMessageTriggerInTx(ctx, tx, domain.ThreadRef{GroupID: groupID, TopicID: request.TopicID}, request.MessageID)
	if err != nil {
		return domain.AdmissionReceipt{}, admissionError("捕获工作来源失败", err)
	}
	scope := mutationScope{operation: "request_work", actorID: "user", groupID: groupID, key: key}
	id, replay, err := lookupMutation(ctx, tx, scope, payload)
	if err != nil {
		return domain.AdmissionReceipt{}, admissionError("读取工作请求幂等键失败", err)
	}
	if !replay {
		id = rand.Text()
		err := tx.QueryRowContext(ctx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id)
		 VALUES(?,?,?,?,?,?) RETURNING record_id`, scope.operation, scope.actorID, groupID, key, payload, id).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			err = fmt.Errorf("%w：请求回执未写入", ErrConflict)
		}
		if err != nil {
			return domain.AdmissionReceipt{}, admissionError("保存工作请求回执失败", err)
		}
		if err := admitWorkInTx(ctx, tx, trigger, request, id); err != nil {
			return domain.AdmissionReceipt{}, admissionError("授权工作失败", err)
		}
	}
	// Both initial and replay responses read only saved immutable evidence.
	receipt, err := admissionReceiptInTx(ctx, tx, groupID, id)
	if err != nil {
		return domain.AdmissionReceipt{}, admissionError("读取工作请求回执失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.AdmissionReceipt{}, admissionError("保存工作请求失败", err)
	}
	return receipt, nil
}

func requestWorkPayload(groupID string, request domain.RequestWorkRequest, key string) ([]byte, error) {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(request.Reason) == "" {
		return nil, fmt.Errorf("%w：工作请求必须提供幂等键和原因", ErrInvalid)
	}
	if err := validateTriggerThread(domain.ThreadRef{GroupID: groupID}); err != nil {
		return nil, err
	}
	if !validTriggerID(request.MessageID) {
		return nil, fmt.Errorf("%w：消息标识无效", ErrInvalid)
	}
	fields := []string{groupID, request.TopicID, request.MessageID, string(request.Mode), request.Reason}
	switch request.Mode {
	case domain.RoutingDecisionCreateNew:
		if request.NewTask == nil || request.ExistingTask != nil {
			return nil, fmt.Errorf("%w：新建模式必须只提供新任务", ErrInvalid)
		}
		n := request.NewTask
		fields = append(fields, n.Title, n.Description, n.Acceptance, n.AssigneeID)
		fields = append(fields, recipientSet(n.DependencyIDs)...)
	case domain.RoutingDecisionLinkExisting:
		if request.ExistingTask == nil || request.NewTask != nil || !validTriggerID(request.ExistingTask.TaskID) || request.ExistingTask.ExpectedRevision < 1 {
			return nil, fmt.Errorf("%w：关联模式必须只提供任务标识和正版本", ErrInvalid)
		}
		fields = append(fields, request.ExistingTask.TaskID, strconv.FormatInt(request.ExistingTask.ExpectedRevision, 10))
	default:
		return nil, fmt.Errorf("%w：工作请求模式无效", ErrInvalid)
	}
	// Length-prefixed fields retain opaque topic bytes and canonical dependency-set semantics.
	return mutationPayload(fields...), nil
}

func admitWorkInTx(ctx context.Context, tx *sql.Tx, trigger domain.Trigger, request domain.RequestWorkRequest, decisionID string) error {
	var coordinated bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM routing_decisions WHERE trigger_id=?)", trigger.ID).Scan(&coordinated); err != nil {
		return err
	}
	if coordinated {
		return fmt.Errorf("%w：来源已获授权", ErrConflict)
	}
	var task domain.Task
	var err error
	if request.Mode == domain.RoutingDecisionCreateNew {
		n := request.NewTask
		task, err = insertTaskInTx(ctx, tx, CreateTaskInput{
			GroupID: trigger.GroupID, Title: n.Title, Description: n.Description, Acceptance: n.Acceptance,
			AssigneeID: n.AssigneeID, DependencyIDs: n.DependencyIDs,
		}, rand.Text())
		if err != nil {
			return err
		}
	} else {
		task, err = taskInTx(ctx, tx, trigger.GroupID, request.ExistingTask.TaskID)
		if err != nil {
			return err
		}
		if task.Status != domain.TaskStatusOpen || task.Revision != request.ExistingTask.ExpectedRevision || task.ActiveAttemptID != "" || task.CurrentSubmissionID != "" {
			return fmt.Errorf("%w：任务状态、版本或当前引用不允许关联", ErrConflict)
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_coordination WHERE task_id=?)
		 OR EXISTS(SELECT 1 FROM mandates INDEXED BY mandates_group_task WHERE group_id=? AND task_id=?)`,
			task.ID, task.GroupID, task.ID).Scan(&coordinated); err != nil {
			return err
		}
		if coordinated {
			return fmt.Errorf("%w：任务已有授权历史", ErrConflict)
		}
	}
	dependencies, err := json.Marshal(task.DependencyIDs)
	if err != nil {
		return err
	}
	mandateID, stamp := rand.Text(), time.Now().UTC().Format(timestampLayout)
	// The v10 deferred FKs validate the exact immutable pair at COMMIT.
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO routing_decisions(" + admissionDecisionColumns + ") VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", []any{
			decisionID, trigger.ID, trigger.GroupID, trigger.TopicID, request.Mode, "user", domain.LocalUserExplicitPolicyRevision,
			request.Reason, task.ID, task.Revision, mandateID, stamp}},
		{"INSERT INTO mandates(" + admissionMandateColumns + ") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", []any{
			mandateID, decisionID, trigger.ID, trigger.GroupID, trigger.TopicID, task.ID, task.Revision,
			domain.MandateAuthorityLocalUserExplicit, "user", domain.MandateStrengthExplicit, domain.LocalUserExplicitPolicyRevision,
			task.Title, task.Description, task.Acceptance, task.AssigneeID, string(dependencies), stamp}},
		// Recheck eligibility at the write, excluding only this operation's just-inserted Mandate.
		{`INSERT INTO task_coordination(task_id,group_id,topic_id,current_mandate_id,authority_generation)
		 SELECT t.id,t.group_id,?,?,1 FROM tasks t
		 WHERE t.id=? AND t.group_id=? AND t.status='open' AND t.revision=?
		 AND t.active_attempt_id IS NULL AND t.current_submission_id IS NULL
		 AND NOT EXISTS(SELECT 1 FROM task_coordination c WHERE c.task_id=t.id)
		 AND NOT EXISTS(SELECT 1 FROM mandates m INDEXED BY mandates_group_task
		 WHERE m.group_id=t.group_id AND m.task_id=t.id AND m.id<>?)`,
			[]any{trigger.TopicID, mandateID, task.ID, trigger.GroupID, task.Revision, mandateID}},
	} {
		result, err := tx.ExecContext(ctx, statement.query, statement.args...)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("%w：授权记录未写入", ErrConflict)
		}
	}
	return nil
}

func admissionError(action string, err error) error {
	var constraint *sqlite.Error
	if errors.As(err, &constraint) && (constraint.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || constraint.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY) {
		err = fmt.Errorf("%w：%w", ErrConflict, err)
	}
	return taskError(action, err)
}
