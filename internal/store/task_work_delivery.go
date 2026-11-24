package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"agentworkspace/internal/domain"
)

const taskWorkDeliveryColumns = `delivery_id,source_kind,source_id,assignment_id,task_id,task_revision,mandate_id,authority_generation,
 owner_member_id,member_id,sender_member_id,group_id,topic_id,conversation_id,message_id`

// Registration and both tables are installed atomically. In the current
// schema, the indexed marker queries below require both actual tables.
func taskWorkSchemaSupportedInTx(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (bool, error) {
	var installed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=29)`).Scan(&installed)
	if err != nil || installed {
		return installed, err
	}
	// Verify the legacy prefix before treating an unregistered schema as absent.
	var partial bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema
 WHERE name IN ('task_work_deliveries','task_work_continuations'))`).Scan(&partial)
	if err == nil && partial {
		err = ErrConflict
	}
	return false, err
}

func taskWorkRecord(v assignmentSnapshot, sender, sourceID string) domain.TaskWorkDelivery {
	return domain.TaskWorkDelivery{
		SourceKind: domain.TaskWorkSourceFollowup, SourceID: sourceID, AssignmentID: v.a.ID, TaskID: v.a.TaskID, MandateID: v.r.MandateID,
		TaskRevision: v.r.TaskRevision, AuthorityGeneration: v.r.AuthorityGeneration,
		OwnerMemberID: v.a.WorkerMemberID, MemberID: v.a.WorkerMemberID, SenderMemberID: sender, GroupID: v.a.GroupID, TopicID: v.r.TopicID,
	}
}

// Only immutable server refs and a fixed purpose go in the physical wake.
// The logical input/report (including any text) is fetched separately by role.
func taskWorkWakeBody(d domain.TaskWorkDelivery) string {
	return fmt.Sprintf("平台工作通知（%s，并非成员原话）\n任务：%s\n任务版本：%d\n分派：%s\n授权：%s（世代 %d）\n本通知不代表执行成功、成果提交或接纳。",
		d.SourceKind, d.TaskID, d.TaskRevision, d.AssignmentID, d.MandateID, d.AuthorityGeneration)
}

// Only the fenced assessment transaction can allocate a wake after persisting
// the exact user input. Source replays read the original receipt instead.
func taskWorkFollowupWakeInTx(ctx context.Context, tx *sql.Tx, sender MemberToolScope, i domain.TaskWorkInput, assessmentID string) (domain.TaskWorkDelivery, error) {
	if err := coordinationDecideLiveInTx(ctx, tx, sender); err != nil {
		return domain.TaskWorkDelivery{}, err
	}
	v, _, err := coordinationAssignmentLoad(ctx, tx, i.AssignmentID)
	if err != nil {
		return domain.TaskWorkDelivery{}, coordinationRoleRelationError(err)
	}
	if sender.Claim.GroupID != v.a.GroupID || sender.TopicID != v.r.TopicID {
		return domain.TaskWorkDelivery{}, ErrMemberToolDenied
	}
	want := taskWorkRecord(v, sender.MemberID, i.ID)
	assessment, actor, err := taskWorkFollowupSourceInTx(ctx, tx, want)
	if err != nil {
		return domain.TaskWorkDelivery{}, err
	}
	if assessment != assessmentID || actor != sender.Claim.TurnID {
		return domain.TaskWorkDelivery{}, ErrMemberToolDenied
	}
	want, err = taskWorkCreateWakeInTx(ctx, tx, want, nil)
	if err != nil {
		return domain.TaskWorkDelivery{}, err
	}
	saved, err := taskWorkDeliveryInTx(ctx, tx, want.GroupID, want.DeliveryID)
	if err != nil {
		return domain.TaskWorkDelivery{}, coordinationRoleRelationError(err)
	}
	if saved.d != want || saved.turnID != "" || saved.assessmentID != assessmentID || saved.sourceTurnID != sender.Claim.TurnID {
		return domain.TaskWorkDelivery{}, ErrConflict
	}
	if err := coordinationDecideLiveInTx(ctx, tx, sender); err != nil {
		return domain.TaskWorkDelivery{}, err
	}
	return saved.d, nil
}

// Only fenced producers may allocate a wake. B3 inserts its logical source
// after allocating the physical delivery but before inserting the marker.
func taskWorkCreateWakeInTx(ctx context.Context, tx *sql.Tx, want domain.TaskWorkDelivery, persistSource func(domain.TaskWorkDelivery) error) (domain.TaskWorkDelivery, error) {
	body := taskWorkWakeBody(want)
	if len(body) > 16<<10 || !validTriggerID(want.TaskID) || !validTriggerID(want.MandateID) {
		return domain.TaskWorkDelivery{}, ErrInvalid
	}
	target := []string{want.MemberID}
	m, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: want.GroupID, TopicID: want.TopicID, ActorID: want.SenderMemberID, Content: body, RecipientIDs: target}, target)
	if err != nil {
		return domain.TaskWorkDelivery{}, err
	}
	if m.GroupID != want.GroupID || m.TopicID != want.TopicID || m.SenderID != want.SenderMemberID || m.Content != body || len(m.Deliveries) != 1 {
		return domain.TaskWorkDelivery{}, ErrConflict
	}
	d := m.Deliveries[0]
	if d.RecipientID != want.MemberID || d.MessageID != m.ID || d.State != domain.DeliveryStateQueued || d.TurnID != "" {
		return domain.TaskWorkDelivery{}, ErrConflict
	}
	want.DeliveryID, want.MessageID, want.ConversationID = d.ID, m.ID, d.ConversationID
	if persistSource != nil {
		if err := persistSource(want); err != nil {
			return domain.TaskWorkDelivery{}, err
		}
	}
	if err := coordinationDispatchWrite(ctx, tx, "INSERT INTO task_work_deliveries("+taskWorkDeliveryColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		want.DeliveryID, want.SourceKind, want.SourceID, want.AssignmentID, want.TaskID, want.TaskRevision, want.MandateID, want.AuthorityGeneration,
		want.OwnerMemberID, want.MemberID, want.SenderMemberID, want.GroupID, want.TopicID, want.ConversationID, want.MessageID); err != nil {
		return domain.TaskWorkDelivery{}, err
	}
	return want, nil
}

// Resolve the original wake on replay; a missing marker never allocates again.
func taskWorkB3WakeInTx(ctx context.Context, tx *sql.Tx, sender MemberToolScope, v assignmentSnapshot, kind domain.TaskWorkSourceKind,
	sourceID, memberID string, replay bool, persistSource func(domain.TaskWorkDelivery) error) (taskWorkFacts, error) {
	want := taskWorkRecord(v, sender.MemberID, sourceID)
	want.SourceKind, want.MemberID = kind, memberID
	if (kind != domain.TaskWorkSourceDelegation && kind != domain.TaskWorkSourceReport) ||
		want.GroupID != sender.Claim.GroupID || want.TopicID != sender.TopicID ||
		!taskWorkTargetValid(want, v.r.CoordinatorMemberID) {
		return taskWorkFacts{}, ErrMemberToolDenied
	}
	var err error
	if replay {
		err = tx.QueryRowContext(ctx, `SELECT delivery_id,message_id,conversation_id FROM task_work_deliveries
		 WHERE source_kind=? AND source_id=?`, kind, sourceID).Scan(&want.DeliveryID, &want.MessageID, &want.ConversationID)
	} else {
		want, err = taskWorkCreateWakeInTx(ctx, tx, want, persistSource)
	}
	if err != nil {
		return taskWorkFacts{}, coordinationRoleRelationError(err)
	}
	saved, err := taskWorkDeliveryInTx(ctx, tx, want.GroupID, want.DeliveryID)
	if err != nil {
		return taskWorkFacts{}, coordinationRoleRelationError(err)
	}
	if saved.d != want || saved.sourceTurnID != sender.Claim.TurnID || (!replay && saved.turnID != "") {
		return taskWorkFacts{}, ErrConflict
	}
	return saved, nil
}

func taskWorkTargetValid(d domain.TaskWorkDelivery, coordinator string) bool {
	switch d.SourceKind {
	case domain.TaskWorkSourceFollowup, domain.TaskWorkSourceReport:
		return d.MemberID == d.OwnerMemberID
	case domain.TaskWorkSourceDelegation:
		return d.MemberID != d.OwnerMemberID && d.MemberID != coordinator
	default:
		return false // Handbacks record notices, never native wakes.
	}
}

const taskWorkClaimDeliveryState = `CASE WHEN t.accepted_at IS NOT NULL THEN 'accepted'
 WHEN t.state IN ('starting','running','cancel_requested','unknown') THEN 'queued' ELSE 'failed' END`

type taskWorkFacts struct {
	d                                  domain.TaskWorkDelivery
	v                                  assignmentSnapshot
	dispatch                           domain.AssignmentDispatchState
	turnID, assessmentID, sourceTurnID string
	decisionAnswer                     bool
	delegation                         *domain.TaskWorkDelegation
	report                             *domain.TaskWorkReport
}

// Probe every identity by its unique key before joining any proof. An orphan
// continuation or a different delivery for the same wake is not ordinary work.
func taskWorkMarkerInTx(ctx context.Context, tx *sql.Tx, deliveryID string) (bool, error) {
	var marked, continuation bool
	var byMessage string
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=?),
 coalesce((SELECT delivery_id FROM task_work_deliveries WHERE message_id=(SELECT message_id FROM deliveries WHERE id=?)),''),
 EXISTS(SELECT 1 FROM task_work_continuations WHERE delivery_id=?)`, deliveryID, deliveryID, deliveryID).Scan(&marked, &byMessage, &continuation)
	if err == nil && ((byMessage != "" && (!marked || byMessage != deliveryID)) || (continuation && !marked)) {
		err = ErrMemberToolDenied
	}
	return marked, err
}

// Read the raw marker first. A damaged relation cannot be mistaken for an
// ordinary delivery. The Assignment loader also verifies original provenance.
func taskWorkDeliveryInTx(ctx context.Context, tx *sql.Tx, groupID, deliveryID string) (f taskWorkFacts, err error) {
	marked, err := taskWorkMarkerInTx(ctx, tx, deliveryID)
	if err != nil {
		return f, err
	}
	if !marked {
		return f, ErrNotFound
	}
	d := &f.d
	err = tx.QueryRowContext(ctx, "SELECT "+taskWorkDeliveryColumns+` FROM task_work_deliveries WHERE delivery_id=?`, deliveryID).Scan(
		&d.DeliveryID, &d.SourceKind, &d.SourceID, &d.AssignmentID, &d.TaskID, &d.TaskRevision, &d.MandateID, &d.AuthorityGeneration,
		&d.OwnerMemberID, &d.MemberID, &d.SenderMemberID, &d.GroupID, &d.TopicID, &d.ConversationID, &d.MessageID)
	if err != nil {
		return f, coordinationRoleRelationError(err)
	}
	if d.GroupID != groupID {
		return f, ErrMemberToolDenied
	}
	for _, id := range []string{d.DeliveryID, d.SourceID, d.AssignmentID, d.TaskID, d.MandateID, d.OwnerMemberID, d.MemberID, d.SenderMemberID, d.ConversationID, d.MessageID} {
		if !validTriggerID(id) {
			return f, ErrMemberToolDenied
		}
	}
	f.v, f.dispatch, err = coordinationAssignmentLoad(ctx, tx, d.AssignmentID)
	if err != nil {
		return f, coordinationRoleRelationError(err)
	}
	r := f.v.r
	if f.v.a.GroupID != d.GroupID || f.v.a.TaskID != d.TaskID || f.v.a.WorkerMemberID != d.OwnerMemberID ||
		f.dispatch.Delivery == nil || r.TopicID != d.TopicID || r.TaskRevision != d.TaskRevision ||
		r.MandateID != d.MandateID || r.AuthorityGeneration != d.AuthorityGeneration || !taskWorkTargetValid(*d, r.CoordinatorMemberID) {
		return f, ErrMemberToolDenied
	}
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT d.turn_id, EXISTS(SELECT 1 FROM messages m JOIN conversations c ON c.id=d.conversation_id
 JOIN members w ON w.id=d.recipient_id JOIN members sender ON sender.id=m.sender_id
 WHERE m.id=d.message_id AND m.group_id=d.group_id AND m.topic_id=d.topic_id AND m.sender_id=:sender AND m.content=:body
 AND (c.group_id,c.topic_id,c.member_id)=(d.group_id,d.topic_id,d.recipient_id)
 AND w.group_id=d.group_id AND sender.group_id=d.group_id
 AND NOT EXISTS(SELECT 1 FROM deliveries other WHERE other.message_id=m.id AND other.id<>d.id)
 AND NOT EXISTS(SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=d.id OR message_id=m.id)
 AND NOT EXISTS(SELECT 1 FROM coordination_assignment_dispatch WHERE delivery_id=d.id OR message_id=m.id)
 AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=m.id)
 AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.delivery_id=d.id AND other.id<>d.turn_id)
 AND ((d.turn_id='' AND d.state='queued') OR EXISTS(SELECT 1 FROM turns t WHERE t.id=d.turn_id
 AND (t.delivery_id,t.group_id,t.conversation_id,t.member_id)=(d.id,d.group_id,d.conversation_id,d.recipient_id)
 AND d.state=`+taskWorkClaimDeliveryState+`)))
 FROM deliveries d WHERE d.id=:delivery AND (d.message_id,d.group_id,d.topic_id,d.conversation_id,d.recipient_id)=
 (:message,:grp,:topic,:conversation,:member)`,
		sql.Named("delivery", d.DeliveryID), sql.Named("message", d.MessageID), sql.Named("grp", d.GroupID), sql.Named("topic", d.TopicID),
		sql.Named("conversation", d.ConversationID), sql.Named("member", d.MemberID), sql.Named("sender", d.SenderMemberID), sql.Named("body", taskWorkWakeBody(*d))).Scan(&f.turnID, &valid)
	if err != nil {
		return f, coordinationRoleRelationError(err)
	}
	if !valid {
		return f, ErrMemberToolDenied
	}
	switch {
	case d.SourceKind == domain.TaskWorkSourceFollowup && (isDecisionAnswerID(d.SourceID) || strings.HasPrefix(d.SourceID, decisionAnswerReplacementPrefix)):
		d.SourceID, err = decisionAnswerWakeSourceInTx(ctx, tx, *d)
		f.decisionAnswer = err == nil
	case d.SourceKind == domain.TaskWorkSourceFollowup:
		f.assessmentID, f.sourceTurnID, err = taskWorkFollowupSourceInTx(ctx, tx, *d)
	default:
		err = taskWorkReportSourceInTx(ctx, tx, &f)
	}
	return f, err
}

func lookupTaskWorkDeliveryInTx(ctx context.Context, tx *sql.Tx, groupID, deliveryID string) (taskWorkFacts, error) {
	supported, err := taskWorkSchemaSupportedInTx(ctx, tx)
	if err != nil {
		return taskWorkFacts{}, err
	}
	if !supported {
		return taskWorkFacts{}, ErrNotFound
	}
	return taskWorkDeliveryInTx(ctx, tx, groupID, deliveryID)
}

// ClaimTurn has reserved the delivery and inserted its Turn in this transaction.
// Busy rolls back the claim; it is not a handling or settlement receipt.
func (s *Store) taskWorkClaimInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn) error {
	f, err := lookupTaskWorkDeliveryInTx(ctx, tx, turn.GroupID, turn.DeliveryID)
	if errors.Is(err, ErrNotFound) {
		return nil // Verified absence of a typed marker.
	}
	if err != nil {
		return err
	}
	if f.turnID != turn.ID || f.d.MemberID != turn.MemberID || f.d.ConversationID != turn.ConversationID {
		return ErrMemberToolDenied
	}
	if f.d.SourceKind == domain.TaskWorkSourceFollowup {
		handled, err := taskWorkInputHandledInTx(ctx, tx, f.d.SourceID)
		if err != nil {
			return err
		}
		if handled {
			return ErrTurnBusy
		}
	}
	blocked, _, err := s.taskWorkReadinessInTx(ctx, tx, f, turn.ID)
	if err == nil && blocked != "" {
		err = ErrTurnBusy
	}
	return err
}

// Legacy write paths must not treat even a damaged typed marker as chat or a
// generic submission. Do not consult current readiness for this check.
func denyTaskWorkFallbackInTx(ctx context.Context, tx *sql.Tx, turnID string) error {
	supported, err := taskWorkSchemaSupportedInTx(ctx, tx)
	if err != nil || !supported {
		return err
	}
	var deliveryID string
	if err := tx.QueryRowContext(ctx, `SELECT delivery_id FROM turns WHERE id=?`, turnID).Scan(&deliveryID); err != nil {
		return coordinationRoleRelationError(err)
	}
	marked, err := taskWorkMarkerInTx(ctx, tx, deliveryID)
	if err == nil && marked {
		err = ErrMemberToolDenied
	}
	return err
}

// ResolveTaskWorkDelivery is a peek, not a claim or a grant. A genuine typed
// delivery stays typed when blocked, stale, or already claimed.
func (s *Store) ResolveTaskWorkDelivery(ctx context.Context, groupID, deliveryID string) (out domain.TaskWorkDelivery, err error) {
	phase := "begin"
	var tx *sql.Tx
	defer func() { err = coordinationAssignmentFinish(tx, phase, err) }()
	if !validTriggerID(groupID) || !validTriggerID(deliveryID) {
		return out, ErrInvalid
	}
	tx, err = s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	phase = "delivery"
	f, err := lookupTaskWorkDeliveryInTx(ctx, tx, groupID, deliveryID)
	if err != nil {
		return out, err
	}
	if f.turnID != "" {
		f.d.Blocked = "already_claimed"
	} else if f.d.Blocked, _, err = s.taskWorkReadinessInTx(ctx, tx, f, ""); err != nil {
		return out, err
	}
	phase = "commit"
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return f.d, nil
}

// The shared first-write memberToolScopeInTx fence must precede these reads.
func taskWorkExecutionInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope) (taskWorkFacts, error) {
	f, err := lookupTaskWorkExecutionInTx(ctx, tx, scope)
	return f, coordinationRoleRelationError(err)
}

// Only verified absence of a raw work marker returns ErrNotFound.
func lookupTaskWorkExecutionInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope) (taskWorkFacts, error) {
	var deliveryID string
	err := tx.QueryRowContext(ctx, "SELECT t.delivery_id"+memberToolBinding+" AND t.member_id=? AND t.conversation_id=? AND c.topic_id=?",
		scope.Claim.GroupID, scope.Claim.TurnID, scope.Claim.Token, scope.MemberID, scope.ConversationID, scope.TopicID).Scan(&deliveryID)
	if err != nil {
		return taskWorkFacts{}, coordinationRoleRelationError(err)
	}
	f, err := lookupTaskWorkDeliveryInTx(ctx, tx, scope.Claim.GroupID, deliveryID)
	if err != nil {
		return taskWorkFacts{}, err
	}
	if f.turnID != scope.Claim.TurnID || f.d.MemberID != scope.MemberID || f.d.ConversationID != scope.ConversationID || f.d.TopicID != scope.TopicID {
		return taskWorkFacts{}, ErrMemberToolDenied
	}
	return f, nil
}

// GetTaskWorkExecution derives typed identity from a live native claim. For
// startup, callers must honor Blocked and check applicable investigation policy.
func (s *Store) GetTaskWorkExecution(ctx context.Context, scope MemberToolScope) (domain.TaskWorkDelivery, error) {
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return domain.TaskWorkDelivery{}, memberToolOperationError("读取工作轮次失败", err)
	}
	defer tx.Rollback()
	f, err := taskWorkExecutionInTx(ctx, tx, scope)
	if err == nil {
		f.d.Blocked, _, err = s.taskWorkReadinessInTx(ctx, tx, f, scope.Claim.TurnID)
	}
	if err != nil {
		return domain.TaskWorkDelivery{}, memberToolOperationError("读取工作轮次失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.TaskWorkDelivery{}, memberToolOperationError("读取工作轮次失败", err)
	}
	return f.d, nil
}
