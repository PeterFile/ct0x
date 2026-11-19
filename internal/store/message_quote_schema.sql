CREATE TABLE message_quotes (
    message_id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    quoted_message_id TEXT NOT NULL,
    quoted_topic_id TEXT NOT NULL,
    quoted_source_kind TEXT NOT NULL CHECK(quoted_source_kind IN ('unknown','local_user_chat','local_user_policy','member','platform')),
    excerpt TEXT NOT NULL,
    CHECK(message_id <> quoted_message_id),
    FOREIGN KEY(message_id, group_id, topic_id) REFERENCES messages(id, group_id, topic_id),
    FOREIGN KEY(quoted_message_id, group_id, quoted_topic_id) REFERENCES messages(id, group_id, topic_id)
) WITHOUT ROWID;
CREATE TRIGGER message_quotes_no_replace BEFORE INSERT ON message_quotes
WHEN EXISTS(SELECT 1 FROM message_quotes WHERE message_id=NEW.message_id)
BEGIN SELECT RAISE(ABORT, '消息引用不可替换'); END;
CREATE TRIGGER message_quotes_no_update BEFORE UPDATE ON message_quotes
BEGIN SELECT RAISE(ABORT, '消息引用不可修改'); END;
CREATE TRIGGER message_quotes_no_delete BEFORE DELETE ON message_quotes
BEGIN SELECT RAISE(ABORT, '消息引用不可删除'); END;
