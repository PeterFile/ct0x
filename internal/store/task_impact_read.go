package store

import (
	"context"
	"database/sql"
	"time"

	"agentworkspace/internal/domain"
)

// ListTaskImpacts is a trusted local-user projection. In particular F can
// consume Source + Decision on work_input_returned events; no other event is
// a pending decision. C can correlate evidence IDs with persisted work views.
func (s *Store) ListTaskImpacts(ctx context.Context, groupID, taskID string, options ListOptions) (Page[domain.TaskImpact], error) {
	const action = "读取任务约束影响失败"
	var zero Page[domain.TaskImpact]
	limit, err := listLimit(options)
	if err != nil {
		return zero, taskError(action, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return zero, taskError(action, err)
	}
	defer tx.Rollback()
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE group_id=? AND id=?`, groupID, taskID).Scan(&found); err != nil {
		return zero, taskError(action, err)
	}
	installed, err := taskImpactSchemaInTx(ctx, tx)
	if err != nil {
		return zero, taskError(action, err)
	}
	page := Page[domain.TaskImpact]{Items: []domain.TaskImpact{}}
	if installed {
		args := []any{sql.Named("group", groupID), sql.Named("task", taskID), sql.Named("limit", limit+1)}
		order := " ORDER BY created_at DESC,id DESC LIMIT :limit"
		if c := options.Before; c != nil {
			order = ` AND (created_at,id)<(:before,:id)` + order
			args = append(args, sql.Named("before", c.CreatedAt.UTC().Format(timestampLayout)), sql.Named("id", c.ID))
		}
		// Seek in both scoped histories before merging; a root with many
		// dependents still exposes only one bounded page.
		query := `WITH incoming AS (SELECT * FROM task_constraint_impacts WHERE group_id=:group AND task_id=:task` + order + `),
		 outgoing AS (SELECT * FROM task_constraint_impacts WHERE group_id=:group AND source_task_id=:task AND task_id<>source_task_id` + order + `),
		 page AS (SELECT * FROM incoming UNION ALL SELECT * FROM outgoing ORDER BY created_at DESC,id DESC LIMIT :limit) ` + taskImpactSelect
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return zero, taskError(action, err)
		}
		defer rows.Close()
		for rows.Next() {
			x, err := scanTaskImpact(rows)
			if err != nil {
				return zero, taskError(action, err)
			}
			page.Items = append(page.Items, x)
		}
		if err := rows.Err(); err != nil {
			return zero, taskError(action, err)
		}
		if len(page.Items) > limit {
			page.Items = page.Items[:limit]
			last := page.Items[limit-1]
			page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
		}
	}
	if err := tx.Commit(); err != nil {
		return zero, taskError(action, err)
	}
	return page, nil
}

const taskImpactSelect = `SELECT x.id,x.kind,x.task_id,x.task_revision,t.status,x.created_at,x.ref_id,
 i.group_id,i.topic_id,i.id,f.reply_message_id,i.source_id,f.trigger_id,i.task_id,i.task_revision,
 i.assignment_id,i.mandate_id,r.authority_generation,i.worker_member_id,
 coalesce(c.turn_id,''),coalesce(c.attempt_id,''),c.consumed_at,
 m.acceptance,i.before_revision,i.before_constraint,i.after_revision,i.after_constraint,coalesce(x.notice_message_id,''),
 coalesce(w.delivery_id,''),coalesce(w.message_id,''),coalesce(w.member_id,''),coalesce(d.state,''),wt.accepted_at,
 coalesce(h.member_id,''),coalesce(h.turn_id,''),coalesce(h.attempt_id,''),coalesce(h.reason,''),coalesce(h.question,''),
 coalesce(h.notice_message_id,''),coalesce(h.disposition,''),coalesce(h.policy_revision,0),coalesce(h.policy_source_id,''),
 coalesce(h.input_id IS NOT NULL AND t.status='open' AND t.revision=i.task_revision
  AND (tc.topic_id,tc.current_mandate_id,tc.authority_generation)=(i.topic_id,i.mandate_id,r.authority_generation)
  AND (t.assignee_id IS NULL OR t.assignee_id=i.worker_member_id) AND worker.status='active'
  AND cb.revision=r.binding_revision AND dp.revision=r.policy_revision
  AND coalesce(cp.revision,0)=h.policy_revision,0),
 coalesce(pin.dependency_id,''),coalesce(pin.submission_id,''),coalesce(pin.artifact_id,''),coalesce(pin.version_id,''),
 coalesce(pin.ready_when,''),coalesce(pin.dependency_revision,0),coalesce(pr.id,''),coalesce(pr.outcome,''),coalesce(x.affected_submission_id,''),
 coalesce(sub.id,''),coalesce(sub.task_id,''),coalesce(sub.task_revision,0),
 coalesce(ct.state,''),ct.cancel_requested_at
 FROM page x
 JOIN tasks t ON (t.id,t.group_id)=(x.task_id,x.group_id)
 JOIN task_work_inputs i ON (i.id,i.group_id)=(x.input_id,x.group_id)
 JOIN chat_followup_routes f ON (f.source_id,f.group_id,f.topic_id)=(i.source_id,i.group_id,i.topic_id)
 JOIN coordination_assignments a ON (a.id,a.task_id,a.group_id,a.worker_member_id)=(i.assignment_id,i.task_id,i.group_id,i.worker_member_id)
 JOIN coordination_dispatch_requests r ON (r.id,r.group_id,r.task_id,r.task_revision,r.topic_id,r.mandate_id)=
 (a.request_id,i.group_id,i.task_id,i.task_revision,i.topic_id,i.mandate_id)
 JOIN mandates m ON (m.id,m.group_id,m.task_id,m.topic_id)=(i.mandate_id,i.group_id,i.task_id,i.topic_id)
 LEFT JOIN task_work_input_consumptions c ON c.input_id=i.id
 LEFT JOIN task_work_input_returns h ON h.input_id=i.id
 LEFT JOIN task_coordination tc ON (tc.task_id,tc.group_id)=(i.task_id,i.group_id)
 LEFT JOIN members worker ON (worker.id,worker.group_id)=(i.worker_member_id,i.group_id)
 LEFT JOIN group_coordinator_bindings cb ON cb.group_id=i.group_id
 LEFT JOIN group_dispatch_policies dp ON dp.group_id=i.group_id
 LEFT JOIN chat_policies cp ON (cp.group_id,cp.topic_id)=(i.group_id,i.topic_id)
 LEFT JOIN task_work_deliveries w ON (w.source_kind,w.source_id,w.group_id,w.topic_id,w.member_id)=
 ('followup',i.id,i.group_id,i.topic_id,i.worker_member_id) AND x.kind IN ('constraint_confirmed','work_input_returned')
 LEFT JOIN deliveries d ON (d.id,d.message_id,d.group_id,d.topic_id,d.recipient_id)=(w.delivery_id,w.message_id,w.group_id,w.topic_id,w.member_id)
 LEFT JOIN turns wt ON (wt.id,wt.delivery_id,wt.group_id,wt.member_id)=(d.turn_id,d.id,d.group_id,d.recipient_id) AND d.state='accepted'
 LEFT JOIN task_artifact_dependencies pin ON (pin.group_id,pin.task_id,pin.dependency_id)=(x.group_id,x.task_id,x.ref_id)
 AND x.kind='stale_pinned_dependency'
 LEFT JOIN task_review_decisions pr ON (pr.id,pr.group_id,pr.task_id,pr.task_revision,pr.submission_id)=
 (x.pin_review_id,pin.group_id,pin.dependency_id,pin.dependency_revision,pin.submission_id)
 LEFT JOIN task_submissions sub ON (sub.id,sub.group_id)=(x.ref_id,x.group_id) AND x.kind='stale_submission'
 LEFT JOIN turns ct ON (ct.id,ct.group_id)=(x.ref_id,x.group_id) AND x.kind='cancel_requested'
 ORDER BY x.created_at DESC,x.id DESC`

func scanTaskImpact(row scanner) (x domain.TaskImpact, err error) {
	var plan domain.TaskImpactPlan
	var ack domain.TaskWorkInputConsumption
	var wake domain.TaskImpactDelivery
	var choice domain.TaskImpactDecision
	var pin domain.TaskImpactPin
	var sub domain.TaskImpactSubmission
	var cancel domain.TaskImpactCancellation
	var consumed, accepted, requested sql.NullString
	var stamp string
	src := &x.Source
	err = row.Scan(&x.ID, &x.Kind, &x.TaskID, &x.TaskRevision, &x.CurrentTaskStatus, &stamp, &x.EvidenceID,
		&src.GroupID, &src.TopicID, &src.InputID, &src.MessageID, &src.SourceID, &src.TriggerID, &src.TaskID, &src.TaskRevision,
		&src.AssignmentID, &src.MandateID, &src.AuthorityGeneration, &src.OwnerMemberID,
		&ack.TurnID, &ack.AttemptID, &consumed,
		&plan.Acceptance, &plan.BeforeConstraintRevision, &plan.BeforeConstraint, &plan.AfterConstraintRevision, &plan.AfterConstraint, &plan.NoticeMessageID,
		&wake.DeliveryID, &wake.MessageID, &wake.MemberID, &wake.State, &accepted,
		&choice.MemberID, &choice.TurnID, &choice.AttemptID, &choice.Reason, &choice.Question,
		&choice.NoticeMessageID, &choice.Disposition, &choice.PolicyRevision, &choice.PolicySourceID, &choice.ScopeCurrent,
		&pin.DependencyID, &pin.SubmissionID, &pin.ArtifactID, &pin.VersionID, &pin.ReadyWhen, &pin.DependencyRevision,
		&pin.ReviewID, &pin.ReviewOutcome, &pin.AffectedSubmissionID, &sub.SubmissionID, &sub.TaskID, &sub.TaskRevision,
		&cancel.State, &requested)
	if err != nil {
		return
	}
	if x.CreatedAt, err = time.Parse(timestampLayout, stamp); err != nil {
		return
	}
	if consumed.Valid {
		if ack.ConsumedAt, err = time.Parse(timestampLayout, consumed.String); err != nil {
			return
		}
		x.Acknowledgement = &ack
	}
	if wake.DeliveryID != "" {
		if accepted.Valid {
			at, err := time.Parse(timestampLayout, accepted.String)
			if err != nil {
				return x, err
			}
			wake.AcceptedAt = &at
		}
		x.Delivery = &wake
	}
	switch x.Kind {
	case domain.TaskImpactConstraintConfirmed:
		x.Plan, x.EvidenceID = &plan, ""
		x.Reason = "负责人已确认新约束；原计划须按原授权和验收要求重新核对。"
	case domain.TaskImpactWorkInputReturned:
		choice.ReturnedAt = x.CreatedAt
		x.Decision, x.EvidenceID = &choice, ""
		x.Reason = "负责人已交回后续要求；未确认该约束或扩大授权。"
	case domain.TaskImpactStaleDelegation:
		x.Reason = "委派问题早于本次约束确认，不能再启动或作为新报告的依据。"
	case domain.TaskImpactStaleWorkReport:
		x.Reason = "该报告基于确认前的委派；保留原报告，但不能作为当前约束下的验证。"
	case domain.TaskImpactStaleSubmission:
		if sub.SubmissionID == "" {
			return x, ErrConflict
		}
		x.Submission = &sub
		x.Reason = "该具体提交属于本次调整前的任务版本或原修复来源，不是新约束下的交付。"
	case domain.TaskImpactStalePin:
		if pin.SubmissionID == "" {
			return x, ErrConflict
		}
		x.Pin = &pin
		x.Reason = "固定的提交与成果版本已不适用；保留原版本和评审，等待重新规划。"
	case domain.TaskImpactCancelRequested:
		cancel.TurnID = x.EvidenceID
		if cancel.RequestedAt, err = time.Parse(timestampLayout, requested.String); err != nil {
			return
		}
		x.Cancellation = &cancel
		x.Reason = "已向受影响的执行轮次请求取消；不代表执行停止或资源已释放。"
	}
	return
}
