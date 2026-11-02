package store

const coordinationAdmissionSchemaV10 = `
CREATE UNIQUE INDEX coordination_triggers_admission_identity ON coordination_triggers(id, group_id, topic_id);
CREATE TABLE routing_decisions (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    trigger_id TEXT NOT NULL UNIQUE CHECK(trigger_id <> ''),
    group_id TEXT NOT NULL CHECK(group_id <> ''),
    topic_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('create_new','link_existing')),
    actor_id TEXT NOT NULL CHECK(actor_id='user'),
    policy_revision TEXT NOT NULL CHECK(policy_revision='local-user-explicit/v1'),
    reason TEXT NOT NULL,
    task_id TEXT NOT NULL CHECK(task_id <> ''),
    task_revision_at_admission INTEGER NOT NULL
        CHECK(typeof(task_revision_at_admission)='integer' AND task_revision_at_admission > 0),
    mandate_id TEXT NOT NULL UNIQUE CHECK(mandate_id <> ''),
    created_at TEXT NOT NULL,
    UNIQUE(id, mandate_id, trigger_id, task_id, group_id, topic_id, task_revision_at_admission),
    FOREIGN KEY(trigger_id, group_id, topic_id) REFERENCES coordination_triggers(id, group_id, topic_id),
    FOREIGN KEY(task_id, group_id) REFERENCES tasks(id, group_id),
    -- Both directions bind the exact pair; neither half can commit alone or be cross-wired.
    FOREIGN KEY(mandate_id, id, trigger_id, task_id, group_id, topic_id, task_revision_at_admission)
        REFERENCES mandates(id, decision_id, origin_trigger_id, task_id, group_id, topic_id, task_revision_at_admission)
        DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE mandates (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    decision_id TEXT NOT NULL UNIQUE CHECK(decision_id <> ''),
    origin_trigger_id TEXT NOT NULL CHECK(origin_trigger_id <> ''),
    group_id TEXT NOT NULL CHECK(group_id <> ''),
    topic_id TEXT NOT NULL,
    task_id TEXT NOT NULL CHECK(task_id <> ''),
    task_revision_at_admission INTEGER NOT NULL
        CHECK(typeof(task_revision_at_admission)='integer' AND task_revision_at_admission > 0),
    authority_kind TEXT NOT NULL CHECK(authority_kind='local_user_explicit'),
    authorized_by TEXT NOT NULL CHECK(authorized_by='user'),
    strength TEXT NOT NULL CHECK(strength='explicit'),
    policy_revision TEXT NOT NULL CHECK(policy_revision='local-user-explicit/v1'),
    objective TEXT NOT NULL CHECK(trim(objective) <> ''),
    description TEXT NOT NULL,
    acceptance TEXT NOT NULL CHECK(trim(acceptance) <> ''),
    assignee_id TEXT NOT NULL CHECK(assignee_id='' OR trim(assignee_id) <> ''),
    dependency_ids TEXT NOT NULL CHECK(json_valid(dependency_ids) AND json_type(dependency_ids)='array'),
    created_at TEXT NOT NULL,
    UNIQUE(id, decision_id, origin_trigger_id, task_id, group_id, topic_id, task_revision_at_admission),
    UNIQUE(id, task_id, group_id, topic_id),
    -- Revision is historical evidence, never a reference to mutable tasks.revision.
    FOREIGN KEY(decision_id, id, origin_trigger_id, task_id, group_id, topic_id, task_revision_at_admission)
        REFERENCES routing_decisions(id, mandate_id, trigger_id, task_id, group_id, topic_id, task_revision_at_admission)
        DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX mandates_group_task ON mandates(group_id, task_id);
CREATE TABLE task_coordination (
    task_id TEXT PRIMARY KEY NOT NULL CHECK(task_id <> ''),
    group_id TEXT NOT NULL CHECK(group_id <> ''),
    topic_id TEXT NOT NULL,
    current_mandate_id TEXT NOT NULL CHECK(current_mandate_id <> ''),
    authority_generation INTEGER NOT NULL CHECK(typeof(authority_generation)='integer' AND authority_generation > 0),
    FOREIGN KEY(task_id, group_id) REFERENCES tasks(id, group_id),
    FOREIGN KEY(current_mandate_id, task_id, group_id, topic_id) REFERENCES mandates(id, task_id, group_id, topic_id)
);
-- BEFORE INSERT guards cover every UNIQUE key, including REPLACE with recursive_triggers=OFF.
CREATE TRIGGER routing_decisions_insert BEFORE INSERT ON routing_decisions
BEGIN
    SELECT RAISE(ABORT, '路由决定不可替换') WHERE EXISTS(
        SELECT 1 FROM routing_decisions WHERE id=NEW.id OR trigger_id=NEW.trigger_id OR mandate_id=NEW.mandate_id);
END;
CREATE TRIGGER routing_decisions_no_update BEFORE UPDATE ON routing_decisions
BEGIN SELECT RAISE(ABORT, '路由决定不可修改'); END;
CREATE TRIGGER routing_decisions_no_delete BEFORE DELETE ON routing_decisions
BEGIN SELECT RAISE(ABORT, '路由决定不可删除'); END;
CREATE TRIGGER mandates_insert BEFORE INSERT ON mandates
BEGIN
    SELECT RAISE(ABORT, '授权记录不可替换') WHERE EXISTS(
        SELECT 1 FROM mandates WHERE id=NEW.id OR decision_id=NEW.decision_id);
    SELECT RAISE(ABORT, '授权依赖快照无效') WHERE EXISTS(
        SELECT 1 FROM json_each(NEW.dependency_ids) WHERE type<>'text' OR trim(value)='' OR value=NEW.task_id)
        OR EXISTS(SELECT value FROM json_each(NEW.dependency_ids) GROUP BY value HAVING count(*) > 1);
END;
CREATE TRIGGER mandates_no_update BEFORE UPDATE ON mandates
BEGIN SELECT RAISE(ABORT, '授权记录不可修改'); END;
CREATE TRIGGER mandates_no_delete BEFORE DELETE ON mandates
BEGIN SELECT RAISE(ABORT, '授权记录不可删除'); END;
`
