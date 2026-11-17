package store

// Write authority is appended separately; the historical read-only DDL and rows
// are unchanged. Both insert directions enforce one immutable grant per member.
const workspaceWriteGrantSchemaV19 = `
CREATE TABLE workspace_write_grants (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    group_id TEXT NOT NULL,
    member_id TEXT NOT NULL,
    root TEXT NOT NULL CHECK(root <> ''),
    resolved_root TEXT NOT NULL CHECK(resolved_root <> ''),
    resource_key TEXT NOT NULL CHECK(resource_key LIKE 'workspace:%'),
    scope TEXT NOT NULL CHECK(scope = 'read_write'),
    node_id TEXT NOT NULL CHECK(node_id = 'local'),
    authorized_by TEXT NOT NULL CHECK(authorized_by = 'user'),
    created_at TEXT NOT NULL,
    FOREIGN KEY(group_id, member_id) REFERENCES members(group_id, id),
    UNIQUE(group_id, member_id)
);
CREATE TRIGGER workspace_write_grants_insert BEFORE INSERT ON workspace_write_grants
WHEN EXISTS(SELECT 1 FROM workspace_write_grants
    WHERE id=NEW.id OR (group_id=NEW.group_id AND member_id=NEW.member_id))
 OR EXISTS(SELECT 1 FROM workspace_grants
    WHERE id=NEW.id OR (group_id=NEW.group_id AND member_id=NEW.member_id))
BEGIN SELECT RAISE(ABORT, '成员目录授权不可重复或替换'); END;
CREATE TRIGGER workspace_grants_exclude_write BEFORE INSERT ON workspace_grants
WHEN EXISTS(SELECT 1 FROM workspace_write_grants
    WHERE id=NEW.id OR (group_id=NEW.group_id AND member_id=NEW.member_id))
BEGIN SELECT RAISE(ABORT, '成员已有读写目录授权'); END;
CREATE TRIGGER workspace_write_grants_no_update BEFORE UPDATE ON workspace_write_grants
BEGIN SELECT RAISE(ABORT, '目录授权回执不可修改'); END;
CREATE TRIGGER workspace_write_grants_no_delete BEFORE DELETE ON workspace_write_grants
BEGIN SELECT RAISE(ABORT, '目录授权回执不可删除'); END;
`
