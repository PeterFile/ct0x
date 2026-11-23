package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"agentworkspace/internal/domain"
)

const workInputSelect = `SELECT i.id,i.group_id,i.topic_id,i.task_id,i.task_revision,i.mandate_id,i.assignment_id,i.worker_member_id,
 r.trigger_id,i.source_id,t.message_sequence,a.candidate_id,r.reply_message_id,r.notice_message_id,i.content,
 i.before_revision,i.before_constraint,i.after_revision,i.after_constraint,i.created_at,c.turn_id,c.attempt_id,c.consumed_at,
 h.member_id,h.turn_id,h.attempt_id,h.reason,h.question,h.notice_message_id,h.returned_at,h.disposition,h.policy_revision,h.policy_source_id
 FROM task_work_inputs i JOIN chat_followup_routes r ON r.source_id=i.source_id
 JOIN coordination_triggers t ON t.id=r.trigger_id JOIN chat_assessments a ON a.id=r.assessment_id
 LEFT JOIN task_work_input_consumptions c ON c.input_id=i.id LEFT JOIN task_work_input_returns h ON h.input_id=i.id`

// The proposal sequence includes returns; it is not an effective constraint view.
const latestWorkConstraintProposal = ` FROM task_work_inputs WHERE group_id=? AND task_id=? AND task_revision=?
 AND after_revision>before_revision ORDER BY after_revision DESC`

func scanWorkInput(row scanner) (i domain.TaskWorkInput, err error) {
	var stamp string
	var turn, attempt, consumed sql.NullString
	var who, returnTurn, returnAttempt, reason, question, notice, returned, disposition, policySource sql.NullString
	var policyRevision sql.NullInt64
	if err = row.Scan(&i.ID, &i.GroupID, &i.TopicID, &i.TaskID, &i.TaskRevision, &i.MandateID, &i.AssignmentID, &i.WorkerMemberID,
		&i.TriggerID, &i.SourceID, &i.SourceSequence, &i.CandidateID, &i.ReplyMessageID, &i.NoticeMessageID, &i.Content,
		&i.BeforeConstraintRevision, &i.BeforeConstraint, &i.AfterConstraintRevision, &i.AfterConstraint, &stamp, &turn, &attempt, &consumed,
		&who, &returnTurn, &returnAttempt, &reason, &question, &notice, &returned, &disposition, &policyRevision, &policySource); err != nil {
		return
	}
	i.CreatedAt, err = time.Parse(timestampLayout, stamp)
	if err == nil && turn.Valid {
		i.Consumption = &domain.TaskWorkInputConsumption{TurnID: turn.String, AttemptID: attempt.String}
		i.Consumption.ConsumedAt, err = time.Parse(timestampLayout, consumed.String)
	}
	if err == nil && returned.Valid {
		i.Return = &domain.TaskWorkInputReturn{MemberID: who.String, TurnID: returnTurn.String, AttemptID: returnAttempt.String,
			Reason: reason.String, Question: question.String, NoticeMessageID: notice.String, Disposition: domain.TaskWorkInputReturnDisposition(disposition.String),
			PolicyRevision: policyRevision.Int64, PolicySourceID: policySource.String}
		i.Return.ReturnedAt, err = time.Parse(timestampLayout, returned.String)
	}
	return
}

func workInputsInTx(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]domain.TaskWorkInput, error) {
	rows, err := tx.QueryContext(ctx, workInputSelect+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []domain.TaskWorkInput{}
	for rows.Next() {
		i, err := scanWorkInput(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	return items, rows.Err()
}

// ListTaskWorkInputs is a trusted local-user read of the historical inbox. The
// cursor is display order only; it cannot acknowledge inputs.
func (s *Store) ListTaskWorkInputs(ctx context.Context, groupID, taskID string, options ListOptions) (Page[domain.TaskWorkInput], error) {
	const action = "读取任务后续输入失败"
	var page Page[domain.TaskWorkInput]
	limit, err := listLimit(options)
	if err != nil {
		return page, taskError(action, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, taskError(action, err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE group_id=? AND id=?", groupID, taskID).Scan(&exists); err != nil {
		return page, taskError(action, err)
	}
	installed, err := workInputSchemaInTx(ctx, tx)
	if err != nil {
		return page, taskError(action, err)
	}
	page.Items = []domain.TaskWorkInput{}
	if installed {
		where := " WHERE i.group_id=? AND i.task_id=?"
		args := []any{groupID, taskID}
		page, err = workInputHistoryInTx(ctx, tx, where, args, limit, options.Before)
		if err != nil {
			return Page[domain.TaskWorkInput]{}, taskError(action, err)
		}
		decisions, err := decisionAnswerInstalledInTx(ctx, tx)
		if err == nil && decisions {
			page, err = mergeDecisionAnswerWorkInputHistoryInTx(ctx, tx, ` WHERE (q.group_id,q.task_id)=(?,?) AND `+decisionAnswerProof,
				[]any{groupID, taskID}, options.Before, limit, page)
		}
		if err != nil {
			return Page[domain.TaskWorkInput]{}, taskError(action, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Page[domain.TaskWorkInput]{}, taskError(action, err)
	}
	return page, nil
}

func workInputHistoryInTx(ctx context.Context, tx *sql.Tx, where string, args []any, limit int, before *Cursor) (Page[domain.TaskWorkInput], error) {
	if before != nil {
		where += " AND (i.created_at,i.id)<(?,?)"
		args = append(args, before.CreatedAt.UTC().Format(timestampLayout), before.ID)
	}
	items, err := workInputsInTx(ctx, tx, where+" ORDER BY i.created_at DESC,i.id DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return Page[domain.TaskWorkInput]{}, err
	}
	page := Page[domain.TaskWorkInput]{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

// ListWorkInputHistoryForMember allows the exact bound owner to page through
// prior confirmations and returns beyond the bounded inbox, not chat history.
func (s *Store) ListWorkInputHistoryForMember(ctx context.Context, scope MemberToolScope, options ListOptions) (Page[domain.TaskWorkInput], error) {
	const action = "读取执行成员后续输入历史失败"
	var zero Page[domain.TaskWorkInput]
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return zero, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	role, err := s.workInputWorkerInTx(ctx, tx, scope)
	if err != nil {
		return zero, memberToolOperationError(action, err)
	}
	limit, err := listLimit(options)
	if err != nil {
		return zero, err
	}
	installed, err := workInputSchemaInTx(ctx, tx)
	if err != nil {
		return zero, memberToolOperationError(action, err)
	}
	page := Page[domain.TaskWorkInput]{Items: []domain.TaskWorkInput{}}
	if installed {
		page, err = workInputHistoryInTx(ctx, tx, workerWorkInputWhere, workerWorkInputArgs(role.Provenance), limit, options.Before)
		if err == nil {
			page, err = decisionAnswerWorkInputHistoryInTx(ctx, tx, role, options, limit, page)
		}
		if err != nil {
			return zero, memberToolOperationError(action, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return zero, memberToolOperationError(action, err)
	}
	return page, nil
}

// Derive the role and canonical Attempt, never from tool arguments. A later
// ordinary Turn of the same member or conversation has no inbox rights.
func (s *Store) workInputWorkerInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope) (CoordinationToolScope, error) {
	role, _, err := s.workInputHandlingOwnerInTx(ctx, tx, scope, false)
	return role, err
}

// Only handback can proceed without current chat execution authority. It still
// requires the live, canonical owner and never bypasses Task/Assignment fences.
func (s *Store) workInputHandlingOwnerInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, returnOnly bool) (CoordinationToolScope, assignmentSnapshot, error) {
	f, err := lookupTaskWorkExecutionInTx(ctx, tx, scope)
	if err == nil {
		state, err := s.taskWorkOwnerContinuationRoleInTx(ctx, tx, f, scope.Claim.TurnID, returnOnly)
		if err != nil {
			return CoordinationToolScope{}, assignmentSnapshot{}, err
		}
		if state.AttemptID == "" || state.Blocked != CoordinationReady {
			return CoordinationToolScope{}, assignmentSnapshot{}, ErrMemberToolDenied
		}
		return CoordinationToolScope{Member: scope, Provenance: state.Provenance, AttemptID: state.AttemptID}, f.v, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return CoordinationToolScope{}, assignmentSnapshot{}, err
	}
	return s.boundTaskWorkOriginalInTx(ctx, tx, scope, returnOnly)
}

// Only use this path after fencing the claim and ruling out a typed delivery.
func (s *Store) boundTaskWorkOriginalInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, returnOnly bool) (CoordinationToolScope, assignmentSnapshot, error) {
	var zero assignmentSnapshot
	p := CoordinationRoleProvenance{Kind: CoordinationRoleWorker, GroupID: scope.Claim.GroupID, MemberID: scope.MemberID,
		ConversationID: scope.ConversationID, TopicID: scope.TopicID}
	var own string
	err := tx.QueryRowContext(ctx, `SELECT b.assignment_id,b.task_id,b.task_revision,b.attempt_id,a.request_id,d.delivery_id,d.message_id
	 FROM coordination_assignment_attempts b JOIN coordination_assignments a ON a.id=b.assignment_id
	 JOIN coordination_assignment_dispatch d ON d.assignment_id=b.assignment_id
	 WHERE b.turn_id=? AND b.group_id=? AND b.worker_member_id=? AND d.status='enqueued'`, scope.Claim.TurnID, p.GroupID, p.MemberID).Scan(
		&p.AssignmentID, &p.TaskID, &p.TaskRevision, &own, &p.RequestID, &p.DeliveryID, &p.MessageID)
	if err != nil {
		return CoordinationToolScope{}, zero, coordinationRoleRelationError(err)
	}
	role := CoordinationToolScope{Member: scope, Provenance: p, AttemptID: own}
	v, view, err := coordinationAssignmentWorkerInTx(ctx, tx, role)
	if err != nil {
		return CoordinationToolScope{}, zero, coordinationRoleRelationError(err)
	}
	if !coordinationAssignmentBound(role, view.Binding) {
		return CoordinationToolScope{}, zero, ErrMemberToolDenied
	}
	if err := coordinationAssignmentStartInTx(ctx, tx, v, role, own); err != nil {
		if errors.Is(err, ErrConflict) {
			err = ErrMemberToolDenied
		}
		return CoordinationToolScope{}, zero, err
	}
	if !returnOnly {
		f := taskWorkFacts{v: v, d: taskWorkRecord(v, "", "")}
		if _, err := s.taskWorkOwnerChatActionInTx(ctx, tx, f); err != nil {
			if errors.Is(err, errTaskWorkChatScope) || errors.Is(err, ErrTaskInvestigationNotReady) {
				err = ErrMemberToolDenied
			}
			return CoordinationToolScope{}, zero, err
		}
	}
	return role, v, nil
}

const workerWorkInputWhere = ` WHERE i.assignment_id=? AND i.task_revision=? AND i.group_id=? AND i.topic_id=? AND i.task_id=? AND i.worker_member_id=?`

func workerWorkInputArgs(p CoordinationRoleProvenance) []any {
	return []any{p.AssignmentID, p.TaskRevision, p.GroupID, p.TopicID, p.TaskID, p.MemberID}
}

func workerWorkInputInTx(ctx context.Context, tx *sql.Tx, p CoordinationRoleProvenance, inputID, mandateID string) (domain.TaskWorkInput, error) {
	if isDecisionAnswerID(inputID) {
		return decisionAnswerWorkInputInTx(ctx, tx, p, inputID, mandateID)
	}
	i, err := scanWorkInput(tx.QueryRowContext(ctx, workInputSelect+workerWorkInputWhere+" AND i.id=? AND i.mandate_id=?",
		append(workerWorkInputArgs(p), inputID, mandateID)...))
	return i, coordinationRoleRelationError(err)
}

// ListWorkInputsForMember returns at most 50 inputs on this live worker's
// Assignment and Task revision. Unacknowledged changes come first; explicitly
// acknowledging them exposes the next batch even if recent context is busy.
func (s *Store) ListWorkInputsForMember(ctx context.Context, scope MemberToolScope) ([]domain.TaskWorkInput, error) {
	const action = "读取执行成员后续输入失败"
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return nil, memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	role, owner, err := s.workInputHandlingOwnerInTx(ctx, tx, scope, false)
	if err != nil {
		return nil, memberToolOperationError(action, err)
	}
	items := []domain.TaskWorkInput{}
	installed, err := workInputSchemaInTx(ctx, tx)
	if err == nil && installed {
		inboxLimit := 50
		var decision []domain.TaskWorkInput
		p := role.Provenance
		if p.SourceKind == domain.TaskWorkSourceFollowup && isDecisionAnswerID(p.SourceID) {
			i, err := decisionAnswerWorkInputInTx(ctx, tx, p, p.SourceID, owner.r.MandateID)
			if err != nil {
				return nil, memberToolOperationError(action, err)
			}
			decision = append(decision, i)
			inboxLimit--
		}
		args := workerWorkInputArgs(p)
		// Reserve one slot for the most recent acknowledged delta, not the
		// before/after snapshot of an acknowledged context-only input.
		var confirmed []domain.TaskWorkInput
		confirmed, err = workInputsInTx(ctx, tx, workerWorkInputWhere+` AND c.input_id IS NOT NULL AND i.after_revision>i.before_revision
		 ORDER BY i.after_revision DESC LIMIT 1`, args...)
		if err == nil {
			items, err = workInputsInTx(ctx, tx, workerWorkInputWhere+` AND i.after_revision>i.before_revision AND c.input_id IS NULL AND h.input_id IS NULL
			 ORDER BY i.after_revision LIMIT ?`, append(args, inboxLimit-len(confirmed))...)
		}
		items = append(items, confirmed...)
		if err == nil && len(items) < inboxLimit {
			var recent []domain.TaskWorkInput
			first := ""
			if len(confirmed) != 0 {
				first = confirmed[0].ID
			}
			recent, err = workInputsInTx(ctx, tx, workerWorkInputWhere+` AND i.id<>? AND (i.after_revision=i.before_revision OR c.input_id IS NOT NULL OR h.input_id IS NOT NULL)
			 ORDER BY (c.input_id IS NULL AND h.input_id IS NULL) DESC,i.created_at DESC,i.id DESC LIMIT ?`, append(args, first, inboxLimit-len(items))...)
			items = append(items, recent...)
		}
		if len(decision) != 0 {
			items = append(decision, items...)
		}
	}
	if err != nil {
		return nil, memberToolOperationError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, memberToolOperationError(action, err)
	}
	return items, nil
}

// AcknowledgeWorkInput is the trusted internal, unkeyed confirmation path.
func (s *Store) AcknowledgeWorkInput(ctx context.Context, scope MemberToolScope, inputID string) (domain.TaskWorkInput, error) {
	return s.handleWorkInput(ctx, scope, workInputAckOperation, inputID, "", "")
}

// AcknowledgeWorkInputWithKey records explicit handling, not acceptance or
// authority beyond the Mandate. The key is 1..128 visible ASCII bytes, excluding
// comma, and is scoped to the bound member, group, and operation. A different
// input or Assignment under that key conflicts. Replay still requires a live
// authorized owner of the same Assignment and Task revision.
func (s *Store) AcknowledgeWorkInputWithKey(ctx context.Context, scope MemberToolScope, inputID, key string) (domain.TaskWorkInput, error) {
	if !memberToolKeyValid(key) {
		return domain.TaskWorkInput{}, ErrInvalid
	}
	return s.handleWorkInput(ctx, scope, workInputAckOperation, inputID, "", key)
}

// Only fresh submissions reach this fence, after the live Attempt check. The
// immutable handling facts remain valid for this revision. An actor-bound
// return with its published notice disposes of the supplement, not the Mandate.
func requireHandledWorkConstraintsInTx(ctx context.Context, tx *sql.Tx, input SubmitTaskInput) error {
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_work_inputs i
	 WHERE i.group_id=? AND i.task_id=? AND i.task_revision=?
	 AND NOT EXISTS(SELECT 1 FROM task_work_input_consumptions c WHERE c.input_id=i.id)
	 AND NOT EXISTS(SELECT 1 FROM task_work_input_returns h JOIN message_log l ON l.message_id=h.notice_message_id
	 WHERE h.input_id=i.id AND h.member_id=i.worker_member_id AND (l.group_id,l.topic_id)=(i.group_id,i.topic_id)))`,
		input.GroupID, input.TaskID, input.ExpectedRevision).Scan(&pending)
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("%w：仍有尚未确认处理的后续输入", ErrConflict)
	}
	return nil
}
