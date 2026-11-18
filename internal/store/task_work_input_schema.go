package store

// Private B1 candidate slot v28, after the staged v27 investigation schema.
const taskWorkInputSchema = `
CREATE INDEX task_coordination_followup_thread ON task_coordination(group_id,topic_id,task_id);
CREATE TABLE chat_followup_routes (
    source_id TEXT PRIMARY KEY NOT NULL REFERENCES message_command_sources(id),
    trigger_id TEXT NOT NULL UNIQUE,
    assessment_id TEXT NOT NULL UNIQUE REFERENCES chat_assessments(id),
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    reply_message_id TEXT NOT NULL,
    notice_message_id TEXT NOT NULL UNIQUE,
    UNIQUE(source_id,group_id,topic_id),
    FOREIGN KEY(trigger_id,group_id,topic_id) REFERENCES coordination_triggers(id,group_id,topic_id),
    FOREIGN KEY(reply_message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id),
    FOREIGN KEY(notice_message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id)
);
CREATE TABLE task_work_inputs (
    id TEXT PRIMARY KEY NOT NULL CHECK(id<>''),
    source_id TEXT NOT NULL UNIQUE,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    task_revision INTEGER NOT NULL CHECK(typeof(task_revision)='integer' AND task_revision>0),
    mandate_id TEXT NOT NULL,
    assignment_id TEXT NOT NULL,
    worker_member_id TEXT NOT NULL,
    before_revision INTEGER NOT NULL CHECK(typeof(before_revision)='integer' AND before_revision>=0),
    before_constraint TEXT NOT NULL CHECK(length(CAST(before_constraint AS BLOB))<=2048),
    after_revision INTEGER NOT NULL CHECK(typeof(after_revision)='integer' AND after_revision>=0),
    after_constraint TEXT NOT NULL CHECK(length(CAST(after_constraint AS BLOB))<=2048),
    created_at TEXT NOT NULL,
    content TEXT NOT NULL CHECK(length(CAST(content AS BLOB)) BETWEEN 1 AND 8192 AND instr(content,char(0))=0),
    CHECK((before_revision=0)=(before_constraint='') AND (after_revision=0)=(after_constraint='')),
    CHECK((after_revision=before_revision AND after_constraint=before_constraint) OR after_revision=before_revision+1),
    FOREIGN KEY(source_id,group_id,topic_id) REFERENCES chat_followup_routes(source_id,group_id,topic_id),
    FOREIGN KEY(task_id,group_id) REFERENCES tasks(id,group_id),
    FOREIGN KEY(mandate_id,task_id,group_id,topic_id) REFERENCES mandates(id,task_id,group_id,topic_id),
    FOREIGN KEY(assignment_id,task_id,group_id,worker_member_id) REFERENCES coordination_assignments(id,task_id,group_id,worker_member_id)
);
CREATE INDEX task_work_inputs_history ON task_work_inputs(group_id,task_id,created_at DESC,id DESC);
CREATE INDEX task_work_inputs_inbox ON task_work_inputs(assignment_id,task_revision,created_at DESC,id DESC);
CREATE UNIQUE INDEX task_work_inputs_constraints ON task_work_inputs(group_id,task_id,task_revision,after_revision DESC)
    WHERE after_revision>before_revision;
CREATE TABLE task_work_input_consumptions (
    input_id TEXT PRIMARY KEY NOT NULL REFERENCES task_work_inputs(id),
    turn_id TEXT NOT NULL REFERENCES turns(id),
    attempt_id TEXT NOT NULL REFERENCES task_attempts(id),
    consumed_at TEXT NOT NULL
);
CREATE TABLE task_work_input_returns (
    input_id TEXT PRIMARY KEY NOT NULL REFERENCES task_work_inputs(id),
    turn_id TEXT NOT NULL REFERENCES turns(id),
    attempt_id TEXT NOT NULL REFERENCES task_attempts(id),
    member_id TEXT NOT NULL REFERENCES members(id),
    reason TEXT NOT NULL CHECK(length(CAST(reason AS BLOB)) BETWEEN 1 AND 2048 AND instr(reason,char(0))=0),
    question TEXT NOT NULL CHECK(length(CAST(question AS BLOB)) BETWEEN 1 AND 2048),
    notice_message_id TEXT NOT NULL UNIQUE REFERENCES messages(id),
    returned_at TEXT NOT NULL
);
CREATE TRIGGER chat_followup_routes_insert BEFORE INSERT ON chat_followup_routes BEGIN
    SELECT RAISE(ABORT,'跟进路由不可替换') WHERE EXISTS(SELECT 1 FROM chat_followup_routes WHERE
        source_id=NEW.source_id OR trigger_id=NEW.trigger_id OR assessment_id=NEW.assessment_id OR notice_message_id=NEW.notice_message_id);
    SELECT RAISE(ABORT,'跟进来源不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM message_command_sources s JOIN coordination_triggers t ON t.message_id=s.message_id
        JOIN chat_assessments a ON a.id=NEW.assessment_id JOIN chat_candidates c ON c.id=a.candidate_id
        JOIN chat_intake_executions e ON e.candidate_id=c.id AND e.source_id=s.id AND e.source_kind=s.kind
        WHERE s.id=NEW.source_id AND s.kind='local_user_chat' AND s.message_id=NEW.reply_message_id
        AND t.id=NEW.trigger_id AND t.source_kind='local_im' AND c.message_id=s.message_id
        AND c.group_id=NEW.group_id AND c.topic_id=NEW.topic_id AND a.notice_message_id=NEW.notice_message_id
        AND a.decision_id IS NULL);
END;
CREATE TRIGGER chat_followup_routes_no_update BEFORE UPDATE ON chat_followup_routes
BEGIN SELECT RAISE(ABORT,'跟进路由不可修改'); END;
CREATE TRIGGER chat_followup_routes_no_delete BEFORE DELETE ON chat_followup_routes
BEGIN SELECT RAISE(ABORT,'跟进路由不可删除'); END;
CREATE TRIGGER task_work_inputs_insert BEFORE INSERT ON task_work_inputs BEGIN
    SELECT RAISE(ABORT,'工作输入不可替换') WHERE EXISTS(SELECT 1 FROM task_work_inputs WHERE id=NEW.id OR source_id=NEW.source_id)
        OR (NEW.after_revision>NEW.before_revision AND EXISTS(SELECT 1 FROM task_work_inputs
            WHERE group_id=NEW.group_id AND task_id=NEW.task_id AND task_revision=NEW.task_revision
            AND after_revision>before_revision AND after_revision=NEW.after_revision));
    SELECT RAISE(ABORT,'工作输入与原分派不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM coordination_assignments a JOIN coordination_dispatch_requests r ON r.id=a.request_id
        WHERE a.id=NEW.assignment_id AND r.task_revision=NEW.task_revision AND r.mandate_id=NEW.mandate_id AND r.topic_id=NEW.topic_id);
    SELECT RAISE(ABORT,'工作输入与原文或评估不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM chat_followup_routes r JOIN messages m ON m.id=r.reply_message_id
        JOIN chat_assessments a ON a.id=r.assessment_id
        WHERE r.source_id=NEW.source_id AND m.sender_id='user'
        AND m.content=NEW.content
        AND json_extract(a.receipt_json,'$.outcome') IS 'continued'
        AND json_extract(a.receipt_json,'$.task_id') IS NEW.task_id
        AND json_extract(a.receipt_json,'$.mandate_id') IS NEW.mandate_id
        AND json_extract(a.receipt_json,'$.work_input_id') IS NEW.id
        AND (NEW.after_revision=NEW.before_revision OR instr(m.content,NEW.after_constraint)>0));
END;
CREATE TRIGGER task_work_inputs_no_update BEFORE UPDATE ON task_work_inputs
BEGIN SELECT RAISE(ABORT,'工作输入不可修改'); END;
CREATE TRIGGER task_work_inputs_no_delete BEFORE DELETE ON task_work_inputs
BEGIN SELECT RAISE(ABORT,'工作输入不可删除'); END;
CREATE TRIGGER task_work_input_consumptions_insert BEFORE INSERT ON task_work_input_consumptions BEGIN
    SELECT RAISE(ABORT,'工作输入确认不可替换') WHERE EXISTS(SELECT 1 FROM task_work_input_consumptions WHERE input_id=NEW.input_id)
        OR EXISTS(SELECT 1 FROM task_work_input_returns WHERE input_id=NEW.input_id);
    SELECT RAISE(ABORT,'工作输入确认执行身份不匹配') WHERE NOT EXISTS(SELECT 1` + workInputHandlingRelations + `);
END;
CREATE TRIGGER task_work_input_consumptions_no_update BEFORE UPDATE ON task_work_input_consumptions
BEGIN SELECT RAISE(ABORT,'工作输入确认不可修改'); END;
CREATE TRIGGER task_work_input_consumptions_no_delete BEFORE DELETE ON task_work_input_consumptions
BEGIN SELECT RAISE(ABORT,'工作输入确认不可删除'); END;
CREATE TRIGGER task_work_input_returns_insert BEFORE INSERT ON task_work_input_returns BEGIN
    SELECT RAISE(ABORT,'工作输入交回不可替换') WHERE EXISTS(SELECT 1 FROM task_work_input_returns
        WHERE input_id=NEW.input_id OR notice_message_id=NEW.notice_message_id)
        OR EXISTS(SELECT 1 FROM task_work_input_consumptions WHERE input_id=NEW.input_id);
    SELECT RAISE(ABORT,'工作输入交回执行身份不匹配') WHERE NOT EXISTS(SELECT 1` + workInputHandlingRelations + `
        AND i.worker_member_id=NEW.member_id AND EXISTS(SELECT 1 FROM messages n WHERE n.id=NEW.notice_message_id
        AND (n.group_id,n.topic_id,n.sender_id)=(i.group_id,i.topic_id,NEW.member_id)
        AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=n.id)
        AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=n.id)));
END;
CREATE TRIGGER task_work_input_returns_no_update BEFORE UPDATE ON task_work_input_returns
BEGIN SELECT RAISE(ABORT,'工作输入交回不可修改'); END;
CREATE TRIGGER task_work_input_returns_no_delete BEFORE DELETE ON task_work_input_returns
BEGIN SELECT RAISE(ABORT,'工作输入交回不可删除'); END;
`

// The canonical Attempt may be either the original or a typed continuation.
// The command also verifies the exact Assignment and dispatch/continuation.
const workInputHandlingRelations = ` FROM task_work_inputs i JOIN tasks t ON t.id=i.task_id AND t.group_id=i.group_id
 JOIN task_attempts a ON a.id=t.active_attempt_id JOIN turns w ON w.id=a.turn_id
 JOIN task_coordination tc ON tc.task_id=t.id AND tc.group_id=t.group_id
 WHERE i.id=NEW.input_id AND t.status='open' AND t.revision=i.task_revision
 AND tc.topic_id=i.topic_id AND tc.current_mandate_id=i.mandate_id
 AND a.id=NEW.attempt_id AND a.task_id=i.task_id AND a.group_id=i.group_id
 AND a.task_revision=i.task_revision AND a.actor_id=i.worker_member_id
 AND w.id=NEW.turn_id AND w.group_id=i.group_id AND w.member_id=i.worker_member_id
 AND w.state IN ('starting','running') AND w.cancel_requested_at IS NULL AND w.candidate_terminal=''`
