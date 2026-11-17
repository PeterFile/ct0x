package store

const workerArtifactSchemaV21 = `
CREATE TABLE worker_artifact_publications (
    version_id TEXT PRIMARY KEY NOT NULL REFERENCES artifact_versions(id),
    operation_id TEXT NOT NULL REFERENCES workspace_operations(id),
    group_id TEXT NOT NULL,
    member_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL CHECK(length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 256 AND trim(idempotency_key)=idempotency_key),
    requested_operation_id TEXT NOT NULL,
    requested_relative_path TEXT NOT NULL,
    requested_artifact_id TEXT NOT NULL,
    requested_name TEXT NOT NULL CHECK(length(CAST(requested_name AS BLOB))<=1024),
    requested_expected_version_id TEXT NOT NULL,
    blob_hash TEXT NOT NULL CHECK(length(blob_hash)=64 AND length(CAST(blob_hash AS BLOB))=64 AND blob_hash NOT GLOB '*[^0-9a-f]*'),
    size INTEGER NOT NULL CHECK(typeof(size)='integer' AND size BETWEEN 0 AND 16777216),
    created_at TEXT NOT NULL CHECK(length(CAST(created_at AS BLOB))=30 AND julianday(created_at) IS NOT NULL),
    UNIQUE(group_id,member_id,idempotency_key),
    FOREIGN KEY(group_id,member_id) REFERENCES members(group_id,id),
    CHECK((requested_operation_id<>'' AND requested_relative_path='') OR (requested_operation_id='' AND requested_relative_path<>'')),
    CHECK((requested_artifact_id='' AND trim(requested_name)<>'' AND requested_expected_version_id='')
        OR (requested_artifact_id<>'' AND requested_name='' AND requested_expected_version_id<>''))
);
CREATE INDEX worker_artifact_publications_operation ON worker_artifact_publications(operation_id,version_id);
CREATE TRIGGER worker_artifact_publications_insert BEFORE INSERT ON worker_artifact_publications
BEGIN
    SELECT RAISE(ABORT, '成员成果回执不可替换') WHERE EXISTS(SELECT 1 FROM worker_artifact_publications
        WHERE version_id=NEW.version_id OR (group_id=NEW.group_id AND member_id=NEW.member_id AND idempotency_key=NEW.idempotency_key));
    SELECT RAISE(ABORT, '成员成果必须来自本工作尝试已同步完成的文件操作') WHERE NOT EXISTS(
        SELECT 1 FROM workspace_operations o JOIN workspace_operation_receipts r ON r.operation_id=o.id AND r.outcome='done'
        JOIN artifact_versions v ON v.id=NEW.version_id JOIN artifacts a ON a.id=v.artifact_id AND a.group_id=v.group_id
        JOIN turns t ON t.id=o.turn_id JOIN members m ON m.id=o.member_id AND m.group_id=o.group_id AND m.status='active'
        JOIN tasks task ON task.id=o.task_id AND task.group_id=o.group_id
        JOIN resource_claims rc ON rc.resource_key=o.resource_key AND rc.turn_id=o.turn_id
        WHERE o.id=NEW.operation_id AND o.group_id=NEW.group_id AND o.member_id=NEW.member_id
        AND v.group_id=o.group_id AND v.source_task_id=o.task_id AND v.author_id=o.member_id
        AND v.blob_hash=o.after_hash AND v.size=o.after_size AND v.blob_hash=NEW.blob_hash AND v.size=NEW.size
        AND r.observed_hash=o.after_hash AND r.observed_size=o.after_size
        AND coalesce(v.expected_version_id,'')=NEW.requested_expected_version_id
        AND ((NEW.requested_artifact_id='' AND a.name=NEW.requested_name AND v.expected_version_id IS NULL)
            OR NEW.requested_artifact_id=v.artifact_id)
        AND ((NEW.requested_operation_id=o.id AND NEW.requested_relative_path='')
            OR (NEW.requested_operation_id='' AND NEW.requested_relative_path=o.relative_path AND NOT EXISTS(
                SELECT 1 FROM workspace_operations later WHERE later.group_id=o.group_id AND later.task_id=o.task_id
                AND later.turn_id=o.turn_id AND later.relative_path=o.relative_path AND later.sequence>o.sequence)))
        AND t.state IN ('starting','running') AND t.cancel_requested_at IS NULL AND t.candidate_terminal=''
        AND task.status='open' AND task.revision=o.task_revision AND task.active_attempt_id=o.attempt_id AND task.current_submission_id IS NULL
        AND NOT EXISTS(SELECT 1 FROM workspace_operations pending LEFT JOIN workspace_operation_receipts pr ON pr.operation_id=pending.id
            WHERE pending.group_id=o.group_id AND (pending.task_id=o.task_id OR pending.turn_id=o.turn_id)
            AND (pr.operation_id IS NULL OR pr.outcome IN ('unknown','applied_sync_error'))));
END;
CREATE TRIGGER worker_artifact_publications_no_update BEFORE UPDATE ON worker_artifact_publications
BEGIN SELECT RAISE(ABORT, '成员成果回执不可修改'); END;
CREATE TRIGGER worker_artifact_publications_no_delete BEFORE DELETE ON worker_artifact_publications
BEGIN SELECT RAISE(ABORT, '成员成果回执不可删除'); END;
`
