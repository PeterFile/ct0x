package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"agentworkspace/internal/domain"
)

type coordinationDecideFailure struct {
	cause   error
	unknown bool
}

func (e *coordinationDecideFailure) Error() string {
	if e.unknown {
		return "协调决定提交结果未确认，请保留原幂等键"
	}
	return "协调决定或读取失败"
}
func (e *coordinationDecideFailure) Unwrap() error { return e.cause }

func coordinationDecideError(err error) error {
	if err == nil {
		return nil
	}
	var prior *coordinationDecideFailure
	if errors.As(err, &prior) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return &coordinationDecideFailure{cause: err}
}

func coordinationDecideRead[T any](ctx context.Context, s *Store, groupID, id string, load func(context.Context, *sql.Tx, string, string) (T, error)) (out T, err error) {
	defer func() { err = coordinationDecideError(err) }()
	if !validTriggerID(groupID) || !validTriggerID(id) {
		return out, ErrInvalid
	}
	tx, err := s.beginCoordinatorRead(ctx, groupID)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	value, err := load(ctx, tx, groupID, id)
	if err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return value, nil
}

// These are trusted local-user reads, never member-tool authorization.
func (s *Store) GetDispatchDecision(ctx context.Context, groupID, requestID string) (domain.DecisionReceipt, error) {
	return coordinationDecideRead(ctx, s, groupID, requestID, coordinationDecideReceiptInTx)
}

func (s *Store) GetAssignment(ctx context.Context, groupID, assignmentID string) (domain.AssignmentView, error) {
	return coordinationDecideRead(ctx, s, groupID, assignmentID, coordinationDecideAssignmentInTx)
}

func coordinationDecideReceiptInTx(ctx context.Context, tx *sql.Tx, groupID, requestID string) (domain.DecisionReceipt, error) {
	var out domain.DecisionReceipt
	d := &out.Decision
	var stamp, assigned string
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT c.id,c.request_id,c.task_id,c.group_id,c.coordinator_member_id,c.coordinator_turn_id,
	 c.kind,coalesce(c.worker_member_id,''),coalesce(c.assignment_id,''),c.reason,c.created_at,coalesce(a.created_at,''),
	 EXISTS(SELECT 1 FROM coordination_dispatch_requests r JOIN coordination_dispatch_state s ON s.request_id=r.id
	 JOIN deliveries dl ON dl.id=s.delivery_id JOIN turns t ON t.id=c.coordinator_turn_id JOIN messages msg ON msg.id=dl.message_id
	 WHERE r.id=c.request_id AND r.task_id=c.task_id AND r.group_id=c.group_id AND r.coordinator_member_id=c.coordinator_member_id
	 AND dl.turn_id=t.id AND t.delivery_id=dl.id AND t.group_id=c.group_id AND t.member_id=c.coordinator_member_id
	 AND t.conversation_id=dl.conversation_id AND dl.group_id=r.group_id AND dl.topic_id=r.topic_id
	 AND dl.message_id=s.message_id AND dl.conversation_id=s.conversation_id AND dl.recipient_id=s.delivered_member_id
	 AND dl.recipient_id=r.coordinator_member_id AND msg.group_id=r.group_id AND msg.topic_id=r.topic_id AND msg.sender_id='user'
	 AND s.task_id=c.task_id AND s.group_id=c.group_id AND s.decision_id=c.id AND
	 ((c.kind='decline' AND c.worker_member_id IS NULL AND c.assignment_id IS NULL AND s.status='declined' AND a.id IS NULL)
	 OR (c.kind='assign' AND s.status='assigned' AND a.id=c.assignment_id AND a.request_id=c.request_id AND a.decision_id=c.id
	 AND a.task_id=c.task_id AND a.group_id=c.group_id AND a.worker_member_id=c.worker_member_id)))
	 FROM coordination_decisions c LEFT JOIN coordination_assignments a ON a.request_id=c.request_id
	 WHERE c.group_id=? AND c.request_id=?`, groupID, requestID).Scan(&d.ID, &d.RequestID, &d.TaskID, &d.GroupID,
		&d.CoordinatorMemberID, &d.CoordinatorTurnID, &d.Kind, &d.WorkerMemberID, &d.AssignmentID, &d.Reason, &stamp, &assigned, &valid)
	if err != nil {
		return out, err
	}
	if !valid {
		return out, ErrInvalid
	}
	if d.CreatedAt, err = coordinationDispatchTime(stamp); err != nil {
		return out, err
	}
	if d.Kind == domain.CoordinatorDecisionKindAssign {
		at, err := coordinationDispatchTime(assigned)
		if err != nil {
			return out, err
		}
		out.Assignment = &domain.Assignment{ID: d.AssignmentID, RequestID: d.RequestID, DecisionID: d.ID,
			TaskID: d.TaskID, GroupID: d.GroupID, WorkerMemberID: d.WorkerMemberID, CreatedAt: at}
	}
	return out, nil
}

func coordinationDecideAssignmentInTx(ctx context.Context, tx *sql.Tx, groupID, id string) (out domain.AssignmentView, err error) {
	var requestID string
	if err := tx.QueryRowContext(ctx, "SELECT request_id FROM coordination_assignments WHERE group_id=? AND id=?", groupID, id).Scan(&requestID); err != nil {
		return out, err
	}
	defer func() {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrInvalid
		}
	}()
	receipt, err := coordinationDecideReceiptInTx(ctx, tx, groupID, requestID)
	if err != nil {
		return out, err
	}
	if receipt.Assignment == nil || receipt.Assignment.ID != id {
		return out, ErrInvalid
	}
	out.Assignment, out.Decision = *receipt.Assignment, receipt.Decision
	if out.Request, err = coordinationDispatchRequestInTx(ctx, tx, groupID, requestID); err != nil {
		return out, err
	}
	d, b := &out.Dispatch, &domain.AssignmentAttemptBinding{}
	var next, updated, materialized, bound, taskStamp string
	var message, delivery, conversation, member sql.NullString
	var generation sql.NullInt64
	var hasBinding, valid bool
	err = tx.QueryRowContext(ctx, `SELECT s.assignment_id,s.generation,s.status,s.attempt_count,s.next_attempt_at,s.updated_at,
	 s.last_error_code,s.held_reason,s.message_id,s.delivery_id,s.conversation_id,s.delivered_member_id,s.materialized_from_generation,
	 coalesce(s.materialized_at,''),b.assignment_id IS NOT NULL,coalesce(b.turn_id,''),coalesce(b.attempt_id,''),coalesce(b.task_revision,0),coalesce(b.created_at,''),
	 (SELECT created_at FROM tasks WHERE id=a.task_id AND group_id=a.group_id),
	 s.task_id=a.task_id AND s.group_id=a.group_id AND s.worker_member_id=a.worker_member_id AND
	 (s.delivery_id IS NULL OR EXISTS(SELECT 1 FROM deliveries dl JOIN messages msg ON msg.id=dl.message_id
	 WHERE dl.id=s.delivery_id AND dl.group_id=a.group_id AND dl.topic_id=r.topic_id AND dl.conversation_id=s.conversation_id
	 AND dl.message_id=s.message_id AND dl.recipient_id=s.delivered_member_id AND dl.recipient_id=a.worker_member_id
	 AND msg.group_id=a.group_id AND msg.topic_id=r.topic_id AND msg.sender_id=r.coordinator_member_id)) AND
	 (b.assignment_id IS NULL OR (s.status='enqueued' AND b.group_id=a.group_id AND b.task_id=a.task_id AND b.worker_member_id=a.worker_member_id
	 AND b.task_revision=r.task_revision AND EXISTS(SELECT 1 FROM task_attempts ta JOIN turns t ON t.id=b.turn_id
	 JOIN deliveries dl ON dl.id=s.delivery_id WHERE ta.id=b.attempt_id AND ta.turn_id=t.id AND ta.task_id=a.task_id
	 AND ta.group_id=a.group_id AND ta.actor_id=a.worker_member_id AND ta.task_revision=b.task_revision
	 AND t.group_id=a.group_id AND t.member_id=a.worker_member_id AND t.delivery_id=dl.id AND dl.turn_id=t.id
	 AND t.conversation_id=s.conversation_id AND dl.group_id=a.group_id AND dl.topic_id=r.topic_id
	 AND dl.message_id=s.message_id AND dl.recipient_id=s.delivered_member_id AND dl.recipient_id=a.worker_member_id)))
	 FROM coordination_assignments a JOIN coordination_dispatch_requests r ON r.id=a.request_id
	 JOIN coordination_assignment_dispatch s ON s.assignment_id=a.id LEFT JOIN coordination_assignment_attempts b ON b.assignment_id=a.id
	 WHERE a.group_id=? AND a.id=?`, groupID, id).Scan(&d.AssignmentID, &d.Generation, &d.Status, &d.AttemptCount, &next, &updated,
		&d.LastErrorCode, &d.HeldReason, &message, &delivery, &conversation, &member, &generation, &materialized,
		&hasBinding, &b.TurnID, &b.AttemptID, &b.TaskRevision, &bound, &taskStamp, &valid)
	if err != nil {
		return out, err
	}
	if !valid {
		return out, ErrInvalid
	}
	for _, field := range []struct {
		stamp string
		dest  *time.Time
	}{{next, &d.NextAttemptAt}, {updated, &d.UpdatedAt}} {
		if *field.dest, err = coordinationDispatchTime(field.stamp); err != nil {
			return out, err
		}
	}
	if message.Valid || delivery.Valid || conversation.Valid || member.Valid || generation.Valid || materialized != "" {
		if !message.Valid || !delivery.Valid || !conversation.Valid || !member.Valid || !generation.Valid || materialized == "" {
			return out, ErrInvalid
		}
		at, err := coordinationDispatchTime(materialized)
		if err != nil {
			return out, err
		}
		d.Delivery = &domain.AssignmentDeliveryReceipt{MessageID: message.String, DeliveryID: delivery.String, ConversationID: conversation.String,
			WorkerMemberID: member.String, MaterializedFromGeneration: generation.Int64, CreatedAt: at}
	}
	if hasBinding {
		b.AssignmentID, b.GroupID, b.TaskID, b.WorkerMemberID = id, groupID, out.Assignment.TaskID, out.Assignment.WorkerMemberID
		if b.CreatedAt, err = coordinationDispatchTime(bound); err != nil {
			return out, err
		}
		out.Binding = b
	}
	out.Task, err = taskInTx(ctx, tx, groupID, out.Assignment.TaskID)
	if err == nil {
		out.Task.CreatedAt, err = coordinationDispatchTime(taskStamp)
	}
	return out, err
}
