package store

import (
	"context"
	"database/sql"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// RecordNativePreflightFailure is only for a wrapper that did not invoke its
// adapter. With proveNotStarted the caller also holds the exact issued registry
// entry and has verified absence of the run directory under its pinned root.
// Store decides and, only if absent, writes the first receipt under the same
// claim lock as Apply. An unknown/cancelled or recovered run is not disproved.
func (s *Store) RecordNativePreflightFailure(ctx context.Context, scope MemberToolScope, deliveryID string, proveNotStarted bool) (provider.ExecutionReceipt, error) {
	const action = "保存原生会话启动前回执失败"
	var empty provider.ExecutionReceipt
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, queueError(action, err)
	}
	defer tx.Rollback()
	turn, err := lockTurnClaim(ctx, tx, scope.Claim)
	if err != nil {
		return empty, queueError(action, err)
	}
	if turn.MemberID != scope.MemberID || turn.ConversationID != scope.ConversationID || turn.DeliveryID != deliveryID {
		return empty, queueError(action, ErrStaleClaim)
	}
	var proof *MemberToolScope
	if proveNotStarted {
		proof = &scope
	}
	_, receipt, err := nativePreflightFailureInTx(ctx, tx, turn, proof)
	if err != nil {
		return empty, queueError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return empty, queueError(action, err)
	}
	receipt.NativePreflightFailure = true // Apply cannot replay this as new evidence.
	return receipt, nil
}

// A second Apply of a preflight refusal carries no absence proof; it may fill
// an empty receipt with unknown, but must not overwrite any prior receipt (even
// one we cannot decode), upgrade noRef or release a claim. Actual adapter and
// reconciliation evidence continue to use normal Apply, not this path.
func nativePreflightFailureInTx(ctx context.Context, tx *sql.Tx, turn domain.Turn, proof *MemberToolScope) (domain.Turn, provider.ExecutionReceipt, error) {
	receipt := normalizeTurnReceipt(turn, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, ExitCode: -1})
	var hasReceipt, hasBarrier bool
	var data string
	if err := tx.QueryRowContext(ctx, `SELECT receipt_json<>'',
	 CASE WHEN length(CAST(receipt_json AS BLOB))<=1048576 THEN receipt_json ELSE '' END,
	 EXISTS(SELECT 1 FROM native_run_bindings WHERE turn_id=turns.id) FROM turns WHERE id=?`, turn.ID).Scan(&hasReceipt, &data, &hasBarrier); err != nil {
		return domain.Turn{}, receipt, err
	}
	if hasReceipt {
		if saved, err := decodeNativeReceipt(turn, data); err == nil && (saved.Outcome == provider.OutcomeUnknown || domain.TurnState(saved.Outcome) == turn.State) {
			receipt = saved
		}
		return turn, receipt, nil
	}
	if !activeTurn(turn.State) {
		return domain.Turn{}, receipt, ErrConflict
	}
	if proof != nil && !hasBarrier && nativeFreshTurn(turn) && memberToolScopeInTx(ctx, tx, *proof) == nil {
		receipt.Outcome = provider.OutcomeNotStarted
		receipt = normalizeTurnReceipt(turn, receipt)
	}
	settled, err := applyTurnReceiptInTx(ctx, tx, turn, receipt)
	return settled, receipt, err
}
