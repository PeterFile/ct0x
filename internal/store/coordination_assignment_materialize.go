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

// This private error is specific to these trusted primitives. A raw SQLite code
// identifies a cause, not rollback proof; every Commit error remains unknown.
type assignmentTransactionError struct {
	cause               error
	phase               string
	unknown, rolledBack bool
}

func (e *assignmentTransactionError) Error() string {
	if e.unknown {
		return "工作分配提交结果未确认，请按原世代对账"
	}
	return "工作分配事务失败"
}
func (e *assignmentTransactionError) Unwrap() error              { return e.cause }
func (e *assignmentTransactionError) Phase() string              { return e.phase }
func (e *assignmentTransactionError) CommitOutcomeUnknown() bool { return e.unknown }
func (e *assignmentTransactionError) RollbackConfirmed() bool    { return e.rolledBack }

func coordinationAssignmentFinish(tx *sql.Tx, phase string, err error) error {
	known := tx == nil
	if tx != nil {
		rollback := tx.Rollback()
		known = rollback == nil
		if rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
			err = errors.Join(err, rollback)
		}
	}
	if err == nil {
		return nil
	}
	if phase == "source" && err == sql.ErrNoRows {
		err = ErrNotFound
	}
	return &assignmentTransactionError{cause: err, phase: phase, unknown: phase == "commit", rolledBack: known && phase != "commit"}
}

type assignmentSnapshot struct {
	a               domain.Assignment
	r               domain.DispatchRequest
	coordinatorTurn string
}

// These relations describe immutable provenance, not a live coordinator scope.
// Receipt candidates are excluded by their raw keys before any identity join can
// hide a damaged second role. Runtime delivery/Turn states are deliberately absent.
const coordinationAssignmentRelations = ` FROM coordination_assignments a
 JOIN coordination_decisions c ON c.id=a.decision_id
 JOIN coordination_dispatch_requests r ON r.id=a.request_id
 JOIN coordination_dispatch_state ds ON ds.request_id=r.id
 JOIN coordination_assignment_dispatch s ON s.assignment_id=a.id
 JOIN tasks t ON t.id=a.task_id AND t.group_id=a.group_id
 JOIN mandates m ON m.id=r.mandate_id
 JOIN routing_decisions rd ON rd.id=m.decision_id
 JOIN group_coordinator_binding_revisions h ON h.group_id=r.group_id AND h.revision=r.binding_revision
 JOIN group_dispatch_policy_revisions p ON p.group_id=r.group_id AND p.revision=r.policy_revision
 JOIN turns ct ON ct.id=c.coordinator_turn_id
 JOIN deliveries cd ON cd.id=ds.delivery_id
 JOIN messages cm ON cm.id=cd.message_id
 JOIN conversations cc ON cc.id=cd.conversation_id
 WHERE a.id=:assignment AND a.request_id=:request AND a.decision_id=:decision AND a.task_id=:task
 AND a.group_id=:grp AND a.worker_member_id=:worker AND a.created_at=:assigned
 AND r.id=a.request_id AND r.task_id=a.task_id AND r.group_id=a.group_id
 AND r.topic_id=:topic AND r.task_revision=:revision AND r.authority_generation=:authority
 AND r.mandate_id=:mandate AND r.coordinator_member_id=:coordinator
 AND r.binding_revision=:binding AND r.policy_revision=:policy AND r.requested_by='user'
 AND c.request_id=r.id AND c.task_id=r.task_id AND c.group_id=r.group_id
 AND c.kind='assign' AND c.assignment_id=a.id AND c.worker_member_id=a.worker_member_id
 AND c.coordinator_member_id=r.coordinator_member_id AND c.coordinator_turn_id=:coordinator_turn
 AND a.worker_member_id<>r.coordinator_member_id
 AND ds.task_id=r.task_id AND ds.group_id=r.group_id AND ds.status='assigned' AND ds.decision_id=c.id
 AND s.task_id=a.task_id AND s.group_id=a.group_id AND s.worker_member_id=a.worker_member_id
 AND ct.delivery_id=cd.id AND cd.turn_id=ct.id AND ct.group_id=r.group_id
 AND ct.member_id=r.coordinator_member_id AND ct.conversation_id=cd.conversation_id
 AND cd.group_id=r.group_id AND cd.topic_id=r.topic_id AND cd.message_id=ds.message_id
 AND cd.conversation_id=ds.conversation_id AND cd.recipient_id=ds.delivered_member_id AND cd.recipient_id=r.coordinator_member_id
 AND cm.group_id=r.group_id AND cm.topic_id=r.topic_id AND cm.sender_id='user'
 AND cc.group_id=r.group_id AND cc.topic_id=r.topic_id AND cc.member_id=r.coordinator_member_id
 AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.delivery_id=cd.id AND other.id<>ct.id)
 AND NOT EXISTS(SELECT 1 FROM coordination_assignment_dispatch other WHERE other.delivery_id=cd.id OR other.message_id=cd.message_id)
 AND m.task_id=r.task_id AND m.group_id=r.group_id AND m.topic_id=r.topic_id AND m.task_revision_at_admission=r.task_revision
 AND rd.mandate_id=m.id AND rd.trigger_id=m.origin_trigger_id AND rd.task_id=m.task_id
 AND rd.group_id=m.group_id AND rd.topic_id=m.topic_id AND rd.task_revision_at_admission=m.task_revision_at_admission
 AND m.authority_kind='local_user_explicit' AND m.authorized_by='user' AND m.strength='explicit'
 AND m.policy_revision='local-user-explicit/v1' AND rd.actor_id='user' AND rd.policy_revision=m.policy_revision
 AND h.member_id=r.coordinator_member_id
 AND EXISTS(SELECT 1 FROM members WHERE id=r.coordinator_member_id)
 AND EXISTS(SELECT 1 FROM members WHERE id=a.worker_member_id)
 AND EXISTS(SELECT 1 FROM json_each(p.allowed_worker_ids) WHERE type='text' AND value=a.worker_member_id)
 AND (m.assignee_id='' OR m.assignee_id=a.worker_member_id)
 AND (s.delivery_id IS NULL OR EXISTS(SELECT 1 FROM deliveries wd JOIN messages wm ON wm.id=wd.message_id
 JOIN conversations wc ON wc.id=wd.conversation_id
 WHERE wd.id=s.delivery_id AND wd.group_id=a.group_id AND wd.topic_id=r.topic_id AND wd.message_id=s.message_id
 AND wd.conversation_id=s.conversation_id AND wd.recipient_id=s.delivered_member_id AND wd.recipient_id=a.worker_member_id
 AND wm.group_id=a.group_id AND wm.topic_id=r.topic_id AND wm.sender_id=c.coordinator_member_id
 AND wc.group_id=a.group_id AND wc.topic_id=r.topic_id AND wc.member_id=a.worker_member_id
 AND NOT EXISTS(SELECT 1 FROM coordination_dispatch_state other WHERE other.delivery_id=wd.id OR other.message_id=wd.message_id)
 AND ((wd.turn_id='' AND NOT EXISTS(SELECT 1 FROM turns wt WHERE wt.delivery_id=wd.id))
 OR EXISTS(SELECT 1 FROM turns wt WHERE wt.id=wd.turn_id AND wt.delivery_id=wd.id AND wt.group_id=wd.group_id
 AND wt.member_id=wd.recipient_id AND wt.conversation_id=wd.conversation_id
 AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.delivery_id=wd.id AND other.id<>wt.id)))))
 AND NOT EXISTS(SELECT 1 FROM coordination_assignment_attempts b WHERE b.assignment_id=a.id AND NOT
 (s.status='enqueued' AND b.group_id=a.group_id AND b.task_id=a.task_id AND b.worker_member_id=a.worker_member_id
 AND b.task_revision=r.task_revision AND EXISTS(SELECT 1 FROM task_attempts ba JOIN turns bt ON bt.id=b.turn_id
 JOIN deliveries bd ON bd.id=s.delivery_id WHERE ba.id=b.attempt_id AND ba.turn_id=bt.id AND ba.task_id=a.task_id
 AND ba.group_id=a.group_id AND ba.actor_id=a.worker_member_id AND ba.task_revision=b.task_revision
 AND bt.delivery_id=bd.id AND bd.turn_id=bt.id AND bt.group_id=a.group_id AND bt.member_id=a.worker_member_id
 AND bt.conversation_id=s.conversation_id)))
 AND NOT EXISTS(SELECT 1 FROM task_attempts ha LEFT JOIN turns ht ON ht.id=ha.turn_id
 WHERE ha.group_id=a.group_id AND ha.task_id=a.task_id AND ha.turn_id IS NOT NULL
 AND (ht.id IS NULL OR ht.group_id<>ha.group_id OR (ha.actor_id<>'user' AND ht.member_id<>ha.actor_id)))`

// Used verbatim by both the metadata read and the final conditional UPDATE.
// History is Task-wide: no worker, revision, timestamp, or recent-N restriction.
// The binding FK and globally unique Assignment.task_id make key lookup sufficient.
const coordinationAssignmentEligibility = `CASE
 WHEN t.status<>'open' OR t.revision<>r.task_revision OR t.active_attempt_id IS NOT NULL OR t.current_submission_id IS NOT NULL
 OR (coalesce(t.assignee_id,'')<>'' AND t.assignee_id<>a.worker_member_id)
 OR EXISTS(SELECT 1 FROM coordination_assignment_attempts b WHERE b.assignment_id=a.id)
 OR EXISTS(SELECT 1 FROM task_attempts ha JOIN turns ht ON ht.id=ha.turn_id
 WHERE ha.group_id=a.group_id AND ha.task_id=a.task_id AND ht.state IN ('starting','running','cancel_requested','unknown')) THEN 'task_stale'
 WHEN NOT EXISTS(SELECT 1 FROM task_coordination tc WHERE tc.task_id=r.task_id AND tc.group_id=r.group_id AND tc.topic_id=r.topic_id
 AND tc.current_mandate_id=r.mandate_id AND tc.authority_generation=r.authority_generation) THEN 'authority_stale'
 WHEN NOT EXISTS(SELECT 1 FROM group_coordinator_bindings b WHERE b.group_id=r.group_id AND b.revision=r.binding_revision) THEN 'binding_stale'
 WHEN NOT EXISTS(SELECT 1 FROM group_dispatch_policies cp WHERE cp.group_id=r.group_id AND cp.revision=r.policy_revision) THEN 'policy_stale'
 WHEN NOT EXISTS(SELECT 1 FROM members co WHERE co.id=r.coordinator_member_id AND co.group_id=r.group_id AND co.status='active') THEN 'coordinator_inactive'
 WHEN NOT EXISTS(SELECT 1 FROM members w WHERE w.id=a.worker_member_id AND w.group_id=a.group_id AND w.status='active') THEN 'worker_inactive'
 ELSE '' END`

func (v assignmentSnapshot) args() []any {
	a, r := v.a, v.r
	return []any{sql.Named("assignment", a.ID), sql.Named("request", a.RequestID), sql.Named("decision", a.DecisionID),
		sql.Named("task", a.TaskID), sql.Named("grp", a.GroupID), sql.Named("worker", a.WorkerMemberID), sql.Named("assigned", a.CreatedAt.Format(timestampLayout)),
		sql.Named("topic", r.TopicID), sql.Named("revision", r.TaskRevision), sql.Named("authority", r.AuthorityGeneration), sql.Named("mandate", r.MandateID),
		sql.Named("coordinator", r.CoordinatorMemberID), sql.Named("binding", r.BindingRevision), sql.Named("policy", r.PolicyRevision), sql.Named("coordinator_turn", v.coordinatorTurn)}
}

func coordinationAssignmentLoad(ctx context.Context, tx *sql.Tx, id string) (v assignmentSnapshot, state domain.AssignmentDispatchState, err error) {
	a, r := &v.a, &v.r
	var assigned, requested string
	err = tx.QueryRowContext(ctx, `SELECT a.id,a.request_id,a.decision_id,a.task_id,a.group_id,a.worker_member_id,a.created_at,
 r.id,r.group_id,r.topic_id,r.task_id,r.task_revision,r.authority_generation,r.mandate_id,r.coordinator_member_id,r.binding_revision,r.policy_revision,r.created_at,c.coordinator_turn_id
 FROM coordination_assignments a LEFT JOIN coordination_dispatch_requests r ON r.id=a.request_id
 LEFT JOIN coordination_decisions c ON c.id=a.decision_id WHERE a.id=?`, id).Scan(
		&a.ID, &a.RequestID, &a.DecisionID, &a.TaskID, &a.GroupID, &a.WorkerMemberID, &assigned, &r.ID, &r.GroupID, &r.TopicID, &r.TaskID,
		&r.TaskRevision, &r.AuthorityGeneration, &r.MandateID, &r.CoordinatorMemberID, &r.BindingRevision, &r.PolicyRevision, &requested, &v.coordinatorTurn)
	if err != nil {
		return
	}
	if a.CreatedAt, err = coordinationDispatchTime(assigned); err != nil {
		return
	}
	if r.CreatedAt, err = coordinationDispatchTime(requested); err != nil {
		return
	}
	if err = coordinationAssignmentSource(ctx, tx, v); err != nil {
		return
	}
	state, err = coordinationAssignmentState(ctx, tx, v.a)
	return
}

func coordinationAssignmentSource(ctx context.Context, tx *sql.Tx, v assignmentSnapshot) error {
	var valid bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1"+coordinationAssignmentRelations+")", v.args()...).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrInvalid
	}
	return nil
}

func coordinationAssignmentState(ctx context.Context, tx *sql.Tx, a domain.Assignment) (d domain.AssignmentDispatchState, err error) {
	var next, updated string
	var message, delivery, conversation, worker, materialized sql.NullString
	var from sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT assignment_id,generation,status,attempt_count,next_attempt_at,updated_at,last_error_code,held_reason,
 message_id,delivery_id,conversation_id,delivered_member_id,materialized_from_generation,materialized_at
 FROM coordination_assignment_dispatch WHERE assignment_id=? AND task_id=? AND group_id=? AND worker_member_id=?`, a.ID, a.TaskID, a.GroupID, a.WorkerMemberID).Scan(
		&d.AssignmentID, &d.Generation, &d.Status, &d.AttemptCount, &next, &updated, &d.LastErrorCode, &d.HeldReason, &message, &delivery, &conversation, &worker, &from, &materialized)
	if err != nil {
		return
	}
	if d.NextAttemptAt, err = coordinationDispatchTime(next); err != nil {
		return
	}
	if d.UpdatedAt, err = coordinationDispatchTime(updated); err != nil {
		return
	}
	present := message.Valid || delivery.Valid || conversation.Valid || worker.Valid || from.Valid || materialized.Valid
	if present {
		if !message.Valid || !delivery.Valid || !conversation.Valid || !worker.Valid || !from.Valid || !materialized.Valid || worker.String != a.WorkerMemberID || from.Int64 < 1 || from.Int64 >= d.Generation {
			return d, ErrInvalid
		}
		for _, id := range []string{message.String, delivery.String, conversation.String, worker.String} {
			if !validTriggerID(id) {
				return d, ErrInvalid
			}
		}
		at, err := coordinationDispatchTime(materialized.String)
		if err != nil {
			return d, err
		}
		d.Delivery = &domain.AssignmentDeliveryReceipt{MessageID: message.String, DeliveryID: delivery.String, ConversationID: conversation.String, WorkerMemberID: worker.String, MaterializedFromGeneration: from.Int64, CreatedAt: at}
	}
	if d.Generation < 1 || d.AttemptCount < 0 {
		return d, ErrInvalid
	}
	switch d.Status {
	case domain.AssignmentDispatchStatusPending:
		if present || d.HeldReason != "" || !(d.AttemptCount == 0 && d.LastErrorCode == "" || d.AttemptCount > 0 && (d.LastErrorCode == domain.DispatchErrorSQLiteBusy || d.LastErrorCode == domain.DispatchErrorSQLiteLocked)) {
			return d, ErrInvalid
		}
	case domain.AssignmentDispatchStatusEnqueued:
		if !present || d.LastErrorCode != "" || d.HeldReason != "" {
			return d, ErrInvalid
		}
	case domain.AssignmentDispatchStatusHeld:
		if d.LastErrorCode != "" || d.HeldReason == "" {
			return d, ErrInvalid
		}
	default:
		return d, ErrInvalid
	}
	return d, nil
}

func coordinationAssignmentPending(d domain.AssignmentDispatchState, generation int64) error {
	if d.Status != domain.AssignmentDispatchStatusPending || d.Generation != generation {
		return ErrConflict
	}
	if generation == math.MaxInt64 {
		return ErrInvalid
	}
	return nil
}
func coordinationAssignmentSame(a, b domain.AssignmentDispatchState) bool {
	if (a.Delivery == nil) != (b.Delivery == nil) || a.Delivery != nil && *a.Delivery != *b.Delivery {
		return false
	}
	a.Delivery, b.Delivery = nil, nil
	return a == b
}
func coordinationAssignmentEligible(ctx context.Context, tx *sql.Tx, v assignmentSnapshot) (string, error) {
	var code string
	err := tx.QueryRowContext(ctx, "SELECT "+coordinationAssignmentEligibility+coordinationAssignmentRelations, v.args()...).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrInvalid
	}
	return code, err
}

// MaterializeAssignment is an unwired trusted local primitive. It accepts no
// caller/group/grants/token, creates no Attempt/Turn, and never retries execution.
func (s *Store) MaterializeAssignment(ctx context.Context, assignmentID string, expectedGeneration int64, at time.Time) (out domain.AssignmentMaterialization, err error) {
	phase := "input"
	var tx *sql.Tx
	defer func() { err = coordinationAssignmentFinish(tx, phase, err) }()
	stamp, err := coordinationDispatchInput(assignmentID, expectedGeneration, at)
	if err != nil {
		return out, err
	}
	phase = "begin"
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	phase = "source"
	v, state, err := coordinationAssignmentLoad(ctx, tx, assignmentID)
	if err != nil {
		return out, err
	}
	if state.Delivery == nil || state.Delivery.MaterializedFromGeneration != expectedGeneration {
		if err := coordinationAssignmentPending(state, expectedGeneration); err != nil {
			return out, err
		}
		phase = "eligibility"
		held, err := coordinationAssignmentEligible(ctx, tx, v)
		if err != nil {
			return out, err
		}
		var body string
		if held == "" {
			phase = "payload"
			body, held, err = coordinationAssignmentBody(ctx, tx, v)
			if err != nil {
				return out, err
			}
		}
		state.Generation++
		state.UpdatedAt = at.UTC()
		state.LastErrorCode, state.HeldReason = "", held
		if held != "" {
			phase = "held"
			state.Status = domain.AssignmentDispatchStatusHeld
			err = coordinationDispatchWrite(ctx, tx, `UPDATE coordination_assignment_dispatch SET generation=generation+1,
    status='held',held_reason=?,last_error_code='',updated_at=? WHERE assignment_id=? AND generation=? AND status='pending'`, held, stamp, assignmentID, expectedGeneration)
		} else {
			phase = "message"
			m, createErr := createMessageInTx(ctx, tx, MessageRequest{GroupID: v.r.GroupID, TopicID: v.r.TopicID, ActorID: v.r.CoordinatorMemberID, Content: body, RecipientIDs: []string{v.a.WorkerMemberID}}, []string{v.a.WorkerMemberID})
			if createErr != nil {
				return out, createErr
			}
			if m.ID == "" || m.GroupID != v.r.GroupID || m.TopicID != v.r.TopicID || m.SenderID != v.r.CoordinatorMemberID || len(m.Deliveries) != 1 {
				return out, ErrConflict
			}
			d := m.Deliveries[0]
			if d.ID == "" || d.ConversationID == "" || d.MessageID != m.ID || d.RecipientID != v.a.WorkerMemberID {
				return out, ErrConflict
			}
			state.Status = domain.AssignmentDispatchStatusEnqueued
			state.Delivery = &domain.AssignmentDeliveryReceipt{MessageID: m.ID, DeliveryID: d.ID, ConversationID: d.ConversationID, WorkerMemberID: d.RecipientID, MaterializedFromGeneration: expectedGeneration, CreatedAt: at.UTC()}
			phase = "receipt"
			args := append(v.args(), sql.Named("stamp", stamp), sql.Named("message", m.ID), sql.Named("delivery", d.ID), sql.Named("conversation", d.ConversationID), sql.Named("generation", expectedGeneration))
			err = coordinationDispatchWrite(ctx, tx, `UPDATE coordination_assignment_dispatch SET generation=generation+1,
    status='enqueued',last_error_code='',held_reason='',updated_at=:stamp,message_id=:message,delivery_id=:delivery,
    conversation_id=:conversation,delivered_member_id=:worker,materialized_from_generation=:generation,materialized_at=:stamp
    WHERE assignment_id=:assignment AND task_id=:task AND group_id=:grp AND worker_member_id=:worker
    AND generation=:generation AND status='pending' AND EXISTS(SELECT 1`+coordinationAssignmentRelations+` AND (`+coordinationAssignmentEligibility+`)='')`, args...)
		}
		if err != nil {
			return out, err
		}
		phase = "readback"
		actual, err := coordinationAssignmentState(ctx, tx, v.a)
		if err != nil {
			return out, err
		}
		if !coordinationAssignmentSame(actual, state) {
			return out, ErrConflict
		}
		if state.Delivery != nil {
			if err := coordinationAssignmentMaterialized(ctx, tx, v, *state.Delivery, body); err != nil {
				return out, err
			}
			code, err := coordinationAssignmentEligible(ctx, tx, v)
			if err != nil {
				return out, err
			}
			if code != "" {
				return out, ErrConflict
			}
		}
	}
	// No writes follow the final runtime-shape and eligibility reads.
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return domain.AssignmentMaterialization{Disposition: string(state.Status), Generation: state.Generation, Delivery: state.Delivery, HeldReason: state.HeldReason}, nil
}

func coordinationAssignmentBody(ctx context.Context, tx *sql.Tx, v assignmentSnapshot) (string, string, error) {
	const template = "平台根据已保存的协调决定生成以下工作分配，并非模型逐字发言。\nAssignment: %s\nDispatchRequest: %s\nTask: %s\nMandate: %s\n目标：\n%s\n验收条件：\n%s\n描述及原因见上述引用。"
	base := fmt.Sprintf(template, v.a.ID, v.r.ID, v.r.TaskID, v.r.MandateID, "", "")
	if !utf8.ValidString(base) {
		return "", domain.DispatchHeldPayloadNotEncodable, nil
	}
	var objective, acceptance sql.NullString
	// BLOB lengths include NULs. CASE bounds bytes transferred before loading text;
	// neither description nor decision/request reason is selected.
	err := tx.QueryRowContext(ctx, `SELECT
 CASE WHEN length(CAST(objective AS BLOB))+length(CAST(acceptance AS BLOB))<=? THEN objective END,
 CASE WHEN length(CAST(objective AS BLOB))+length(CAST(acceptance AS BLOB))<=? THEN acceptance END
 FROM mandates WHERE id=? AND task_id=? AND group_id=? AND topic_id=? AND task_revision_at_admission=?`,
		32768-len(base), 32768-len(base), v.r.MandateID, v.r.TaskID, v.r.GroupID, v.r.TopicID, v.r.TaskRevision).Scan(&objective, &acceptance)
	if err != nil {
		return "", "", err
	}
	if !objective.Valid || !acceptance.Valid {
		return "", domain.DispatchHeldPayloadTooLarge, nil
	}
	body := fmt.Sprintf(template, v.a.ID, v.r.ID, v.r.TaskID, v.r.MandateID, objective.String, acceptance.String)
	if !utf8.ValidString(body) {
		return "", domain.DispatchHeldPayloadNotEncodable, nil
	}
	if len(body) > 32768 {
		return "", domain.DispatchHeldPayloadTooLarge, nil
	}
	return body, "", nil
}

// This initial shape applies only to a newly written receipt, never its replay.
func coordinationAssignmentMaterialized(ctx context.Context, tx *sql.Tx, v assignmentSnapshot, receipt domain.AssignmentDeliveryReceipt, body string) error {
	m, err := messageInTx(ctx, tx, v.r.GroupID, receipt.MessageID)
	if err != nil {
		return err
	}
	if m.ID != receipt.MessageID || m.GroupID != v.r.GroupID || m.TopicID != v.r.TopicID || m.SenderID != v.r.CoordinatorMemberID || m.Content != body || len(m.Deliveries) != 1 {
		return ErrConflict
	}
	d := m.Deliveries[0]
	if d.ID != receipt.DeliveryID || d.MessageID != m.ID || d.ConversationID != receipt.ConversationID || d.RecipientID != v.a.WorkerMemberID || d.State != domain.DeliveryStateQueued || d.TurnID != "" || d.Error != "" {
		return ErrConflict
	}
	var persisted bool
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM deliveries WHERE message_id=?)=1
 AND EXISTS(SELECT 1 FROM message_log WHERE message_id=? AND group_id=? AND topic_id=?)`,
		m.ID, m.ID, v.r.GroupID, v.r.TopicID).Scan(&persisted); err != nil {
		return err
	}
	if !persisted {
		return ErrConflict
	}
	return nil
}
