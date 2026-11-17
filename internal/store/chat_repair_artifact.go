package store

import (
	"context"
	"database/sql"
)

// The publication caller first fences the actual live role/Task and checks its
// original key receipt. This helper adds no permission and performs no IO.
func chatRepairArtifactBaseInTx(ctx context.Context, tx *sql.Tx, groupID, taskID, artifactID, expectedVersionID string) error {
	supported, err := chatRepairSupportedInTx(ctx, tx)
	if err != nil || !supported {
		return err
	}
	var repair bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM chat_repair_links WHERE group_id=? AND repair_task_id=?)", groupID, taskID).Scan(&repair); err != nil {
		return err
	}
	if !repair {
		return nil
	}
	if artifactID == "" || expectedVersionID == "" {
		return ErrConflict
	}
	var exact bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM chat_repair_links l
	 JOIN chat_repair_artifact_versions b ON b.repair_task_id=l.repair_task_id
	 JOIN task_submission_artifacts a ON a.submission_id=l.parent_submission_id AND a.task_id=l.parent_task_id AND a.group_id=l.group_id
	 AND a.task_revision=l.reviewed_task_revision AND a.artifact_id=b.artifact_id AND a.version_id=b.version_id
	 JOIN artifact_versions v ON v.id=b.version_id AND v.artifact_id=b.artifact_id AND v.group_id=l.group_id
	 WHERE l.group_id=? AND l.repair_task_id=? AND b.artifact_id=? AND b.version_id=?)`, groupID, taskID, artifactID, expectedVersionID).Scan(&exact); err != nil {
		return err
	}
	if !exact {
		return ErrConflict
	}
	return nil
}
