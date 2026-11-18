package store

// Private B1 candidate slot v29. No legacy row is backfilled.
const taskWorkDeliverySchema = `
CREATE TABLE task_work_deliveries (
    delivery_id TEXT PRIMARY KEY NOT NULL,
    source_kind TEXT NOT NULL CHECK(source_kind IN ('followup','report','delegation','handback')),
    source_id TEXT NOT NULL CHECK(length(CAST(source_id AS BLOB)) BETWEEN 1 AND 4096 AND trim(source_id)=source_id),
    assignment_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    task_revision INTEGER NOT NULL CHECK(task_revision>0),
    mandate_id TEXT NOT NULL,
    authority_generation INTEGER NOT NULL CHECK(authority_generation>0),
    owner_member_id TEXT NOT NULL,
    member_id TEXT NOT NULL,
    sender_member_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    message_id TEXT NOT NULL UNIQUE,
    UNIQUE(source_kind,source_id),
    CHECK((source_kind IN ('followup','report')) = (member_id=owner_member_id)),
    FOREIGN KEY(assignment_id,task_id,group_id,owner_member_id)
        REFERENCES coordination_assignments(id,task_id,group_id,worker_member_id),
    FOREIGN KEY(mandate_id,task_id,group_id,topic_id) REFERENCES mandates(id,task_id,group_id,topic_id),
    FOREIGN KEY(group_id,member_id) REFERENCES members(group_id,id),
    FOREIGN KEY(group_id,sender_member_id) REFERENCES members(group_id,id),
    FOREIGN KEY(message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id),
    FOREIGN KEY(conversation_id,group_id,member_id,topic_id) REFERENCES conversations(id,group_id,member_id,topic_id),
    FOREIGN KEY(delivery_id,group_id,conversation_id,member_id) REFERENCES deliveries(id,group_id,conversation_id,recipient_id)
) STRICT;
CREATE INDEX task_work_deliveries_task ON task_work_deliveries(group_id,task_id);
CREATE TABLE task_work_continuations (
    delivery_id TEXT PRIMARY KEY NOT NULL REFERENCES task_work_deliveries(delivery_id),
    turn_id TEXT NOT NULL UNIQUE REFERENCES turns(id),
    attempt_id TEXT NOT NULL UNIQUE REFERENCES task_attempts(id)
) STRICT;

-- Explicit guards cover REPLACE even with recursive_triggers disabled.
CREATE TRIGGER task_work_deliveries_insert BEFORE INSERT ON task_work_deliveries
BEGIN
    SELECT RAISE(ABORT, '工作投递不可替换') WHERE EXISTS(SELECT 1 FROM task_work_deliveries
        WHERE delivery_id=NEW.delivery_id OR message_id=NEW.message_id OR (source_kind,source_id)=(NEW.source_kind,NEW.source_id));
    SELECT RAISE(ABORT, '工作投递来源不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM coordination_assignments a JOIN coordination_dispatch_requests r ON r.id=a.request_id
        JOIN deliveries d ON d.id=NEW.delivery_id JOIN messages m ON m.id=NEW.message_id
        WHERE (a.id,a.task_id,a.group_id,a.worker_member_id)=(NEW.assignment_id,NEW.task_id,NEW.group_id,NEW.owner_member_id)
        AND (r.task_id,r.group_id,r.topic_id,r.task_revision,r.mandate_id,r.authority_generation)=
            (NEW.task_id,NEW.group_id,NEW.topic_id,NEW.task_revision,NEW.mandate_id,NEW.authority_generation)
        AND (NEW.source_kind<>'handback' OR NEW.member_id=r.coordinator_member_id)
        AND (NEW.source_kind<>'delegation' OR NEW.member_id<>r.coordinator_member_id)
        AND (d.message_id,d.group_id,d.topic_id,d.conversation_id,d.recipient_id)=
            (NEW.message_id,NEW.group_id,NEW.topic_id,NEW.conversation_id,NEW.member_id)
        AND d.state='queued' AND d.turn_id='' AND m.sender_id=NEW.sender_member_id
        AND NOT EXISTS(SELECT 1 FROM turns WHERE delivery_id=d.id)
        AND NOT EXISTS(SELECT 1 FROM deliveries other WHERE other.message_id=m.id AND other.id<>d.id)
        AND NOT EXISTS(SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=d.id OR message_id=m.id)
        AND NOT EXISTS(SELECT 1 FROM coordination_assignment_dispatch WHERE delivery_id=d.id OR message_id=m.id)
        AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=m.id));
END;
-- Future source kinds are reserved, not enabled without their own immutable proof.
CREATE TRIGGER task_work_delivery_source_insert BEFORE INSERT ON task_work_deliveries
WHEN NEW.source_kind<>'followup' OR NOT EXISTS(
    SELECT 1 FROM task_work_inputs i JOIN chat_followup_routes f ON f.source_id=i.source_id
    JOIN chat_assessments a ON a.id=f.assessment_id JOIN chat_candidates c ON c.id=a.candidate_id
    WHERE (i.id,i.assignment_id,i.task_id,i.task_revision,i.mandate_id,i.worker_member_id,i.group_id,i.topic_id)=
       (NEW.source_id,NEW.assignment_id,NEW.task_id,NEW.task_revision,NEW.mandate_id,NEW.owner_member_id,NEW.group_id,NEW.topic_id)
    AND NEW.sender_member_id=c.member_id AND json_type(a.receipt_json,'$.work_input_pending') IS NULL)
BEGIN SELECT RAISE(ABORT, '工作投递缺少已验证的来源'); END;
CREATE TRIGGER task_work_deliveries_no_update BEFORE UPDATE ON task_work_deliveries
BEGIN SELECT RAISE(ABORT, '工作投递不可修改'); END;
CREATE TRIGGER task_work_deliveries_no_delete BEFORE DELETE ON task_work_deliveries
BEGIN SELECT RAISE(ABORT, '工作投递不可删除'); END;

CREATE TRIGGER task_work_continuations_insert BEFORE INSERT ON task_work_continuations
BEGIN
    SELECT RAISE(ABORT, '工作续办尝试不可替换') WHERE EXISTS(SELECT 1 FROM task_work_continuations
        WHERE delivery_id=NEW.delivery_id OR turn_id=NEW.turn_id OR attempt_id=NEW.attempt_id);
    SELECT RAISE(ABORT, '工作续办尝试关联无效') WHERE NOT EXISTS(
        SELECT 1` + taskWorkContinuationRelations + `
        WHERE w.delivery_id=NEW.delivery_id AND t.id=NEW.turn_id AND a.id=NEW.attempt_id
        AND t.state IN ('starting','running') AND t.cancel_requested_at IS NULL AND t.candidate_terminal=''
        AND EXISTS(SELECT 1 FROM tasks WHERE id=w.task_id AND group_id=w.group_id
            AND revision=w.task_revision AND status='open' AND active_attempt_id=a.id)
        AND EXISTS(SELECT 1 FROM coordination_assignment_attempts WHERE assignment_id=w.assignment_id));
END;
CREATE TRIGGER task_work_continuations_no_update BEFORE UPDATE ON task_work_continuations
BEGIN SELECT RAISE(ABORT, '工作续办尝试不可修改'); END;
CREATE TRIGGER task_work_continuations_no_delete BEFORE DELETE ON task_work_continuations
BEGIN SELECT RAISE(ABORT, '工作续办尝试不可删除'); END;
`

// At insert the claimed Turn already points back to this exact, owner-only delivery.
const taskWorkContinuationRelations = ` FROM task_work_deliveries w JOIN deliveries d ON d.id=w.delivery_id
 JOIN turns t ON t.id=d.turn_id JOIN task_attempts a ON a.turn_id=t.id
 AND (a.task_id,a.group_id,a.task_revision,a.actor_id)=(w.task_id,w.group_id,w.task_revision,w.owner_member_id)
 AND w.source_kind IN ('followup','report') AND w.member_id=w.owner_member_id
 AND (d.message_id,d.group_id,d.topic_id,d.conversation_id,d.recipient_id)=
     (w.message_id,w.group_id,w.topic_id,w.conversation_id,w.member_id)
 AND (t.delivery_id,t.group_id,t.conversation_id,t.member_id)=(w.delivery_id,w.group_id,w.conversation_id,w.member_id)`
