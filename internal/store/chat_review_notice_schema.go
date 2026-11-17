package store

// Append only the source relationship. Legacy Reviews are never backfilled.
const chatReviewNoticeSchemaV18 = `
CREATE TABLE chat_review_notices (
    review_id TEXT PRIMARY KEY NOT NULL REFERENCES task_review_decisions(id),
    message_id TEXT NOT NULL UNIQUE REFERENCES messages(id)
);
CREATE TRIGGER chat_review_notices_insert BEFORE INSERT ON chat_review_notices
BEGIN
    SELECT RAISE(ABORT, '聊天评审通知不可替换') WHERE EXISTS(
        SELECT 1 FROM chat_review_notices WHERE review_id=NEW.review_id OR message_id=NEW.message_id);
    SELECT RAISE(ABORT, '聊天评审通知必须关联用户评审及原群话题的平台消息') WHERE NOT EXISTS(
        SELECT 1 FROM task_review_decisions r
        JOIN mandates m INDEXED BY mandates_group_task ON m.group_id=r.group_id AND m.task_id=r.task_id
        JOIN chat_assessments a ON a.decision_id=m.decision_id
        JOIN chat_candidates c ON c.id=a.candidate_id AND c.group_id=m.group_id AND c.topic_id=m.topic_id
        JOIN messages msg ON msg.id=NEW.message_id AND msg.group_id=r.group_id AND msg.topic_id=c.topic_id
        JOIN message_log l ON l.message_id=msg.id AND l.group_id=msg.group_id AND l.topic_id=msg.topic_id
        WHERE r.id=NEW.review_id AND r.reviewed_by='user' AND msg.sender_id='user'
        AND NOT EXISTS(SELECT 1 FROM message_command_sources s WHERE s.message_id=msg.id));
END;
CREATE TRIGGER chat_review_notices_no_update BEFORE UPDATE ON chat_review_notices
BEGIN SELECT RAISE(ABORT, '聊天评审通知不可修改'); END;
CREATE TRIGGER chat_review_notices_no_delete BEFORE DELETE ON chat_review_notices
BEGIN SELECT RAISE(ABORT, '聊天评审通知不可删除'); END;
`
