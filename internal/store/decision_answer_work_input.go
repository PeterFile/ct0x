package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

func decisionAnswerWakeSourceInTx(ctx context.Context, tx *sql.Tx, d domain.TaskWorkDelivery) (id string, err error) {
	if err := requireDecisionAnswersInTx(ctx, tx); err != nil {
		return "", err
	}
	query := `SELECT ans.id` + decisionAnswerEvidence
	identity := `(work.source_id,work.delivery_id,work.message_id,work.sender_member_id)`
	if strings.HasPrefix(d.SourceID, decisionAnswerReplacementPrefix) {
		query += decisionAnswerReplacementJoin
		identity = `(replacement.source_id,replacement.delivery_id,replacement.message_id,replacement.sender_member_id)`
	}
	err = tx.QueryRowContext(ctx, query+` WHERE `+identity+`=(?,?,?,?) AND `+decisionAnswerProof,
		d.SourceID, d.DeliveryID, d.MessageID, d.SenderMemberID).Scan(&id)
	return id, coordinationRoleRelationError(err)
}

// Readiness and ACK both recheck the current version rather than treating a
// saved human statement or physically queued delivery as authority to start.
const decisionAnswerCurrentVersion = `q.withdrawn_at IS NULL
 AND v.version=(SELECT max(version) FROM decision_request_versions WHERE request_id=q.id) AND (`

const decisionAnswerCurrentScope = decisionAnswerCurrentVersion + decisionRequestCurrentTask + `)`

func decisionAnswerCurrentInTx(ctx context.Context, tx *sql.Tx) (string, error) {
	scope, _, err := decisionTaskCurrentScopeInTx(ctx, tx)
	return decisionAnswerCurrentVersion + scope + `) AND (v.expires_at IS NULL OR v.expires_at>?)`, err
}

func decisionAnswerLiveInTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	return decisionAnswerLiveAtInTx(ctx, tx, id, time.Now().UTC())
}

func decisionAnswerLiveAtInTx(ctx context.Context, tx *sql.Tx, id string, at time.Time) (bool, error) {
	current, err := decisionAnswerCurrentInTx(ctx, tx)
	if err != nil {
		return false, err
	}
	var live bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1`+decisionAnswerEvidence+`
	 JOIN tasks t ON (t.id,t.group_id)=(q.task_id,q.group_id)
	 WHERE ans.id=? AND `+current+`)`, id, at.UTC().Format(timestampLayout)).Scan(&live)
	return live, err
}

// An ACK is historical evidence only for the exact typed delivery and its
// bound owner Attempt. This check does not depend on current task/turn state.
const decisionAnswerHandlingMatch = `EXISTS(SELECT 1` + taskWorkContinuationRelations + `
 JOIN task_work_continuations b ON (b.delivery_id,b.turn_id,b.attempt_id)=(w.delivery_id,t.id,a.id)
 WHERE b.delivery_id IN (work.delivery_id,replacement.delivery_id) AND (b.turn_id,b.attempt_id)=(c.turn_id,c.attempt_id))`

// The common fresh Submission guard has already checked the Task's active
// Attempt. The current scope proves that very Attempt is bound to the
// answer's original Assignment, Mandate and authority generation.
func requireHandledDecisionAnswersInTx(ctx context.Context, tx *sql.Tx, input SubmitTaskInput) error {
	installed, err := decisionAnswerInstalledInTx(ctx, tx)
	if err != nil || !installed {
		return err
	}
	current, err := decisionAnswerCurrentInTx(ctx, tx)
	if err != nil {
		return err
	}
	var pending bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1`+decisionAnswerEvidence+decisionAnswerReplacementJoin+`
 JOIN tasks t ON (t.id,t.group_id)=(q.task_id,q.group_id)
 WHERE (q.group_id,q.task_id,q.task_revision,t.active_attempt_id)=(?,?,?,?) AND `+current+` AND `+decisionAnswerProof+`
 AND NOT EXISTS(SELECT 1 FROM decision_answer_consumptions c WHERE c.answer_id=ans.id AND `+decisionAnswerHandlingMatch+`))`,
		input.GroupID, input.TaskID, input.ExpectedRevision, input.AttemptID, time.Now().UTC().Format(timestampLayout)).Scan(&pending)
	if err == nil && pending {
		err = fmt.Errorf("%w：仍有尚未确认处理的用户决定", ErrConflict)
	}
	return err
}

// Same scan shape as a WorkInput, with genuine source IDs and no chat candidate,
// author Turn, constraint delta or claim fabricated for the user's statement.
// Native input tools receive bounded, explicitly attributed question + reply.
const decisionAnswerWorkInputSelect = `SELECT ans.id,q.group_id,q.topic_id,q.task_id,q.task_revision,q.mandate_id,q.assignment_id,q.owner_member_id,
 '',ans.id,l.sequence,'',reply.id,work.message_id,
 '本机用户决定（不改变原授权）' || char(10) || '问题：' || json_extract(v.question_json,'$.question') ||
 coalesce(char(10) || '拟议范围：' || json_extract(v.question_json,'$.authorization.scope') ||
          char(10) || '影响：' || json_extract(v.question_json,'$.authorization.impact'),'') || char(10) || '答复原文：' || char(10) || reply.content,
 0,'',0,'',ans.created_at,c.turn_id,c.attempt_id,c.consumed_at,
 NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL` + decisionAnswerEvidence + decisionAnswerReplacementJoin + `
 JOIN tasks t ON (t.id,t.group_id)=(q.task_id,q.group_id)
 LEFT JOIN decision_answer_consumptions c ON c.answer_id=ans.id AND ` + decisionAnswerHandlingMatch

// A pending answer is visible to its exact physical wake only; later turns of
// the same work may see it as history only after explicit handling.
const decisionAnswerWorkerWhere = ` WHERE (q.assignment_id,q.task_revision,q.group_id,q.topic_id,q.task_id,q.owner_member_id,q.mandate_id)=
 (?,?,?,?,?,?,?) AND (? IN (work.delivery_id,replacement.delivery_id) OR c.answer_id IS NOT NULL) AND ` + decisionAnswerProof + ` AND `

func decisionAnswerWorkInputArgs(p CoordinationRoleProvenance, mandateID string) []any {
	return append(workerWorkInputArgs(p), mandateID, p.DeliveryID, time.Now().UTC().Format(timestampLayout))
}

func decisionAnswerWorkInputInTx(ctx context.Context, tx *sql.Tx, p CoordinationRoleProvenance, id, mandateID string) (domain.TaskWorkInput, error) {
	if err := requireDecisionAnswersInTx(ctx, tx); err != nil {
		return domain.TaskWorkInput{}, err
	}
	current, err := decisionAnswerCurrentInTx(ctx, tx)
	if err != nil {
		return domain.TaskWorkInput{}, err
	}
	args := append(decisionAnswerWorkInputArgs(p, mandateID), id)
	i, err := scanWorkInput(tx.QueryRowContext(ctx, decisionAnswerWorkInputSelect+decisionAnswerWorkerWhere+current+` AND ans.id=?`, args...))
	return i, coordinationRoleRelationError(err)
}

func decisionAnswerWorkInputHistoryInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, opts ListOptions, limit int, page Page[domain.TaskWorkInput]) (Page[domain.TaskWorkInput], error) {
	installed, err := decisionAnswerInstalledInTx(ctx, tx)
	if err != nil || !installed {
		return page, err
	}
	p := role.Provenance
	r, err := coordinationDispatchRequestInTx(ctx, tx, p.GroupID, p.RequestID)
	if err != nil {
		return page, err
	}
	current, err := decisionAnswerCurrentInTx(ctx, tx)
	if err != nil {
		return page, err
	}
	return mergeDecisionAnswerWorkInputHistoryInTx(ctx, tx, decisionAnswerWorkerWhere+current, decisionAnswerWorkInputArgs(p, r.MandateID), opts.Before, limit, page)
}

// Read at most limit+1 answers with the same exclusive cursor and sort order
// as legacy inputs, then merge the two bounded streams for a single page.
func mergeDecisionAnswerWorkInputHistoryInTx(ctx context.Context, tx *sql.Tx, where string, args []any, before *Cursor, limit int, page Page[domain.TaskWorkInput]) (Page[domain.TaskWorkInput], error) {
	if c := before; c != nil {
		where += ` AND (ans.created_at,ans.id)<(?,?)`
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	rows, err := tx.QueryContext(ctx, decisionAnswerWorkInputSelect+where+` ORDER BY ans.created_at DESC,ans.id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	var items []domain.TaskWorkInput
	for rows.Next() {
		i, err := scanWorkInput(rows)
		if err != nil {
			return page, err
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil || len(items) == 0 {
		return page, err
	}
	items = append(items, page.Items...)
	slices.SortFunc(items, func(a, b domain.TaskWorkInput) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(b.ID, a.ID)
	})
	page.Items = items[:min(len(items), limit)]
	if len(items) > limit || page.NextCursor != nil {
		last := page.Items[len(page.Items)-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

// The legacy tool wrapper and idempotency namespace are shared. Only the
// exact wake's live canonical Attempt (not the original owner Turn, another
// wake, a child or a same-name Task) can consume this answer.
func (s *Store) consumeDecisionAnswerInTx(ctx context.Context, tx *sql.Tx, role CoordinationToolScope, original assignmentSnapshot, id, key string) (domain.TaskWorkInput, error) {
	var zero domain.TaskWorkInput
	p := role.Provenance
	if p.SourceKind != domain.TaskWorkSourceFollowup || p.SourceID != id {
		return zero, ErrMemberToolDenied
	}
	r := original.r
	i, err := decisionAnswerWorkInputInTx(ctx, tx, p, id, r.MandateID)
	if err != nil {
		return zero, err
	}
	m := mutationScope{operation: workInputAckOperation, actorID: p.MemberID, groupID: p.GroupID, key: key}
	payload := mutationPayload(id, p.AssignmentID)
	record, replay, err := lookupMutation(ctx, tx, m, payload)
	if err != nil {
		return zero, err
	}
	if replay && (record != id || i.Consumption == nil) {
		return zero, ErrConflict
	}
	c := i.Consumption
	if c == nil {
		c = &domain.TaskWorkInputConsumption{TurnID: role.Member.Claim.TurnID, AttemptID: role.AttemptID, ConsumedAt: time.Now().UTC()}
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO decision_answer_consumptions(answer_id,turn_id,attempt_id,consumed_at) VALUES(?,?,?,?)`,
			id, c.TurnID, c.AttemptID, c.ConsumedAt.Format(timestampLayout)); err != nil {
			return zero, err
		}
	}
	if c.TurnID != role.Member.Claim.TurnID || c.AttemptID != role.AttemptID {
		return zero, ErrMemberToolDenied
	}
	if key != "" && !replay {
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id)
		 VALUES(?,?,?,?,?,?)`, m.operation, m.actorID, m.groupID, m.key, payload, id); err != nil {
			return zero, err
		}
	}
	current, owner, err := s.workInputHandlingOwnerInTx(ctx, tx, role.Member, false)
	if err != nil {
		return zero, err
	}
	if current != role || owner.r != r {
		return zero, ErrMemberToolDenied
	}
	saved, err := decisionAnswerWorkInputInTx(ctx, tx, p, id, r.MandateID)
	if err != nil {
		return zero, err
	}
	if saved.Consumption == nil || *saved.Consumption != *c {
		return zero, ErrConflict
	}
	if key != "" && !replay {
		savedID, present, err := lookupMutation(ctx, tx, m, payload)
		if err != nil {
			return zero, err
		}
		if !present || savedID != id {
			return zero, ErrConflict
		}
	}
	return saved, nil
}

func decisionAnswerHandledInTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var handled bool
	if err := requireDecisionAnswersInTx(ctx, tx); err != nil {
		return false, err
	}
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM decision_answer_consumptions WHERE answer_id=ans.id)
	 FROM decision_answers ans WHERE ans.id=?`, id).Scan(&handled)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrMemberToolDenied
	}
	return handled, err
}
