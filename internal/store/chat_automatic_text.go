package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

// ChatAutomaticTextFacts may only come from trusted runtime sampling. They are
// implementation support for the exact actors, not authorization or grants.
type ChatAutomaticTextFacts struct {
	Coordinator DispatchWorkerCapabilityFact   `json:"-"`
	Workers     []DispatchWorkerCapabilityFact `json:"-"`
}

type chatAutomaticTextActors struct {
	coordinator, worker DispatchWorkerCapabilityFact
	workerCreationKey   string
}

type chatAutomaticTextPlan struct {
	initialize bool
	topic      string
	actors     chatAutomaticTextActors
}

func chatAutomaticTextSchemaInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	version := slices.Index(migrations, chatAutomaticTextSchema) + 1
	if version <= 0 {
		return false, nil
	}
	var supported bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.schema_migrations WHERE version=?)`, version).Scan(&supported)
	return supported, err
}

func insertGroupAuthorizationImportInTx(ctx context.Context, tx *sql.Tx, groupID, sourceGroupID, sourceTopicID string) error {
	supported, err := chatAutomaticTextSchemaInTx(ctx, tx)
	if err != nil {
		return err
	}
	if !supported {
		return fmt.Errorf("%w：当前数据库版本不支持新群导入自动文本分派授权", ErrInvalid)
	}
	if !validTriggerID(sourceGroupID) {
		return fmt.Errorf("%w：来源群组 ID 无效", ErrInvalid)
	}
	var sourceExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM groups WHERE id=?)`, sourceGroupID).Scan(&sourceExists); err != nil {
		return err
	}
	if !sourceExists {
		return ErrNotFound
	}
	srcPolicy, err := chatPolicyInTx(ctx, tx, domain.ThreadRef{GroupID: sourceGroupID, TopicID: sourceTopicID}, 0)
	if err != nil {
		return err
	}
	if srcPolicy.Revision < 1 || !srcPolicy.Enabled || !srcPolicy.AutomaticTextDispatch ||
		srcPolicy.Action != domain.ChatActionTextSubmission || srcPolicy.AuthorizedBy != "user" || srcPolicy.ConfigurationSourceID == "" {
		return fmt.Errorf("%w：来源群组未启用本机用户自动文本分派授权", ErrInvalid)
	}
	return coordinatorWrite(ctx, tx,
		`INSERT INTO chat_automatic_text_group_imports(group_id,source_group_id,source_topic_id,source_policy_revision,max_admissions,source_command_source_id,created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		groupID, sourceGroupID, sourceTopicID, srcPolicy.Revision, srcPolicy.MaxAdmissions,
		srcPolicy.ConfigurationSourceID, time.Now().UTC().Format(timestampLayout))
}

func (s *Store) materializeImportedChatAutomaticTextPolicyInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef) (domain.ChatPolicy, error) {
	supported, err := chatAutomaticTextSchemaInTx(ctx, tx)
	if err != nil || !supported {
		return domain.ChatPolicy{}, err
	}
	var sourceGroupID, sourceTopicID, sourceCommandSourceID string
	var sourceRev int64
	var maxAdmissions int
	err = tx.QueryRowContext(ctx, `SELECT source_group_id,source_topic_id,source_policy_revision,max_admissions,source_command_source_id
	 FROM chat_automatic_text_group_imports WHERE group_id=?`, thread.GroupID).Scan(
		&sourceGroupID, &sourceTopicID, &sourceRev, &maxAdmissions, &sourceCommandSourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ChatPolicy{}, nil
	}
	if err != nil {
		return domain.ChatPolicy{}, err
	}
	current, err := chatPolicyInTx(ctx, tx, thread, 0)
	if err != nil {
		return domain.ChatPolicy{}, err
	}
	if current.Revision > 0 {
		return current, nil
	}
	req := domain.ChatPolicyRequest{
		Enabled:               true,
		Action:                domain.ChatActionTextSubmission,
		MaxAdmissions:         maxAdmissions,
		AutomaticTextDispatch: true,
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return domain.ChatPolicy{}, err
	}
	topicHash := sha256.Sum256([]byte(thread.TopicID))
	importKey := fmt.Sprintf("imported_auth:%s:%x", thread.GroupID, topicHash[:16])
	payload := mutationPayload(thread.GroupID, thread.TopicID, "0", string(encoded))
	rule := fmt.Sprintf(
		"本机用户创建本群时已从群组 %s（话题 %q，策略版本 %d，来源 %s）导入自动文本分派授权，本话题自动启用纯文本交付；本版总准入额度=%d。\n允许在本策略启用时为本话题的本机用户明确纯文本请求选择接收成员为协调员，并从由本机用户创建或本群后台调度合规加入的活跃同群成员中选择一位不同的执行成员。两人均须支持受限文本执行；此选项不授权自主准备、目录、修复或调查工作。",
		sourceGroupID, sourceTopicID, sourceRev, sourceCommandSourceID, maxAdmissions)
	command, err := createMessageInTx(ctx, tx, MessageRequest{
		GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: "user", Content: rule, RecipientIDs: []string{},
	}, nil)
	if err != nil {
		return domain.ChatPolicy{}, err
	}
	sourceID, err := messageCommandSourceInTx(ctx, tx, command.ID, "local_user_policy", payload, importKey, nil)
	if err != nil {
		return domain.ChatPolicy{}, err
	}
	// Trigger order: chat_policy_revisions -> chat_policy_scopes -> chat_automatic_text_policy_revisions -> chat_policies.
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO chat_policy_revisions(group_id,topic_id,revision,enabled,allow_preparation,preauthorization_message_id,action,max_admissions,authorized_by)
		  VALUES(?,?,1,1,0,NULL,'text_submission',?,'user')`, []any{thread.GroupID, thread.TopicID, maxAdmissions}},
		{`INSERT INTO chat_policy_scopes(group_id,topic_id,revision,scope_kind,command_source_id) VALUES(?,?,1,'',?)`,
			[]any{thread.GroupID, thread.TopicID, sourceID}},
		{`INSERT INTO chat_automatic_text_policy_revisions(group_id,topic_id,revision) VALUES(?,?,1)`,
			[]any{thread.GroupID, thread.TopicID}},
		{`INSERT INTO chat_policies(group_id,topic_id,revision) VALUES(?,?,1)`,
			[]any{thread.GroupID, thread.TopicID}},
	} {
		if err := coordinatorWrite(ctx, tx, stmt.sql, stmt.args...); err != nil {
			return domain.ChatPolicy{}, err
		}
	}
	return chatPolicyInTx(ctx, tx, thread, 1)
}

func chatAutomaticTextPolicyInTx(ctx context.Context, tx *sql.Tx, policy *domain.ChatPolicy) error {
	supported, err := chatAutomaticTextSchemaInTx(ctx, tx)
	if err != nil || !supported {
		return err
	}
	return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM chat_automatic_text_policy_revisions
	 WHERE group_id=? AND topic_id=? AND revision=?)`, policy.GroupID, policy.TopicID, policy.Revision).Scan(&policy.AutomaticTextDispatch)
}

func chatAutomaticTextPlanInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate) (*chatAutomaticTextPlan, error) {
	initialize := c.BindingRevision == 0 && c.DispatchPolicyRevision == 0 && c.Policy.AutomaticTextDispatch
	if !initialize && c.BindingRevision != 1 && c.DispatchPolicyRevision != 1 {
		return nil, nil
	}
	supported, err := chatAutomaticTextSchemaInTx(ctx, tx)
	if err != nil || !supported {
		return nil, err
	}
	if initialize {
		return &chatAutomaticTextPlan{initialize: true, topic: c.TopicID}, nil
	}
	plan := &chatAutomaticTextPlan{}
	a := &plan.actors
	err = tx.QueryRowContext(ctx, `SELECT topic_id,coordinator_id,coordinator_provider_id,coordinator_model,
	 worker_id,worker_provider_id,worker_model,worker_creation_key FROM chat_automatic_text_routes
	 WHERE group_id=? AND initializes_routing=1`, c.GroupID).Scan(&plan.topic,
		&a.coordinator.MemberID, &a.coordinator.ProviderID, &a.coordinator.Model,
		&a.worker.MemberID, &a.worker.ProviderID, &a.worker.Model, &a.workerCreationKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // Revision 1 is not necessarily automatic.
	}
	a.coordinator.GroupID, a.worker.GroupID = c.GroupID, c.GroupID
	return plan, err
}

func (p *chatAutomaticTextPlan) checkScope(c domain.ChatCandidate, current domain.ChatPolicy, input domain.ChatAssessment) error {
	if !p.initialize && (c.TopicID != p.topic || c.BindingRevision != 1 || c.DispatchPolicyRevision != 1) {
		return chatDeny("automatic_text_routing_scope", "自动配置仅适用于原话题的明确纯文本请求，不能替代手动协调配置。", "是否在需要工作的范围内显式配置协调员和执行成员？")
	}
	if !current.AutomaticTextDispatch || current.ConfigurationSourceID == "" || current.AuthorizedBy != "user" {
		return chatDeny("automatic_text_dispatch_disabled", "本话题当前版本未授权自动选择纯文本工作成员。", "是否在本话题策略中启用自动文本分派？")
	}
	if input.Disposition != "request_work" || input.Action != domain.ChatActionTextSubmission || input.Repair != nil || input.Followup != nil {
		return chatDeny("automatic_text_routing_scope", "自动配置只允许本机用户明确请求的新纯文本交付。", "是否只需要在本话题交付纯文本？")
	}
	return nil
}

func (p *chatAutomaticTextPlan) selectActorsInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, c domain.ChatCandidate, facts ChatAutomaticTextFacts) error {
	if _, err := chatDiscussionArrivalInTx(ctx, tx, scope, c.ID); err != nil {
		return err
	}
	root, err := chatDiscussionRootSourceInTx(ctx, tx, c)
	if err != nil {
		return err
	}
	if chatAutomaticTextDiscussionOnly(root.content) {
		return chatDeny("discussion_only", "当前用户原文包含仅讨论或不开工的限制。", "是否另发一条消息，明确现在需要开展哪项工作？")
	}
	var policySource bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM message_command_sources s
	 JOIN messages m ON m.id=s.message_id WHERE s.id=? AND s.kind='local_user_policy'
	 AND m.group_id=? AND m.topic_id=? AND m.sender_id='user')`, c.Policy.ConfigurationSourceID, c.GroupID, c.TopicID).Scan(&policySource); err != nil {
		return err
	}
	if !policySource {
		return chatDeny("automatic_text_dispatch_disabled", "没有可验证的本话题本机用户配置来源。", "是否重新保存本话题自动文本分派策略？")
	}
	co := facts.Coordinator
	if co.MemberID != c.MemberID || !plainTextRoleFact(co, c.GroupID) || !p.initialize && co != p.actors.coordinator {
		return chatAutoTextCoordinatorUnavailable()
	}
	var live bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM members WHERE (group_id,id,provider_id,model)=(?,?,?,?)
	 AND status='active' AND workspace='')`, c.GroupID, co.MemberID, co.ProviderID, co.Model).Scan(&live); err != nil {
		return err
	}
	if !live {
		return chatAutoTextCoordinatorUnavailable()
	}
	for _, w := range facts.Workers {
		if w.MemberID == co.MemberID || !plainTextRoleFact(w, c.GroupID) || !p.initialize && w != p.actors.worker {
			continue
		}
		// The normal LocalUser creation command stamps creator and records the
		// same-group mutation. A discussion join, role label or owner is not proof.
		var creationKey string
		err := tx.QueryRowContext(ctx, `SELECT u.idempotency_key FROM members w JOIN mutation_idempotency u
		 ON u.operation='create_member' AND u.actor_id='user' AND u.group_id=w.group_id AND u.record_id=w.id
		 WHERE (w.group_id,w.id,w.provider_id,w.model)=(?,?,?,?) AND w.status='active' AND w.workspace='' AND w.creator_id='user'
		 ORDER BY u.idempotency_key LIMIT 1`, c.GroupID, w.MemberID, w.ProviderID, w.Model).Scan(&creationKey)
		if errors.Is(err, sql.ErrNoRows) {
			var hasBackgroundJoins bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='group_background_joins')`).Scan(&hasBackgroundJoins); err != nil {
				return err
			}
			if hasBackgroundJoins {
				err = tx.QueryRowContext(ctx, `SELECT 'background_join:'||j.origin_message_id FROM members w
				 JOIN group_background_joins j ON (j.group_id,j.member_id,j.provider_id,j.model)=(w.group_id,w.id,w.provider_id,w.model)
				 WHERE (w.group_id,w.id,w.provider_id,w.model)=(?,?,?,?)
				   AND w.status='active' AND w.workspace='' AND w.creator_id='system' AND w.owner_id='user'
				 LIMIT 1`, c.GroupID, w.MemberID, w.ProviderID, w.Model).Scan(&creationKey)
			}
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		p.actors = chatAutomaticTextActors{coordinator: co, worker: w, workerCreationKey: creationKey}
		return nil
	}
	return chatDeny("automatic_text_worker_unavailable", "没有另一个由本机用户创建、处于活跃状态且支持纯文本执行的同群成员。", "是否在本群添加一位支持纯文本执行的成员？")
}

// This is only a veto, after the durable discussion gate. Ignore a complete
// leading past-only clause only when separated from a marked current clause.
// The remaining text still gets the same veto; no phrase supplies authority.
func chatAutomaticTextDiscussionOnly(body string) bool {
	restrictions := []string{"只讨论", "仅讨论", "不开工", "不要开工"}
	const clauseBreak = "；;。！？.!?\r\n"
	if end := strings.IndexAny(body, clauseBreak); end >= 0 {
		previous := strings.TrimSpace(body[:end])
		current := strings.TrimSpace(strings.TrimLeft(body[end:], clauseBreak))
		for _, marker := range []string{"现在", "本次", "这次"} {
			rest, ok := strings.CutPrefix(current, marker)
			if !ok || strings.Trim(rest, clauseBreak+"，,：: \t") == "" {
				continue
			}
			for _, prefix := range []string{"上一条消息", "上一条", "上次"} {
				if rest, ok := strings.CutPrefix(previous, prefix); ok && slices.Contains(restrictions, strings.TrimSpace(rest)) {
					body = current
					break
				}
			}
			break
		}
	}
	for _, restriction := range restrictions {
		if strings.Contains(body, restriction) {
			return true
		}
	}
	// Veto explicit no-start wording without widening the past-clause exemption
	// or treating unrelated constraints (such as not editing files) as no-start.
	for _, restriction := range []string{"不要启动任务", "不启动任务", "别启动任务", "不要开始任务", "不要开始工作", "不开始工作", "别开工"} {
		if strings.Contains(body, restriction) {
			return true
		}
	}
	return false
}

func plainTextRoleFact(f DispatchWorkerCapabilityFact, groupID string) bool {
	return f.GroupID == groupID && f == (DispatchWorkerCapabilityFact{GroupID: groupID, MemberID: f.MemberID, ProviderID: f.ProviderID, Model: f.Model}) &&
		memberToolIDsValid(f.MemberID, f.ProviderID, f.Model) && len(f.Model) <= 256
}

func chatAutoTextCoordinatorUnavailable() error {
	return chatDeny("automatic_text_coordinator_unavailable", "本轮接收成员无法核验为可用的纯文本协调员。", "是否确认本轮接收成员具备受支持的纯文本协调能力且未配置目录？")
}

// The exact same transaction owns the projections, Task, Mandate, request and
// assessment. Neither a known denial nor a failed write leaves routing behind.
func (p *chatAutomaticTextPlan) initializeInTx(ctx context.Context, tx *sql.Tx, groupID string) error {
	if !p.initialize {
		return nil
	}
	workers, _ := json.Marshal([]string{p.actors.worker.MemberID})
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO group_coordinator_binding_revisions(group_id,revision,member_id) VALUES(?,1,?)`, []any{groupID, p.actors.coordinator.MemberID}},
		{`INSERT INTO group_coordinator_bindings(group_id,revision) VALUES(?,1)`, []any{groupID}},
		{`INSERT INTO group_dispatch_policy_revisions(group_id,revision,allowed_worker_ids) VALUES(?,1,?)`, []any{groupID, string(workers)}},
		{`INSERT INTO group_dispatch_policies(group_id,revision) VALUES(?,1)`, []any{groupID}},
	} {
		if err := coordinatorWrite(ctx, tx, stmt.sql, stmt.args...); err != nil {
			return err
		}
	}
	return nil
}

func (p *chatAutomaticTextPlan) recordInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, decisionID string) error {
	a := p.actors
	return coordinatorWrite(ctx, tx, `INSERT INTO chat_automatic_text_routes(decision_id,candidate_id,source_id,group_id,topic_id,
	 chat_policy_revision,initializes_routing,binding_revision,dispatch_policy_revision,coordinator_id,coordinator_provider_id,coordinator_model,
	 worker_id,worker_provider_id,worker_model,worker_creation_key) VALUES(?,?,?,?,?,?,?,1,1,?,?,?,?,?,?,?)`, decisionID, c.ID, c.SourceID,
		c.GroupID, c.TopicID, c.Policy.Revision, p.initialize, a.coordinator.MemberID, a.coordinator.ProviderID, a.coordinator.Model,
		a.worker.MemberID, a.worker.ProviderID, a.worker.Model, a.workerCreationKey)
}

// A second live delivery of the same source sees the first evaluation's
// immutable receipt, including known denials. No second notice or quota use.
func chatAutomaticTextReceiptIDsInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, expectedID string) (id, candidateID string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT a.id,c.id FROM deliveries d
	 CROSS JOIN chat_candidates c ON c.turn_id=d.turn_id AND (c.message_id,c.group_id,c.topic_id,c.member_id)=(d.message_id,d.group_id,d.topic_id,d.recipient_id)
	 CROSS JOIN chat_intake_executions i ON i.candidate_id=c.id AND i.turn_id=c.turn_id AND i.source_kind='local_user_chat'
	 CROSS JOIN chat_assessments a ON a.candidate_id=c.id
	 WHERE (d.message_id,d.group_id,d.topic_id,i.source_id)=(?,?,?,?) AND c.generation=?
	 AND i.purpose=? AND (?='' OR a.id=?) AND json_extract(a.receipt_json,'$.automatic_text_routing_checked')=1`,
		c.MessageID, c.GroupID, c.TopicID, c.SourceID, c.Generation, domain.ChatIntakePurpose, expectedID, expectedID).Scan(&id, &candidateID)
	return
}

// Role preparation and fresh writes check the same frozen roles. Only decline
// can omit the worker proof. Policy edits do not revoke an already admitted Task.
func chatAutomaticTextRoleInTx(ctx context.Context, tx *sql.Tx, groupID, requestID string, requireWorker bool) (worker string, live bool, err error) {
	applied, err := chatAutomaticTextSchemaInTx(ctx, tx)
	if err != nil {
		return "", false, err
	}
	if !applied {
		// v16 introduced chat assessments. A marked receipt for this exact
		// request still requires the frozen roles if the auto ledger entry is lost.
		var hasAssessments, marked bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.schema_migrations WHERE version=16)`).Scan(&hasAssessments); err != nil || !hasAssessments {
			return "", err == nil, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.coordination_dispatch_requests r
		 JOIN main.mandates m ON m.id=r.mandate_id
		 JOIN main.chat_assessments a ON a.dispatch_request_id=r.id AND a.decision_id=m.decision_id
		 WHERE r.id=? AND r.group_id=? AND json_extract(a.receipt_json,'$.automatic_text_routing_checked')=1)`, requestID, groupID).Scan(&marked); err != nil || !marked {
			return "", err == nil, err
		}
	}
	err = tx.QueryRowContext(ctx, `SELECT a.worker_id,(a.group_id,a.coordinator_id)=(r.group_id,r.coordinator_member_id)
	 AND EXISTS(SELECT 1 FROM members co
	 WHERE (co.group_id,co.id,co.provider_id,co.model)=(a.group_id,a.coordinator_id,a.coordinator_provider_id,a.coordinator_model)
	 AND co.status='active' AND co.workspace='')
	 AND CASE WHEN ? THEN EXISTS(SELECT 1 FROM members w
	 WHERE (w.group_id,w.id,w.provider_id,w.model)=(a.group_id,a.worker_id,a.worker_provider_id,a.worker_model)
	 AND w.status='active' AND w.workspace='') ELSE 1 END
	 FROM coordination_dispatch_requests r JOIN mandates m ON m.id=r.mandate_id
	 JOIN chat_automatic_text_routes a ON a.decision_id=m.decision_id WHERE r.id=? AND r.group_id=?`, requireWorker, requestID, groupID).Scan(&worker, &live)
	if errors.Is(err, sql.ErrNoRows) {
		// With no ledger entry we only got here for a marked automatic request.
		return "", applied, nil
	}
	return
}

func requireChatAutomaticTextAssignmentInTx(ctx context.Context, tx *sql.Tx, r domain.DispatchRequest, workerID string) error {
	worker, live, err := chatAutomaticTextRoleInTx(ctx, tx, r.GroupID, r.ID, true)
	if err != nil {
		return err
	}
	if !live || (worker != "" && worker != workerID) {
		return ErrConflict
	}
	return nil
}
