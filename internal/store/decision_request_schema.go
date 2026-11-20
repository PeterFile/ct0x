package store

// Append after the v30 work ledgers. The registry slot is assigned at merge;
// no preexisting chat or Task rows are promoted into decision requests.
const decisionRequestSchema = `
CREATE TABLE decision_requests (
    id TEXT PRIMARY KEY NOT NULL CHECK(id<>''),
    group_id TEXT NOT NULL REFERENCES groups(id),
    topic_id TEXT NOT NULL,
    recipient_id TEXT NOT NULL CHECK(recipient_id='user'),
    kind TEXT NOT NULL CHECK(kind IN ('decision','authorization','unblock','clarification')),
    relay TEXT NOT NULL CHECK(relay IN ('task_owner','origin_thread')),
    task_id TEXT,
    task_revision INTEGER CHECK(task_revision>0),
    assignment_id TEXT,
    mandate_id TEXT,
    authority_generation INTEGER CHECK(authority_generation>0),
    owner_member_id TEXT,
    origin_trigger_id TEXT REFERENCES coordination_triggers(id),
    chat_source_id TEXT REFERENCES message_command_sources(id),
    chat_assessment_id TEXT REFERENCES chat_assessments(id),
    source_message_id TEXT NOT NULL,
    source_sequence INTEGER NOT NULL REFERENCES message_log(sequence),
    created_at TEXT NOT NULL CHECK(length(created_at)=30),
    withdrawn_at TEXT CHECK(length(withdrawn_at)=30 AND withdrawn_at>=created_at),
    CHECK((kind='clarification' AND relay='origin_thread'
        AND task_id IS NULL AND task_revision IS NULL AND assignment_id IS NULL AND mandate_id IS NULL
        AND authority_generation IS NULL AND owner_member_id IS NULL AND origin_trigger_id IS NULL
        AND chat_source_id IS NOT NULL AND chat_assessment_id IS NOT NULL)
        OR (kind<>'clarification' AND relay='task_owner'
        AND task_id IS NOT NULL AND task_revision IS NOT NULL AND assignment_id IS NOT NULL AND mandate_id IS NOT NULL
        AND authority_generation IS NOT NULL AND owner_member_id IS NOT NULL AND origin_trigger_id IS NOT NULL
        AND chat_source_id IS NULL AND chat_assessment_id IS NULL)),
    FOREIGN KEY(assignment_id,task_id,group_id,owner_member_id)
        REFERENCES coordination_assignments(id,task_id,group_id,worker_member_id),
    FOREIGN KEY(mandate_id,task_id,group_id,topic_id) REFERENCES mandates(id,task_id,group_id,topic_id),
    FOREIGN KEY(source_message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id)
) STRICT, WITHOUT ROWID;
CREATE UNIQUE INDEX decision_requests_task_slot ON decision_requests(
    group_id,topic_id,task_id,task_revision,assignment_id,mandate_id,authority_generation,owner_member_id,kind)
    WHERE task_id IS NOT NULL;
CREATE UNIQUE INDEX decision_requests_chat_source ON decision_requests(chat_source_id) WHERE chat_source_id IS NOT NULL;
CREATE INDEX decision_requests_pending_order ON decision_requests(recipient_id,created_at DESC,id DESC) WHERE withdrawn_at IS NULL;

CREATE TABLE decision_request_versions (
    request_id TEXT NOT NULL REFERENCES decision_requests(id),
    version INTEGER NOT NULL CHECK(version>0),
    question_json TEXT NOT NULL CHECK(length(CAST(question_json AS BLOB)) BETWEEN 1 AND 49152 AND json_valid(question_json)
        AND json_type(question_json)='object'),
    author_member_id TEXT NOT NULL,
    author_turn_id TEXT NOT NULL REFERENCES turns(id),
    author_attempt_id TEXT REFERENCES task_attempts(id),
    created_at TEXT NOT NULL CHECK(length(created_at)=30),
    expires_at TEXT CHECK(length(expires_at)=30 AND expires_at>created_at),
    PRIMARY KEY(request_id,version)
) STRICT, WITHOUT ROWID;

-- Guard INSERT OR REPLACE even when recursive triggers are off.
CREATE TRIGGER decision_requests_insert BEFORE INSERT ON decision_requests
BEGIN
    SELECT RAISE(ABORT,'待决事项不可替换') WHERE EXISTS(SELECT 1 FROM decision_requests WHERE id=NEW.id)
        OR EXISTS(SELECT 1 FROM decision_requests WHERE chat_source_id=NEW.chat_source_id)
        OR EXISTS(SELECT 1 FROM decision_requests
            WHERE (group_id,topic_id,task_id,task_revision,assignment_id,mandate_id,authority_generation,owner_member_id,kind)=
                (NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision,NEW.assignment_id,NEW.mandate_id,
                 NEW.authority_generation,NEW.owner_member_id,NEW.kind));
    SELECT RAISE(ABORT,'待决事项来源不匹配') WHERE NOT EXISTS(SELECT 1 FROM message_log
        WHERE (message_id,group_id,topic_id,sequence)=(NEW.source_message_id,NEW.group_id,NEW.topic_id,NEW.source_sequence));
END;
CREATE TRIGGER decision_requests_identity BEFORE UPDATE OF id,group_id,topic_id,recipient_id,kind,relay,task_id,task_revision,
    assignment_id,mandate_id,authority_generation,owner_member_id,origin_trigger_id,chat_source_id,chat_assessment_id,
    source_message_id,source_sequence,created_at ON decision_requests
BEGIN SELECT RAISE(ABORT,'待决事项绑定不可修改'); END;
CREATE TRIGGER decision_requests_withdraw BEFORE UPDATE OF withdrawn_at ON decision_requests
WHEN OLD.withdrawn_at IS NOT NULL OR NEW.withdrawn_at IS NULL
BEGIN SELECT RAISE(ABORT,'待决事项不可恢复'); END;
CREATE TRIGGER decision_requests_no_delete BEFORE DELETE ON decision_requests
BEGIN SELECT RAISE(ABORT,'待决事项不可删除'); END;
CREATE TRIGGER decision_request_versions_insert BEFORE INSERT ON decision_request_versions
BEGIN
    SELECT RAISE(ABORT,'待决事项版本不连续') WHERE NEW.version<>1+coalesce((SELECT max(version)
        FROM decision_request_versions WHERE request_id=NEW.request_id),0);
    SELECT RAISE(ABORT,'待决事项提出者不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM decision_requests q JOIN turns t ON t.id=NEW.author_turn_id AND t.group_id=q.group_id
        WHERE q.id=NEW.request_id AND q.withdrawn_at IS NULL AND t.member_id=NEW.author_member_id
        AND ((q.task_id IS NULL AND NEW.version=1 AND NEW.author_attempt_id IS NULL AND EXISTS(
            SELECT 1 FROM chat_assessments a JOIN chat_candidates c ON c.id=a.candidate_id
            WHERE a.id=q.chat_assessment_id AND c.turn_id=t.id))
        OR (q.task_id IS NOT NULL AND q.owner_member_id=t.member_id AND EXISTS(
            SELECT 1 FROM task_attempts a WHERE a.id=NEW.author_attempt_id
            AND (a.turn_id,a.actor_id,a.task_id,a.group_id,a.task_revision)=(t.id,t.member_id,q.task_id,q.group_id,q.task_revision)))));
END;
CREATE TRIGGER decision_request_versions_no_update BEFORE UPDATE ON decision_request_versions
BEGIN SELECT RAISE(ABORT,'待决事项历史版本不可修改'); END;
CREATE TRIGGER decision_request_versions_no_delete BEFORE DELETE ON decision_request_versions
BEGIN SELECT RAISE(ABORT,'待决事项历史版本不可删除'); END;
`
