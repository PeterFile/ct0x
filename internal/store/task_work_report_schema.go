package store

// Provisional v30. The v29 tables and stored rows are not rebuilt or backfilled.
const taskWorkReportVersion = 30

const taskWorkReportSchema = `
CREATE TABLE task_work_delegations (
    id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    task_revision INTEGER NOT NULL CHECK(task_revision>0),
    assignment_id TEXT NOT NULL,
    mandate_id TEXT NOT NULL,
    authority_generation INTEGER NOT NULL CHECK(authority_generation>0),
    owner_member_id TEXT NOT NULL,
    author_turn_id TEXT NOT NULL,
    author_attempt_id TEXT NOT NULL,
    member_id TEXT NOT NULL CHECK(member_id<>owner_member_id),
    question TEXT NOT NULL CHECK(length(CAST(question AS BLOB)) BETWEEN 1 AND 4096),
    allow_topic_messages INTEGER NOT NULL CHECK(allow_topic_messages IN (0,1)),
    artifact_versions TEXT NOT NULL CHECK(json_valid(artifact_versions) AND json_type(artifact_versions)='array'
        AND json_array_length(artifact_versions)<=16 AND length(CAST(artifact_versions AS BLOB))<=32768),
    max_delegations INTEGER NOT NULL CHECK(max_delegations BETWEEN 1 AND 4),
    ordinal INTEGER NOT NULL CHECK(ordinal BETWEEN 1 AND max_delegations),
    delivery_id TEXT NOT NULL UNIQUE REFERENCES task_work_deliveries(delivery_id) DEFERRABLE INITIALLY DEFERRED,
    created_at TEXT NOT NULL,
    UNIQUE(group_id,task_id,ordinal),
    UNIQUE(id,group_id,task_id,member_id),
    FOREIGN KEY(assignment_id,task_id,group_id,owner_member_id)
        REFERENCES coordination_assignments(id,task_id,group_id,worker_member_id),
    FOREIGN KEY(mandate_id,task_id,group_id,topic_id) REFERENCES mandates(id,task_id,group_id,topic_id),
    FOREIGN KEY(author_turn_id,group_id,owner_member_id) REFERENCES turns(id,group_id,member_id),
    FOREIGN KEY(author_attempt_id,task_id,group_id,task_revision) REFERENCES task_attempts(id,task_id,group_id,task_revision),
    FOREIGN KEY(group_id,member_id) REFERENCES members(group_id,id)
) STRICT, WITHOUT ROWID;

CREATE TABLE task_work_reports (
    id TEXT PRIMARY KEY NOT NULL,
    delegation_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    worker_member_id TEXT NOT NULL,
    source_turn_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('progress','question','blocked','delivered')),
    summary TEXT NOT NULL CHECK(length(CAST(summary AS BLOB)) BETWEEN 1 AND 8192),
    ordinal INTEGER NOT NULL CHECK(ordinal BETWEEN 1 AND 4 AND (ordinal<4 OR kind<>'progress')),
    delivery_id TEXT UNIQUE REFERENCES task_work_deliveries(delivery_id) DEFERRABLE INITIALLY DEFERRED,
    created_at TEXT NOT NULL,
    CHECK((delivery_id IS NULL)=(kind='progress')),
    UNIQUE(delegation_id,ordinal),
    FOREIGN KEY(delegation_id,group_id,task_id,worker_member_id) REFERENCES task_work_delegations(id,group_id,task_id,member_id),
    FOREIGN KEY(source_turn_id,group_id,worker_member_id) REFERENCES turns(id,group_id,member_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX task_work_reports_task ON task_work_reports(group_id,task_id,created_at DESC,id DESC);

CREATE TABLE task_work_handbacks (
    id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    task_revision INTEGER NOT NULL CHECK(task_revision>0),
    assignment_id TEXT NOT NULL,
    mandate_id TEXT NOT NULL,
    authority_generation INTEGER NOT NULL CHECK(authority_generation>0),
    owner_member_id TEXT NOT NULL,
    author_turn_id TEXT NOT NULL,
    author_attempt_id TEXT NOT NULL,
    coordinator_member_id TEXT NOT NULL CHECK(coordinator_member_id<>owner_member_id),
    summary TEXT NOT NULL CHECK(length(CAST(summary AS BLOB)) BETWEEN 1 AND 8192),
    notice_message_id TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    UNIQUE(group_id,task_id),
    FOREIGN KEY(assignment_id,task_id,group_id,owner_member_id)
        REFERENCES coordination_assignments(id,task_id,group_id,worker_member_id),
    FOREIGN KEY(mandate_id,task_id,group_id,topic_id) REFERENCES mandates(id,task_id,group_id,topic_id),
    FOREIGN KEY(author_turn_id,group_id,owner_member_id) REFERENCES turns(id,group_id,member_id),
    FOREIGN KEY(author_attempt_id,task_id,group_id,task_revision) REFERENCES task_attempts(id,task_id,group_id,task_revision),
    FOREIGN KEY(group_id,coordinator_member_id) REFERENCES members(group_id,id),
    FOREIGN KEY(notice_message_id,group_id,topic_id) REFERENCES messages(id,group_id,topic_id)
) STRICT, WITHOUT ROWID;

CREATE TRIGGER task_work_delegations_insert BEFORE INSERT ON task_work_delegations
BEGIN
    SELECT RAISE(ABORT,'委派记录不可替换') WHERE EXISTS(SELECT 1 FROM task_work_delegations
        WHERE id=NEW.id OR delivery_id=NEW.delivery_id OR (group_id,task_id,ordinal)=(NEW.group_id,NEW.task_id,NEW.ordinal));
    SELECT RAISE(ABORT,'委派来源不匹配') WHERE NOT EXISTS(` + taskWorkOwnerOrigin + `
        AND NEW.member_id<>r.coordinator_member_id
        AND t.state IN ('starting','running') AND t.cancel_requested_at IS NULL AND t.candidate_terminal='')
        OR EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=NEW.delivery_id);
END;
CREATE TRIGGER task_work_reports_insert BEFORE INSERT ON task_work_reports
BEGIN
    SELECT RAISE(ABORT,'报告记录不可替换或已交付') WHERE EXISTS(SELECT 1 FROM task_work_reports
        WHERE id=NEW.id OR delivery_id=NEW.delivery_id OR (delegation_id=NEW.delegation_id AND (ordinal=NEW.ordinal OR kind='delivered')));
    SELECT RAISE(ABORT,'报告来源不匹配') WHERE NOT EXISTS(
        SELECT 1` + taskWorkReportChildRelations + `
        WHERE (p.id,p.group_id,p.task_id,p.member_id,t.id)=(NEW.delegation_id,NEW.group_id,NEW.task_id,NEW.worker_member_id,NEW.source_turn_id)
        AND t.state IN ('starting','running') AND t.cancel_requested_at IS NULL AND t.candidate_terminal='')
        OR EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=NEW.delivery_id);
END;
CREATE TRIGGER task_work_handbacks_insert BEFORE INSERT ON task_work_handbacks
BEGIN
    SELECT RAISE(ABORT,'移交记录不可替换') WHERE EXISTS(SELECT 1 FROM task_work_handbacks
        WHERE id=NEW.id OR notice_message_id=NEW.notice_message_id OR (group_id,task_id)=(NEW.group_id,NEW.task_id));
    SELECT RAISE(ABORT,'移交来源不匹配') WHERE NOT EXISTS(` + taskWorkOwnerOrigin + `
        AND NEW.coordinator_member_id=r.coordinator_member_id
        AND t.state IN ('starting','running') AND t.cancel_requested_at IS NULL AND t.candidate_terminal='')
        OR NOT EXISTS(SELECT 1 FROM messages WHERE id=NEW.notice_message_id AND sender_id=NEW.owner_member_id)
        OR EXISTS(SELECT 1 FROM deliveries WHERE message_id=NEW.notice_message_id);
END;

-- Keep the v29 followup proof. Only v30 adds the two new source arms, and
-- their deferred foreign keys require the matching marker before commit.
DROP TRIGGER task_work_delivery_source_insert;
CREATE TRIGGER task_work_delivery_source_insert BEFORE INSERT ON task_work_deliveries
BEGIN
    SELECT RAISE(ABORT, '工作投递缺少已验证的来源') WHERE NOT CASE NEW.source_kind
    WHEN 'followup' THEN NOT (EXISTS(SELECT 1 FROM task_work_delegations WHERE delivery_id=NEW.delivery_id)
        OR EXISTS(SELECT 1 FROM task_work_reports WHERE delivery_id=NEW.delivery_id)) AND EXISTS(
        SELECT 1 FROM task_work_inputs i JOIN chat_followup_routes f ON f.source_id=i.source_id
        JOIN chat_assessments a ON a.id=f.assessment_id JOIN chat_candidates c ON c.id=a.candidate_id
        WHERE (i.id,i.assignment_id,i.task_id,i.task_revision,i.mandate_id,i.worker_member_id,i.group_id,i.topic_id)=
           (NEW.source_id,NEW.assignment_id,NEW.task_id,NEW.task_revision,NEW.mandate_id,NEW.owner_member_id,NEW.group_id,NEW.topic_id)
        AND NEW.sender_member_id=c.member_id AND json_type(a.receipt_json,'$.work_input_pending') IS NULL)
    WHEN 'delegation' THEN NOT EXISTS(SELECT 1 FROM task_work_reports WHERE delivery_id=NEW.delivery_id) AND EXISTS(
        SELECT 1 FROM task_work_delegations p WHERE (p.delivery_id,p.id,p.member_id,p.owner_member_id)=
            (NEW.delivery_id,NEW.source_id,NEW.member_id,NEW.sender_member_id) AND ` + taskWorkSourceParentMatch + `)
    WHEN 'report' THEN NOT EXISTS(SELECT 1 FROM task_work_delegations WHERE delivery_id=NEW.delivery_id) AND EXISTS(
        SELECT 1 FROM task_work_reports r JOIN task_work_delegations p ON p.id=r.delegation_id
        WHERE (r.delivery_id,r.id,p.owner_member_id,r.worker_member_id)=
            (NEW.delivery_id,NEW.source_id,NEW.member_id,NEW.sender_member_id) AND ` + taskWorkSourceParentMatch + `)
    ELSE 0 END;
END;

CREATE TRIGGER task_work_delegations_no_update BEFORE UPDATE ON task_work_delegations
BEGIN SELECT RAISE(ABORT,'委派记录不可修改'); END;
CREATE TRIGGER task_work_delegations_no_delete BEFORE DELETE ON task_work_delegations
BEGIN SELECT RAISE(ABORT,'委派记录不可删除'); END;
CREATE TRIGGER task_work_reports_no_update BEFORE UPDATE ON task_work_reports
BEGIN SELECT RAISE(ABORT,'报告记录不可修改'); END;
CREATE TRIGGER task_work_reports_no_delete BEFORE DELETE ON task_work_reports
BEGIN SELECT RAISE(ABORT,'报告记录不可删除'); END;
CREATE TRIGGER task_work_handbacks_no_update BEFORE UPDATE ON task_work_handbacks
BEGIN SELECT RAISE(ABORT,'移交记录不可修改'); END;
CREATE TRIGGER task_work_handbacks_no_delete BEFORE DELETE ON task_work_handbacks
BEGIN SELECT RAISE(ABORT,'移交记录不可删除'); END;
` + taskWorkDeliveryQueueGuards

const taskWorkSourceParentMatch = `(p.group_id,p.topic_id,p.task_id,p.task_revision,p.assignment_id,p.mandate_id,p.authority_generation,p.owner_member_id)=
    (NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision,NEW.assignment_id,NEW.mandate_id,NEW.authority_generation,NEW.owner_member_id)`

// The logical report names the unique receiving Turn of this exact delegation.
const taskWorkReportChildRelations = ` FROM task_work_delegations p
 JOIN task_work_deliveries child ON (child.delivery_id,child.source_kind,child.source_id)=(p.delivery_id,'delegation',p.id)
 AND (child.group_id,child.topic_id,child.task_id,child.task_revision,child.assignment_id,child.mandate_id,child.authority_generation,
      child.owner_member_id,child.sender_member_id,child.member_id)=
     (p.group_id,p.topic_id,p.task_id,p.task_revision,p.assignment_id,p.mandate_id,p.authority_generation,p.owner_member_id,p.owner_member_id,p.member_id)
 JOIN deliveries cd ON (cd.id,cd.message_id,cd.group_id,cd.topic_id,cd.recipient_id,cd.conversation_id)=
 (child.delivery_id,child.message_id,p.group_id,p.topic_id,p.member_id,child.conversation_id)
 JOIN turns t ON t.id=cd.turn_id AND (t.group_id,t.member_id,t.delivery_id,t.conversation_id)=
 (p.group_id,p.member_id,child.delivery_id,child.conversation_id) AND cd.state=` + taskWorkClaimDeliveryState + `
 AND NOT EXISTS(SELECT 1 FROM turns other WHERE other.delivery_id=child.delivery_id AND other.id<>t.id)`

// A logical source can only originate in the exact original or continuation
// attempt. A child can never satisfy either arm, even if it is also a worker.
const taskWorkOwnerOrigin = `SELECT 1 FROM coordination_assignments ca
    JOIN coordination_dispatch_requests r ON r.id=ca.request_id
    JOIN task_attempts a ON a.id=NEW.author_attempt_id
    JOIN turns t ON t.id=NEW.author_turn_id
    JOIN deliveries d ON d.id=t.delivery_id AND d.turn_id=t.id
    WHERE ca.id=NEW.assignment_id
    AND (ca.task_id,ca.group_id,ca.worker_member_id)=(NEW.task_id,NEW.group_id,NEW.owner_member_id)
    AND (r.topic_id,r.mandate_id,r.task_revision,r.authority_generation)=
        (NEW.topic_id,NEW.mandate_id,NEW.task_revision,NEW.authority_generation)
    AND (a.turn_id,a.task_id,a.group_id,a.task_revision,a.actor_id)=(t.id,NEW.task_id,NEW.group_id,NEW.task_revision,NEW.owner_member_id)
    AND (d.group_id,d.topic_id,d.recipient_id)=(NEW.group_id,NEW.topic_id,NEW.owner_member_id)
    AND (EXISTS(SELECT 1 FROM coordination_assignment_attempts b JOIN coordination_assignment_dispatch s USING(assignment_id)
        WHERE (b.assignment_id,b.attempt_id,b.turn_id,s.delivery_id)=(ca.id,a.id,t.id,d.id))
        OR EXISTS(SELECT 1 FROM task_work_continuations b JOIN task_work_deliveries w USING(delivery_id)
        WHERE (w.assignment_id,b.attempt_id,b.turn_id,w.delivery_id)=(ca.id,a.id,t.id,d.id)
        AND w.source_kind IN ('report','followup') AND (w.group_id,w.topic_id,w.task_id,w.task_revision,w.mandate_id,
            w.authority_generation,w.owner_member_id,w.member_id,w.message_id,w.conversation_id)=
            (NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision,NEW.mandate_id,NEW.authority_generation,
            NEW.owner_member_id,NEW.owner_member_id,d.message_id,d.conversation_id)))`

// Keep the frozen v29 rowid tables and protect their physical queue identity
// after upgrade, including against REPLACE with recursive triggers disabled.
const taskWorkDeliveryQueueGuards = `
CREATE TRIGGER task_work_deliveries_no_rowid_replace BEFORE INSERT ON task_work_deliveries
WHEN EXISTS(SELECT 1 FROM task_work_deliveries WHERE rowid=NEW.rowid)
BEGIN SELECT RAISE(ABORT, '工作投递不可替换'); END;
CREATE TRIGGER task_work_continuations_no_rowid_replace BEFORE INSERT ON task_work_continuations
WHEN EXISTS(SELECT 1 FROM task_work_continuations WHERE rowid=NEW.rowid)
BEGIN SELECT RAISE(ABORT, '工作续办尝试不可替换'); END;

-- ClaimTurn sets the empty back-pointer before inserting the exact Turn.
CREATE TRIGGER task_work_queue_deliveries_insert BEFORE INSERT ON deliveries
BEGIN
    SELECT RAISE(ABORT, '工作投递不可替换') WHERE EXISTS(SELECT 1 FROM task_work_deliveries
        WHERE delivery_id=NEW.id OR message_id=NEW.message_id)
        OR EXISTS(SELECT 1 FROM deliveries d JOIN task_work_deliveries w ON w.delivery_id=d.id WHERE d.rowid=NEW.rowid)
        OR EXISTS(SELECT 1 FROM task_work_handbacks WHERE notice_message_id=NEW.message_id);
END;
CREATE TRIGGER task_work_queue_deliveries_identity
BEFORE UPDATE OF rowid,_rowid_,oid,id,message_id,group_id,topic_id,conversation_id,recipient_id,turn_id ON deliveries
BEGIN
    SELECT RAISE(ABORT, '工作投递身份不可修改') WHERE EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=OLD.id)
        AND (NEW.rowid IS NOT OLD.rowid OR NEW.id IS NOT OLD.id OR NEW.message_id IS NOT OLD.message_id OR NEW.group_id IS NOT OLD.group_id
            OR NEW.topic_id IS NOT OLD.topic_id OR NEW.conversation_id IS NOT OLD.conversation_id OR NEW.recipient_id IS NOT OLD.recipient_id);
    SELECT RAISE(ABORT, '工作投递回指不可修改') WHERE EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=OLD.id)
        AND NEW.turn_id IS NOT OLD.turn_id
        AND NOT (OLD.turn_id='' AND typeof(NEW.turn_id)='text' AND NEW.turn_id<>'' AND trim(NEW.turn_id)=NEW.turn_id);
    SELECT RAISE(ABORT, '工作投递不可替换') WHERE EXISTS(SELECT 1 FROM task_work_deliveries
        WHERE delivery_id<>OLD.id AND (delivery_id=NEW.id OR message_id=NEW.message_id))
        OR EXISTS(SELECT 1 FROM deliveries d JOIN task_work_deliveries w ON w.delivery_id=d.id
            WHERE d.rowid=NEW.rowid AND d.rowid<>OLD.rowid)
        OR EXISTS(SELECT 1 FROM task_work_handbacks WHERE notice_message_id=NEW.message_id);
END;
CREATE TRIGGER task_work_queue_deliveries_no_delete BEFORE DELETE ON deliveries
WHEN EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=OLD.id)
BEGIN SELECT RAISE(ABORT, '工作投递不可删除'); END;

CREATE TRIGGER task_work_turns_insert BEFORE INSERT ON turns
BEGIN
    -- REPLACE may evict a typed claim on the primary key or either active index.
    SELECT RAISE(ABORT, '工作轮次不可替换') WHERE EXISTS(SELECT 1 FROM turns t WHERE
        (t.rowid=NEW.rowid OR t.id=NEW.id
        OR (t.member_id=NEW.member_id AND t.state IN ('starting','running','cancel_requested','unknown')
            AND NEW.state IN ('starting','running','cancel_requested','unknown'))
        OR (t.conversation_id=NEW.conversation_id AND t.state IN ('starting','running','cancel_requested','unknown')
            AND NEW.state IN ('starting','running','cancel_requested','unknown')))
        AND EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=t.delivery_id));
    SELECT RAISE(ABORT, '工作轮次来源无效') WHERE EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=NEW.delivery_id)
        AND NOT EXISTS(SELECT 1 FROM deliveries d WHERE d.id=NEW.delivery_id AND d.turn_id=NEW.id
            AND d.group_id=NEW.group_id AND d.conversation_id=NEW.conversation_id AND d.recipient_id=NEW.member_id);
END;
CREATE TRIGGER task_work_turns_update
BEFORE UPDATE OF rowid,_rowid_,oid,id,group_id,delivery_id,conversation_id,member_id,state ON turns
BEGIN
    SELECT RAISE(ABORT, '工作轮次身份不可修改') WHERE EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=OLD.delivery_id)
        AND (NEW.rowid IS NOT OLD.rowid OR NEW.id IS NOT OLD.id OR NEW.group_id IS NOT OLD.group_id OR NEW.delivery_id IS NOT OLD.delivery_id
            OR NEW.conversation_id IS NOT OLD.conversation_id OR NEW.member_id IS NOT OLD.member_id);
    SELECT RAISE(ABORT, '工作轮次不可替换') WHERE EXISTS(SELECT 1 FROM turns t WHERE t.rowid<>OLD.rowid AND
        (t.rowid=NEW.rowid OR t.id=NEW.id
        OR (t.member_id=NEW.member_id AND t.state IN ('starting','running','cancel_requested','unknown')
            AND NEW.state IN ('starting','running','cancel_requested','unknown'))
        OR (t.conversation_id=NEW.conversation_id AND t.state IN ('starting','running','cancel_requested','unknown')
            AND NEW.state IN ('starting','running','cancel_requested','unknown')))
        AND EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=t.delivery_id));
    SELECT RAISE(ABORT, '工作轮次来源无效') WHERE EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=NEW.delivery_id)
        AND NOT EXISTS(SELECT 1 FROM deliveries d WHERE d.id=NEW.delivery_id AND d.turn_id=NEW.id
            AND d.group_id=NEW.group_id AND d.conversation_id=NEW.conversation_id AND d.recipient_id=NEW.member_id);
END;
CREATE TRIGGER task_work_turns_no_delete BEFORE DELETE ON turns
WHEN EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=OLD.delivery_id)
BEGIN SELECT RAISE(ABORT, '工作轮次不可删除'); END;
`
