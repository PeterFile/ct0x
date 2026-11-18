package store

import (
	"context"

	"agentworkspace/internal/domain"
)

// VerifyFreshTaskWorkResource is the runtime's last Store check for a fresh
// text followup. It checks B's current authority and the exact sole resource
// of the same live Turn under SQLite write serialization. It neither binds an
// Attempt nor authorizes native resume. No transaction escapes to the caller;
// filesystem, transport and native I/O must stay outside this check.
func (s *Store) VerifyFreshTaskWorkResource(ctx context.Context, scope MemberToolScope, resourceKey string) error {
	const action = "核验任务续办轮次资源失败"
	if !memberToolIDsValid(resourceKey) {
		return ErrInvalid
	}
	// The first conditional write fences other handles even on a deferred
	// connection. B's loaders also recheck the exact scope after that write.
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return memberToolOperationError(action, err)
	}
	defer tx.Rollback()
	f, err := taskWorkExecutionInTx(ctx, tx, scope)
	if err != nil {
		return memberToolOperationError(action, err)
	}
	if f.d.SourceKind != domain.TaskWorkSourceFollowup {
		return ErrMemberToolDenied
	}
	// Reuse the same pending-input predicate as B's claim gate. Readiness
	// alone allows an already-bound Turn to finish handling its input.
	handled, err := taskWorkInputHandledInTx(ctx, tx, f.d.SourceID)
	if err != nil {
		return memberToolOperationError(action, err)
	}
	if handled {
		return ErrMemberToolDenied
	}
	blocked, _, err := s.taskWorkReadinessInTx(ctx, tx, f, f.turnID)
	if err != nil {
		return memberToolOperationError(action, err)
	}
	if blocked != "" {
		return ErrMemberToolDenied
	}
	chatAction, err := s.taskWorkOwnerChatActionInTx(ctx, tx, f)
	if err != nil {
		return memberToolOperationError(action, err)
	}
	if chatAction != domain.ChatActionTextSubmission {
		return ErrMemberToolDenied // This runtime does not launch investigations.
	}
	var held bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM resource_claims r
	 JOIN turns t ON t.id=r.turn_id
	 WHERE r.resource_key=? AND r.turn_id=? AND t.state='starting'
	 AND t.accepted_at IS NULL AND t.native_session_id='' AND t.cancel_requested_at IS NULL
	 AND t.candidate_text='' AND t.candidate_terminal='' AND t.receipt_json=''
	 AND NOT EXISTS(SELECT 1 FROM resource_claims other WHERE other.turn_id=t.id AND other.resource_key<>r.resource_key))`,
		resourceKey, f.turnID).Scan(&held)
	if err != nil {
		return memberToolOperationError(action, err)
	}
	if !held {
		return ErrMemberToolDenied
	}
	// The no-op scope write was only a lock. Release it without publishing
	// anything, and do not report success if the release was not confirmed.
	if err := tx.Rollback(); err != nil {
		return memberToolOperationError(action, err)
	}
	return ctx.Err()
}
