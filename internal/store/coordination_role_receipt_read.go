package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

// CoordinationRoleReceiptSnapshot is a trusted local read, not a transport DTO.
type CoordinationRoleReceiptSnapshot struct {
	Turn    domain.Turn                `json:"-"`
	Receipt *provider.ExecutionReceipt `json:"-"`
}

type coordinationRoleReceiptReadError struct{ cause error }

func (e *coordinationRoleReceiptReadError) Error() string { return "读取角色当前回执失败" }
func (e *coordinationRoleReceiptReadError) Unwrap() error { return e.cause }

// GetCoordinationRoleReceipt reads CURRENT normalized evidence in one snapshot.
// Absence does not prove nonexecution; this is not the original Apply snapshot.
func (s *Store) GetCoordinationRoleReceipt(ctx context.Context, groupID, turnID string) (out CoordinationRoleReceiptSnapshot, err error) {
	defer func() {
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				err = ErrNotFound
			}
			err = &coordinationRoleReceiptReadError{cause: err}
		}
	}()
	if !validTriggerID(groupID) || !validTriggerID(turnID) {
		return out, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	turn, err := scanTurn(tx.QueryRowContext(ctx, "SELECT "+turnColumns+" FROM turns WHERE group_id=? AND id=?", groupID, turnID))
	if err != nil {
		return out, err
	}
	var data string
	if err = tx.QueryRowContext(ctx, "SELECT receipt_json FROM turns WHERE group_id=? AND id=?", groupID, turnID).Scan(&data); err != nil {
		return out, err
	}
	var receipt *provider.ExecutionReceipt
	if data != "" {
		// Current normalized receipts have a <=64 KiB session, <=4096-byte
		// paths and fixed Reasons. Bound decoding/round-trip allocation even
		// for a damaged column; this does not constrain execution or Apply.
		if len(data) > 1<<20 {
			return out, ErrInvalid
		}
		decoder := json.NewDecoder(strings.NewReader(data))
		decoder.DisallowUnknownFields()
		if !utf8.ValidString(data) || decoder.Decode(&receipt) != nil || receipt == nil || receipt.Reason == "" {
			return out, ErrInvalid
		}
		switch receipt.Outcome {
		case provider.OutcomeUnknown, provider.OutcomeNotStarted, provider.OutcomeSucceeded, provider.OutcomeFailed, provider.OutcomeAborted, provider.OutcomeTruncated:
		default:
			return out, ErrInvalid
		}
		if decoder.Decode(new(any)) != io.EOF {
			return out, ErrInvalid
		}
		canonical, encodeErr := json.Marshal(receipt)
		if encodeErr != nil || string(canonical) != data {
			return out, ErrInvalid
		}
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	return CoordinationRoleReceiptSnapshot{Turn: turn, Receipt: receipt}, nil
}
