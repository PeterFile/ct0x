package store

// Append only: no policy, member or routing history is upgraded by migration.
// The initial route tags both generated revision-1 projections. A later route
// may use that pair but must carry its own explicit request and policy evidence.
const chatAutomaticTextSchema = `
CREATE TABLE chat_automatic_text_policy_revisions (
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    PRIMARY KEY(group_id,topic_id,revision),
    FOREIGN KEY(group_id,topic_id,revision) REFERENCES chat_policy_scopes(group_id,topic_id,revision)
) WITHOUT ROWID;
CREATE TRIGGER chat_automatic_text_policy_insert BEFORE INSERT ON chat_automatic_text_policy_revisions
WHEN EXISTS(SELECT 1 FROM chat_policies WHERE (group_id,topic_id)=(NEW.group_id,NEW.topic_id) AND revision>=NEW.revision)
  OR EXISTS(SELECT 1 FROM chat_automatic_text_policy_revisions WHERE (group_id,topic_id,revision)=(NEW.group_id,NEW.topic_id,NEW.revision))
BEGIN SELECT RAISE(ABORT, '自动文本分派只能随新策略版本显式设置'); END;
CREATE TRIGGER chat_automatic_text_policy_update BEFORE UPDATE ON chat_automatic_text_policy_revisions
BEGIN SELECT RAISE(ABORT, '自动文本分派授权不可修改'); END;
CREATE TRIGGER chat_automatic_text_policy_delete BEFORE DELETE ON chat_automatic_text_policy_revisions
BEGIN SELECT RAISE(ABORT, '自动文本分派授权不可删除'); END;

CREATE TABLE chat_automatic_text_routes (
    decision_id TEXT PRIMARY KEY NOT NULL REFERENCES chat_assessments(decision_id) DEFERRABLE INITIALLY DEFERRED,
    candidate_id TEXT NOT NULL UNIQUE REFERENCES chat_candidates(id),
    source_id TEXT NOT NULL UNIQUE REFERENCES message_command_sources(id),
    group_id TEXT NOT NULL,
    topic_id TEXT NOT NULL,
    chat_policy_revision INTEGER NOT NULL,
    initializes_routing INTEGER NOT NULL CHECK(initializes_routing IN (0,1)),
    binding_revision INTEGER NOT NULL CHECK(binding_revision=1),
    dispatch_policy_revision INTEGER NOT NULL CHECK(dispatch_policy_revision=1),
    coordinator_id TEXT NOT NULL,
    coordinator_provider_id TEXT NOT NULL CHECK(coordinator_provider_id<>''),
    coordinator_model TEXT NOT NULL CHECK(coordinator_model<>''),
    worker_id TEXT NOT NULL CHECK(worker_id<>coordinator_id),
    worker_provider_id TEXT NOT NULL CHECK(worker_provider_id<>''),
    worker_model TEXT NOT NULL CHECK(worker_model<>''),
    worker_creation_key TEXT NOT NULL CHECK(worker_creation_key<>''),
    FOREIGN KEY(group_id,topic_id,chat_policy_revision) REFERENCES chat_automatic_text_policy_revisions(group_id,topic_id,revision),
    FOREIGN KEY(group_id,binding_revision) REFERENCES group_coordinator_binding_revisions(group_id,revision),
    FOREIGN KEY(group_id,dispatch_policy_revision) REFERENCES group_dispatch_policy_revisions(group_id,revision),
    FOREIGN KEY(group_id,coordinator_id) REFERENCES members(group_id,id),
    FOREIGN KEY(group_id,worker_id) REFERENCES members(group_id,id)
);
CREATE UNIQUE INDEX chat_automatic_text_initial_route ON chat_automatic_text_routes(group_id) WHERE initializes_routing=1;
CREATE TRIGGER chat_automatic_text_route_insert BEFORE INSERT ON chat_automatic_text_routes
BEGIN
    SELECT RAISE(ABORT, '自动文本分派依据不可替换') WHERE EXISTS(SELECT 1 FROM chat_automatic_text_routes r
        WHERE r.decision_id=NEW.decision_id OR r.candidate_id=NEW.candidate_id OR r.source_id=NEW.source_id
        OR (NEW.initializes_routing=1 AND r.initializes_routing=1 AND r.group_id=NEW.group_id));
    SELECT RAISE(ABORT, '自动文本分派不得追认已有分派') WHERE NEW.initializes_routing=1 AND EXISTS(
        SELECT 1 FROM coordination_dispatch_requests WHERE group_id=NEW.group_id AND (binding_revision=1 OR policy_revision=1));
    SELECT RAISE(ABORT, '自动文本分派依据不匹配') WHERE NOT EXISTS(
        SELECT 1 FROM chat_candidates c
        JOIN chat_intake_executions i ON i.candidate_id=c.id AND i.turn_id=c.turn_id
        JOIN message_command_sources src ON src.id=i.source_id AND src.kind='local_user_chat' AND src.message_id=c.message_id
        JOIN chat_policies p ON (p.group_id,p.topic_id,p.revision)=(c.group_id,c.topic_id,c.policy_revision)
        JOIN chat_policy_revisions v ON (v.group_id,v.topic_id,v.revision)=(p.group_id,p.topic_id,p.revision)
        JOIN chat_policy_scopes sc ON (sc.group_id,sc.topic_id,sc.revision)=(p.group_id,p.topic_id,p.revision)
        JOIN message_command_sources rule ON rule.id=sc.command_source_id AND rule.kind='local_user_policy'
        JOIN routing_decisions d ON d.id=NEW.decision_id AND d.kind='create_new'
        JOIN coordination_triggers t ON t.id=d.trigger_id
        JOIN mandates m ON m.id=d.mandate_id AND m.assignee_id=NEW.worker_id AND m.dependency_ids='[]'
        JOIN group_coordinator_bindings b ON (b.group_id,b.revision)=(c.group_id,NEW.binding_revision)
        JOIN group_coordinator_binding_revisions cb ON (cb.group_id,cb.revision)=(b.group_id,b.revision) AND cb.member_id=c.member_id
        JOIN group_dispatch_policies dp ON (dp.group_id,dp.revision)=(b.group_id,NEW.dispatch_policy_revision)
        JOIN group_dispatch_policy_revisions dr ON (dr.group_id,dr.revision)=(dp.group_id,dp.revision) AND dr.allowed_worker_ids=json_array(NEW.worker_id)
        JOIN members co ON (co.group_id,co.id,co.provider_id,co.model)=(b.group_id,cb.member_id,NEW.coordinator_provider_id,NEW.coordinator_model)
        JOIN members w ON (w.group_id,w.id,w.provider_id,w.model)=(b.group_id,NEW.worker_id,NEW.worker_provider_id,NEW.worker_model)
        WHERE (c.id,c.group_id,c.topic_id,c.policy_revision,c.member_id,src.id)=(NEW.candidate_id,NEW.group_id,NEW.topic_id,NEW.chat_policy_revision,NEW.coordinator_id,NEW.source_id)
        AND (t.message_id,t.group_id,t.topic_id,t.message_sequence)=(c.message_id,c.group_id,c.topic_id,c.generation)
        AND i.source_kind='local_user_chat' AND v.enabled=1 AND v.action='text_submission'
        AND co.status='active' AND co.workspace='' AND w.status='active' AND w.workspace=''
        AND ((w.creator_id='user' AND EXISTS(SELECT 1 FROM mutation_idempotency u
                WHERE (u.operation,u.actor_id,u.group_id,u.idempotency_key,u.record_id)=('create_member','user',w.group_id,NEW.worker_creation_key,w.id)))
          OR (w.creator_id='system' AND w.owner_id='user'
              AND length(NEW.worker_creation_key)>16 AND substr(NEW.worker_creation_key,1,16)='background_join:'
              AND EXISTS(SELECT 1 FROM messages orig
                JOIN message_command_sources orig_src ON orig_src.message_id=orig.id AND orig_src.kind='local_user_chat'
                JOIN messages notice ON notice.group_id=w.group_id AND notice.topic_id=orig.topic_id AND notice.sender_id='platformnotice'
                WHERE orig.group_id=w.group_id AND orig.id=substr(NEW.worker_creation_key,17) AND orig.sender_id='user')))
        AND ((NEW.initializes_routing=1 AND c.binding_revision=0 AND c.dispatch_policy_revision=0)
         OR (NEW.initializes_routing=0 AND c.binding_revision=1 AND c.dispatch_policy_revision=1
            AND EXISTS(SELECT 1 FROM chat_automatic_text_routes initial WHERE initial.group_id=c.group_id AND initial.initializes_routing=1
                AND (initial.topic_id,initial.coordinator_id,initial.worker_id)=(c.topic_id,NEW.coordinator_id,NEW.worker_id)))));
END;
CREATE TRIGGER chat_automatic_text_route_update BEFORE UPDATE ON chat_automatic_text_routes
BEGIN SELECT RAISE(ABORT, '自动文本分派依据不可修改'); END;
CREATE TRIGGER chat_automatic_text_route_delete BEFORE DELETE ON chat_automatic_text_routes
BEGIN SELECT RAISE(ABORT, '自动文本分派依据不可删除'); END;

-- A generated revision is not a general group grant, even when paired with a
-- newer manually configured revision. Both v12 and this guard must pass.
CREATE TRIGGER chat_automatic_text_dispatch_scope BEFORE INSERT ON coordination_dispatch_requests
WHEN (NEW.binding_revision=1 OR NEW.policy_revision=1) AND EXISTS(
    SELECT 1 FROM chat_automatic_text_routes WHERE group_id=NEW.group_id AND initializes_routing=1)
BEGIN
    SELECT RAISE(ABORT, '自动配置仅供原话题获准的明确纯文本请求分派') WHERE NOT EXISTS(
        SELECT 1 FROM mandates m JOIN chat_automatic_text_routes r ON r.decision_id=m.decision_id
        WHERE m.id=NEW.mandate_id AND (r.group_id,r.topic_id,r.coordinator_id,r.binding_revision,r.dispatch_policy_revision)
            =(NEW.group_id,NEW.topic_id,NEW.coordinator_member_id,NEW.binding_revision,NEW.policy_revision));
END;
CREATE TRIGGER chat_automatic_text_assessment_scope BEFORE INSERT ON chat_assessments
WHEN EXISTS(SELECT 1 FROM chat_automatic_text_routes WHERE decision_id=NEW.decision_id OR candidate_id=NEW.candidate_id)
BEGIN
    SELECT RAISE(ABORT, '自动分派必须有明确纯文本请求的准入回执') WHERE NOT EXISTS(
        SELECT 1 FROM chat_automatic_text_routes a JOIN mandates m ON m.decision_id=a.decision_id
        JOIN coordination_dispatch_requests r ON r.mandate_id=m.id AND r.task_id=m.task_id
        WHERE a.decision_id=NEW.decision_id AND a.candidate_id=NEW.candidate_id AND r.id=NEW.dispatch_request_id
        AND json_extract(NEW.receipt_json,'$.outcome')='admitted'
        AND json_extract(NEW.receipt_json,'$.disposition')='request_work'
        AND json_extract(NEW.receipt_json,'$.action')='text_submission'
        AND json_extract(NEW.receipt_json,'$.authority')='explicit_request'
        AND json_extract(NEW.receipt_json,'$.source_id')=a.source_id
        AND json_extract(NEW.receipt_json,'$.automatic_text_routing_checked')=1);
END;

CREATE TABLE chat_automatic_text_group_imports (
    group_id TEXT PRIMARY KEY NOT NULL REFERENCES groups(id),
    source_group_id TEXT NOT NULL CHECK(source_group_id<>'' AND source_group_id<>group_id),
    source_topic_id TEXT NOT NULL,
    source_policy_revision INTEGER NOT NULL CHECK(typeof(source_policy_revision)='integer' AND source_policy_revision>0),
    max_admissions INTEGER NOT NULL CHECK(typeof(max_admissions)='integer' AND max_admissions BETWEEN 1 AND 100),
    source_command_source_id TEXT NOT NULL REFERENCES message_command_sources(id),
    created_at TEXT NOT NULL CHECK(created_at<>''),
    FOREIGN KEY(source_group_id,source_topic_id,source_policy_revision)
        REFERENCES chat_automatic_text_policy_revisions(group_id,topic_id,revision)
) WITHOUT ROWID;
CREATE TRIGGER chat_automatic_text_group_imports_insert BEFORE INSERT ON chat_automatic_text_group_imports
WHEN EXISTS(SELECT 1 FROM chat_automatic_text_group_imports WHERE group_id=NEW.group_id)
  OR NOT EXISTS(
      SELECT 1 FROM chat_policies p
      JOIN chat_policy_revisions v ON (v.group_id,v.topic_id,v.revision)=(p.group_id,p.topic_id,p.revision)
      JOIN chat_policy_scopes sc ON (sc.group_id,sc.topic_id,sc.revision)=(p.group_id,p.topic_id,p.revision)
      JOIN message_command_sources s ON s.id=sc.command_source_id
      JOIN messages m ON m.id=s.message_id
      JOIN chat_automatic_text_policy_revisions a ON (a.group_id,a.topic_id,a.revision)=(p.group_id,p.topic_id,p.revision)
      WHERE (p.group_id,p.topic_id,p.revision)=(NEW.source_group_id,NEW.source_topic_id,NEW.source_policy_revision)
        AND v.enabled=1 AND v.action='text_submission' AND v.max_admissions=NEW.max_admissions AND v.authorized_by='user'
        AND sc.command_source_id=NEW.source_command_source_id AND s.kind='local_user_policy'
        AND m.group_id=p.group_id AND m.topic_id=p.topic_id AND m.sender_id='user')
BEGIN SELECT RAISE(ABORT, '新群导入授权必须绑定有效的本机用户自动文本分派策略'); END;
CREATE TRIGGER chat_automatic_text_group_imports_update BEFORE UPDATE ON chat_automatic_text_group_imports
BEGIN SELECT RAISE(ABORT, '新群导入授权记录不可修改'); END;
CREATE TRIGGER chat_automatic_text_group_imports_delete BEFORE DELETE ON chat_automatic_text_group_imports
BEGIN SELECT RAISE(ABORT, '新群导入授权记录不可删除'); END;
`
