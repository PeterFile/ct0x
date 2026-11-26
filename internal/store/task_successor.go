package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"agentworkspace/internal/domain"
)

type taskSuccessorProof struct {
	predecessorMandateID string
	chatPolicyRevision   int64
}

func taskSuccessorSchemaInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	version := slices.Index(migrations, taskSuccessorSchema) + 1
	if version == 0 {
		return false, nil
	}
	var installed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=?)`, version).Scan(&installed)
	return installed, err
}

func prepareTaskSuccessorInTx(ctx context.Context, tx *sql.Tx, trigger domain.Trigger, prior domain.TaskSuccessorInput) (taskSuccessorProof, error) {
	var proof taskSuccessorProof
	installed, err := taskSuccessorSchemaInTx(ctx, tx)
	if err != nil {
		return proof, err
	}
	if !installed {
		return proof, fmt.Errorf("%w：当前数据库尚未启用已接纳任务续办", ErrConflict)
	}
	kind, id, err := messageSourceInTx(ctx, tx, trigger.MessageID)
	if err != nil {
		return proof, err
	}
	if kind != "local_user_chat" || id == "" {
		return proof, fmt.Errorf("%w：续办须来自明确的本机用户消息", ErrConflict)
	}
	thread := domain.ThreadRef{GroupID: trigger.GroupID, TopicID: trigger.TopicID}
	policy, err := chatPolicyInTx(ctx, tx, thread, 0)
	if err != nil {
		return proof, err
	}
	if policy.Revision < 1 || !policy.Enabled || policy.AuthorizedBy != "user" || policy.Action != domain.ChatActionTextSubmission {
		return proof, fmt.Errorf("%w：续办只允许在启用文本工作的同一话题进行", ErrConflict)
	}
	proof.chatPolicyRevision = policy.Revision
	group := trigger.GroupID
	task, err := taskInTx(ctx, tx, group, prior.TaskID)
	if err != nil {
		return proof, err
	}
	if task.Status != domain.TaskStatusAccepted || task.Revision != prior.ExpectedRevision || task.CurrentSubmissionID != prior.SubmissionID || task.ActiveAttemptID != "" {
		return proof, fmt.Errorf("%w：来源任务、版本或当前接纳提交不匹配", ErrConflict)
	}
	sub, err := taskSubmissionInTx(ctx, tx, group, task.ID, prior.SubmissionID)
	if err != nil {
		return proof, err
	}
	review, err := taskReviewInTx(ctx, tx, group, task.ID, prior.ReviewID)
	if err != nil {
		return proof, err
	}
	if review.Outcome != domain.ReviewOutcomeAccepted || review.ReviewedBy != "user" || review.SubmissionID != sub.ID ||
		review.TaskRevision != task.Revision || sub.TaskRevision != task.Revision || !slices.Equal(review.ArtifactVersions, sub.ArtifactVersions) {
		return proof, fmt.Errorf("%w：原评审未接纳该确切提交及其成果版本", ErrConflict)
	}
	err = tx.QueryRowContext(ctx, `SELECT m.id FROM task_coordination c
	 JOIN mandates m ON (m.id,m.group_id,m.task_id,m.topic_id)=(c.current_mandate_id,c.group_id,c.task_id,c.topic_id)
	 JOIN routing_decisions d ON (d.id,d.mandate_id,d.group_id,d.task_id,d.topic_id,d.trigger_id,d.task_revision_at_admission)=
	 (m.decision_id,m.id,m.group_id,m.task_id,m.topic_id,m.origin_trigger_id,m.task_revision_at_admission)
	 WHERE (c.group_id,c.topic_id,c.task_id,m.task_revision_at_admission)=(?,?,?,?) AND m.authorized_by='user'`,
		group, trigger.TopicID, task.ID, task.Revision).Scan(&proof.predecessorMandateID)
	if errors.Is(err, sql.ErrNoRows) {
		return proof, fmt.Errorf("%w：来源任务没有同话题的当前授权记录", ErrConflict)
	}
	if err != nil {
		return proof, err
	}
	return proof, requireSettledTaskPredecessorInTx(ctx, tx, group, task.ID)
}

func requireSettledTaskPredecessorInTx(ctx context.Context, tx *sql.Tx, group, task string) error {
	var unsettled bool
	err := tx.QueryRowContext(ctx, taskWorkUnsettledTurns, sql.Named("grp", group), sql.Named("task", task),
		sql.Named("coordinator", nil), sql.Named("original", nil), sql.Named("owner_running", nil), sql.Named("turn", nil)).Scan(&unsettled)
	if err != nil {
		return err
	}
	if unsettled {
		return fmt.Errorf("%w：%w", ErrConflict, ErrChatRepairPending)
	}
	// A delivery can acquire an external Turn before an Attempt is bound.
	const materialized = `WITH prior(delivery_id) AS (
	 SELECT delivery_id FROM coordination_dispatch_state WHERE group_id=:grp AND task_id=:task AND delivery_id IS NOT NULL
	 UNION SELECT delivery_id FROM coordination_assignment_dispatch WHERE group_id=:grp AND task_id=:task AND delivery_id IS NOT NULL)
	 SELECT EXISTS(SELECT 1 FROM prior p LEFT JOIN deliveries d ON d.id=p.delivery_id AND d.group_id=:grp
	 LEFT JOIN turns h ON (h.delivery_id,h.group_id,h.conversation_id,h.member_id)=(d.id,d.group_id,d.conversation_id,d.recipient_id)
	 WHERE NOT ` + taskWorkTurnSettled + `)`
	if err := tx.QueryRowContext(ctx, materialized, sql.Named("grp", group), sql.Named("task", task)).Scan(&unsettled); err != nil {
		return err
	}
	if unsettled {
		return fmt.Errorf("%w：%w", ErrConflict, ErrChatRepairPending)
	}
	unsettled, err = workspaceOperationsUnresolvedInTx(ctx, tx, group, task, "")
	if err == nil && unsettled {
		return fmt.Errorf("%w：%w", ErrConflict, ErrChatRepairPending)
	}
	return err
}

func insertTaskSuccessorInTx(ctx context.Context, tx *sql.Tx, trigger domain.Trigger, task domain.Task, mandate, reason string, prior domain.TaskSuccessorInput, proof taskSuccessorProof) error {
	dispatch, err := coordinationDispatchCreateInTx(ctx, tx, task.GroupID, task.ID, task.Revision, 1, reason)
	if err != nil {
		return err
	}
	return coordinationDispatchWrite(ctx, tx, `INSERT INTO task_successors
	 (successor_task_id,group_id,topic_id,predecessor_task_id,predecessor_task_revision,predecessor_submission_id,
	 predecessor_review_id,predecessor_mandate_id,successor_mandate_id,successor_dispatch_request_id,source_trigger_id,chat_policy_revision)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, task.ID, task.GroupID, trigger.TopicID, prior.TaskID, prior.ExpectedRevision,
		prior.SubmissionID, prior.ReviewID, proof.predecessorMandateID, mandate, dispatch.ID, trigger.ID, proof.chatPolicyRevision)
}

func readTaskSuccessorReceiptInTx(ctx context.Context, tx *sql.Tx, receipt *domain.AdmissionReceipt) error {
	installed, err := taskSuccessorSchemaInTx(ctx, tx)
	if err != nil || !installed {
		return err
	}
	var prior domain.TaskSuccessorInput
	err = tx.QueryRowContext(ctx, `SELECT predecessor_task_id,predecessor_task_revision,predecessor_submission_id,
	 predecessor_review_id,successor_dispatch_request_id FROM task_successors
	 WHERE (successor_task_id,group_id,topic_id,successor_mandate_id,source_trigger_id)=(?,?,?,?,?)`,
		receipt.Task.ID, receipt.Task.GroupID, receipt.Mandate.TopicID, receipt.Mandate.ID, receipt.Mandate.OriginTriggerID).Scan(
		&prior.TaskID, &prior.ExpectedRevision, &prior.SubmissionID, &prior.ReviewID, &receipt.DispatchRequestID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err == nil {
		receipt.Predecessor = &prior
	}
	return err
}

// VerifyQueuedTextTaskSuccessor is a preclaim scope check for the exact
// successor wake. Claim and launch must still recheck their own fences.
func (s *Store) VerifyQueuedTextTaskSuccessor(ctx context.Context, delivery domain.TaskWorkDelivery, requestID string) error {
	if delivery.SourceKind != domain.TaskWorkSourceFollowup || delivery.Blocked != "" ||
		!validTriggerID(delivery.GroupID) || !validTriggerID(delivery.DeliveryID) || !validTriggerID(requestID) {
		return ErrMemberToolDenied
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	f, err := lookupTaskWorkDeliveryInTx(ctx, tx, delivery.GroupID, delivery.DeliveryID)
	if err != nil {
		return err
	}
	if f.d != delivery || f.turnID != "" || f.v.r.ID != requestID {
		return ErrMemberToolDenied
	}
	if _, err := taskSuccessorChatBasisInTx(ctx, tx, f.v.r); err != nil {
		if errors.Is(err, errTaskWorkChatScope) {
			return ErrMemberToolDenied
		}
		return err
	}
	return tx.Commit()
}

// The new dispatch is eligible for normal text followups only while the exact
// explicit command, Mandate, and text policy recorded at admission still agree.
func taskSuccessorChatBasisInTx(ctx context.Context, tx *sql.Tx, request domain.DispatchRequest) (basis taskWorkChatBasis, err error) {
	installed, err := taskSuccessorSchemaInTx(ctx, tx)
	if err != nil {
		return basis, err
	}
	if !installed {
		return basis, errTaskWorkChatScope
	}
	var messageID string
	err = tx.QueryRowContext(ctx, `SELECT r.action,r.revision,coalesce(s.command_source_id,''),o.message_id
	 FROM task_successors a JOIN mandates m ON (m.id,m.group_id,m.topic_id,m.task_id,m.origin_trigger_id)=
	 (a.successor_mandate_id,a.group_id,a.topic_id,a.successor_task_id,a.source_trigger_id)
	 JOIN coordination_triggers o ON (o.id,o.group_id,o.topic_id)=(a.source_trigger_id,a.group_id,a.topic_id)
	 JOIN chat_policies p ON (p.group_id,p.topic_id,p.revision)=(a.group_id,a.topic_id,a.chat_policy_revision)
	 JOIN chat_policy_revisions r ON (r.group_id,r.topic_id,r.revision)=(p.group_id,p.topic_id,p.revision)
	 LEFT JOIN chat_policy_scopes s ON (s.group_id,s.topic_id,s.revision)=(p.group_id,p.topic_id,p.revision)
	 WHERE (a.successor_task_id,a.group_id,a.topic_id,a.successor_mandate_id,a.successor_dispatch_request_id)=(?,?,?,?,?)
	 AND m.task_revision_at_admission=? AND m.authority_kind='local_user_explicit' AND m.authorized_by='user'
	 AND r.enabled=1 AND r.authorized_by='user' AND r.action=?
	 AND NOT EXISTS(SELECT 1 FROM chat_workspace_scopes WHERE task_id=a.successor_task_id)`, request.TaskID,
		request.GroupID, request.TopicID, request.MandateID, request.ID, request.TaskRevision, domain.ChatActionTextSubmission).Scan(
		&basis.action, &basis.policyRevision, &basis.policySourceID, &messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return taskWorkChatBasis{}, errTaskWorkChatScope
	}
	if err != nil {
		return taskWorkChatBasis{}, err
	}
	kind, id, err := messageSourceInTx(ctx, tx, messageID)
	if err != nil {
		return taskWorkChatBasis{}, err
	}
	if kind != "local_user_chat" || id == "" {
		return taskWorkChatBasis{}, errTaskWorkChatScope
	}
	return basis, nil
}

func taskSuccessorSubmissionInTx(ctx context.Context, tx *sql.Tx, request domain.DispatchRequest) (string, error) {
	installed, err := taskSuccessorSchemaInTx(ctx, tx)
	if err != nil || !installed {
		return "", err
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT predecessor_submission_id FROM task_successors
	 WHERE (successor_task_id,group_id,topic_id,successor_mandate_id,successor_dispatch_request_id)=(?,?,?,?,?)`,
		request.TaskID, request.GroupID, request.TopicID, request.MandateID, request.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func isTextTaskSuccessorInTx(ctx context.Context, tx *sql.Tx, groupID, taskID string) (bool, error) {
	installed, err := taskSuccessorSchemaInTx(ctx, tx)
	if !installed || err != nil {
		return false, err
	}
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_successors WHERE (successor_task_id,group_id)=(?,?))`, taskID, groupID).Scan(&exists)
	return exists, err
}
