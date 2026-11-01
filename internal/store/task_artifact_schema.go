package store

const taskArtifactSchemaV7 = `
CREATE TABLE task_submission_artifacts (
    submission_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    task_revision INTEGER NOT NULL CHECK(typeof(task_revision)='integer' AND task_revision > 0),
    artifact_id TEXT NOT NULL,
    version_id TEXT NOT NULL,
    PRIMARY KEY(submission_id, artifact_id, version_id),
    FOREIGN KEY(submission_id, task_id, group_id, task_revision)
        REFERENCES task_submissions(id, task_id, group_id, task_revision),
    FOREIGN KEY(version_id, artifact_id, group_id)
        REFERENCES artifact_versions(id, artifact_id, group_id)
);
-- Only the initial submit transaction still owns this open generation/attempt.
CREATE TRIGGER task_submission_artifacts_insert BEFORE INSERT ON task_submission_artifacts
WHEN EXISTS(SELECT 1 FROM task_submission_artifacts
    WHERE submission_id=NEW.submission_id AND artifact_id=NEW.artifact_id AND version_id=NEW.version_id)
OR NOT EXISTS(
    SELECT 1 FROM task_submissions s JOIN tasks t ON t.id=s.task_id AND t.group_id=s.group_id
    WHERE s.id=NEW.submission_id AND s.task_id=NEW.task_id AND s.group_id=NEW.group_id
        AND s.task_revision=NEW.task_revision AND t.revision=s.task_revision
        AND t.status='open' AND t.active_attempt_id=s.attempt_id AND t.current_submission_id IS NULL
)
BEGIN SELECT RAISE(ABORT, '提交成果引用只能随当前提交创建且不可替换'); END;
CREATE TRIGGER task_submission_artifacts_no_update BEFORE UPDATE ON task_submission_artifacts
BEGIN SELECT RAISE(ABORT, '提交成果引用不可修改'); END;
CREATE TRIGGER task_submission_artifacts_no_delete BEFORE DELETE ON task_submission_artifacts
BEGIN SELECT RAISE(ABORT, '提交成果引用不可删除'); END;
CREATE INDEX task_attempts_group_task_created_id ON task_attempts(group_id, task_id, created_at DESC, id DESC);
CREATE INDEX task_submissions_group_task_created_id ON task_submissions(group_id, task_id, created_at DESC, id DESC);
CREATE INDEX task_reviews_group_task_created_id ON task_review_decisions(group_id, task_id, created_at DESC, id DESC);
`
