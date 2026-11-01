package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"agentworkspace/internal/domain"
)

// submission already contains the domain's canonical concrete reference set.
func insertTaskArtifactRefs(ctx context.Context, tx *sql.Tx, submission domain.Submission) error {
	for _, ref := range submission.ArtifactVersions {
		// Applied, current head, and SourceTaskID do not constrain historical evidence.
		if _, err := artifactVersionInTx(ctx, tx, submission.GroupID, ref.ArtifactID, ref.VersionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_submission_artifacts
			(submission_id,task_id,group_id,task_revision,artifact_id,version_id) VALUES(?,?,?,?,?,?)`,
			submission.ID, submission.TaskID, submission.GroupID, submission.TaskRevision, ref.ArtifactID, ref.VersionID); err != nil {
			return err
		}
	}
	return nil
}

// Load only returned parents, after their rows are closed. Empty sets remain [].
func taskArtifactRefsInTx(ctx context.Context, tx *sql.Tx, groupID, taskID string, submissionIDs []string) (map[string][]domain.ArtifactVersionRef, error) {
	if len(submissionIDs) > 200 {
		return nil, fmt.Errorf("%w：单次最多读取 200 份提交的成果引用", ErrInvalid)
	}
	refs := make(map[string][]domain.ArtifactVersionRef, len(submissionIDs))
	if len(submissionIDs) == 0 {
		return refs, nil
	}
	placeholders := make([]string, len(submissionIDs))
	args := []any{groupID, taskID}
	for i, id := range submissionIDs {
		refs[id] = []domain.ArtifactVersionRef{}
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `SELECT submission_id,artifact_id,version_id FROM task_submission_artifacts
		WHERE group_id=? AND task_id=? AND submission_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY submission_id,artifact_id,version_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ref domain.ArtifactVersionRef
		if err := rows.Scan(&id, &ref.ArtifactID, &ref.VersionID); err != nil {
			return nil, err
		}
		refs[id] = append(refs[id], ref)
	}
	return refs, rows.Err()
}
