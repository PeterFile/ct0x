package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agentworkspace/internal/domain"
)

var ErrDecisionRequestsUnavailable = errors.New("当前数据库未安装待决事项记录")

func requireDecisionRequestsInTx(ctx context.Context, tx *sql.Tx) error {
	// B assigns the migration slot, so do not probe for a numerical version.
	var installed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='decision_requests')`).Scan(&installed); err != nil {
		return err
	}
	if !installed {
		return ErrDecisionRequestsUnavailable
	}
	return nil
}

func decisionRequestError(action string, err error) error {
	for _, known := range []error{ErrDecisionRequestsUnavailable, ErrDecisionAnswersUnavailable, ErrDecisionRelayUnsupported} {
		if errors.Is(err, known) {
			return known
		}
	}
	return queueError(action, err)
}

// On v30 the trusted local-user group's visibility predicate is group existence
// (the same scope as ListGroups), not membership of a model or a synthetic Project.
// Keep it in this transaction and replace it with D's predicate when available.
const decisionRequestColumns = `SELECT q.id,v.version,q.kind,q.group_id,q.topic_id,g.name,q.recipient_id,q.relay,
 coalesce(q.task_id,''),coalesce(t.title,''),coalesce(q.task_revision,0),coalesce(q.assignment_id,''),coalesce(q.mandate_id,''),
 coalesce(q.authority_generation,0),coalesce(q.owner_member_id,''),coalesce(q.origin_trigger_id,''),
 coalesce(q.chat_source_id,''),coalesce(a.candidate_id,''),coalesce(a.id,''),coalesce(a.notice_message_id,''),
 q.source_message_id,q.source_sequence,v.author_member_id,v.author_turn_id,coalesce(v.author_attempt_id,''),
 v.question_json,q.created_at,v.created_at,v.expires_at, `

const decisionRequestRelations = ` FROM decision_requests q JOIN groups g ON g.id=q.group_id
 JOIN decision_request_versions v ON v.request_id=q.id AND v.version=(SELECT max(version) FROM decision_request_versions WHERE request_id=q.id)
 LEFT JOIN tasks t ON (t.id,t.group_id)=(q.task_id,q.group_id)
 LEFT JOIN chat_assessments a ON a.id=q.chat_assessment_id`

// Settlement of the asking Turn is not an answer. The Task still needs the
// exact open work generation and its canonical owner, not the original claim.
const decisionRequestTaskScope = `q.task_id IS NOT NULL AND t.status='open' AND t.revision=q.task_revision
 AND t.current_submission_id IS NULL AND coalesce(t.assignee_id,'') IN ('',q.owner_member_id)
 AND EXISTS(SELECT 1 FROM task_coordination tc
 JOIN coordination_assignments ca ON (ca.id,ca.task_id,ca.group_id,ca.worker_member_id)=(q.assignment_id,q.task_id,q.group_id,q.owner_member_id)
 JOIN coordination_dispatch_requests r ON (r.id,r.task_id,r.group_id,r.topic_id,r.task_revision,r.mandate_id,r.authority_generation)=
    (ca.request_id,q.task_id,q.group_id,q.topic_id,q.task_revision,q.mandate_id,q.authority_generation)
 JOIN coordination_assignment_dispatch d ON d.assignment_id=ca.id AND d.status='enqueued'
 JOIN coordination_assignment_attempts original ON original.assignment_id=ca.id`

const decisionRequestTaskCurrentRelations = `
 JOIN coordination_triggers o ON (o.id,o.group_id,o.topic_id,o.message_id,o.message_sequence)=
    (q.origin_trigger_id,q.group_id,q.topic_id,q.source_message_id,q.source_sequence)
 JOIN group_coordinator_bindings b ON (b.group_id,b.revision)=(q.group_id,r.binding_revision)
 JOIN group_dispatch_policies dp ON (dp.group_id,dp.revision)=(q.group_id,r.policy_revision)
 JOIN members w ON (w.id,w.group_id,w.status)=(q.owner_member_id,q.group_id,'active')
 JOIN members co ON (co.id,co.group_id,co.status)=(r.coordinator_member_id,q.group_id,'active')
 JOIN chat_assessments admission ON (admission.decision_id,admission.dispatch_request_id)=(m.decision_id,r.id)
 JOIN chat_candidates c ON c.id=admission.candidate_id
 JOIN chat_policies p ON (p.group_id,p.topic_id,p.revision)=(q.group_id,q.topic_id,c.policy_revision)
 WHERE (tc.task_id,tc.group_id,tc.topic_id,tc.current_mandate_id,tc.authority_generation)=
    (q.task_id,q.group_id,q.topic_id,q.mandate_id,q.authority_generation)
 AND (t.active_attempt_id=original.attempt_id OR EXISTS(SELECT 1 FROM task_work_continuations k
    JOIN task_work_deliveries work ON work.delivery_id=k.delivery_id
    WHERE k.attempt_id=t.active_attempt_id AND work.source_kind IN ('followup','report')
    AND (work.assignment_id,work.task_id,work.group_id,work.topic_id,work.task_revision,work.mandate_id,
         work.authority_generation,work.owner_member_id,work.member_id)=
        (q.assignment_id,q.task_id,q.group_id,q.topic_id,q.task_revision,q.mandate_id,
         q.authority_generation,q.owner_member_id,q.owner_member_id)))`

const decisionRequestCurrentTask = decisionRequestTaskScope + `
 JOIN mandates m ON (m.id,m.origin_trigger_id,m.authorized_by)=(q.mandate_id,q.origin_trigger_id,q.recipient_id)` + decisionRequestTaskCurrentRelations + `)`

const decisionRequestCurrentTaskWithReturnStart = decisionRequestTaskScope + `
 JOIN mandates m ON (m.id,m.authorized_by)=(q.mandate_id,q.recipient_id)` + decisionRequestTaskCurrentRelations + `
 AND CASE WHEN m.origin_trigger_id=q.origin_trigger_id THEN 1 WHEN ` + workInputDecisionIDMatch + `
     THEN EXISTS(SELECT 1 FROM `
const decisionRequestCurrentTaskWithReturnEnd = ` b WHERE ` + workInputDecisionReadMatch + ` AND b.policy_revision=p.revision)
     ELSE 0 END)`
const decisionRequestCurrentTaskWithReturn = decisionRequestCurrentTaskWithReturnStart + workInputDecisionSources + decisionRequestCurrentTaskWithReturnEnd

const decisionRequestChatJoins = ` JOIN chat_intake_executions i ON (i.candidate_id,i.turn_id,i.purpose,i.source_kind,i.source_id)=
    (c.id,c.turn_id,'chat_intake/v1','local_user_chat',q.chat_source_id)
 JOIN turns rt ON rt.id=c.turn_id JOIN deliveries rd ON rd.id=rt.delivery_id
 JOIN message_command_sources s ON (s.id,s.message_id,s.kind)=(q.chat_source_id,q.source_message_id,'local_user_chat')
 JOIN messages source ON (source.id,source.sender_id)=(s.message_id,q.recipient_id)
 JOIN messages n ON (n.id,n.group_id,n.topic_id,n.sender_id)=(a.notice_message_id,q.group_id,q.topic_id,c.member_id)`

const decisionRequestChatProof = `(c.group_id,c.topic_id,c.message_id,c.generation,c.turn_id,c.member_id)=
    (q.group_id,q.topic_id,q.source_message_id,q.source_sequence,v.author_turn_id,v.author_member_id)
 AND ` + chatDiscussionReceiveMatches + `
 AND a.decision_id IS NULL AND a.dispatch_request_id IS NULL AND json_extract(a.receipt_json,'$.outcome')='clarify'
 AND json_extract(a.receipt_json,'$.question')=json_extract(v.question_json,'$.question')
 AND NOT EXISTS(SELECT 1 FROM deliveries wake WHERE wake.message_id=n.id)`

const decisionRequestChatUnadvanced = `NOT EXISTS(SELECT 1 FROM message_log l JOIN message_command_sources s ON s.message_id=l.message_id AND s.kind='local_user_chat'
    WHERE (l.group_id,l.topic_id)=(q.group_id,q.topic_id) AND l.sequence>q.source_sequence)
 AND NOT EXISTS(SELECT 1 FROM coordination_triggers o JOIN routing_decisions r ON r.trigger_id=o.id
    WHERE (o.source_kind,o.message_id,o.group_id,o.topic_id)=('local_im',q.source_message_id,q.group_id,q.topic_id))
 AND NOT EXISTS(SELECT 1 FROM chat_followup_routes f WHERE f.source_id=q.chat_source_id)`

const decisionRequestCurrentChat = `q.task_id IS NULL AND EXISTS(SELECT 1 FROM chat_candidates c` + decisionRequestChatJoins + `
 WHERE c.id=a.candidate_id AND ` + decisionRequestChatProof + `) AND ` + decisionRequestChatUnadvanced

const decisionRequestCurrent = ` WHERE q.recipient_id='user' AND q.withdrawn_at IS NULL
 AND (v.expires_at IS NULL OR v.expires_at>?) AND ((` + decisionRequestCurrentTask + `) OR (` + decisionRequestCurrentChat + `))`

const decisionRequestUnanswered = ` AND NOT EXISTS(SELECT 1 FROM decision_answers ans WHERE ans.request_id=q.id)`

func currentDecisionSQLInTx(ctx context.Context, tx *sql.Tx) (columns, where string, bridge bool, err error) {
	bridge, err = workInputDecisionInstalledInTx(ctx, tx)
	if err != nil {
		return "", "", false, err
	}
	inputID, taskScope := `''`, decisionRequestCurrentTask
	if bridge {
		inputID = `CASE WHEN ` + workInputDecisionIDMatch + ` THEN coalesce((SELECT b.input_id FROM ` + workInputDecisionSources + ` b
		 WHERE ` + workInputDecisionReadMatch + `),'') ELSE '' END`
		taskScope = decisionRequestCurrentTaskWithReturn
	}
	return decisionRequestColumns + inputID + decisionRequestRelations, ` WHERE q.recipient_id='user' AND q.withdrawn_at IS NULL
	 AND (v.expires_at IS NULL OR v.expires_at>?) AND ((` + taskScope + `) OR (` + decisionRequestCurrentChat + `))`, bridge, nil
}

func decisionRequestPendingSQL(ctx context.Context, tx *sql.Tx, where string) (string, error) {
	installed, err := decisionAnswerInstalledInTx(ctx, tx)
	if err != nil {
		return "", err
	}
	if installed {
		where += decisionRequestUnanswered
	}
	threadInstalled, err := decisionThreadAnswerInstalled(ctx, tx)
	if threadInstalled {
		where += decisionThreadUnanswered
	}
	return where, err
}

func scanDecisionRequest(row scanner) (out domain.DecisionRequest, err error) {
	var task domain.DecisionTaskRef
	var chat domain.DecisionChatProof
	var question, created, updated string
	var expires sql.NullString
	err = row.Scan(&out.ID, &out.Version, &out.Kind, &out.GroupID, &out.TopicID, &out.GroupName, &out.RecipientID, &out.Relay,
		&task.TaskID, &task.TaskTitle, &task.TaskRevision, &task.AssignmentID, &task.MandateID, &task.AuthorityGeneration, &task.OwnerMemberID, &task.OriginTriggerID,
		&chat.SourceID, &chat.CandidateID, &chat.AssessmentID, &chat.NoticeMessageID,
		&out.SourceMessageID, &out.SourceSequence, &out.AuthorMemberID, &out.AuthorTurnID, &out.AuthorAttemptID,
		&question, &created, &updated, &expires, &task.WorkInputID)
	if err != nil {
		return out, err
	}
	if task.TaskID != "" {
		out.Task = &task
	} else {
		out.Chat = &chat
	}
	if err := json.Unmarshal([]byte(question), &out.DecisionQuestion); err != nil {
		return out, err
	}
	if out.CreatedAt, err = time.Parse(timestampLayout, created); err != nil {
		return out, err
	}
	if out.UpdatedAt, err = time.Parse(timestampLayout, updated); err != nil {
		return out, err
	}
	if expires.Valid {
		at, err := time.Parse(timestampLayout, expires.String)
		if err != nil {
			return out, err
		}
		out.ExpiresAt = &at
	}
	return out, nil
}

func pendingDecisionRequestInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, id string, version int64, now time.Time) (domain.DecisionRequest, error) {
	columns, where, _, err := currentDecisionSQLInTx(ctx, tx)
	if err != nil {
		return domain.DecisionRequest{}, err
	}
	where, err = decisionRequestPendingSQL(ctx, tx, where)
	if err != nil {
		return domain.DecisionRequest{}, err
	}
	return decisionRequestVersionInTx(ctx, tx, columns+where, thread, id, version, now)
}

func decisionRequestVersionInTx(ctx context.Context, tx *sql.Tx, query string, thread domain.ThreadRef, id string, version int64, now time.Time) (domain.DecisionRequest, error) {
	return scanDecisionRequest(tx.QueryRowContext(ctx, query+`
 AND (q.group_id,q.topic_id,q.id,v.version)=(?,?,?,?)`, now.UTC().Format(timestampLayout), thread.GroupID, thread.TopicID, id, version))
}

type decisionList uint8

const (
	decisionListRequests decisionList = iota
	decisionListTasks
	decisionListAnswerable
)

// ListPendingDecisionRequests is for trusted local-user composition only. The
// cursor refers to original capture order; amendments do not reorder a page.
func (s *Store) ListPendingDecisionRequests(ctx context.Context, options ListOptions) (Page[domain.DecisionRequest], error) {
	return s.listPendingDecisionRequests(ctx, options, decisionListRequests)
}

func (s *Store) ListPendingTaskDecisions(ctx context.Context, options ListOptions) (Page[domain.DecisionRequest], error) {
	return s.listPendingDecisionRequests(ctx, options, decisionListTasks)
}

// ListAnswerableDecisions is the local-user inbox, not the request kernel.
func (s *Store) ListAnswerableDecisions(ctx context.Context, options ListOptions) (Page[domain.DecisionRequest], error) {
	return s.listPendingDecisionRequests(ctx, options, decisionListAnswerable)
}

func (s *Store) listPendingDecisionRequests(ctx context.Context, options ListOptions, list decisionList) (Page[domain.DecisionRequest], error) {
	const action = "读取待决事项失败"
	var page Page[domain.DecisionRequest]
	limit, err := listLimit(options)
	if err != nil {
		return page, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, decisionRequestError(action, err)
	}
	defer tx.Rollback()
	if err := requireDecisionRequestsInTx(ctx, tx); err != nil {
		return page, decisionRequestError(action, err)
	}
	columns, current, bridge, err := currentDecisionSQLInTx(ctx, tx)
	if err != nil {
		return page, decisionRequestError(action, err)
	}
	var where string
	if list == decisionListRequests {
		where, err = decisionRequestPendingSQL(ctx, tx, current)
	} else {
		if err := requireDecisionAnswersInTx(ctx, tx); err != nil {
			return page, decisionRequestError(action, err)
		}
		where = current + decisionRequestUnanswered + ` AND q.task_id IS NOT NULL`
		if list == decisionListAnswerable {
			where, err = s.answerableDecisionSQL(ctx, tx, current, where)
		}
	}
	if err != nil {
		return page, decisionRequestError(action, err)
	}
	query := columns + where
	args := []any{time.Now().UTC().Format(timestampLayout)}
	if c := options.Before; c != nil {
		query += ` AND (q.created_at,q.id)<(?,?)`
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	query += ` ORDER BY q.created_at DESC,q.id DESC`
	if !bridge {
		query += ` LIMIT ?`
		args = append(args, limit+1)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return page, decisionRequestError(action, err)
	}
	defer rows.Close()
	items := make([]domain.DecisionRequest, 0, limit+1)
	for len(items) <= limit && rows.Next() {
		item, err := scanDecisionRequest(rows)
		if err != nil {
			return page, decisionRequestError(action, err)
		}
		if item.Task != nil && item.Task.WorkInputID != "" {
			facts, err := decisionTaskFactsInTx(ctx, tx, item.Task.AssignmentID)
			if err != nil {
				return page, decisionRequestError(action, err)
			}
			if err := decisionTaskChatAuthorityInTx(ctx, tx, facts); errors.Is(err, ErrConflict) {
				continue
			} else if err != nil {
				return page, decisionRequestError(action, err)
			}
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return page, decisionRequestError(action, err)
	}
	if err := rows.Close(); err != nil {
		return page, decisionRequestError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return page, decisionRequestError(action, err)
	}
	page.Items = items
	if len(items) > limit {
		page.Items = items[:limit]
		last := items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}
