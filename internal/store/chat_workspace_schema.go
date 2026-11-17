package store

// Workspace authority is an optional extension, never an upgrade of old rows.
const chatWorkspaceSchemaV20 = `
CREATE TABLE chat_workspace_policy_revisions (
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    allow_workspace_changes INTEGER NOT NULL CHECK(allow_workspace_changes=1),
    PRIMARY KEY(group_id,topic_id,revision),
    FOREIGN KEY(group_id,topic_id,revision) REFERENCES chat_policy_scopes(group_id,topic_id,revision)
);
CREATE TRIGGER chat_workspace_policy_revisions_insert BEFORE INSERT ON chat_workspace_policy_revisions
WHEN EXISTS(SELECT 1 FROM chat_workspace_policy_revisions
 WHERE group_id=NEW.group_id AND topic_id=NEW.topic_id AND revision=NEW.revision)
 OR EXISTS(SELECT 1 FROM chat_policies WHERE group_id=NEW.group_id AND topic_id=NEW.topic_id AND revision>=NEW.revision)
BEGIN SELECT RAISE(ABORT, '目录工作策略不可补授或替换'); END;
CREATE TRIGGER chat_workspace_policy_revisions_update BEFORE UPDATE ON chat_workspace_policy_revisions
BEGIN SELECT RAISE(ABORT, '目录工作策略不可修改'); END;
CREATE TRIGGER chat_workspace_policy_revisions_delete BEFORE DELETE ON chat_workspace_policy_revisions
BEGIN SELECT RAISE(ABORT, '目录工作策略不可删除'); END;
CREATE UNIQUE INDEX workspace_write_grants_chat_identity
 ON workspace_write_grants(id,group_id,member_id,resource_key,node_id,root,resolved_root);
CREATE TABLE chat_workspace_scopes (
    task_id TEXT PRIMARY KEY NOT NULL,
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    task_revision INTEGER NOT NULL CHECK(typeof(task_revision)='integer' AND task_revision>0),
    mandate_id TEXT NOT NULL,
    candidate_id TEXT NOT NULL UNIQUE REFERENCES chat_candidates(id),
    assessment_id TEXT NOT NULL UNIQUE REFERENCES chat_assessments(id) DEFERRABLE INITIALLY DEFERRED,
    dispatch_request_id TEXT NOT NULL UNIQUE REFERENCES coordination_dispatch_requests(id),
    policy_revision INTEGER NOT NULL,
    resource_key TEXT NOT NULL CHECK(resource_key LIKE 'workspace:%'),
    node_id TEXT NOT NULL CHECK(node_id='local'),
    UNIQUE(task_id,group_id,resource_key,node_id),
    FOREIGN KEY(task_id,group_id) REFERENCES tasks(id,group_id),
    FOREIGN KEY(mandate_id,task_id,group_id,topic_id) REFERENCES mandates(id,task_id,group_id,topic_id),
    FOREIGN KEY(group_id,topic_id,policy_revision) REFERENCES chat_workspace_policy_revisions(group_id,topic_id,revision)
);
CREATE TABLE chat_workspace_scope_grants (
    task_id TEXT NOT NULL,
    group_id TEXT NOT NULL,
    resource_key TEXT NOT NULL,
    node_id TEXT NOT NULL,
    grant_id TEXT NOT NULL,
    member_id TEXT NOT NULL,
    root TEXT NOT NULL,
    resolved_root TEXT NOT NULL,
    grant_schema_version INTEGER NOT NULL CHECK(grant_schema_version=19),
    PRIMARY KEY(task_id,member_id),
    FOREIGN KEY(task_id,group_id,resource_key,node_id) REFERENCES chat_workspace_scopes(task_id,group_id,resource_key,node_id),
    FOREIGN KEY(grant_id,group_id,member_id,resource_key,node_id,root,resolved_root)
      REFERENCES workspace_write_grants(id,group_id,member_id,resource_key,node_id,root,resolved_root)
);
CREATE TRIGGER chat_workspace_scopes_insert BEFORE INSERT ON chat_workspace_scopes
WHEN EXISTS(SELECT 1 FROM chat_workspace_scopes WHERE task_id=NEW.task_id OR candidate_id=NEW.candidate_id
 OR assessment_id=NEW.assessment_id OR dispatch_request_id=NEW.dispatch_request_id)
 OR EXISTS(SELECT 1 FROM chat_assessments WHERE id=NEW.assessment_id OR candidate_id=NEW.candidate_id)
BEGIN SELECT RAISE(ABORT, '任务目录范围不可补授或替换'); END;
CREATE TRIGGER chat_workspace_scopes_authority BEFORE INSERT ON chat_workspace_scopes
WHEN NOT EXISTS(
 SELECT 1 FROM chat_candidates c
 JOIN chat_intake_executions e ON e.candidate_id=c.id AND e.purpose='chat_intake/v1'
 JOIN message_command_sources src ON src.id=e.source_id AND src.message_id=c.message_id
   AND src.kind='local_user_chat' AND e.source_kind=src.kind
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
   AND c.policy_revision=NEW.policy_revision AND pr.enabled=1)
BEGIN SELECT RAISE(ABORT, '目录范围必须绑定真实用户来源和当前准入分派'); END;
CREATE TRIGGER chat_workspace_scopes_update BEFORE UPDATE ON chat_workspace_scopes
BEGIN SELECT RAISE(ABORT, '任务目录范围不可修改'); END;
CREATE TRIGGER chat_workspace_scopes_delete BEFORE DELETE ON chat_workspace_scopes
BEGIN SELECT RAISE(ABORT, '任务目录范围不可删除'); END;
CREATE TRIGGER chat_workspace_scope_grants_insert BEFORE INSERT ON chat_workspace_scope_grants
WHEN EXISTS(SELECT 1 FROM chat_workspace_scope_grants WHERE task_id=NEW.task_id AND member_id=NEW.member_id)
BEGIN SELECT RAISE(ABORT, '任务目录成员不可替换'); END;
CREATE TRIGGER chat_workspace_scope_grants_authority BEFORE INSERT ON chat_workspace_scope_grants
WHEN EXISTS(SELECT 1 FROM chat_workspace_scopes s JOIN chat_assessments a ON a.id=s.assessment_id WHERE s.task_id=NEW.task_id)
 OR NOT EXISTS(
 SELECT 1 FROM chat_workspace_scopes s JOIN chat_candidates c ON c.id=s.candidate_id
 JOIN group_dispatch_policy_revisions p ON p.group_id=c.group_id AND p.revision=c.dispatch_policy_revision
 JOIN json_each(p.allowed_worker_ids) w ON w.type='text' AND w.value=NEW.member_id
 JOIN group_coordinator_binding_revisions b ON b.group_id=c.group_id AND b.revision=c.binding_revision
 JOIN members m ON m.group_id=c.group_id AND m.id=NEW.member_id AND m.status='active' AND m.id<>b.member_id
 WHERE s.task_id=NEW.task_id AND s.group_id=NEW.group_id)
BEGIN SELECT RAISE(ABORT, '目录范围成员必须来自准入配置且不得事后追加'); END;
CREATE TRIGGER chat_workspace_scope_grants_update BEFORE UPDATE ON chat_workspace_scope_grants
BEGIN SELECT RAISE(ABORT, '任务目录成员不可修改'); END;
CREATE TRIGGER chat_workspace_scope_grants_delete BEFORE DELETE ON chat_workspace_scope_grants
BEGIN SELECT RAISE(ABORT, '任务目录成员不可删除'); END;
CREATE TRIGGER chat_workspace_assessments_insert BEFORE INSERT ON chat_assessments
WHEN (NEW.decision_id IS NOT NULL AND json_extract(NEW.receipt_json,'$.action')='workspace_change')
 OR EXISTS(SELECT 1 FROM chat_workspace_scopes WHERE assessment_id=NEW.id OR candidate_id=NEW.candidate_id)
BEGIN
 SELECT RAISE(ABORT, '目录工作回执必须匹配范围和真实来源') WHERE NOT EXISTS(
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
  AND json_extract(NEW.receipt_json,'$.authorization_message_id') IS c.message_id
  AND json_extract(NEW.receipt_json,'$.message_id') IS c.message_id
  AND json_extract(NEW.receipt_json,'$.source_sender_id') IS 'user'
  AND json_extract(NEW.receipt_json,'$.assessed_by') IS c.member_id
  AND json_extract(NEW.receipt_json,'$.turn_id') IS c.turn_id
  AND json_extract(NEW.receipt_json,'$.candidate_generation') IS c.generation
  AND json_extract(NEW.receipt_json,'$.policy_revision') IS s.policy_revision
  AND json_extract(NEW.receipt_json,'$.source_id') IS e.source_id
  AND json_extract(NEW.receipt_json,'$.source_kind') IS 'local_user_chat'
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
 SELECT RAISE(ABORT, '目录工作回执成员集合不匹配') WHERE
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
 SELECT RAISE(ABORT, '目录范围必须保留完整的服务端可用成员集合') WHERE EXISTS(
  SELECT 1 FROM chat_candidates c
  JOIN group_dispatch_policy_revisions p ON p.group_id=c.group_id AND p.revision=c.dispatch_policy_revision
  JOIN json_each(p.allowed_worker_ids) w ON w.type='text'
  JOIN group_coordinator_binding_revisions b ON b.group_id=c.group_id AND b.revision=c.binding_revision
  JOIN members m ON m.group_id=c.group_id AND m.id=w.value AND m.status='active' AND m.id<>b.member_id
  JOIN workspace_write_grants g ON g.group_id=c.group_id AND g.member_id=m.id
  WHERE c.id=NEW.candidate_id AND NOT EXISTS(
   SELECT 1 FROM chat_workspace_scope_grants sg JOIN chat_workspace_scopes s ON s.task_id=sg.task_id
   WHERE s.assessment_id=NEW.id AND sg.grant_id=g.id));
END;
`
