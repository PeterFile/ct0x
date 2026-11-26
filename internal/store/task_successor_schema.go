package store

const taskSuccessorSchema = `
CREATE TABLE task_successors (
    successor_task_id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    predecessor_task_id TEXT NOT NULL CHECK(predecessor_task_id<>successor_task_id),
    predecessor_task_revision INTEGER NOT NULL CHECK(predecessor_task_revision>0),
    predecessor_submission_id TEXT NOT NULL REFERENCES task_submissions(id),
    predecessor_review_id TEXT NOT NULL REFERENCES task_review_decisions(id),
    predecessor_mandate_id TEXT NOT NULL,
    successor_mandate_id TEXT NOT NULL,
    successor_dispatch_request_id TEXT NOT NULL UNIQUE,
    source_trigger_id TEXT NOT NULL,
    chat_policy_revision INTEGER NOT NULL CHECK(chat_policy_revision>0),
    FOREIGN KEY(predecessor_task_id,group_id) REFERENCES tasks(id,group_id),
    FOREIGN KEY(successor_task_id,group_id) REFERENCES tasks(id,group_id),
    FOREIGN KEY(predecessor_mandate_id,predecessor_task_id,group_id,topic_id) REFERENCES mandates(id,task_id,group_id,topic_id),
    FOREIGN KEY(successor_mandate_id,successor_task_id,group_id,topic_id) REFERENCES mandates(id,task_id,group_id,topic_id),
    FOREIGN KEY(successor_dispatch_request_id,successor_task_id,group_id) REFERENCES coordination_dispatch_requests(id,task_id,group_id),
    FOREIGN KEY(source_trigger_id,group_id,topic_id) REFERENCES coordination_triggers(id,group_id,topic_id),
    FOREIGN KEY(group_id,topic_id,chat_policy_revision) REFERENCES chat_policy_revisions(group_id,topic_id,revision)
) STRICT, WITHOUT ROWID;
CREATE TRIGGER task_successors_guard BEFORE INSERT ON task_successors BEGIN
    SELECT RAISE(ABORT,'续办关系不可替换') WHERE EXISTS(SELECT 1 FROM task_successors
      WHERE successor_task_id=NEW.successor_task_id OR successor_dispatch_request_id=NEW.successor_dispatch_request_id);
    SELECT RAISE(ABORT,'续办来源必须是同话题确切已接纳提交') WHERE NOT EXISTS(
      SELECT 1 FROM tasks t JOIN task_submissions s ON (s.group_id,s.task_id,s.task_revision,s.id)=
        (t.group_id,t.id,t.revision,t.current_submission_id)
      JOIN task_review_decisions r ON (r.group_id,r.task_id,r.task_revision,r.submission_id)=(s.group_id,s.task_id,s.task_revision,s.id)
      JOIN task_coordination c ON (c.group_id,c.task_id)=(t.group_id,t.id)
      WHERE (t.group_id,t.id,t.revision,s.id,r.id,c.topic_id,c.current_mandate_id)=
        (NEW.group_id,NEW.predecessor_task_id,NEW.predecessor_task_revision,NEW.predecessor_submission_id,
         NEW.predecessor_review_id,NEW.topic_id,NEW.predecessor_mandate_id)
      AND t.status='accepted' AND t.active_attempt_id IS NULL AND r.outcome='accepted' AND r.reviewed_by='user');
    SELECT RAISE(ABORT,'续办必须绑定新的明确用户授权和分派') WHERE NOT EXISTS(
      SELECT 1 FROM mandates m JOIN routing_decisions d ON d.id=m.decision_id AND d.mandate_id=m.id
      JOIN coordination_triggers o ON (o.id,o.group_id,o.topic_id)=(m.origin_trigger_id,m.group_id,m.topic_id)
      JOIN message_command_sources src ON src.message_id=o.message_id AND src.kind='local_user_chat'
      JOIN coordination_dispatch_requests q ON (q.group_id,q.topic_id,q.task_id,q.mandate_id)=(m.group_id,m.topic_id,m.task_id,m.id)
      JOIN coordination_dispatch_state st ON st.request_id=q.id AND st.status='pending'
      JOIN chat_policies p ON (p.group_id,p.topic_id)=(m.group_id,m.topic_id)
      JOIN chat_policy_revisions pr ON (pr.group_id,pr.topic_id,pr.revision)=(p.group_id,p.topic_id,p.revision)
      WHERE (m.group_id,m.topic_id,m.task_id,m.id,o.id,q.id,p.revision)=
        (NEW.group_id,NEW.topic_id,NEW.successor_task_id,NEW.successor_mandate_id,NEW.source_trigger_id,
         NEW.successor_dispatch_request_id,NEW.chat_policy_revision)
      AND m.authority_kind='local_user_explicit' AND m.authorized_by='user' AND d.kind='create_new' AND d.actor_id='user'
      AND q.requested_by='user' AND q.task_revision=m.task_revision_at_admission AND pr.enabled=1
      AND pr.authorized_by='user' AND pr.action='text_submission');
END;
CREATE TRIGGER task_successors_immutable_update BEFORE UPDATE ON task_successors
BEGIN SELECT RAISE(ABORT,'续办关系不可修改'); END;
CREATE TRIGGER task_successors_immutable_delete BEFORE DELETE ON task_successors
BEGIN SELECT RAISE(ABORT,'续办关系不可删除'); END;

DROP TRIGGER decision_answers_insert_by_origin;
DROP TRIGGER decision_answer_replacements_insert_by_origin;
DROP TRIGGER decision_answer_consumptions_insert_by_origin;
CREATE TRIGGER decision_answers_insert_by_origin` + decisionAnswerInsertGuard + taskSuccessorDecisionGuardTaskScope + decisionAnswerInsertEnd + `
CREATE TRIGGER decision_answer_replacements_insert_by_origin` + decisionAnswerReplacementGuard + taskSuccessorDecisionGuardAnswerScope + decisionAnswerReplacementEnd + `
CREATE TRIGGER decision_answer_consumptions_insert_by_origin` + decisionAnswerConsumptionGuard + taskSuccessorDecisionGuardAnswerScope + decisionAnswerConsumptionEnd + `
`

// Successors may return a new work input to F. This does not create a new
// source for ordinary decision proposals or replace the original user command.
const taskSuccessorDecisionScopeStart = workInputDecisionIDMatch + ` AND ` + decisionRequestTaskScope + `
 JOIN mandates m ON (m.id,m.authorized_by)=(q.mandate_id,q.recipient_id)` + decisionRequestTaskCurrentJoins + `
 JOIN task_successors successor ON (successor.successor_task_id,successor.group_id,successor.topic_id,
    successor.successor_mandate_id,successor.successor_dispatch_request_id,successor.source_trigger_id)=
    (q.task_id,q.group_id,q.topic_id,q.mandate_id,r.id,m.origin_trigger_id)
 JOIN coordination_triggers authorization ON (authorization.id,authorization.group_id,authorization.topic_id,authorization.source_kind)=
    (successor.source_trigger_id,q.group_id,q.topic_id,'local_im')
 JOIN message_command_sources src ON (src.message_id,src.kind)=(authorization.message_id,'local_user_chat')
 JOIN messages source ON (source.id,source.group_id,source.topic_id,source.sender_id)=
    (src.message_id,q.group_id,q.topic_id,q.recipient_id)
 JOIN chat_policies p ON (p.group_id,p.topic_id,p.revision)=(q.group_id,q.topic_id,successor.chat_policy_revision)
 JOIN chat_policy_revisions pr ON (pr.group_id,pr.topic_id,pr.revision)=(p.group_id,p.topic_id,p.revision)` + decisionRequestTaskCurrentAuthority + `
 AND (m.group_id,m.topic_id,m.task_id,m.task_revision_at_admission)=(q.group_id,q.topic_id,q.task_id,q.task_revision)
 AND m.authority_kind='local_user_explicit' AND m.authorized_by='user' AND authorization.message_sequence<q.source_sequence
 AND pr.enabled=1 AND pr.authorized_by='user' AND pr.action='text_submission'
 AND EXISTS(SELECT 1 FROM `
const taskSuccessorDecisionScopeEnd = ` b WHERE ` + workInputDecisionReadMatch + ` AND b.policy_revision=p.revision))`

const decisionRequestCurrentTaskWithSuccessor = `(` + decisionRequestCurrentTaskWithReturn + `) OR (` +
	taskSuccessorDecisionScopeStart + workInputDecisionSources + taskSuccessorDecisionScopeEnd + `)`

// As in v46, database write guards prove the exact source without depending
// on the read projection view. Preserve the previous origin and receipt rules.
const taskSuccessorDecisionGuardTaskScope = `(` + workInputDecisionGuardTaskScope + `) OR (` +
	taskSuccessorDecisionScopeStart + workInputDecisionGuardSources + taskSuccessorDecisionScopeEnd + `)`
const taskSuccessorDecisionGuardAnswerScope = decisionAnswerCurrentVersion + taskSuccessorDecisionGuardTaskScope + `)`
