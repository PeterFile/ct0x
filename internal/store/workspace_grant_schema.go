package store

const workspaceGrantSchemaV15 = `
CREATE TABLE workspace_grants (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    group_id TEXT NOT NULL,
    member_id TEXT NOT NULL,
    root TEXT NOT NULL CHECK(root <> ''),
    resolved_root TEXT NOT NULL CHECK(resolved_root <> ''),
    resource_key TEXT NOT NULL CHECK(resource_key LIKE 'workspace:%'),
    scope TEXT NOT NULL CHECK(scope = 'read_only'),
    node_id TEXT NOT NULL CHECK(node_id = 'local'),
    authorized_by TEXT NOT NULL CHECK(authorized_by = 'user'),
    created_at TEXT NOT NULL,
    FOREIGN KEY(group_id, member_id) REFERENCES members(group_id, id),
    UNIQUE(group_id, member_id)
);
CREATE TRIGGER workspace_grants_no_update BEFORE UPDATE ON workspace_grants
BEGIN SELECT RAISE(ABORT, '目录授权回执不可修改'); END;
CREATE TRIGGER workspace_grants_no_delete BEFORE DELETE ON workspace_grants
BEGIN SELECT RAISE(ABORT, '目录授权回执不可删除'); END;
`
