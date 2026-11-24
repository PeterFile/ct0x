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

func taskArtifactRefsInTx(ctx context.Context, tx *sql.Tx, groupID, taskID string, submissionIDs []string) (map[string][]domain.ArtifactVersionRef, error) {
	return submissionArtifactRefsInTx(ctx, tx, submissionIDs, &[2]string{groupID, taskID})
}

// Load only returned parents, after their rows are closed. Empty sets remain [].
// An unscoped batch requires authoritative Submission IDs from this same tx.
func submissionArtifactRefsInTx(ctx context.Context, tx *sql.Tx, submissionIDs []string, scope *[2]string) (map[string][]domain.ArtifactVersionRef, error) {
	if len(submissionIDs) > 200 {
		return nil, fmt.Errorf("%w：单次最多读取 200 份提交的成果引用", ErrInvalid)
	}
	refs := make(map[string][]domain.ArtifactVersionRef, len(submissionIDs))
	if len(submissionIDs) == 0 {
		return refs, nil
	}
	placeholders := make([]string, len(submissionIDs))
	var args []any
	where := ""
	if scope != nil {
		where = "group_id=? AND task_id=? AND "
		args = append(args, scope[0], scope[1])
	}
	for i, id := range submissionIDs {
		refs[id] = []domain.ArtifactVersionRef{}
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `SELECT submission_id,artifact_id,version_id FROM task_submission_artifacts
		WHERE `+where+`submission_id IN (`+strings.Join(placeholders, ",")+`)
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
