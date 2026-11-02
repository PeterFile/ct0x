package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"agentworkspace/internal/domain"
)

const coordinationDispatchRequestColumns = `id,group_id,topic_id,task_id,task_revision,authority_generation,
 mandate_id,coordinator_member_id,binding_revision,policy_revision,requested_by,reason,created_at`

// GetDispatchRequest reads history and current state, not current dispatch eligibility.
func (s *Store) GetDispatchRequest(ctx context.Context, groupID, requestID string) (domain.DispatchView, error) {
	if !validTriggerID(groupID) || !validTriggerID(requestID) {
		return domain.DispatchView{}, fmt.Errorf("%w：群组或分派请求标识无效", ErrInvalid)
	}
	tx, err := s.beginCoordinatorRead(ctx, groupID)
	if err != nil {
		return domain.DispatchView{}, storeError("读取分派请求失败", err)
	}
	defer tx.Rollback()
	request, err := coordinationDispatchRequestInTx(ctx, tx, groupID, requestID)
	if err != nil {
		return domain.DispatchView{}, storeError("读取分派请求失败", err)
	}
	state, err := coordinationDispatchStateInTx(ctx, tx, request)
	if err != nil {
		return domain.DispatchView{}, storeError("读取分派状态失败", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.DispatchView{}, storeError("读取分派请求失败", err)
	}
	return domain.DispatchView{Request: request, State: state}, nil
}

func coordinationDispatchRequestInTx(ctx context.Context, tx *sql.Tx, groupID, requestID string) (domain.DispatchRequest, error) {
	var request domain.DispatchRequest
	var stamp string
	err := tx.QueryRowContext(ctx, "SELECT "+coordinationDispatchRequestColumns+" FROM coordination_dispatch_requests WHERE group_id=? AND id=?", groupID, requestID).Scan(
		&request.ID, &request.GroupID, &request.TopicID, &request.TaskID, &request.TaskRevision, &request.AuthorityGeneration,
		&request.MandateID, &request.CoordinatorMemberID, &request.BindingRevision, &request.PolicyRevision, &request.RequestedBy, &request.Reason, &stamp)
	if err != nil {
		return domain.DispatchRequest{}, err
	}
	request.CreatedAt, err = coordinationDispatchTime(stamp)
	return request, err
}

func coordinationDispatchStateInTx(ctx context.Context, tx *sql.Tx, request domain.DispatchRequest) (domain.DispatchState, error) {
	var state domain.DispatchState
	var next, updated string
	var messageID, deliveryID, conversationID, memberID, materialized sql.NullString
	var fromGeneration sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT request_id,generation,status,attempt_count,next_attempt_at,updated_at,
	 last_error_code,held_reason,message_id,delivery_id,conversation_id,delivered_member_id,materialized_from_generation,materialized_at
	 FROM coordination_dispatch_state WHERE group_id=? AND request_id=? AND task_id=?`, request.GroupID, request.ID, request.TaskID).Scan(
		&state.RequestID, &state.Generation, &state.Status, &state.AttemptCount, &next, &updated,
		&state.LastErrorCode, &state.HeldReason, &messageID, &deliveryID, &conversationID, &memberID, &fromGeneration, &materialized)
	if err != nil {
		return domain.DispatchState{}, err
	}
	if state.NextAttemptAt, err = coordinationDispatchTime(next); err != nil {
		return domain.DispatchState{}, err
	}
	if _, err := coordinationDispatchTime(updated); err != nil {
		return domain.DispatchState{}, err
	}
	present := messageID.Valid || deliveryID.Valid || conversationID.Valid || memberID.Valid || fromGeneration.Valid || materialized.Valid
	if present {
		if !messageID.Valid || !deliveryID.Valid || !conversationID.Valid || !memberID.Valid || !fromGeneration.Valid || !materialized.Valid {
			return domain.DispatchState{}, fmt.Errorf("%w：分派投递回执不完整", ErrInvalid)
		}
		at, err := coordinationDispatchTime(materialized.String)
		if err != nil {
			return domain.DispatchState{}, err
		}
		state.Delivery = &domain.DispatchDeliveryReceipt{
			MessageID: messageID.String, DeliveryID: deliveryID.String, ConversationID: conversationID.String,
			CoordinatorMemberID: memberID.String, MaterializedFromGeneration: fromGeneration.Int64, CreatedAt: at,
		}
	}
	return state, nil
}

func coordinationDispatchTime(stamp string) (time.Time, error) {
	at, err := time.Parse(timestampLayout, stamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w：分派时间无效：%w", ErrInvalid, err)
	}
	if at.Year() < 1 || at.Year() > 9999 || at.UTC().Format(timestampLayout) != stamp {
		return time.Time{}, fmt.Errorf("%w：分派时间不是规范的 UTC 时间", ErrInvalid)
	}
	return at.UTC(), nil
}
