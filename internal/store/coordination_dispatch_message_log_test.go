package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"modernc.org/sqlite"
)

func TestMaterializeDispatchMessageLog(t *testing.T) {
	t.Run("normal_and_claimed_replay", func(t *testing.T) {
		f, r := materializePending(t)
		observer := queueHandle(t, f.path, false)
		before := messageSequenceSnapshot(t, observer)
		got, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
		if err != nil || got.Delivery == nil {
			t.Fatalf("normal materialization: %+v, %v", got, err)
		}
		materializeAssertReceipt(t, observer, r, got, 1, materializeAt)
		message, err := observer.GetMessage(ctx, r.GroupID, got.Delivery.MessageID)
		if err != nil || message.Content != materializeWantBody(r, f.admission.Mandate.Scope.Objective, f.admission.Mandate.Scope.Acceptance) || len(message.Deliveries) != 1 || message.Deliveries[0].State != domain.DeliveryStateQueued {
			t.Fatalf("normal message/delivery: %+v, %v", message, err)
		}
		var logs, matched int
		var sequence int64
		if err := observer.db.QueryRow(`SELECT count(*),coalesce(max(sequence),0),coalesce(sum(group_id=? AND topic_id=?),0)
		 FROM message_log WHERE message_id=?`, r.GroupID, r.TopicID, got.Delivery.MessageID).Scan(&logs, &sequence, &matched); err != nil {
			t.Fatal(err)
		}
		after := messageSequenceSnapshot(t, observer)
		if logs != 1 || matched != 1 || sequence <= 0 || len(after["messages"]) != len(before["messages"])+1 || len(after["message_log"]) != len(before["message_log"])+1 || len(after["deliveries"]) != len(before["deliveries"])+1 {
			t.Fatalf("normal materialization lacks one exact positive log mapping: count=%d matching=%d sequence=%d", logs, matched, sequence)
		}
		turn, err := observer.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: got.Delivery.DeliveryID, MaxActive: 8})
		if err != nil || turn.ID == "" {
			t.Fatalf("claim normal delivery: %+v, %v", turn, err)
		}
		message, err = observer.GetMessage(ctx, r.GroupID, got.Delivery.MessageID)
		if err != nil || len(message.Deliveries) != 1 || message.Deliveries[0].State != domain.DeliveryStateQueued || message.Deliveries[0].TurnID != turn.ID {
			t.Fatalf("delivery claim was not persisted: %+v, %v", message, err)
		}
		claimed := messageSequenceSnapshot(t, observer)
		claimed["sqlite_sequence"] = messageSequenceTableRows(t, observer, "sqlite_sequence")
		replayed, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt.Add(time.Hour))
		if err != nil || !reflect.DeepEqual(replayed, got) {
			t.Fatalf("same-generation claimed receipt replay: %+v, %v", replayed, err)
		}
		messageSequencePreserved(t, observer, claimed)
		if err := f.s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := observer.Close(); err != nil {
			t.Fatal(err)
		}
		reopened := queueHandle(t, f.path, false)
		replayed, err = reopened.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt.Add(2*time.Hour))
		if err != nil || !reflect.DeepEqual(replayed, got) {
			t.Fatalf("reopened claimed receipt replay: %+v, %v", replayed, err)
		}
		messageSequencePreserved(t, reopened, claimed)
	})

	t.Run("ignored_message_log_insert", func(t *testing.T) {
		f, r := materializePending(t)
		observer := queueHandle(t, f.path, false)
		if _, err := f.s.db.Exec(`CREATE TABLE n2_log_fault_hits(message_id TEXT NOT NULL);
		 CREATE TRIGGER n2_log_fault BEFORE INSERT ON message_log BEGIN
		 INSERT INTO n2_log_fault_hits(message_id) VALUES(NEW.message_id);
		 SELECT RAISE(IGNORE); END`); err != nil {
			t.Fatal(err)
		}
		before := messageSequenceSnapshot(t, observer)
		before["schema_migrations"] = messageSequenceTableRows(t, observer, "schema_migrations")
		before["sqlite_sequence"] = messageSequenceTableRows(t, observer, "sqlite_sequence")
		beforeView, err := observer.GetDispatchRequest(ctx, r.GroupID, r.ID)
		if err != nil || beforeView.State.Status != domain.DispatchStatusPending || beforeView.State.Generation != 1 || beforeView.State.Delivery != nil {
			t.Fatalf("bad initial generation: %+v, %v", beforeView, err)
		}
		// Calibrate the trigger without retaining the injected write or hit.
		tx, err := f.s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		probe := domain.Message{ID: "n2-log-calibration", GroupID: r.GroupID, TopicID: r.TopicID, SenderID: "user", Content: "local fault calibration", CreatedAt: materializeAt}
		if err := insertMessageRow(ctx, tx, probe); err != nil {
			t.Fatal(err)
		}
		var probeMessages, probeLogs, probeHits int
		if err := tx.QueryRow(`SELECT (SELECT count(*) FROM messages WHERE id=?),
		 (SELECT count(*) FROM message_log WHERE message_id=?),
		 (SELECT count(*) FROM n2_log_fault_hits WHERE message_id=?)`, probe.ID, probe.ID, probe.ID).Scan(&probeMessages, &probeLogs, &probeHits); err != nil {
			t.Fatal(err)
		}
		if probeMessages != 1 || probeLogs != 0 || probeHits != 1 {
			t.Fatalf("invalid fault fixture: message=%d log=%d hits=%d", probeMessages, probeLogs, probeHits)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		messageSequencePreserved(t, observer, before)
		messageSequencePreserved(t, f.s, before)

		got, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
		requestDispatchZero(t, got, err, ErrConflict)
		messageSequencePreserved(t, observer, before)
		messageSequencePreserved(t, f.s, before)
		view, err := observer.GetDispatchRequest(ctx, r.GroupID, r.ID)
		if err != nil || !reflect.DeepEqual(view, beforeView) {
			t.Fatalf("failed materialization changed request/state: %+v, %v", view, err)
		}
		if err := f.s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := observer.Close(); err != nil {
			t.Fatal(err)
		}
		reopened := queueHandle(t, f.path, false)
		messageSequencePreserved(t, reopened, before)
		view, err = reopened.GetDispatchRequest(ctx, r.GroupID, r.ID)
		if err != nil || !reflect.DeepEqual(view, beforeView) {
			t.Fatalf("reopened rollback changed request/state: %+v, %v", view, err)
		}
		if _, err := reopened.db.Exec("DROP TRIGGER n2_log_fault"); err != nil {
			t.Fatal(err)
		}
		retried, err := reopened.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
		if err != nil || retried.Delivery == nil {
			t.Fatalf("original generation was not retryable: %+v, %v", retried, err)
		}
		materializeAssertReceipt(t, reopened, r, retried, 1, materializeAt)
		message, err := reopened.GetMessage(ctx, r.GroupID, retried.Delivery.MessageID)
		if err != nil || message.Content != materializeWantBody(r, f.admission.Mandate.Scope.Objective, f.admission.Mandate.Scope.Acceptance) || len(message.Deliveries) != 1 || message.Deliveries[0].State != domain.DeliveryStateQueued {
			t.Fatalf("retry message/delivery: %+v, %v", message, err)
		}
		var logs int
		if err := reopened.db.QueryRow("SELECT count(*) FROM message_log WHERE message_id=? AND group_id=? AND topic_id=? AND sequence>0", retried.Delivery.MessageID, r.GroupID, r.TopicID).Scan(&logs); err != nil || logs != 1 {
			t.Fatalf("retry lacks exact positive message log: count=%d err=%v", logs, err)
		}
		after := messageSequenceSnapshot(t, reopened)
		after["schema_migrations"] = messageSequenceTableRows(t, reopened, "schema_migrations")
		after["sqlite_sequence"] = messageSequenceTableRows(t, reopened, "sqlite_sequence")
		for table, rows := range before {
			switch table {
			case "messages", "message_log", "deliveries", "conversations":
				if len(after[table]) != len(rows)+1 {
					t.Fatalf("retry did not write exactly one %s row", table)
				}
			case "coordination_dispatch_state", "sqlite_sequence":
			default:
				if !reflect.DeepEqual(after[table], rows) {
					t.Fatalf("retry changed unrelated %s rows", table)
				}
			}
		}
		replayed, err := reopened.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt.Add(time.Hour))
		if err != nil || !reflect.DeepEqual(replayed, retried) {
			t.Fatalf("retry receipt did not replay: %+v, %v", replayed, err)
		}
		messageSequencePreserved(t, reopened, after)
		dispatchAssertFK(t, reopened)
	})
}

func TestMaterializeDispatchMessageLogReadback(t *testing.T) {
	f, r := materializePending(t)
	got, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	if err != nil || got.Delivery == nil {
		t.Fatalf("normal materialization: %+v, %v", got, err)
	}
	observer := queueHandle(t, f.path, false)
	before := messageSequenceSnapshot(t, observer)
	before["sqlite_sequence"] = messageSequenceTableRows(t, observer, "sqlite_sequence")
	body := materializeWantBody(r, f.admission.Mandate.Scope.Objective, f.admission.Mandate.Scope.Acceptance)
	for _, tc := range []struct {
		name, projection string
		sqlError         bool
	}{
		{"missing", "SELECT * FROM main.message_log WHERE 0", false},
		{"message", "SELECT sequence,'wrong' AS message_id,group_id,topic_id FROM main.message_log", false},
		{"group", "SELECT sequence,message_id,'wrong' AS group_id,topic_id FROM main.message_log", false},
		{"topic", "SELECT sequence,message_id,group_id,'wrong' AS topic_id FROM main.message_log", false},
		{"zero-sequence", "SELECT 0 AS sequence,message_id,group_id,topic_id FROM main.message_log", false},
		{"negative-sequence", "SELECT -1 AS sequence,message_id,group_id,topic_id FROM main.message_log", false},
		{"sql-error", "SELECT sequence,message_id,group_id FROM main.message_log", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := coordinationDispatchMaterializedInTx(ctx, tx, r, *got.Delivery, body); err != nil {
				t.Fatalf("valid readback rejected: %v", err)
			}
			// Shadow only this transaction's readback; main-table constraints stay intact.
			if _, err := tx.Exec("CREATE TEMP VIEW message_log AS " + tc.projection); err != nil {
				t.Fatal(err)
			}
			err = coordinationDispatchMaterializedInTx(ctx, tx, r, *got.Delivery, body)
			if tc.sqlError {
				var cause *sqlite.Error
				if !errors.As(err, &cause) || errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
					t.Fatalf("readback lost SQL cause: %v", err)
				}
			} else if !errors.Is(err, ErrConflict) {
				t.Fatalf("invalid log mapping was not rejected: %v", err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			messageSequencePreserved(t, observer, before)
			messageSequencePreserved(t, f.s, before)
		})
	}
}
