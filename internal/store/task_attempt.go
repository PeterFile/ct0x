package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"agentworkspace/internal/domain"
)

type ActivateTaskAttemptInput struct {
	GroupID, TaskID, ExpectedActiveAttemptID, IdempotencyKey string
	ExpectedRevision                                         int64
	ActorID                                                  string           `json:"-"`
	MemberScope                                              *MemberToolScope `json:"-"`
}

const taskAttemptColumns = "id,task_id,group_id,task_revision,coalesce(turn_id,''),created_at,actor_id"

func (s *Store) ActivateTaskAttempt(ctx context.Context, input ActivateTaskAttemptInput) (domain.TaskAttempt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.TaskAttempt{}, taskError("激活任务尝试失败", err)
	}
	defer tx.Rollback()
	attempt, err := activateTaskAttemptInTx(ctx, tx, input, nil)
	if err != nil {
		if errors.Is(err, ErrMemberToolDenied) {
			return domain.TaskAttempt{}, ErrMemberToolDenied
		}
		return domain.TaskAttempt{}, taskError("激活任务尝试失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.TaskAttempt{}, taskError("保存任务尝试失败", err)
	}
	return attempt, nil
}

// taskActorInTx must perform the operation's first SQL, even on receipt replay.
// Agent identity is authorized by the existing run fence, never by a preflight.
func taskActorInTx(ctx context.Context, tx *sql.Tx, groupID, actorID string, scope *MemberToolScope) (string, error) {
	if actorID == "user" {
		if scope != nil {
			return "", ErrMemberToolDenied
		}
		return "", requireMessageActor(ctx, tx, groupID, actorID)
	}
	if scope == nil {
		return "", ErrMemberToolDenied
	}
	bound := *scope
	if actorID != bound.MemberID || groupID != bound.Claim.GroupID {
		return "", ErrMemberToolDenied
	}
	if err := memberToolScopeInTx(ctx, tx, bound); err != nil {
		return "", err
	}
	return bound.Claim.TurnID, nil
}

func activateTaskAttemptInTx(ctx context.Context, tx *sql.Tx, input ActivateTaskAttemptInput, authority *assignmentActivationAuthority) (domain.TaskAttempt, error) {
	turnID, err := taskActorInTx(ctx, tx, input.GroupID, input.ActorID, input.MemberScope)
	if err != nil {
		return domain.TaskAttempt{}, err
	}
	scope := mutationScope{operation: "activate_task_attempt", actorID: input.ActorID, groupID: input.GroupID, key: input.IdempotencyKey}
	// The original fences are semantic input; a new live scope cannot rebind a replay.
	payload := mutationPayload(input.GroupID, input.TaskID, strconv.FormatInt(input.ExpectedRevision, 10), input.ExpectedActiveAttemptID)
	id, replay, err := reserveMutation(ctx, tx, scope, payload)
	if err != nil {
		return domain.TaskAttempt{}, err
	}
	if replay {
		attempt, _, err := taskAttemptInTx(ctx, tx, input.GroupID, input.TaskID, id)
		return attempt, err
	}
	if err := coordinationActivateOwnershipInTx(ctx, tx, input, authority); err != nil {
		return domain.TaskAttempt{}, err
	}
	if input.ExpectedRevision < 1 {
		return domain.TaskAttempt{}, fmt.Errorf("%w：预期任务版本必须大于零", domain.ErrTaskInvalid)
	}
	task, err := taskInTx(ctx, tx, input.GroupID, input.TaskID)
	if err != nil {
		return domain.TaskAttempt{}, err
	}
	if input.ActorID != "user" && task.AssigneeID != "" && task.AssigneeID != input.ActorID {
		return domain.TaskAttempt{}, ErrMemberToolDenied
	}
	if task.Status != domain.TaskStatusOpen || task.Revision != input.ExpectedRevision || task.ActiveAttemptID != input.ExpectedActiveAttemptID {
		return domain.TaskAttempt{}, fmt.Errorf("%w：任务状态、版本或当前尝试已改变", ErrConflict)
	}
	var blocked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM task_dependencies d JOIN tasks parent ON parent.id=d.dependency_id AND parent.group_id=d.group_id
		WHERE d.group_id=? AND d.task_id=? AND parent.status<>'accepted')`, task.GroupID, task.ID).Scan(&blocked); err != nil {
		return domain.TaskAttempt{}, err
	}
	if blocked {
		return domain.TaskAttempt{}, fmt.Errorf("%w：任务依赖尚未全部接纳", ErrConflict)
	}
	next, attempt, err := task.ActivateAttempt(domain.TaskAttempt{
		ID: id, TaskID: task.ID, GroupID: task.GroupID, TaskRevision: task.Revision,
		TurnID: turnID, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return domain.TaskAttempt{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_attempts(id,task_id,group_id,task_revision,turn_id,actor_id,created_at)
		VALUES(?,?,?,?,nullif(?,''),?,?)`, attempt.ID, attempt.TaskID, attempt.GroupID, attempt.TaskRevision,
		attempt.TurnID, input.ActorID, attempt.CreatedAt.Format(timestampLayout)); err != nil {
		return domain.TaskAttempt{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET active_attempt_id=?
		WHERE id=? AND group_id=? AND status=? AND revision=?
		AND active_attempt_id IS nullif(?,'') AND current_submission_id IS nullif(?,'')`,
		next.ActiveAttemptID, task.ID, task.GroupID, task.Status, task.Revision, task.ActiveAttemptID, task.CurrentSubmissionID)
	if err != nil {
		return domain.TaskAttempt{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.TaskAttempt{}, err
	}
	if changed != 1 {
		return domain.TaskAttempt{}, fmt.Errorf("%w：任务当前状态已改变", ErrConflict)
	}
	if authority == nil {
		if err := coordinationActivateOwnershipInTx(ctx, tx, input, nil); err != nil {
			return domain.TaskAttempt{}, err
		}
		// RowsAffected excludes trigger side effects; read back the actual result.
		current, err := taskInTx(ctx, tx, input.GroupID, input.TaskID)
		if err != nil {
			return domain.TaskAttempt{}, err
		}
		saved, actor, err := taskAttemptInTx(ctx, tx, input.GroupID, input.TaskID, attempt.ID)
		if err != nil {
			return domain.TaskAttempt{}, err
		}
		if current.Status != next.Status || current.Revision != next.Revision || current.ActiveAttemptID != next.ActiveAttemptID ||
			current.CurrentSubmissionID != next.CurrentSubmissionID || saved != attempt || actor != input.ActorID {
			return domain.TaskAttempt{}, ErrConflict
		}
	}
	if input.IdempotencyKey != "" {
		persisted, present, err := lookupMutation(ctx, tx, scope, payload)
		if err != nil {
			return domain.TaskAttempt{}, err
		}
		if !present || persisted != attempt.ID {
			return domain.TaskAttempt{}, ErrConflict
		}
	}
	return attempt, nil
}

// GetTaskAttempt is a trusted historical read. Live tools must instead authorize
// and use taskAttemptInTx in the same transaction.
func (s *Store) GetTaskAttempt(ctx context.Context, groupID, taskID, attemptID string) (domain.TaskAttempt, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.TaskAttempt{}, taskError("读取任务尝试失败", err)
	}
	defer tx.Rollback()
	attempt, _, err := taskAttemptInTx(ctx, tx, groupID, taskID, attemptID)
	if err != nil {
		return domain.TaskAttempt{}, taskError("读取任务尝试失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.TaskAttempt{}, taskError("读取任务尝试失败", err)
	}
	return attempt, nil
}

func taskAttemptInTx(ctx context.Context, tx *sql.Tx, groupID, taskID, attemptID string) (domain.TaskAttempt, string, error) {
	return scanTaskAttempt(tx.QueryRowContext(ctx, "SELECT "+taskAttemptColumns+" FROM task_attempts WHERE group_id=? AND task_id=? AND id=?", groupID, taskID, attemptID))
}

func scanTaskAttempt(row scanner) (attempt domain.TaskAttempt, actorID string, err error) {
	var stamp string
	if err = row.Scan(&attempt.ID, &attempt.TaskID, &attempt.GroupID, &attempt.TaskRevision, &attempt.TurnID, &stamp, &actorID); err == nil {
		attempt.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}

// ListTaskAttempts shares the trusted historical-read boundary of GetTaskAttempt.
func (s *Store) ListTaskAttempts(ctx context.Context, groupID, taskID string, options ListOptions) (Page[domain.TaskAttempt], error) {
	limit, err := listLimit(options)
	if err != nil {
		return Page[domain.TaskAttempt]{}, taskError("列出任务尝试失败", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page[domain.TaskAttempt]{}, taskError("列出任务尝试失败", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE group_id=? AND id=?", groupID, taskID).Scan(&exists); err != nil {
		return Page[domain.TaskAttempt]{}, taskError("列出任务尝试失败", err)
	}
	query := "SELECT " + taskAttemptColumns + " FROM task_attempts WHERE group_id=? AND task_id=?"
	args := []any{groupID, taskID}
	if c := options.Before; c != nil {
		query += " AND (created_at,id)<(?,?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	query += " ORDER BY created_at DESC,id DESC LIMIT ?"
	rows, err := tx.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return Page[domain.TaskAttempt]{}, taskError("列出任务尝试失败", err)
	}
	defer rows.Close()
	page := Page[domain.TaskAttempt]{Items: make([]domain.TaskAttempt, 0, limit+1)}
	for rows.Next() {
		attempt, _, err := scanTaskAttempt(rows)
		if err != nil {
			return Page[domain.TaskAttempt]{}, taskError("列出任务尝试失败", err)
		}
		page.Items = append(page.Items, attempt)
	}
	if err := rows.Err(); err != nil {
		return Page[domain.TaskAttempt]{}, taskError("列出任务尝试失败", err)
	}
	if err := rows.Close(); err != nil {
		return Page[domain.TaskAttempt]{}, taskError("列出任务尝试失败", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if err := tx.Commit(); err != nil {
		return Page[domain.TaskAttempt]{}, taskError("列出任务尝试失败", err)
	}
	return page, nil
}
