package store

// Registered separately by the migration coordinator, after threadRoutingSchema.
// Only the topical overlay is new; old tuples retain their physical scope.
const threadCorrectionSchema = `
CREATE INDEX thread_correction_message_scope ON messages(group_id,topic_id,created_at DESC,id DESC);
CREATE TABLE thread_corrections (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT CHECK(sequence>0),
    group_id TEXT NOT NULL REFERENCES groups(id),
    command_message_id TEXT NOT NULL UNIQUE REFERENCES messages(id),
    target_message_id TEXT,
    original_topic_id TEXT,
    previous_topic_id TEXT,
    effective_topic_id TEXT,
    question TEXT NOT NULL DEFAULT '',
    CHECK((question<>'' AND target_message_id IS NULL AND original_topic_id IS NULL AND previous_topic_id IS NULL AND effective_topic_id IS NULL)
       OR (question='' AND target_message_id IS NOT NULL AND target_message_id<>command_message_id
           AND original_topic_id IS NOT NULL AND previous_topic_id IS NOT NULL AND effective_topic_id IS NOT NULL AND effective_topic_id<>'')),
    FOREIGN KEY(target_message_id,group_id,original_topic_id) REFERENCES messages(id,group_id,topic_id),
    FOREIGN KEY(group_id,effective_topic_id) REFERENCES thread_routing_topics(group_id,topic_id)
);
CREATE INDEX thread_corrections_latest ON thread_corrections(group_id,target_message_id,sequence DESC)
    WHERE previous_topic_id<>effective_topic_id;
CREATE INDEX thread_corrections_incoming ON thread_corrections(group_id,effective_topic_id,target_message_id,sequence)
    WHERE previous_topic_id<>effective_topic_id;
CREATE TRIGGER thread_corrections_insert BEFORE INSERT ON thread_corrections
BEGIN
    SELECT RAISE(ABORT, '话题纠正记录不可替换') WHERE EXISTS(
        SELECT 1 FROM thread_corrections WHERE command_message_id=NEW.command_message_id OR sequence=NEW.sequence);
    SELECT RAISE(ABORT, '话题纠正须来自本群本机用户且不投递') WHERE NOT EXISTS(
        SELECT 1 FROM messages m WHERE (m.id,m.group_id,m.topic_id,m.sender_id)=(NEW.command_message_id,NEW.group_id,'','user')
        AND NOT EXISTS(SELECT 1 FROM deliveries d WHERE d.message_id=m.id)
        AND NOT EXISTS(SELECT 1 FROM message_command_sources s WHERE s.message_id=m.id));
    SELECT RAISE(ABORT, '话题纠正必须指向此前原文及其当前归属') WHERE NEW.target_message_id IS NOT NULL AND NOT EXISTS(
        SELECT 1 FROM message_log target JOIN message_log command ON command.message_id=NEW.command_message_id
        WHERE target.message_id=NEW.target_message_id AND target.group_id=NEW.group_id AND target.sequence<command.sequence
        AND NEW.previous_topic_id=coalesce((SELECT c.effective_topic_id FROM thread_corrections c
            WHERE c.group_id=NEW.group_id AND c.target_message_id=NEW.target_message_id
            AND c.previous_topic_id<>c.effective_topic_id ORDER BY c.sequence DESC LIMIT 1),NEW.original_topic_id));
END;
CREATE TRIGGER thread_corrections_update BEFORE UPDATE ON thread_corrections
BEGIN SELECT RAISE(ABORT, '话题纠正不可修改'); END;
CREATE TRIGGER thread_corrections_delete BEFORE DELETE ON thread_corrections
BEGIN SELECT RAISE(ABORT, '话题纠正不可删除'); END;
`
