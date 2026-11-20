-- B-owned registration: requires the message/delivery identity, Artifact version,
-- and immutable local-user command source schemas (v3/v4, v6, v8, v17).
-- No backfill: only a new LocalUser message can carry a grant.
CREATE TABLE delivery_artifact_read_grants (
    delivery_id TEXT PRIMARY KEY NOT NULL,
    message_id TEXT NOT NULL UNIQUE,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    recipient_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    version_id TEXT NOT NULL,
    FOREIGN KEY(delivery_id, group_id, conversation_id, recipient_id)
        REFERENCES deliveries(id, group_id, conversation_id, recipient_id),
    FOREIGN KEY(message_id, group_id, topic_id) REFERENCES messages(id, group_id, topic_id),
    FOREIGN KEY(message_id) REFERENCES message_command_sources(message_id),
    FOREIGN KEY(version_id, artifact_id, group_id) REFERENCES artifact_versions(id, artifact_id, group_id)
) WITHOUT ROWID;

CREATE TRIGGER delivery_artifact_read_grants_insert BEFORE INSERT ON delivery_artifact_read_grants
BEGIN
    SELECT RAISE(ABORT, '投递成果读取许可不可替换') WHERE EXISTS(
        SELECT 1 FROM delivery_artifact_read_grants WHERE delivery_id=NEW.delivery_id OR message_id=NEW.message_id);
    SELECT RAISE(ABORT, '成果读取许可必须绑定本机用户消息的单个待投递记录') WHERE NOT EXISTS(
        SELECT 1 FROM deliveries d
        JOIN messages m ON (m.id,m.group_id,m.topic_id)=(d.message_id,d.group_id,d.topic_id)
        JOIN message_command_sources s ON s.message_id=m.id AND s.kind='local_user_chat'
        WHERE (d.id,d.message_id,d.group_id,d.topic_id,d.conversation_id,d.recipient_id)=
            (NEW.delivery_id,NEW.message_id,NEW.group_id,NEW.topic_id,NEW.conversation_id,NEW.recipient_id)
        AND m.sender_id='user' AND d.state='queued' AND d.turn_id=''
        AND NOT EXISTS(SELECT 1 FROM deliveries other WHERE other.message_id=d.message_id AND other.id<>d.id));
END;
CREATE TRIGGER delivery_artifact_read_grants_update BEFORE UPDATE ON delivery_artifact_read_grants
BEGIN SELECT RAISE(ABORT, '投递成果读取许可不可修改'); END;
CREATE TRIGGER delivery_artifact_read_grants_delete BEFORE DELETE ON delivery_artifact_read_grants
BEGIN SELECT RAISE(ABORT, '投递成果读取许可不可删除'); END;
