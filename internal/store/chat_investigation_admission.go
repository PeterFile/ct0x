package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

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
	if err := tx.QueryRowContext(ctx, `SELECT message_id FROM message_command_sources WHERE id=? AND kind='local_user_policy'`, p.ConfigurationSourceID).Scan(&authorization); err != nil {
		return "", err
	}
	followup, err := chatInvestigationReferencesExistingWorkInTx(ctx, tx, c)
	if err != nil {
		return "", err
	}
	if followup {
		return "", chatDeny("investigation_followup_requires_clarification", "本机用户消息指向本话题已有工作，不应作为新调查准入。", "请确认要在本话题继续哪项已有任务，以及是否沿用原范围？")
	}
	return authorization, nil
}

var (
	chatInvestigationEnglishCue = regexp.MustCompile(
		`^(?i:(?:please )?(?:continue|resume) the original task)(?:$|[\s\p{Z},.!?;:，。！？；：、])`)
	chatInvestigationMetaCue = regexp.MustCompile(
		`(?:(?:这句(?:话)?|这条消息)[\s\p{Z}]*(?:只|仅)?是[\s\p{Z}]*(?:旧消息(?:示例)?|(?:一个)?(?:示例|例子))|` +
			`(?:^|[,:，：、])[\s\p{Z}]*并?(?:非|不是)本次指令)[\s\p{Z}]*(?:$|[,，])`)
	chatInvestigationNegatedActivity = regexp.MustCompile(`^(?:非|不是|不要|不必|不用|无需|没有|未曾)`)
)

// This high-precision cue only considers the opening sentence. It neither
// interprets arbitrary phrasing (the model IntentPass handles meaning) nor
// supplies authority. Keep quoted openings and explicit mentions distinct.
func chatInvestigationHasOriginalTaskCue(content string) bool {
	content = strings.TrimSpace(content)
	text := strings.TrimSpace(strings.TrimPrefix(content, "请"))
	rest, ok := strings.CutPrefix(text, "继续原任务")
	if !ok {
		rest, ok = strings.CutPrefix(text, "续办原任务")
	}
	if !ok {
		return chatInvestigationEnglishCue.MatchString(content)
	}
	rest = strings.TrimPrefix(rest, "吧")
	if end := strings.IndexAny(rest, ".!?;。！？；"); end >= 0 {
		rest = rest[:end]
	}
	if chatInvestigationMetaCue.MatchString(rest) {
		return false
	}
	tail := strings.TrimLeftFunc(rest, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",:，：、", r)
	})
	for _, connective := range [...]string{"并且", "并", "然后", "同时", "再"} {
		if activity, ok := strings.CutPrefix(tail, connective); ok {
			activity = strings.TrimSpace(activity)
			first, _ := utf8.DecodeRuneInString(activity)
			// Require activity text, not a bare connective or an explicit negation.
			return unicode.IsLetter(first) && !chatInvestigationNegatedActivity.MatchString(activity)
		}
	}
	return rest == "" || tail != rest // End of sentence or an ordinary separator.
}

func chatInvestigationReferencesExistingWorkInTx(ctx context.Context, tx *sql.Tx, c domain.ChatCandidate) (bool, error) {
	if c.SourceKind != "local_user_chat" {
		return false, nil // Member text is not a local-user command.
	}
	source, err := chatDiscussionRootSourceInTx(ctx, tx, c)
	if err != nil {
		return false, err
	}
	var found bool
	if chatInvestigationHasOriginalTaskCue(source.content) {
		// Walk only the bound thread's indexed lineage, stopping at the first prior Task.
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		 SELECT 1 FROM coordination_triggers o INDEXED BY coordination_triggers_thread_created_id
		 CROSS JOIN routing_decisions d ON (d.trigger_id,d.group_id,d.topic_id)=(o.id,o.group_id,o.topic_id)
		 CROSS JOIN tasks t ON (t.id,t.group_id)=(d.task_id,d.group_id)
		 WHERE o.group_id=? AND o.topic_id=? AND o.message_sequence<?)`, c.GroupID, c.TopicID, source.sequence).Scan(&found)
		if err != nil || found {
			return found, err
		}
	}

	// Probe whole tokens long enough for crypto/rand.Text IDs, without folding
	// case or matching titles or substrings of longer ASCII identifiers.
	var ids []string
	for token := range strings.FieldsFuncSeq(source.content, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) {
		if len(token) >= 26 && validTriggerID(token) {
			ids = append(ids, token)
		}
	}
	if len(ids) == 0 {
		return false, nil
	}
	refs, _ := json.Marshal(ids) // A slice of plain strings cannot fail to encode.
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(
	 SELECT 1 FROM json_each(?) ref
	 CROSS JOIN task_coordination tc ON tc.task_id=ref.value
	 CROSS JOIN tasks t ON (t.id,t.group_id)=(tc.task_id,tc.group_id)
	 WHERE tc.group_id=? AND tc.topic_id=?)`, string(refs), c.GroupID, c.TopicID).Scan(&found)
	return found, err
}

const investigationSubmissionMaxBytes = 8192

func chatInvestigationTask(c domain.ChatCandidate) (title, description, acceptance string) {
	p := c.Policy.Investigation
	refs, _ := json.Marshal(p.ArtifactVersions) // Only validated pairs of strings.
	return p.Objective, fmt.Sprintf("只读调查仅限准入群和话题。允许读取本话题消息=%t；允许读取的确切成果版本=%s；委派上限=%d（不是工具授权）。策略版本=%d，配置来源=%s。不得使用文件、进程或网络工具。",
			p.AllowTopicMessages, refs, p.MaxDelegations, c.Policy.Revision, c.Policy.ConfigurationSourceID),
		fmt.Sprintf("仅提交非空、有效UTF-8且不超过%d字节的纯文本调查结论；注明所用的获准资料并说明未决问题；不得附带成果版本。", investigationSubmissionMaxBytes)
}
