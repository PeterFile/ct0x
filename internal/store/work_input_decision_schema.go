package store

const workInputDecisionPrefix = "work-input-return:"

const workInputDecisionSources = `decision_work_input_return_sources`
const workInputDecisionIDMatch = `q.id GLOB '` + workInputDecisionPrefix + `*'`

const workInputDecisionBinding = `(b.trigger_id,b.group_id,b.topic_id,b.task_id,b.task_revision,b.assignment_id,b.mandate_id,
 b.authority_generation,b.member_id,b.message_id,b.sequence,b.returned_at)`

const workInputDecisionRequestMatch = `q.kind='authorization' AND ` + workInputDecisionBinding + `=
 (q.origin_trigger_id,q.group_id,q.topic_id,q.task_id,q.task_revision,q.assignment_id,q.mandate_id,
 q.authority_generation,q.owner_member_id,q.source_message_id,q.source_sequence,q.created_at)`

const workInputDecisionVersionFields = `(b.member_id,b.turn_id,b.attempt_id,b.returned_at,b.question,b.reason)`

const workInputDecisionReadMatch = workInputDecisionRequestMatch + ` AND b.impacted
 AND ` + workInputDecisionVersionFields + `=(v.author_member_id,v.author_turn_id,v.author_attempt_id,v.created_at,
    json_extract(v.question_json,'$.question'),json_extract(v.question_json,'$.facts'))
 AND v.version=1 AND v.expires_at IS NULL`

const workInputDecisionEnrolling = `NOT EXISTS(SELECT 1 FROM task_constraint_impacts WHERE id=b.input_id)
 AND EXISTS(SELECT 1 FROM tasks t
 JOIN task_coordination tc ON (tc.task_id,tc.group_id,tc.topic_id,tc.current_mandate_id,tc.authority_generation)=
    (b.task_id,b.group_id,b.topic_id,b.mandate_id,b.authority_generation)
 JOIN chat_policies p ON (p.group_id,p.topic_id,p.revision)=(b.group_id,b.topic_id,b.policy_revision)
 JOIN chat_policy_revisions pr ON (pr.group_id,pr.topic_id,pr.revision)=(p.group_id,p.topic_id,p.revision)
 LEFT JOIN chat_policy_scopes ps ON (ps.group_id,ps.topic_id,ps.revision)=(p.group_id,p.topic_id,p.revision)
 JOIN turns w ON w.id=b.turn_id
 WHERE (t.id,t.group_id,t.revision,t.active_attempt_id)=(b.task_id,b.group_id,b.task_revision,b.attempt_id)
 AND t.status='open' AND t.current_submission_id IS NULL AND coalesce(t.assignee_id,'') IN ('',b.member_id)
 AND pr.enabled=1 AND ps.command_source_id IS b.policy_source_id
 AND w.state IN ('starting','running') AND w.cancel_requested_at IS NULL AND w.candidate_terminal='')`

// Seek by the unique followup trigger, never by excerpt or latest return.
// The author binding is historical; only enrollment needs a live Turn. The
// impact completes the proof but cannot supply it on its own.
const workInputDecisionSourceSelect = `SELECT i.id AS input_id,f.trigger_id,i.group_id,i.topic_id,i.task_id,i.task_revision,i.assignment_id,i.mandate_id,r.authority_generation,
 h.member_id,h.turn_id,h.attempt_id,o.message_id,o.message_sequence AS sequence,h.returned_at,h.question,h.reason,
 h.policy_revision,h.policy_source_id,x.id IS NOT NULL AS impacted
 FROM chat_followup_routes f JOIN task_work_inputs i ON (i.source_id,i.group_id,i.topic_id)=(f.source_id,f.group_id,f.topic_id)
 JOIN coordination_triggers o ON (o.id,o.group_id,o.topic_id,o.message_id,o.source_kind)=
    (f.trigger_id,i.group_id,i.topic_id,f.reply_message_id,'local_im')
 JOIN message_command_sources s ON (s.id,s.message_id,s.kind)=(f.source_id,o.message_id,'local_user_chat')
 JOIN messages m ON (m.id,m.group_id,m.topic_id,m.sender_id,m.content)=(o.message_id,i.group_id,i.topic_id,'user',i.content)
 JOIN message_log l ON (l.sequence,l.message_id,l.group_id,l.topic_id)=(o.message_sequence,m.id,i.group_id,i.topic_id)
 JOIN task_work_input_returns h ON (h.input_id,h.member_id)=(i.id,i.worker_member_id)
 JOIN messages n ON (n.id,n.group_id,n.topic_id,n.sender_id)=(h.notice_message_id,i.group_id,i.topic_id,h.member_id)
 JOIN message_log published ON (published.message_id,published.group_id,published.topic_id)=(n.id,n.group_id,n.topic_id)
 JOIN coordination_assignments ca ON (ca.id,ca.task_id,ca.group_id,ca.worker_member_id)=(i.assignment_id,i.task_id,i.group_id,i.worker_member_id)
 JOIN coordination_dispatch_requests r ON (r.id,r.group_id,r.topic_id,r.task_id,r.task_revision,r.mandate_id)=
    (ca.request_id,i.group_id,i.topic_id,i.task_id,i.task_revision,i.mandate_id)
 JOIN task_attempts author ON (author.id,author.turn_id,author.actor_id,author.task_id,author.group_id,author.task_revision)=
    (h.attempt_id,h.turn_id,h.member_id,i.task_id,i.group_id,i.task_revision)
 LEFT JOIN task_constraint_impacts x ON (x.id,x.input_id,x.kind,x.group_id,x.source_task_id,x.task_id,x.task_revision,x.ref_id,x.created_at)=
    (i.id,i.id,'work_input_returned',i.group_id,i.task_id,i.task_id,i.task_revision,i.id,h.returned_at)
 WHERE h.disposition='separate_authorization_required' AND published.sequence>l.sequence
 AND published.sequence>(SELECT sequence FROM decision_work_input_boundary)
 AND NOT EXISTS(SELECT 1 FROM task_work_input_consumptions WHERE input_id=i.id)
 AND NOT EXISTS(SELECT 1 FROM routing_decisions WHERE trigger_id=f.trigger_id)
 AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=n.id)
 AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=n.id)
 AND (EXISTS(SELECT 1 FROM coordination_assignment_attempts b
    WHERE (b.assignment_id,b.attempt_id,b.turn_id)=(i.assignment_id,h.attempt_id,h.turn_id))
 OR EXISTS(SELECT 1` + taskWorkContinuationRelations + `
    JOIN task_work_continuations b ON (b.delivery_id,b.turn_id,b.attempt_id)=(w.delivery_id,t.id,a.id)
    WHERE (b.attempt_id,b.turn_id)=(h.attempt_id,h.turn_id)
    AND (w.assignment_id,w.mandate_id,w.authority_generation,w.topic_id)=(i.assignment_id,i.mandate_id,r.authority_generation,i.topic_id)))`

// Replacement guards must still compile for Mandate sources if a copy loses
// the optional projection view. Use the same proof without depending on it.
const workInputDecisionGuardSources = `(` + workInputDecisionSourceSelect + `)`
const workInputDecisionGuardTaskScope = decisionRequestCurrentTaskWithReturnStart + workInputDecisionGuardSources + decisionRequestCurrentTaskWithReturnEnd
const workInputDecisionGuardAnswerScope = decisionAnswerCurrentVersion + workInputDecisionGuardTaskScope + `)`

// Apply after B's impact ledger and both F ledgers. Do not rewrite, populate,
// or reinterpret either legacy table. The reserved ID namespace prevents even
// a pre-bridge non-Mandate row with coincidentally exact provenance from
// becoming current. An unexpected occupant aborts installation, not history.
const workInputDecisionSchema = `
CREATE TABLE decision_work_input_preflight(ok INTEGER CHECK(ok=1));
INSERT INTO decision_work_input_preflight SELECT 0 WHERE EXISTS(
    SELECT 1 FROM decision_requests WHERE id GLOB '` + workInputDecisionPrefix + `*');
DROP TABLE decision_work_input_preflight;

-- An unimpacted return may predate B3. Only a notice published after this
-- installation can enroll, even if the pre-install author is still live.
CREATE TABLE decision_work_input_boundary(sequence INTEGER PRIMARY KEY CHECK(sequence>=0)) STRICT, WITHOUT ROWID;
INSERT INTO decision_work_input_boundary SELECT coalesce(max(sequence),0) FROM message_log;
CREATE TRIGGER decision_work_input_boundary_no_insert BEFORE INSERT ON decision_work_input_boundary
BEGIN SELECT RAISE(ABORT,'交回待决事项安装边界不可修改'); END;
CREATE TRIGGER decision_work_input_boundary_no_update BEFORE UPDATE ON decision_work_input_boundary
BEGIN SELECT RAISE(ABORT,'交回待决事项安装边界不可修改'); END;
CREATE TRIGGER decision_work_input_boundary_no_delete BEFORE DELETE ON decision_work_input_boundary
BEGIN SELECT RAISE(ABORT,'交回待决事项安装边界不可修改'); END;

CREATE VIEW ` + workInputDecisionSources + ` AS ` + workInputDecisionSourceSelect + `;

DROP INDEX decision_requests_task_slot;
CREATE UNIQUE INDEX decision_requests_task_origin_slot ON decision_requests(
    group_id,topic_id,task_id,task_revision,assignment_id,mandate_id,authority_generation,owner_member_id,kind,origin_trigger_id)
    WHERE task_id IS NOT NULL;
DROP TRIGGER decision_requests_insert;
CREATE TRIGGER decision_requests_insert_by_origin BEFORE INSERT ON decision_requests
BEGIN
    SELECT RAISE(ABORT,'待决事项不可替换') WHERE EXISTS(SELECT 1 FROM decision_requests WHERE id=NEW.id)
        OR EXISTS(SELECT 1 FROM decision_requests WHERE chat_source_id=NEW.chat_source_id)
        OR EXISTS(SELECT 1 FROM decision_requests WHERE task_id IS NOT NULL
            AND (group_id,topic_id,task_id,task_revision,assignment_id,mandate_id,authority_generation,owner_member_id,kind,origin_trigger_id)=
                (NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision,NEW.assignment_id,NEW.mandate_id,
                 NEW.authority_generation,NEW.owner_member_id,NEW.kind,NEW.origin_trigger_id));
    SELECT RAISE(ABORT,'待决事项来源不匹配') WHERE NOT EXISTS(SELECT 1 FROM message_log
        WHERE (message_id,group_id,topic_id,sequence)=(NEW.source_message_id,NEW.group_id,NEW.topic_id,NEW.source_sequence));
    SELECT RAISE(ABORT,'待决事项来源不匹配') WHERE CASE
        WHEN NEW.id GLOB '` + workInputDecisionPrefix + `*' THEN NEW.kind IS NOT 'authorization' OR NOT EXISTS(
            SELECT 1 FROM ` + workInputDecisionGuardSources + ` b WHERE ` + workInputDecisionBinding + `=
                (NEW.origin_trigger_id,NEW.group_id,NEW.topic_id,NEW.task_id,NEW.task_revision,NEW.assignment_id,NEW.mandate_id,
                 NEW.authority_generation,NEW.owner_member_id,NEW.source_message_id,NEW.source_sequence,NEW.created_at)
            AND ` + workInputDecisionEnrolling + `)
        WHEN NEW.task_id IS NOT NULL THEN NOT EXISTS(
            SELECT 1 FROM mandates m JOIN coordination_triggers o ON o.id=m.origin_trigger_id
            WHERE (m.id,m.origin_trigger_id,o.message_id,o.message_sequence)=(NEW.mandate_id,NEW.origin_trigger_id,NEW.source_message_id,NEW.source_sequence))
        ELSE 0 END;
END;

CREATE TRIGGER decision_work_input_versions_insert BEFORE INSERT ON decision_request_versions
WHEN EXISTS(SELECT 1 FROM decision_requests q WHERE q.id=NEW.request_id AND q.task_id IS NOT NULL
    AND NOT EXISTS(SELECT 1 FROM mandates m WHERE (m.id,m.origin_trigger_id)=(q.mandate_id,q.origin_trigger_id)))
BEGIN
    SELECT RAISE(ABORT,'待决事项与工作输入交回记录不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM decision_requests q JOIN ` + workInputDecisionGuardSources + ` b ON b.trigger_id=q.origin_trigger_id
        WHERE q.id=NEW.request_id AND ` + workInputDecisionIDMatch + ` AND ` + workInputDecisionRequestMatch + ` AND ` + workInputDecisionEnrolling + `
        AND NEW.version=1 AND NEW.expires_at IS NULL
        AND ` + workInputDecisionVersionFields + `=(NEW.author_member_id,NEW.author_turn_id,NEW.author_attempt_id,NEW.created_at,
             json_extract(NEW.question_json,'$.question'),json_extract(NEW.question_json,'$.facts')));
END;

DROP TRIGGER decision_answers_insert;
DROP TRIGGER decision_answer_replacements_insert;
DROP TRIGGER decision_answer_consumptions_insert;
CREATE TRIGGER decision_answers_insert_by_origin` + decisionAnswerInsertGuard + workInputDecisionGuardTaskScope + decisionAnswerInsertEnd + `
CREATE TRIGGER decision_answer_replacements_insert_by_origin` + decisionAnswerReplacementGuard + workInputDecisionGuardAnswerScope + decisionAnswerReplacementEnd + `
CREATE TRIGGER decision_answer_consumptions_insert_by_origin` + decisionAnswerConsumptionGuard + workInputDecisionGuardAnswerScope + decisionAnswerConsumptionEnd + `
`

// The bridge replaces only the current-origin predicates of these guards.
// Keep the legacy DDL and all other answer, wake and ACK constraints intact.
const decisionAnswerInsertGuard = ` BEFORE INSERT ON decision_answers
BEGIN
    SELECT RAISE(ABORT,'已保存的决定不可替换') WHERE EXISTS(SELECT 1 FROM decision_answers WHERE
        id=NEW.id OR request_id=NEW.request_id OR reply_message_id=NEW.reply_message_id OR wake_delivery_id=NEW.wake_delivery_id)
        OR EXISTS(SELECT 1 FROM task_work_inputs WHERE id=NEW.id)
        OR EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=NEW.wake_delivery_id);
    SELECT RAISE(ABORT,'决定与当前待决范围或用户原话不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM decision_requests q JOIN decision_request_versions v ON (v.request_id,v.version)=(q.id,NEW.version)
        JOIN tasks t ON (t.id,t.group_id)=(q.task_id,q.group_id)
        JOIN messages reply ON (reply.id,reply.group_id,reply.topic_id,reply.sender_id,reply.content)=
            (NEW.reply_message_id,q.group_id,q.topic_id,NEW.actor_id,NEW.text)
        JOIN message_log l ON (l.message_id,l.group_id,l.topic_id)=(reply.id,q.group_id,q.topic_id)
        WHERE q.id=NEW.request_id AND q.recipient_id=NEW.actor_id AND q.relay='task_owner'
        AND reply.created_at=NEW.created_at AND l.sequence>q.source_sequence
        AND q.withdrawn_at IS NULL AND (v.expires_at IS NULL OR v.expires_at>NEW.created_at)
        AND v.version=(SELECT max(version) FROM decision_request_versions WHERE request_id=q.id)
        AND (NEW.option_index IS NULL OR NEW.text IS json_extract(v.question_json,'$.choices['||(NEW.option_index-1)||']'))
        AND NOT EXISTS(SELECT 1 FROM deliveries WHERE message_id=reply.id)
        AND NOT EXISTS(SELECT 1 FROM message_command_sources WHERE message_id=reply.id)
        AND (`
const decisionAnswerInsertEnd = `));
END;`

const decisionAnswerReplacementGuard = ` BEFORE INSERT ON decision_answer_wake_replacements
BEGIN
    SELECT RAISE(ABORT,'决定补投不可替换') WHERE EXISTS(SELECT 1 FROM decision_answer_wake_replacements
        WHERE answer_id=NEW.answer_id OR prior_turn_id=NEW.prior_turn_id OR delivery_id=NEW.delivery_id)
        OR EXISTS(SELECT 1 FROM task_work_deliveries WHERE delivery_id=NEW.delivery_id);
    SELECT RAISE(ABORT,'决定补投与原投递不匹配') WHERE NOT EXISTS(
        SELECT 1` + decisionAnswerEvidence + `
        JOIN tasks t ON (t.id,t.group_id)=(q.task_id,q.group_id)
        JOIN deliveries next ON (next.id,next.group_id,next.topic_id,next.conversation_id,next.recipient_id)=
            (NEW.delivery_id,work.group_id,work.topic_id,work.conversation_id,work.member_id)
        WHERE ans.id=NEW.answer_id AND next.turn_id='' AND next.state='queued'
        AND ` + decisionAnswerProof + ` AND `
const decisionAnswerReplacementEnd = `
        AND NEW.created_at>=ans.created_at AND next.created_at>=NEW.created_at AND (v.expires_at IS NULL OR v.expires_at>NEW.created_at)
        AND NOT EXISTS(SELECT 1 FROM decision_answer_consumptions WHERE answer_id=ans.id)
        AND EXISTS(SELECT 1` + decisionAnswerPriorTurn + ` AND h.id=NEW.prior_turn_id
            AND prior.state='queued' AND h.state='starting' AND h.accepted_at IS NULL AND h.native_session_id=''
            AND h.candidate_text='' AND h.candidate_terminal='' AND h.receipt_json=''
            AND NOT EXISTS(SELECT 1 FROM native_run_bindings WHERE turn_id=h.id)));
END;`

const decisionAnswerConsumptionGuard = ` BEFORE INSERT ON decision_answer_consumptions
BEGIN
    SELECT RAISE(ABORT,'用户决定处理回执不可替换') WHERE EXISTS(SELECT 1 FROM decision_answer_consumptions WHERE answer_id=NEW.answer_id);
    SELECT RAISE(ABORT,'用户决定处理回执不匹配') WHERE NOT EXISTS(
        SELECT 1` + decisionAnswerEvidence + decisionAnswerReplacementJoin + `
        JOIN tasks t ON (t.id,t.group_id)=(q.task_id,q.group_id)
        JOIN task_work_continuations c ON c.delivery_id IN (work.delivery_id,replacement.delivery_id)
        AND (c.turn_id,c.attempt_id)=(NEW.turn_id,NEW.attempt_id)
        JOIN turns w ON (w.id,w.group_id,w.member_id,w.delivery_id)=(c.turn_id,q.group_id,q.owner_member_id,c.delivery_id)
        WHERE ans.id=NEW.answer_id AND NEW.consumed_at>=ans.created_at AND t.active_attempt_id=c.attempt_id
        AND w.state IN ('starting','running') AND w.cancel_requested_at IS NULL AND w.candidate_terminal=''
        AND ` + decisionAnswerProof + ` AND `
const decisionAnswerConsumptionEnd = `
        AND (v.expires_at IS NULL OR v.expires_at>NEW.consumed_at));
END;`
