package store

import (
	"context"
	"database/sql"

	"agentworkspace/internal/domain"
)

// FreshTaskWorkLaunch is the runtime's frozen plan and issued scope, not a
// launch permit. AttemptID is empty only for verification before B binds it.
type FreshTaskWorkLaunch struct {
	Scope       MemberToolScope
	Delivery    domain.TaskWorkDelivery
	RequestID   string
	AttemptID   string
	ProviderID  string
	Model       string
	ResourceKey string
}

// VerifyFreshTaskWorkStartup is a snapshot check before constructing the
// executor. It does not bind an Attempt or authorize invocation.
func (s *Store) VerifyFreshTaskWorkStartup(ctx context.Context, launch FreshTaskWorkLaunch) error {
	return s.checkFreshTaskWorkLaunch(ctx, launch, nil)
}

// CommitFreshTaskWorkLaunch is the final authorization point for fresh Pi text
// work. Hold the issuing registry's revoke lock across the exact capability
// check and this call. admit spends the existing memory-only owner once; it
// must not do Store, filesystem, transport or provider I/O. Only a successful
// commit with a live context permits invocation. On failure the spent owner
// is not restored. No execution or acceptance evidence is persisted here.
// Later writes affect live tools and cancellation, not the committed launch.
func (s *Store) CommitFreshTaskWorkLaunch(ctx context.Context, launch FreshTaskWorkLaunch, admit func() bool) error {
	if !memberToolIDsValid(launch.AttemptID) || admit == nil {
		return ErrInvalid
	}
	return s.checkFreshTaskWorkLaunch(ctx, launch, admit)
}

func (s *Store) checkFreshTaskWorkLaunch(ctx context.Context, launch FreshTaskWorkLaunch, admit func() bool) error {
	const action = "核验任务续办启动失败"
	if !memberToolIDsValid(launch.ResourceKey, launch.Delivery.DeliveryID, launch.RequestID, launch.ProviderID, launch.Model) {
		return ErrInvalid
	}
	// The first statement is the existing conditional scope write, even on a
	// deferred connection. It fences member, policy, claim and receipt writers
	// on other handles before any reads; a read-only snapshot would not.
	tx, err := s.beginMemberToolTx(ctx, launch.Scope)
	if err != nil {
		return memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	var exact bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM resource_claims r
	 JOIN turns t ON t.id=r.turn_id
	 JOIN members m ON (m.id,m.group_id)=(t.member_id,t.group_id)
	 LEFT JOIN task_work_continuations b ON b.delivery_id=t.delivery_id
	 WHERE r.resource_key=:resource AND t.id=:turn AND t.delivery_id=:delivery
	 AND m.id=:member AND m.status='active' AND m.provider_id=:provider AND m.model=:model AND m.workspace=''
	 AND t.state='starting' AND t.accepted_at IS NULL AND t.native_session_id='' AND t.cancel_requested_at IS NULL
	 AND t.candidate_text='' AND t.candidate_terminal='' AND t.receipt_json=''
	 AND ((:attempt='' AND b.delivery_id IS NULL) OR (:attempt<>'' AND b.turn_id=t.id AND b.attempt_id=:attempt))
	 AND NOT EXISTS(SELECT 1 FROM resource_claims other WHERE other.turn_id=t.id AND other.resource_key<>r.resource_key))`,
		sql.Named("resource", launch.ResourceKey), sql.Named("turn", launch.Scope.Claim.TurnID), sql.Named("delivery", launch.Delivery.DeliveryID),
		sql.Named("member", launch.Scope.MemberID), sql.Named("provider", launch.ProviderID), sql.Named("model", launch.Model),
		sql.Named("attempt", launch.AttemptID)).Scan(&exact)
	if err != nil {
		return memberToolOperationError(action, err)
	}
	if !exact {
		return ErrMemberToolDenied
	}
	f, err := taskWorkExecutionInTx(ctx, tx, launch.Scope)
	if err != nil {
		return memberToolOperationError(action, err)
	}
	if f.d != launch.Delivery || f.v.r.ID != launch.RequestID {
		return ErrMemberToolDenied
	}
	// B permits an already-bound Turn to finish handling its input. Startup
	// still requires the exact source to be pending, as at B's claim gate.
	handled, err := taskWorkInputHandledInTx(ctx, tx, f.d.SourceID)
	if err != nil {
		return memberToolOperationError(action, err)
	}
	if handled {
		return ErrMemberToolDenied
	}
	basis, err := taskWorkOwnerChatBasisInTx(ctx, tx, f)
	if err != nil {
		return memberToolOperationError(action, err)
	}
	if basis.action != domain.ChatActionTextSubmission {
		return ErrMemberToolDenied
	}
	// Preserve the former role resolver's exclusion of retained or damaged
	// investigation evidence, even though this admission is text-only.
	if _, err := s.investigationProfileInTx(ctx, tx, taskWorkOwnerProvenance(f), false); err != nil {
		return memberToolOperationError(action, err)
	}
	// Check the remaining B readiness predicates (including the current
	// Mandate and active Attempt) without repeating the chat authority query.
	blocked, active, err := s.taskWorkReadinessForOwnerInTx(ctx, tx, f, f.turnID, true)
	if err != nil {
		return memberToolOperationError(action, err)
	}
	if blocked != "" || (launch.AttemptID != "" && active != launch.AttemptID) {
		return ErrMemberToolDenied
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if admit == nil {
		err = tx.Rollback() // The scope write was only a lock.
	} else {
		if !admit() {
			return ErrMemberToolDenied
		}
		err = tx.Commit()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return memberToolOperationError(action, err)
	}
	return nil
}
