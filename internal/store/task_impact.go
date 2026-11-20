package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"agentworkspace/internal/domain"
)

func taskImpactSchemaInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var installed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=?)`, taskImpactSchemaVersion).Scan(&installed)
	return installed, err
}

// The real owner handling command calls this only on fresh handling. Each
// changed input is independent: neither proposal order nor a shared excerpt
// proves supersession. The ACK transaction fences the evidence it invalidates.
func recordTaskImpactInTx(ctx context.Context, tx *sql.Tx, i domain.TaskWorkInput, request domain.DispatchRequest) error {
	if i.Return == nil && (i.Consumption == nil || i.AfterConstraintRevision <= i.BeforeConstraintRevision) {
		return nil
	}
	installed, err := taskImpactSchemaInTx(ctx, tx)
	if err != nil || !installed {
		return err
	}
	const insertRoot = `INSERT INTO task_constraint_impacts
	 (id,input_id,group_id,source_task_id,task_id,task_revision,created_at,kind,ref_id,notice_message_id)
	 VALUES(?,?,?,?,?,?,?,?,?,nullif(?,''))`
	if i.Return != nil {
		return coordinationDispatchWrite(ctx, tx, insertRoot, i.ID, i.ID, i.GroupID, i.TaskID, i.TaskID, i.TaskRevision,
			i.Return.ReturnedAt.Format(timestampLayout), domain.TaskImpactWorkInputReturned, i.ID, "")
	}

	// Publish only in the original thread, without deliveries, source text,
	// or foreign-topic task IDs. This is not a wake, read receipt or grant.
	message, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: i.GroupID, TopicID: i.TopicID, ActorID: i.WorkerMemberID,
		RecipientIDs: []string{}, Content: fmt.Sprintf("平台记录：负责人已确认任务 %s（版本 %d）一条后续输入中的约束。请据此重新核对原计划和验收要求；历史报告、提交和评审不被改写。", i.TaskID, i.TaskRevision)}, nil)
	if err != nil {
		return err
	}
	stamp := i.Consumption.ConsumedAt.Format(timestampLayout)
	if err := coordinationDispatchWrite(ctx, tx, insertRoot, i.ID, i.ID, i.GroupID, i.TaskID, i.TaskID, i.TaskRevision, stamp,
		domain.TaskImpactConstraintConfirmed, i.ID, message.ID); err != nil {
		return err
	}
	args := []any{sql.Named("grp", i.GroupID), sql.Named("task", i.TaskID), sql.Named("rev", i.TaskRevision),
		sql.Named("assignment", i.AssignmentID), sql.Named("mandate", i.MandateID), sql.Named("topic", i.TopicID),
		sql.Named("generation", request.AuthorityGeneration), sql.Named("request", request.ID), sql.Named("input", i.ID), sql.Named("stamp", stamp)}
	var expected int64
	if err := tx.QueryRowContext(ctx, taskImpactFacts+` SELECT count(*) FROM facts`, args[:8]...).Scan(&expected); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, taskImpactFacts+`
	 INSERT INTO task_constraint_impacts
	 (id,input_id,group_id,source_task_id,task_id,task_revision,created_at,kind,ref_id,pin_review_id,affected_submission_id)
	 SELECT lower(hex(randomblob(16))),:input,:grp,:task,task_id,revision,:stamp,kind,ref_id,review_id,affected_submission FROM facts`, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != expected {
		return ErrConflict
	}
	return cancelAffectedImpactOwnersInTx(ctx, tx, i, stamp)
}

// Submissions qualify by exact Task revision or by the existing direct repair
// lineage. Newer versions of the same artifact, plain edges and topic peers
// are never used to discover dependents. No text is proof of verification.
const taskImpactFacts = `WITH old_submissions(id,task_id,revision) AS (
 SELECT id,task_id,task_revision FROM task_submissions WHERE (group_id,task_id,task_revision)=(:grp,:task,:rev)
 UNION
 SELECT parent_submission_id,parent_task_id,reviewed_task_revision FROM chat_repair_links
 WHERE (group_id,topic_id,repair_task_id,mandate_id,dispatch_request_id)=(:grp,:topic,:task,:mandate,:request)
), old_delegations(id) AS (
 SELECT id FROM task_work_delegations WHERE (group_id,task_id,task_revision,assignment_id,mandate_id,authority_generation,topic_id)=
 (:grp,:task,:rev,:assignment,:mandate,:generation,:topic)
), facts(kind,task_id,revision,ref_id,review_id,affected_submission) AS (
 SELECT 'stale_delegation',:task,:rev,id,NULL,NULL FROM old_delegations
 UNION ALL SELECT 'stale_work_report',:task,:rev,r.id,NULL,NULL FROM old_delegations d JOIN task_work_reports r ON r.delegation_id=d.id
 WHERE (r.group_id,r.task_id)=(:grp,:task)
 UNION ALL SELECT 'stale_submission',:task,:rev,id,NULL,NULL FROM old_submissions
 UNION ALL SELECT 'stale_pinned_dependency',pin.task_id,t.revision,pin.dependency_id,r.id,t.current_submission_id
 FROM old_submissions s JOIN task_artifact_dependencies pin ON (pin.group_id,pin.dependency_id,pin.dependency_revision,pin.submission_id)=(:grp,s.task_id,s.revision,s.id)
 JOIN task_submission_artifacts p ON (p.submission_id,p.artifact_id,p.version_id)=(pin.submission_id,pin.artifact_id,pin.version_id)
 JOIN tasks t ON (t.group_id,t.id)=(pin.group_id,pin.task_id)
 LEFT JOIN task_review_decisions r ON (r.submission_id,r.group_id,r.task_id,r.task_revision)=(s.id,:grp,s.task_id,s.revision) AND r.reviewed_by='user'
)`

// A Task's current owner Attempt alone does not make an ordinary conversation
// Turn exclusive to that work. Only a proved original or typed owner binding
// qualifies. Starting, unknown, already-cancelled and terminal candidates
// are intentionally excluded. Never edit resources, deliveries or receipts.
func cancelAffectedImpactOwnersInTx(ctx context.Context, tx *sql.Tx, i domain.TaskWorkInput, stamp string) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT t.id,t.revision,w.id FROM task_constraint_impacts x
	 JOIN tasks t ON (t.id,t.group_id,t.revision)=(x.task_id,x.group_id,x.task_revision)
	 JOIN task_attempts a ON (a.id,a.task_id,a.group_id,a.task_revision)=(t.active_attempt_id,t.id,t.group_id,t.revision)
	 JOIN turns w ON (w.id,w.group_id,w.member_id)=(a.turn_id,a.group_id,a.actor_id)
	 JOIN deliveries d ON (d.id,d.turn_id,d.group_id,d.conversation_id,d.recipient_id)=(w.delivery_id,w.id,w.group_id,w.conversation_id,w.member_id)
	 JOIN coordination_assignments ca ON (ca.task_id,ca.group_id,ca.worker_member_id)=(t.id,t.group_id,a.actor_id)
	 JOIN coordination_dispatch_requests r ON (r.id,r.group_id,r.task_id,r.task_revision,r.topic_id)=(ca.request_id,t.group_id,t.id,t.revision,d.topic_id)
	 JOIN task_coordination c ON (c.task_id,c.group_id,c.topic_id,c.current_mandate_id,c.authority_generation)=
	 (t.id,t.group_id,r.topic_id,r.mandate_id,r.authority_generation)
	 WHERE x.input_id=? AND x.group_id=? AND x.kind='stale_pinned_dependency' AND w.id<>?
	 AND w.state='running' AND w.accepted_at IS NOT NULL AND w.native_session_id<>'' AND w.cancel_requested_at IS NULL AND w.candidate_terminal=''
	 AND d.state='accepted' AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.delivery_id=d.id AND other.id<>w.id)
	 AND NOT EXISTS(SELECT 1 FROM task_attempts other WHERE other.turn_id=w.id AND other.group_id=w.group_id AND other.task_id<>t.id)
	 AND (EXISTS(SELECT 1 FROM coordination_assignment_attempts b JOIN coordination_assignment_dispatch s USING(assignment_id)
	 WHERE (b.assignment_id,b.attempt_id,b.turn_id,s.delivery_id,s.message_id,s.conversation_id,s.delivered_member_id)=
	 (ca.id,a.id,w.id,d.id,d.message_id,d.conversation_id,d.recipient_id) AND s.status='enqueued')
	 OR EXISTS(SELECT 1 FROM task_work_continuations b JOIN task_work_deliveries src USING(delivery_id)
	 WHERE (src.assignment_id,b.attempt_id,b.turn_id,src.delivery_id)=(ca.id,a.id,w.id,d.id)
	 AND src.source_kind IN ('report','followup') AND (src.group_id,src.topic_id,src.task_id,src.task_revision,src.mandate_id,
	 src.authority_generation,src.owner_member_id,src.member_id,src.message_id,src.conversation_id)=
	 (t.group_id,r.topic_id,t.id,t.revision,r.mandate_id,r.authority_generation,a.actor_id,a.actor_id,d.message_id,d.conversation_id)))`,
		i.ID, i.GroupID, i.Consumption.TurnID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type owner struct {
		task, turn string
		revision   int64
	}
	var owners []owner
	for rows.Next() {
		var o owner
		if err := rows.Scan(&o.task, &o.revision, &o.turn); err != nil {
			return err
		}
		owners = append(owners, o)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range owners {
		turn, err := requestActiveTurnCancelInTx(ctx, tx, i.GroupID, o.turn, time.Now().UTC())
		if err != nil {
			return err
		}
		if turn.CancelRequestedAt == nil || turn.State != domain.TurnStateCancelRequested {
			return ErrConflict
		}
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO task_constraint_impacts
		 (id,input_id,group_id,source_task_id,task_id,task_revision,created_at,kind,ref_id) VALUES(lower(hex(randomblob(16))),?,?,?,?,?,?,'cancel_requested',?)`,
			i.ID, i.GroupID, i.TaskID, o.task, o.revision, stamp, turn.ID); err != nil {
			return err
		}
	}
	return nil
}

func taskImpactRefStaleInTx(ctx context.Context, tx *sql.Tx, groupID string, kind domain.TaskImpactKind, refID string) (bool, error) {
	installed, err := taskImpactSchemaInTx(ctx, tx)
	if err != nil || !installed {
		return false, err
	}
	var stale bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_constraint_impacts WHERE (group_id,kind,ref_id)=(?,?,?))`, groupID, kind, refID).Scan(&stale)
	return stale, err
}

// Staleness follows the immutable pin, not the mutable downstream revision.
// A rejected or accepted historical decision is preserved separately.
const taskImpactStalePin = `EXISTS(SELECT 1 FROM task_constraint_impacts x
 WHERE x.group_id=pin.group_id AND x.kind='stale_submission' AND x.ref_id=pin.submission_id)`

func requireFreshTaskPinsInTx(ctx context.Context, tx *sql.Tx, groupID, taskID string) error {
	installed, err := taskImpactSchemaInTx(ctx, tx)
	if err != nil || !installed {
		return err
	}
	var stale bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_artifact_dependencies pin
	 WHERE (pin.group_id,pin.task_id)=(?,?) AND `+taskImpactStalePin+`)`, groupID, taskID).Scan(&stale)
	if err == nil && stale {
		return errTaskImpactPinStale
	}
	return err
}

var errTaskImpactPinStale = fmt.Errorf("%w：固定成果版本因原约束变更已过时，需重新规划", ErrConflict)

// Hold unclaimed obsolete wakes. An already bound report owner may handle a
// current WorkInput in its existing Attempt; the old report is no longer a
// live input. A stale child may settle but cannot start or publish new reports.
func taskImpactWakeBlockedInTx(ctx context.Context, tx *sql.Tx, d domain.TaskWorkDelivery, turnID string) (bool, error) {
	switch d.SourceKind {
	case domain.TaskWorkSourceDelegation:
		return taskImpactRefStaleInTx(ctx, tx, d.GroupID, domain.TaskImpactStaleDelegation, d.SourceID)
	case domain.TaskWorkSourceReport:
		stale, err := taskImpactRefStaleInTx(ctx, tx, d.GroupID, domain.TaskImpactStaleWorkReport, d.SourceID)
		if err != nil || !stale || turnID == "" {
			return stale, err
		}
		var own bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_work_continuations WHERE delivery_id=? AND turn_id=?)`,
			d.DeliveryID, turnID).Scan(&own)
		return !own, err
	default:
		return false, nil
	}
}

func requireCurrentWorkReportInTx(ctx context.Context, tx *sql.Tx, group, report string) error {
	stale, err := taskImpactRefStaleInTx(ctx, tx, group, domain.TaskImpactStaleWorkReport, report)
	if err == nil && stale {
		return errors.Join(ErrConflict, errors.New("协作报告已因后续约束变更过时，仅保留为历史记录"))
	}
	return err
}
