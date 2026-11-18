package store

import (
	"context"
	"database/sql"
	"errors"

	"agentworkspace/internal/domain"
)

var ErrTaskWorkExecutionNotReady = errors.New("原任务后续执行尚未启用")

// EnableTaskWorkExecution is a process-local startup assertion by the trusted
// runtime, after it has installed the typed-role preparation and execution path.
// It is not a policy setting or a provider/adapter capability.
func (s *Store) EnableTaskWorkExecution() { s.taskWorkExecution.Store(true) }

// Verify the exact logical input and the assessor's actual source Turn, not
// just a syntactically valid source ID or a message supplied by the model.
func taskWorkFollowupSourceInTx(ctx context.Context, tx *sql.Tx, d domain.TaskWorkDelivery) (assessmentID, actorTurn string, err error) {
	installed, err := workInputSchemaInTx(ctx, tx)
	if err != nil {
		return "", "", err
	}
	if !installed {
		return "", "", ErrMemberToolDenied
	}
	err = tx.QueryRowContext(ctx, `SELECT a.id,c.turn_id FROM task_work_inputs i
	 JOIN chat_followup_routes r ON (r.source_id,r.group_id,r.topic_id)=(i.source_id,i.group_id,i.topic_id)
	 JOIN message_command_sources s ON s.id=r.source_id AND s.message_id=r.reply_message_id AND s.kind='local_user_chat'
	 JOIN messages m ON (m.id,m.group_id,m.topic_id)=(r.reply_message_id,i.group_id,i.topic_id) AND m.sender_id='user' AND m.content=i.content
	 JOIN coordination_triggers g ON (g.id,g.group_id,g.topic_id,g.message_id)=(r.trigger_id,i.group_id,i.topic_id,m.id) AND g.source_kind='local_im'
	 JOIN message_log l ON (l.sequence,l.group_id,l.topic_id,l.message_id)=(g.message_sequence,i.group_id,i.topic_id,m.id)
	 JOIN chat_assessments a ON a.id=r.assessment_id AND a.notice_message_id=r.notice_message_id AND a.decision_id IS NULL AND a.dispatch_request_id IS NULL
	 JOIN chat_candidates c ON (c.id,c.group_id,c.topic_id,c.message_id,c.generation)=(a.candidate_id,i.group_id,i.topic_id,m.id,l.sequence)
	 JOIN chat_intake_executions e ON (e.candidate_id,e.turn_id,e.source_id,e.source_kind)=(c.id,c.turn_id,s.id,s.kind) AND e.purpose='chat_intake/v1'
	 JOIN turns t ON (t.id,t.group_id,t.member_id)=(c.turn_id,i.group_id,c.member_id)
	 JOIN deliveries o ON (o.id,o.turn_id,o.message_id,o.group_id,o.topic_id,o.conversation_id,o.recipient_id)=
	 (t.delivery_id,t.id,m.id,i.group_id,i.topic_id,t.conversation_id,c.member_id)
	 AND o.state=`+taskWorkClaimDeliveryState+`
	 WHERE (i.id,i.assignment_id,i.task_id,i.task_revision,i.mandate_id,i.worker_member_id,i.group_id,i.topic_id)=(?,?,?,?,?,?,?,?)
	 AND c.member_id=? AND json_extract(a.receipt_json,'$.assessed_by') IS c.member_id
	 AND json_extract(a.receipt_json,'$.id') IS a.id AND json_extract(a.receipt_json,'$.candidate_id') IS c.id
	 AND json_extract(a.receipt_json,'$.group_id') IS c.group_id AND json_extract(a.receipt_json,'$.topic_id') IS c.topic_id
	 AND json_extract(a.receipt_json,'$.message_id') IS m.id AND json_extract(a.receipt_json,'$.notice_message_id') IS a.notice_message_id
	 AND json_extract(a.receipt_json,'$.candidate_generation') IS c.generation AND json_extract(a.receipt_json,'$.policy_revision') IS c.policy_revision
	 AND json_extract(a.receipt_json,'$.purpose') IS e.purpose AND json_extract(a.receipt_json,'$.source_sender_id') IS m.sender_id
	 AND json_extract(a.receipt_json,'$.authority') IS 'existing_mandate' AND json_type(a.receipt_json,'$.authority_evidence_verified') IS 'true'
	 AND json_extract(a.receipt_json,'$.turn_id') IS c.turn_id AND json_extract(a.receipt_json,'$.work_input_id') IS i.id
	 AND json_extract(a.receipt_json,'$.source_id') IS s.id AND json_extract(a.receipt_json,'$.source_kind') IS s.kind
	 AND json_extract(a.receipt_json,'$.task_id') IS i.task_id AND json_extract(a.receipt_json,'$.mandate_id') IS i.mandate_id
	 AND json_extract(a.receipt_json,'$.disposition') IS 'continue_work' AND json_extract(a.receipt_json,'$.outcome') IS 'continued'
	 AND json_type(a.receipt_json,'$.work_input_pending') IS NULL`,
		d.SourceID, d.AssignmentID, d.TaskID, d.TaskRevision, d.MandateID, d.OwnerMemberID, d.GroupID, d.TopicID, d.SenderMemberID).Scan(&assessmentID, &actorTurn)
	return assessmentID, actorTurn, coordinationRoleRelationError(err)
}

// A wake's source_id is the WorkInput ID. The WorkInput's own source_id is
// instead a chat command source, never the key of a handling fact.
func taskWorkInputHandledSQL(inputIDColumn string) string {
	return `(EXISTS(SELECT 1 FROM task_work_input_consumptions WHERE input_id=` + inputIDColumn + `)
	 OR EXISTS(SELECT 1 FROM task_work_input_returns WHERE input_id=` + inputIDColumn + `))`
}

func taskWorkInputHandledInTx(ctx context.Context, tx *sql.Tx, inputID string) (bool, error) {
	var handled bool
	err := tx.QueryRowContext(ctx, `SELECT `+taskWorkInputHandledSQL("i.id")+` FROM task_work_inputs i WHERE i.id=?`, inputID).Scan(&handled)
	return handled, coordinationRoleRelationError(err)
}
