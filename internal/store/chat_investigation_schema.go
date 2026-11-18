package store

// An optional extension of a single immutable chat Policy revision. The existing
// assessment/decision/Mandate chain binds a Task to that revision at admission.
// Its provenance guard follows messageSourcesInTx's source precedence.
const chatInvestigationSchema = `
CREATE TABLE chat_investigation_policy_revisions (
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    scope_json TEXT NOT NULL CHECK(json_valid(scope_json) AND json_type(scope_json)='object'),
    PRIMARY KEY(group_id,topic_id,revision),
    FOREIGN KEY(group_id,topic_id,revision) REFERENCES chat_policy_scopes(group_id,topic_id,revision)
);
CREATE TRIGGER chat_investigation_policy_insert BEFORE INSERT ON chat_investigation_policy_revisions
WHEN EXISTS(SELECT 1 FROM chat_investigation_policy_revisions WHERE group_id=NEW.group_id AND topic_id=NEW.topic_id AND revision=NEW.revision)
 OR EXISTS(SELECT 1 FROM chat_policies WHERE group_id=NEW.group_id AND topic_id=NEW.topic_id AND revision>=NEW.revision)
BEGIN SELECT RAISE(ABORT, '只读调查策略不可补授或替换'); END;
CREATE TRIGGER chat_investigation_policy_update BEFORE UPDATE ON chat_investigation_policy_revisions
BEGIN SELECT RAISE(ABORT, '只读调查策略不可修改'); END;
CREATE TRIGGER chat_investigation_policy_delete BEFORE DELETE ON chat_investigation_policy_revisions
BEGIN SELECT RAISE(ABORT, '只读调查策略不可删除'); END;
CREATE TRIGGER chat_investigation_assessment_insert BEFORE INSERT ON chat_assessments
WHEN NEW.decision_id IS NOT NULL AND json_extract(NEW.receipt_json,'$.action')='read_only_investigation'
BEGIN
 SELECT RAISE(ABORT, '只读调查必须绑定当前本机用户策略和真实来源') WHERE NOT EXISTS(
  SELECT 1 FROM chat_candidates c JOIN chat_intake_executions e ON e.candidate_id=c.id AND e.turn_id=c.turn_id
  JOIN messages msg ON msg.id=c.message_id AND msg.group_id=c.group_id AND msg.topic_id=c.topic_id
  LEFT JOIN message_command_sources src ON src.message_id=msg.id
  JOIN chat_policies p ON p.group_id=c.group_id AND p.topic_id=c.topic_id AND p.revision=c.policy_revision
  JOIN chat_policy_revisions pr ON pr.group_id=p.group_id AND pr.topic_id=p.topic_id AND pr.revision=p.revision
  JOIN chat_policy_scopes s ON s.group_id=p.group_id AND s.topic_id=p.topic_id AND s.revision=p.revision
  JOIN chat_investigation_policy_revisions i ON i.group_id=p.group_id AND i.topic_id=p.topic_id AND i.revision=p.revision
  JOIN message_command_sources cmd ON cmd.id=s.command_source_id AND cmd.kind='local_user_policy'
  JOIN mandates m ON m.decision_id=NEW.decision_id AND m.group_id=c.group_id AND m.topic_id=c.topic_id
  JOIN coordination_triggers t ON t.id=m.origin_trigger_id AND t.message_id=msg.id AND t.group_id=msg.group_id AND t.topic_id=msg.topic_id
  JOIN coordination_dispatch_requests d ON d.id=NEW.dispatch_request_id AND d.mandate_id=m.id
   AND d.task_id=m.task_id AND d.group_id=m.group_id AND d.topic_id=m.topic_id AND d.task_revision=m.task_revision_at_admission
   AND d.binding_revision=c.binding_revision AND d.policy_revision=c.dispatch_policy_revision
  JOIN tasks task ON task.id=d.task_id AND task.group_id=d.group_id AND task.revision=d.task_revision AND task.status='open'
  JOIN task_coordination tc ON tc.task_id=d.task_id AND tc.group_id=d.group_id AND tc.topic_id=d.topic_id
   AND tc.current_mandate_id=m.id AND tc.authority_generation=d.authority_generation
  JOIN group_coordinator_bindings cb ON cb.group_id=c.group_id AND cb.revision=c.binding_revision
  JOIN group_dispatch_policies dp ON dp.group_id=c.group_id AND dp.revision=c.dispatch_policy_revision
  WHERE c.id=NEW.candidate_id AND pr.enabled=1 AND e.purpose='chat_intake/v1' AND msg.id<>NEW.notice_message_id
  AND NOT EXISTS(SELECT 1 FROM chat_review_notices WHERE message_id=msg.id)
  AND NOT EXISTS(SELECT 1 FROM workspace_operations WHERE notice_message_id=msg.id AND group_id=msg.group_id)
  AND NOT EXISTS(SELECT 1 FROM workspace_operation_receipts wr JOIN workspace_operations w ON w.id=wr.operation_id
   WHERE wr.notice_message_id=msg.id AND w.group_id=msg.group_id)
  AND ((e.source_kind='local_user_chat' AND src.kind=e.source_kind AND src.id=e.source_id AND msg.sender_id='user')
   OR (e.source_kind='member' AND EXISTS(
    SELECT 1 FROM turns rt JOIN conversations rc ON rc.id=rt.conversation_id AND rc.group_id=rt.group_id AND rc.member_id=rt.member_id
    WHERE rt.id=CASE WHEN src.id IS NOT NULL THEN src.turn_id ELSE substr(e.source_id,6) END
    AND rt.group_id=msg.group_id AND rt.member_id=msg.sender_id AND rc.topic_id=msg.topic_id
    AND ((src.kind=e.source_kind AND src.id=e.source_id) OR
     (src.id IS NULL AND rt.reply_message_id=msg.id AND e.source_id='turn:'||rt.id
      AND NOT EXISTS(SELECT 1 FROM coordination_dispatch_state WHERE message_id=msg.id)
      AND NOT EXISTS(SELECT 1 FROM coordination_assignment_dispatch WHERE message_id=msg.id)
      AND NOT EXISTS(SELECT 1 FROM chat_assessments WHERE notice_message_id=msg.id)
      AND NOT EXISTS(SELECT 1 FROM chat_submission_notices WHERE message_id=msg.id))))))
  AND m.objective=json_extract(i.scope_json,'$.objective')
  AND json_extract(NEW.receipt_json,'$.id') IS NEW.id
  AND json_extract(NEW.receipt_json,'$.candidate_id') IS c.id
  AND json_extract(NEW.receipt_json,'$.group_id') IS msg.group_id
  AND json_extract(NEW.receipt_json,'$.topic_id') IS msg.topic_id
  AND json_extract(NEW.receipt_json,'$.message_id') IS msg.id
  AND json_extract(NEW.receipt_json,'$.source_sender_id') IS msg.sender_id
  AND json_extract(NEW.receipt_json,'$.assessed_by') IS c.member_id
  AND json_extract(NEW.receipt_json,'$.turn_id') IS c.turn_id
  AND json_extract(NEW.receipt_json,'$.candidate_generation') IS c.generation
  AND json_extract(NEW.receipt_json,'$.task_id') IS m.task_id
  AND json_extract(NEW.receipt_json,'$.mandate_id') IS m.id
  AND json_extract(NEW.receipt_json,'$.dispatch_request_id') IS d.id
  AND json_extract(NEW.receipt_json,'$.notice_message_id') IS NEW.notice_message_id
  AND json_extract(NEW.receipt_json,'$.policy_revision') IS c.policy_revision
  AND json_extract(NEW.receipt_json,'$.authorization_message_id') IS cmd.message_id
  AND json_extract(NEW.receipt_json,'$.authority') IS 'written_preauthorization'
  AND json_extract(NEW.receipt_json,'$.authority_evidence_verified') IS 1
  AND json_extract(NEW.receipt_json,'$.disposition') IS 'prepare_work'
  AND json_extract(NEW.receipt_json,'$.outcome') IS 'admitted'
  AND json_extract(NEW.receipt_json,'$.purpose') IS e.purpose
  AND json_extract(NEW.receipt_json,'$.source_id') IS e.source_id
  AND json_extract(NEW.receipt_json,'$.source_kind') IS e.source_kind);
END;
CREATE TRIGGER chat_investigation_artifact_insert BEFORE INSERT ON artifact_versions
WHEN NEW.source_task_id IS NOT NULL AND EXISTS(
 SELECT 1 FROM mandates m INDEXED BY mandates_group_task JOIN chat_assessments a ON a.decision_id=m.decision_id
 WHERE m.group_id=NEW.group_id AND m.task_id=NEW.source_task_id AND json_extract(a.receipt_json,'$.action')='read_only_investigation')
BEGIN SELECT RAISE(ABORT, '只读调查任务不得发布成果版本'); END;
`
