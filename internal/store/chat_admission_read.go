package store

import (
	"context"
	"database/sql"

	"agentworkspace/internal/domain"
)

// GetChatAdmission returns immutable intake evidence for the original Task.
// Legacy explicit API admissions have no chat evidence and return not found.
func (s *Store) GetChatAdmission(ctx context.Context, groupID, taskID string) (domain.ChatAssessmentReceipt, error) {
	if !validTriggerID(groupID) || !validTriggerID(taskID) {
		return domain.ChatAssessmentReceipt{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.ChatAssessmentReceipt{}, admissionError("读取聊天授权依据失败", err)
	}
	defer tx.Rollback()
	if err := requireMessageActor(ctx, tx, groupID, "user"); err != nil {
		return domain.ChatAssessmentReceipt{}, admissionError("读取聊天授权依据失败", err)
	}
	var id, candidateID string
	err = tx.QueryRowContext(ctx, `SELECT a.id,a.candidate_id FROM mandates m INDEXED BY mandates_group_task JOIN chat_assessments a ON a.decision_id=m.decision_id
	 WHERE m.group_id=? AND m.task_id=?`, groupID, taskID).Scan(&id, &candidateID)
	if err != nil {
		return domain.ChatAssessmentReceipt{}, admissionError("读取聊天授权依据失败", err)
	}
	receipt, err := chatAssessmentReceiptInTx(ctx, tx, id, candidateID)
	if err != nil {
		return domain.ChatAssessmentReceipt{}, admissionError("读取聊天授权依据失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ChatAssessmentReceipt{}, admissionError("读取聊天授权依据失败", err)
	}
	return receipt, nil
}
