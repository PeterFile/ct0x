package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func taskV4Fixture(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v4.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys=ON; CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY);` + strings.Join(migrations[:4], "\n") + `INSERT INTO schema_migrations VALUES(1),(2),(3),(4);`); err != nil {
		t.Fatal(err)
	}
	return &Store{db: db}, path
}

func TestTaskV4UpgradePreservesOriginalReads(t *testing.T) {
	old, path := taskV4Fixture(t)
	g, err := old.CreateGroup(ctx, keyedGroup())
	if err != nil {
		t.Fatal(err)
	}
	m := member(t, old, g.ID)
	r := MessageRequest{GroupID: g.ID, ActorID: "user", Content: "durable input", IdempotencyKey: "old-message"}
	message, err := old.CreateMessage(ctx, r, []string{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := old.ClaimTurn(ctx, ClaimTurnInput{GroupID: g.ID, DeliveryID: message.Deliveries[0].ID, ResourceKeys: []string{"workspace:held"}, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	stamp := turn.CreatedAt.Format(timestampLayout)
	if _, err := old.db.Exec(`UPDATE turns SET state='unknown',native_session_id='native',accepted_at=?,cancel_requested_at=?,
 candidate_text='candidate',candidate_terminal='succeeded',receipt_json='{"native":"receipt"}',reason='uncertain' WHERE id=?`, stamp, stamp, turn.ID); err != nil {
		t.Fatal(err)
	}
	beforeTurn, err := old.GetTurn(ctx, g.ID, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeMessage, err := old.GetMessage(ctx, g.ID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version, count int
	if err := s.db.QueryRow("SELECT max(version),count(*) FROM schema_migrations").Scan(&version, &count); err != nil || version != len(migrations) || count != len(migrations) {
		t.Fatalf("v4 upgrade: %d, %d, %v", version, count, err)
	}
	if got, err := s.GetTurn(ctx, g.ID, turn.ID); err != nil || !reflect.DeepEqual(got, beforeTurn) {
		t.Fatalf("v5 changed unknown turn: %+v, %v", got, err)
	}
	if got, err := s.GetMessage(ctx, g.ID, message.ID); err != nil || !reflect.DeepEqual(got, beforeMessage) {
		t.Fatalf("v5 changed original message read: %+v, %v", got, err)
	}
	if got, found, err := s.LookupMessageReceipt(ctx, r); err != nil || !found || !reflect.DeepEqual(got, beforeMessage) {
		t.Fatalf("v5 changed original message receipt: %+v, %v, %v", got, found, err)
	}
	if got, err := s.CreateMessage(ctx, r, nil); err != nil || !reflect.DeepEqual(got, beforeMessage) {
		t.Fatalf("v5 changed message replay: %+v, %v", got, err)
	}
	if got, err := s.CreateGroup(ctx, keyedGroup()); err != nil || !reflect.DeepEqual(got, g) {
		t.Fatalf("v5 changed group receipt: %+v, %v", got, err)
	}
	if got, err := s.GetMember(ctx, g.ID, m.ID); err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("v5 changed member read: %+v, %v", got, err)
	}
	var resourceTurn, receipt string
	if err := s.db.QueryRow("SELECT turn_id FROM resource_claims WHERE resource_key='workspace:held'").Scan(&resourceTurn); err != nil || resourceTurn != turn.ID {
		t.Fatalf("v5 released resource: %q, %v", resourceTurn, err)
	}
	if err := s.db.QueryRow("SELECT receipt_json FROM turns WHERE id=?", turn.ID).Scan(&receipt); err != nil || receipt != `{"native":"receipt"}` {
		t.Fatalf("v5 changed terminal evidence: %q, %v", receipt, err)
	}
	createTestTask(t, s, g.ID)
}

func TestTaskV5MigrationFailureIsAtomic(t *testing.T) {
	old, path := taskV4Fixture(t)
	if _, err := old.db.Exec("CREATE TABLE task_submissions(collision TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("colliding v5 succeeded")
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version, count int
	if err := raw.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != 4 {
		t.Fatalf("v5 ledger partially committed: %d, %v", version, err)
	}
	if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('tasks','task_dependencies','task_attempts','turns_task_identity')").Scan(&count); err != nil || count != 0 {
		t.Fatalf("v5 DDL partially committed: %d, %v", count, err)
	}
}
