package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"agentworkspace/internal/domain"
)

type CreateTaskInput struct {
	GroupID, Title, Description, Acceptance, AssigneeID, IdempotencyKey string
	DependencyIDs                                                       []string
	ActorID                                                             string `json:"-"` // Bound by trusted platform/session code.
}

const taskColumns = `id, group_id, title, description, acceptance, coalesce(assignee_id,''),
 status, revision, coalesce(active_attempt_id,''), coalesce(current_submission_id,''), created_at`

func (s *Store) CreateTask(ctx context.Context, input CreateTaskInput) (domain.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Task{}, taskError("创建任务失败", err)
	}
	defer tx.Rollback()
	task, err := createTaskInTx(ctx, tx, input)
	if err != nil {
		return domain.Task{}, taskError("创建任务失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Task{}, taskError("保存任务失败", err)
	}
	return task, nil
}

func createTaskInTx(ctx context.Context, tx *sql.Tx, input CreateTaskInput) (domain.Task, error) {
	// Current access precedes both replay and changed-payload conflict disclosure.
	if err := requireMessageActor(ctx, tx, input.GroupID, input.ActorID); err != nil {
		return domain.Task{}, err
	}
	deps := slices.Clone(input.DependencyIDs)
	slices.Sort(deps)
	deps = slices.Compact(deps)
	scope := mutationScope{operation: "create_task", actorID: input.ActorID, groupID: input.GroupID, key: input.IdempotencyKey}
	payload := mutationPayload(append([]string{input.GroupID, input.Title, input.Description, input.Acceptance, input.AssigneeID}, deps...)...)
	id, replay, err := reserveMutation(ctx, tx, scope, payload)
	if err != nil {
		return domain.Task{}, err
	}
	if replay {
		return taskInTx(ctx, tx, input.GroupID, id)
	}
	task, err := domain.NewTask(domain.Task{
		ID: id, GroupID: input.GroupID, Title: input.Title, Description: input.Description,
		Acceptance: input.Acceptance, AssigneeID: input.AssigneeID, DependencyIDs: deps,
		Revision: 1, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return domain.Task{}, err
	}
	if task.AssigneeID != "" {
		var active int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM members WHERE group_id=? AND id=? AND status='active'", task.GroupID, task.AssigneeID).Scan(&active); err != nil {
			return domain.Task{}, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks(id,group_id,title,description,acceptance,assignee_id,status,revision,created_at)
		 VALUES(?,?,?,?,?,nullif(?,''),?,?,?)`, task.ID, task.GroupID, task.Title, task.Description,
		task.Acceptance, task.AssigneeID, task.Status, task.Revision, task.CreatedAt.Format(timestampLayout)); err != nil {
		return domain.Task{}, err
	}
	for _, dependency := range task.DependencyIDs {
		var saved string
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO task_dependencies(group_id,task_id,dependency_id)
			 SELECT ?,?,id FROM tasks WHERE group_id=? AND id=? RETURNING dependency_id`,
			task.GroupID, task.ID, task.GroupID, dependency).Scan(&saved); err != nil {
			return domain.Task{}, err
		}
	}
	return task, nil
}

func (s *Store) GetTask(ctx context.Context, groupID, taskID string) (domain.Task, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.Task{}, taskError("读取任务失败", err)
	}
	defer tx.Rollback()
	task, err := taskInTx(ctx, tx, groupID, taskID)
	if err != nil {
		return domain.Task{}, taskError("读取任务失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Task{}, taskError("读取任务失败", err)
	}
	return task, nil
}

func taskInTx(ctx context.Context, tx *sql.Tx, groupID, taskID string) (domain.Task, error) {
	task, err := scanTask(tx.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE group_id=? AND id=?", groupID, taskID))
	if err == nil {
		err = taskDependencies(ctx, tx, &task)
	}
	return task, err
}

func scanTask(row scanner) (task domain.Task, err error) {
	var stamp string
	task.DependencyIDs = []string{}
	if err = row.Scan(&task.ID, &task.GroupID, &task.Title, &task.Description, &task.Acceptance, &task.AssigneeID,
		&task.Status, &task.Revision, &task.ActiveAttemptID, &task.CurrentSubmissionID, &stamp); err == nil {
		task.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}

func taskDependencies(ctx context.Context, tx *sql.Tx, task *domain.Task) error {
	rows, err := tx.QueryContext(ctx, "SELECT dependency_id FROM task_dependencies WHERE group_id=? AND task_id=? ORDER BY dependency_id", task.GroupID, task.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		task.DependencyIDs = append(task.DependencyIDs, id)
	}
	return rows.Err()
}

func (s *Store) ListTasks(ctx context.Context, groupID string, options ListOptions) (Page[domain.Task], error) {
	limit, err := listLimit(options)
	if err != nil {
		return Page[domain.Task]{}, taskError("列出任务失败", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page[domain.Task]{}, taskError("列出任务失败", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM groups WHERE id=?", groupID).Scan(&exists); err != nil {
		return Page[domain.Task]{}, taskError("列出任务失败", err)
	}
	query := "SELECT " + taskColumns + " FROM tasks WHERE group_id=?"
	args := []any{groupID}
	if c := options.Before; c != nil {
		query += " AND (created_at,id)<(?,?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	query += " ORDER BY created_at DESC,id DESC LIMIT ?"
	rows, err := tx.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return Page[domain.Task]{}, taskError("列出任务失败", err)
	}
	defer rows.Close()
	page := Page[domain.Task]{Items: make([]domain.Task, 0, limit+1)}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return Page[domain.Task]{}, taskError("列出任务失败", err)
		}
		page.Items = append(page.Items, task)
	}
	if err := rows.Err(); err != nil {
		return Page[domain.Task]{}, taskError("列出任务失败", err)
	}
	if err := rows.Close(); err != nil {
		return Page[domain.Task]{}, taskError("列出任务失败", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	for i := range page.Items {
		if err := taskDependencies(ctx, tx, &page.Items[i]); err != nil {
			return Page[domain.Task]{}, taskError("读取任务依赖失败", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Page[domain.Task]{}, taskError("列出任务失败", err)
	}
	return page, nil
}

func taskError(action string, err error) error {
	switch {
	case errors.Is(err, domain.ErrTaskInvalid):
		err = fmt.Errorf("%w：%w", ErrInvalid, err)
	case errors.Is(err, domain.ErrTaskConflict):
		err = fmt.Errorf("%w：%w", ErrConflict, err)
	}
	return storeError(action, err)
}
