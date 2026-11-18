package store

import "slices"

// Derive the one-based version before historical fixtures truncate the registry.
var taskArtifactDependencySchemaVersion = slices.Index(migrations, taskArtifactDependencySchema) + 1

const taskArtifactDependencySchema = `
CREATE TABLE task_artifact_dependencies (
    group_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    dependency_id TEXT NOT NULL,
    submission_id TEXT NOT NULL,
    dependency_revision INTEGER NOT NULL CHECK(typeof(dependency_revision)='integer' AND dependency_revision>0),
    artifact_id TEXT NOT NULL,
    version_id TEXT NOT NULL,
    ready_when TEXT NOT NULL CHECK(ready_when='accepted'),
    PRIMARY KEY(group_id, task_id, dependency_id),
    FOREIGN KEY(group_id, task_id, dependency_id)
        REFERENCES task_dependencies(group_id, task_id, dependency_id),
    FOREIGN KEY(submission_id, dependency_id, group_id, dependency_revision)
        REFERENCES task_submissions(id, task_id, group_id, task_revision),
    FOREIGN KEY(submission_id, artifact_id, version_id)
        REFERENCES task_submission_artifacts(submission_id, artifact_id, version_id)
) WITHOUT ROWID;
CREATE TRIGGER task_artifact_dependencies_insert BEFORE INSERT ON task_artifact_dependencies
WHEN EXISTS(SELECT 1 FROM task_artifact_dependencies
    WHERE group_id=NEW.group_id AND task_id=NEW.task_id AND dependency_id=NEW.dependency_id)
OR NOT EXISTS(SELECT 1 FROM artifact_versions
    WHERE id=NEW.version_id AND artifact_id=NEW.artifact_id AND group_id=NEW.group_id AND source_task_id=NEW.dependency_id)
BEGIN SELECT RAISE(ABORT, '成果依赖来源无效或已固定'); END;
CREATE TRIGGER task_artifact_dependencies_no_update BEFORE UPDATE ON task_artifact_dependencies
BEGIN SELECT RAISE(ABORT, '成果依赖不可修改'); END;
CREATE TRIGGER task_artifact_dependencies_no_delete BEFORE DELETE ON task_artifact_dependencies
BEGIN SELECT RAISE(ABORT, '成果依赖不可删除'); END;
`
