package store

const artifactSchemaV6 = `
CREATE TABLE artifacts (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    group_id TEXT NOT NULL REFERENCES groups(id),
    name TEXT NOT NULL CHECK(trim(name) <> ''),
    current_version_id TEXT NOT NULL CHECK(current_version_id <> ''),
    created_at TEXT NOT NULL,
    UNIQUE(id, group_id),
    -- Both IDs are generated before insertion; the initial version follows in this transaction.
    FOREIGN KEY(current_version_id, id, group_id)
        REFERENCES artifact_versions(id, artifact_id, group_id) DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE artifact_versions (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    artifact_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    source_task_id TEXT CHECK(source_task_id IS NULL OR source_task_id <> ''),
    author_id TEXT NOT NULL CHECK(trim(author_id) <> ''),
    -- Only the local user bypasses member binding; historical membership need not be active.
    author_member_id TEXT GENERATED ALWAYS AS (nullif(author_id,'user')) VIRTUAL,
    blob_hash TEXT NOT NULL CHECK(typeof(blob_hash)='text' AND length(blob_hash)=64
        AND length(CAST(blob_hash AS BLOB))=64 AND blob_hash NOT GLOB '*[^0-9a-f]*'),
    size INTEGER NOT NULL CHECK(typeof(size)='integer' AND size >= 0),
    expected_version_id TEXT CHECK(expected_version_id IS NULL OR expected_version_id <> ''),
    observed_version_id TEXT CHECK(observed_version_id IS NULL OR observed_version_id <> ''),
    applied INTEGER NOT NULL CHECK(typeof(applied)='integer' AND applied IN (0,1)),
    created_at TEXT NOT NULL,
    UNIQUE(id, artifact_id, group_id),
    FOREIGN KEY(artifact_id, group_id) REFERENCES artifacts(id, group_id),
    FOREIGN KEY(source_task_id, group_id) REFERENCES tasks(id, group_id),
    FOREIGN KEY(group_id, author_member_id) REFERENCES members(group_id, id),
    FOREIGN KEY(expected_version_id, artifact_id, group_id) REFERENCES artifact_versions(id, artifact_id, group_id),
    FOREIGN KEY(observed_version_id, artifact_id, group_id) REFERENCES artifact_versions(id, artifact_id, group_id),
    CHECK((applied=1 AND expected_version_id IS observed_version_id) OR
        (applied=0 AND expected_version_id IS NOT NULL AND observed_version_id IS NOT NULL
            AND expected_version_id <> observed_version_id))
);
-- Explicit insert guards also block REPLACE when recursive delete triggers are disabled.
CREATE TRIGGER artifacts_insert BEFORE INSERT ON artifacts
WHEN EXISTS(SELECT 1 FROM artifacts WHERE id=NEW.id)
BEGIN SELECT RAISE(ABORT, '成果身份不可替换'); END;
CREATE TRIGGER artifact_versions_insert BEFORE INSERT ON artifact_versions
WHEN EXISTS(SELECT 1 FROM artifact_versions WHERE id=NEW.id) OR NOT EXISTS(
    SELECT 1 FROM artifacts WHERE id=NEW.artifact_id AND group_id=NEW.group_id AND
        ((current_version_id=NEW.id AND NEW.applied=1
            AND NEW.expected_version_id IS NULL AND NEW.observed_version_id IS NULL) OR
        (current_version_id<>NEW.id AND NEW.expected_version_id IS NOT NULL
            AND NEW.observed_version_id=current_version_id)))
BEGIN SELECT RAISE(ABORT, '成果版本身份或原有引用无效'); END;
CREATE TRIGGER artifact_versions_no_update BEFORE UPDATE ON artifact_versions
BEGIN SELECT RAISE(ABORT, '成果版本不可修改'); END;
CREATE TRIGGER artifact_versions_no_delete BEFORE DELETE ON artifact_versions
BEGIN SELECT RAISE(ABORT, '成果版本不可删除'); END;
CREATE TRIGGER artifacts_identity BEFORE UPDATE ON artifacts
WHEN NEW.id IS NOT OLD.id OR NEW.group_id IS NOT OLD.group_id
    OR NEW.name IS NOT OLD.name OR NEW.created_at IS NOT OLD.created_at
BEGIN SELECT RAISE(ABORT, '成果身份不可修改'); END;
CREATE TRIGGER artifacts_no_delete BEFORE DELETE ON artifacts
BEGIN SELECT RAISE(ABORT, '成果不可删除'); END;
CREATE TRIGGER artifacts_head_update BEFORE UPDATE OF current_version_id ON artifacts
WHEN NEW.current_version_id IS NOT OLD.current_version_id AND NOT EXISTS(
    SELECT 1 FROM artifact_versions WHERE id=NEW.current_version_id AND artifact_id=OLD.id
        AND group_id=OLD.group_id AND applied=1 AND expected_version_id=OLD.current_version_id
        AND observed_version_id=OLD.current_version_id)
BEGIN SELECT RAISE(ABORT, '成果当前版本引用无效'); END;
CREATE INDEX artifacts_group_created_id ON artifacts(group_id, created_at DESC, id DESC);
CREATE INDEX artifact_versions_group_created_id ON artifact_versions(group_id, artifact_id, created_at DESC, id DESC);
`
