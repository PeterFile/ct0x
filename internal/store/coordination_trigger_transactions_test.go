package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestCoordinationTriggerInTxOwnsNoTransactionBoundary(t *testing.T) {
	s, path := openTestStore(t)
	observer := queueHandle(t, path, false)
	thread := domain.ThreadRef{GroupID: group(t, s).ID}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(bounded, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	message := domain.Message{ID: "outer-transaction-message", GroupID: thread.GroupID, SenderID: "user", Content: "same transaction", CreatedAt: time.Now().UTC()}
	if err := insertMessageRow(bounded, tx, message); err != nil {
		t.Fatal(err)
	}
	trigger, err := recordLocalMessageTriggerInTx(bounded, tx, thread, message.ID)
	if err != nil || trigger.MessageID != message.ID || trigger.MessageSequence <= 0 {
		t.Fatalf("helper cannot see caller's uncommitted message/log: %+v, %v", trigger, err)
	}
	if got, err := recordLocalMessageTriggerInTx(bounded, tx, thread, message.ID); err != nil || !reflect.DeepEqual(got, trigger) {
		t.Fatalf("uncommitted helper replay: %+v, %v", got, err)
	}
	if _, err := recordLocalMessageTriggerInTx(bounded, tx, thread, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("helper validation: %v", err)
	}
	var alive int
	if err := tx.QueryRowContext(bounded, "SELECT count(*) FROM coordination_triggers").Scan(&alive); err != nil || alive != 1 {
		t.Fatalf("helper ended caller transaction: %d, %v", alive, err)
	}
	coordinationTriggerCount(t, observer, 0)
	if _, err := observer.GetTrigger(ctx, thread, trigger.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("helper committed caller state: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	coordinationTriggerCount(t, s, 0)
	if _, err := s.GetMessage(ctx, thread.GroupID, message.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outer rollback left source message: %v", err)
	}
	// A later outer transaction may commit both rows; the helper must not commit early.
	tx, err = s.db.BeginTx(bounded, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := insertMessageRow(bounded, tx, message); err != nil {
		t.Fatal(err)
	}
	trigger, err = recordLocalMessageTriggerInTx(bounded, tx, thread, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	coordinationTriggerCount(t, observer, 0)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := observer.GetTrigger(ctx, thread, trigger.ID); err != nil || !reflect.DeepEqual(got, trigger) {
		t.Fatalf("caller commit did not persist capture: %+v, %v", got, err)
	}
}

func TestCoordinationTriggerStatementAndCommitFailuresRollback(t *testing.T) {
	for _, failure := range []string{"statement", "commit", "ignored-insert"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := openTestStore(t)
			thread := domain.ThreadRef{GroupID: group(t, s).ID}
			message := coordinationTriggerMessage(t, s, thread)
			before := messageSequenceSnapshot(t, s)
			body, timing := "SELECT RAISE(ABORT,'注入语句失败');", "AFTER"
			if failure == "commit" {
				if _, err := s.db.Exec("CREATE TABLE coordination_trigger_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)"); err != nil {
					t.Fatal(err)
				}
				body = "INSERT INTO coordination_trigger_commit_guard VALUES('missing');"
			} else if failure == "ignored-insert" {
				body, timing = "SELECT RAISE(IGNORE);", "BEFORE"
			}
			if _, err := s.db.Exec("CREATE TRIGGER coordination_trigger_fail " + timing + " INSERT ON coordination_triggers BEGIN " + body + " END"); err != nil {
				t.Fatal(err)
			}
			if got, err := s.RecordLocalMessageTrigger(ctx, thread, message.ID); err == nil || got != (domain.Trigger{}) {
				t.Fatalf("failed %s returned success/partial evidence: %+v, %v", failure, got, err)
			}
			coordinationTriggerCount(t, s, 0)
			messageSequencePreserved(t, s, before)
			if failure == "commit" {
				var leaked int
				if err := s.db.QueryRow("SELECT count(*) FROM coordination_trigger_commit_guard").Scan(&leaked); err != nil || leaked != 0 {
					t.Fatalf("failed commit leaked deferred row: %d, %v", leaked, err)
				}
			}
			if _, err := s.db.Exec("DROP TRIGGER coordination_trigger_fail"); err != nil {
				t.Fatal(err)
			}
			if got, err := s.RecordLocalMessageTrigger(ctx, thread, message.ID); err != nil || got.ID == "" {
				t.Fatalf("failed write prevented clean retry: %+v, %v", got, err)
			}
			coordinationTriggerCount(t, s, 1)
		})
	}
}
