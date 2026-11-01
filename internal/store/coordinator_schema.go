package store

const coordinatorSchemaV11 = `
CREATE TABLE group_coordinator_binding_revisions (
    group_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK(typeof(revision)='integer' AND revision > 0),
    member_id TEXT NOT NULL,
    PRIMARY KEY(group_id, revision),
    FOREIGN KEY(group_id, member_id) REFERENCES members(group_id, id)
) WITHOUT ROWID;
CREATE TABLE group_dispatch_policy_revisions (
    group_id TEXT NOT NULL REFERENCES groups(id),
    revision INTEGER NOT NULL CHECK(typeof(revision)='integer' AND revision > 0),
    allowed_worker_ids TEXT NOT NULL CHECK(typeof(allowed_worker_ids)='text'
        AND json_valid(allowed_worker_ids) AND json_type(allowed_worker_ids)='array'),
    PRIMARY KEY(group_id, revision)
) WITHOUT ROWID;
CREATE TABLE group_coordinator_bindings (
    group_id TEXT PRIMARY KEY NOT NULL,
    revision INTEGER NOT NULL CHECK(typeof(revision)='integer' AND revision > 0),
    FOREIGN KEY(group_id, revision) REFERENCES group_coordinator_binding_revisions(group_id, revision)
) WITHOUT ROWID;
CREATE TABLE group_dispatch_policies (
    group_id TEXT PRIMARY KEY NOT NULL,
    revision INTEGER NOT NULL CHECK(typeof(revision)='integer' AND revision > 0),
    FOREIGN KEY(group_id, revision) REFERENCES group_dispatch_policy_revisions(group_id, revision)
) WITHOUT ROWID;

-- Guard every unique key before REPLACE deletes, even without recursive triggers.
CREATE TRIGGER group_coordinator_binding_revisions_insert BEFORE INSERT ON group_coordinator_binding_revisions
WHEN EXISTS(SELECT 1 FROM group_coordinator_binding_revisions
    WHERE group_id=NEW.group_id AND revision=NEW.revision)
BEGIN SELECT RAISE(ABORT, '协调员绑定版本不可替换'); END;
CREATE TRIGGER group_coordinator_binding_revisions_no_update BEFORE UPDATE ON group_coordinator_binding_revisions
BEGIN SELECT RAISE(ABORT, '协调员绑定版本不可修改'); END;
CREATE TRIGGER group_coordinator_binding_revisions_no_delete BEFORE DELETE ON group_coordinator_binding_revisions
BEGIN SELECT RAISE(ABORT, '协调员绑定版本不可删除'); END;
CREATE TRIGGER group_dispatch_policy_revisions_insert BEFORE INSERT ON group_dispatch_policy_revisions
BEGIN
    SELECT RAISE(ABORT, '分派策略版本不可替换') WHERE EXISTS(
        SELECT 1 FROM group_dispatch_policy_revisions
        WHERE group_id=NEW.group_id AND revision=NEW.revision);
    SELECT RAISE(ABORT, '允许执行成员列表无效') WHERE EXISTS(
        SELECT 1 FROM json_each(NEW.allowed_worker_ids) WHERE type<>'text' OR trim(value)='')
        OR EXISTS(SELECT value FROM json_each(NEW.allowed_worker_ids) GROUP BY value HAVING count(*) > 1);
END;
CREATE TRIGGER group_dispatch_policy_revisions_no_update BEFORE UPDATE ON group_dispatch_policy_revisions
BEGIN SELECT RAISE(ABORT, '分派策略版本不可修改'); END;
CREATE TRIGGER group_dispatch_policy_revisions_no_delete BEFORE DELETE ON group_dispatch_policy_revisions
BEGIN SELECT RAISE(ABORT, '分派策略版本不可删除'); END;

-- Only current projections enforce active membership; historical evidence remains readable.
CREATE TRIGGER group_coordinator_bindings_insert BEFORE INSERT ON group_coordinator_bindings
BEGIN
    SELECT RAISE(ABORT, '协调员绑定初始版本冲突') WHERE NEW.revision<>1
        OR EXISTS(SELECT 1 FROM group_coordinator_bindings WHERE group_id=NEW.group_id);
    SELECT RAISE(ABORT, '协调员必须为同群活跃成员且不能是当前执行成员') WHERE NOT EXISTS(
        SELECT 1 FROM group_coordinator_binding_revisions b
        JOIN members m ON m.group_id=b.group_id AND m.id=b.member_id
        WHERE b.group_id=NEW.group_id AND b.revision=NEW.revision AND m.status='active'
        AND NOT EXISTS(
            SELECT 1 FROM group_dispatch_policies p
            JOIN group_dispatch_policy_revisions r USING(group_id,revision)
            JOIN json_each(r.allowed_worker_ids) w ON w.value=b.member_id
            WHERE p.group_id=b.group_id));
END;
CREATE TRIGGER group_coordinator_bindings_update BEFORE UPDATE ON group_coordinator_bindings
BEGIN
    SELECT RAISE(ABORT, '协调员绑定版本必须逐次递增')
        WHERE NEW.group_id<>OLD.group_id OR NEW.revision<>OLD.revision+1;
    SELECT RAISE(ABORT, '协调员必须为同群活跃成员且不能是当前执行成员') WHERE NOT EXISTS(
        SELECT 1 FROM group_coordinator_binding_revisions b
        JOIN members m ON m.group_id=b.group_id AND m.id=b.member_id
        WHERE b.group_id=NEW.group_id AND b.revision=NEW.revision AND m.status='active'
        AND NOT EXISTS(
            SELECT 1 FROM group_dispatch_policies p
            JOIN group_dispatch_policy_revisions r USING(group_id,revision)
            JOIN json_each(r.allowed_worker_ids) w ON w.value=b.member_id
            WHERE p.group_id=b.group_id));
END;
CREATE TRIGGER group_coordinator_bindings_no_delete BEFORE DELETE ON group_coordinator_bindings
BEGIN SELECT RAISE(ABORT, '当前协调员绑定不可删除'); END;
CREATE TRIGGER group_dispatch_policies_insert BEFORE INSERT ON group_dispatch_policies
BEGIN
    SELECT RAISE(ABORT, '分派策略初始版本冲突') WHERE NEW.revision<>1
        OR EXISTS(SELECT 1 FROM group_dispatch_policies WHERE group_id=NEW.group_id);
    SELECT RAISE(ABORT, '执行成员必须同群活跃且不能是当前协调员') WHERE NOT EXISTS(
        SELECT 1 FROM group_dispatch_policy_revisions p WHERE p.group_id=NEW.group_id AND p.revision=NEW.revision
        AND NOT EXISTS(
            SELECT 1 FROM json_each(p.allowed_worker_ids) w
            WHERE NOT EXISTS(
                SELECT 1 FROM members m WHERE m.group_id=p.group_id AND m.id=w.value AND m.status='active')
            OR EXISTS(
                SELECT 1 FROM group_coordinator_bindings b
                JOIN group_coordinator_binding_revisions r USING(group_id,revision)
                WHERE b.group_id=p.group_id AND r.member_id=w.value)));
END;
CREATE TRIGGER group_dispatch_policies_update BEFORE UPDATE ON group_dispatch_policies
BEGIN
    SELECT RAISE(ABORT, '分派策略版本必须逐次递增')
        WHERE NEW.group_id<>OLD.group_id OR NEW.revision<>OLD.revision+1;
    SELECT RAISE(ABORT, '执行成员必须同群活跃且不能是当前协调员') WHERE NOT EXISTS(
        SELECT 1 FROM group_dispatch_policy_revisions p WHERE p.group_id=NEW.group_id AND p.revision=NEW.revision
        AND NOT EXISTS(
            SELECT 1 FROM json_each(p.allowed_worker_ids) w
            WHERE NOT EXISTS(
                SELECT 1 FROM members m WHERE m.group_id=p.group_id AND m.id=w.value AND m.status='active')
            OR EXISTS(
                SELECT 1 FROM group_coordinator_bindings b
                JOIN group_coordinator_binding_revisions r USING(group_id,revision)
                WHERE b.group_id=p.group_id AND r.member_id=w.value)));
END;
CREATE TRIGGER group_dispatch_policies_no_delete BEFORE DELETE ON group_dispatch_policies
BEGIN SELECT RAISE(ABORT, '当前分派策略不可删除'); END;
`
