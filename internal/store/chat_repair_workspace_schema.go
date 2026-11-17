package store

// Forward v22 extension of the v20 guards. Normal admission retains the same
// checks; the additional source branch requires an already-bound repair link.
const chatRepairWorkspaceSchemaV22 = `
DROP TRIGGER chat_workspace_scopes_authority;
CREATE TRIGGER chat_workspace_scopes_authority BEFORE INSERT ON chat_workspace_scopes
WHEN NOT EXISTS(
 SELECT 1 FROM chat_candidates c
 JOIN chat_intake_executions e ON e.candidate_id=c.id AND e.purpose='chat_intake/v1'
 JOIN messages msg ON msg.id=c.message_id AND msg.sender_id='user'
 JOIN mandates m ON m.id=NEW.mandate_id AND m.task_id=NEW.task_id AND m.group_id=NEW.group_id
 AND m.topic_id=NEW.topic_id AND m.task_revision_at_admission=NEW.task_revision
 JOIN coordination_triggers t ON t.id=m.origin_trigger_id AND t.message_id=c.message_id
 JOIN coordination_dispatch_requests d ON d.id=NEW.dispatch_request_id AND d.mandate_id=m.id
 AND d.task_id=m.task_id AND d.group_id=m.group_id AND d.topic_id=m.topic_id AND d.task_revision=NEW.task_revision
 AND d.binding_revision=c.binding_revision AND d.policy_revision=c.dispatch_policy_revision
 JOIN chat_policies p ON p.group_id=c.group_id AND p.topic_id=c.topic_id AND p.revision=c.policy_revision
 JOIN chat_policy_revisions pr ON pr.group_id=p.group_id AND pr.topic_id=p.topic_id AND pr.revision=p.revision
 JOIN group_coordinator_bindings cb ON cb.group_id=c.group_id AND cb.revision=c.binding_revision
 JOIN group_dispatch_policies dp ON dp.group_id=c.group_id AND dp.revision=c.dispatch_policy_revision
 WHERE c.id=NEW.candidate_id AND c.group_id=NEW.group_id AND c.topic_id=NEW.topic_id
 AND c.policy_revision=NEW.policy_revision AND pr.enabled=1 AND (
 EXISTS(SELECT 1 FROM message_command_sources src WHERE src.id=e.source_id AND src.message_id=c.message_id
 AND src.kind='local_user_chat' AND e.source_kind=src.kind)
 OR EXISTS(SELECT 1 FROM chat_repair_links l WHERE l.repair_task_id=NEW.task_id AND l.group_id=NEW.group_id
 AND l.topic_id=NEW.topic_id AND l.candidate_id=c.id AND l.assessment_id=NEW.assessment_id
 AND l.mandate_id=m.id AND l.dispatch_request_id=d.id AND l.policy_revision=c.policy_revision
 AND l.authorization IN ('review_request','original_agreement')
 AND e.source_kind='platform' AND e.source_id='review:'||l.parent_review_id)))
BEGIN SELECT RAISE(ABORT,'目录范围必须绑定真实用户来源或正式拒收修复关联'); END;
CREATE TRIGGER chat_repair_workspace_scopes_insert BEFORE INSERT ON chat_workspace_scopes
WHEN EXISTS(SELECT 1 FROM chat_repair_links WHERE repair_task_id=NEW.task_id)
 AND NOT EXISTS(SELECT 1 FROM chat_repair_links l JOIN chat_workspace_scopes parent ON parent.task_id=l.parent_task_id
 AND parent.group_id=l.group_id AND parent.topic_id=l.topic_id
 WHERE l.repair_task_id=NEW.task_id AND parent.resource_key=NEW.resource_key AND parent.node_id=NEW.node_id)
BEGIN SELECT RAISE(ABORT,'目录修复不得扩大原物理范围'); END;
CREATE TRIGGER chat_repair_workspace_grants_insert BEFORE INSERT ON chat_workspace_scope_grants
WHEN EXISTS(SELECT 1 FROM chat_repair_links WHERE repair_task_id=NEW.task_id)
 AND NOT EXISTS(SELECT 1 FROM chat_repair_links l JOIN chat_workspace_scope_grants parent ON parent.task_id=l.parent_task_id
 AND parent.group_id=l.group_id WHERE l.repair_task_id=NEW.task_id AND parent.grant_id=NEW.grant_id
 AND parent.member_id=NEW.member_id AND parent.resource_key=NEW.resource_key AND parent.node_id=NEW.node_id
 AND parent.root=NEW.root AND parent.resolved_root=NEW.resolved_root AND parent.grant_schema_version=NEW.grant_schema_version)
BEGIN SELECT RAISE(ABORT,'目录修复成员必须来自原正式授权集合'); END;
DROP TRIGGER chat_workspace_assessments_insert;
CREATE TRIGGER chat_workspace_assessments_insert BEFORE INSERT ON chat_assessments
WHEN (NEW.decision_id IS NOT NULL AND json_extract(NEW.receipt_json,'$.action')='workspace_change')
 OR EXISTS(SELECT 1 FROM chat_workspace_scopes WHERE assessment_id=NEW.id OR candidate_id=NEW.candidate_id)
BEGIN
 SELECT RAISE(ABORT,'目录工作回执必须匹配范围和真实来源') WHERE NOT EXISTS(
 SELECT 1 FROM chat_workspace_scopes s JOIN chat_candidates c ON c.id=s.candidate_id
 JOIN chat_intake_executions e ON e.candidate_id=c.id
 JOIN mandates m ON m.id=s.mandate_id AND m.decision_id=NEW.decision_id
 WHERE s.assessment_id=NEW.id AND s.candidate_id=NEW.candidate_id AND s.dispatch_request_id=NEW.dispatch_request_id
 AND json_extract(NEW.receipt_json,'$.id') IS NEW.id
 AND json_extract(NEW.receipt_json,'$.candidate_id') IS c.id
 AND json_extract(NEW.receipt_json,'$.group_id') IS s.group_id
 AND json_extract(NEW.receipt_json,'$.topic_id') IS s.topic_id
 AND json_extract(NEW.receipt_json,'$.task_id') IS s.task_id
 AND json_extract(NEW.receipt_json,'$.mandate_id') IS s.mandate_id
 AND json_extract(NEW.receipt_json,'$.dispatch_request_id') IS s.dispatch_request_id
 AND json_extract(NEW.receipt_json,'$.notice_message_id') IS NEW.notice_message_id
 AND json_extract(NEW.receipt_json,'$.action') IS 'workspace_change'
 AND json_extract(NEW.receipt_json,'$.disposition') IS 'request_work'
 AND json_extract(NEW.receipt_json,'$.outcome') IS 'admitted'
 AND json_extract(NEW.receipt_json,'$.authority') IS 'explicit_request'
 AND json_extract(NEW.receipt_json,'$.authority_evidence_verified') IS 1
 AND json_extract(NEW.receipt_json,'$.authorization_message_id') IS CASE
 WHEN EXISTS(SELECT 1 FROM chat_repair_links l WHERE l.assessment_id=NEW.id)
 THEN coalesce((SELECT l.authorization_message_id FROM chat_repair_links l WHERE l.assessment_id=NEW.id),'') ELSE c.message_id END
 AND json_extract(NEW.receipt_json,'$.message_id') IS c.message_id
 AND json_extract(NEW.receipt_json,'$.source_sender_id') IS 'user'
 AND json_extract(NEW.receipt_json,'$.assessed_by') IS c.member_id
 AND json_extract(NEW.receipt_json,'$.turn_id') IS c.turn_id
 AND json_extract(NEW.receipt_json,'$.candidate_generation') IS c.generation
 AND json_extract(NEW.receipt_json,'$.policy_revision') IS s.policy_revision
 AND json_extract(NEW.receipt_json,'$.source_id') IS e.source_id
 AND json_extract(NEW.receipt_json,'$.source_kind') IS CASE
 WHEN EXISTS(SELECT 1 FROM chat_repair_links l WHERE l.assessment_id=NEW.id) THEN e.source_kind ELSE 'local_user_chat' END
 AND json_extract(NEW.receipt_json,'$.purpose') IS 'chat_intake/v1'
 AND json_extract(NEW.receipt_json,'$.workspace_scope.task_id') IS s.task_id
 AND json_extract(NEW.receipt_json,'$.workspace_scope.group_id') IS s.group_id
 AND json_extract(NEW.receipt_json,'$.workspace_scope.topic_id') IS s.topic_id
 AND json_extract(NEW.receipt_json,'$.workspace_scope.task_revision') IS s.task_revision
 AND json_extract(NEW.receipt_json,'$.workspace_scope.mandate_id') IS s.mandate_id
 AND json_extract(NEW.receipt_json,'$.workspace_scope.candidate_id') IS c.id
 AND json_extract(NEW.receipt_json,'$.workspace_scope.assessment_id') IS s.assessment_id
 AND json_extract(NEW.receipt_json,'$.workspace_scope.dispatch_request_id') IS s.dispatch_request_id
 AND json_extract(NEW.receipt_json,'$.workspace_scope.policy_revision') IS s.policy_revision
 AND json_extract(NEW.receipt_json,'$.workspace_scope.resource_key') IS s.resource_key
 AND json_extract(NEW.receipt_json,'$.workspace_scope.node_id') IS s.node_id);
 SELECT RAISE(ABORT,'目录工作回执成员集合不匹配') WHERE
 coalesce(json_type(NEW.receipt_json,'$.workspace_scope.grants'),'')<>'array'
 OR json_array_length(NEW.receipt_json,'$.workspace_scope.grants')=0
 OR json_array_length(NEW.receipt_json,'$.workspace_scope.grants')<>
 (SELECT count(*) FROM chat_workspace_scope_grants g JOIN chat_workspace_scopes s ON s.task_id=g.task_id WHERE s.assessment_id=NEW.id)
 OR EXISTS(SELECT 1 FROM json_each(NEW.receipt_json,'$.workspace_scope.grants') j WHERE NOT EXISTS(
 SELECT 1 FROM chat_workspace_scope_grants g JOIN chat_workspace_scopes s ON s.task_id=g.task_id
 WHERE s.assessment_id=NEW.id AND json_extract(j.value,'$.grant_id') IS g.grant_id
 AND json_extract(j.value,'$.member_id') IS g.member_id AND json_extract(j.value,'$.root') IS g.root
 AND json_extract(j.value,'$.resolved_root') IS g.resolved_root
 AND json_extract(j.value,'$.grant_schema_version') IS g.grant_schema_version))
 OR EXISTS(SELECT json_extract(value,'$.member_id') FROM json_each(NEW.receipt_json,'$.workspace_scope.grants')
 GROUP BY json_extract(value,'$.member_id') HAVING count(*)>1);
 SELECT RAISE(ABORT,'目录范围必须保留完整的服务端可用成员集合') WHERE EXISTS(
 SELECT 1 FROM chat_candidates c
 JOIN group_dispatch_policy_revisions p ON p.group_id=c.group_id AND p.revision=c.dispatch_policy_revision
 JOIN json_each(p.allowed_worker_ids) w ON w.type='text'
 JOIN group_coordinator_binding_revisions b ON b.group_id=c.group_id AND b.revision=c.binding_revision
 JOIN members m ON m.group_id=c.group_id AND m.id=w.value AND m.status='active' AND m.id<>b.member_id
 JOIN workspace_write_grants g ON g.group_id=c.group_id AND g.member_id=m.id
 WHERE c.id=NEW.candidate_id AND (
 NOT EXISTS(SELECT 1 FROM chat_repair_links l WHERE l.assessment_id=NEW.id)
 OR EXISTS(SELECT 1 FROM chat_repair_links l JOIN chat_workspace_scope_grants parent ON parent.task_id=l.parent_task_id
 AND parent.group_id=l.group_id WHERE l.assessment_id=NEW.id AND parent.grant_id=g.id))
 AND NOT EXISTS(SELECT 1 FROM chat_workspace_scope_grants sg JOIN chat_workspace_scopes s ON s.task_id=sg.task_id
 WHERE s.assessment_id=NEW.id AND sg.grant_id=g.id));
END;
`
