package store

// v22 is intentionally not registered until the frozen v21 import window.
const chatRepairSchemaV22 = `
CREATE TABLE chat_repair_review_intakes (
 review_id TEXT PRIMARY KEY NOT NULL REFERENCES task_review_decisions(id),
 message_id TEXT NOT NULL UNIQUE REFERENCES chat_review_notices(message_id),
 delivery_id TEXT NOT NULL UNIQUE REFERENCES deliveries(id),
 group_id TEXT NOT NULL REFERENCES groups(id), topic_id TEXT NOT NULL,
 member_id TEXT NOT NULL REFERENCES members(id),
 binding_revision INTEGER NOT NULL CHECK(binding_revision>0),
 policy_revision INTEGER NOT NULL CHECK(policy_revision>0)
);
CREATE TABLE chat_repair_candidates (
 candidate_id TEXT PRIMARY KEY NOT NULL REFERENCES chat_candidates(id),
 review_id TEXT NOT NULL REFERENCES task_review_decisions(id),
 source_json TEXT NOT NULL CHECK(json_valid(source_json))
);
CREATE INDEX chat_repair_candidates_review ON chat_repair_candidates(review_id,candidate_id);
CREATE TABLE chat_repair_links (
 id TEXT PRIMARY KEY NOT NULL CHECK(id<>''),
 repair_task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id),
 parent_task_id TEXT NOT NULL REFERENCES tasks(id),
 parent_submission_id TEXT NOT NULL REFERENCES task_submissions(id),
 parent_review_id TEXT NOT NULL UNIQUE REFERENCES task_review_decisions(id),
 reviewed_task_revision INTEGER NOT NULL CHECK(reviewed_task_revision>0),
 parent_mandate_id TEXT NOT NULL REFERENCES mandates(id),
 group_id TEXT NOT NULL REFERENCES groups(id), topic_id TEXT NOT NULL,
 candidate_id TEXT NOT NULL UNIQUE REFERENCES chat_candidates(id),
 assessment_id TEXT NOT NULL UNIQUE REFERENCES chat_assessments(id) DEFERRABLE INITIALLY DEFERRED,
 mandate_id TEXT NOT NULL UNIQUE REFERENCES mandates(id),
 dispatch_request_id TEXT NOT NULL UNIQUE REFERENCES coordination_dispatch_requests(id),
 policy_revision INTEGER NOT NULL CHECK(policy_revision>0),
 authorization TEXT NOT NULL CHECK(authorization IN ('review_request','original_agreement','chat_request')),
 authorization_source_kind TEXT NOT NULL CHECK(authorization_source_kind IN ('local_user_review','local_user_chat','local_user_policy')),
 authorization_source_id TEXT NOT NULL CHECK(authorization_source_id<>''),
 authorization_message_id TEXT REFERENCES messages(id),
 authorization_policy_revision INTEGER NOT NULL CHECK(authorization_policy_revision>0),
 created_at TEXT NOT NULL,
 CHECK(repair_task_id<>parent_task_id)
);
CREATE INDEX chat_repair_links_group_task ON chat_repair_links(group_id,repair_task_id);
CREATE TABLE chat_repair_artifact_versions (
 repair_task_id TEXT NOT NULL REFERENCES chat_repair_links(repair_task_id),
 artifact_id TEXT NOT NULL REFERENCES artifacts(id),
 version_id TEXT NOT NULL REFERENCES artifact_versions(id),
 PRIMARY KEY(repair_task_id,artifact_id,version_id)
);
CREATE TRIGGER chat_repair_review_intakes_insert BEFORE INSERT ON chat_repair_review_intakes
BEGIN
 SELECT RAISE(ABORT,'修复评估来源不可替换') WHERE EXISTS(SELECT 1 FROM chat_repair_review_intakes
 WHERE review_id=NEW.review_id OR message_id=NEW.message_id OR delivery_id=NEW.delivery_id);
 SELECT RAISE(ABORT,'修复评估必须绑定实际用户拒收和本群话题投递') WHERE NOT EXISTS(
 SELECT 1 FROM task_review_decisions r JOIN chat_review_notices rn ON rn.review_id=r.id
 JOIN mandates m INDEXED BY mandates_group_task ON m.group_id=r.group_id AND m.task_id=r.task_id
 JOIN chat_assessments a ON a.decision_id=m.decision_id
 JOIN chat_candidates c ON c.id=a.candidate_id AND c.group_id=m.group_id AND c.topic_id=m.topic_id
 JOIN deliveries d ON d.id=NEW.delivery_id AND d.message_id=rn.message_id AND d.group_id=r.group_id AND d.topic_id=m.topic_id
 JOIN members member ON member.id=d.recipient_id AND member.group_id=d.group_id AND member.status='active'
 JOIN group_coordinator_bindings cb ON cb.group_id=r.group_id AND cb.revision=NEW.binding_revision
 JOIN group_coordinator_binding_revisions b ON b.group_id=cb.group_id AND b.revision=cb.revision AND b.member_id=member.id
 JOIN chat_policies cp ON cp.group_id=r.group_id AND cp.topic_id=m.topic_id AND cp.revision=NEW.policy_revision
 JOIN chat_policy_revisions p ON p.group_id=cp.group_id AND p.topic_id=cp.topic_id AND p.revision=cp.revision AND p.enabled=1
 WHERE r.id=NEW.review_id AND r.reviewed_by='user' AND r.outcome='rejected'
 AND rn.message_id=NEW.message_id AND r.group_id=NEW.group_id AND m.topic_id=NEW.topic_id
 AND d.recipient_id=NEW.member_id AND d.state='queued' AND d.turn_id=''
 AND NOT EXISTS(SELECT 1 FROM message_command_sources s WHERE s.message_id=rn.message_id));
END;
CREATE TRIGGER chat_repair_candidates_insert BEFORE INSERT ON chat_repair_candidates
BEGIN
 SELECT RAISE(ABORT,'修复候选不可替换或补写') WHERE EXISTS(SELECT 1 FROM chat_repair_candidates WHERE candidate_id=NEW.candidate_id)
 OR EXISTS(SELECT 1 FROM chat_assessments WHERE candidate_id=NEW.candidate_id);
 SELECT RAISE(ABORT,'修复候选必须绑定本轮正式拒收来源') WHERE NOT EXISTS(
 SELECT 1 FROM chat_candidates c JOIN turns t ON t.id=c.turn_id
 JOIN chat_repair_review_intakes i ON i.delivery_id=t.delivery_id AND i.message_id=c.message_id
 JOIN chat_intake_executions e ON e.candidate_id=c.id AND e.turn_id=t.id
 WHERE c.id=NEW.candidate_id AND i.review_id=NEW.review_id
 AND c.group_id=i.group_id AND c.topic_id=i.topic_id AND c.member_id=i.member_id
 AND e.purpose='chat_intake/v1' AND e.source_kind='platform' AND e.source_id='review:'||i.review_id
 AND json_extract(NEW.source_json,'$.parent_review_id') IS i.review_id
 AND json_extract(NEW.source_json,'$.group_id') IS c.group_id AND json_extract(NEW.source_json,'$.topic_id') IS c.topic_id);
END;
CREATE TRIGGER chat_repair_links_insert BEFORE INSERT ON chat_repair_links
BEGIN
 SELECT RAISE(ABORT,'修复关联不可替换或补写') WHERE EXISTS(SELECT 1 FROM chat_repair_links WHERE id=NEW.id
 OR repair_task_id=NEW.repair_task_id OR parent_review_id=NEW.parent_review_id OR candidate_id=NEW.candidate_id
 OR assessment_id=NEW.assessment_id OR mandate_id=NEW.mandate_id OR dispatch_request_id=NEW.dispatch_request_id)
 OR EXISTS(SELECT 1 FROM chat_assessments WHERE id=NEW.assessment_id OR candidate_id=NEW.candidate_id);
 SELECT RAISE(ABORT,'修复关联必须绑定原拒收版本及新任务准入') WHERE NOT EXISTS(
 SELECT 1 FROM task_review_decisions r JOIN task_submissions sub ON sub.id=r.submission_id AND sub.task_id=r.task_id
 AND sub.group_id=r.group_id AND sub.task_revision=r.task_revision
 JOIN tasks parent ON parent.id=r.task_id AND parent.group_id=r.group_id AND parent.status='open'
 AND parent.revision-1=r.task_revision AND parent.active_attempt_id IS NULL AND parent.current_submission_id IS NULL
 JOIN mandates pm ON pm.id=NEW.parent_mandate_id AND pm.task_id=r.task_id AND pm.group_id=r.group_id AND pm.topic_id=NEW.topic_id
 JOIN chat_assessments pa ON pa.decision_id=pm.decision_id
 JOIN chat_candidates pc ON pc.id=pa.candidate_id AND pc.group_id=pm.group_id AND pc.topic_id=pm.topic_id
 JOIN mandates m ON m.id=NEW.mandate_id AND m.task_id=NEW.repair_task_id AND m.group_id=r.group_id AND m.topic_id=pm.topic_id
 JOIN tasks repair ON repair.id=m.task_id AND repair.group_id=m.group_id AND repair.status='open' AND repair.revision=m.task_revision_at_admission
 JOIN coordination_dispatch_requests d ON d.id=NEW.dispatch_request_id AND d.task_id=m.task_id AND d.mandate_id=m.id
 JOIN chat_candidates c ON c.id=NEW.candidate_id AND c.group_id=m.group_id AND c.topic_id=m.topic_id AND c.policy_revision=NEW.policy_revision
 JOIN coordination_triggers tr ON tr.id=m.origin_trigger_id AND tr.message_id=c.message_id
 JOIN chat_policies cp ON cp.group_id=c.group_id AND cp.topic_id=c.topic_id AND cp.revision=c.policy_revision
 JOIN chat_policy_revisions p ON p.group_id=cp.group_id AND p.topic_id=cp.topic_id AND p.revision=cp.revision AND p.enabled=1
 WHERE r.id=NEW.parent_review_id AND r.reviewed_by='user' AND r.outcome='rejected'
 AND r.task_id=NEW.parent_task_id AND r.submission_id=NEW.parent_submission_id AND r.task_revision=NEW.reviewed_task_revision
 AND r.group_id=NEW.group_id AND repair.title=pm.objective AND repair.description=pm.description AND repair.acceptance=pm.acceptance
 AND NOT EXISTS(SELECT 1 FROM task_attempts a JOIN turns t ON t.id=a.turn_id WHERE a.task_id=parent.id AND a.group_id=parent.group_id
 AND t.state IN ('starting','running','cancel_requested','unknown')));
 SELECT RAISE(ABORT,'修复许可必须来自实际用户命令或原始约定') WHERE NOT EXISTS(
 SELECT 1 FROM chat_candidates c JOIN chat_intake_executions e ON e.candidate_id=c.id AND e.turn_id=c.turn_id AND e.purpose='chat_intake/v1'
 WHERE c.id=NEW.candidate_id AND (
 (NEW.authorization='chat_request' AND NEW.authorization_source_kind='local_user_chat'
 AND NEW.authorization_message_id=c.message_id AND NEW.authorization_source_id=e.source_id AND e.source_kind='local_user_chat'
 AND EXISTS(SELECT 1 FROM message_command_sources src JOIN messages msg ON msg.id=src.message_id
 WHERE src.id=e.source_id AND src.kind='local_user_chat' AND src.message_id=c.message_id AND msg.sender_id='user'))
 OR ((NEW.authorization='review_request' OR NEW.authorization='original_agreement') AND EXISTS(
 SELECT 1 FROM chat_repair_candidates rc JOIN turns t ON t.id=c.turn_id JOIN chat_repair_review_intakes i ON i.delivery_id=t.delivery_id
 WHERE rc.candidate_id=c.id AND rc.review_id=NEW.parent_review_id AND i.review_id=rc.review_id
 AND e.source_kind='platform' AND e.source_id='review:'||i.review_id)
 AND ((NEW.authorization='review_request' AND NEW.authorization_source_kind='local_user_review'
 AND NEW.authorization_source_id=NEW.parent_review_id AND NEW.authorization_message_id IS NULL)
 OR (NEW.authorization='original_agreement' AND EXISTS(
 SELECT 1 FROM chat_repair_candidates rc WHERE rc.candidate_id=c.id
 AND json_extract(rc.source_json,'$.original_authorization.source_kind') IS NEW.authorization_source_kind
 AND json_extract(rc.source_json,'$.original_authorization.source_id') IS NEW.authorization_source_id
 AND json_extract(rc.source_json,'$.original_authorization.message_id') IS NEW.authorization_message_id
 AND json_extract(rc.source_json,'$.original_authorization.policy_revision') IS NEW.authorization_policy_revision))))));
END;
CREATE TRIGGER chat_repair_artifact_versions_insert BEFORE INSERT ON chat_repair_artifact_versions
BEGIN
 SELECT RAISE(ABORT,'修复原版本不可替换或补写') WHERE EXISTS(SELECT 1 FROM chat_repair_artifact_versions
 WHERE repair_task_id=NEW.repair_task_id AND artifact_id=NEW.artifact_id AND version_id=NEW.version_id)
 OR EXISTS(SELECT 1 FROM chat_repair_links l JOIN chat_assessments a ON a.id=l.assessment_id WHERE l.repair_task_id=NEW.repair_task_id);
 SELECT RAISE(ABORT,'修复版本必须来自原正式提交') WHERE NOT EXISTS(
 SELECT 1 FROM chat_repair_links l JOIN task_submission_artifacts a ON a.submission_id=l.parent_submission_id
 AND a.task_id=l.parent_task_id AND a.group_id=l.group_id AND a.task_revision=l.reviewed_task_revision
 WHERE l.repair_task_id=NEW.repair_task_id AND a.artifact_id=NEW.artifact_id AND a.version_id=NEW.version_id);
END;
CREATE TRIGGER chat_repair_assessments_insert BEFORE INSERT ON chat_assessments
WHEN EXISTS(SELECT 1 FROM chat_repair_links WHERE assessment_id=NEW.id OR candidate_id=NEW.candidate_id)
 OR json_type(NEW.receipt_json,'$.repair_link') IS NOT NULL
BEGIN
 SELECT RAISE(ABORT,'修复回执必须匹配正式关联') WHERE NOT EXISTS(
 SELECT 1 FROM chat_repair_links l JOIN chat_candidates c ON c.id=l.candidate_id
 WHERE l.assessment_id=NEW.id AND l.candidate_id=NEW.candidate_id AND l.dispatch_request_id=NEW.dispatch_request_id
 AND json_extract(NEW.receipt_json,'$.task_id') IS l.repair_task_id
 AND json_extract(NEW.receipt_json,'$.mandate_id') IS l.mandate_id
 AND json_extract(NEW.receipt_json,'$.authority') IS CASE WHEN l.authorization='original_agreement' AND l.authorization_source_kind='local_user_policy'
 THEN 'written_preauthorization' ELSE 'explicit_request' END
 AND json_extract(NEW.receipt_json,'$.outcome') IS 'admitted'
 AND json_extract(NEW.receipt_json,'$.repair_link.id') IS l.id
 AND json_extract(NEW.receipt_json,'$.repair_link.repair_task_id') IS l.repair_task_id
 AND json_extract(NEW.receipt_json,'$.repair_link.parent_task_id') IS l.parent_task_id
 AND json_extract(NEW.receipt_json,'$.repair_link.parent_submission_id') IS l.parent_submission_id
 AND json_extract(NEW.receipt_json,'$.repair_link.parent_review_id') IS l.parent_review_id
 AND json_extract(NEW.receipt_json,'$.repair_link.reviewed_task_revision') IS l.reviewed_task_revision
 AND json_extract(NEW.receipt_json,'$.repair_link.authorization_source_id') IS l.authorization_source_id);
 SELECT RAISE(ABORT,'修复必须保留完整原成果版本集合') WHERE EXISTS(
 SELECT 1 FROM chat_repair_links l JOIN task_submission_artifacts a ON a.submission_id=l.parent_submission_id
 WHERE l.assessment_id=NEW.id AND NOT EXISTS(SELECT 1 FROM chat_repair_artifact_versions v
 WHERE v.repair_task_id=l.repair_task_id AND v.artifact_id=a.artifact_id AND v.version_id=a.version_id));
END;
CREATE TRIGGER chat_repair_review_intakes_update BEFORE UPDATE ON chat_repair_review_intakes BEGIN SELECT RAISE(ABORT,'修复评估来源不可修改'); END;
CREATE TRIGGER chat_repair_review_intakes_delete BEFORE DELETE ON chat_repair_review_intakes BEGIN SELECT RAISE(ABORT,'修复评估来源不可删除'); END;
CREATE TRIGGER chat_repair_candidates_update BEFORE UPDATE ON chat_repair_candidates BEGIN SELECT RAISE(ABORT,'修复候选不可修改'); END;
CREATE TRIGGER chat_repair_candidates_delete BEFORE DELETE ON chat_repair_candidates BEGIN SELECT RAISE(ABORT,'修复候选不可删除'); END;
CREATE TRIGGER chat_repair_links_update BEFORE UPDATE ON chat_repair_links BEGIN SELECT RAISE(ABORT,'修复关联不可修改'); END;
CREATE TRIGGER chat_repair_links_delete BEFORE DELETE ON chat_repair_links BEGIN SELECT RAISE(ABORT,'修复关联不可删除'); END;
CREATE TRIGGER chat_repair_artifact_versions_update BEFORE UPDATE ON chat_repair_artifact_versions BEGIN SELECT RAISE(ABORT,'修复原成果版本不可修改'); END;
CREATE TRIGGER chat_repair_artifact_versions_delete BEFORE DELETE ON chat_repair_artifact_versions BEGIN SELECT RAISE(ABORT,'修复原成果版本不可删除'); END;
` + chatRepairWorkspaceSchemaV22
