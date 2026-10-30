package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

var ErrStaleClaim = errors.New("轮次认领已失效")

type TurnClaim struct {
	GroupID string
	TurnID  string
	Token   string `json:"-"`
}

func (s *Store) RecordTurnEvent(ctx context.Context, claim TurnClaim, event provider.Event) error {
	const action = "保存轮次事件失败"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return queueError(action, err)
	}
	defer tx.Rollback()
	turn, err := lockTurnClaim(ctx, tx, claim)
	if err != nil {
		return queueError(action, err)
	}
	if !activeTurn(turn.State) {
		return queueError(action, ErrConflict)
	}
	if !validTurnEvent(turn, event) {
		return queueError(action, ErrInvalid)
	}
	turn.NativeSessionID = event.NativeSessionID
	switch event.Kind {
	case provider.AcceptedEvent:
		acceptTurn(&turn)
		if turn.State == domain.TurnStateStarting {
			turn.State = domain.TurnStateRunning
		}
	case provider.ResponseEvent:
		turn.CandidateText = event.Text
	case provider.TerminalEvent:
		turn.CandidateTerminal = domain.TurnState(event.Terminal)
	}
	if err := writeClaimedTurn(ctx, tx, &turn); err != nil {
		return queueError(action, err)
	}
	if err := tx.Commit(); err != nil {
		return queueError(action, err)
	}
	return nil
}

func validNativeSession(id string) bool {
	return id != "" && len(id) <= 64<<10 && utf8.ValidString(id) && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "\x00\r\n")
}

func terminalOutcome(outcome provider.Outcome) bool {
	return outcome == provider.OutcomeSucceeded || outcome == provider.OutcomeFailed || outcome == provider.OutcomeAborted || outcome == provider.OutcomeTruncated
}

func activeTurn(state domain.TurnState) bool {
	return state == domain.TurnStateStarting || state == domain.TurnStateRunning || state == domain.TurnStateCancelRequested || state == domain.TurnStateUnknown
}

func validTurnEvent(turn domain.Turn, event provider.Event) bool {
	if !validNativeSession(event.NativeSessionID) || (turn.NativeSessionID != "" && turn.NativeSessionID != event.NativeSessionID) || len(event.Text) > 64<<10 || !utf8.ValidString(event.Text) {
		return false
	}
	if turn.CandidateTerminal != "" {
		return event.Kind == provider.TerminalEvent && event.Text == "" && domain.TurnState(event.Terminal) == turn.CandidateTerminal
	}
	switch event.Kind {
	case provider.SessionEvent:
		return turn.AcceptedAt == nil && event.Text == "" && event.Terminal == ""
	case provider.AcceptedEvent:
		return event.Text == "" && event.Terminal == ""
	case provider.ResponseEvent:
		return turn.AcceptedAt != nil && event.Terminal == ""
	case provider.TerminalEvent:
		return turn.AcceptedAt != nil && event.Text == "" && terminalOutcome(event.Terminal)
	}
	return false
}

// The conditional write serializes deferred transactions before any decision.
// Terminal history is read only, for receipt replay or a rejected late event.
func lockTurnClaim(ctx context.Context, tx *sql.Tx, claim TurnClaim) (domain.Turn, error) {
	turn, err := scanTurn(tx.QueryRowContext(ctx, `UPDATE turns SET claim_token=claim_token
	 WHERE group_id=? AND id=? AND claim_token=? AND `+activeTurnPredicate+` RETURNING `+turnColumns, claim.GroupID, claim.TurnID, claim.Token))
	if errors.Is(err, sql.ErrNoRows) {
		turn, err = scanTurn(tx.QueryRowContext(ctx, "SELECT "+turnColumns+" FROM turns WHERE group_id=? AND id=? AND claim_token=?", claim.GroupID, claim.TurnID, claim.Token))
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrStaleClaim
		}
	}
	return turn, err
}

func acceptTurn(turn *domain.Turn) {
	if turn.AcceptedAt == nil {
		now := time.Now().UTC()
		turn.AcceptedAt = &now
	}
}

func writeClaimedTurn(ctx context.Context, tx *sql.Tx, turn *domain.Turn) error {
	turn.UpdatedAt = time.Now().UTC()
	_, err := tx.ExecContext(ctx, `UPDATE turns SET state=?,native_session_id=?,accepted_at=?,cancel_requested_at=?,
	 candidate_text=?,candidate_terminal=?,reply_message_id=?,reason=?,updated_at=? WHERE group_id=? AND id=? AND claim_token=?`,
		turn.State, turn.NativeSessionID, turnStamp(turn.AcceptedAt), turnStamp(turn.CancelRequestedAt), turn.CandidateText, turn.CandidateTerminal,
		turn.ReplyMessageID, turn.Reason, turn.UpdatedAt.Format(timestampLayout), turn.GroupID, turn.ID, turn.ClaimToken)
	if err == nil && turn.AcceptedAt != nil {
		_, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='accepted',error=''
		 WHERE group_id=? AND id=? AND turn_id=? AND EXISTS(SELECT 1 FROM turns WHERE group_id=? AND id=? AND claim_token=?)`,
			turn.GroupID, turn.DeliveryID, turn.ID, turn.GroupID, turn.ID, turn.ClaimToken)
	}
	return err
}

func turnStamp(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(timestampLayout)
}

// Cancellation records user intent, never evidence that an external run stopped.
func (s *Store) RequestTurnCancel(ctx context.Context, groupID, turnID string) (domain.Turn, error) {
	const action = "请求取消轮次失败"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	defer tx.Rollback()
	turn, err := scanTurn(tx.QueryRowContext(ctx, `UPDATE turns SET claim_token=claim_token
	 WHERE group_id=? AND id=? AND `+activeTurnPredicate+` RETURNING `+turnColumns, groupID, turnID))
	if errors.Is(err, sql.ErrNoRows) {
		turn, err = scanTurn(tx.QueryRowContext(ctx, "SELECT "+turnColumns+" FROM turns WHERE group_id=? AND id=?", groupID, turnID))
	}
	if err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	if activeTurn(turn.State) && turn.CancelRequestedAt == nil {
		now := time.Now().UTC().Format(timestampLayout)
		turn, err = scanTurn(tx.QueryRowContext(ctx, `UPDATE turns SET cancel_requested_at=?,updated_at=?,
		 state=CASE WHEN state IN ('starting','running') THEN 'cancel_requested' ELSE state END
		 WHERE group_id=? AND id=? AND `+activeTurnPredicate+` RETURNING `+turnColumns, now, now, groupID, turnID))
		if err != nil {
			return domain.Turn{}, queueError(action, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	return turn, nil
}
