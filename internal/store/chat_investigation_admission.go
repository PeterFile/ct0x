package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"agentworkspace/internal/domain"
)

// EnableInvestigationExecution is a process-local startup assertion by trusted
// composition. It never grants tools, and a reopened Store starts disabled.
func (s *Store) EnableInvestigationExecution() { s.investigationExecution.Store(true) }

// The common chat gate has already checked the live revision, source, budget
// and routing. Both fresh policy and material checks are in that same tx.
func (s *Store) chatInvestigationAdmissionInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate, input domain.ChatAssessment) (string, error) {
	if input.Disposition != "prepare_work" || input.Repair != nil {
		return "", chatDeny("investigation_prepare_required", "只读调查只允许在本机用户配置的范围内自主准备。", "是否按本话题已配置的只读调查目标准备？")
	}
	p := c.Policy
	if p.Investigation == nil || p.ConfigurationSourceID == "" || p.AuthorizedBy != "user" {
		return "", chatDeny("investigation_not_authorized", "本话题未获本机用户的只读调查预授权。", "是否在本话题策略中明确配置调查目标和可读资料？")
	}
	if !s.investigationExecution.Load() {
		return "", chatDeny("investigation_runtime_unavailable", "当前实例未启用专用的只读调查执行。", "是否先启用支持受限工具的只读调查执行配置？")
	}
	materialsMissing := func() error {
		return chatDeny("investigation_materials_unavailable", "已配置的只读调查资料不足或已不可用。", "是否在本话题策略中允许读取本话题消息或指定可用的成果版本？")
	}
	// AllowTopicMessages has at least the candidate's persisted same-thread source.
	if err := validateChatInvestigationPolicy(*p.Investigation); err != nil {
		return "", materialsMissing()
	}
	if err := chatInvestigationVersionsInTx(ctx, tx, c.GroupID, p.Investigation.ArtifactVersions); err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", materialsMissing()
		}
		return "", err
	}
	var authorization string
	err := tx.QueryRowContext(ctx, `SELECT message_id FROM message_command_sources WHERE id=? AND kind='local_user_policy'`, p.ConfigurationSourceID).Scan(&authorization)
	return authorization, err
}

const investigationSubmissionMaxBytes = 8192

func chatInvestigationTask(c domain.ChatCandidate) (title, description, acceptance string) {
	p := c.Policy.Investigation
	refs, _ := json.Marshal(p.ArtifactVersions) // Only validated pairs of strings.
	return p.Objective, fmt.Sprintf("只读调查仅限准入群和话题。允许读取本话题消息=%t；允许读取的确切成果版本=%s；委派上限=%d（不是工具授权）。策略版本=%d，配置来源=%s。不得使用文件、进程或网络工具。",
			p.AllowTopicMessages, refs, p.MaxDelegations, c.Policy.Revision, c.Policy.ConfigurationSourceID),
		fmt.Sprintf("仅提交非空、有效UTF-8且不超过%d字节的纯文本调查结论；注明所用的获准资料并说明未决问题；不得附带成果版本。", investigationSubmissionMaxBytes)
}
