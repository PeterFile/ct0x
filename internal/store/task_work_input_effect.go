package store

import (
	"context"
	"database/sql"
	"time"

	"agentworkspace/internal/domain"
)

const (
	workInputAckOperation    = "acknowledge_work_input"
	workInputReturnOperation = "hand_back_work_input"
)

func (s *Store) handleWorkInput(ctx context.Context, scope MemberToolScope, operation, inputID, reason, key string) (domain.TaskWorkInput, error) {
	action := "确认执行成员后续输入失败"
	if operation == workInputReturnOperation {
		action = "交回超出授权的后续输入失败"
	}
	return taskWorkMutationTx(ctx, s, scope, action, func(tx *sql.Tx) (domain.TaskWorkInput, error) {
		return s.handleWorkInputInTx(ctx, tx, scope, operation, inputID, reason, key)
	})
}

func (s *Store) handleWorkInputInTx(ctx context.Context, tx *sql.Tx, scope MemberToolScope, operation, inputID, reason, key string) (out domain.TaskWorkInput, err error) {
	handBack := operation == workInputReturnOperation
	role, original, err := s.workInputHandlingOwnerInTx(ctx, tx, scope, handBack)
	if err != nil {
		return out, err
	}
	if !validTriggerID(inputID) || handBack && !memberToolTextValid(reason, 2048) {
		return out, ErrInvalid
	}
	if isDecisionAnswerID(inputID) {
		if handBack {
			return out, ErrMemberToolDenied
		}
		return s.consumeDecisionAnswerInTx(ctx, tx, role, original, inputID, key)
	}
	installed, err := workInputSchemaInTx(ctx, tx)
	if err != nil {
		return out, err
	}
	if !installed {
		return out, ErrMemberToolDenied
	}

	// Scope the key only after deriving the real live role. An exact retry can
	// cross Turns of the same Assignment, but the key never grants that role.
	p := role.Provenance
	m := mutationScope{operation: operation, actorID: p.MemberID, groupID: p.GroupID, key: key}
	fields := []string{inputID, p.AssignmentID}
	if handBack {
		fields = append(fields, reason)
	}
	payload := mutationPayload(fields...)
	id, replay, err := lookupMutation(ctx, tx, m, payload)
	if err != nil {
		return out, err
	}
	if replay && id != inputID {
		return out, ErrConflict
	}
	r := original.r
	i, err := workerWorkInputInTx(ctx, tx, p, inputID, r.MandateID)
	if err != nil {
		return out, err
	}
	if handBack {
		if i.Consumption != nil || i.Return != nil && i.Return.Reason != reason {
			return out, ErrConflict
		}
	} else if i.Return != nil {
		return out, ErrConflict
	}
	fresh := i.Consumption == nil && i.Return == nil
	var decision workInputReturnDecision
	if fresh {
		if replay {
			return out, ErrConflict // A key is not a substitute for a persisted fact.
		}
		if handBack {
			decision, err = decideWorkInputReturnInTx(ctx, tx, i, original)
			if err != nil {
				return out, err
			}
			i.Return, err = insertWorkInputReturnInTx(ctx, tx, role, i, reason, decision)
		} else {
			c := &domain.TaskWorkInputConsumption{TurnID: scope.Claim.TurnID, AttemptID: role.AttemptID, ConsumedAt: time.Now().UTC()}
			err = coordinationDispatchWrite(ctx, tx, `INSERT INTO task_work_input_consumptions(input_id,turn_id,attempt_id,consumed_at) VALUES(?,?,?,?)`,
				inputID, c.TurnID, c.AttemptID, c.ConsumedAt.Format(timestampLayout))
			i.Consumption = c
		}
		if err != nil {
			return out, err
		}
	}

	newKey := key != "" && !replay
	if newKey {
		if err := coordinationDispatchWrite(ctx, tx, `INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id)
		 VALUES(?,?,?,?,?,?)`, m.operation, m.actorID, m.groupID, m.key, payload, inputID); err != nil {
			return out, err
		}
	}
	if fresh || newKey {
		// Verify the actual effect and current authority after the last write,
		// including any changes made by triggers in this transaction.
		current, owner, err := s.workInputHandlingOwnerInTx(ctx, tx, scope, handBack)
		if err != nil {
			return out, err
		}
		if current != role || owner.r != r {
			return out, ErrMemberToolDenied
		}
		if handBack && fresh {
			applied, err := decideWorkInputReturnInTx(ctx, tx, i, owner)
			if err != nil {
				return out, err
			}
			if applied != decision {
				return out, ErrConflict
			}
		}
		saved, err := workerWorkInputInTx(ctx, tx, p, inputID, r.MandateID)
		if err != nil {
			return out, err
		}
		if c := i.Consumption; c != nil {
			if saved.Consumption == nil || *saved.Consumption != *c || saved.Return != nil {
				return out, ErrConflict
			}
		} else if saved.Return == nil || *saved.Return != *i.Return || saved.Consumption != nil {
			return out, ErrConflict
		}
		i = saved
	}
	if err := workInputReceiptsInTx(ctx, tx, role, r, i); err != nil {
		return out, err
	}
	if i.Return != nil {
		if err := verifyWorkInputReturnNoticeInTx(ctx, tx, i); err != nil {
			return out, err
		}
	}
	if newKey {
		id, present, err := lookupMutation(ctx, tx, m, payload)
		if err != nil {
			return out, err
		}
		if !present || id != inputID {
			return out, ErrConflict
		}
	}
	return i, nil
}
