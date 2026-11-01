package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// ApplyTurnReceipt accepts trusted internal adapter evidence, never model/tool input.
// Native terminal evidence alone cannot publish a reply or release execution claims.
func (s *Store) ApplyTurnReceipt(ctx context.Context, claim TurnClaim, receipt provider.ExecutionReceipt) (domain.Turn, error) {
	const action = "结算轮次失败"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	defer tx.Rollback()
	turn, err := lockTurnClaim(ctx, tx, claim)
	if err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	receipt = normalizeTurnReceipt(turn, receipt)
	data, err := json.Marshal(receipt)
	if err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	if !activeTurn(turn.State) {
		var previous string
		if err := tx.QueryRowContext(ctx, "SELECT receipt_json FROM turns WHERE group_id=? AND id=? AND claim_token=?", claim.GroupID, claim.TurnID, claim.Token).Scan(&previous); err != nil {
			return domain.Turn{}, queueError(action, err)
		}
		if previous != string(data) {
			return domain.Turn{}, queueError(action, ErrConflict)
		}
		if err := tx.Commit(); err != nil {
			return domain.Turn{}, queueError(action, err)
		}
		return turn, nil
	}
	// A callback may have failed to persist acceptance even though the adapter
	// observed it. Unknown/DeliveryFailed does not erase that matching fact.
	if receipt.Started && validNativeSession(receipt.NativeSessionID) && (turn.NativeSessionID == "" || turn.NativeSessionID == receipt.NativeSessionID) {
		turn.NativeSessionID = receipt.NativeSessionID
		if receipt.Accepted {
			acceptTurn(&turn)
		}
	}
	turn.State, turn.Reason = domain.TurnState(receipt.Outcome), receipt.Reason
	if terminalOutcome(receipt.Outcome) && strings.TrimSpace(turn.CandidateText) != "" {
		if err := publishTurnReply(ctx, tx, &turn); err != nil {
			return domain.Turn{}, queueError(action, err)
		}
	}
	if err := writeClaimedTurn(ctx, tx, &turn); err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE turns SET receipt_json=? WHERE group_id=? AND id=? AND claim_token=?", string(data), claim.GroupID, claim.TurnID, claim.Token); err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	if !activeTurn(turn.State) {
		if turn.AcceptedAt == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET state='failed',error=?
			 WHERE group_id=? AND id=? AND turn_id=? AND state<>'accepted'
			 AND EXISTS(SELECT 1 FROM turns WHERE group_id=? AND id=? AND claim_token=?)`,
				turn.Reason, turn.GroupID, turn.DeliveryID, turn.ID, claim.GroupID, claim.TurnID, claim.Token); err != nil {
				return domain.Turn{}, queueError(action, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM resource_claims WHERE turn_id=?
		 AND EXISTS(SELECT 1 FROM turns WHERE group_id=? AND id=? AND claim_token=?)`, turn.ID, claim.GroupID, claim.TurnID, claim.Token); err != nil {
			return domain.Turn{}, queueError(action, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Turn{}, queueError(action, err)
	}
	return turn, nil
}

func normalizeTurnReceipt(turn domain.Turn, receipt provider.ExecutionReceipt) provider.ExecutionReceipt {
	outcome := provider.OutcomeUnknown
	bound := validNativeSession(receipt.NativeSessionID) && (turn.NativeSessionID == "" || turn.NativeSessionID == receipt.NativeSessionID)
	if terminalOutcome(receipt.Outcome) && receipt.Terminal == receipt.Outcome && receipt.Started && receipt.Accepted && bound &&
		receipt.OutputComplete && receipt.DirectChildReaped && !receipt.DeliveryFailed &&
		(turn.CandidateTerminal == "" || turn.CandidateTerminal == domain.TurnState(receipt.Terminal)) {
		// Outcome is adapter-normalized. In particular, trustworthy failed turns
		// may have nonzero exit codes; storage must not reinterpret CLI semantics.
		outcome = receipt.Outcome
	}
	if receipt.Outcome == provider.OutcomeNotStarted && !receipt.Started && !receipt.Accepted && receipt.NativeSessionID == "" &&
		receipt.Terminal == "" && !receipt.DeliveryFailed && !receipt.DirectChildReaped && !receipt.OutputComplete &&
		turn.AcceptedAt == nil && turn.NativeSessionID == "" && turn.CandidateText == "" && turn.CandidateTerminal == "" &&
		(turn.State == domain.TurnStateStarting || turn.State == domain.TurnStateCancelRequested || turn.State == domain.TurnStateNotStarted) {
		// Unknown/recovered executions cannot be disproved by a fresh invocation
		// that did not start. NotStarted is included only for identical replay.
		outcome = provider.OutcomeNotStarted
	}
	receipt.Outcome = outcome
	if !terminalOutcome(receipt.Terminal) {
		receipt.Terminal = ""
	}
	if !validNativeSession(receipt.NativeSessionID) {
		receipt.NativeSessionID = ""
	}
	receipt.StdoutPath, receipt.StderrPath = receiptLogPath(receipt.StdoutPath), receiptLogPath(receipt.StderrPath)
	receipt.Reason = map[provider.Outcome]string{
		provider.OutcomeUnknown: "执行结果未知，需对账后再处理", provider.OutcomeNotStarted: "执行未启动",
		provider.OutcomeSucceeded: "执行完成", provider.OutcomeFailed: "模型执行失败",
		provider.OutcomeAborted: "模型执行已中止", provider.OutcomeTruncated: "模型输出被截断",
	}[outcome]
	return receipt
}

func receiptLogPath(path string) string {
	if len(path) > 4096 || !utf8.ValidString(path) || strings.ContainsAny(path, "\x00\r\n") {
		return ""
	}
	return path
}

func publishTurnReply(ctx context.Context, tx *sql.Tx, turn *domain.Turn) error {
	message := domain.Message{
		ID: rand.Text(), GroupID: turn.GroupID, SenderID: turn.MemberID,
		Content: turn.CandidateText, CreatedAt: time.Now().UTC(), Deliveries: []domain.Delivery{},
	}
	if err := tx.QueryRowContext(ctx, "SELECT topic_id FROM conversations WHERE group_id=? AND id=? AND member_id=?",
		turn.GroupID, turn.ConversationID, turn.MemberID).Scan(&message.TopicID); err != nil {
		return err
	}
	if err := insertMessageRow(ctx, tx, message); err != nil {
		return err
	}
	turn.ReplyMessageID = message.ID
	return nil
}
