package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"agentworkspace/internal/domain"
)

type SubmitTaskInput struct {
	GroupID, TaskID, AttemptID, Summary, IdempotencyKey string
	ExpectedRevision                                    int64
	ArtifactVersions                                    []domain.ArtifactVersionRef
	ActorID                                             string           `json:"-"`
	MemberScope                                         *MemberToolScope `json:"-"`
}

const taskSubmissionColumns = "id,task_id,group_id,task_revision,attempt_id,summary,submitted_by,created_at"

func (s *Store) SubmitTask(ctx context.Context, input SubmitTaskInput) (domain.Submission, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Submission{}, taskError("提交任务失败", err)
	}
	defer tx.Rollback()
	submission, err := submitTaskInTx(ctx, tx, input)
	if err != nil {
		if errors.Is(err, ErrMemberToolDenied) {
			return domain.Submission{}, ErrMemberToolDenied
		}
		return domain.Submission{}, taskError("提交任务失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Submission{}, taskError("保存任务提交失败", err)
	}
	return submission, nil
}

func submitTaskInTx(ctx context.Context, tx *sql.Tx, input SubmitTaskInput) (domain.Submission, error) {
	// Current authority precedes receipts and all task/attempt reads.
	turnID, err := taskActorInTx(ctx, tx, input.GroupID, input.ActorID, input.MemberScope)
	if err != nil {
		return domain.Submission{}, err
	}
	scope := mutationScope{operation: "submit_task", actorID: input.ActorID, groupID: input.GroupID, key: input.IdempotencyKey}
	id, replay, err := reserveMutation(ctx, tx, scope, taskSubmissionPayload(input))
	if err != nil {
		return domain.Submission{}, err
	}
	if replay {
		return taskSubmissionInTx(ctx, tx, input.GroupID, input.TaskID, id)
	}
	if input.ExpectedRevision < 1 {
		return domain.Submission{}, fmt.Errorf("%w：预期任务版本必须大于零", domain.ErrTaskInvalid)
	}
	task, err := taskInTx(ctx, tx, input.GroupID, input.TaskID)
	if err != nil {
		return domain.Submission{}, err
	}
	if task.Status != domain.TaskStatusOpen || task.Revision != input.ExpectedRevision || task.ActiveAttemptID == "" || task.ActiveAttemptID != input.AttemptID {
		return domain.Submission{}, fmt.Errorf("%w：任务状态、版本或当前尝试已改变", ErrConflict)
	}
	attempt, actorID, err := taskAttemptInTx(ctx, tx, input.GroupID, input.TaskID, input.AttemptID)
	if err != nil {
		return domain.Submission{}, err
	}
	if actorID != input.ActorID || (input.ActorID != "user" && attempt.TurnID != turnID) {
		return domain.Submission{}, ErrMemberToolDenied
	}
	next, submission, err := task.Submit(domain.Submission{
		ID: id, TaskID: task.ID, GroupID: task.GroupID, TaskRevision: task.Revision,
		AttemptID: attempt.ID, Summary: input.Summary, SubmittedBy: actorID,
		ArtifactVersions: input.ArtifactVersions, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return domain.Submission{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_submissions(id,task_id,group_id,task_revision,attempt_id,summary,submitted_by,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, submission.ID, submission.TaskID, submission.GroupID, submission.TaskRevision,
		submission.AttemptID, submission.Summary, submission.SubmittedBy, submission.CreatedAt.Format(timestampLayout)); err != nil {
		return domain.Submission{}, err
	}
	if err := insertTaskArtifactRefs(ctx, tx, submission); err != nil {
		return domain.Submission{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET status=?,current_submission_id=?
		WHERE id=? AND group_id=? AND status=? AND revision=?
		AND active_attempt_id IS nullif(?,'') AND current_submission_id IS nullif(?,'')`,
		next.Status, next.CurrentSubmissionID, task.ID, task.GroupID, task.Status, task.Revision, task.ActiveAttemptID, task.CurrentSubmissionID)
	if err != nil {
		return domain.Submission{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.Submission{}, err
	}
	if changed != 1 {
		return domain.Submission{}, fmt.Errorf("%w：任务当前状态已改变", ErrConflict)
	}
	return submission, nil
}

// Canonicalize refs before validation so keyed conflicts retain their original semantics.
func taskSubmissionPayload(input SubmitTaskInput) []byte {
	refs := slices.Clone(input.ArtifactVersions)
	slices.SortFunc(refs, func(a, b domain.ArtifactVersionRef) int {
		if order := strings.Compare(a.ArtifactID, b.ArtifactID); order != 0 {
			return order
		}
		return strings.Compare(a.VersionID, b.VersionID)
	})
	refs = slices.Compact(refs)
	fields := []string{input.GroupID, input.TaskID, input.AttemptID, input.Summary, strconv.FormatInt(input.ExpectedRevision, 10)}
	for _, ref := range refs {
		fields = append(fields, ref.ArtifactID, ref.VersionID)
	}
	return mutationPayload(fields...)
}

// GetTaskSubmission is a trusted historical read. Live tools must instead authorize
// and use taskSubmissionInTx in the same transaction.
func (s *Store) GetTaskSubmission(ctx context.Context, groupID, taskID, submissionID string) (domain.Submission, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.Submission{}, taskError("读取任务提交失败", err)
	}
	defer tx.Rollback()
	submission, err := taskSubmissionInTx(ctx, tx, groupID, taskID, submissionID)
	if err != nil {
		return domain.Submission{}, taskError("读取任务提交失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Submission{}, taskError("读取任务提交失败", err)
	}
	return submission, nil
}

func taskSubmissionInTx(ctx context.Context, tx *sql.Tx, groupID, taskID, submissionID string) (domain.Submission, error) {
	submission, err := scanTaskSubmission(tx.QueryRowContext(ctx, "SELECT "+taskSubmissionColumns+" FROM task_submissions WHERE group_id=? AND task_id=? AND id=?", groupID, taskID, submissionID))
	if err != nil {
		return domain.Submission{}, err
	}
	refs, err := taskArtifactRefsInTx(ctx, tx, groupID, taskID, []string{submission.ID})
	if err == nil {
		submission.ArtifactVersions = refs[submission.ID]
	}
	return submission, err
}

func scanTaskSubmission(row scanner) (submission domain.Submission, err error) {
	var stamp string
	submission.ArtifactVersions = []domain.ArtifactVersionRef{}
	if err = row.Scan(&submission.ID, &submission.TaskID, &submission.GroupID, &submission.TaskRevision,
		&submission.AttemptID, &submission.Summary, &submission.SubmittedBy, &stamp); err == nil {
		submission.CreatedAt, err = time.Parse(timestampLayout, stamp)
	}
	return
}

// ListTaskSubmissions shares the trusted historical-read boundary of GetTaskSubmission.
func (s *Store) ListTaskSubmissions(ctx context.Context, groupID, taskID string, options ListOptions) (Page[domain.Submission], error) {
	limit, err := listLimit(options)
	if err != nil {
		return Page[domain.Submission]{}, taskError("列出任务提交失败", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page[domain.Submission]{}, taskError("列出任务提交失败", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE group_id=? AND id=?", groupID, taskID).Scan(&exists); err != nil {
		return Page[domain.Submission]{}, taskError("列出任务提交失败", err)
	}
	query := "SELECT " + taskSubmissionColumns + " FROM task_submissions WHERE group_id=? AND task_id=?"
	args := []any{groupID, taskID}
	if c := options.Before; c != nil {
		query += " AND (created_at,id)<(?,?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	query += " ORDER BY created_at DESC,id DESC LIMIT ?"
	rows, err := tx.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return Page[domain.Submission]{}, taskError("列出任务提交失败", err)
	}
	defer rows.Close()
	page := Page[domain.Submission]{Items: make([]domain.Submission, 0, limit+1)}
	for rows.Next() {
		submission, err := scanTaskSubmission(rows)
		if err != nil {
			return Page[domain.Submission]{}, taskError("列出任务提交失败", err)
		}
		page.Items = append(page.Items, submission)
	}
	if err := rows.Err(); err != nil {
		return Page[domain.Submission]{}, taskError("列出任务提交失败", err)
	}
	if err := rows.Close(); err != nil {
		return Page[domain.Submission]{}, taskError("列出任务提交失败", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	ids := make([]string, len(page.Items))
	for i, submission := range page.Items {
		ids[i] = submission.ID
	}
	refs, err := taskArtifactRefsInTx(ctx, tx, groupID, taskID, ids)
	if err != nil {
		return Page[domain.Submission]{}, taskError("读取提交成果引用失败", err)
	}
	for i := range page.Items {
		page.Items[i].ArtifactVersions = refs[page.Items[i].ID]
	}
	if err := tx.Commit(); err != nil {
		return Page[domain.Submission]{}, taskError("列出任务提交失败", err)
	}
	return page, nil
}
