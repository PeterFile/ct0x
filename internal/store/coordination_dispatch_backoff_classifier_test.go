package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func dispatchTransientRequire(t *testing.T, err error, exact int, want DispatchTransientCode) {
	t.Helper()
	var cause *sqlite.Error
	if !errors.As(err, &cause) || cause.Code() != exact {
		t.Fatalf("not a real SQLite code %d: %v", exact, err)
	}
	wrapped := fmt.Errorf("外层存储操作：%w", err)
	code, ok := ClassifyDispatchTransient(wrapped)
	if !ok || code != want {
		t.Fatalf("real/wrapped SQLite failure was not classified: %v => %q, %t", wrapped, code, ok)
	}
	for _, cancelled := range []error{context.Canceled, context.DeadlineExceeded} {
		if code, ok := ClassifyDispatchTransient(errors.Join(wrapped, fmt.Errorf("已取消：%w", cancelled))); ok || code != "" {
			t.Fatalf("context failure was overridden by a nested SQLite cause: %q, %t", code, ok)
		}
	}
}

func TestDispatchTransientRealSQLiteBusyRollbackBeforeDefer(t *testing.T) {
	f, r := materializePending(t)
	second := queueHandle(t, f.path, false)
	if _, err := second.db.Exec("PRAGMA busy_timeout=0"); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	tx, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, materializeErr := materializeAPI(t, second).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	requestDispatchZero(t, got, materializeErr, nil)
	dispatchTransientRequire(t, materializeErr, sqlite3.SQLITE_BUSY, DispatchTransientSQLiteBusy)
	// Defer must report failure while SQLite itself cannot persist the new transaction.
	err = deferDispatchStore(t, second).DeferDispatchRequest(ctx, r.ID, 1, materializeAt, DispatchTransientSQLiteBusy)
	dispatchTransientRequire(t, err, sqlite3.SQLITE_BUSY, DispatchTransientSQLiteBusy)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	messageSequencePreserved(t, f.s, before)
	code, ok := ClassifyDispatchTransient(materializeErr)
	if !ok {
		t.Fatal("lost transient evidence after rollback")
	}
	if err := deferDispatchStore(t, second).DeferDispatchRequest(ctx, r.ID, 1, materializeAt, code); err != nil {
		t.Fatal(err)
	}
	delete(before, "coordination_dispatch_state")
	messageSequencePreserved(t, second, before)
}

func TestDispatchTransientRealSQLiteLocked(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "locked.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "CREATE TABLE locked_scan(id); INSERT INTO locked_scan VALUES(1),(2)"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(ctx, "SELECT id FROM locked_scan")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("missing real active SQLite cursor")
	}
	_, locked := conn.ExecContext(ctx, "DROP TABLE locked_scan")
	dispatchTransientRequire(t, locked, sqlite3.SQLITE_LOCKED, DispatchTransientSQLiteLocked)
}

func TestDispatchTransientExtendedLockedVirtualTable(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "rtree.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "CREATE VIRTUAL TABLE locked_tree USING rtree(id,minX,maxX); INSERT INTO locked_tree VALUES(1,0,10),(2,10,20)"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(ctx, "SELECT id FROM locked_tree WHERE minX>=0")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("missing real active R-tree cursor")
	}
	_, locked := conn.ExecContext(ctx, "INSERT INTO locked_tree VALUES(3,20,30)")
	dispatchTransientRequire(t, locked, sqlite3.SQLITE_LOCKED_VTAB, DispatchTransientSQLiteLocked)
}

func TestDispatchTransientExtendedBusySnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.sqlite")
	first := queueHandle(t, path, true)
	second := queueHandle(t, path, true)
	if _, err := first.db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE snapshot_write(id PRIMARY KEY,v); INSERT INTO snapshot_write VALUES(1,0)"); err != nil {
		t.Fatal(err)
	}
	tx, err := first.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var value int
	if err := tx.QueryRow("SELECT v FROM snapshot_write WHERE id=1").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if _, err := second.db.Exec("UPDATE snapshot_write SET v=1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	_, busy := tx.Exec("UPDATE snapshot_write SET v=2 WHERE id=1")
	dispatchTransientRequire(t, busy, sqlite3.SQLITE_BUSY_SNAPSHOT, DispatchTransientSQLiteBusy)
}

type dispatchNativeUnknownError struct{}

func (dispatchNativeUnknownError) Error() string { return "native accepted/unknown: SQLITE_BUSY (5)" }

func TestDispatchTransientRejectsStringsConstraintNativeAndContext(t *testing.T) {
	s, _ := openTestStore(t)
	_, constraint := s.db.Exec("INSERT INTO groups(id,name,description,created_at) VALUES(NULL,'x','x','x')")
	var cause *sqlite.Error
	if !errors.As(constraint, &cause) || cause.Code()&0xff != sqlite3.SQLITE_CONSTRAINT {
		t.Fatalf("expected a real SQLite constraint: %v", constraint)
	}
	for _, err := range []error{nil, errors.New("database is locked (SQLITE_BUSY)"), errors.New("SQLITE_LOCKED (6)"), constraint, dispatchNativeUnknownError{}, context.Canceled, fmt.Errorf("timeout: %w", context.DeadlineExceeded)} {
		if code, ok := ClassifyDispatchTransient(err); ok || code != "" {
			t.Fatalf("non-storage transient was accepted: %v => %q, %t", err, code, ok)
		}
	}
}

func TestDeferDispatchHeldNeverRetriesAfterMembershipRestored(t *testing.T) {
	f, r := materializePending(t)
	materializeChangeEligibility(t, f, "coordinator-left")
	got, err := materializeAPI(t, f.s).MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	if err != nil || got.Disposition != "held" {
		t.Fatalf("fixture did not hold: %+v, %v", got, err)
	}
	if _, err := f.s.db.Exec("UPDATE members SET status='active' WHERE id=?", f.coord); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	for _, generation := range []int64{1, 2} {
		if err := deferDispatchStore(t, f.s).DeferDispatchRequest(ctx, r.ID, generation, materializeAt, DispatchTransientSQLiteBusy); !errors.Is(err, ErrConflict) {
			t.Fatalf("held was rescheduled: %v", err)
		}
	}
	page, err := dispatchDueStore(t, f.s).ListDueDispatchRequests(ctx, materializeAt.Add(365*24*time.Hour), DispatchDueOptions{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("held reentered the due queue: %+v, %v", page, err)
	}
	messageSequencePreserved(t, f.s, before)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	if err := deferDispatchStore(t, reopened).DeferDispatchRequest(ctx, r.ID, 2, materializeAt, DispatchTransientSQLiteLocked); !errors.Is(err, ErrConflict) {
		t.Fatalf("reopen automatically retried held: %v", err)
	}
	got, err = materializeAPI(t, reopened).MaterializeDispatchRequest(ctx, r.ID, 2, materializeAt)
	requestDispatchZero(t, got, err, ErrConflict)
	_, err = reopened.RequestDispatch(ctx, r.GroupID, r.TaskID, 1, 1, "不能绕过暂停请求", "new-key")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("restored membership released the held Task slot: %v", err)
	}
	messageSequencePreserved(t, reopened, before)
}
