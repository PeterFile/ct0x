package store

// Append after decisionRequestSchema and v30; B assigns the public slot. Only
// the source-validation trigger is replaced; physical queue and role guards
// from v29/v30 remain in force and no historical rows are rewritten.
const decisionAnswerSchema = `
CREATE TABLE decision_answers (
    id TEXT PRIMARY KEY NOT NULL CHECK(instr(id,'decision-answer:')=1),
    request_id TEXT NOT NULL UNIQUE,
    version INTEGER NOT NULL,
    actor_id TEXT NOT NULL CHECK(actor_id='user'),
    text TEXT NOT NULL CHECK(length(CAST(text AS BLOB)) BETWEEN 1 AND 4096 AND instr(text,char(0))=0),
    option_index INTEGER CHECK(option_index BETWEEN 1 AND 4),
    reply_message_id TEXT NOT NULL UNIQUE REFERENCES messages(id),
    wake_delivery_id TEXT NOT NULL UNIQUE REFERENCES task_work_deliveries(delivery_id) DEFERRABLE INITIALLY DEFERRED,
    created_at TEXT NOT NULL CHECK(length(created_at)=30),
    FOREIGN KEY(request_id,version) REFERENCES decision_request_versions(request_id,version)
) STRICT, WITHOUT ROWID;
CREATE TABLE decision_answer_consumptions (
    answer_id TEXT PRIMARY KEY NOT NULL REFERENCES decision_answers(id),
    turn_id TEXT NOT NULL REFERENCES turns(id),
    attempt_id TEXT NOT NULL REFERENCES task_attempts(id),
    consumed_at TEXT NOT NULL CHECK(length(consumed_at)=30)
) STRICT, WITHOUT ROWID;

CREATE TRIGGER decision_answers_insert BEFORE INSERT ON decision_answers
BEGIN
    SELECT RAISE(ABORT,'已保存的决定不可替换') WHERE EXISTS(SELECT 1 FROM decision_answers WHERE
        id=NEW.id OR request_id=NEW.request_id OR reply_message_id=NEW.reply_message_id OR wake_delivery_id=NEW.wake_delivery_id)
        OR EXISTS(SELECT 1 FROM task_work_inputs WHERE id=NEW.id)
        OR EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=NEW.wake_delivery_id);
    SELECT RAISE(ABORT,'决定与当前待决范围或用户原话不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM decision_requests q JOIN decision_request_versions v ON (v.request_id,v.version)=(q.id,NEW.version)
        JOIN tasks t ON (t.id,t.group_id)=(q.task_id,q.group_id)
        JOIN messages reply ON (reply.id,reply.group_id,reply.topic_id,reply.sender_id,reply.content)=
            (NEW.reply_message_id,q.group_id,q.topic_id,NEW.actor_id,NEW.text)
        JOIN message_log l ON (l.message_id,l.group_id,l.topic_id)=(reply.id,q.group_id,q.topic_id)
        WHERE q.id=NEW.request_id AND q.recipient_id=NEW.actor_id AND q.relay='task_owner'
        AND reply.created_at=NEW.created_at AND l.sequence>q.source_sequence
        AND q.withdrawn_at IS NULL AND (v.expires_at IS NULL OR v.expires_at>NEW.created_at)
        AND v.version=(SELECT max(version) FROM decision_request_versions WHERE request_id=q.id)
        AND (NEW.option_index IS NULL OR NEW.text IS json_extract(v.question_json,'$.choices['||(NEW.option_index-1)||']'))
        AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=reply.id)
        AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=reply.id)
        AND (` + decisionRequestCurrentTask + `));
END;
CREATE TRIGGER decision_answers_no_update BEFORE UPDATE ON decision_answers
BEGIN SELECT RAISE(ABORT,'已保存的决定不可修改'); END;
CREATE TRIGGER decision_answers_no_delete BEFORE DELETE ON decision_answers
BEGIN SELECT RAISE(ABORT,'已保存的决定不可删除'); END;
CREATE TRIGGER decision_answer_versions_closed BEFORE INSERT ON decision_request_versions
WHEN EXISTS(SELECT 1 FROM decision_answers WHERE request_id=NEW.request_id)
BEGIN SELECT RAISE(ABORT,'已答复的待决事项不可更新'); END;

CREATE TRIGGER decision_answer_consumptions_insert BEFORE INSERT ON decision_answer_consumptions
BEGIN
    SELECT RAISE(ABORT,'用户决定处理回执不可替换') WHERE EXISTS(SELECT 1 FROM decision_answer_consumptions WHERE answer_id=NEW.answer_id);
    SELECT RAISE(ABORT,'用户决定处理回执不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM decision_answers ans JOIN decision_requests q ON q.id=ans.request_id
        JOIN decision_request_versions v ON (v.request_id,v.version)=(q.id,ans.version)
        JOIN tasks t ON (t.id,t.group_id)=(q.task_id,q.group_id)
        JOIN task_work_continuations c ON (c.delivery_id,c.turn_id,c.attempt_id)=(ans.wake_delivery_id,NEW.turn_id,NEW.attempt_id)
        JOIN turns w ON (w.id,w.group_id,w.member_id,w.delivery_id)=(c.turn_id,q.group_id,q.owner_member_id,c.delivery_id)
        WHERE ans.id=NEW.answer_id AND NEW.consumed_at>=ans.created_at AND t.active_attempt_id=c.attempt_id
        AND w.state IN ('starting','running') AND w.cancel_requested_at IS NULL AND w.candidate_terminal=''
        AND q.withdrawn_at IS NULL AND (v.expires_at IS NULL OR v.expires_at>NEW.consumed_at)
        AND v.version=(SELECT max(version) FROM decision_request_versions WHERE request_id=q.id)
        AND (` + decisionRequestCurrentTask + `));
END;
CREATE TRIGGER decision_answer_consumptions_no_update BEFORE UPDATE ON decision_answer_consumptions
BEGIN SELECT RAISE(ABORT,'用户决定处理回执不可修改'); END;
CREATE TRIGGER decision_answer_consumptions_no_delete BEFORE DELETE ON decision_answer_consumptions
BEGIN SELECT RAISE(ABORT,'用户决定处理回执不可删除'); END;

DROP TRIGGER task_work_delivery_source_insert;
CREATE TRIGGER task_work_delivery_source_insert BEFORE INSERT ON task_work_deliveries
BEGIN
    SELECT RAISE(ABORT, '工作投递缺少已验证的来源') WHERE NOT CASE NEW.source_kind
    WHEN 'followup' THEN NOT (EXISTS(SELECT 1 FROM task_work_delegations WHERE delivery_id=NEW.delivery_id)
        OR EXISTS(SELECT 1 FROM task_work_reports WHERE delivery_id=NEW.delivery_id)) AND CASE
        WHEN instr(NEW.source_id,'decision-answer:')=1 THEN EXISTS(SELECT 1
            FROM decision_answers ans JOIN decision_requests p ON p.id=ans.request_id
            JOIN coordination_assignments ca ON ca.id=p.assignment_id
            JOIN coordination_dispatch_requests r ON r.id=ca.request_id
            WHERE (ans.id,ans.wake_delivery_id)=(NEW.source_id,NEW.delivery_id)
            AND NEW.member_id=p.owner_member_id AND NEW.sender_member_id=r.coordinator_member_id
            AND ` + taskWorkSourceParentMatch + `)
        ELSE EXISTS(
            SELECT 1 FROM task_work_inputs i JOIN chat_followup_routes f ON f.source_id=i.source_id
            JOIN chat_assessments a ON a.id=f.assessment_id JOIN chat_candidates c ON c.id=a.candidate_id
            WHERE (i.id,i.assignment_id,i.task_id,i.task_revision,i.mandate_id,i.worker_member_id,i.group_id,i.topic_id)=
               (NEW.source_id,NEW.assignment_id,NEW.task_id,NEW.task_revision,NEW.mandate_id,NEW.owner_member_id,NEW.group_id,NEW.topic_id)
            AND NEW.sender_member_id=c.member_id AND json_type(a.receipt_json,'$.work_input_pending') IS NULL) END
    WHEN 'delegation' THEN NOT EXISTS(SELECT 1 FROM task_work_reports WHERE delivery_id=NEW.delivery_id) AND EXISTS(
        SELECT 1 FROM task_work_delegations p WHERE (p.delivery_id,p.id,p.member_id,p.owner_member_id)=
            (NEW.delivery_id,NEW.source_id,NEW.member_id,NEW.sender_member_id) AND ` + taskWorkSourceParentMatch + `)
    WHEN 'report' THEN NOT EXISTS(SELECT 1 FROM task_work_delegations WHERE delivery_id=NEW.delivery_id) AND EXISTS(
        SELECT 1 FROM task_work_reports r JOIN task_work_delegations p ON p.id=r.delegation_id
        WHERE (r.delivery_id,r.id,p.owner_member_id,r.worker_member_id)=
            (NEW.delivery_id,NEW.source_id,NEW.member_id,NEW.sender_member_id) AND ` + taskWorkSourceParentMatch + `)
    ELSE 0 END;
END;
`
