package store

// Match the immutable asking assessment, not the live view, latest version, or
// answer ledger. Supersession and departure itself can hide a pending question.
// Withdrawal is request-level, so it fences all versions.
const decisionThreadQuestionAsker = `(SELECT c.member_id FROM chat_assessments a JOIN chat_candidates c ON c.id=a.candidate_id
    WHERE (a.id,c.group_id,c.topic_id,c.message_id,c.generation)=
          (decision_requests.chat_assessment_id,decision_requests.group_id,decision_requests.topic_id,
           decision_requests.source_message_id,decision_requests.source_sequence))`

const decisionThreadWithdraw = `UPDATE decision_requests
    SET withdrawn_at=max(created_at,strftime('%Y-%m-%dT%H:%M:%f','now')||'000000Z')
    WHERE withdrawn_at IS NULL AND kind='clarification' AND relay='origin_thread' AND task_id IS NULL`

// Append after decisionRequestSchema and decisionAnswerSchema. B assigns the
// public migration slot. Installation only stamps legacy Thread withdrawals;
// Task requests and historical content are untouched.
const decisionThreadAnswerSchema = `
CREATE VIEW decision_thread_answerable AS
SELECT q.id AS request_id,v.version,v.expires_at,q.group_id,q.topic_id,
    c.member_id AS asker_id,c.turn_id AS asker_turn_id,rt.conversation_id,
    q.source_message_id,q.source_sequence,n.id AS notice_message_id,n.content AS question
FROM decision_requests q
JOIN decision_request_versions v ON v.request_id=q.id AND v.version=(SELECT max(version) FROM decision_request_versions WHERE request_id=q.id)
JOIN chat_assessments a ON a.id=q.chat_assessment_id
JOIN chat_candidates c ON c.id=a.candidate_id
` + decisionRequestChatJoins + `
JOIN members asker ON (asker.id,asker.group_id,asker.status)=(c.member_id,q.group_id,'active')
LEFT JOIN chat_policies p ON (p.group_id,p.topic_id)=(q.group_id,q.topic_id)
WHERE q.kind='clarification' AND q.relay='origin_thread' AND q.task_id IS NULL AND q.recipient_id='user'
AND q.withdrawn_at IS NULL AND v.author_attempt_id IS NULL
AND c.member_id<>'user' AND p.revision IS c.policy_revision
AND ` + decisionRequestChatProof + `
AND ` + decisionRequestChatUnadvanced + `
AND (source.group_id,source.topic_id,source.sender_id)=(q.group_id,q.topic_id,'user')
AND NOT EXISTS(SELECT 1 FROM deliveries extra WHERE extra.message_id=source.id AND extra.id<>rd.id)
AND n.content=json_extract(v.question_json,'$.question')
AND (json_extract(a.receipt_json,'$.id'),json_extract(a.receipt_json,'$.candidate_id'),
    json_extract(a.receipt_json,'$.group_id'),json_extract(a.receipt_json,'$.topic_id'),
    json_extract(a.receipt_json,'$.turn_id'),json_extract(a.receipt_json,'$.assessed_by'),
    json_extract(a.receipt_json,'$.message_id'),json_extract(a.receipt_json,'$.source_id'),
    json_extract(a.receipt_json,'$.candidate_generation'),json_extract(a.receipt_json,'$.policy_revision'),
    json_extract(a.receipt_json,'$.notice_message_id'),json_extract(a.receipt_json,'$.disposition'))=
    (a.id,c.id,q.group_id,q.topic_id,c.turn_id,c.member_id,source.id,s.id,c.generation,coalesce(c.policy_revision,0),n.id,'clarify')
-- Compare the exact length-prefixed MessageRequest payload in hex (byte lengths,
-- not character counts). The fourth field is the literal 'explicit'.
AND length(CAST(source.content AS BLOB))<=65536 AND length(s.payload)<=69632
AND hex(s.payload)=(printf('%016X',length(CAST(q.group_id AS BLOB)))||hex(q.group_id)||
    printf('%016X',length(CAST(q.topic_id AS BLOB)))||hex(q.topic_id)||
    printf('%016X',length(CAST(source.content AS BLOB)))||hex(source.content)||'00000000000000086578706C69636974'||
    printf('%016X',length(CAST(c.member_id AS BLOB)))||hex(c.member_id));

CREATE TABLE decision_thread_answers (
    id TEXT PRIMARY KEY NOT NULL CHECK(instr(id,'` + threadDecisionAnswerPrefix + `')=1),
    request_id TEXT NOT NULL UNIQUE,
    version INTEGER NOT NULL,
    option_index INTEGER CHECK(option_index BETWEEN 1 AND 4),
    reply_message_id TEXT NOT NULL UNIQUE REFERENCES messages(id) CHECK(instr(reply_message_id,'` + threadDecisionAnswerPrefix + `')=1),
    delivery_id TEXT NOT NULL UNIQUE REFERENCES deliveries(id) CHECK(instr(delivery_id,'` + threadDecisionAnswerPrefix + `')=1),
    created_at TEXT NOT NULL CHECK(length(created_at)=30),
    UNIQUE(id,delivery_id),
    FOREIGN KEY(request_id,version) REFERENCES decision_request_versions(request_id,version)
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX turns_thread_decision_delivery_identity ON turns(id,delivery_id);
CREATE TABLE decision_thread_answer_consumptions (
    answer_id TEXT PRIMARY KEY NOT NULL,
    delivery_id TEXT NOT NULL,
    turn_id TEXT NOT NULL UNIQUE,
    call_key TEXT NOT NULL CHECK(length(CAST(call_key AS BLOB)) BETWEEN 1 AND 128
        AND instr(call_key,char(0))=0 AND call_key NOT GLOB '*[^!-~]*' AND instr(call_key,',')=0),
    consumed_at TEXT NOT NULL CHECK(length(consumed_at)=30),
    FOREIGN KEY(answer_id,delivery_id) REFERENCES decision_thread_answers(id,delivery_id),
    FOREIGN KEY(turn_id,delivery_id) REFERENCES turns(id,delivery_id)
) STRICT, WITHOUT ROWID;

CREATE TRIGGER decision_thread_answer_consumptions_insert BEFORE INSERT ON decision_thread_answer_consumptions
BEGIN
    SELECT RAISE(ABORT,'话题答复处理回执不可替换') WHERE EXISTS(SELECT 1 FROM decision_thread_answer_consumptions
        WHERE answer_id=NEW.answer_id OR turn_id=NEW.turn_id);
    SELECT RAISE(ABORT,'话题答复处理回执与当前认领不匹配') WHERE NOT EXISTS(
        SELECT 1` + decisionThreadWakeRelations + `
        JOIN turns t ON (t.id,t.delivery_id,t.group_id,t.conversation_id,t.member_id)=
            (d.turn_id,d.id,d.group_id,d.conversation_id,d.recipient_id)
        WHERE (ans.id,d.id,t.id)=(NEW.answer_id,NEW.delivery_id,NEW.turn_id)
        AND t.id<>r.asker_turn_id AND d.state='accepted' AND ` + decisionThreadAcceptedTurn + `
        AND NEW.consumed_at>=ans.created_at AND NEW.consumed_at>=t.accepted_at
        AND (r.expires_at IS NULL OR r.expires_at>NEW.consumed_at)
        AND ` + decisionThreadWakeExclusive + `);
END;
CREATE TRIGGER decision_thread_answer_consumptions_no_update BEFORE UPDATE ON decision_thread_answer_consumptions
BEGIN SELECT RAISE(ABORT,'话题答复处理回执不可修改'); END;
CREATE TRIGGER decision_thread_answer_consumptions_no_delete BEFORE DELETE ON decision_thread_answer_consumptions
BEGIN SELECT RAISE(ABORT,'话题答复处理回执不可删除'); END;

CREATE TRIGGER decision_thread_answers_insert BEFORE INSERT ON decision_thread_answers
BEGIN
    SELECT RAISE(ABORT,'已保存的话题答复不可替换') WHERE EXISTS(SELECT 1 FROM decision_thread_answers
        WHERE id=NEW.id OR request_id=NEW.request_id OR reply_message_id=NEW.reply_message_id OR delivery_id=NEW.delivery_id);
    SELECT RAISE(ABORT,'话题答复与原提问或接收人不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM decision_thread_answerable r
        JOIN decision_request_versions v ON (v.request_id,v.version)=(r.request_id,r.version)
        JOIN messages m ON (m.id,m.group_id,m.topic_id,m.sender_id)=(NEW.reply_message_id,r.group_id,r.topic_id,'user')
        JOIN message_log l ON (l.message_id,l.group_id,l.topic_id)=(m.id,r.group_id,r.topic_id)
        JOIN deliveries d ON (d.id,d.message_id,d.group_id,d.topic_id,d.recipient_id,d.conversation_id)=
            (NEW.delivery_id,m.id,r.group_id,r.topic_id,r.asker_id,r.conversation_id)
        WHERE (r.request_id,r.version)=(NEW.request_id,NEW.version)
        AND (r.expires_at IS NULL OR r.expires_at>NEW.created_at)
        AND l.sequence>r.source_sequence AND m.created_at=NEW.created_at
        AND length(CAST(m.content AS BLOB)) BETWEEN 1 AND 4096 AND instr(m.content,char(0))=0
        AND (NEW.option_index IS NULL OR m.content IS json_extract(v.question_json,'$.choices['||(NEW.option_index-1)||']'))
        AND d.state='queued' AND d.turn_id=''
        AND NOT EXISTS(SELECT 1 FROM turns WHERE delivery_id=d.id)
        AND NOT EXISTS(SELECT 1 FROM deliveries other WHERE other.message_id=m.id AND other.id<>d.id)
        AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=m.id)
        AND NOT EXISTS(SELECT 1 FROM coordination_triggers WHERE message_id=m.id));
END;
CREATE TRIGGER decision_thread_answers_no_update BEFORE UPDATE ON decision_thread_answers
BEGIN SELECT RAISE(ABORT,'已保存的话题答复不可修改'); END;
CREATE TRIGGER decision_thread_answers_no_delete BEFORE DELETE ON decision_thread_answers
BEGIN SELECT RAISE(ABORT,'已保存的话题答复不可删除'); END;

CREATE TRIGGER decision_thread_delivery_insert BEFORE INSERT ON deliveries
WHEN EXISTS(SELECT 1 FROM decision_thread_answers WHERE reply_message_id=NEW.message_id OR delivery_id=NEW.id)
BEGIN SELECT RAISE(ABORT,'话题答复投递不可替换或增发'); END;
CREATE TRIGGER decision_thread_delivery_identity
BEFORE UPDATE OF id,message_id,group_id,topic_id,conversation_id,recipient_id,turn_id ON deliveries
WHEN (NEW.id,NEW.message_id,NEW.group_id,NEW.topic_id,NEW.conversation_id,NEW.recipient_id)
       IS NOT (OLD.id,OLD.message_id,OLD.group_id,OLD.topic_id,OLD.conversation_id,OLD.recipient_id)
    AND EXISTS(SELECT 1 FROM decision_thread_answers
        WHERE reply_message_id IN (OLD.message_id,NEW.message_id) OR delivery_id IN (OLD.id,NEW.id))
 OR OLD.turn_id<>'' AND NEW.turn_id IS NOT OLD.turn_id
    AND EXISTS(SELECT 1 FROM decision_thread_answers WHERE delivery_id=OLD.id)
BEGIN SELECT RAISE(ABORT,'话题答复投递目标或认领不可修改'); END;

-- The user is the author, but this bounded answer is never a command granting work.
CREATE TRIGGER decision_thread_no_chat_command BEFORE INSERT ON message_command_sources
WHEN instr(NEW.message_id,'` + threadDecisionAnswerPrefix + `')=1
BEGIN SELECT RAISE(ABORT,'话题答复不是新的聊天命令'); END;
CREATE TRIGGER decision_thread_no_coordination_trigger BEFORE INSERT ON coordination_triggers
WHEN instr(NEW.message_id,'` + threadDecisionAnswerPrefix + `')=1
BEGIN SELECT RAISE(ABORT,'话题答复不是新的工作授权'); END;

-- No trusted roster epoch proves continuity for legacy questions, even when
-- the asker is active now. No Thread answer exists before first installation.
` + decisionThreadWithdraw + `;

-- Neither unanswered nor queued/claimed questions may revive after rejoin.
CREATE TRIGGER decision_thread_asker_departed AFTER UPDATE OF status ON members
WHEN OLD.status='active' AND NEW.status='left'
BEGIN
    ` + decisionThreadWithdraw + ` AND group_id=OLD.group_id AND ` + decisionThreadQuestionAsker + `=OLD.id;
END;
`
