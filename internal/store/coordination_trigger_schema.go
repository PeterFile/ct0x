package store

const coordinationTriggerSchemaV9 = `
CREATE TABLE coordination_triggers (
    id TEXT PRIMARY KEY NOT NULL CHECK(id <> ''),
    source_kind TEXT NOT NULL CHECK(source_kind='local_im'),
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    message_sequence INTEGER NOT NULL UNIQUE CHECK(typeof(message_sequence)='integer' AND message_sequence > 0),
    created_at TEXT NOT NULL,
    UNIQUE(source_kind, message_id),
    FOREIGN KEY(message_id, group_id, topic_id) REFERENCES messages(id, group_id, topic_id),
    FOREIGN KEY(message_sequence) REFERENCES message_log(sequence)
);
CREATE TRIGGER coordination_triggers_insert BEFORE INSERT ON coordination_triggers
BEGIN
    SELECT RAISE(ABORT, '协调触发记录不可替换') WHERE EXISTS(
        SELECT 1 FROM coordination_triggers WHERE id=NEW.id
            OR (source_kind=NEW.source_kind AND message_id=NEW.message_id)
            OR message_sequence=NEW.message_sequence);
    SELECT RAISE(ABORT, '消息序号与来源不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM message_log WHERE sequence=NEW.message_sequence AND message_id=NEW.message_id
            AND group_id=NEW.group_id AND topic_id=NEW.topic_id);
END;
CREATE TRIGGER coordination_triggers_no_update BEFORE UPDATE ON coordination_triggers
BEGIN SELECT RAISE(ABORT, '协调触发记录不可修改'); END;
CREATE TRIGGER coordination_triggers_no_delete BEFORE DELETE ON coordination_triggers
BEGIN SELECT RAISE(ABORT, '协调触发记录不可删除'); END;
CREATE INDEX coordination_triggers_thread_created_id ON coordination_triggers(group_id, topic_id, created_at, id);
`
