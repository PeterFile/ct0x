package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

// Keep the legacy N2 display separate from transaction evidence and rollback causes.
type dispatchTransactionError struct {
	cause          error
	message, phase string
	rolledBack     bool
}

func (e *dispatchTransactionError) Error() string              { return e.message }
func (e *dispatchTransactionError) Unwrap() error              { return e.cause }
func (e *dispatchTransactionError) Phase() string              { return e.phase }
func (e *dispatchTransactionError) RollbackConfirmed() bool    { return e.rolledBack }
func (e *dispatchTransactionError) CommitOutcomeUnknown() bool { return e.phase == "commit" }

func coordinationDispatchFinish(tx *sql.Tx, phase string, err error) error {
	confirmed := tx == nil
	var rollback error
	if tx != nil {
		rollback = tx.Rollback()
		confirmed = rollback == nil
	}
	if err == nil {
		return nil
	}
	// Normalize NoRows before wrapping evidence; never normalize a joined rollback error.
	legacy := storeError("物化分派请求失败", err)
	cause := legacy
	if rollback != nil {
		cause = errors.Join(cause, rollback)
	}
	return &dispatchTransactionError{cause: cause, message: legacy.Error(), phase: phase, rolledBack: confirmed && phase != "commit"}
}

// MaterializeDispatchRequest is trusted composition, not a model or HTTP command.
// Its receipt, not the user-controlled create_message namespace, provides idempotency.
func (s *Store) MaterializeDispatchRequest(ctx context.Context, requestID string, expectedGeneration int64, at time.Time) (out domain.DispatchMaterialization, err error) {
	phase := "input"
	var tx *sql.Tx
	defer func() { err = coordinationDispatchFinish(tx, phase, err) }()
	stamp, err := coordinationDispatchInput(requestID, expectedGeneration, at)
	if err != nil {
		return out, err
	}
	phase = "begin"
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	phase = "source"
	r, state, err := coordinationDispatchLoadInTx(ctx, tx, requestID)
	if err != nil {
		return out, err
	}
	if state.Delivery == nil || state.Delivery.MaterializedFromGeneration != expectedGeneration {
		if err := coordinationDispatchPending(state, expectedGeneration); err != nil {
			return out, err
		}
		phase = "eligibility"
		held, err := coordinationDispatchEligibility(ctx, tx, r)
		if err != nil {
			return out, err
		}
		var body string
		if held == "" {
			phase = "payload"
			body, held, err = coordinationDispatchBody(ctx, tx, r)
			if err != nil {
				return out, err
			}
		}
		state.Generation++
		state.LastErrorCode, state.HeldReason = "", held
		if held != "" {
			phase = "held"
			state.Status = domain.DispatchStatusHeld
			err = coordinationDispatchWrite(ctx, tx, `UPDATE coordination_dispatch_state SET generation=generation+1,
			 status='held',held_reason=?,last_error_code='',updated_at=? WHERE request_id=? AND generation=? AND status='pending'`, held, stamp, r.ID, expectedGeneration)
		} else {
			phase = "message"
			m, createErr := createMessageInTx(ctx, tx, MessageRequest{GroupID: r.GroupID, TopicID: r.TopicID,
				ActorID: "user", Content: body, RecipientIDs: []string{r.CoordinatorMemberID}}, []string{r.CoordinatorMemberID})
			if createErr != nil {
				return out, createErr
			}
			if m.ID == "" || m.GroupID != r.GroupID || m.TopicID != r.TopicID || m.SenderID != "user" || len(m.Deliveries) != 1 {
				return out, fmt.Errorf("%w：分派消息或单一投递未写入", ErrConflict)
			}
			d := m.Deliveries[0]
			if d.ID == "" || d.ConversationID == "" || d.MessageID != m.ID || d.RecipientID != r.CoordinatorMemberID {
				return out, fmt.Errorf("%w：分派投递身份不匹配", ErrConflict)
			}
			if code, err := coordinationDispatchEligibility(ctx, tx, r); err != nil || code != "" {
				return out, coordinationDispatchFenceError(code, err)
			}
			state.Status = domain.DispatchStatusEnqueued
			state.Delivery = &domain.DispatchDeliveryReceipt{MessageID: m.ID, DeliveryID: d.ID, ConversationID: d.ConversationID,
				CoordinatorMemberID: d.RecipientID, MaterializedFromGeneration: expectedGeneration, CreatedAt: at.UTC()}
			phase = "receipt"
			err = coordinationDispatchWrite(ctx, tx, `UPDATE coordination_dispatch_state SET generation=generation+1,
			 status='enqueued',last_error_code='',held_reason='',updated_at=?,message_id=?,delivery_id=?,conversation_id=?,
			 delivered_member_id=?,materialized_from_generation=?,materialized_at=?
			 WHERE request_id=? AND generation=? AND status='pending'`, stamp, m.ID, d.ID, d.ConversationID, d.RecipientID, expectedGeneration, stamp, r.ID, expectedGeneration)
		}
		if err != nil {
			return out, err
		}
		phase = "readback"
		actual, err := coordinationDispatchStateInTx(ctx, tx, r)
		if err != nil {
			return out, err
		}
		if !coordinationDispatchSameState(actual, state) {
			return out, fmt.Errorf("%w：分派状态回执未保存", ErrConflict)
		}
		if state.Delivery != nil {
			if err := coordinationDispatchMaterializedInTx(ctx, tx, r, *state.Delivery, body); err != nil {
				return out, err
			}
			if code, err := coordinationDispatchEligibility(ctx, tx, r); err != nil || code != "" {
				return out, coordinationDispatchFenceError(code, err)
			}
		}
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, storeError("保存分派物化失败", err)
	}
	return domain.DispatchMaterialization{Disposition: string(state.Status), Generation: state.Generation, Delivery: state.Delivery, HeldReason: state.HeldReason}, nil
}

// Read after the receipt write, only for new materializations; historical replays may be claimed.
func coordinationDispatchMaterializedInTx(ctx context.Context, tx *sql.Tx, r domain.DispatchRequest, receipt domain.DispatchDeliveryReceipt, body string) error {
	m, err := messageInTx(ctx, tx, r.GroupID, receipt.MessageID)
	if err != nil {
		return err
	}
	if m.ID != receipt.MessageID || m.GroupID != r.GroupID || m.TopicID != r.TopicID || m.SenderID != "user" || m.Content != body || len(m.Deliveries) != 1 {
		return fmt.Errorf("%w：分派消息或单一投递未保存", ErrConflict)
	}
	d := m.Deliveries[0]
	if d.ID != receipt.DeliveryID || d.MessageID != m.ID || d.ConversationID != receipt.ConversationID || d.RecipientID != receipt.CoordinatorMemberID ||
		d.RecipientID != r.CoordinatorMemberID || d.State != domain.DeliveryStateQueued || d.TurnID != "" || d.Error != "" {
		return fmt.Errorf("%w：分派投递身份或排队状态不匹配", ErrConflict)
	}
	var logged bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM message_log
	 WHERE message_id=? AND group_id=? AND topic_id=? AND sequence>0)`, receipt.MessageID, r.GroupID, r.TopicID).Scan(&logged); err != nil {
		return err
	}
	if !logged {
		return fmt.Errorf("%w：分派消息序号映射未保存或不匹配", ErrConflict)
	}
	return nil
}

func coordinationDispatchInput(id string, generation int64, at time.Time) (string, error) {
	if !validTriggerID(id) || generation < 1 {
		return "", fmt.Errorf("%w：分派请求标识或世代无效", ErrInvalid)
	}
	stamp := at.UTC().Format(timestampLayout)
	_, err := coordinationDispatchTime(stamp)
	return stamp, err
}

func coordinationDispatchLoadInTx(ctx context.Context, tx *sql.Tx, id string) (domain.DispatchRequest, domain.DispatchState, error) {
	var groupID string
	if err := tx.QueryRowContext(ctx, "SELECT group_id FROM coordination_dispatch_requests WHERE id=?", id).Scan(&groupID); err != nil {
		return domain.DispatchRequest{}, domain.DispatchState{}, err
	}
	r, err := coordinationDispatchRequestInTx(ctx, tx, groupID, id)
	if err != nil {
		return r, domain.DispatchState{}, err
	}
	state, err := coordinationDispatchStateInTx(ctx, tx, r)
	return r, state, err
}

func coordinationDispatchPending(state domain.DispatchState, generation int64) error {
	if state.Status != domain.DispatchStatusPending || state.Generation != generation {
		return fmt.Errorf("%w：分派状态或世代已变化", ErrConflict)
	}
	if generation == math.MaxInt64 {
		return fmt.Errorf("%w：分派世代溢出", ErrInvalid)
	}
	return nil
}

func coordinationDispatchSameState(a, b domain.DispatchState) bool {
	if (a.Delivery == nil) != (b.Delivery == nil) || a.Delivery != nil && *a.Delivery != *b.Delivery {
		return false
	}
	a.Delivery, b.Delivery = nil, nil
	return a == b
}

func coordinationDispatchFenceError(code string, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%w：分派写入期间资格已变化（%s）", ErrConflict, code)
}

// All three fences share this metadata-only check and its fixed held precedence.
func coordinationDispatchEligibility(ctx context.Context, tx *sql.Tx, r domain.DispatchRequest) (string, error) {
	var taskOK, authorityOK, bindingOK, policyOK bool
	var taskAssignee, mandateAssignee string
	err := tx.QueryRowContext(ctx, `SELECT
	 EXISTS(SELECT 1 FROM tasks t WHERE t.id=r.task_id AND t.group_id=r.group_id AND t.status='open'
	 AND t.revision=r.task_revision AND t.active_attempt_id IS NULL AND t.current_submission_id IS NULL),
	 EXISTS(SELECT 1 FROM task_coordination c JOIN mandates m ON m.id=c.current_mandate_id
	 AND m.task_id=c.task_id AND m.group_id=c.group_id AND m.topic_id=c.topic_id
	 JOIN routing_decisions d ON d.id=m.decision_id AND d.mandate_id=m.id AND d.trigger_id=m.origin_trigger_id
	 AND d.task_id=m.task_id AND d.group_id=m.group_id AND d.topic_id=m.topic_id
	 AND d.task_revision_at_admission=m.task_revision_at_admission
	 WHERE c.task_id=r.task_id AND c.group_id=r.group_id AND c.topic_id=r.topic_id
	 AND c.current_mandate_id=r.mandate_id AND c.authority_generation=r.authority_generation
	 AND m.task_revision_at_admission=r.task_revision),
	 EXISTS(SELECT 1 FROM group_coordinator_bindings b JOIN group_coordinator_binding_revisions h USING(group_id,revision)
	 WHERE b.group_id=r.group_id AND b.revision=r.binding_revision AND h.member_id=r.coordinator_member_id),
	 EXISTS(SELECT 1 FROM group_dispatch_policies p JOIN group_dispatch_policy_revisions h USING(group_id,revision)
	 WHERE p.group_id=r.group_id AND p.revision=r.policy_revision),
	 coalesce((SELECT assignee_id FROM tasks WHERE id=r.task_id AND group_id=r.group_id),''),
	 coalesce((SELECT assignee_id FROM mandates WHERE id=r.mandate_id AND group_id=r.group_id),'')
	 FROM coordination_dispatch_requests r WHERE r.id=? AND r.group_id=?`, r.ID, r.GroupID).Scan(
		&taskOK, &authorityOK, &bindingOK, &policyOK, &taskAssignee, &mandateAssignee)
	if err != nil {
		return "", err
	}
	for i, ok := range []bool{taskOK, authorityOK, bindingOK, policyOK} {
		if !ok {
			return []string{domain.DispatchHeldTaskStale, domain.DispatchHeldAuthorityStale, domain.DispatchHeldBindingStale, domain.DispatchHeldPolicyStale}[i], nil
		}
	}
	coordinator, worker, err := coordinationDispatchMembersInTx(ctx, tx, r.GroupID, r.CoordinatorMemberID, r.PolicyRevision, taskAssignee, mandateAssignee)
	if err != nil {
		return "", err
	}
	if !coordinator {
		return domain.DispatchHeldCoordinatorInactive, nil
	}
	if !worker {
		return domain.DispatchHeldNoEligibleWorker, nil
	}
	return "", nil
}

func coordinationDispatchBody(ctx context.Context, tx *sql.Tx, r domain.DispatchRequest) (string, string, error) {
	const template = "系统根据本地用户调度命令及历史 Mandate 生成以下分派表示，并非用户逐字原话。\nDispatchRequest: %s\nTask: %s\nMandate: %s\n目标：\n%s\n验收条件：\n%s\n描述及原因见上述引用。"
	base := fmt.Sprintf(template, r.ID, r.TaskID, r.MandateID, "", "")
	if !utf8.ValidString(base) {
		return "", domain.DispatchHeldPayloadNotEncodable, nil
	}
	var objective, acceptance sql.NullString
	// CASE bounds transferred bytes before loading either authorization text; BLOB length includes NULs.
	err := tx.QueryRowContext(ctx, `SELECT
	 CASE WHEN length(CAST(objective AS BLOB))+length(CAST(acceptance AS BLOB))<=? THEN objective END,
	 CASE WHEN length(CAST(objective AS BLOB))+length(CAST(acceptance AS BLOB))<=? THEN acceptance END
	 FROM mandates WHERE id=? AND group_id=? AND task_id=? AND topic_id=? AND task_revision_at_admission=?`,
		32*1024-len(base), 32*1024-len(base), r.MandateID, r.GroupID, r.TaskID, r.TopicID, r.TaskRevision).Scan(&objective, &acceptance)
	if err != nil {
		return "", "", err
	}
	if !objective.Valid || !acceptance.Valid {
		return "", domain.DispatchHeldPayloadTooLarge, nil
	}
	body := fmt.Sprintf(template, r.ID, r.TaskID, r.MandateID, objective.String, acceptance.String)
	if !utf8.ValidString(body) {
		return "", domain.DispatchHeldPayloadNotEncodable, nil
	}
	return body, "", nil
}
