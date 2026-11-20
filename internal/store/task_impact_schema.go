package store

import "slices"

// Derive the slot before historical fixtures truncate the registry.
var taskImpactSchemaVersion = slices.Index(migrations, taskImpactSchema) + 1

const taskImpactSchema = `
CREATE TABLE task_constraint_impacts (
    id TEXT PRIMARY KEY NOT NULL,
    input_id TEXT NOT NULL REFERENCES task_work_inputs(id),
    group_id TEXT NOT NULL,
    source_task_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    task_revision INTEGER NOT NULL CHECK(task_revision>0),
    created_at TEXT NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('constraint_confirmed','work_input_returned','stale_delegation',
        'stale_work_report','stale_submission','stale_pinned_dependency','cancel_requested')),
    ref_id TEXT NOT NULL,
    notice_message_id TEXT UNIQUE REFERENCES messages(id),
    pin_review_id TEXT REFERENCES task_review_decisions(id),
    affected_submission_id TEXT REFERENCES task_submissions(id),
    FOREIGN KEY(task_id,group_id) REFERENCES tasks(id,group_id),
    UNIQUE(input_id,kind,task_id,ref_id),
    CHECK((kind='constraint_confirmed')=(notice_message_id IS NOT NULL)),
    CHECK(kind='stale_pinned_dependency' OR (pin_review_id IS NULL AND affected_submission_id IS NULL))
) STRICT, WITHOUT ROWID;
CREATE INDEX task_constraint_impacts_history ON task_constraint_impacts(group_id,task_id,created_at DESC,id DESC);
CREATE INDEX task_constraint_impacts_downstream ON task_constraint_impacts(group_id,source_task_id,created_at DESC,id DESC)
    WHERE task_id<>source_task_id;
CREATE INDEX task_constraint_impacts_evidence ON task_constraint_impacts(group_id,kind,ref_id);
CREATE INDEX task_artifact_dependencies_source ON task_artifact_dependencies(group_id,dependency_id,dependency_revision,submission_id);
CREATE INDEX task_attempts_turn_scope ON task_attempts(turn_id,group_id,task_id) WHERE turn_id IS NOT NULL;

CREATE TRIGGER task_constraint_impacts_insert BEFORE INSERT ON task_constraint_impacts BEGIN
    SELECT RAISE(ABORT,'约束影响记录不可替换') WHERE EXISTS(SELECT 1 FROM task_constraint_impacts
        WHERE id=NEW.id OR notice_message_id=NEW.notice_message_id OR
        (input_id,kind,task_id,ref_id)=(NEW.input_id,NEW.kind,NEW.task_id,NEW.ref_id));
    SELECT RAISE(ABORT,'约束影响缺少对应的负责人确认或交回') WHERE NOT EXISTS(
        SELECT 1 FROM task_work_inputs i
        LEFT JOIN task_work_input_consumptions c ON c.input_id=i.id
        LEFT JOIN task_work_input_returns h ON h.input_id=i.id
        WHERE (i.id,i.group_id,i.task_id)=(NEW.input_id,NEW.group_id,NEW.source_task_id) AND CASE NEW.kind
        WHEN 'work_input_returned' THEN h.returned_at=NEW.created_at
            AND (NEW.id,NEW.ref_id,NEW.task_id,NEW.task_revision)=(i.id,i.id,i.task_id,i.task_revision)
        ELSE c.consumed_at=NEW.created_at AND i.after_revision>i.before_revision
            AND (NEW.kind IN ('stale_pinned_dependency','cancel_requested') OR
                (NEW.task_id,NEW.task_revision)=(i.task_id,i.task_revision)) END);
    SELECT RAISE(ABORT,'约束影响缺少原话题平台通知') WHERE NEW.kind='constraint_confirmed' AND NOT EXISTS(
        SELECT 1 FROM task_work_inputs i JOIN messages m ON (m.group_id,m.topic_id,m.sender_id)=(i.group_id,i.topic_id,i.worker_member_id)
        JOIN message_log l ON (l.message_id,l.group_id,l.topic_id)=(m.id,m.group_id,m.topic_id)
        WHERE (i.id,i.id,i.id,m.id)=(NEW.input_id,NEW.id,NEW.ref_id,NEW.notice_message_id)
        AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=m.id)
        AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=m.id));
END;
CREATE TRIGGER task_constraint_impacts_no_update BEFORE UPDATE ON task_constraint_impacts
BEGIN SELECT RAISE(ABORT,'约束影响记录不可修改'); END;
CREATE TRIGGER task_constraint_impacts_no_delete BEFORE DELETE ON task_constraint_impacts
BEGIN SELECT RAISE(ABORT,'约束影响记录不可删除'); END;
`
