package store

import (
	"context"
	"database/sql"
	"time"
)

const workspaceOperationColumns = `o.id,o.group_id,o.task_id,o.task_revision,o.assignment_id,o.attempt_id,o.turn_id,o.member_id,
 o.grant_id,o.resource_key,o.node_id,o.claim_digest,o.relative_path,o.kind,o.expected_hash,o.after_hash,o.after_size,
 o.preimage_exists,o.preimage_hash,o.preimage_blob_hash,o.preimage_size,o.notice_message_id,o.created_at,
 r.outcome,r.observed_hash,r.observed_size,r.error_code,r.notice_message_id,r.finished_at`
const workspaceOperationFrom = ` FROM workspace_operations o LEFT JOIN workspace_operation_receipts r ON r.operation_id=o.id`
const workspaceOperationUnresolved = `(r.operation_id IS NULL OR r.outcome IN ('unknown','applied_sync_error'))`

func scanWorkspaceOperation(row scanner) (o WorkspaceOperation, err error) {
	var created string
	var outcome, hash, code, notice, finished sql.NullString
	var size sql.NullInt64
	err = row.Scan(&o.ID, &o.GroupID, &o.TaskID, &o.TaskRevision, &o.AssignmentID, &o.AttemptID, &o.TurnID, &o.MemberID,
		&o.GrantID, &o.ResourceKey, &o.NodeID, &o.claimDigest, &o.Input.RelativePath, &o.Input.Kind, &o.Input.ExpectedHash, &o.Input.AfterHash, &o.Input.AfterSize,
		&o.Preimage.Exists, &o.Preimage.Hash, &o.Preimage.BlobHash, &o.Preimage.Size, &o.NoticeMessageID, &created,
		&outcome, &hash, &size, &code, &notice, &finished)
	if err != nil {
		return o, err
	}
	if o.CreatedAt, err = time.Parse(timestampLayout, created); err != nil {
		return o, err
	}
	if outcome.Valid {
		o.Result = &WorkspaceOperationResult{Outcome: outcome.String, ObservedHash: hash.String, ObservedSize: size.Int64, ErrorCode: code.String}
		o.FinishNoticeMessageID = notice.String
		var at time.Time
		if at, err = time.Parse(timestampLayout, finished.String); err == nil {
			o.FinishedAt = &at
		}
	}
	return o, err
}

func workspaceOperationInTx(ctx context.Context, tx *sql.Tx, groupID, id string) (WorkspaceOperation, error) {
	return scanWorkspaceOperation(tx.QueryRowContext(ctx, "SELECT "+workspaceOperationColumns+workspaceOperationFrom+" WHERE o.group_id=? AND o.id=?", groupID, id))
}

func workspaceOperationsUnresolvedInTx(ctx context.Context, tx *sql.Tx, groupID, taskID, turnID string) (bool, error) {
	var unresolved bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1"+workspaceOperationFrom+" WHERE o.group_id=? AND (o.task_id=? OR o.turn_id=?) AND "+workspaceOperationUnresolved+")", groupID, taskID, turnID).Scan(&unresolved)
	return unresolved, err
}

// HasUnresolvedWorkspaceOperations is a trusted runtime audit, not member tool
// authority. Errors must also aggregate to OutcomeUnknown before settling a Turn.
// It observes retained pending/unknown/error evidence after cancellation/restart.
func (s *Store) HasUnresolvedWorkspaceOperations(ctx context.Context, groupID, turnID string) (bool, error) {
	var unresolved bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1"+workspaceOperationFrom+` WHERE o.group_id=t.group_id AND o.turn_id=t.id AND `+workspaceOperationUnresolved+`) FROM turns t WHERE t.group_id=? AND t.id=?`, groupID, turnID).Scan(&unresolved)
	return unresolved, storeError("核对未确认文件操作失败", err)
}

// ListWorkspaceOperationsForTurn reads only this exact group's Turn, with a
// bounded immutable-history cursor. It neither repairs evidence nor releases claims.
func (s *Store) ListWorkspaceOperationsForTurn(ctx context.Context, groupID, turnID string, options ListOptions) (Page[WorkspaceOperation], error) {
	limit, err := listLimit(options)
	if err != nil {
		return Page[WorkspaceOperation]{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page[WorkspaceOperation]{}, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM turns WHERE group_id=? AND id=?", groupID, turnID).Scan(&exists); err != nil {
		return Page[WorkspaceOperation]{}, storeError("读取文件操作审计失败", err)
	}
	query := "SELECT " + workspaceOperationColumns + workspaceOperationFrom + " WHERE o.group_id=? AND o.turn_id=?"
	args := []any{groupID, turnID}
	if c := options.Before; c != nil {
		query += " AND (o.created_at,o.id)<(?,?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	rows, err := tx.QueryContext(ctx, query+" ORDER BY o.created_at DESC,o.id DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return Page[WorkspaceOperation]{}, err
	}
	defer rows.Close()
	page := Page[WorkspaceOperation]{Items: make([]WorkspaceOperation, 0, limit+1)}
	for rows.Next() {
		o, err := scanWorkspaceOperation(rows)
		if err != nil {
			return Page[WorkspaceOperation]{}, err
		}
		page.Items = append(page.Items, o)
	}
	if err := rows.Err(); err != nil {
		return Page[WorkspaceOperation]{}, err
	}
	if err := rows.Close(); err != nil {
		return Page[WorkspaceOperation]{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if err := tx.Commit(); err != nil {
		return Page[WorkspaceOperation]{}, err
	}
	return page, nil
}
