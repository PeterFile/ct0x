package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"agentworkspace/internal/domain"
)

const (
	investigationMaxObjectiveBytes = 256
	investigationMaxArtifactRefs   = 16
	investigationMaxIDBytes        = 128
	investigationMaxDelegations    = 4
)

// Genuine legacy storage has neither the migration marker nor the scope table.
// A mismatch must not make a restricted task look like ordinary work.
func chatInvestigationSupportedInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	version := slices.Index(migrations, chatInvestigationSchema) + 1
	var applied, present bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=?),
	 EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='chat_investigation_policy_revisions')`, version).Scan(&applied, &present)
	if err != nil {
		return false, err
	}
	if applied != present {
		return false, fmt.Errorf("%w：只读调查迁移标记与策略表不一致", ErrTaskInvestigationNotReady)
	}
	return applied, nil
}

func validateChatInvestigationPolicy(input domain.ChatInvestigationPolicy) error {
	if !memberToolTextValid(input.Objective, investigationMaxObjectiveBytes) || input.MaxDelegations < 0 || input.MaxDelegations > investigationMaxDelegations {
		return fmt.Errorf("%w：只读调查必须提供不超过%d字节的非空目标，委派上限为0至%d", ErrInvalid, investigationMaxObjectiveBytes, investigationMaxDelegations)
	}
	if len(input.ArtifactVersions) > investigationMaxArtifactRefs || (!input.AllowTopicMessages && len(input.ArtifactVersions) == 0) {
		return fmt.Errorf("%w：只读调查必须明确允许本话题消息或指定实际成果版本（最多%d个）", ErrInvalid, investigationMaxArtifactRefs)
	}
	seen := make(map[domain.ArtifactVersionRef]bool, len(input.ArtifactVersions))
	for _, ref := range input.ArtifactVersions {
		if len(ref.ArtifactID) > investigationMaxIDBytes || len(ref.VersionID) > investigationMaxIDBytes ||
			!validTriggerID(ref.ArtifactID) || !validTriggerID(ref.VersionID) || seen[ref] {
			return fmt.Errorf("%w：只读调查资料必须使用不重复的确切成果和版本标识", ErrInvalid)
		}
		seen[ref] = true
	}
	return nil
}

func chatInvestigationVersionsInTx(ctx context.Context, tx *sql.Tx, groupID string, refs []domain.ArtifactVersionRef) error {
	for _, ref := range refs {
		if _, err := artifactVersionInTx(ctx, tx, groupID, ref.ArtifactID, ref.VersionID); err != nil {
			return err
		}
	}
	return nil
}

func prepareChatInvestigationPolicyInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, input *domain.ChatInvestigationPolicy) (string, error) {
	if input == nil {
		return "", nil
	}
	if err := validateChatInvestigationPolicy(*input); err != nil {
		return "", err
	}
	supported, err := chatInvestigationSupportedInTx(ctx, tx)
	if err != nil {
		return "", err
	}
	if !supported {
		return "", fmt.Errorf("%w：当前数据库版本不支持只读调查策略", ErrInvalid)
	}
	err = chatInvestigationVersionsInTx(ctx, tx, thread.GroupID, input.ArtifactVersions)
	if errors.Is(err, ErrNotFound) {
		return "", fmt.Errorf("%w：只读调查资料必须引用本群已保存的确切成果版本", ErrInvalid)
	}
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(input)
	return string(data), err
}

func chatInvestigationPolicyInTx(ctx context.Context, tx *sql.Tx, thread domain.ThreadRef, revision int64) (*domain.ChatInvestigationPolicy, error) {
	var data string
	err := tx.QueryRowContext(ctx, `SELECT scope_json FROM chat_investigation_policy_revisions WHERE group_id=? AND topic_id=? AND revision=?`,
		thread.GroupID, thread.TopicID, revision).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var policy domain.ChatInvestigationPolicy
	if err := json.Unmarshal([]byte(data), &policy); err != nil {
		return nil, err
	}
	return &policy, nil
}
