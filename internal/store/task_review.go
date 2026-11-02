package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"agentworkspace/internal/domain"
)

type ReviewTaskInput struct {
	GroupID, TaskID, SubmissionID, Reason, IdempotencyKey string
	ExpectedRevision                                      int64
	Outcome                                               domain.ReviewOutcome
	ActorID                                               string `json:"-"`
}

const taskReviewColumns = "id,submission_id,task_id,group_id,task_revision,outcome,reviewed_by,reason,created_at"

func (s *Store) ReviewTask(ctx context.Context, input ReviewTaskInput) (domain.ReviewDecision, error) {
	if input.ActorID != "user" {
		return domain.ReviewDecision{}, ErrMemberToolDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ReviewDecision{}, taskError("评审任务失败", err)
	}
	defer tx.Rollback()
	decision, err := reviewTaskInTx(ctx, tx, input)
	if err != nil {
		return domain.ReviewDecision{}, taskError("评审任务失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ReviewDecision{}, taskError("保存任务评审失败", err)
	}
	return decision, nil
}

func reviewTaskInTx(ctx context.Context, tx *sql.Tx, input ReviewTaskInput) (domain.ReviewDecision, error) {
	if input.ActorID != "user" {
		return domain.ReviewDecision{}, ErrMemberToolDenied
	}
	// Existing-group authority precedes receipts, including historical replays.
	if err := requireMessageActor(ctx, tx, input.GroupID, input.ActorID); err != nil {
		return domain.ReviewDecision{}, err
	}
	scope := mutationScope{operation: "review_task", actorID: "user", groupID: input.GroupID, key: input.IdempotencyKey}
	payload := mutationPayload(input.GroupID, input.TaskID, input.SubmissionID, strconv.FormatInt(input.ExpectedRevision, 10), string(input.Outcome), input.Reason)
	id, replay, err := reserveMutation(ctx, tx, scope, payload)
	if err != nil {
		return domain.ReviewDecision{}, err
	}
	if replay {
		return taskReviewInTx(ctx, tx, input.GroupID, input.TaskID, id)
	}
	if input.ExpectedRevision < 1 || (input.Outcome != domain.ReviewOutcomeAccepted && input.Outcome != domain.ReviewOutcomeRejected) {
		return domain.ReviewDecision{}, fmt.Errorf("%w：预期任务版本或评审结论无效", domain.ErrTaskInvalid)
	}
	task, err := taskInTx(ctx, tx, input.GroupID, input.TaskID)
	if err != nil {
		return domain.ReviewDecision{}, err
	}
	submission, err := taskSubmissionInTx(ctx, tx, input.GroupID, input.TaskID, input.SubmissionID)
	if err != nil {
		return domain.ReviewDecision{}, err
	}
	if task.Status != domain.TaskStatusSubmitted || task.Revision != input.ExpectedRevision || task.Revision != submission.TaskRevision ||
		task.CurrentSubmissionID != submission.ID || task.ActiveAttemptID != submission.AttemptID {
		return domain.ReviewDecision{}, fmt.Errorf("%w：任务状态、版本或当前提交已改变", ErrConflict)
	}
	next, decision, err := task.Review(submission, domain.ReviewDecision{
		ID: id, SubmissionID: submission.ID, TaskID: submission.TaskID, GroupID: submission.GroupID,
		TaskRevision: submission.TaskRevision, Outcome: input.Outcome, ReviewedBy: "user", Reason: input.Reason,
		ArtifactVersions: submission.ArtifactVersions, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return domain.ReviewDecision{}, err
	}
	// Accepted-state guards require this immutable decision before the task CAS.
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_review_decisions(id,submission_id,task_id,group_id,task_revision,outcome,reviewed_by,reason,created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, decision.ID, decision.SubmissionID, decision.TaskID, decision.GroupID, decision.TaskRevision,
		decision.Outcome, decision.ReviewedBy, decision.Reason, decision.CreatedAt.Format(timestampLayout)); err != nil {
		return domain.ReviewDecision{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET status=?,revision=?,active_attempt_id=nullif(?,''),current_submission_id=nullif(?,'')
		WHERE id=? AND group_id=? AND status=? AND revision=?
		AND active_attempt_id IS nullif(?,'') AND current_submission_id IS nullif(?,'')`,
		next.Status, next.Revision, next.ActiveAttemptID, next.CurrentSubmissionID,
		task.ID, task.GroupID, task.Status, task.Revision, task.ActiveAttemptID, task.CurrentSubmissionID)
	if err != nil {
		return domain.ReviewDecision{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.ReviewDecision{}, err
	}
	if changed != 1 {
		return domain.ReviewDecision{}, fmt.Errorf("%w：任务当前状态已改变", ErrConflict)
	}
	return decision, nil
}

// GetTaskReview is a trusted historical read. Live tools must instead authorize
// and use taskReviewInTx in the same transaction.
func (s *Store) GetTaskReview(ctx context.Context, groupID, taskID, decisionID string) (domain.ReviewDecision, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.ReviewDecision{}, taskError("读取任务评审失败", err)
	}
	defer tx.Rollback()
	decision, err := taskReviewInTx(ctx, tx, groupID, taskID, decisionID)
	if err != nil {
		return domain.ReviewDecision{}, taskError("读取任务评审失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ReviewDecision{}, taskError("读取任务评审失败", err)
	}
	return decision, nil
}

func taskReviewInTx(ctx context.Context, tx *sql.Tx, groupID, taskID, decisionID string) (domain.ReviewDecision, error) {
	decision, err := scanTaskReview(tx.QueryRowContext(ctx, "SELECT "+taskReviewColumns+" FROM task_review_decisions WHERE group_id=? AND task_id=? AND id=?", groupID, taskID, decisionID))
	if err != nil {
		return domain.ReviewDecision{}, err
	}
	refs, err := taskArtifactRefsInTx(ctx, tx, groupID, taskID, []string{decision.SubmissionID})
	if err == nil {
		decision.ArtifactVersions = refs[decision.SubmissionID]
	}
	return decision, err
}

func scanTaskReview(row scanner) (decision domain.ReviewDecision, err error) {
	var stamp string
	decision.ArtifactVersions = []domain.ArtifactVersionRef{}
	if err = row.Scan(&decision.ID, &decision.SubmissionID, &decision.TaskID, &decision.GroupID, &decision.TaskRevision,
		&decision.Outcome, &decision.ReviewedBy, &decision.Reason, &stamp); err == nil {
		decision.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}

// ListTaskReviews shares the trusted historical-read boundary of GetTaskReview.
func (s *Store) ListTaskReviews(ctx context.Context, groupID, taskID string, options ListOptions) (Page[domain.ReviewDecision], error) {
	limit, err := listLimit(options)
	if err != nil {
		return Page[domain.ReviewDecision]{}, taskError("列出任务评审失败", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page[domain.ReviewDecision]{}, taskError("列出任务评审失败", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE group_id=? AND id=?", groupID, taskID).Scan(&exists); err != nil {
		return Page[domain.ReviewDecision]{}, taskError("列出任务评审失败", err)
	}
	query := "SELECT " + taskReviewColumns + " FROM task_review_decisions WHERE group_id=? AND task_id=?"
	args := []any{groupID, taskID}
	if c := options.Before; c != nil {
		query += " AND (created_at,id)<(?,?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	query += " ORDER BY created_at DESC,id DESC LIMIT ?"
	rows, err := tx.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return Page[domain.ReviewDecision]{}, taskError("列出任务评审失败", err)
	}
	defer rows.Close()
	page := Page[domain.ReviewDecision]{Items: make([]domain.ReviewDecision, 0, limit+1)}
	for rows.Next() {
		decision, err := scanTaskReview(rows)
		if err != nil {
			return Page[domain.ReviewDecision]{}, taskError("列出任务评审失败", err)
		}
		page.Items = append(page.Items, decision)
	}
	if err := rows.Err(); err != nil {
		return Page[domain.ReviewDecision]{}, taskError("列出任务评审失败", err)
	}
	if err := rows.Close(); err != nil {
		return Page[domain.ReviewDecision]{}, taskError("列出任务评审失败", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	ids := make([]string, len(page.Items))
	for i, decision := range page.Items {
		ids[i] = decision.SubmissionID
	}
	refs, err := taskArtifactRefsInTx(ctx, tx, groupID, taskID, ids)
	if err != nil {
		return Page[domain.ReviewDecision]{}, taskError("读取评审成果引用失败", err)
	}
	for i := range page.Items {
		page.Items[i].ArtifactVersions = refs[page.Items[i].SubmissionID]
	}
	if err := tx.Commit(); err != nil {
		return Page[domain.ReviewDecision]{}, taskError("列出任务评审失败", err)
	}
	return page, nil
}
