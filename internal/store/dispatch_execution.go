package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"agentworkspace/internal/domain"
)

// GetDispatchExecution reuses historical relationship checks in one bounded snapshot.
// It never reconciles uncertain execution, reads logs, or releases claims.
func (s *Store) GetDispatchExecution(ctx context.Context, groupID, taskID, requestID string) (out domain.DispatchExecution, err error) {
	defer func() {
		if err != nil {
			out = domain.DispatchExecution{}
			err = storeError("读取分派执行回执失败", err)
		}
	}()
	if !validTriggerID(groupID) || !validTriggerID(taskID) || !validTriggerID(requestID) {
		return out, ErrInvalid
	}
	tx, err := s.beginCoordinatorRead(ctx, groupID)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	r, err := coordinationDispatchRequestInTx(ctx, tx, groupID, requestID)
	if err != nil {
		return out, err
	}
	if r.TaskID != taskID {
		return out, ErrNotFound
	}
	state, err := coordinationDispatchStateInTx(ctx, tx, r)
	if err != nil {
		return out, err
	}
	out.GroupID, out.TaskID, out.RequestID, out.DispatchStatus = groupID, taskID, requestID, state.Status
	if d := state.Delivery; d != nil {
		if d.CoordinatorMemberID != r.CoordinatorMemberID {
			return out, ErrInvalid
		}
		out.CoordinatorTurn, err = dispatchExecutionTurnInTx(ctx, tx, r, d.DeliveryID, d.MessageID, d.ConversationID, d.CoordinatorMemberID, "user")
		if err != nil {
			return out, err
		}
	}
	receipt, err := coordinationDecideReceiptInTx(ctx, tx, groupID, requestID)
	if errors.Is(err, sql.ErrNoRows) && state.Status != domain.DispatchStatusAssigned && state.Status != domain.DispatchStatusDeclined {
		err = nil
	} else if err == nil {
		d := receipt.Decision
		if out.CoordinatorTurn == nil || out.CoordinatorTurn.ID != d.CoordinatorTurnID {
			return out, ErrInvalid
		}
		out.Decision = &domain.DispatchExecutionDecision{ID: d.ID, Kind: d.Kind, CoordinatorTurnID: d.CoordinatorTurnID, CreatedAt: d.CreatedAt}
		if receipt.Assignment != nil {
			a, readErr := coordinationDecideAssignmentProjectionInTx(ctx, tx, groupID, receipt.Assignment.ID, false)
			if readErr != nil {
				return out, readErr
			}
			out.Assignment, out.AssignmentDispatchStatus, out.Binding = &a.Assignment, &a.Dispatch.Status, a.Binding
			if delivery := a.Dispatch.Delivery; delivery != nil {
				out.WorkerTurn, err = dispatchExecutionTurnInTx(ctx, tx, r, delivery.DeliveryID, delivery.MessageID, delivery.ConversationID, a.Assignment.WorkerMemberID, r.CoordinatorMemberID)
				if err != nil {
					return out, err
				}
			}
			if out.Binding != nil && (out.WorkerTurn == nil || out.WorkerTurn.ID != out.Binding.TurnID) {
				return out, ErrInvalid
			}
		}
	}
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}

func dispatchExecutionTurnInTx(ctx context.Context, tx *sql.Tx, r domain.DispatchRequest, deliveryID, messageID, conversationID, memberID, senderID string) (*domain.DispatchExecutionTurn, error) {
	var turnID string
	err := tx.QueryRowContext(ctx, `SELECT d.turn_id FROM deliveries d JOIN messages m ON m.id=d.message_id
	 JOIN conversations c ON c.id=d.conversation_id WHERE d.id=? AND d.group_id=? AND d.topic_id=?
	 AND d.message_id=? AND d.conversation_id=? AND d.recipient_id=? AND m.group_id=d.group_id
	 AND m.topic_id=d.topic_id AND m.sender_id=? AND c.group_id=d.group_id AND c.topic_id=d.topic_id AND c.member_id=d.recipient_id`,
		deliveryID, r.GroupID, r.TopicID, messageID, conversationID, memberID, senderID).Scan(&turnID)
	if err != nil {
		return nil, err
	}
	if turnID == "" {
		return nil, nil
	}
	t := &domain.DispatchExecutionTurn{}
	var created, updated, data string
	var accepted, canceled sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,group_id,delivery_id,conversation_id,member_id,state,native_session_id,
	 accepted_at,cancel_requested_at,created_at,updated_at,receipt_json FROM turns
	 WHERE id=? AND group_id=? AND delivery_id=? AND conversation_id=? AND member_id=?`,
		turnID, r.GroupID, deliveryID, conversationID, memberID).Scan(&t.ID, &t.GroupID, &t.DeliveryID, &t.ConversationID, &t.MemberID, &t.State,
		&t.NativeSessionID, &accepted, &canceled, &created, &updated, &data)
	if err != nil {
		return nil, err
	}
	for _, field := range []struct {
		stamp string
		dest  *time.Time
	}{{created, &t.CreatedAt}, {updated, &t.UpdatedAt}} {
		if *field.dest, err = coordinationDispatchTime(field.stamp); err != nil {
			return nil, err
		}
	}
	for _, field := range []struct {
		stamp sql.NullString
		dest  **time.Time
	}{{accepted, &t.AcceptedAt}, {canceled, &t.CancelRequestedAt}} {
		if field.stamp.Valid {
			at, err := coordinationDispatchTime(field.stamp.String)
			if err != nil {
				return nil, err
			}
			*field.dest = &at
		}
	}
	t.CancelRequested = t.CancelRequestedAt != nil
	if data != "" {
		// Stored receipts are adapter-normalized. Decode only the explicit transport whitelist.
		if len(data) > 1<<20 || !utf8.ValidString(data) || json.Unmarshal([]byte(data), &t.Receipt) != nil || t.Receipt == nil {
			return nil, ErrInvalid
		}
		switch t.Receipt.Outcome {
		case domain.TurnStateUnknown, domain.TurnStateNotStarted, domain.TurnStateSucceeded, domain.TurnStateFailed, domain.TurnStateAborted, domain.TurnStateTruncated:
		default:
			return nil, ErrInvalid
		}
		switch t.Receipt.Terminal {
		case "", domain.TurnStateSucceeded, domain.TurnStateFailed, domain.TurnStateAborted, domain.TurnStateTruncated:
		default:
			return nil, ErrInvalid
		}
	}
	return t, nil
}
