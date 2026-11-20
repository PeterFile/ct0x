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

-- Block both a second delivery and replacement by message, delivery ID or
-- rowid, even when recursive delete triggers are disabled. Grant insertion
-- itself takes place after the one allowed delivery is created.
CREATE TRIGGER delivery_artifact_read_grant_deliveries_insert BEFORE INSERT ON deliveries
WHEN EXISTS(SELECT 1 FROM delivery_artifact_read_grants WHERE delivery_id=NEW.id OR message_id=NEW.message_id)
 OR EXISTS(SELECT 1 FROM deliveries d JOIN delivery_artifact_read_grants g ON g.delivery_id=d.id WHERE d.rowid=NEW.rowid)
BEGIN SELECT RAISE(ABORT, '成果读取许可的投递集合不可修改'); END;

-- Check both sides: no moving the authorized delivery away or moving an
-- existing delivery into the authorized message. Queue state and turn writes
-- do not change identity and remain on the usual lifecycle path.
CREATE TRIGGER delivery_artifact_read_grant_deliveries_identity
BEFORE UPDATE OF rowid,_rowid_,oid,id,message_id,group_id,topic_id,conversation_id,recipient_id ON deliveries
WHEN (NEW.rowid IS NOT OLD.rowid OR NEW.id IS NOT OLD.id OR NEW.message_id IS NOT OLD.message_id
 OR NEW.group_id IS NOT OLD.group_id OR NEW.topic_id IS NOT OLD.topic_id
 OR NEW.conversation_id IS NOT OLD.conversation_id OR NEW.recipient_id IS NOT OLD.recipient_id)
 AND (EXISTS(SELECT 1 FROM delivery_artifact_read_grants
    WHERE delivery_id IN (OLD.id,NEW.id) OR message_id IN (OLD.message_id,NEW.message_id))
 OR EXISTS(SELECT 1 FROM deliveries d JOIN delivery_artifact_read_grants g ON g.delivery_id=d.id WHERE d.rowid=NEW.rowid))
BEGIN SELECT RAISE(ABORT, '成果读取许可的投递身份不可修改'); END;
CREATE TRIGGER delivery_artifact_read_grant_deliveries_delete BEFORE DELETE ON deliveries
WHEN EXISTS(SELECT 1 FROM delivery_artifact_read_grants WHERE delivery_id=OLD.id)
BEGIN SELECT RAISE(ABORT, '成果读取许可的投递不可删除'); END;
