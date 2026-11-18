package store

const chatDiscussionRecipientLeftReason = "接收成员已离开，讨论交接未启动"

// Topics are opaque within a group; the composite message foreign keys bind
// all evidence to the exact topic (including the main topic's empty ID).
const chatDiscussionSchema = `
CREATE TABLE chat_discussions (
    origin_message_id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL REFERENCES groups(id),
    topic_id TEXT NOT NULL,
    root_inviter_id TEXT NOT NULL,
    max_invite_hops INTEGER NOT NULL CHECK(typeof(max_invite_hops)='integer' AND max_invite_hops BETWEEN 0 AND 32),
    max_targeted_wakes INTEGER NOT NULL CHECK(typeof(max_targeted_wakes)='integer' AND max_targeted_wakes BETWEEN 0 AND 64),
    UNIQUE(origin_message_id,group_id,topic_id),
    FOREIGN KEY(origin_message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id),
    FOREIGN KEY(group_id,root_inviter_id) REFERENCES members(group_id,id)
);
CREATE TABLE chat_discussion_events (
    message_id TEXT PRIMARY KEY NOT NULL,
    origin_message_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    step INTEGER NOT NULL CHECK(typeof(step)='integer' AND step>0),
    source_message_id TEXT NOT NULL UNIQUE,
    candidate_id TEXT NOT NULL UNIQUE REFERENCES chat_assessments(candidate_id),
    kind TEXT NOT NULL,
    actor_id TEXT NOT NULL,
    recipient_id TEXT,
    delivery_id TEXT UNIQUE REFERENCES deliveries(id),
    related_invitation_id TEXT,
    -- An invite without a refinement uses the immutable question at message_id.
    expected_reply TEXT NOT NULL,
    message_json TEXT NOT NULL CHECK(json_valid(message_json)),
    UNIQUE(origin_message_id,step),
    UNIQUE(origin_message_id,message_id),
    FOREIGN KEY(origin_message_id,group_id,topic_id) REFERENCES chat_discussions(origin_message_id,group_id,topic_id),
    FOREIGN KEY(message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id),
    FOREIGN KEY(source_message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id),
    FOREIGN KEY(origin_message_id,related_invitation_id) REFERENCES chat_discussion_events(origin_message_id,message_id),
    FOREIGN KEY(group_id,actor_id) REFERENCES members(group_id,id),
    FOREIGN KEY(group_id,recipient_id) REFERENCES members(group_id,id),
    CHECK((kind='invite' AND recipient_id IS NOT NULL AND delivery_id IS NOT NULL) OR
          (kind='reply' AND recipient_id IS NOT NULL AND expected_reply='') OR
          (kind='resolve' AND recipient_id IS NULL AND delivery_id IS NULL AND expected_reply='')),
    CHECK((step=1 AND source_message_id=origin_message_id AND related_invitation_id IS NULL AND kind<>'reply') OR
          (step>1 AND source_message_id<>origin_message_id AND related_invitation_id IS NOT NULL))
);
CREATE TRIGGER chat_discussions_insert BEFORE INSERT ON chat_discussions
BEGIN
    SELECT RAISE(ABORT,'讨论来源不可替换') WHERE EXISTS(
        SELECT 1 FROM chat_discussions WHERE origin_message_id=NEW.origin_message_id);
    SELECT RAISE(ABORT,'讨论必须源自本机用户消息') WHERE NOT EXISTS(
        SELECT 1 FROM messages m JOIN message_command_sources s ON s.message_id=m.id
        WHERE m.id=NEW.origin_message_id AND m.sender_id='user' AND s.kind='local_user_chat');
END;
CREATE TRIGGER chat_discussions_update BEFORE UPDATE ON chat_discussions
BEGIN SELECT RAISE(ABORT,'讨论来源与预算不可修改'); END;
CREATE TRIGGER chat_discussions_delete BEFORE DELETE ON chat_discussions
BEGIN SELECT RAISE(ABORT,'讨论来源不可删除'); END;
CREATE TRIGGER chat_discussion_events_insert BEFORE INSERT ON chat_discussion_events
BEGIN
    SELECT RAISE(ABORT,'讨论事件不可替换') WHERE EXISTS(SELECT 1 FROM chat_discussion_events
        WHERE message_id=NEW.message_id OR source_message_id=NEW.source_message_id OR candidate_id=NEW.candidate_id
        OR delivery_id=NEW.delivery_id OR (origin_message_id=NEW.origin_message_id AND step=NEW.step));
    SELECT RAISE(ABORT,'讨论事件必须接续同一讨论') WHERE NEW.step>1 AND NOT EXISTS(
        SELECT 1 FROM chat_discussion_events WHERE origin_message_id=NEW.origin_message_id
        AND step=NEW.step-1 AND message_id=NEW.source_message_id);
    SELECT RAISE(ABORT,'讨论事件缺少绑定的命令来源') WHERE NOT EXISTS(
        SELECT 1 FROM chat_candidates c JOIN chat_intake_executions i ON i.candidate_id=c.id AND i.turn_id=c.turn_id
        JOIN messages m ON m.id=NEW.message_id AND m.sender_id=c.member_id
        JOIN message_command_sources s ON s.message_id=m.id AND s.kind='member' AND s.turn_id=c.turn_id AND s.claim_digest=i.claim_digest
        WHERE c.id=NEW.candidate_id AND c.message_id=NEW.source_message_id AND c.member_id=NEW.actor_id
        AND c.group_id=NEW.group_id AND c.topic_id=NEW.topic_id);
    SELECT RAISE(ABORT,'讨论事件投递目标不符') WHERE EXISTS(
        SELECT 1 FROM deliveries d WHERE d.message_id=NEW.message_id AND d.id IS NOT NEW.delivery_id)
        OR (NEW.delivery_id IS NOT NULL AND NOT EXISTS(
            SELECT 1 FROM deliveries d WHERE d.id=NEW.delivery_id AND d.message_id=NEW.message_id AND d.recipient_id=NEW.recipient_id));
END;
CREATE TRIGGER chat_discussion_events_update BEFORE UPDATE ON chat_discussion_events
BEGIN SELECT RAISE(ABORT,'讨论事件不可修改'); END;
CREATE TRIGGER chat_discussion_events_delete BEFORE DELETE ON chat_discussion_events
BEGIN SELECT RAISE(ABORT,'讨论事件不可删除'); END;

-- Match either unique-key victim before REPLACE can delete it, even with
-- recursive_triggers=OFF. This also keeps public event target sets empty.
CREATE TRIGGER chat_discussion_deliveries_insert BEFORE INSERT ON deliveries
WHEN EXISTS(SELECT 1 FROM chat_discussion_events WHERE message_id=NEW.message_id)
  OR EXISTS(SELECT 1 FROM chat_discussion_events WHERE delivery_id=NEW.id)
BEGIN SELECT RAISE(ABORT,'讨论事件投递集合不可修改'); END;
-- UPDATE OF excludes routine queue writes; check both sides to forbid moving
-- into or out of an event, as well as overwriting an event's delivery by ID.
CREATE TRIGGER chat_discussion_deliveries_identity
BEFORE UPDATE OF id,message_id,group_id,topic_id,conversation_id,recipient_id,created_at ON deliveries
WHEN (NEW.id IS NOT OLD.id OR NEW.message_id IS NOT OLD.message_id OR NEW.group_id IS NOT OLD.group_id
   OR NEW.topic_id IS NOT OLD.topic_id OR NEW.conversation_id IS NOT OLD.conversation_id
   OR NEW.recipient_id IS NOT OLD.recipient_id OR NEW.created_at IS NOT OLD.created_at)
 AND (EXISTS(SELECT 1 FROM chat_discussion_events WHERE message_id=OLD.message_id)
   OR EXISTS(SELECT 1 FROM chat_discussion_events WHERE message_id=NEW.message_id)
   OR EXISTS(SELECT 1 FROM chat_discussion_events WHERE delivery_id=NEW.id))
BEGIN SELECT RAISE(ABORT,'讨论事件投递身份不可修改'); END;
CREATE TRIGGER chat_discussion_deliveries_delete BEFORE DELETE ON deliveries
WHEN EXISTS(SELECT 1 FROM chat_discussion_events WHERE message_id=OLD.message_id)
BEGIN SELECT RAISE(ABORT,'讨论事件投递不可删除'); END;

CREATE INDEX chat_discussion_events_recipient_delivery ON chat_discussion_events(group_id,recipient_id,delivery_id)
    WHERE delivery_id IS NOT NULL;
CREATE TRIGGER chat_discussion_recipient_departed AFTER UPDATE OF status ON members
WHEN OLD.status<>'left' AND NEW.status='left'
BEGIN
    -- No claim or acceptance is fabricated. Once claimed, only the existing
    -- Turn lifecycle may settle; ordinary pending deliveries are unaffected.
    UPDATE deliveries SET state='failed',error='` + chatDiscussionRecipientLeftReason + `'
    WHERE state='queued' AND turn_id='' AND id IN (
        SELECT delivery_id FROM chat_discussion_events
        WHERE group_id=NEW.group_id AND recipient_id=NEW.id AND delivery_id IS NOT NULL)
    AND NOT EXISTS(SELECT 1 FROM turns WHERE delivery_id=deliveries.id);
END;
`
