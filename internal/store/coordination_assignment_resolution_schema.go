package store

const coordinationAssignmentResolutionSchemaV14 = `
-- Foundation rows cannot exist through the v13 INSERT gate. Never repair counterfactual data.
CREATE TABLE coordination_resolution_preflight(ok INTEGER CHECK(ok=1));
INSERT INTO coordination_resolution_preflight SELECT 0 WHERE
    EXISTS(SELECT 1 FROM coordination_assignments) OR EXISTS(SELECT 1 FROM coordination_assignment_dispatch)
    OR EXISTS(SELECT 1 FROM coordination_assignment_attempts);
-- Actual v13 EXPLAIN scans turns for both first-reference probes; keep ordinary deliveries nonunique.
CREATE INDEX turns_coordination_delivery ON turns(delivery_id);
INSERT INTO coordination_resolution_preflight SELECT 0 WHERE EXISTS(
    SELECT 1 FROM coordination_dispatch_state s JOIN deliveries d ON d.id=s.delivery_id WHERE
    (d.turn_id='' AND EXISTS(SELECT 1 FROM turns t WHERE t.delivery_id=d.id)) OR
    (d.turn_id<>'' AND (NOT EXISTS(SELECT 1 FROM turns t WHERE t.id=d.turn_id
        AND t.delivery_id=d.id AND t.group_id=d.group_id AND t.conversation_id=d.conversation_id AND t.member_id=d.recipient_id)
        OR EXISTS(SELECT 1 FROM turns t WHERE t.delivery_id=d.id AND t.id<>d.turn_id))));
DROP TABLE coordination_resolution_preflight;

CREATE TABLE coordination_decisions (
    id TEXT PRIMARY KEY NOT NULL CHECK(typeof(id)='text' AND id<>'' AND trim(id)=id),
    request_id TEXT NOT NULL UNIQUE CHECK(typeof(request_id)='text' AND request_id<>'' AND trim(request_id)=request_id),
    task_id TEXT NOT NULL CHECK(typeof(task_id)='text' AND task_id<>'' AND trim(task_id)=task_id),
    group_id TEXT NOT NULL CHECK(typeof(group_id)='text' AND group_id<>'' AND trim(group_id)=group_id),
    coordinator_member_id TEXT NOT NULL CHECK(typeof(coordinator_member_id)='text'
        AND coordinator_member_id<>'' AND trim(coordinator_member_id)=coordinator_member_id),
    coordinator_turn_id TEXT NOT NULL UNIQUE CHECK(typeof(coordinator_turn_id)='text'
        AND coordinator_turn_id<>'' AND trim(coordinator_turn_id)=coordinator_turn_id),
    kind TEXT NOT NULL CHECK(kind IN ('assign','decline')),
    worker_member_id TEXT CHECK(worker_member_id IS NULL OR
        (typeof(worker_member_id)='text' AND worker_member_id<>'' AND trim(worker_member_id)=worker_member_id)),
    assignment_id TEXT CHECK(assignment_id IS NULL OR
        (typeof(assignment_id)='text' AND assignment_id<>'' AND trim(assignment_id)=assignment_id)),
    reason TEXT NOT NULL CHECK(typeof(reason)='text' AND trim(reason)<>''),
    created_at TEXT NOT NULL CHECK(typeof(created_at)='text' AND length(CAST(created_at AS BLOB))=30
        AND created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z'
        AND substr(created_at,1,4) BETWEEN '0001' AND '9999'
        AND substr(created_at,12,2) BETWEEN '00' AND '23'
        AND substr(created_at,15,2) BETWEEN '00' AND '59' AND substr(created_at,18,2) BETWEEN '00' AND '59'
        AND date(substr(created_at,1,10),'+0 days') IS substr(created_at,1,10)),
    CHECK((kind='assign' AND worker_member_id IS NOT NULL AND assignment_id IS NOT NULL)
        OR (kind='decline' AND worker_member_id IS NULL AND assignment_id IS NULL)),
    UNIQUE(id,request_id,task_id,group_id),
    FOREIGN KEY(request_id,task_id,group_id) REFERENCES coordination_dispatch_requests_new(id,task_id,group_id),
    FOREIGN KEY(coordinator_turn_id,group_id,coordinator_member_id) REFERENCES turns(id,group_id,member_id),
    FOREIGN KEY(group_id,worker_member_id) REFERENCES members(group_id,id),
    FOREIGN KEY(id,request_id,task_id,group_id)
        REFERENCES coordination_dispatch_state_new(decision_id,request_id,task_id,group_id) DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE coordination_dispatch_requests_new (
    id TEXT PRIMARY KEY NOT NULL CHECK(typeof(id)='text' AND id<>'' AND trim(id)=id),
    group_id TEXT NOT NULL CHECK(typeof(group_id)='text' AND group_id<>'' AND trim(group_id)=group_id),
    topic_id TEXT NOT NULL,
    task_id TEXT NOT NULL CHECK(typeof(task_id)='text' AND task_id<>'' AND trim(task_id)=task_id),
    task_revision INTEGER NOT NULL CHECK(typeof(task_revision)='integer' AND task_revision>0),
    authority_generation INTEGER NOT NULL CHECK(typeof(authority_generation)='integer' AND authority_generation>0),
    mandate_id TEXT NOT NULL CHECK(typeof(mandate_id)='text' AND mandate_id<>'' AND trim(mandate_id)=mandate_id),
    coordinator_member_id TEXT NOT NULL CHECK(typeof(coordinator_member_id)='text'
        AND coordinator_member_id<>'' AND trim(coordinator_member_id)=coordinator_member_id),
    binding_revision INTEGER NOT NULL CHECK(typeof(binding_revision)='integer' AND binding_revision>0),
    policy_revision INTEGER NOT NULL CHECK(typeof(policy_revision)='integer' AND policy_revision>0),
    requested_by TEXT NOT NULL CHECK(requested_by='user'),
    reason TEXT NOT NULL CHECK(typeof(reason)='text' AND trim(reason)<>''),
    -- ASCII syntax and normalized calendar date are checked separately; fractions never enter date arithmetic.
    created_at TEXT NOT NULL CHECK(typeof(created_at)='text' AND length(CAST(created_at AS BLOB))=30
        AND created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z'
        AND substr(created_at,1,4) BETWEEN '0001' AND '9999'
        AND substr(created_at,12,2) BETWEEN '00' AND '23'
        AND substr(created_at,15,2) BETWEEN '00' AND '59' AND substr(created_at,18,2) BETWEEN '00' AND '59'
        AND date(substr(created_at,1,10),'+0 days') IS substr(created_at,1,10)),
    UNIQUE(id,task_id,group_id),
    FOREIGN KEY(task_id,group_id) REFERENCES tasks(id,group_id),
    FOREIGN KEY(mandate_id,task_id,group_id,topic_id) REFERENCES mandates(id,task_id,group_id,topic_id),
    FOREIGN KEY(group_id,coordinator_member_id) REFERENCES members(group_id,id),
    FOREIGN KEY(group_id,binding_revision) REFERENCES group_coordinator_binding_revisions(group_id,revision),
    FOREIGN KEY(group_id,policy_revision) REFERENCES group_dispatch_policy_revisions(group_id,revision),
    -- Request-first is required: only this direction is deferred.
    FOREIGN KEY(id,task_id,group_id) REFERENCES coordination_dispatch_state_new(request_id,task_id,group_id)
        DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE coordination_dispatch_state_new (
    request_id TEXT PRIMARY KEY NOT NULL CHECK(typeof(request_id)='text' AND request_id<>'' AND trim(request_id)=request_id),
    task_id TEXT NOT NULL CHECK(typeof(task_id)='text' AND task_id<>'' AND trim(task_id)=task_id),
    group_id TEXT NOT NULL CHECK(typeof(group_id)='text' AND group_id<>'' AND trim(group_id)=group_id),
    generation INTEGER NOT NULL CHECK(typeof(generation)='integer' AND generation>0),
    status TEXT NOT NULL CHECK(status IN ('pending','enqueued','held','assigned','declined')),
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
    decision_id TEXT CHECK(decision_id IS NULL OR (typeof(decision_id)='text' AND decision_id<>'' AND trim(decision_id)=decision_id)),
    UNIQUE(request_id,task_id,group_id),
    UNIQUE(decision_id,request_id,task_id,group_id),
    FOREIGN KEY(request_id,task_id,group_id) REFERENCES coordination_dispatch_requests_new(id,task_id,group_id),
    FOREIGN KEY(decision_id,request_id,task_id,group_id) REFERENCES coordination_decisions(id,request_id,task_id,group_id),
    FOREIGN KEY(delivery_id,group_id,conversation_id,delivered_member_id)
        REFERENCES deliveries(id,group_id,conversation_id,recipient_id),
    CHECK((message_id IS NULL AND delivery_id IS NULL AND conversation_id IS NULL AND delivered_member_id IS NULL
        AND materialized_from_generation IS NULL AND materialized_at IS NULL) OR
        (message_id IS NOT NULL AND delivery_id IS NOT NULL AND conversation_id IS NOT NULL AND delivered_member_id IS NOT NULL
        AND materialized_from_generation IS NOT NULL AND materialized_at IS NOT NULL)),
    CHECK((status IN ('pending','enqueued','held') AND decision_id IS NULL)
        OR (status IN ('assigned','declined') AND decision_id IS NOT NULL)),
    -- A future migration may resolve held without a receipt; N2 exposes no release or reopen path.
    CHECK((status='pending' AND message_id IS NULL AND held_reason=''
        AND ((attempt_count=0 AND last_error_code='') OR
            (attempt_count>0 AND last_error_code IN ('sqlite_busy','sqlite_locked')))) OR
        (status IN ('enqueued','assigned','declined') AND message_id IS NOT NULL AND last_error_code='' AND held_reason='') OR
        (status='held' AND last_error_code='' AND held_reason IN
            ('task_stale','authority_stale','binding_stale','policy_stale','coordinator_inactive',
             'no_eligible_worker','payload_too_large','payload_not_encodable')))
);
-- Copy original columns explicitly, before installing insertion-only current business fences.
INSERT INTO coordination_dispatch_requests_new(id,group_id,topic_id,task_id,task_revision,authority_generation,mandate_id,
    coordinator_member_id,binding_revision,policy_revision,requested_by,reason,created_at)
    SELECT id,group_id,topic_id,task_id,task_revision,authority_generation,mandate_id,
        coordinator_member_id,binding_revision,policy_revision,requested_by,reason,created_at FROM coordination_dispatch_requests;
INSERT INTO coordination_dispatch_state_new(request_id,task_id,group_id,generation,status,attempt_count,next_attempt_at,updated_at,
    last_error_code,held_reason,message_id,delivery_id,conversation_id,delivered_member_id,materialized_from_generation,materialized_at,decision_id)
    SELECT request_id,task_id,group_id,generation,status,attempt_count,next_attempt_at,updated_at,
        last_error_code,held_reason,message_id,delivery_id,conversation_id,delivered_member_id,materialized_from_generation,materialized_at,NULL
        FROM coordination_dispatch_state;
-- Only these five external guards refer to the paired tables during the rename window.
DROP TRIGGER coordination_dispatch_deliveries_insert;
DROP TRIGGER coordination_dispatch_deliveries_identity;
DROP TRIGGER coordination_dispatch_deliveries_no_delete;
DROP TRIGGER coordination_assignment_dispatch_update;
DROP TRIGGER coordination_assignment_attempts_insert;
DROP TABLE coordination_dispatch_state;
DROP TABLE coordination_dispatch_requests;
ALTER TABLE coordination_dispatch_requests_new RENAME TO coordination_dispatch_requests;
ALTER TABLE coordination_dispatch_state_new RENAME TO coordination_dispatch_state;
-- Keep the original predicate as an OR term: SQLite cannot prove an IN-list subset for legacy reads.
CREATE UNIQUE INDEX coordination_dispatch_task_unfinished ON coordination_dispatch_state(task_id)
    WHERE status IN ('pending','enqueued','held') OR status='declined';
CREATE INDEX coordination_dispatch_pending_due ON coordination_dispatch_state(next_attempt_at,request_id)
    WHERE status='pending';

CREATE TRIGGER coordination_dispatch_requests_insert BEFORE INSERT ON coordination_dispatch_requests
BEGIN
    -- The primary key also covers every conflict on the composite unique identity.
    SELECT RAISE(ABORT, '分派请求不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_dispatch_requests WHERE id=NEW.id);
    SELECT RAISE(ABORT, '任务分派所有权冲突') WHERE EXISTS(SELECT 1 FROM coordination_assignments WHERE task_id=NEW.task_id);
    SELECT RAISE(ABORT, '分派授权历史版本不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM mandates WHERE id=NEW.mandate_id AND task_id=NEW.task_id
        AND group_id=NEW.group_id AND topic_id=NEW.topic_id AND task_revision_at_admission=NEW.task_revision);
    SELECT RAISE(ABORT, '分派协调员绑定不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM group_coordinator_binding_revisions WHERE group_id=NEW.group_id
        AND revision=NEW.binding_revision AND member_id=NEW.coordinator_member_id);
    -- Current fences are checked only at insertion; historical FKs never bind mutable current pointers.
    -- Active membership, eligible workers and the final materialization fence belong to N2b/N2c.
    SELECT RAISE(ABORT, '分派当前任务、授权或配置不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM tasks t JOIN task_coordination c ON c.task_id=t.id AND c.group_id=t.group_id
        JOIN group_coordinator_bindings b ON b.group_id=t.group_id
        JOIN group_dispatch_policies p ON p.group_id=t.group_id
        WHERE t.id=NEW.task_id AND t.group_id=NEW.group_id AND t.status='open' AND t.revision=NEW.task_revision
        AND t.active_attempt_id IS NULL AND t.current_submission_id IS NULL
        AND c.topic_id=NEW.topic_id AND c.current_mandate_id=NEW.mandate_id
        AND c.authority_generation=NEW.authority_generation AND b.revision=NEW.binding_revision AND p.revision=NEW.policy_revision);
END;
CREATE TRIGGER coordination_dispatch_requests_no_update BEFORE UPDATE ON coordination_dispatch_requests
BEGIN SELECT RAISE(ABORT, '分派请求不可修改'); END;
CREATE TRIGGER coordination_dispatch_requests_no_delete BEFORE DELETE ON coordination_dispatch_requests
BEGIN SELECT RAISE(ABORT, '分派请求不可删除'); END;
CREATE TRIGGER coordination_dispatch_state_insert BEFORE INSERT ON coordination_dispatch_state
BEGIN
    SELECT RAISE(ABORT, '分派状态不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_dispatch_state WHERE request_id=NEW.request_id
        OR (task_id=NEW.task_id AND (status IN ('pending','enqueued','held') OR status='declined'))
        OR (NEW.message_id IS NOT NULL AND message_id=NEW.message_id)
        OR (NEW.delivery_id IS NOT NULL AND delivery_id=NEW.delivery_id));
    -- Close request-first's deferred missing-state window, even after another request resolves in this transaction.
    SELECT RAISE(ABORT, '任务分派所有权冲突') WHERE EXISTS(SELECT 1 FROM coordination_assignments WHERE task_id=NEW.task_id);
    SELECT RAISE(ABORT, '分派初始状态无效') WHERE NEW.status IS NOT 'pending' OR NEW.generation IS NOT 1
        OR NEW.attempt_count IS NOT 0 OR NEW.last_error_code IS NOT '' OR NEW.held_reason IS NOT '' OR NEW.decision_id IS NOT NULL
        OR NEW.message_id IS NOT NULL OR NEW.delivery_id IS NOT NULL OR NEW.conversation_id IS NOT NULL
        OR NEW.delivered_member_id IS NOT NULL OR NEW.materialized_from_generation IS NOT NULL OR NEW.materialized_at IS NOT NULL;
    SELECT RAISE(ABORT, '分派初始时间无效') WHERE EXISTS(
        SELECT 1 FROM coordination_dispatch_requests WHERE id=NEW.request_id
        AND (NEW.next_attempt_at IS NOT created_at OR NEW.updated_at IS NOT created_at));
END;
CREATE TRIGGER coordination_dispatch_state_update BEFORE UPDATE ON coordination_dispatch_state
BEGIN
    SELECT RAISE(ABORT, '分派状态不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_dispatch_state WHERE request_id<>OLD.request_id AND
        (request_id=NEW.request_id OR (task_id=NEW.task_id AND (status IN ('pending','enqueued','held') OR status='declined'))
        OR (NEW.message_id IS NOT NULL AND message_id=NEW.message_id)
        OR (NEW.delivery_id IS NOT NULL AND delivery_id=NEW.delivery_id)));
    SELECT RAISE(ABORT, '分派状态身份、冻结状态或世代无效') WHERE
        NEW.request_id IS NOT OLD.request_id OR NEW.task_id IS NOT OLD.task_id OR NEW.group_id IS NOT OLD.group_id
        OR OLD.generation=9223372036854775807 OR NEW.generation IS NOT OLD.generation+1
        OR NOT ((OLD.status='pending' AND NEW.status IN ('pending','held','enqueued') AND NEW.decision_id IS NULL)
            OR (OLD.status='enqueued' AND NEW.status IN ('assigned','declined') AND NEW.decision_id IS NOT NULL));
    SELECT RAISE(ABORT, '分派决定必须保留原回执和计数') WHERE OLD.status='enqueued' AND
        (NEW.attempt_count IS NOT OLD.attempt_count OR NEW.next_attempt_at IS NOT OLD.next_attempt_at
        OR NEW.message_id IS NOT OLD.message_id OR NEW.delivery_id IS NOT OLD.delivery_id
        OR NEW.conversation_id IS NOT OLD.conversation_id OR NEW.delivered_member_id IS NOT OLD.delivered_member_id
        OR NEW.materialized_from_generation IS NOT OLD.materialized_from_generation OR NEW.materialized_at IS NOT OLD.materialized_at);
    SELECT RAISE(ABORT, '分派决定关联无效') WHERE NEW.status IN ('assigned','declined') AND NOT EXISTS(
        SELECT 1 FROM coordination_decisions c WHERE (c.id,c.request_id,c.task_id,c.group_id)=(NEW.decision_id,NEW.request_id,NEW.task_id,NEW.group_id)
        AND ((NEW.status='declined' AND c.kind='decline' AND NOT EXISTS(SELECT 1 FROM coordination_assignments WHERE task_id=NEW.task_id))
            OR (NEW.status='assigned' AND c.kind='assign' AND EXISTS(
                SELECT 1 FROM coordination_assignments a JOIN coordination_assignment_dispatch s
                ON (s.assignment_id,s.task_id,s.group_id,s.worker_member_id)=(a.id,a.task_id,a.group_id,a.worker_member_id)
                WHERE (a.id,a.decision_id,a.request_id,a.task_id,a.group_id,a.worker_member_id)=
                    (c.assignment_id,c.id,c.request_id,c.task_id,c.group_id,c.worker_member_id)
                AND s.status='pending' AND s.generation=1 AND s.attempt_count=0 AND s.message_id IS NULL))));
    -- Strip fractions before whole-second arithmetic, then append their exact original bytes.
    SELECT RAISE(ABORT, '分派退避状态无效') WHERE NEW.status='pending' AND
        (OLD.attempt_count=9223372036854775807 OR NEW.attempt_count IS NOT OLD.attempt_count+1
        OR NEW.next_attempt_at IS NOT (strftime('%Y-%m-%dT%H:%M:%S',substr(NEW.updated_at,1,19)||'Z',
            CASE OLD.attempt_count WHEN 0 THEN '+2 seconds' WHEN 1 THEN '+4 seconds' WHEN 2 THEN '+8 seconds'
                WHEN 3 THEN '+16 seconds' WHEN 4 THEN '+32 seconds' ELSE '+60 seconds' END)||substr(NEW.updated_at,20)));
    SELECT RAISE(ABORT, '分派完成状态无效') WHERE NEW.status IN ('held','enqueued') AND
        (NEW.attempt_count IS NOT OLD.attempt_count OR NEW.next_attempt_at IS NOT OLD.next_attempt_at);
    SELECT RAISE(ABORT, '暂停分派不能生成回执') WHERE NEW.status='held' AND NEW.message_id IS NOT NULL;
    SELECT RAISE(ABORT, '分派回执世代或时间无效') WHERE NEW.status='enqueued' AND
        (NEW.materialized_from_generation IS NOT OLD.generation OR NEW.materialized_at IS NOT NEW.updated_at);
    SELECT RAISE(ABORT, '分派回执关联无效') WHERE NEW.status='enqueued' AND NOT EXISTS(
        SELECT 1 FROM coordination_dispatch_requests r JOIN deliveries d ON d.id=NEW.delivery_id
        JOIN messages m ON m.id=d.message_id
        WHERE r.id=NEW.request_id AND r.task_id=NEW.task_id AND r.group_id=NEW.group_id
        AND d.group_id=r.group_id AND d.topic_id=r.topic_id AND d.message_id=NEW.message_id
        AND d.conversation_id=NEW.conversation_id AND d.recipient_id=NEW.delivered_member_id
        AND d.recipient_id=r.coordinator_member_id AND m.group_id=r.group_id AND m.topic_id=r.topic_id AND m.sender_id='user');
END;
CREATE TRIGGER coordination_dispatch_state_no_delete BEFORE DELETE ON coordination_dispatch_state
BEGIN SELECT RAISE(ABORT, '分派状态不可删除'); END;

CREATE TRIGGER coordination_decisions_insert BEFORE INSERT ON coordination_decisions
BEGIN
    SELECT RAISE(ABORT, '协调决定不可替换') WHERE EXISTS(SELECT 1 FROM coordination_decisions
        WHERE id=NEW.id OR request_id=NEW.request_id OR coordinator_turn_id=NEW.coordinator_turn_id);
    SELECT RAISE(ABORT, '协调决定来源无效') WHERE NOT EXISTS(
        SELECT 1 FROM coordination_dispatch_requests r JOIN coordination_dispatch_state s
        ON (s.request_id,s.task_id,s.group_id)=(r.id,r.task_id,r.group_id)
        JOIN deliveries d ON d.id=s.delivery_id JOIN turns t ON t.id=NEW.coordinator_turn_id
        WHERE (r.id,r.task_id,r.group_id,r.coordinator_member_id)=(NEW.request_id,NEW.task_id,NEW.group_id,NEW.coordinator_member_id)
        AND s.status='enqueued' AND s.decision_id IS NULL AND d.message_id=s.message_id
        AND d.group_id=r.group_id AND d.topic_id=r.topic_id AND d.recipient_id=r.coordinator_member_id
        AND d.conversation_id=s.conversation_id AND d.recipient_id=s.delivered_member_id
        AND t.delivery_id=d.id AND d.turn_id=t.id AND t.group_id=r.group_id
        AND t.conversation_id=d.conversation_id AND t.member_id=r.coordinator_member_id);
END;
CREATE TRIGGER coordination_decisions_no_update BEFORE UPDATE ON coordination_decisions
BEGIN SELECT RAISE(ABORT, '协调决定不可修改'); END;
CREATE TRIGGER coordination_decisions_no_delete BEFORE DELETE ON coordination_decisions
BEGIN SELECT RAISE(ABORT, '协调决定不可删除'); END;
CREATE TRIGGER coordination_assignments_decision_insert BEFORE INSERT ON coordination_assignments
BEGIN
    SELECT RAISE(ABORT, '工作分配决定或所有权无效') WHERE NOT EXISTS(
        SELECT 1 FROM coordination_decisions c JOIN coordination_dispatch_state s
        ON (s.request_id,s.task_id,s.group_id)=(c.request_id,c.task_id,c.group_id)
        WHERE (c.id,c.assignment_id,c.request_id,c.task_id,c.group_id,c.worker_member_id)=
            (NEW.decision_id,NEW.id,NEW.request_id,NEW.task_id,NEW.group_id,NEW.worker_member_id)
        AND c.kind='assign' AND s.status='enqueued' AND s.decision_id IS NULL)
        OR EXISTS(SELECT 1 FROM coordination_dispatch_state WHERE task_id=NEW.task_id AND request_id<>NEW.request_id
            AND (status IN ('pending','enqueued','held') OR status='declined'));
END;
-- Restore both external v13 definitions without changing their existing semantics.
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

-- A projection, not a third binding table. Both branches have a UNIQUE delivery lookup.
-- Decisions and Attempt bindings use these exact receipts, which can never be removed or repointed.
CREATE VIEW coordination_role_deliveries AS
    SELECT delivery_id FROM coordination_dispatch_state WHERE delivery_id IS NOT NULL
    UNION ALL SELECT delivery_id FROM coordination_assignment_dispatch WHERE delivery_id IS NOT NULL;
CREATE TRIGGER coordination_dispatch_receipt_source BEFORE UPDATE ON coordination_dispatch_state
WHEN NEW.delivery_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, '角色回执不能复用') WHERE EXISTS(SELECT 1 FROM coordination_assignment_dispatch
        WHERE message_id=NEW.message_id OR delivery_id=NEW.delivery_id);
    SELECT RAISE(ABORT, '角色轮次来源无效') WHERE EXISTS(SELECT 1 FROM deliveries d WHERE d.id=NEW.delivery_id AND
        ((d.turn_id='' AND EXISTS(SELECT 1 FROM turns t WHERE t.delivery_id=d.id)) OR
        (d.turn_id<>'' AND (NOT EXISTS(SELECT 1 FROM turns t WHERE t.id=d.turn_id
            AND t.delivery_id=d.id AND t.group_id=d.group_id AND t.conversation_id=d.conversation_id AND t.member_id=d.recipient_id)
            OR EXISTS(SELECT 1 FROM turns t WHERE t.delivery_id=d.id AND t.id<>d.turn_id)))));
END;
CREATE TRIGGER coordination_assignment_receipt_source BEFORE UPDATE ON coordination_assignment_dispatch
WHEN NEW.delivery_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, '角色回执不能复用') WHERE EXISTS(SELECT 1 FROM coordination_dispatch_state
        WHERE message_id=NEW.message_id OR delivery_id=NEW.delivery_id);
    SELECT RAISE(ABORT, '角色轮次来源无效') WHERE EXISTS(SELECT 1 FROM deliveries d WHERE d.id=NEW.delivery_id AND
        ((d.turn_id='' AND EXISTS(SELECT 1 FROM turns t WHERE t.delivery_id=d.id)) OR
        (d.turn_id<>'' AND (NOT EXISTS(SELECT 1 FROM turns t WHERE t.id=d.turn_id
            AND t.delivery_id=d.id AND t.group_id=d.group_id AND t.conversation_id=d.conversation_id AND t.member_id=d.recipient_id)
            OR EXISTS(SELECT 1 FROM turns t WHERE t.delivery_id=d.id AND t.id<>d.turn_id)))));
END;
-- v8 message invariants remain untouched; delivery lifecycle fields remain mutable.
CREATE TRIGGER coordination_dispatch_deliveries_insert BEFORE INSERT ON deliveries
BEGIN
    SELECT RAISE(ABORT, '角色回执投递不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_role_deliveries WHERE delivery_id=NEW.id)
        OR EXISTS(SELECT 1 FROM deliveries d JOIN coordination_role_deliveries s ON s.delivery_id=d.id
            WHERE d.message_id=NEW.message_id AND d.recipient_id=NEW.recipient_id);
END;
CREATE TRIGGER coordination_dispatch_deliveries_identity
BEFORE UPDATE OF id,message_id,group_id,topic_id,conversation_id,recipient_id,turn_id ON deliveries
BEGIN
    SELECT RAISE(ABORT, '角色回执投递身份不可修改') WHERE EXISTS(
        SELECT 1 FROM coordination_role_deliveries WHERE delivery_id=OLD.id)
        AND (NEW.id IS NOT OLD.id OR NEW.message_id IS NOT OLD.message_id OR NEW.group_id IS NOT OLD.group_id
            OR NEW.topic_id IS NOT OLD.topic_id OR NEW.conversation_id IS NOT OLD.conversation_id OR NEW.recipient_id IS NOT OLD.recipient_id);
    SELECT RAISE(ABORT, '角色回执投递回指不可修改') WHERE EXISTS(
        SELECT 1 FROM coordination_role_deliveries WHERE delivery_id=OLD.id) AND NEW.turn_id IS NOT OLD.turn_id
        AND NOT (OLD.turn_id='' AND typeof(NEW.turn_id)='text' AND NEW.turn_id<>'' AND trim(NEW.turn_id)=NEW.turn_id);
    SELECT RAISE(ABORT, '角色回执投递不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_role_deliveries WHERE delivery_id=NEW.id AND delivery_id<>OLD.id)
        OR EXISTS(SELECT 1 FROM deliveries d JOIN coordination_role_deliveries s ON s.delivery_id=d.id
            WHERE d.message_id=NEW.message_id AND d.recipient_id=NEW.recipient_id AND d.id<>OLD.id);
END;
CREATE TRIGGER coordination_dispatch_deliveries_no_delete BEFORE DELETE ON deliveries
WHEN EXISTS(SELECT 1 FROM coordination_role_deliveries WHERE delivery_id=OLD.id)
BEGIN SELECT RAISE(ABORT, '角色回执投递不可删除'); END;
CREATE TRIGGER coordination_role_turns_insert BEFORE INSERT ON turns
BEGIN
    -- Spell out all three indexed conflict branches; ordinary REPLACE can delete a role victim.
    SELECT RAISE(ABORT, '角色轮次不可替换') WHERE EXISTS(SELECT 1 FROM turns t WHERE
        (t.id=NEW.id
        OR (t.member_id=NEW.member_id AND t.state IN ('starting','running','cancel_requested','unknown')
            AND NEW.state IN ('starting','running','cancel_requested','unknown'))
        OR (t.conversation_id=NEW.conversation_id AND t.state IN ('starting','running','cancel_requested','unknown')
            AND NEW.state IN ('starting','running','cancel_requested','unknown')))
        AND EXISTS(SELECT 1 FROM coordination_role_deliveries WHERE delivery_id=t.delivery_id));
    SELECT RAISE(ABORT, '角色轮次来源无效') WHERE EXISTS(
        SELECT 1 FROM coordination_role_deliveries WHERE delivery_id=NEW.delivery_id) AND NOT EXISTS(
        SELECT 1 FROM deliveries d WHERE d.id=NEW.delivery_id AND d.turn_id=NEW.id
        AND d.group_id=NEW.group_id AND d.conversation_id=NEW.conversation_id AND d.recipient_id=NEW.member_id);
END;
CREATE TRIGGER coordination_role_turns_update
BEFORE UPDATE OF id,group_id,delivery_id,conversation_id,member_id,state ON turns
BEGIN
    SELECT RAISE(ABORT, '角色轮次身份不可修改') WHERE EXISTS(
        SELECT 1 FROM coordination_role_deliveries WHERE delivery_id=OLD.delivery_id)
        AND (NEW.id IS NOT OLD.id OR NEW.group_id IS NOT OLD.group_id OR NEW.delivery_id IS NOT OLD.delivery_id
            OR NEW.conversation_id IS NOT OLD.conversation_id OR NEW.member_id IS NOT OLD.member_id);
    SELECT RAISE(ABORT, '角色轮次不可替换') WHERE EXISTS(SELECT 1 FROM turns t WHERE t.id<>OLD.id AND
        (t.id=NEW.id
        OR (t.member_id=NEW.member_id AND t.state IN ('starting','running','cancel_requested','unknown')
            AND NEW.state IN ('starting','running','cancel_requested','unknown'))
        OR (t.conversation_id=NEW.conversation_id AND t.state IN ('starting','running','cancel_requested','unknown')
            AND NEW.state IN ('starting','running','cancel_requested','unknown')))
        AND EXISTS(SELECT 1 FROM coordination_role_deliveries WHERE delivery_id=t.delivery_id));
    SELECT RAISE(ABORT, '角色轮次来源无效') WHERE EXISTS(
        SELECT 1 FROM coordination_role_deliveries WHERE delivery_id=NEW.delivery_id) AND NOT EXISTS(
        SELECT 1 FROM deliveries d WHERE d.id=NEW.delivery_id AND d.turn_id=NEW.id
        AND d.group_id=NEW.group_id AND d.conversation_id=NEW.conversation_id AND d.recipient_id=NEW.member_id);
END;
CREATE TRIGGER coordination_role_turns_no_delete BEFORE DELETE ON turns
WHEN EXISTS(SELECT 1 FROM coordination_role_deliveries WHERE delivery_id=OLD.delivery_id)
BEGIN SELECT RAISE(ABORT, '角色轮次不可删除'); END;
-- Enable SQL capability only after every decision, ownership, receipt and parent invariant is installed.
DROP TRIGGER coordination_assignments_disabled;
`
