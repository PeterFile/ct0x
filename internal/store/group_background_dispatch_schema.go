package store

// Only trusted new groups receive bindings. No historical message is promoted.
const groupBackgroundDispatchSchema = `
CREATE TABLE group_background_bindings (
    group_id TEXT PRIMARY KEY NOT NULL REFERENCES groups(id)
);
CREATE TABLE group_background_intents (
    sequence INTEGER PRIMARY KEY NOT NULL REFERENCES message_log(sequence),
    message_id TEXT NOT NULL UNIQUE,
    source_id TEXT NOT NULL UNIQUE REFERENCES message_command_sources(id),
    group_id TEXT NOT NULL REFERENCES group_background_bindings(group_id),
    topic_id TEXT NOT NULL,
    delivery_id TEXT UNIQUE REFERENCES deliveries(id),
    recipient_id TEXT,
    route_kind TEXT CHECK(route_kind IN ('existing','preset','create')),
    completed_at TEXT,
    FOREIGN KEY(message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id),
    FOREIGN KEY(group_id,recipient_id) REFERENCES members(group_id,id),
    CHECK((delivery_id IS NULL AND recipient_id IS NULL AND route_kind IS NULL AND completed_at IS NULL) OR
        (delivery_id IS NOT NULL AND recipient_id IS NOT NULL AND route_kind IS NOT NULL AND completed_at IS NOT NULL))
);
CREATE INDEX group_background_pending ON group_background_intents(sequence) WHERE delivery_id IS NULL;
CREATE INDEX group_background_active_profiles ON members(group_id,workspace,provider_id,model,created_at,id) WHERE status='active';
CREATE TABLE group_background_joins (
    member_id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    origin_message_id TEXT NOT NULL UNIQUE REFERENCES group_background_intents(message_id),
    notice_message_id TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL CHECK(kind IN ('preset','create')),
    template_id TEXT NOT NULL CHECK(template_id<>''),
    provider_id TEXT NOT NULL CHECK(provider_id<>''),
    model TEXT NOT NULL CHECK(model<>''),
    FOREIGN KEY(group_id,member_id) REFERENCES members(group_id,id),
    FOREIGN KEY(notice_message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id)
);
CREATE UNIQUE INDEX group_background_preset_membership ON group_background_joins(group_id,template_id) WHERE kind='preset';
CREATE TRIGGER group_background_intent_insert BEFORE INSERT ON group_background_intents
WHEN NEW.delivery_id IS NOT NULL OR EXISTS(SELECT 1 FROM group_background_intents
    WHERE sequence=NEW.sequence OR message_id=NEW.message_id OR source_id=NEW.source_id) OR NOT EXISTS(
    SELECT 1 FROM message_log l JOIN messages m ON m.id=l.message_id
    JOIN message_command_sources s ON s.message_id=m.id
    WHERE l.sequence=NEW.sequence AND m.id=NEW.message_id AND m.group_id=NEW.group_id AND m.topic_id=NEW.topic_id
      AND l.group_id=m.group_id AND l.topic_id=m.topic_id AND m.sender_id='user'
      AND s.id=NEW.source_id AND s.kind='local_user_chat' AND s.turn_id IS NULL AND s.claim_digest IS NULL
      AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=m.id))
BEGIN SELECT RAISE(ABORT,'后台调度意图必须绑定原始用户消息'); END;
CREATE TRIGGER group_background_intent_finish BEFORE UPDATE ON group_background_intents
WHEN OLD.delivery_id IS NOT NULL OR NEW.sequence IS NOT OLD.sequence OR NEW.message_id IS NOT OLD.message_id
 OR NEW.source_id IS NOT OLD.source_id OR NEW.group_id IS NOT OLD.group_id OR NEW.topic_id IS NOT OLD.topic_id OR NOT EXISTS(
    SELECT 1 FROM deliveries d WHERE d.id=NEW.delivery_id AND d.message_id=OLD.message_id
      AND d.group_id=OLD.group_id AND d.topic_id=OLD.topic_id AND d.recipient_id=NEW.recipient_id
      AND (NEW.route_kind='existing' OR EXISTS(SELECT 1 FROM group_background_joins j
        WHERE j.origin_message_id=OLD.message_id AND j.member_id=d.recipient_id AND j.kind=NEW.route_kind)))
BEGIN SELECT RAISE(ABORT,'后台路由结果必须绑定原消息投递'); END;
CREATE TRIGGER group_background_intent_delete BEFORE DELETE ON group_background_intents
BEGIN SELECT RAISE(ABORT,'后台路由记录不可删除'); END;
CREATE TRIGGER group_background_join_insert BEFORE INSERT ON group_background_joins
WHEN EXISTS(SELECT 1 FROM group_background_joins WHERE member_id=NEW.member_id OR origin_message_id=NEW.origin_message_id
    OR notice_message_id=NEW.notice_message_id OR (NEW.kind='preset' AND kind='preset' AND group_id=NEW.group_id AND template_id=NEW.template_id))
 OR NOT EXISTS(SELECT 1 FROM group_background_intents i
    JOIN members w ON w.id=NEW.member_id AND w.group_id=i.group_id
    JOIN messages e ON e.id=NEW.notice_message_id AND e.group_id=i.group_id AND e.topic_id=i.topic_id
    WHERE i.message_id=NEW.origin_message_id AND i.group_id=NEW.group_id AND i.topic_id=NEW.topic_id
      AND i.delivery_id IS NULL AND w.status='active' AND w.creator_id='system' AND w.owner_id='user' AND w.workspace=''
      AND w.provider_id=NEW.provider_id AND w.model=NEW.model AND e.sender_id='platformnotice'
      AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=e.id))
BEGIN SELECT RAISE(ABORT,'后台入群来源或事件无效'); END;
CREATE TRIGGER group_background_join_update BEFORE UPDATE ON group_background_joins
BEGIN SELECT RAISE(ABORT,'后台入群来源不可修改'); END;
CREATE TRIGGER group_background_join_delete BEFORE DELETE ON group_background_joins
BEGIN SELECT RAISE(ABORT,'后台入群来源不可删除'); END;
CREATE TRIGGER group_background_delivery_insert BEFORE INSERT ON deliveries
WHEN (EXISTS(SELECT 1 FROM group_background_intents WHERE message_id=NEW.message_id)
      AND EXISTS(SELECT 1 FROM deliveries WHERE message_id=NEW.message_id))
 OR EXISTS(SELECT 1 FROM group_background_joins WHERE notice_message_id=NEW.message_id)
BEGIN SELECT RAISE(ABORT,'后台原消息只投递一次，入群事件不投递'); END;
`
