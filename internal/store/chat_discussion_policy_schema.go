package store

const chatDiscussionPolicySchema = `
CREATE TABLE chat_discussion_policy_revisions (
    group_id TEXT NOT NULL REFERENCES groups(id),
    topic_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK(typeof(revision)='integer' AND revision>0),
    max_invite_hops INTEGER NOT NULL CHECK(typeof(max_invite_hops)='integer' AND max_invite_hops BETWEEN 0 AND 2),
    PRIMARY KEY(group_id,topic_id,revision)
);
-- A conflicting REPLACE must not delete the original revision, even when
-- recursive triggers are disabled.
CREATE TRIGGER chat_discussion_policy_revisions_insert BEFORE INSERT ON chat_discussion_policy_revisions
WHEN NEW.revision IS NOT (SELECT coalesce(max(revision),0)+1 FROM chat_discussion_policy_revisions
    WHERE group_id=NEW.group_id AND topic_id=NEW.topic_id)
BEGIN SELECT RAISE(ABORT,'讨论策略必须追加新版本'); END;
CREATE TRIGGER chat_discussion_policy_revisions_update BEFORE UPDATE ON chat_discussion_policy_revisions
BEGIN SELECT RAISE(ABORT,'讨论策略版本不可修改'); END;
CREATE TRIGGER chat_discussion_policy_revisions_delete BEFORE DELETE ON chat_discussion_policy_revisions
BEGIN SELECT RAISE(ABORT,'讨论策略版本不可删除'); END;
`
