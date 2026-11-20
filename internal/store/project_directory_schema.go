package store

// projectDirectorySchema follows the legacy group ownership migration. Directory
// selection is metadata only; neither its path nor its node grants access.
const projectDirectorySchema = `
CREATE TABLE project_directories (
    project_id TEXT PRIMARY KEY NOT NULL REFERENCES projects(id),
    path TEXT NOT NULL CHECK(path <> ''),
    node_id TEXT NOT NULL DEFAULT 'local' CHECK(node_id = 'local')
);
`
