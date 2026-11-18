package store

import (
	"context"
	"time"

	"agentworkspace/internal/domain"
)

const (
	workInputAckOperation    = "acknowledge_work_input"
	workInputReturnOperation = "hand_back_work_input"
)

func (s *Store) handleWorkInput(ctx context.Context, scope MemberToolScope, operation, inputID, reason, key string) (out domain.TaskWorkInput, err error) {
	handBack := operation == workInputReturnOperation
	action := "确认执行成员后续输入失败"
	if handBack {
		action = "交回超出授权的后续输入失败"
	}
	defer func() {
		if err != nil {
			err = memberToolOperationError(action, err)
		}
	}()
	tx, err := s.beginMemberToolTx(ctx, scope)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	role, err := s.workInputWorkerInTx(ctx, tx, scope)
	if err != nil {
		return out, err
	}
	if !validTriggerID(inputID) || handBack && !memberToolTextValid(reason, 2048) {
		return out, ErrInvalid
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
	r, err := coordinationDispatchRequestInTx(ctx, tx, p.GroupID, p.RequestID)
	if err != nil {
		return out, coordinationRoleRelationError(err)
	}
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
	if fresh {
		if replay {
			return out, ErrConflict // A key is not a substitute for a persisted fact.
		}
		if handBack {
			i.Return, err = insertWorkInputReturnInTx(ctx, tx, role, i, reason)
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
		current, err := s.workInputWorkerInTx(ctx, tx, scope)
		if err != nil {
			return out, err
		}
		if current != role {
			return out, ErrMemberToolDenied
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
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return i, nil
}
