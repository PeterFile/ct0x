package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

// New writes honor the actual chat action. Historical same-key receipts bypass
// this gate in the caller; legacy explicit tasks have no chat evidence.
func (s *Store) chatTextSubmissionInTx(ctx context.Context, tx *sql.Tx, input SubmitTaskInput) (evidence taskInvestigationEvidence, err error) {
	hasInputs, err := workInputSchemaInTx(ctx, tx)
	if err != nil {
		return evidence, err
	}
	// Both chat relations are mandatory in the applied v28 prefix. Do not
	// infer their absence on the normal path; the keyed join must resolve.
	hasChat, hasScope := hasInputs, hasInputs
	if hasInputs {
		if err := requireHandledWorkConstraintsInTx(ctx, tx, input); err != nil {
			return evidence, err
		}
		if err := requireHandledDecisionAnswersInTx(ctx, tx, input); err != nil {
			return evidence, err
		}
	} else if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='chat_assessments'),
	 EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='chat_policy_scopes')`).Scan(&hasChat, &hasScope); err != nil {
		return evidence, err
	}
	if !hasChat {
		return evidence, nil
	}
	var action, authority, scope string
	scopeColumn, scopeJoin := "''", ""
	if hasScope {
		scopeColumn = "coalesce(s.scope_kind,'')"
		scopeJoin = " LEFT JOIN chat_policy_scopes s ON (s.group_id,s.topic_id,s.revision)=(c.group_id,c.topic_id,c.policy_revision)"
	}
	err = tx.QueryRowContext(ctx, `SELECT coalesce(json_extract(a.receipt_json,'$.action'),''),coalesce(json_extract(a.receipt_json,'$.authority'),''),`+scopeColumn+`
	 FROM mandates m INDEXED BY mandates_group_task JOIN chat_assessments a ON a.decision_id=m.decision_id
	 LEFT JOIN chat_candidates c ON c.id=a.candidate_id`+scopeJoin+`
	 WHERE m.group_id=? AND m.task_id=?`, input.GroupID, input.TaskID).Scan(&action, &authority, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return evidence, nil
	}
	if err != nil {
		return evidence, err
	}
	if action == domain.ChatActionWorkspaceChange {
		return evidence, chatWorkspaceSubmissionInTx(ctx, tx, input)
	}
	if action == domain.ChatActionReadOnlyInvestigation {
		return s.chatInvestigationSubmissionInTx(ctx, tx, input)
	}
	if action != domain.ChatActionTextSubmission || len(input.ArtifactVersions) != 0 || !utf8.ValidString(input.Summary) {
		return evidence, fmt.Errorf("%w：聊天文本交付不允许Artifact版本或非文本结果", ErrInvalid)
	}
	if len(input.Summary) > 8192 {
		if authority == "written_preauthorization" && scope == domain.ChatPreparationTopicSummary {
			return evidence, fmt.Errorf("%w：本话题预授权摘要最多8192字节", ErrInvalid)
		}
		return evidence, fmt.Errorf("%w：聊天文本交付最多8192字节", ErrInvalid)
	}
	return evidence, nil
}

func chatWorkspaceSubmissionInTx(ctx context.Context, tx *sql.Tx, input SubmitTaskInput) error {
	if _, err := taskWorkspaceGrantInTx(ctx, tx, input.GroupID, input.TaskID, input.ActorID); err != nil {
		return err
	}
	if len(input.ArtifactVersions) == 0 || !utf8.ValidString(input.Summary) {
		return fmt.Errorf("%w：目录工作必须提交真实冻结的成果版本和有效文本说明", ErrInvalid)
	}
	for _, ref := range input.ArtifactVersions {
		version, err := artifactVersionInTx(ctx, tx, input.GroupID, ref.ArtifactID, ref.VersionID)
		if err != nil {
			return err
		}
		if version.SourceTaskID != input.TaskID {
			return fmt.Errorf("%w：目录成果版本必须来自同群同任务", ErrInvalid)
		}
		if version.AuthorID != input.ActorID {
			return ErrMemberToolDenied
		}
	}
	// Validate immutable execution-bound publication receipts in this SAME tx.
	// Legacy trusted artifact metadata is not proof of scoped filesystem IO.
	return workerArtifactPublicationsInTx(ctx, tx, input)
}

// A durable Submission is returned to its chat origin in the same transaction.
// This notice is not a native terminal receipt and never releases claims.
func chatSubmissionNoticeInTx(ctx context.Context, tx *sql.Tx, submission domain.Submission, expectedTopic string) error {
	var topic string
	err := tx.QueryRowContext(ctx, `SELECT m.topic_id FROM mandates m INDEXED BY mandates_group_task
	 JOIN chat_assessments a ON a.decision_id=m.decision_id WHERE m.group_id=? AND m.task_id=?`, submission.GroupID, submission.TaskID).Scan(&topic)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if topic != expectedTopic || !utf8.ValidString(submission.Summary) {
		return ErrConflict
	}
	summary := submission.Summary
	if len(summary) > 8192 {
		end := 8192
		for !utf8.RuneStart(summary[end]) {
			end--
		}
		summary = summary[:end] + "\n（完整成果见此 Submission。）"
	}
	message, err := createMessageInTx(ctx, tx, MessageRequest{GroupID: submission.GroupID, TopicID: topic, ActorID: submission.SubmittedBy,
		RecipientIDs: []string{}, Content: fmt.Sprintf("成果已提交，尚未由本机用户接纳。\n任务：%s\nSubmission：%s\n任务版本：%d\n%s", submission.TaskID, submission.ID, submission.TaskRevision, summary)}, nil)
	if err != nil {
		return err
	}
	return coordinationDispatchWrite(ctx, tx, "INSERT INTO chat_submission_notices(submission_id,message_id) VALUES(?,?)", submission.ID, message.ID)
}
