package store

const coordinationAssignmentSchemaV13 = `
CREATE TABLE coordination_assignments (
    id TEXT PRIMARY KEY NOT NULL CHECK(typeof(id)='text' AND id<>'' AND trim(id)=id),
    request_id TEXT NOT NULL UNIQUE CHECK(typeof(request_id)='text' AND request_id<>'' AND trim(request_id)=request_id),
    -- Reserved provenance only: N3a1 has no decision table or decision authority.
    decision_id TEXT NOT NULL UNIQUE CHECK(typeof(decision_id)='text' AND decision_id<>'' AND trim(decision_id)=decision_id),
    task_id TEXT NOT NULL UNIQUE CHECK(typeof(task_id)='text' AND task_id<>'' AND trim(task_id)=task_id),
    group_id TEXT NOT NULL CHECK(typeof(group_id)='text' AND group_id<>'' AND trim(group_id)=group_id),
    worker_member_id TEXT NOT NULL CHECK(typeof(worker_member_id)='text' AND worker_member_id<>'' AND trim(worker_member_id)=worker_member_id),
    created_at TEXT NOT NULL CHECK(typeof(created_at)='text' AND length(CAST(created_at AS BLOB))=30
        AND created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z'
        AND substr(created_at,1,4) BETWEEN '0001' AND '9999'
        AND substr(created_at,12,2) BETWEEN '00' AND '23'
        AND substr(created_at,15,2) BETWEEN '00' AND '59' AND substr(created_at,18,2) BETWEEN '00' AND '59'
        AND date(substr(created_at,1,10),'+0 days') IS substr(created_at,1,10)),
    UNIQUE(id,task_id,group_id,worker_member_id),
    FOREIGN KEY(request_id,task_id,group_id) REFERENCES coordination_dispatch_requests(id,task_id,group_id),
    FOREIGN KEY(group_id,worker_member_id) REFERENCES members(group_id,id),
    -- Assignment-first is required; an intent cannot commit independently.
    FOREIGN KEY(id,task_id,group_id,worker_member_id)
        REFERENCES coordination_assignment_dispatch(assignment_id,task_id,group_id,worker_member_id)
        DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE coordination_assignment_dispatch (
    assignment_id TEXT PRIMARY KEY NOT NULL CHECK(typeof(assignment_id)='text' AND assignment_id<>'' AND trim(assignment_id)=assignment_id),
    task_id TEXT NOT NULL CHECK(typeof(task_id)='text' AND task_id<>'' AND trim(task_id)=task_id),
    group_id TEXT NOT NULL CHECK(typeof(group_id)='text' AND group_id<>'' AND trim(group_id)=group_id),
    worker_member_id TEXT NOT NULL CHECK(typeof(worker_member_id)='text' AND worker_member_id<>'' AND trim(worker_member_id)=worker_member_id),
    generation INTEGER NOT NULL CHECK(typeof(generation)='integer' AND generation>0),
    status TEXT NOT NULL CHECK(status IN ('pending','enqueued','held')),
    attempt_count INTEGER NOT NULL CHECK(typeof(attempt_count)='integer' AND attempt_count>=0),
    next_attempt_at TEXT NOT NULL CHECK(typeof(next_attempt_at)='text' AND length(CAST(next_attempt_at AS BLOB))=30
        AND next_attempt_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z'
        AND substr(next_attempt_at,1,4) BETWEEN '0001' AND '9999'
        AND substr(next_attempt_at,12,2) BETWEEN '00' AND '23'
        AND substr(next_attempt_at,15,2) BETWEEN '00' AND '59' AND substr(next_attempt_at,18,2) BETWEEN '00' AND '59'
        AND date(substr(next_attempt_at,1,10),'+0 days') IS substr(next_attempt_at,1,10)),
    updated_at TEXT NOT NULL CHECK(typeof(updated_at)='text' AND length(CAST(updated_at AS BLOB))=30
        AND updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z'
        AND substr(updated_at,1,4) BETWEEN '0001' AND '9999'
        AND substr(updated_at,12,2) BETWEEN '00' AND '23'
        AND substr(updated_at,15,2) BETWEEN '00' AND '59' AND substr(updated_at,18,2) BETWEEN '00' AND '59'
        AND date(substr(updated_at,1,10),'+0 days') IS substr(updated_at,1,10)),
    last_error_code TEXT NOT NULL CHECK(typeof(last_error_code)='text'),
    held_reason TEXT NOT NULL CHECK(typeof(held_reason)='text'),
    message_id TEXT UNIQUE CHECK(message_id IS NULL OR
        (typeof(message_id)='text' AND message_id<>'' AND trim(message_id)=message_id)),
    delivery_id TEXT UNIQUE CHECK(delivery_id IS NULL OR
        (typeof(delivery_id)='text' AND delivery_id<>'' AND trim(delivery_id)=delivery_id)),
    conversation_id TEXT CHECK(conversation_id IS NULL OR
        (typeof(conversation_id)='text' AND conversation_id<>'' AND trim(conversation_id)=conversation_id)),
    delivered_member_id TEXT CHECK(delivered_member_id IS NULL OR
        (typeof(delivered_member_id)='text' AND delivered_member_id<>'' AND trim(delivered_member_id)=delivered_member_id)),
    materialized_from_generation INTEGER CHECK(materialized_from_generation IS NULL OR
        (typeof(materialized_from_generation)='integer' AND materialized_from_generation>0
        AND materialized_from_generation<generation)),
    materialized_at TEXT CHECK(materialized_at IS NULL OR
        (typeof(materialized_at)='text' AND length(CAST(materialized_at AS BLOB))=30
        AND materialized_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z'
        AND substr(materialized_at,1,4) BETWEEN '0001' AND '9999'
        AND substr(materialized_at,12,2) BETWEEN '00' AND '23'
        AND substr(materialized_at,15,2) BETWEEN '00' AND '59' AND substr(materialized_at,18,2) BETWEEN '00' AND '59'
        AND date(substr(materialized_at,1,10),'+0 days') IS substr(materialized_at,1,10))),
    UNIQUE(assignment_id,task_id,group_id,worker_member_id),
    FOREIGN KEY(assignment_id,task_id,group_id,worker_member_id)
        REFERENCES coordination_assignments(id,task_id,group_id,worker_member_id),
    FOREIGN KEY(delivery_id,group_id,conversation_id,delivered_member_id)
        REFERENCES deliveries(id,group_id,conversation_id,recipient_id),
    CHECK((message_id IS NULL AND delivery_id IS NULL AND conversation_id IS NULL AND delivered_member_id IS NULL
        AND materialized_from_generation IS NULL AND materialized_at IS NULL) OR
        (message_id IS NOT NULL AND delivery_id IS NOT NULL AND conversation_id IS NOT NULL AND delivered_member_id IS NOT NULL
        AND materialized_from_generation IS NOT NULL AND materialized_at IS NOT NULL)),
    CHECK((status='pending' AND message_id IS NULL AND held_reason=''
        AND ((attempt_count=0 AND last_error_code='') OR
            (attempt_count>0 AND last_error_code IN ('sqlite_busy','sqlite_locked')))) OR
        (status='enqueued' AND message_id IS NOT NULL AND last_error_code='' AND held_reason='') OR
        (status='held' AND last_error_code='' AND held_reason IN
            ('task_stale','authority_stale','binding_stale','policy_stale','coordinator_inactive',
             'worker_inactive','payload_too_large','payload_not_encodable')))
);
CREATE INDEX coordination_assignment_pending_due ON coordination_assignment_dispatch(next_attempt_at,assignment_id)
    WHERE status='pending';

CREATE TABLE coordination_assignment_attempts (
    assignment_id TEXT PRIMARY KEY NOT NULL CHECK(typeof(assignment_id)='text' AND assignment_id<>'' AND trim(assignment_id)=assignment_id),
    group_id TEXT NOT NULL CHECK(typeof(group_id)='text' AND group_id<>'' AND trim(group_id)=group_id),
    task_id TEXT NOT NULL CHECK(typeof(task_id)='text' AND task_id<>'' AND trim(task_id)=task_id),
    worker_member_id TEXT NOT NULL CHECK(typeof(worker_member_id)='text' AND worker_member_id<>'' AND trim(worker_member_id)=worker_member_id),
    turn_id TEXT NOT NULL UNIQUE CHECK(typeof(turn_id)='text' AND turn_id<>'' AND trim(turn_id)=turn_id),
    attempt_id TEXT NOT NULL UNIQUE CHECK(typeof(attempt_id)='text' AND attempt_id<>'' AND trim(attempt_id)=attempt_id),
    task_revision INTEGER NOT NULL CHECK(typeof(task_revision)='integer' AND task_revision>0),
    created_at TEXT NOT NULL CHECK(typeof(created_at)='text' AND length(CAST(created_at AS BLOB))=30
        AND created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z'
        AND substr(created_at,1,4) BETWEEN '0001' AND '9999'
        AND substr(created_at,12,2) BETWEEN '00' AND '23'
        AND substr(created_at,15,2) BETWEEN '00' AND '59' AND substr(created_at,18,2) BETWEEN '00' AND '59'
        AND date(substr(created_at,1,10),'+0 days') IS substr(created_at,1,10)),
    FOREIGN KEY(assignment_id,task_id,group_id,worker_member_id)
        REFERENCES coordination_assignments(id,task_id,group_id,worker_member_id),
    FOREIGN KEY(turn_id,group_id,worker_member_id) REFERENCES turns(id,group_id,member_id),
    FOREIGN KEY(attempt_id,task_id,group_id,task_revision,worker_member_id)
        REFERENCES task_attempts(id,task_id,group_id,task_revision,actor_id)
);

-- Dropping this block alone is NOT N3 enablement: decision/resolution and parent fences are future gates.
CREATE TRIGGER coordination_assignments_disabled BEFORE INSERT ON coordination_assignments
BEGIN SELECT RAISE(ABORT, '工作分配尚未启用'); END;
CREATE TRIGGER coordination_assignments_insert BEFORE INSERT ON coordination_assignments
BEGIN
    -- Covers every UNIQUE victim, including the composite identity via its id.
    SELECT RAISE(ABORT, '工作分配不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_assignments WHERE id=NEW.id OR request_id=NEW.request_id
        OR decision_id=NEW.decision_id OR task_id=NEW.task_id);
END;
CREATE TRIGGER coordination_assignments_no_update BEFORE UPDATE ON coordination_assignments
BEGIN SELECT RAISE(ABORT, '工作分配不可修改'); END;
CREATE TRIGGER coordination_assignments_no_delete BEFORE DELETE ON coordination_assignments
BEGIN SELECT RAISE(ABORT, '工作分配不可删除'); END;

CREATE TRIGGER coordination_assignment_dispatch_insert BEFORE INSERT ON coordination_assignment_dispatch
BEGIN
    SELECT RAISE(ABORT, '工作分配状态不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_assignment_dispatch WHERE assignment_id=NEW.assignment_id
        OR (NEW.message_id IS NOT NULL AND message_id=NEW.message_id)
        OR (NEW.delivery_id IS NOT NULL AND delivery_id=NEW.delivery_id));
    SELECT RAISE(ABORT, '工作分配初始状态无效') WHERE NEW.status IS NOT 'pending' OR NEW.generation IS NOT 1
        OR NEW.attempt_count IS NOT 0 OR NEW.last_error_code IS NOT '' OR NEW.held_reason IS NOT ''
        OR NEW.message_id IS NOT NULL OR NEW.delivery_id IS NOT NULL OR NEW.conversation_id IS NOT NULL
        OR NEW.delivered_member_id IS NOT NULL OR NEW.materialized_from_generation IS NOT NULL OR NEW.materialized_at IS NOT NULL;
    SELECT RAISE(ABORT, '工作分配初始时间无效') WHERE EXISTS(
        SELECT 1 FROM coordination_assignments WHERE id=NEW.assignment_id
        AND (NEW.next_attempt_at IS NOT created_at OR NEW.updated_at IS NOT created_at));
END;
CREATE TRIGGER coordination_assignment_dispatch_update BEFORE UPDATE ON coordination_assignment_dispatch
BEGIN
    SELECT RAISE(ABORT, '工作分配状态不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_assignment_dispatch WHERE assignment_id<>OLD.assignment_id AND
        (assignment_id=NEW.assignment_id OR (NEW.message_id IS NOT NULL AND message_id=NEW.message_id)
        OR (NEW.delivery_id IS NOT NULL AND delivery_id=NEW.delivery_id)));
    -- Enqueued/held freeze every column, including all six receipt fields and no-op updates.
    SELECT RAISE(ABORT, '工作分配身份、冻结状态或世代无效') WHERE OLD.status<>'pending'
        OR NEW.assignment_id IS NOT OLD.assignment_id OR NEW.task_id IS NOT OLD.task_id OR NEW.group_id IS NOT OLD.group_id
        OR NEW.worker_member_id IS NOT OLD.worker_member_id
        OR OLD.generation=9223372036854775807 OR NEW.generation IS NOT OLD.generation+1;
    -- Whole-second arithmetic preserves the original nanoseconds and checks calendar overflow.
    SELECT RAISE(ABORT, '工作分配退避状态无效') WHERE NEW.status='pending' AND
        (OLD.attempt_count=9223372036854775807 OR NEW.attempt_count IS NOT OLD.attempt_count+1
        OR NEW.next_attempt_at IS NOT (strftime('%Y-%m-%dT%H:%M:%S',substr(NEW.updated_at,1,19)||'Z',
            CASE OLD.attempt_count WHEN 0 THEN '+2 seconds' WHEN 1 THEN '+4 seconds' WHEN 2 THEN '+8 seconds'
                WHEN 3 THEN '+16 seconds' WHEN 4 THEN '+32 seconds' ELSE '+60 seconds' END)||substr(NEW.updated_at,20)));
    SELECT RAISE(ABORT, '工作分配完成状态无效') WHERE NEW.status IN ('held','enqueued') AND
        (NEW.attempt_count IS NOT OLD.attempt_count OR NEW.next_attempt_at IS NOT OLD.next_attempt_at);
    SELECT RAISE(ABORT, '暂停工作分配不能生成回执') WHERE NEW.status='held' AND NEW.message_id IS NOT NULL;
    SELECT RAISE(ABORT, '工作分配回执世代或时间无效') WHERE NEW.status='enqueued' AND
        (NEW.materialized_from_generation IS NOT OLD.generation OR NEW.materialized_at IS NOT NEW.updated_at);
    SELECT RAISE(ABORT, '工作分配回执关联无效') WHERE NEW.status='enqueued' AND NOT EXISTS(
        SELECT 1 FROM coordination_assignments a JOIN coordination_dispatch_requests r
        ON (r.id,r.task_id,r.group_id)=(a.request_id,a.task_id,a.group_id)
        JOIN deliveries d ON d.id=NEW.delivery_id JOIN messages m ON m.id=d.message_id
        WHERE a.id=NEW.assignment_id AND a.task_id=NEW.task_id AND a.group_id=NEW.group_id AND a.worker_member_id=NEW.worker_member_id
        AND d.group_id=a.group_id AND d.topic_id=r.topic_id AND d.message_id=NEW.message_id
        AND d.conversation_id=NEW.conversation_id AND d.recipient_id=NEW.delivered_member_id
        AND d.recipient_id=a.worker_member_id AND m.group_id=a.group_id AND m.topic_id=r.topic_id AND m.sender_id=r.coordinator_member_id);
END;
CREATE TRIGGER coordination_assignment_dispatch_no_delete BEFORE DELETE ON coordination_assignment_dispatch
BEGIN SELECT RAISE(ABORT, '工作分配状态不可删除'); END;

CREATE TRIGGER coordination_assignment_attempts_insert BEFORE INSERT ON coordination_assignment_attempts
BEGIN
    SELECT RAISE(ABORT, '工作分配尝试绑定不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_assignment_attempts WHERE assignment_id=NEW.assignment_id OR turn_id=NEW.turn_id OR attempt_id=NEW.attempt_id);
    SELECT RAISE(ABORT, '工作分配尝试绑定关联无效') WHERE NOT EXISTS(
        SELECT 1 FROM coordination_assignments a JOIN coordination_dispatch_requests r
        ON (r.id,r.task_id,r.group_id)=(a.request_id,a.task_id,a.group_id)
        JOIN coordination_assignment_dispatch s
        ON (s.assignment_id,s.task_id,s.group_id,s.worker_member_id)=(a.id,a.task_id,a.group_id,a.worker_member_id)
        JOIN turns t ON t.id=NEW.turn_id JOIN deliveries d ON d.id=s.delivery_id
        JOIN task_attempts ta ON ta.id=NEW.attempt_id
        WHERE a.id=NEW.assignment_id AND a.group_id=NEW.group_id AND a.task_id=NEW.task_id AND a.worker_member_id=NEW.worker_member_id
        AND s.status='enqueued' AND t.group_id=a.group_id AND t.member_id=a.worker_member_id
        AND t.delivery_id=d.id AND d.turn_id=t.id AND t.conversation_id=s.conversation_id
        AND d.group_id=a.group_id AND d.topic_id=r.topic_id AND d.recipient_id=a.worker_member_id
        AND d.message_id=s.message_id AND d.conversation_id=s.conversation_id AND d.recipient_id=s.delivered_member_id
        AND ta.turn_id=t.id AND ta.task_id=a.task_id AND ta.group_id=a.group_id AND ta.actor_id=a.worker_member_id
        AND ta.task_revision=r.task_revision AND NEW.task_revision=r.task_revision);
END;
CREATE TRIGGER coordination_assignment_attempts_no_update BEFORE UPDATE ON coordination_assignment_attempts
BEGIN SELECT RAISE(ABORT, '工作分配尝试绑定不可修改'); END;
CREATE TRIGGER coordination_assignment_attempts_no_delete BEFORE DELETE ON coordination_assignment_attempts
BEGIN SELECT RAISE(ABORT, '工作分配尝试绑定不可删除'); END;
`
