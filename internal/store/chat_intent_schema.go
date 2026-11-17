package store

const chatIntentSchemaV16 = `
CREATE TABLE chat_policy_revisions (
    group_id TEXT NOT NULL REFERENCES groups(id),
    topic_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK(typeof(revision)='integer' AND revision>0),
    enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
    allow_preparation INTEGER NOT NULL CHECK(allow_preparation IN (0,1)),
    preauthorization_message_id TEXT,
    action TEXT NOT NULL CHECK(action='text_submission'),
    max_admissions INTEGER NOT NULL CHECK(typeof(max_admissions)='integer' AND max_admissions BETWEEN 1 AND 100),
    authorized_by TEXT NOT NULL CHECK(authorized_by='user'),
    PRIMARY KEY(group_id,topic_id,revision),
    CHECK(allow_preparation=0 OR preauthorization_message_id IS NOT NULL),
    FOREIGN KEY(preauthorization_message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id)
);
CREATE TABLE chat_policies (
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    PRIMARY KEY(group_id,topic_id),
    FOREIGN KEY(group_id,topic_id,revision) REFERENCES chat_policy_revisions(group_id,topic_id,revision)
);
CREATE TABLE chat_candidates (
    id TEXT PRIMARY KEY NOT NULL CHECK(id<>''),
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    member_id TEXT NOT NULL,
    turn_id TEXT NOT NULL UNIQUE,
    generation INTEGER NOT NULL CHECK(typeof(generation)='integer' AND generation>0),
    policy_revision INTEGER CHECK(policy_revision IS NULL OR (typeof(policy_revision)='integer' AND policy_revision>0)),
    binding_revision INTEGER NOT NULL CHECK(typeof(binding_revision)='integer' AND binding_revision>=0),
    dispatch_policy_revision INTEGER NOT NULL CHECK(typeof(dispatch_policy_revision)='integer' AND dispatch_policy_revision>=0),
    FOREIGN KEY(message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id),
    FOREIGN KEY(turn_id,group_id,member_id) REFERENCES turns(id,group_id,member_id),
    FOREIGN KEY(group_id,topic_id,policy_revision) REFERENCES chat_policy_revisions(group_id,topic_id,revision)
);
CREATE INDEX chat_candidates_policy ON chat_candidates(group_id,topic_id,policy_revision,id);
CREATE TABLE chat_assessments (
    id TEXT PRIMARY KEY NOT NULL CHECK(id<>''),
    candidate_id TEXT NOT NULL UNIQUE REFERENCES chat_candidates(id),
    decision_id TEXT UNIQUE REFERENCES routing_decisions(id),
    dispatch_request_id TEXT UNIQUE REFERENCES coordination_dispatch_requests(id),
    notice_message_id TEXT REFERENCES messages(id),
    payload BLOB NOT NULL,
    receipt_json TEXT NOT NULL CHECK(json_valid(receipt_json)),
    CHECK((decision_id IS NULL AND dispatch_request_id IS NULL) OR
          (decision_id IS NOT NULL AND dispatch_request_id IS NOT NULL AND notice_message_id IS NOT NULL))
);
CREATE TABLE chat_submission_notices (
    submission_id TEXT PRIMARY KEY NOT NULL REFERENCES task_submissions(id),
    message_id TEXT NOT NULL UNIQUE REFERENCES messages(id)
);
CREATE TRIGGER chat_submission_notices_insert BEFORE INSERT ON chat_submission_notices
WHEN EXISTS(SELECT 1 FROM chat_submission_notices WHERE submission_id=NEW.submission_id OR message_id=NEW.message_id)
BEGIN SELECT RAISE(ABORT, '聊天成果回执不可替换'); END;
CREATE TRIGGER chat_submission_notices_no_update BEFORE UPDATE ON chat_submission_notices
BEGIN SELECT RAISE(ABORT, '聊天成果回执不可修改'); END;
CREATE TRIGGER chat_submission_notices_no_delete BEFORE DELETE ON chat_submission_notices
BEGIN SELECT RAISE(ABORT, '聊天成果回执不可删除'); END;
CREATE TRIGGER chat_policy_revisions_insert BEFORE INSERT ON chat_policy_revisions
BEGIN
    SELECT RAISE(ABORT, '聊天策略版本不可替换') WHERE EXISTS(SELECT 1 FROM chat_policy_revisions
        WHERE group_id=NEW.group_id AND topic_id=NEW.topic_id AND revision=NEW.revision);
    SELECT RAISE(ABORT, '书面预授权必须来自本机用户') WHERE NEW.preauthorization_message_id IS NOT NULL AND NOT EXISTS(
        SELECT 1 FROM messages WHERE id=NEW.preauthorization_message_id AND group_id=NEW.group_id
        AND topic_id=NEW.topic_id AND sender_id='user');
END;
CREATE TRIGGER chat_policy_revisions_no_update BEFORE UPDATE ON chat_policy_revisions
BEGIN SELECT RAISE(ABORT, '聊天策略版本不可修改'); END;
CREATE TRIGGER chat_policy_revisions_no_delete BEFORE DELETE ON chat_policy_revisions
BEGIN SELECT RAISE(ABORT, '聊天策略版本不可删除'); END;
CREATE TRIGGER chat_candidates_insert BEFORE INSERT ON chat_candidates
BEGIN
    SELECT RAISE(ABORT, '聊天候选不可替换') WHERE EXISTS(SELECT 1 FROM chat_candidates WHERE id=NEW.id OR turn_id=NEW.turn_id);
    SELECT RAISE(ABORT, '聊天候选执行来源不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM turns t JOIN deliveries d ON d.id=t.delivery_id
        JOIN conversations c ON c.id=t.conversation_id JOIN message_log l ON l.message_id=d.message_id
        WHERE t.id=NEW.turn_id AND t.group_id=NEW.group_id AND t.member_id=NEW.member_id
        AND d.message_id=NEW.message_id AND c.topic_id=NEW.topic_id AND l.sequence=NEW.generation);
END;
CREATE TRIGGER chat_candidates_no_update BEFORE UPDATE ON chat_candidates
BEGIN SELECT RAISE(ABORT, '聊天候选不可修改'); END;
CREATE TRIGGER chat_candidates_no_delete BEFORE DELETE ON chat_candidates
BEGIN SELECT RAISE(ABORT, '聊天候选不可删除'); END;
CREATE TRIGGER chat_assessments_insert BEFORE INSERT ON chat_assessments
BEGIN
    SELECT RAISE(ABORT, '聊天评估不可替换') WHERE EXISTS(SELECT 1 FROM chat_assessments
        WHERE id=NEW.id OR candidate_id=NEW.candidate_id
        OR (NEW.decision_id IS NOT NULL AND decision_id=NEW.decision_id)
        OR (NEW.dispatch_request_id IS NOT NULL AND dispatch_request_id=NEW.dispatch_request_id));
END;
CREATE TRIGGER chat_assessments_no_update BEFORE UPDATE ON chat_assessments
BEGIN SELECT RAISE(ABORT, '聊天评估不可修改'); END;
CREATE TRIGGER chat_assessments_no_delete BEFORE DELETE ON chat_assessments
BEGIN SELECT RAISE(ABORT, '聊天评估不可删除'); END;
`
