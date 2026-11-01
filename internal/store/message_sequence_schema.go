package store

const messageSequenceSchemaV8 = `
CREATE TABLE message_log (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT CHECK(sequence > 0),
    message_id TEXT NOT NULL UNIQUE,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    FOREIGN KEY(message_id, group_id, topic_id) REFERENCES messages(id, group_id, topic_id)
);
-- Deterministic bootstrap order only: pre-v8 commit chronology cannot be recovered.
INSERT INTO message_log(message_id, group_id, topic_id)
    SELECT id, group_id, topic_id FROM messages ORDER BY created_at, id;
-- Committed positions increase, but need not be consecutive; rolled-back values may be reused.
-- Only committed positions are durable coverage, not delivery acknowledgements or a full event stream.
CREATE TRIGGER messages_append_log AFTER INSERT ON messages
BEGIN
    INSERT INTO message_log(message_id, group_id, topic_id) VALUES(NEW.id, NEW.group_id, NEW.topic_id);
END;
-- Explicit insert guards block REPLACE even with recursive delete triggers disabled.
CREATE TRIGGER messages_insert BEFORE INSERT ON messages
WHEN EXISTS(SELECT 1 FROM messages WHERE id=NEW.id)
BEGIN SELECT RAISE(ABORT, '消息身份不可替换'); END;
CREATE TRIGGER messages_identity BEFORE UPDATE ON messages
WHEN NEW.id IS NOT OLD.id OR NEW.group_id IS NOT OLD.group_id OR NEW.topic_id IS NOT OLD.topic_id
    OR NEW.sender_id IS NOT OLD.sender_id OR NEW.content IS NOT OLD.content
BEGIN SELECT RAISE(ABORT, '消息身份、范围、发送者和内容不可修改'); END;
-- created_at remains a mutable display timestamp, never a durable coverage position.
CREATE TRIGGER messages_no_delete BEFORE DELETE ON messages
BEGIN SELECT RAISE(ABORT, '消息不可删除'); END;
CREATE TRIGGER message_log_insert BEFORE INSERT ON message_log
WHEN EXISTS(SELECT 1 FROM message_log WHERE message_id=NEW.message_id OR sequence=NEW.sequence)
BEGIN SELECT RAISE(ABORT, '消息序号映射不可替换'); END;
CREATE TRIGGER message_log_no_update BEFORE UPDATE ON message_log
BEGIN SELECT RAISE(ABORT, '消息序号映射不可修改'); END;
CREATE TRIGGER message_log_no_delete BEFORE DELETE ON message_log
BEGIN SELECT RAISE(ABORT, '消息序号映射不可删除'); END;
CREATE INDEX message_log_group_sequence ON message_log(group_id, sequence);
CREATE INDEX message_log_topic_sequence ON message_log(group_id, topic_id, sequence);
`
