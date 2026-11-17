package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrChatRepairPending is temporary readiness, never a durable assessment or a
// terminal execution receipt. Trusted preparation maps it to scheduler NotReady.
var ErrChatRepairPending = errors.New("原执行或文件操作尚待终态对账")

// CheckChatRepairIntakeReady is trusted preclaim preparation. A pending result
// leaves the SAME queued delivery unclaimed for the existing scheduler. It does
// not backfill old Review events, mutate assessments, or release any claim.
func (s *Store) CheckChatRepairIntakeReady(ctx context.Context, groupID, deliveryID string) error {
	if !validTriggerID(groupID) || !validTriggerID(deliveryID) {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return admissionError("读取修复评估就绪状态失败", err)
	}
	defer tx.Rollback()
	var messageID string
	if err := tx.QueryRowContext(ctx, "SELECT message_id FROM deliveries WHERE group_id=? AND id=?", groupID, deliveryID).Scan(&messageID); err != nil {
		return admissionError("读取修复评估投递失败", err)
	}
	supported, err := chatRepairSupportedInTx(ctx, tx)
	if err != nil || !supported {
		return err
	}
	var parentTaskID string
	var stale bool
	err = tx.QueryRowContext(ctx, `SELECT r.task_id,NOT(parent.status='open' AND parent.revision-1=r.task_revision
	 AND parent.active_attempt_id IS NULL AND parent.current_submission_id IS NULL)
	 OR EXISTS(SELECT 1 FROM chat_repair_links l WHERE l.parent_review_id=r.id)
	 FROM chat_repair_review_intakes i JOIN task_review_decisions r
	 ON r.id=i.review_id AND r.group_id=i.group_id AND r.outcome='rejected' AND r.reviewed_by='user'
	 JOIN tasks parent ON parent.id=r.task_id AND parent.group_id=r.group_id
	 WHERE i.group_id=? AND i.delivery_id=? AND i.message_id=?`, groupID, deliveryID, messageID).Scan(&parentTaskID, &stale)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return admissionError("读取正式修复评估来源失败", err)
	}
	if !stale {
		if err := chatRepairParentReadyInTx(ctx, tx, groupID, parentTaskID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return admissionError("读取修复评估就绪状态失败", err)
	}
	return nil
}

func chatRepairParentReadyInTx(ctx context.Context, tx *sql.Tx, groupID, taskID string) error {
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_attempts a JOIN turns t ON t.id=a.turn_id
	 WHERE a.group_id=? AND a.task_id=? AND t.state IN ('starting','running','cancel_requested','unknown'))`, groupID, taskID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return ErrChatRepairPending
	}
	var workspace bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM chat_workspace_scopes WHERE group_id=? AND task_id=?)", groupID, taskID).Scan(&workspace); err != nil {
		return err
	}
	if workspace {
		pending, err := workspaceOperationsUnresolvedInTx(ctx, tx, groupID, taskID, "")
		if err != nil {
			return err
		}
		if pending {
			return ErrChatRepairPending
		}
	}
	return nil
}
