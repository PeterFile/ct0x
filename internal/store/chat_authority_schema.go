package store

// Append provenance and evaluator evidence; never reinterpret legacy rows.
const chatAuthoritySchemaV17 = `
CREATE INDEX chat_assessments_notice_source ON chat_assessments(notice_message_id);
CREATE INDEX turns_reply_message_source ON turns(reply_message_id) WHERE reply_message_id<>'';
CREATE TABLE message_command_sources (
    id TEXT PRIMARY KEY NOT NULL CHECK(id<>''),
    message_id TEXT NOT NULL UNIQUE REFERENCES messages(id),
    kind TEXT NOT NULL CHECK(kind IN ('local_user_chat','local_user_policy','member')),
    payload BLOB NOT NULL,
    idempotency_key TEXT NOT NULL,
    turn_id TEXT REFERENCES turns(id),
    claim_digest BLOB,
    CHECK((kind='member' AND turn_id IS NOT NULL AND length(claim_digest)=32) OR
          (kind<>'member' AND turn_id IS NULL AND claim_digest IS NULL))
);
CREATE INDEX message_command_sources_kind ON message_command_sources(kind,message_id);
CREATE INDEX message_command_sources_turn ON message_command_sources(turn_id,message_id);
CREATE TABLE chat_policy_scopes (
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    scope_kind TEXT NOT NULL CHECK(scope_kind IN ('','topic_summary')),
    command_source_id TEXT NOT NULL UNIQUE REFERENCES message_command_sources(id),
    PRIMARY KEY(group_id,topic_id,revision),
    FOREIGN KEY(group_id,topic_id,revision) REFERENCES chat_policy_revisions(group_id,topic_id,revision)
);
CREATE TABLE chat_intake_executions (
    candidate_id TEXT PRIMARY KEY NOT NULL REFERENCES chat_candidates(id),
    turn_id TEXT NOT NULL UNIQUE REFERENCES turns(id),
    purpose TEXT NOT NULL CHECK(purpose='chat_intake/v1'),
    claim_digest BLOB NOT NULL CHECK(length(claim_digest)=32),
    source_id TEXT NOT NULL,
    source_kind TEXT NOT NULL CHECK(source_kind IN ('local_user_chat','local_user_policy','member','platform','unknown'))
);
CREATE TRIGGER message_command_sources_insert BEFORE INSERT ON message_command_sources
WHEN EXISTS(SELECT 1 FROM message_command_sources WHERE id=NEW.id OR message_id=NEW.message_id)
BEGIN SELECT RAISE(ABORT, '消息命令来源不可替换'); END;
CREATE TRIGGER message_command_sources_update BEFORE UPDATE ON message_command_sources
BEGIN SELECT RAISE(ABORT, '消息命令来源不可修改'); END;
CREATE TRIGGER message_command_sources_delete BEFORE DELETE ON message_command_sources
BEGIN SELECT RAISE(ABORT, '消息命令来源不可删除'); END;
CREATE TRIGGER chat_policy_scopes_insert BEFORE INSERT ON chat_policy_scopes
BEGIN
 SELECT RAISE(ABORT, '策略范围不可替换') WHERE EXISTS(SELECT 1 FROM chat_policy_scopes
  WHERE group_id=NEW.group_id AND topic_id=NEW.topic_id AND revision=NEW.revision);
 SELECT RAISE(ABORT, '策略范围必须由本机用户配置命令产生') WHERE NOT EXISTS(
  SELECT 1 FROM message_command_sources s JOIN messages m ON m.id=s.message_id
  JOIN chat_policy_revisions p ON p.group_id=m.group_id AND p.topic_id=m.topic_id
  WHERE s.id=NEW.command_source_id AND s.kind='local_user_policy' AND m.sender_id='user'
  AND p.group_id=NEW.group_id AND p.topic_id=NEW.topic_id AND p.revision=NEW.revision
  AND ((p.allow_preparation=0 AND NEW.scope_kind='') OR
       (p.allow_preparation=1 AND NEW.scope_kind='topic_summary' AND p.preauthorization_message_id=m.id)));
END;
CREATE TRIGGER chat_policy_scopes_update BEFORE UPDATE ON chat_policy_scopes
BEGIN SELECT RAISE(ABORT, '策略范围不可修改'); END;
CREATE TRIGGER chat_policy_scopes_delete BEFORE DELETE ON chat_policy_scopes
BEGIN SELECT RAISE(ABORT, '策略范围不可删除'); END;
CREATE TRIGGER chat_intake_executions_insert BEFORE INSERT ON chat_intake_executions
BEGIN
 SELECT RAISE(ABORT, '意图职责不可替换') WHERE EXISTS(SELECT 1 FROM chat_intake_executions
  WHERE candidate_id=NEW.candidate_id OR turn_id=NEW.turn_id);
 SELECT RAISE(ABORT, '意图职责必须绑定实际执行') WHERE NOT EXISTS(
  SELECT 1 FROM chat_candidates c WHERE c.id=NEW.candidate_id AND c.turn_id=NEW.turn_id);
END;
CREATE TRIGGER chat_intake_executions_update BEFORE UPDATE ON chat_intake_executions
BEGIN SELECT RAISE(ABORT, '意图职责不可修改'); END;
CREATE TRIGGER chat_intake_executions_delete BEFORE DELETE ON chat_intake_executions
BEGIN SELECT RAISE(ABORT, '意图职责不可删除'); END;
`
