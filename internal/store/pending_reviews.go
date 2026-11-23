package store

import (
	"context"
	"database/sql"
	"time"

	"agentworkspace/internal/domain"
)

// PendingReview projects only the source and immutable evidence needed for Review.
type PendingReview struct {
	Group      PendingReviewGroup `json:"group"`
	Task       PendingReviewTask  `json:"task"`
	TopicID    string             `json:"topic_id,omitempty"`
	Submission domain.Submission  `json:"submission"`
}

type PendingReviewGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type PendingReviewTask struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Acceptance string `json:"acceptance"`
}

func (s *Store) ListPendingReviews(ctx context.Context, actorID string, options ListOptions) (Page[PendingReview], error) {
	if actorID != "user" {
		return Page[PendingReview]{}, ErrMemberToolDenied
	}
	const action = "读取待验收事项失败"
	limit, err := listLimit(options)
	if err != nil {
		return Page[PendingReview]{}, taskError(action, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page[PendingReview]{}, taskError(action, err)
	}
	defer tx.Rollback()

	// For the bound local user, Review authorizes any existing group. The join
	// enforces that boundary here, in the same snapshot as the evidence.
	// Prefer the exact Submission notice. Legacy submissions can use a task's
	// immutable mandates only when they agree on one source topic.
	query := `SELECT g.id,g.name,t.id,t.title,t.acceptance,
		coalesce((SELECT msg.topic_id FROM chat_submission_notices n
			JOIN messages msg ON msg.id=n.message_id AND msg.group_id=s.group_id
			WHERE n.submission_id=s.id),
			(SELECT CASE WHEN min(m.topic_id)=max(m.topic_id) THEN min(m.topic_id) END
			 FROM mandates m WHERE m.group_id=t.group_id AND m.task_id=t.id),''),
		s.id,s.task_id,s.group_id,s.task_revision,s.attempt_id,s.summary,s.submitted_by,s.created_at
		FROM tasks t JOIN groups g ON g.id=t.group_id
		JOIN task_submissions s ON (s.id,s.task_id,s.group_id,s.task_revision,s.attempt_id)=
			(t.current_submission_id,t.id,t.group_id,t.revision,t.active_attempt_id)
		JOIN task_attempts a ON (a.id,a.task_id,a.group_id,a.task_revision,a.actor_id)=
			(s.attempt_id,s.task_id,s.group_id,s.task_revision,s.submitted_by)
		WHERE t.status=? AND NOT EXISTS(SELECT 1 FROM task_review_decisions r WHERE r.submission_id=s.id)`
	args := []any{domain.TaskStatusSubmitted}
	if c := options.Before; c != nil {
		query += " AND (s.created_at,s.id)<(?,?)"
		args = append(args, c.CreatedAt.UTC().Format(timestampLayout), c.ID)
	}
	query += " ORDER BY s.created_at DESC,s.id DESC LIMIT ?"
	rows, err := tx.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return Page[PendingReview]{}, taskError(action, err)
	}
	defer rows.Close()
	page := Page[PendingReview]{Items: make([]PendingReview, 0, limit+1)}
	for rows.Next() {
		var item PendingReview
		var stamp string
		sub := &item.Submission
		if err := rows.Scan(&item.Group.ID, &item.Group.Name, &item.Task.ID, &item.Task.Title, &item.Task.Acceptance, &item.TopicID,
			&sub.ID, &sub.TaskID, &sub.GroupID, &sub.TaskRevision, &sub.AttemptID, &sub.Summary, &sub.SubmittedBy, &stamp); err != nil {
			return Page[PendingReview]{}, taskError(action, err)
		}
		sub.CreatedAt, err = time.Parse(timestampLayout, stamp)
		if err != nil {
			return Page[PendingReview]{}, taskError(action, err)
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return Page[PendingReview]{}, taskError(action, err)
	}
	if err := rows.Close(); err != nil {
		return Page[PendingReview]{}, taskError(action, err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1].Submission
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	ids := make([]string, len(page.Items))
	for i, item := range page.Items {
		ids[i] = item.Submission.ID
	}
	// These globally unique Submission IDs were authorized above in this tx.
	refs, err := submissionArtifactRefsInTx(ctx, tx, ids, nil)
	if err != nil {
		return Page[PendingReview]{}, taskError(action, err)
	}
	for i := range page.Items {
		sub := &page.Items[i].Submission
		sub.ArtifactVersions = refs[sub.ID]
	}
	if err := tx.Commit(); err != nil {
		return Page[PendingReview]{}, taskError(action, err)
	}
	return page, nil
}
