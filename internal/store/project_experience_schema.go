package store

// Append after D's projectIdentitySchema in actual release order.
const projectExperienceSchema = `
SELECT w.id,p.id,c.id,c.legacy_group_id FROM channels c
 JOIN projects p ON p.id=c.project_id JOIN workspaces w ON w.id=p.workspace_id LIMIT 0;

CREATE TABLE project_experiences (
 id TEXT PRIMARY KEY NOT NULL CHECK(id<>''),
 workspace_id TEXT NOT NULL REFERENCES workspaces(id),
 project_id TEXT NOT NULL REFERENCES projects(id),
 channel_id TEXT NOT NULL REFERENCES channels(id),
 group_id TEXT NOT NULL REFERENCES groups(id),
 topic_id TEXT NOT NULL,
 problem TEXT NOT NULL CHECK(trim(problem)<>''),
 decision TEXT NOT NULL CHECK(trim(decision)<>''),
 component TEXT NOT NULL CHECK(trim(component)<>''),
 applicable_version TEXT NOT NULL CHECK(trim(applicable_version)<>''),
 conditions TEXT NOT NULL,
 original_message_id TEXT NOT NULL REFERENCES message_log(message_id),
 confirmation_message_id TEXT REFERENCES message_log(message_id),
 review_id TEXT REFERENCES task_review_decisions(id),
 submission_id TEXT,
 task_id TEXT,
 artifact_id TEXT,
 version_id TEXT,
 confirmed_at TEXT NOT NULL,
 selected_at TEXT NOT NULL,
 UNIQUE(id,group_id,topic_id),
 FOREIGN KEY(original_message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id),
 FOREIGN KEY(confirmation_message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id),
 FOREIGN KEY(submission_id,artifact_id,version_id) REFERENCES task_submission_artifacts(submission_id,artifact_id,version_id),
 CHECK((confirmation_message_id IS NOT NULL AND review_id IS NULL AND submission_id IS NULL
        AND task_id IS NULL AND artifact_id IS NULL AND version_id IS NULL) OR
       (confirmation_message_id IS NULL AND review_id IS NOT NULL AND submission_id IS NOT NULL
        AND task_id IS NOT NULL AND artifact_id IS NOT NULL AND version_id IS NOT NULL))
);
CREATE INDEX project_experiences_scope ON project_experiences(group_id,project_id,topic_id,selected_at DESC,id DESC);

CREATE TABLE project_experience_retirements (
 id TEXT PRIMARY KEY NOT NULL CHECK(id<>''),
 experience_id TEXT NOT NULL UNIQUE,
 group_id TEXT NOT NULL,
 topic_id TEXT NOT NULL,
 message_id TEXT NOT NULL REFERENCES message_log(message_id),
 replacement_version TEXT NOT NULL,
 review_id TEXT REFERENCES task_review_decisions(id),
 submission_id TEXT,
 task_id TEXT,
 artifact_id TEXT,
 version_id TEXT,
 retired_at TEXT NOT NULL,
 FOREIGN KEY(experience_id,group_id,topic_id) REFERENCES project_experiences(id,group_id,topic_id),
 FOREIGN KEY(message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id),
 FOREIGN KEY(submission_id,artifact_id,version_id) REFERENCES task_submission_artifacts(submission_id,artifact_id,version_id),
 CHECK((review_id IS NULL AND submission_id IS NULL AND task_id IS NULL AND artifact_id IS NULL AND version_id IS NULL) OR
       (review_id IS NOT NULL AND submission_id IS NOT NULL AND task_id IS NOT NULL AND artifact_id IS NOT NULL
        AND version_id IS NOT NULL AND trim(replacement_version)<>''))
);

-- The witness has its own lifetime. Losing the retirement detail must not make
-- the original record look as though it never retired.
CREATE TABLE project_experience_retirement_witnesses (
 experience_id TEXT PRIMARY KEY NOT NULL REFERENCES project_experiences(id),
 receipt TEXT NOT NULL
) WITHOUT ROWID;

CREATE TRIGGER project_experiences_no_replace BEFORE INSERT ON project_experiences
WHEN EXISTS(SELECT 1 FROM project_experiences WHERE id=NEW.id)
BEGIN SELECT RAISE(ABORT, '经验原始记录不可替换'); END;
CREATE TRIGGER project_experiences_no_update BEFORE UPDATE ON project_experiences
BEGIN SELECT RAISE(ABORT, '经验原始记录不可修改'); END;
CREATE TRIGGER project_experiences_no_delete BEFORE DELETE ON project_experiences
BEGIN SELECT RAISE(ABORT, '经验原始记录不可删除'); END;
CREATE TRIGGER project_experience_retirements_no_replace BEFORE INSERT ON project_experience_retirements
WHEN EXISTS(SELECT 1 FROM project_experience_retirements WHERE id=NEW.id OR experience_id=NEW.experience_id)
BEGIN SELECT RAISE(ABORT, '经验停用记录不可替换'); END;
CREATE TRIGGER project_experience_retirements_no_update BEFORE UPDATE ON project_experience_retirements
BEGIN SELECT RAISE(ABORT, '经验停用记录不可修改'); END;
CREATE TRIGGER project_experience_retirements_no_delete BEFORE DELETE ON project_experience_retirements
BEGIN SELECT RAISE(ABORT, '经验停用记录不可删除'); END;
CREATE TRIGGER project_experience_retirement_witnesses_no_replace BEFORE INSERT ON project_experience_retirement_witnesses
WHEN EXISTS(SELECT 1 FROM project_experience_retirement_witnesses WHERE experience_id=NEW.experience_id)
BEGIN SELECT RAISE(ABORT, '经验停用见证不可替换'); END;
CREATE TRIGGER project_experience_retirement_witnesses_no_update BEFORE UPDATE ON project_experience_retirement_witnesses
BEGIN SELECT RAISE(ABORT, '经验停用见证不可修改'); END;
CREATE TRIGGER project_experience_retirement_witnesses_no_delete BEFORE DELETE ON project_experience_retirement_witnesses
BEGIN SELECT RAISE(ABORT, '经验停用见证不可删除'); END;

-- A sender label alone is never confirmation. A legacy message may still be
-- the original proposal, but even a real chat needs an explicit selection write.
CREATE VIEW project_experience_message_evidence AS
 SELECT m.id,m.group_id,m.topic_id,l.sequence,m.content,
        coalesce(m.sender_id='user' AND src.kind='local_user_chat',0) AS local_user_chat
 FROM message_log l JOIN messages m ON (m.id,m.group_id,m.topic_id)=(l.message_id,l.group_id,l.topic_id)
 LEFT JOIN message_command_sources src ON src.message_id=m.id;

-- This is a read-time witness of the same immutable tuple checked on write.
-- Current task state and Artifact head are deliberately not consulted.
CREATE VIEW project_experience_accepted_evidence AS
 SELECT r.id AS review_id,r.group_id,r.task_id,r.submission_id,ref.artifact_id,ref.version_id,r.created_at
 FROM task_review_decisions r
 JOIN task_submissions s ON (s.id,s.group_id,s.task_id,s.task_revision)=(r.submission_id,r.group_id,r.task_id,r.task_revision)
 JOIN tasks t ON (t.id,t.group_id)=(s.task_id,s.group_id)
 JOIN task_submission_artifacts ref ON (ref.submission_id,ref.group_id,ref.task_id,ref.task_revision)=(s.id,s.group_id,s.task_id,s.task_revision)
 JOIN artifact_versions v ON (v.id,v.artifact_id,v.group_id)=(ref.version_id,ref.artifact_id,ref.group_id)
 JOIN artifacts a ON (a.id,a.group_id)=(v.artifact_id,v.group_id)
 WHERE r.outcome='accepted' AND r.reviewed_by='user';
`
