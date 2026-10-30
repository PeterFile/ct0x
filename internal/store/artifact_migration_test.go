package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func artifactV5Fixture(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v5.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys=ON; CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY);` +
		strings.Join(migrations[:5], "\n") + `INSERT INTO schema_migrations VALUES(1),(2),(3),(4),(5);`); err != nil {
		t.Fatal(err)
	}
	artifactForeignKeys(t, db)
	return &Store{db: db}, path
}

func TestArtifactV5UpgradePreservesExistingReads(t *testing.T) {
	old, path := artifactV5Fixture(t)
	g, err := old.CreateGroup(ctx, keyedGroup())
	if err != nil {
		t.Fatal(err)
	}
	m := member(t, old, g.ID)
	r := MessageRequest{GroupID: g.ID, ActorID: "user", Content: "durable input", IdempotencyKey: "message"}
	message, err := old.CreateMessage(ctx, r, []string{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := old.ClaimTurn(ctx, ClaimTurnInput{GroupID: g.ID, DeliveryID: message.Deliveries[0].ID, ResourceKeys: []string{"workspace:held"}, MaxActive: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec(`UPDATE turns SET state='unknown',native_session_id='native',reason='uncertain',
 receipt_json='{"native":"receipt"}' WHERE id=?`, turn.ID); err != nil {
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
	dependency := createTestTask(t, old, g.ID)
	input := taskInput(g.ID)
	input.DependencyIDs, input.AssigneeID, input.IdempotencyKey = []string{dependency.ID}, m.ID, "task"
	task, err := old.CreateTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		artifactForeignKeys(t, s.db)
		var version, count int
		if err := s.db.QueryRow("SELECT max(version),count(*) FROM schema_migrations").Scan(&version, &count); err != nil || version != 6 || count != 6 {
			t.Fatalf("v6 ledger after upgrade/reopen: %d, %d, %v", version, count, err)
		}
		for _, check := range []struct {
			name string
			want any
			read func() (any, error)
		}{
			{"group", g, func() (any, error) { return s.GetGroup(ctx, g.ID) }},
			{"member", m, func() (any, error) { return s.GetMember(ctx, g.ID, m.ID) }},
			{"message", beforeMessage, func() (any, error) { return s.GetMessage(ctx, g.ID, message.ID) }},
			{"unknown turn", beforeTurn, func() (any, error) { return s.GetTurn(ctx, g.ID, turn.ID) }},
			{"task/dependencies", task, func() (any, error) { return s.GetTask(ctx, g.ID, task.ID) }},
			{"group receipt", g, func() (any, error) { return s.CreateGroup(ctx, keyedGroup()) }},
			{"message receipt", beforeMessage, func() (any, error) { return s.CreateMessage(ctx, r, nil) }},
			{"task receipt", task, func() (any, error) { return s.CreateTask(ctx, input) }},
		} {
			if got, err := check.read(); err != nil || !reflect.DeepEqual(got, check.want) {
				t.Fatalf("v6 changed %s: %+v, %v", check.name, got, err)
			}
		}
		if got, found, err := s.LookupMessageReceipt(ctx, r); err != nil || !found || !reflect.DeepEqual(got, beforeMessage) {
			t.Fatalf("v6 changed receipt lookup: %+v, %t, %v", got, found, err)
		}
		var resourceTurn, receipt string
		if err := s.db.QueryRow("SELECT turn_id FROM resource_claims WHERE resource_key='workspace:held'").Scan(&resourceTurn); err != nil || resourceTurn != turn.ID {
			t.Fatalf("v6 released unknown resource: %q, %v", resourceTurn, err)
		}
		if err := s.db.QueryRow("SELECT receipt_json FROM turns WHERE id=?", turn.ID).Scan(&receipt); err != nil || receipt != `{"native":"receipt"}` {
			t.Fatalf("v6 changed unknown evidence: %q, %v", receipt, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArtifactV6MigrationFailureRollsBackAndRetries(t *testing.T) {
	old, path := artifactV5Fixture(t)
	g := group(t, old)
	// This name belongs to the final v6 index, after its tables and guards.
	if _, err := old.db.Exec("CREATE TABLE artifact_versions_group_created_id(collision TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(old.db); err == nil {
		t.Fatal("colliding v6 unexpectedly succeeded")
	}
	var version, count int
	if err := old.db.QueryRow("SELECT max(version),count(*) FROM schema_migrations").Scan(&version, &count); err != nil || version != 5 || count != 5 {
		t.Fatalf("v6 ledger partially committed: %d, %d, %v", version, count, err)
	}
	if err := old.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'artifact%' AND name<>'artifact_versions_group_created_id'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("v6 DDL partially committed: %d, %v", count, err)
	}
	if got, err := old.GetGroup(ctx, g.ID); err != nil || !reflect.DeepEqual(got, g) {
		t.Fatalf("failed migration changed v5: %+v, %v", got, err)
	}
	if _, err := old.db.Exec("DROP TABLE artifact_versions_group_created_id"); err != nil {
		t.Fatalf("collision object was not preserved: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("clean migration retry: %v", err)
	}
	defer s.Close()
	if err := s.db.QueryRow("SELECT max(version),count(*) FROM schema_migrations").Scan(&version, &count); err != nil || version != 6 || count != 6 {
		t.Fatalf("v6 retry ledger: %d, %d, %v", version, count, err)
	}
	seedArtifact(t, s, g.ID, "artifact", "version", "user")
}
