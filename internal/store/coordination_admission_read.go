package store

import (
	"context"
	"database/sql"
	"fmt"

	"agentworkspace/internal/domain"
)

// GetAdmissionReceipt reads the original receipt, never the current Task or authority pointer.
func (s *Store) GetAdmissionReceipt(ctx context.Context, thread domain.ThreadRef, decisionID string) (domain.AdmissionReceipt, error) {
	if err := validateTriggerThread(thread); err != nil {
		return domain.AdmissionReceipt{}, err
	}
	if !validTriggerID(decisionID) {
		return domain.AdmissionReceipt{}, fmt.Errorf("%w：决定标识无效", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.AdmissionReceipt{}, storeError("读取工作请求回执失败", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM routing_decisions WHERE group_id=? AND topic_id=? AND id=?`, thread.GroupID, thread.TopicID, decisionID).Scan(&exists); err != nil {
		return domain.AdmissionReceipt{}, storeError("读取工作请求回执失败", err)
	}
	receipt, err := admissionReceiptInTx(ctx, tx, thread.GroupID, decisionID)
	if err != nil {
		return domain.AdmissionReceipt{}, storeError("读取工作请求回执失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.AdmissionReceipt{}, storeError("读取工作请求回执失败", err)
	}
	return receipt, nil
}

// GetTaskCoordination resolves the saved pointer and its exact receipt in one snapshot.
func (s *Store) GetTaskCoordination(ctx context.Context, groupID, taskID string) (domain.TaskCoordinationView, error) {
	if err := validateTriggerThread(domain.ThreadRef{GroupID: groupID}); err != nil {
		return domain.TaskCoordinationView{}, err
	}
	if !validTriggerID(taskID) {
		return domain.TaskCoordinationView{}, fmt.Errorf("%w：任务标识无效", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.TaskCoordinationView{}, storeError("读取任务协调记录失败", err)
	}
	defer tx.Rollback()
	var view domain.TaskCoordinationView
	var decisionID string
	c := &view.Coordination
	// Topic comes only from the saved coordination. Validate the entire immutable pair.
	err = tx.QueryRowContext(ctx, `SELECT c.task_id,c.group_id,c.topic_id,c.current_mandate_id,c.authority_generation,d.id
	 FROM task_coordination c JOIN mandates m ON m.id=c.current_mandate_id
	 AND m.task_id=c.task_id AND m.group_id=c.group_id AND m.topic_id=c.topic_id
	 JOIN routing_decisions d ON d.id=m.decision_id AND d.mandate_id=m.id
	 AND d.trigger_id=m.origin_trigger_id AND d.task_id=m.task_id AND d.group_id=m.group_id
	 AND d.topic_id=m.topic_id AND d.task_revision_at_admission=m.task_revision_at_admission
	 WHERE c.group_id=? AND c.task_id=?`, groupID, taskID).Scan(
		&c.TaskID, &c.GroupID, &c.TopicID, &c.CurrentMandateID, &c.AuthorityGeneration, &decisionID)
	if err != nil {
		return domain.TaskCoordinationView{}, storeError("读取任务协调记录失败", err)
	}
	view.Admission, err = admissionReceiptInTx(ctx, tx, groupID, decisionID)
	if err != nil {
		return domain.TaskCoordinationView{}, storeError("读取任务授权回执失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.TaskCoordinationView{}, storeError("读取任务协调记录失败", err)
	}
	return view, nil
}
