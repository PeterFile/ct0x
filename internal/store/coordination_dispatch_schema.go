package store

const coordinationDispatchSchemaV12 = `
CREATE TABLE coordination_dispatch_requests (
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
    FOREIGN KEY(id,task_id,group_id) REFERENCES coordination_dispatch_state(request_id,task_id,group_id)
        DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE coordination_dispatch_state (
    request_id TEXT PRIMARY KEY NOT NULL CHECK(typeof(request_id)='text' AND request_id<>'' AND trim(request_id)=request_id),
    task_id TEXT NOT NULL CHECK(typeof(task_id)='text' AND task_id<>'' AND trim(task_id)=task_id),
    group_id TEXT NOT NULL CHECK(typeof(group_id)='text' AND group_id<>'' AND trim(group_id)=group_id),
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
    UNIQUE(request_id,task_id,group_id),
    FOREIGN KEY(request_id,task_id,group_id) REFERENCES coordination_dispatch_requests(id,task_id,group_id),
    FOREIGN KEY(delivery_id,group_id,conversation_id,delivered_member_id)
        REFERENCES deliveries(id,group_id,conversation_id,recipient_id),
    CHECK((message_id IS NULL AND delivery_id IS NULL AND conversation_id IS NULL AND delivered_member_id IS NULL
        AND materialized_from_generation IS NULL AND materialized_at IS NULL) OR
        (message_id IS NOT NULL AND delivery_id IS NOT NULL AND conversation_id IS NOT NULL AND delivered_member_id IS NOT NULL
        AND materialized_from_generation IS NOT NULL AND materialized_at IS NOT NULL)),
    -- A future migration may resolve held without a receipt; N2 exposes no release or reopen path.
    CHECK((status='pending' AND message_id IS NULL AND held_reason=''
        AND ((attempt_count=0 AND last_error_code='') OR
            (attempt_count>0 AND last_error_code IN ('sqlite_busy','sqlite_locked')))) OR
        (status='enqueued' AND message_id IS NOT NULL AND last_error_code='' AND held_reason='') OR
        (status='held' AND last_error_code='' AND held_reason IN
            ('task_stale','authority_stale','binding_stale','policy_stale','coordinator_inactive',
             'no_eligible_worker','payload_too_large','payload_not_encodable')))
);
CREATE UNIQUE INDEX coordination_dispatch_task_unfinished ON coordination_dispatch_state(task_id)
    WHERE status IN ('pending','enqueued','held');
CREATE INDEX coordination_dispatch_pending_due ON coordination_dispatch_state(next_attempt_at,request_id)
    WHERE status='pending';

CREATE TRIGGER coordination_dispatch_requests_insert BEFORE INSERT ON coordination_dispatch_requests
BEGIN
    -- The primary key also covers every conflict on the composite unique identity.
    SELECT RAISE(ABORT, '分派请求不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_dispatch_requests WHERE id=NEW.id);
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
    -- Explicit victim checks must precede shape checks, including for duplicate OR IGNORE/REPLACE.
    SELECT RAISE(ABORT, '分派状态不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_dispatch_state WHERE request_id=NEW.request_id
        OR (task_id=NEW.task_id AND status IN ('pending','enqueued','held'))
        OR (NEW.message_id IS NOT NULL AND message_id=NEW.message_id)
        OR (NEW.delivery_id IS NOT NULL AND delivery_id=NEW.delivery_id));
    SELECT RAISE(ABORT, '分派初始状态无效') WHERE NEW.status IS NOT 'pending' OR NEW.generation IS NOT 1
        OR NEW.attempt_count IS NOT 0 OR NEW.last_error_code IS NOT '' OR NEW.held_reason IS NOT ''
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
        (request_id=NEW.request_id OR (task_id=NEW.task_id AND status IN ('pending','enqueued','held'))
        OR (NEW.message_id IS NOT NULL AND message_id=NEW.message_id)
        OR (NEW.delivery_id IS NOT NULL AND delivery_id=NEW.delivery_id)));
    -- Enqueued/held freeze every column, including no-op updates and the receipt's six fields.
    SELECT RAISE(ABORT, '分派状态身份、冻结状态或世代无效') WHERE OLD.status<>'pending'
        OR NEW.request_id IS NOT OLD.request_id OR NEW.task_id IS NOT OLD.task_id OR NEW.group_id IS NOT OLD.group_id
        OR OLD.generation=9223372036854775807 OR NEW.generation IS NOT OLD.generation+1;
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

-- v8 already freezes message identity/content and blocks replacement/deletion. Do not add message write indexes.
-- These lookups use state.delivery_id UNIQUE and deliveries' existing PK / (message_id,recipient_id) UNIQUE.
CREATE TRIGGER coordination_dispatch_deliveries_insert BEFORE INSERT ON deliveries
BEGIN
    SELECT RAISE(ABORT, '分派回执投递不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=NEW.id)
        OR EXISTS(SELECT 1 FROM deliveries d JOIN coordination_dispatch_state s ON s.delivery_id=d.id
            WHERE d.message_id=NEW.message_id AND d.recipient_id=NEW.recipient_id);
END;
CREATE TRIGGER coordination_dispatch_deliveries_identity
BEFORE UPDATE OF id,message_id,group_id,topic_id,conversation_id,recipient_id ON deliveries
BEGIN
    SELECT RAISE(ABORT, '分派回执投递身份不可修改') WHERE EXISTS(
        SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=OLD.id)
        AND (NEW.id IS NOT OLD.id OR NEW.message_id IS NOT OLD.message_id OR NEW.group_id IS NOT OLD.group_id
            OR NEW.topic_id IS NOT OLD.topic_id OR NEW.conversation_id IS NOT OLD.conversation_id OR NEW.recipient_id IS NOT OLD.recipient_id);
    -- An unreferenced OLD can still delete a referenced victim via UPDATE OR REPLACE.
    SELECT RAISE(ABORT, '分派回执投递不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=NEW.id AND delivery_id<>OLD.id)
        OR EXISTS(SELECT 1 FROM deliveries d JOIN coordination_dispatch_state s ON s.delivery_id=d.id
            WHERE d.message_id=NEW.message_id AND d.recipient_id=NEW.recipient_id AND d.id<>OLD.id);
END;
CREATE TRIGGER coordination_dispatch_deliveries_no_delete BEFORE DELETE ON deliveries
WHEN EXISTS(SELECT 1 FROM coordination_dispatch_state WHERE delivery_id=OLD.id)
BEGIN SELECT RAISE(ABORT, '分派回执投递不可删除'); END;
`
