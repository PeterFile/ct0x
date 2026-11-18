package store

import (
	"context"
	"database/sql"
	"errors"

	"agentworkspace/internal/domain"
)

// InvestigationTaskWorkLaunch is a frozen typed plan, not a launch permit.
// AttemptID identifies the bound owner continuation, or is empty for a delegation.
type InvestigationTaskWorkLaunch struct {
	Scope       MemberToolScope
	Delivery    domain.TaskWorkDelivery
	RequestID   string
	AttemptID   string
	ProviderID  string
	Model       string
	ResourceKey string
}

// CommitInvestigationTaskWorkLaunch is the final Store gate for a fresh typed
// investigation Turn. The caller holds the exact issued token, profile and
// executor fence and verifies physical resource identity. admit may only spend
// the trusted memory-only one-shot, not do I/O or start execution. Only a
// successful commit with a live context permits invocation. Errors and refusals
// are not execution outcomes; this gate neither settles nor releases a claim.
func (s *Store) CommitInvestigationTaskWorkLaunch(ctx context.Context, launch InvestigationTaskWorkLaunch, admit func() bool) (err error) {
	if admit == nil || !memberToolIDsValid(launch.Delivery.DeliveryID, launch.RequestID, launch.ProviderID, launch.Model, launch.ResourceKey) ||
		(launch.AttemptID != "" && !memberToolIDsValid(launch.AttemptID)) {
		return ErrInvalid
	}
	defer func() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		} else if err != nil {
			err = memberToolOperationError("核验调查工作启动失败", err)
		}
	}()

	// The first conditional scope write fences other Store writers before reads,
	// including on deferred SQLite connections.
	tx, err := s.beginMemberToolTx(ctx, launch.Scope)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	f, err := taskWorkExecutionInTx(ctx, tx, launch.Scope)
	if err != nil {
		return err
	}
	if f.d != launch.Delivery || f.v.r.ID != launch.RequestID {
		return ErrMemberToolDenied
	}
	if err := s.investigationTaskWorkLaunchRoleInTx(ctx, tx, f, launch); err != nil {
		return err
	}
	var exact bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turns t
	 JOIN members m ON (m.id,m.group_id)=(t.member_id,t.group_id)
	 JOIN resource_claims r ON r.turn_id=t.id AND r.resource_key=:resource
	 WHERE t.id=:turn AND m.status='active' AND m.provider_id=:provider AND m.model=:model AND m.workspace=''
	 AND t.state='starting' AND t.accepted_at IS NULL AND t.native_session_id='' AND t.cancel_requested_at IS NULL
	 AND t.candidate_terminal='' AND t.candidate_text='' AND t.receipt_json=''
	 AND NOT EXISTS(SELECT 1 FROM resource_claims other WHERE other.turn_id=t.id AND other.resource_key<>r.resource_key)
	 AND (:kind<>'delegation' OR NOT EXISTS(SELECT 1 FROM task_attempts a WHERE a.group_id=t.group_id AND a.turn_id=t.id)))`,
		sql.Named("turn", f.turnID), sql.Named("provider", launch.ProviderID), sql.Named("model", launch.Model),
		sql.Named("resource", launch.ResourceKey), sql.Named("kind", string(f.d.SourceKind))).Scan(&exact)
	if err != nil {
		return err
	}
	if !exact {
		return ErrMemberToolDenied
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !admit() {
		return ErrMemberToolDenied
	}
	return tx.Commit()
}

func (s *Store) investigationTaskWorkLaunchRoleInTx(ctx context.Context, tx *sql.Tx, f taskWorkFacts, launch InvestigationTaskWorkLaunch) error {
	followup := f.d.SourceKind == domain.TaskWorkSourceFollowup
	if followup {
		// A bound owner can handle its input, but a fresh start still needs
		// the exact pending input and the admitted investigation action.
		handled, err := taskWorkInputHandledInTx(ctx, tx, f.d.SourceID)
		if err != nil {
			return err
		}
		if handled {
			return ErrMemberToolDenied
		}
		action, err := s.taskWorkOwnerChatActionInTx(ctx, tx, f)
		if errors.Is(err, errTaskWorkChatScope) {
			return ErrMemberToolDenied
		}
		if err != nil {
			return investigationToolError(err)
		}
		if action != domain.ChatActionReadOnlyInvestigation {
			return ErrMemberToolDenied
		}
	}
	// The loader already proved the exact report or delegation source. Readiness
	// rechecks report policy, delegated materials and related Turn settlement.
	// Skip only the followup chat check, proved explicitly above.
	blocked, active, err := s.taskWorkReadinessForOwnerInTx(ctx, tx, f, f.turnID, followup)
	if err != nil {
		return err
	}
	if blocked != "" {
		return ErrMemberToolDenied
	}
	if f.d.SourceKind.IsOwnerContinuation() {
		bound, err := taskWorkBoundOwnerInTx(ctx, tx, f, launch.Scope)
		if err != nil {
			return err
		}
		if bound != launch.AttemptID || bound != active {
			return ErrMemberToolDenied
		}
	} else if launch.AttemptID != "" {
		return ErrMemberToolDenied
	}
	return nil
}
