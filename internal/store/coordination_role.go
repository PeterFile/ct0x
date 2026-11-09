package store

import (
	"context"
	"database/sql"
	"errors"
)

type CoordinationRoleKind string

const (
	CoordinationRoleOrdinary    CoordinationRoleKind = "ordinary"
	CoordinationRoleCoordinator CoordinationRoleKind = "coordinator"
	CoordinationRoleWorker      CoordinationRoleKind = "worker"
)

type CoordinationRoleProvenance struct {
	Kind                                                              CoordinationRoleKind `json:"-"`
	GroupID, DeliveryID, MessageID, ConversationID, MemberID, TopicID string               `json:"-"`
	RequestID, TaskID, AssignmentID                                   string               `json:"-"`
	TaskRevision                                                      int64                `json:"-"`
}

type CoordinationToolScope struct {
	Member     MemberToolScope            `json:"-"`
	Provenance CoordinationRoleProvenance `json:"-"`
	AttemptID  string                     `json:"-"`
}

// This SELECT must immediately follow the shared first-write scope fence.
func coordinationDecideLiveInTx(ctx context.Context, tx *sql.Tx, m MemberToolScope) error {
	var found int
	err := tx.QueryRowContext(ctx, "SELECT 1"+memberToolBinding+" AND t.member_id=? AND t.conversation_id=? AND c.topic_id=?",
		m.Claim.GroupID, m.Claim.TurnID, m.Claim.Token, m.MemberID, m.ConversationID, m.TopicID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMemberToolDenied
	}
	return err
}

// Raw receipt candidates are selected before validating relations: a broken second role cannot disappear.
// Each receipt delivery key is UNIQUE, so this observes at most two candidates. No current eligibility is read.
func coordinationExecutionProvenanceInTx(ctx context.Context, tx *sql.Tx, member MemberToolScope, expected CoordinationRoleProvenance) (CoordinationRoleProvenance, error) {
	rows, err := tx.QueryContext(ctx, `WITH source AS (
	 SELECT 'coordinator' kind,s.request_id,s.task_id,s.group_id,s.delivery_id,s.message_id,s.conversation_id,
	 s.delivered_member_id,'' assignment_id,1 linked FROM coordination_dispatch_state s WHERE s.delivery_id=(SELECT delivery_id FROM turns WHERE id=?)
	 UNION ALL SELECT 'worker',a.request_id,s.task_id,s.group_id,s.delivery_id,s.message_id,s.conversation_id,
	 s.delivered_member_id,s.assignment_id,(a.task_id=s.task_id AND a.group_id=s.group_id
	 AND a.worker_member_id=s.worker_member_id AND s.worker_member_id=s.delivered_member_id)
	 FROM coordination_assignment_dispatch s LEFT JOIN coordination_assignments a ON a.id=s.assignment_id
	 WHERE s.delivery_id=(SELECT delivery_id FROM turns WHERE id=?))
	 SELECT p.kind,t.group_id,d.id,d.message_id,t.conversation_id,t.member_id,d.topic_id,
	 coalesce(r.id,''),coalesce(r.task_id,''),coalesce(r.task_revision,0),p.assignment_id,
	 coalesce(p.linked AND r.task_id=p.task_id AND r.group_id=p.group_id AND r.group_id=t.group_id
	 AND d.turn_id=t.id AND d.group_id=t.group_id AND d.conversation_id=t.conversation_id AND d.recipient_id=t.member_id
	 AND p.message_id=d.message_id AND p.conversation_id=d.conversation_id AND p.delivered_member_id=d.recipient_id
	 AND r.topic_id=d.topic_id AND m.group_id=d.group_id AND m.topic_id=d.topic_id
	 AND ((p.kind='coordinator' AND r.coordinator_member_id=t.member_id AND m.sender_id='user')
	 OR (p.kind='worker' AND m.sender_id=r.coordinator_member_id)),0)
	 FROM turns t JOIN deliveries d ON d.id=t.delivery_id JOIN source p ON p.delivery_id=d.id
	 LEFT JOIN coordination_dispatch_requests r ON r.id=p.request_id LEFT JOIN messages m ON m.id=d.message_id
	 WHERE t.id=? AND t.group_id=? AND t.member_id=? AND t.conversation_id=? AND d.topic_id=?`,
		member.Claim.TurnID, member.Claim.TurnID, member.Claim.TurnID, member.Claim.GroupID, member.MemberID, member.ConversationID, member.TopicID)
	if err != nil {
		return CoordinationRoleProvenance{}, err
	}
	defer rows.Close()
	var actual CoordinationRoleProvenance
	count, valid := 0, true
	for rows.Next() {
		var linked bool
		if err := rows.Scan(&actual.Kind, &actual.GroupID, &actual.DeliveryID, &actual.MessageID, &actual.ConversationID, &actual.MemberID,
			&actual.TopicID, &actual.RequestID, &actual.TaskID, &actual.TaskRevision, &actual.AssignmentID, &linked); err != nil {
			return CoordinationRoleProvenance{}, err
		}
		count++
		valid = valid && linked
	}
	if err := rows.Err(); err != nil {
		return CoordinationRoleProvenance{}, err
	}
	if count != 1 || !valid || actual != expected {
		return CoordinationRoleProvenance{}, ErrMemberToolDenied
	}
	return actual, nil
}
