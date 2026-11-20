package store

// Append after chatDiscussionSchema and taskWorkReportSchema. One immutable
// result uses the originating chain's reserved return slot, not a new invite.
const chatDiscussionWorkReturnSchema = `
CREATE TABLE chat_discussion_work_returns (
    origin_message_id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    invitation_message_id TEXT NOT NULL UNIQUE,
    handback_id TEXT NOT NULL UNIQUE REFERENCES task_work_handbacks(id),
    message_id TEXT NOT NULL UNIQUE,
    delivery_id TEXT UNIQUE REFERENCES deliveries(id),
    CHECK(message_id NOT IN (origin_message_id,invitation_message_id)),
    FOREIGN KEY(origin_message_id,group_id,topic_id) REFERENCES chat_discussions(origin_message_id,group_id,topic_id),
    FOREIGN KEY(origin_message_id,invitation_message_id) REFERENCES chat_discussion_events(origin_message_id,message_id),
    FOREIGN KEY(message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id)
) STRICT, WITHOUT ROWID;

CREATE TRIGGER chat_discussion_work_returns_insert BEFORE INSERT ON chat_discussion_work_returns
BEGIN
    SELECT RAISE(ABORT,'讨论工作回传不可替换') WHERE EXISTS(SELECT 1 FROM chat_discussion_work_returns
        WHERE origin_message_id=NEW.origin_message_id OR invitation_message_id=NEW.invitation_message_id
        OR handback_id=NEW.handback_id OR message_id=NEW.message_id OR delivery_id=NEW.delivery_id);
    SELECT RAISE(ABORT,'讨论工作回传来源或投递不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM chat_discussions c
        JOIN members root ON (root.id,root.group_id)=(c.root_inviter_id,c.group_id)
        JOIN chat_discussion_events e ON (e.origin_message_id,e.group_id,e.topic_id)=(c.origin_message_id,c.group_id,c.topic_id)
        JOIN task_work_handbacks h ON (h.id,h.group_id,h.topic_id)=(NEW.handback_id,c.group_id,c.topic_id)
        JOIN mandates m ON (m.id,m.task_id,m.group_id,m.topic_id)=(h.mandate_id,h.task_id,h.group_id,h.topic_id)
        JOIN coordination_triggers t ON (t.id,t.group_id,t.topic_id,t.message_id)=(m.origin_trigger_id,e.group_id,e.topic_id,e.message_id)
        JOIN messages out ON (out.id,out.group_id,out.topic_id,out.sender_id)=(NEW.message_id,e.group_id,e.topic_id,h.owner_member_id)
        WHERE (c.origin_message_id,c.group_id,c.topic_id)=(NEW.origin_message_id,NEW.group_id,NEW.topic_id)
        AND e.message_id=NEW.invitation_message_id AND e.kind='invite' AND out.id<>h.notice_message_id
        AND NOT EXISTS(SELECT 1 FROM chat_discussion_events later WHERE later.origin_message_id=c.origin_message_id AND later.step>e.step)
        AND ((NEW.delivery_id IS NULL AND root.status='left') OR (root.status='active' AND EXISTS(
            SELECT 1 FROM deliveries d WHERE (d.id,d.message_id,d.group_id,d.topic_id,d.recipient_id)=
                (NEW.delivery_id,out.id,out.group_id,out.topic_id,root.id) AND d.state='queued' AND d.turn_id='' AND d.error=''
                AND NOT EXISTS(SELECT 1 FROM turns WHERE delivery_id=d.id))))
    ) OR EXISTS(SELECT 1 FROM deliveries WHERE message_id=NEW.message_id AND id IS NOT NEW.delivery_id)
      OR EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=NEW.message_id);
    SELECT RAISE(ABORT,'讨论工作回传不能作为轮次回复') WHERE EXISTS(
        SELECT 1 FROM turns WHERE reply_message_id=NEW.message_id AND reply_message_id<>'');
END;
CREATE TRIGGER chat_discussion_work_returns_update BEFORE UPDATE ON chat_discussion_work_returns
BEGIN SELECT RAISE(ABORT,'讨论工作回传不可修改'); END;
CREATE TRIGGER chat_discussion_work_returns_delete BEFORE DELETE ON chat_discussion_work_returns
BEGIN SELECT RAISE(ABORT,'讨论工作回传不可删除'); END;

-- This publication consumes the path; it is not a model-authored reply.
CREATE TRIGGER chat_discussion_work_returns_seal BEFORE INSERT ON chat_discussion_events
WHEN EXISTS(SELECT 1 FROM chat_discussion_work_returns WHERE origin_message_id=NEW.origin_message_id)
BEGIN SELECT RAISE(ABORT,'讨论已收到工作回传'); END;
CREATE TRIGGER chat_discussion_work_returns_no_command BEFORE INSERT ON message_command_sources
WHEN EXISTS(SELECT 1 FROM chat_discussion_work_returns WHERE message_id=NEW.message_id)
BEGIN SELECT RAISE(ABORT,'讨论工作回传不是聊天命令'); END;
CREATE TRIGGER chat_discussion_work_returns_no_reply_insert BEFORE INSERT ON turns
WHEN NEW.reply_message_id<>'' AND EXISTS(SELECT 1 FROM chat_discussion_work_returns WHERE message_id=NEW.reply_message_id)
BEGIN SELECT RAISE(ABORT,'讨论工作回传不能作为轮次回复'); END;
CREATE TRIGGER chat_discussion_work_returns_no_reply_update BEFORE UPDATE OF reply_message_id ON turns
WHEN NEW.reply_message_id<>'' AND EXISTS(SELECT 1 FROM chat_discussion_work_returns WHERE message_id=NEW.reply_message_id)
BEGIN SELECT RAISE(ABORT,'讨论工作回传不能作为轮次回复'); END;

-- A return can be ordinary chat, but contradictory source evidence must
-- not give it authority even if queue routing did not use the typed reader.
CREATE TRIGGER chat_discussion_work_returns_no_authority BEFORE INSERT ON chat_intake_executions
WHEN (NEW.source_kind<>'unknown' OR NEW.source_id<>'') AND EXISTS(
    SELECT 1 FROM chat_candidates c JOIN chat_discussion_work_returns w ON w.message_id=c.message_id WHERE c.id=NEW.candidate_id)
BEGIN SELECT RAISE(ABORT,'讨论工作回传不能作为工作准入来源'); END;

-- Keep the publication's delivery set and route immutable. The ordinary queue
-- still owns claims, acceptance and settlement.
CREATE TRIGGER chat_discussion_work_return_deliveries_insert BEFORE INSERT ON deliveries
WHEN EXISTS(SELECT 1 FROM chat_discussion_work_returns WHERE message_id=NEW.message_id OR delivery_id=NEW.id)
BEGIN SELECT RAISE(ABORT,'讨论工作回传投递集合不可修改'); END;
CREATE TRIGGER chat_discussion_work_return_deliveries_identity
BEFORE UPDATE OF id,message_id,group_id,topic_id,conversation_id,recipient_id,created_at ON deliveries
WHEN (NEW.id IS NOT OLD.id OR NEW.message_id IS NOT OLD.message_id OR NEW.group_id IS NOT OLD.group_id
   OR NEW.topic_id IS NOT OLD.topic_id OR NEW.conversation_id IS NOT OLD.conversation_id
   OR NEW.recipient_id IS NOT OLD.recipient_id OR NEW.created_at IS NOT OLD.created_at)
 AND EXISTS(SELECT 1 FROM chat_discussion_work_returns
    WHERE message_id IN (OLD.message_id,NEW.message_id) OR delivery_id=NEW.id)
BEGIN SELECT RAISE(ABORT,'讨论工作回传投递身份不可修改'); END;
CREATE TRIGGER chat_discussion_work_return_deliveries_delete BEFORE DELETE ON deliveries
WHEN EXISTS(SELECT 1 FROM chat_discussion_work_returns WHERE message_id=OLD.message_id)
BEGIN SELECT RAISE(ABORT,'讨论工作回传投递不可删除'); END;
`
