package store

// Registered only after the frozen F3 v20 scope migration. These tables record
// metadata evidence, not filesystem capability or a second execution engine.
const workspaceOperationSchemaV21 = workspaceOperationIntentSchemaV21 + workerArtifactSchemaV21

const workspaceOperationIntentSchemaV21 = `
CREATE TABLE workspace_operations (
    -- SQLite serializes writers; this declared ordinal survives VACUUM/reopen.
    sequence INTEGER PRIMARY KEY AUTOINCREMENT CHECK(sequence > 0),
    id TEXT NOT NULL UNIQUE CHECK(id<>'' AND length(CAST(id AS BLOB))<=256),
    group_id TEXT NOT NULL,
    task_id TEXT NOT NULL REFERENCES chat_workspace_scopes(task_id),
    task_revision INTEGER NOT NULL CHECK(typeof(task_revision)='integer' AND task_revision>0),
    assignment_id TEXT NOT NULL REFERENCES coordination_assignments(id),
    attempt_id TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    member_id TEXT NOT NULL,
    grant_id TEXT NOT NULL REFERENCES workspace_write_grants(id),
    resource_key TEXT NOT NULL CHECK(resource_key LIKE 'workspace:%'),
    node_id TEXT NOT NULL CHECK(node_id='local'),
    claim_digest TEXT NOT NULL CHECK(length(claim_digest)=64 AND claim_digest NOT GLOB '*[^0-9a-f]*'),
    idempotency_key TEXT NOT NULL CHECK(length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 256 AND trim(idempotency_key)=idempotency_key),
    relative_path TEXT NOT NULL CHECK(length(CAST(relative_path AS BLOB)) BETWEEN 1 AND 4096
        AND trim(relative_path)=relative_path AND substr(relative_path,1,1)<>'/' AND substr(relative_path,-1)<>'/'
        AND instr(relative_path,char(0))=0 AND instr(relative_path,'\')=0 AND instr(relative_path,':')=0
        AND instr(relative_path,'//')=0 AND instr('/'||relative_path||'/','/./')=0
        AND instr('/'||relative_path||'/','/../')=0
        AND relative_path NOT GLOB ('*['||char(1)||'-'||char(31)||char(127)||'-'||char(159)||']*')),
    kind TEXT NOT NULL CHECK(kind IN ('create','replace')),
    expected_hash TEXT NOT NULL,
    after_hash TEXT NOT NULL CHECK(length(after_hash)=64 AND length(CAST(after_hash AS BLOB))=64 AND after_hash NOT GLOB '*[^0-9a-f]*'),
    after_size INTEGER NOT NULL CHECK(typeof(after_size)='integer' AND after_size BETWEEN 0 AND 16777216),
    preimage_exists INTEGER NOT NULL CHECK(typeof(preimage_exists)='integer' AND preimage_exists IN (0,1)),
    preimage_hash TEXT NOT NULL,
    preimage_blob_hash TEXT NOT NULL,
    preimage_size INTEGER NOT NULL CHECK(typeof(preimage_size)='integer' AND preimage_size BETWEEN 0 AND 16777216),
    notice_message_id TEXT NOT NULL UNIQUE REFERENCES messages(id),
    created_at TEXT NOT NULL CHECK(length(CAST(created_at AS BLOB))=30 AND julianday(created_at) IS NOT NULL),
    UNIQUE(group_id,member_id,idempotency_key),
    FOREIGN KEY(task_id,group_id) REFERENCES tasks(id,group_id),
    FOREIGN KEY(turn_id,group_id,member_id) REFERENCES turns(id,group_id,member_id),
    FOREIGN KEY(attempt_id,task_id,group_id,task_revision,member_id) REFERENCES task_attempts(id,task_id,group_id,task_revision,actor_id),
    CHECK((kind='create' AND expected_hash='' AND preimage_exists=0 AND preimage_hash='' AND preimage_blob_hash='' AND preimage_size=0)
        OR (kind='replace' AND preimage_exists=1 AND length(expected_hash)=64 AND length(CAST(expected_hash AS BLOB))=64
            AND expected_hash NOT GLOB '*[^0-9a-f]*' AND expected_hash=preimage_hash AND preimage_hash=preimage_blob_hash))
);
CREATE INDEX workspace_operations_task ON workspace_operations(group_id,task_id,id);
CREATE INDEX workspace_operations_turn ON workspace_operations(group_id,turn_id,created_at DESC,id DESC);
CREATE INDEX workspace_operations_path ON workspace_operations(group_id,task_id,turn_id,relative_path,sequence DESC);
CREATE TABLE workspace_operation_receipts (
    operation_id TEXT PRIMARY KEY NOT NULL REFERENCES workspace_operations(id),
    outcome TEXT NOT NULL CHECK(outcome IN ('unchanged','done','unknown','applied_sync_error')),
    observed_hash TEXT NOT NULL CHECK(observed_hash='' OR (length(observed_hash)=64 AND length(CAST(observed_hash AS BLOB))=64 AND observed_hash NOT GLOB '*[^0-9a-f]*')),
    observed_size INTEGER NOT NULL CHECK(typeof(observed_size)='integer' AND observed_size BETWEEN 0 AND 16777216 AND (observed_hash<>'' OR observed_size=0)),
    error_code TEXT NOT NULL CHECK(length(CAST(error_code AS BLOB))<=64 AND error_code NOT GLOB '*[^a-z0-9_]*'),
    notice_message_id TEXT NOT NULL UNIQUE REFERENCES messages(id),
    finished_at TEXT NOT NULL CHECK(length(CAST(finished_at AS BLOB))=30 AND julianday(finished_at) IS NOT NULL),
    CHECK((outcome='done' AND error_code='') OR (outcome IN ('unknown','applied_sync_error') AND error_code<>'') OR outcome='unchanged')
);
CREATE TRIGGER workspace_operations_insert BEFORE INSERT ON workspace_operations
BEGIN
    SELECT RAISE(ABORT, '文件操作意图不可替换') WHERE EXISTS(SELECT 1 FROM workspace_operations
        WHERE sequence=NEW.sequence OR id=NEW.id OR (group_id=NEW.group_id AND member_id=NEW.member_id AND idempotency_key=NEW.idempotency_key)
        OR notice_message_id=NEW.notice_message_id)
        OR EXISTS(SELECT 1 FROM workspace_operation_receipts WHERE notice_message_id=NEW.notice_message_id);
    SELECT RAISE(ABORT, '任务或轮次存在未确认文件操作') WHERE EXISTS(
        SELECT 1 FROM workspace_operations o LEFT JOIN workspace_operation_receipts r ON r.operation_id=o.id
        WHERE o.group_id=NEW.group_id AND (o.task_id=NEW.task_id OR o.turn_id=NEW.turn_id)
        AND (r.operation_id IS NULL OR r.outcome IN ('unknown','applied_sync_error')));
    SELECT RAISE(ABORT, '文件操作必须绑定当前工作尝试、目录授权和资源占用') WHERE NOT EXISTS(
        SELECT 1 FROM coordination_assignments a JOIN coordination_assignment_attempts b ON b.assignment_id=a.id
        JOIN turns t ON t.id=b.turn_id JOIN tasks task ON task.id=b.task_id AND task.group_id=b.group_id
        JOIN members m ON m.id=b.worker_member_id AND m.group_id=b.group_id AND m.status='active'
        JOIN chat_workspace_scopes s ON s.task_id=b.task_id AND s.group_id=b.group_id
        JOIN chat_workspace_scope_grants g ON g.task_id=s.task_id AND g.member_id=b.worker_member_id
        JOIN workspace_write_grants w ON w.id=g.grant_id
        JOIN resource_claims rc ON rc.resource_key=s.resource_key AND rc.turn_id=t.id
        WHERE a.id=NEW.assignment_id AND a.task_id=NEW.task_id AND a.group_id=NEW.group_id AND a.worker_member_id=NEW.member_id
        AND a.request_id=s.dispatch_request_id AND b.task_id=a.task_id AND b.group_id=a.group_id AND b.worker_member_id=a.worker_member_id
        AND b.attempt_id=NEW.attempt_id AND b.turn_id=NEW.turn_id AND b.task_revision=NEW.task_revision
        AND s.task_revision=NEW.task_revision AND s.node_id=NEW.node_id AND s.resource_key=NEW.resource_key
        AND g.group_id=NEW.group_id AND g.resource_key=s.resource_key AND g.node_id=s.node_id AND g.grant_id=NEW.grant_id AND g.grant_schema_version=19
        AND w.group_id=NEW.group_id AND w.member_id=NEW.member_id AND w.root=g.root AND w.resolved_root=g.resolved_root
        AND w.resource_key=s.resource_key AND w.node_id=s.node_id AND w.scope='read_write' AND w.authorized_by='user'
        AND t.group_id=NEW.group_id AND t.member_id=NEW.member_id AND t.state IN ('starting','running') AND t.cancel_requested_at IS NULL AND t.candidate_terminal=''
        AND task.status='open' AND task.revision=NEW.task_revision AND task.active_attempt_id=NEW.attempt_id AND task.current_submission_id IS NULL);
    SELECT RAISE(ABORT, '文件操作通知必须是原群话题的公开平台记录') WHERE NOT EXISTS(
        SELECT 1 FROM messages m JOIN message_log l ON l.message_id=m.id
        JOIN chat_workspace_scopes s ON s.task_id=NEW.task_id AND s.group_id=m.group_id AND s.topic_id=m.topic_id
        WHERE m.id=NEW.notice_message_id AND m.group_id=NEW.group_id AND m.sender_id=NEW.member_id
        AND l.group_id=m.group_id AND l.topic_id=m.topic_id
        AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=m.id)
        AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=m.id));
END;
CREATE TRIGGER workspace_operations_no_update BEFORE UPDATE ON workspace_operations
BEGIN SELECT RAISE(ABORT, '文件操作意图不可修改'); END;
CREATE TRIGGER workspace_operations_no_delete BEFORE DELETE ON workspace_operations
BEGIN SELECT RAISE(ABORT, '文件操作意图不可删除'); END;
CREATE TRIGGER workspace_operation_receipts_insert BEFORE INSERT ON workspace_operation_receipts
BEGIN
    SELECT RAISE(ABORT, '文件操作结果不可替换') WHERE EXISTS(SELECT 1 FROM workspace_operation_receipts WHERE operation_id=NEW.operation_id OR notice_message_id=NEW.notice_message_id)
        OR EXISTS(SELECT 1 FROM workspace_operations WHERE notice_message_id=NEW.notice_message_id);
    -- The intent FK and immutable receipt establish causality, not wall-clock order.
    SELECT RAISE(ABORT, '文件操作结果必须绑定原意图及仍有效的执行资源') WHERE NOT EXISTS(
        SELECT 1 FROM workspace_operations o JOIN turns t ON t.id=o.turn_id
        JOIN members m ON m.id=o.member_id AND m.group_id=o.group_id AND m.status='active'
        JOIN tasks task ON task.id=o.task_id AND task.group_id=o.group_id
        JOIN resource_claims rc ON rc.resource_key=o.resource_key AND rc.turn_id=o.turn_id
        WHERE o.id=NEW.operation_id AND t.state IN ('starting','running') AND t.cancel_requested_at IS NULL AND t.candidate_terminal=''
        AND task.status='open' AND task.revision=o.task_revision AND task.active_attempt_id=o.attempt_id AND task.current_submission_id IS NULL
        AND ((NEW.outcome IN ('done','applied_sync_error') AND NEW.observed_hash=o.after_hash AND NEW.observed_size=o.after_size)
        OR (NEW.outcome='unchanged' AND NEW.observed_hash=o.preimage_hash AND NEW.observed_size=o.preimage_size) OR NEW.outcome='unknown'));
    SELECT RAISE(ABORT, '文件操作结果通知必须关联原群话题') WHERE NOT EXISTS(
        SELECT 1 FROM workspace_operations o JOIN chat_workspace_scopes s ON s.task_id=o.task_id
        JOIN messages m ON m.id=NEW.notice_message_id AND m.group_id=o.group_id AND m.topic_id=s.topic_id AND m.sender_id=o.member_id
        JOIN message_log l ON l.message_id=m.id AND l.group_id=m.group_id AND l.topic_id=m.topic_id
        WHERE o.id=NEW.operation_id AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=m.id)
        AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=m.id));
END;
CREATE TRIGGER workspace_operation_receipts_no_update BEFORE UPDATE ON workspace_operation_receipts
BEGIN SELECT RAISE(ABORT, '文件操作结果不可修改'); END;
CREATE TRIGGER workspace_operation_receipts_no_delete BEFORE DELETE ON workspace_operation_receipts
BEGIN SELECT RAISE(ABORT, '文件操作结果不可删除'); END;
`
