package store

// Register as an append in landing order. Existing Turns have no explanation.
const agentRunExplanationSchema = agentRunExplanationTable + ";\n" +
	agentRunExplanationNoReplace + ";\n" +
	agentRunExplanationNoUpdate + ";\n" +
	agentRunExplanationNoDelete + ";\n"

// SQLite stores each CREATE definition without its statement terminator.
const agentRunExplanationTable = `CREATE TABLE agent_run_explanations (
    -- The Turn ID is also the source ID; the public timeline namespaces it.
    id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL,
    member_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    claim_digest BLOB NOT NULL CHECK(typeof(claim_digest)='blob' AND length(claim_digest)=32),
    idempotency_key TEXT NOT NULL CHECK(typeof(idempotency_key)='text' AND length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 128
        AND idempotency_key NOT GLOB '*[^!-~]*' AND instr(idempotency_key,',')=0 AND instr(idempotency_key,char(0))=0),
    text TEXT NOT NULL CHECK(typeof(text)='text' AND length(CAST(text AS BLOB)) BETWEEN 1 AND 1024 AND instr(text,char(0))=0),
    created_at TEXT NOT NULL CHECK(length(created_at)=30 AND julianday(created_at) IS NOT NULL),
    UNIQUE(group_id,member_id,idempotency_key),
    FOREIGN KEY(id,group_id,member_id,conversation_id) REFERENCES turns(id,group_id,member_id,conversation_id),
    FOREIGN KEY(conversation_id,group_id,member_id,topic_id) REFERENCES conversations(id,group_id,member_id,topic_id)
) WITHOUT ROWID`

const agentRunExplanationNoReplace = `CREATE TRIGGER agent_run_explanations_no_replace BEFORE INSERT ON agent_run_explanations
WHEN EXISTS(SELECT 1 FROM agent_run_explanations WHERE id=NEW.id
    OR (group_id,member_id,idempotency_key)=(NEW.group_id,NEW.member_id,NEW.idempotency_key))
BEGIN SELECT RAISE(ABORT, '公开运行说明不可替换'); END`

const agentRunExplanationNoUpdate = `CREATE TRIGGER agent_run_explanations_no_update BEFORE UPDATE ON agent_run_explanations
BEGIN SELECT RAISE(ABORT, '公开运行说明不可修改'); END`

const agentRunExplanationNoDelete = `CREATE TRIGGER agent_run_explanations_no_delete BEFORE DELETE ON agent_run_explanations
BEGIN SELECT RAISE(ABORT, '公开运行说明不可删除'); END`
