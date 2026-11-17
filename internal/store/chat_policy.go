package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"agentworkspace/internal/domain"
)

// SetChatPolicy is local-user-only. Member tools cannot enable or revise policy.
func (s *Store) SetChatPolicy(ctx context.Context, thread domain.ThreadRef, input domain.ChatPolicyRequest, expected int64, key string) (domain.ChatPolicy, error) {
	if !validTriggerID(thread.GroupID) || !memberToolKeyValid(key) || expected < 0 || expected == math.MaxInt64 {
		return domain.ChatPolicy{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ChatPolicy{}, admissionError("设置聊天策略失败", err)
	}
	defer tx.Rollback()
	if err := requireMessageActor(ctx, tx, thread.GroupID, "user"); err != nil {
		return domain.ChatPolicy{}, admissionError("验证聊天策略权限失败", err)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return domain.ChatPolicy{}, err
	}
	payload := mutationPayload(thread.GroupID, thread.TopicID, strconv.FormatInt(expected, 10), string(encoded))
	scope := mutationScope{operation: "set_chat_policy", actorID: "user", groupID: thread.GroupID, key: key}
	id, replay, err := lookupMutation(ctx, tx, scope, payload)
	if err != nil {
		return domain.ChatPolicy{}, admissionError("读取聊天策略回执失败", err)
	}
	revision := expected + 1
	if replay {
		revision, err = strconv.ParseInt(id, 10, 64)
		if err != nil || revision < 1 {
			return domain.ChatPolicy{}, ErrConflict
		}
	} else {
		if input.Action != domain.ChatActionTextSubmission || input.MaxAdmissions < 1 || input.MaxAdmissions > 100 ||
			input.PreauthorizationMessageID != "" ||
			(input.AllowPreparation && input.PreparationScopeKind != domain.ChatPreparationTopicSummary) ||
			(!input.AllowPreparation && input.PreparationScopeKind != "") {
			return domain.ChatPolicy{}, fmt.Errorf("%w：只支持显式配置本话题文本摘要范围，不支持聊天消息ID或自由文本预授权", ErrInvalid)
		}
		current, err := chatPolicyInTx(ctx, tx, thread, 0)
		if err != nil {
			return domain.ChatPolicy{}, admissionError("读取聊天策略失败", err)
		}
		if current.Revision != expected {
			return domain.ChatPolicy{}, ErrConflict
		}
		rule := fmt.Sprintf("本机用户已配置本群、本话题聊天准入策略：启用=%t；仅允许纯文本交付；本版总准入额度=%d。", input.Enabled, input.MaxAdmissions)
		if input.AllowPreparation {
			rule += "\n预授权范围 topic_summary：仅依据本群本话题已有消息准备纯文本摘要；目标与验收由服务端固定。不得改为其他目标、引用Artifact版本、读写文件或执行外部动作。"
		}
		command, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: thread.GroupID, TopicID: thread.TopicID, ActorID: "user", Content: rule, RecipientIDs: []string{}}, nil)
		if err != nil {
			return domain.ChatPolicy{}, admissionError("保存策略规范规则失败", err)
		}
		sourceID, err := messageCommandSourceInTx(ctx, tx, command.ID, "local_user_policy", payload, key, nil)
		if err != nil {
			return domain.ChatPolicy{}, admissionError("保存策略命令来源失败", err)
		}
		preauthorizationID := ""
		if input.AllowPreparation {
			preauthorizationID = command.ID
		}
		if err := coordinatorWrite(ctx, tx, `INSERT INTO chat_policy_revisions(group_id,topic_id,revision,enabled,allow_preparation,preauthorization_message_id,action,max_admissions,authorized_by)
		 VALUES(?,?,?,?,?,nullif(?,''),?,?, 'user')`, thread.GroupID, thread.TopicID, revision, input.Enabled, input.AllowPreparation,
			preauthorizationID, input.Action, input.MaxAdmissions); err != nil {
			return domain.ChatPolicy{}, admissionError("保存聊天策略版本失败", err)
		}
		if err := coordinatorWrite(ctx, tx, `INSERT INTO chat_policy_scopes(group_id,topic_id,revision,scope_kind,command_source_id) VALUES(?,?,?,?,?)`, thread.GroupID, thread.TopicID, revision, input.PreparationScopeKind, sourceID); err != nil {
			return domain.ChatPolicy{}, admissionError("保存策略可执行范围失败", err)
		}
		if expected == 0 {
			err = coordinatorWrite(ctx, tx, "INSERT INTO chat_policies(group_id,topic_id,revision) VALUES(?,?,?)", thread.GroupID, thread.TopicID, revision)
		} else {
			err = coordinatorWrite(ctx, tx, "UPDATE chat_policies SET revision=? WHERE group_id=? AND topic_id=? AND revision=?", revision, thread.GroupID, thread.TopicID, expected)
		}
		if err == nil {
			err = coordinatorWrite(ctx, tx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id) VALUES(?,?,?,?,?,?)`, scope.operation, scope.actorID, scope.groupID, key, payload, strconv.FormatInt(revision, 10))
		}
		if err != nil {
			return domain.ChatPolicy{}, admissionError("保存聊天策略失败", err)
		}
	}
	policy, err := chatPolicyInTx(ctx, tx, thread, revision)
	if err != nil {
		return domain.ChatPolicy{}, admissionError("读取聊天策略版本失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ChatPolicy{}, admissionError("保存聊天策略失败", err)
	}
	return policy, nil
}

func (s *Store) GetChatPolicy(ctx context.Context, thread domain.ThreadRef) (domain.ChatPolicy, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.ChatPolicy{}, admissionError("读取聊天策略失败", err)
	}
	defer tx.Rollback()
	if err := requireMessageActor(ctx, tx, thread.GroupID, "user"); err != nil {
		return domain.ChatPolicy{}, admissionError("读取聊天策略失败", err)
	}
	policy, err := chatPolicyInTx(ctx, tx, thread, 0)
	if err != nil {
		return domain.ChatPolicy{}, admissionError("读取聊天策略失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ChatPolicy{}, admissionError("读取聊天策略失败", err)
	}
	return policy, nil
}

// revision=0 selects current; absence is explicitly disabled, not inferred authorization.
func chatPolicyInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, revision int64) (domain.ChatPolicy, error) {
	policy := domain.ChatPolicy{ThreadRef: thread}
	query := `SELECT r.revision,r.enabled,r.allow_preparation,coalesce(r.preauthorization_message_id,''),r.action,r.max_admissions,r.authorized_by,coalesce(m.content,'')
	 FROM chat_policy_revisions r LEFT JOIN messages m ON m.id=r.preauthorization_message_id
	 WHERE r.group_id=? AND r.topic_id=? AND r.revision=?`
	if revision == 0 {
		query = `SELECT r.revision,r.enabled,r.allow_preparation,coalesce(r.preauthorization_message_id,''),r.action,r.max_admissions,r.authorized_by,coalesce(m.content,'')
		 FROM chat_policies p JOIN chat_policy_revisions r USING(group_id,topic_id,revision)
		 LEFT JOIN messages m ON m.id=r.preauthorization_message_id WHERE p.group_id=? AND p.topic_id=?`
	}
	args := []any{thread.GroupID, thread.TopicID}
	if revision != 0 {
		args = append(args, revision)
	}
	err := tx.QueryRowContext(ctx, query, args...).Scan(&policy.Revision, &policy.Enabled, &policy.AllowPreparation,
		&policy.PreauthorizationMessageID, &policy.Action, &policy.MaxAdmissions, &policy.AuthorizedBy, &policy.PreauthorizationText)
	if revision == 0 && errors.Is(err, sql.ErrNoRows) {
		return policy, nil
	}
	if err == nil {
		scopeErr := tx.QueryRowContext(ctx, `SELECT scope_kind,command_source_id FROM chat_policy_scopes WHERE group_id=? AND topic_id=? AND revision=?`, thread.GroupID, thread.TopicID, policy.Revision).Scan(&policy.PreparationScopeKind, &policy.ConfigurationSourceID)
		if scopeErr != nil && !errors.Is(scopeErr, sql.ErrNoRows) {
			return domain.ChatPolicy{}, scopeErr
		}
		policy.PreparationSupported = policy.AllowPreparation && policy.PreparationScopeKind == domain.ChatPreparationTopicSummary && policy.ConfigurationSourceID != ""
	}
	return policy, err
}
