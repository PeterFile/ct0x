package store

import (
	"context"
	"database/sql"
	"errors"

	"agentworkspace/internal/domain"
)

// Only fresh, already-authorized investigation submissions have this
// obligation. Historical replay and local-user Review bypass it.
func taskWorkSubmissionParentInTx(ctx context.Context, tx *sql.Tx, in SubmitTaskInput, investigation taskInvestigationEvidence) (domain.TaskWorkParent, error) {
	var zero domain.TaskWorkParent
	if investigation.TaskID == "" {
		return zero, nil
	}
	supported, err := taskWorkReportsSupportedInTx(ctx, tx)
	if err != nil || !supported {
		return zero, err
	}
	if in.MemberScope == nil {
		return zero, ErrMemberToolDenied
	}
	f, attempt, err := taskWorkOwnerInTx(ctx, tx, *in.MemberScope)
	if err != nil {
		return zero, err
	}
	p := taskWorkParent(f)
	if !investigation.matchesDispatch(f.v.r) || p.GroupID != in.GroupID || p.TaskID != in.TaskID ||
		p.TaskRevision != in.ExpectedRevision || p.OwnerMemberID != in.ActorID || attempt != in.AttemptID {
		return zero, ErrMemberToolDenied
	}
	blocked, active, err := taskWorkCurrentInTx(ctx, tx, f)
	if err != nil {
		return zero, err
	}
	if blocked != "" || active != attempt {
		return zero, ErrConflict
	}
	return p, taskWorkChildrenSettledInTx(ctx, tx, p)
}

func taskWorkChildrenSettledInTx(ctx context.Context, tx *sql.Tx, p domain.TaskWorkParent) error {
	if p.AssignmentID == "" {
		return nil
	}
	// Look from both sides so a broken association cannot disappear in a join.
	// The exact assignment and revision filter is deliberate: the formal gate
	// cannot impose a new obligation for another scoped assignment.
	rows, err := tx.QueryContext(ctx, `SELECT delivery_id FROM task_work_delegations
 WHERE (group_id,task_id,task_revision,assignment_id)=(:grp,:task,:rev,:assignment)
 UNION SELECT delivery_id FROM task_work_deliveries WHERE source_kind='delegation'
 AND (group_id,task_id,task_revision,assignment_id)=(:grp,:task,:rev,:assignment)`,
		sql.Named("grp", p.GroupID), sql.Named("task", p.TaskID), sql.Named("rev", p.TaskRevision), sql.Named("assignment", p.AssignmentID))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		child, err := taskWorkDeliveryInTx(ctx, tx, p.GroupID, id)
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrMemberToolDenied) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if child.delegation == nil || child.delegation.TaskWorkParent != p || child.turnID == "" {
			return ErrConflict
		}
		var settled bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turns h WHERE h.id=? AND h.group_id=? AND `+taskWorkTurnSettled+`)`,
			child.turnID, p.GroupID).Scan(&settled); err != nil {
			return err
		}
		if !settled {
			return ErrConflict
		}
	}
	return rows.Err()
}
