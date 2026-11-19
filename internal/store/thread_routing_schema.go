package store

// threadRoutingSchema is registered by the migration coordinator. It does not
// backfill names from legacy chat or change any legacy scope or position.
const threadRoutingSchema = `
CREATE TABLE thread_routing_topics (
    group_id TEXT NOT NULL REFERENCES groups(id),
    topic_id TEXT NOT NULL CHECK(topic_id<>''),
    title TEXT NOT NULL CHECK(title<>''),
    title_key TEXT NOT NULL CHECK(title_key<>''),
    issue_id TEXT NOT NULL,
    origin_message_id TEXT NOT NULL,
    PRIMARY KEY(group_id, topic_id),
    UNIQUE(group_id, title_key),
    FOREIGN KEY(origin_message_id, group_id, topic_id) REFERENCES messages(id, group_id, topic_id)
);
CREATE UNIQUE INDEX thread_routing_issue ON thread_routing_topics(group_id, issue_id) WHERE issue_id<>'';
CREATE TABLE thread_routing_links (
    message_id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    reply_to_message_id TEXT,
    reply_to_topic_id TEXT,
    task_id TEXT,
    CHECK((reply_to_message_id IS NULL)=(reply_to_topic_id IS NULL)),
    CHECK(reply_to_message_id IS NULL OR (reply_to_message_id<>'' AND reply_to_message_id<>message_id)),
    CHECK(task_id IS NOT NULL OR reply_to_message_id IS NOT NULL),
    FOREIGN KEY(message_id, group_id, topic_id) REFERENCES messages(id, group_id, topic_id),
    FOREIGN KEY(reply_to_message_id, group_id, reply_to_topic_id) REFERENCES messages(id, group_id, topic_id),
    FOREIGN KEY(task_id, group_id) REFERENCES tasks(id, group_id)
);
CREATE TRIGGER thread_routing_links_insert BEFORE INSERT ON thread_routing_links
WHEN EXISTS(SELECT 1 FROM thread_routing_links WHERE message_id=NEW.message_id)
BEGIN SELECT RAISE(ABORT, '消息引用不可替换'); END;
CREATE TRIGGER thread_routing_links_update BEFORE UPDATE ON thread_routing_links
BEGIN SELECT RAISE(ABORT, '消息引用不可修改'); END;
CREATE TRIGGER thread_routing_links_delete BEFORE DELETE ON thread_routing_links
BEGIN SELECT RAISE(ABORT, '消息引用不可删除'); END;
`
