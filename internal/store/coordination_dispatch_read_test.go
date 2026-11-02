package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestGetDispatchRequestExactScopeAndValidation(t *testing.T) {
	f := requestDispatchSetup(t)
	api := requestDispatchStore(t, f.s)
	request, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "read")
	if err != nil {
		t.Fatal(err)
	}
	other := group(t, f.s).ID
	before := messageSequenceSnapshot(t, f.s)
	for _, scope := range []struct{ groupID, requestID string }{
		{other, request.ID}, {"missing-group", request.ID}, {f.groupID, "missing-request"},
	} {
		got, err := api.GetDispatchRequest(ctx, scope.groupID, scope.requestID)
		requestDispatchZero(t, got, err, ErrNotFound)
	}
	for _, bad := range []string{"", " ", "\u2003", "id\n", "id\x00", string([]byte{0xff})} {
		got, err := api.GetDispatchRequest(ctx, bad, request.ID)
		requestDispatchZero(t, got, err, ErrInvalid)
		got, err = api.GetDispatchRequest(ctx, f.groupID, bad)
		requestDispatchZero(t, got, err, ErrInvalid)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err := api.GetDispatchRequest(cancelled, f.groupID, request.ID)
	requestDispatchZero(t, got, err, context.Canceled)
	messageSequencePreserved(t, f.s, before)
}

func TestGetDispatchRequestCurrentStateAndFullDelivery(t *testing.T) {
	for _, status := range []string{"backoff", "held", "enqueued"} {
		t.Run(status, func(t *testing.T) {
			f := requestDispatchSetup(t)
			api := requestDispatchStore(t, f.s)
			request, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "read")
			if err != nil {
				t.Fatal(err)
			}
			want := domain.DispatchView{Request: request, State: domain.DispatchState{
				RequestID: request.ID, Generation: 2, Status: domain.DispatchStatusPending, NextAttemptAt: request.CreatedAt,
			}}
			rows := requestDispatchSavedRows(t, f.s, request.ID)
			switch status {
			case "backoff":
				at := request.CreatedAt.Add(time.Second)
				want.State.AttemptCount, want.State.LastErrorCode, want.State.NextAttemptAt = 1, domain.DispatchErrorSQLiteLocked, at.Add(2*time.Second)
				coordinatorMustCommit(t, f.s, coordinatorSQL{`UPDATE coordination_dispatch_state SET generation=2,
				 attempt_count=1,last_error_code='sqlite_locked',next_attempt_at=?,updated_at=? WHERE request_id=?`,
					[]any{want.State.NextAttemptAt.Format(timestampLayout), at.Format(timestampLayout), request.ID}})
			case "held":
				want.State.Status, want.State.HeldReason = domain.DispatchStatusHeld, domain.DispatchHeldNoEligibleWorker
				coordinatorMustCommit(t, f.s, coordinatorSQL{`UPDATE coordination_dispatch_state SET generation=2,
				 status='held',held_reason='no_eligible_worker' WHERE request_id=?`, []any{request.ID}})
			case "enqueued":
				receipt := dispatchReceipt(t, f.s, rows)
				coordinatorMustCommit(t, f.s, dispatchUpdate(receipt))
				want.State.Status = domain.DispatchStatusEnqueued
				want.State.Delivery = &domain.DispatchDeliveryReceipt{
					MessageID: receipt[10].(string), DeliveryID: receipt[11].(string), ConversationID: receipt[12].(string),
					CoordinatorMemberID: f.coord, MaterializedFromGeneration: 1, CreatedAt: request.CreatedAt,
				}
			}
			if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE group_id=?", f.groupID); err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, f.s)
			for range 2 {
				if got, err := api.GetDispatchRequest(ctx, f.groupID, request.ID); err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("current projection differs: %+v, %v; want %+v", got, err, want)
				}
				if got, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "read"); err != nil || got != request {
					t.Fatalf("mutable projection polluted request replay: %+v, %v", got, err)
				}
			}
			messageSequencePreserved(t, f.s, before)
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := queueHandle(t, f.path, false)
			if got, err := requestDispatchStore(t, reopened).GetDispatchRequest(ctx, f.groupID, request.ID); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("reopened projection differs: %+v, %v", got, err)
			}
			messageSequencePreserved(t, reopened, before)
		})
	}
}

func TestGetDispatchRequestCanonicalTimestampDecoding(t *testing.T) {
	for _, stamp := range []string{
		"0001-01-01T00:00:00.000000000Z", "9999-12-31T23:59:59.999999999Z", "2024-02-29T23:59:59.123456789Z",
		"0000-01-01T00:00:00.000000000Z", "2026-01-02T03:04:05Z", "2026-01-02T03:04:05.123Z",
		"2026-01-02T03:04:05.123456789+00:00", "2026-02-29T03:04:05.123456789Z", "2026-01-02T24:00:00.000000000Z",
	} {
		t.Run(stamp, func(t *testing.T) {
			f := requestDispatchSetup(t)
			api := requestDispatchStore(t, f.s)
			request, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "read")
			if err != nil {
				t.Fatal(err)
			}
			// Bypass SQL validation only to exercise the read boundary's own canonical decoder.
			if _, err := f.s.db.Exec(`PRAGMA ignore_check_constraints=ON;
			 DROP TRIGGER coordination_dispatch_requests_no_update; DROP TRIGGER coordination_dispatch_state_update;`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec("UPDATE coordination_dispatch_requests SET created_at=? WHERE id=?", stamp, request.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec("UPDATE coordination_dispatch_state SET next_attempt_at=?,updated_at=? WHERE request_id=?", stamp, stamp, request.ID); err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, f.s)
			parsed, parseErr := time.Parse(timestampLayout, stamp)
			valid := parseErr == nil && parsed.Year() >= 1 && parsed.Year() <= 9999 && parsed.Format(timestampLayout) == stamp
			got, err := api.GetDispatchRequest(ctx, f.groupID, request.ID)
			if !valid {
				requestDispatchZero(t, got, err, ErrInvalid)
			} else if err != nil || !got.Request.CreatedAt.Equal(parsed) || !got.State.NextAttemptAt.Equal(parsed) || got.Request.CreatedAt.Location() != time.UTC {
				t.Fatalf("valid timestamp changed: %+v, %v", got, err)
			}
			messageSequencePreserved(t, f.s, before)
		})
	}
}

func TestGetDispatchRequestRejectsPartialReceiptAndMalformedStateTime(t *testing.T) {
	for _, column := range []string{"message_id", "delivery_id", "conversation_id", "delivered_member_id", "materialized_from_generation", "materialized_at", "next_attempt_at", "updated_at", "bad-materialized-at"} {
		t.Run(column, func(t *testing.T) {
			f := requestDispatchSetup(t)
			api := requestDispatchStore(t, f.s)
			request, err := api.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "安排", "read")
			if err != nil {
				t.Fatal(err)
			}
			rows := requestDispatchSavedRows(t, f.s, request.ID)
			coordinatorMustCommit(t, f.s, dispatchUpdate(dispatchReceipt(t, f.s, rows)))
			if _, err := f.s.db.Exec("PRAGMA ignore_check_constraints=ON; DROP TRIGGER coordination_dispatch_state_update;"); err != nil {
				t.Fatal(err)
			}
			value := "NULL"
			if column == "next_attempt_at" || column == "updated_at" || column == "bad-materialized-at" {
				value = "'2026-01-02T03:04:05Z'"
				if column == "bad-materialized-at" {
					column = "materialized_at"
				}
			}
			if _, err := f.s.db.Exec(fmt.Sprintf("UPDATE coordination_dispatch_state SET %s=%s WHERE request_id=?", column, value), request.ID); err != nil {
				t.Fatal(err)
			}
			before := messageSequenceSnapshot(t, f.s)
			got, err := api.GetDispatchRequest(ctx, f.groupID, request.ID)
			requestDispatchZero(t, got, err, ErrInvalid)
			messageSequencePreserved(t, f.s, before)
		})
	}
}
