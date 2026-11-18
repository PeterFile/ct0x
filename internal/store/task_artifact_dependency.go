package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"agentworkspace/internal/domain"
)

func canonicalTaskArtifactDependencies(input []domain.TaskArtifactDependency) []domain.TaskArtifactDependency {
	pins := slices.Clone(input)
	slices.SortFunc(pins, func(a, b domain.TaskArtifactDependency) int { return strings.Compare(a.DependencyID, b.DependencyID) })
	return pins
}

// Both inputs are sorted by dependency ID.
func validateTaskArtifactDependencies(deps []string, pins []domain.TaskArtifactDependency) error {
	for i, pin := range pins {
		if strings.TrimSpace(pin.DependencyID) == "" || strings.TrimSpace(pin.SubmissionID) == "" ||
			strings.TrimSpace(pin.ArtifactID) == "" || strings.TrimSpace(pin.VersionID) == "" || pin.ReadyWhen != "accepted" {
			return fmt.Errorf("%w：成果依赖必须包含完整的任务、提交、成果和版本标识，就绪条件必须为 accepted", ErrInvalid)
		}
		if i > 0 && pins[i-1].DependencyID == pin.DependencyID {
			return fmt.Errorf("%w：同一上游任务只能固定一个成果版本", ErrInvalid)
		}
		if _, ok := slices.BinarySearch(deps, pin.DependencyID); !ok {
			return fmt.Errorf("%w：成果依赖必须对应显式的任务依赖", ErrInvalid)
		}
	}
	return nil
}

func taskArtifactDependencyPayload(legacy []byte, pins []domain.TaskArtifactDependency) []byte {
	fields := make([]string, 0, len(pins)*5)
	for _, pin := range pins {
		fields = append(fields, pin.DependencyID, pin.SubmissionID, pin.ArtifactID, pin.VersionID, pin.ReadyWhen)
	}
	// Three framed fields cannot collide with the legacy payload (at least five).
	return mutationPayload("task-artifact-dependencies/v1", string(legacy), string(mutationPayload(fields...)))
}

func taskArtifactDependencySchemaInTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	// Historic fixtures may stop before this migration. A missing table after
	// its recorded migration is an error, not evidence of no dependencies.
	var installed bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=?)", taskArtifactDependencySchemaVersion).Scan(&installed)
	return installed, err
}

func insertTaskArtifactDependenciesInTx(ctx context.Context, tx *sql.Tx, task domain.Task, pins []domain.TaskArtifactDependency) error {
	installed, err := taskArtifactDependencySchemaInTx(ctx, tx)
	if err != nil {
		return err
	}
	if !installed {
		return fmt.Errorf("%w：数据库尚不支持固定成果依赖", ErrInvalid)
	}
	for _, pin := range pins {
		var revision int64
		var eligible bool
		// A Submission can cite any same-group ArtifactVersion as evidence, but
		// a dependency must pin an output actually sourced from its prerequisite.
		err := tx.QueryRowContext(ctx, `SELECT s.task_revision, CASE WHEN r.id IS NULL THEN
		 p.status='submitted' AND p.revision=s.task_revision AND p.current_submission_id IS s.id AND p.active_attempt_id IS s.attempt_id
		 ELSE r.outcome='accepted' AND r.reviewed_by='user' AND r.group_id=s.group_id AND r.task_id=s.task_id AND r.task_revision=s.task_revision END
		 FROM task_submissions s JOIN tasks p ON p.id=s.task_id AND p.group_id=s.group_id
		 JOIN task_submission_artifacts ref ON ref.submission_id=s.id AND ref.group_id=s.group_id AND ref.task_id=s.task_id AND ref.task_revision=s.task_revision
		 JOIN artifact_versions v ON v.id=ref.version_id AND v.artifact_id=ref.artifact_id AND v.group_id=ref.group_id AND v.source_task_id=s.task_id
		 LEFT JOIN task_review_decisions r ON r.submission_id=s.id
		 WHERE s.group_id=? AND s.task_id=? AND s.id=? AND ref.artifact_id=? AND ref.version_id=?`,
			task.GroupID, pin.DependencyID, pin.SubmissionID, pin.ArtifactID, pin.VersionID).Scan(&revision, &eligible)
		if err != nil {
			return err
		}
		if !eligible {
			return fmt.Errorf("%w：成果依赖必须引用待评审或已由本地用户接纳的提交", ErrInvalid)
		}
		var saved string
		if err := tx.QueryRowContext(ctx, `INSERT INTO task_artifact_dependencies
		 (group_id,task_id,dependency_id,submission_id,dependency_revision,artifact_id,version_id,ready_when)
		 VALUES(?,?,?,?,?,?,?,?) RETURNING version_id`, task.GroupID, task.ID, pin.DependencyID,
			pin.SubmissionID, revision, pin.ArtifactID, pin.VersionID, pin.ReadyWhen).Scan(&saved); err != nil {
			return err
		}
	}
	return nil
}

// GetTaskArtifactDependencies is a trusted read of explicit immutable pins,
// not a grant to access Artifact contents or execute the Task.
func (s *Store) GetTaskArtifactDependencies(ctx context.Context, groupID, taskID string) ([]domain.TaskArtifactDependency, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, taskError("读取任务成果依赖失败", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE group_id=? AND id=?", groupID, taskID).Scan(&exists); err != nil {
		return nil, taskError("读取任务成果依赖失败", err)
	}
	pins, err := taskArtifactDependenciesInTx(ctx, tx, groupID, taskID)
	if err != nil {
		return nil, taskError("读取任务成果依赖失败", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, taskError("读取任务成果依赖失败", err)
	}
	return pins, nil
}

func taskArtifactDependenciesInTx(ctx context.Context, tx *sql.Tx, groupID, taskID string) ([]domain.TaskArtifactDependency, error) {
	pins := []domain.TaskArtifactDependency{}
	installed, err := taskArtifactDependencySchemaInTx(ctx, tx)
	if err != nil || !installed {
		return pins, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT dependency_id,submission_id,artifact_id,version_id,ready_when
	 FROM task_artifact_dependencies WHERE group_id=? AND task_id=? ORDER BY dependency_id`, groupID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pin domain.TaskArtifactDependency
		if err := rows.Scan(&pin.DependencyID, &pin.SubmissionID, &pin.ArtifactID, &pin.VersionID, &pin.ReadyWhen); err != nil {
			return nil, err
		}
		pins = append(pins, pin)
	}
	return pins, rows.Err()
}

// Check only new starts that passed the existing plain dependency gate. The
// immutable pin/Submission FK fixes the reviewed ArtifactVersion and revision.
func taskArtifactDependenciesBlockedInTx(ctx context.Context, tx *sql.Tx, groupID, taskID string) (bool, error) {
	installed, err := taskArtifactDependencySchemaInTx(ctx, tx)
	if err != nil || !installed {
		return false, err
	}
	var blocked bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_artifact_dependencies pin
	 WHERE pin.group_id=? AND pin.task_id=? AND NOT EXISTS(SELECT 1 FROM task_review_decisions r
	 WHERE r.submission_id=pin.submission_id AND r.group_id=pin.group_id AND r.task_id=pin.dependency_id
	 AND r.task_revision=pin.dependency_revision AND r.outcome='accepted' AND r.reviewed_by='user' AND pin.ready_when='accepted'))`, groupID, taskID).Scan(&blocked)
	return blocked, err
}
