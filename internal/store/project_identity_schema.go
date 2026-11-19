package store

const projectIdentitySchema = `
CREATE TABLE workspaces (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    name TEXT NOT NULL,
    is_default INTEGER NOT NULL DEFAULT 0 CHECK(is_default IN (0, 1))
) WITHOUT ROWID;
CREATE UNIQUE INDEX workspaces_default ON workspaces(is_default) WHERE is_default=1;

CREATE TABLE projects (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    -- Provenance for default creation, not the ownership or authorization relation.
    legacy_origin_group_id TEXT UNIQUE REFERENCES groups(id),
    CHECK(id <> legacy_origin_group_id)
) WITHOUT ROWID;
CREATE INDEX projects_workspace_created_id ON projects(workspace_id, created_at DESC, id DESC);

CREATE TABLE channels (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    project_id TEXT NOT NULL REFERENCES projects(id),
    legacy_group_id TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    CHECK(id <> legacy_group_id),
    -- Allow an explicit Channel before its Group in one transaction. RESTRICT
    -- also blocks REPLACE's implicit delete of a mapped Group.
    FOREIGN KEY(legacy_group_id) REFERENCES groups(id)
        ON DELETE RESTRICT ON UPDATE RESTRICT DEFERRABLE INITIALLY DEFERRED
) WITHOUT ROWID;
CREATE INDEX channels_project_created_id ON channels(project_id, created_at DESC, id DESC);

INSERT INTO workspaces(id, name, is_default) VALUES(lower(hex(randomblob(16))), '本地工作区', 1);
INSERT INTO projects(id, workspace_id, name, description, created_at, legacy_origin_group_id)
    SELECT lower(hex(randomblob(16))), (SELECT id FROM workspaces WHERE is_default=1),
        g.name, g.description, g.created_at, g.id FROM groups g;
INSERT INTO channels(id, project_id, legacy_group_id, name, description, created_at)
    SELECT lower(hex(randomblob(16))), (SELECT p.id FROM projects p WHERE p.legacy_origin_group_id=g.id),
        g.id, g.name, g.description, g.created_at FROM groups g;

-- Guard every uniqueness key, since REPLACE can bypass DELETE triggers.
CREATE TRIGGER workspaces_no_replace BEFORE INSERT ON workspaces
WHEN EXISTS(SELECT 1 FROM workspaces WHERE id=NEW.id)
    OR (NEW.is_default=1 AND EXISTS(SELECT 1 FROM workspaces WHERE is_default=1))
BEGIN SELECT RAISE(ABORT, '工作区身份不可替换'); END;
CREATE TRIGGER workspaces_no_rebind BEFORE UPDATE OF id, is_default ON workspaces
WHEN NEW.id IS NOT OLD.id OR NEW.is_default IS NOT OLD.is_default
BEGIN SELECT RAISE(ABORT, '工作区身份不可修改'); END;
CREATE TRIGGER workspaces_no_delete BEFORE DELETE ON workspaces
BEGIN SELECT RAISE(ABORT, '工作区不可删除'); END;

CREATE TRIGGER projects_no_replace BEFORE INSERT ON projects
WHEN EXISTS(SELECT 1 FROM projects WHERE id=NEW.id OR legacy_origin_group_id=NEW.legacy_origin_group_id)
BEGIN SELECT RAISE(ABORT, '项目身份不可替换'); END;
CREATE TRIGGER projects_no_rebind BEFORE UPDATE OF id, workspace_id, legacy_origin_group_id ON projects
WHEN NEW.id IS NOT OLD.id OR NEW.workspace_id IS NOT OLD.workspace_id
    OR NEW.legacy_origin_group_id IS NOT OLD.legacy_origin_group_id
BEGIN SELECT RAISE(ABORT, '项目归属不可修改'); END;
CREATE TRIGGER projects_no_delete BEFORE DELETE ON projects
BEGIN SELECT RAISE(ABORT, '项目不可删除'); END;

CREATE TRIGGER channels_no_replace BEFORE INSERT ON channels
WHEN EXISTS(SELECT 1 FROM channels WHERE id=NEW.id OR legacy_group_id=NEW.legacy_group_id)
BEGIN SELECT RAISE(ABORT, '频道身份不可替换'); END;
CREATE TRIGGER channels_no_rebind BEFORE UPDATE OF id, project_id, legacy_group_id ON channels
WHEN NEW.id IS NOT OLD.id OR NEW.project_id IS NOT OLD.project_id OR NEW.legacy_group_id IS NOT OLD.legacy_group_id
BEGIN SELECT RAISE(ABORT, '频道归属不可修改'); END;
CREATE TRIGGER channels_no_delete BEFORE DELETE ON channels
BEGIN SELECT RAISE(ABORT, '频道不可删除'); END;

CREATE TRIGGER groups_project_identity_insert AFTER INSERT ON groups
BEGIN
    INSERT INTO projects(id, workspace_id, name, description, created_at, legacy_origin_group_id)
        SELECT lower(hex(randomblob(16))), (SELECT id FROM workspaces WHERE is_default=1),
            NEW.name, NEW.description, NEW.created_at, NEW.id
        WHERE NOT EXISTS(SELECT 1 FROM channels WHERE legacy_group_id=NEW.id);
    INSERT INTO channels(id, project_id, legacy_group_id, name, description, created_at)
        SELECT lower(hex(randomblob(16))), (SELECT id FROM projects WHERE legacy_origin_group_id=NEW.id),
            NEW.id, NEW.name, NEW.description, NEW.created_at
        WHERE NOT EXISTS(SELECT 1 FROM channels WHERE legacy_group_id=NEW.id);
    -- A skipped nested INSERT must not commit the Group or a partial chain.
    SELECT RAISE(ABORT, '群组归属记录不完整') WHERE NOT EXISTS(
        SELECT 1 FROM channels c
        JOIN projects p ON p.id=c.project_id
        JOIN workspaces w ON w.id=p.workspace_id
        WHERE c.legacy_group_id=NEW.id
    );
END;
`
