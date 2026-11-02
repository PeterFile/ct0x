package store

import (
	"crypto/sha256"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
)

func TestCoordinationTriggerFrozenMigrationHistory(t *testing.T) {
	// Hashes were captured from the approved c8ea145 baseline before implementation.
	want := []string{
		"0c4b8cb8a1b1dde7a98596f45e8c6996d892392deac0481a83bc9f73f8eca64c",
		"15f9694d11b3e60c893dd633e815a97ada9a1b219c621c157eb7eab76dff1b28",
		"6161295e75c2dcd7435d451f2a8359fb1aff445da0209aa2e42f599f25bb8399",
		"a5e0d499caaa68a606af016f72b1022679639eab38594a12759976075a761714",
		"f843e64ea2d1b7611c781f513a102e3d52fbb67317c7a8e23953a2d28a9538e4",
		"d73117e5319acdd6884ff57df747dd67cd2f2958efac5318799cd1677a1ab32c",
		"f68370d3140ed156c84567f4cd99f82381293b656486ba1dd18691cfdf08a77f",
		"f8f82d4a122e630608f5e78760882a1ac025ed38bd6f76edb7013cff1c6f8e5f",
	}
	if len(migrations) < 9 {
		t.Fatal("v9 was not appended")
	}
	for i, hash := range want {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(migrations[i]))); got != hash {
			t.Errorf("frozen v%d SQL changed: %s; want %s", i+1, got, hash)
		}
	}
	source, err := os.ReadFile("migrations.go")
	if err != nil {
		t.Fatal(err)
	}
	_, runner, found := strings.Cut(string(source), "// Migration history")
	if !found {
		t.Fatal("migration runner marker missing")
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte("// Migration history"+runner))); got != "289b8c8bf09c1f8cb4e59a43b10eeb75020791a00759d9617b57cc400a84e4c5" {
		t.Fatalf("frozen migration runner changed: %s", got)
	}
}

func TestCoordinationTriggerV8UpgradeReopenAndFutureAppend(t *testing.T) {
	// A valid future migration already exists before the first upgrade/open.
	original := migrations
	migrations = append(append([]string(nil), migrations...), `
CREATE TABLE coordination_trigger_existing_future_fixture(id TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE INDEX coordination_trigger_existing_future_value ON coordination_trigger_existing_future_fixture(value);
`)
	defer func() { migrations = original }()
	old, path, check := messageSequenceV7Fixture(t)
	if _, err := old.db.Exec(migrations[7] + "INSERT INTO schema_migrations VALUES(8);"); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, old, 8)
	before := messageSequenceSnapshot(t, old)
	objects := messageSequenceRows(t, old.db, "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name")
	var messageID, groupID, topicID string
	if err := old.db.QueryRow("SELECT id,group_id,topic_id FROM messages LIMIT 1").Scan(&messageID, &groupID, &topicID); err != nil {
		t.Fatal(err)
	}
	// Isolate v9's schema changes from legitimate objects added by later migrations.
	if _, err := old.db.Exec(migrations[8] + "INSERT INTO schema_migrations VALUES(9);"); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, old, 9)
	// v9 may add objects only to coordination_triggers, not indexes or guards to v8 tables.
	currentObjects := messageSequenceRows(t, old.db, "SELECT type,name,tbl_name,sql FROM sqlite_master WHERE tbl_name<>'coordination_triggers' ORDER BY type,name")
	if !reflect.DeepEqual(currentObjects, objects) {
		t.Fatalf("v9 modified existing schema objects: %+v; want %+v", currentObjects, objects)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s := queueHandle(t, path, false)
	messageSequenceLedger(t, s, len(migrations))
	check(s)
	messageSequencePreserved(t, s, before)
	coordinationTriggerCount(t, s, 0)
	var futureObjects int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master
	 WHERE (type='table' AND name='coordination_trigger_existing_future_fixture')
	 OR (type='index' AND name='coordination_trigger_existing_future_value')`).Scan(&futureObjects); err != nil || futureObjects != 2 {
		t.Fatalf("preexisting future migration was not applied on first open: %d objects, %v", futureObjects, err)
	}
	thread := domain.ThreadRef{GroupID: groupID, TopicID: topicID}
	trigger, err := s.RecordLocalMessageTrigger(ctx, thread, messageID)
	if err != nil || trigger.MessageSequence <= 0 {
		t.Fatalf("upgraded source is not capturable: %+v, %v", trigger, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Also append another migration after capture to exercise Trigger replay across upgrades.
	migrations = append(append([]string(nil), migrations...), "CREATE TABLE coordination_trigger_future_fixture(id TEXT PRIMARY KEY);")
	for range 2 {
		reopened := queueHandle(t, path, false)
		messageSequenceLedger(t, reopened, len(migrations))
		check(reopened)
		messageSequencePreserved(t, reopened, before)
		if got, err := reopened.RecordLocalMessageTrigger(ctx, thread, messageID); err != nil || !reflect.DeepEqual(got, trigger) {
			t.Fatalf("future migration/reopen changed capture: %+v, %v", got, err)
		}
		var exists int
		if err := reopened.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='coordination_trigger_future_fixture'").Scan(&exists); err != nil || exists != 1 {
			t.Fatalf("future append was not applied exactly once: %d, %v", exists, err)
		}
		coordinationTriggerCount(t, reopened, 1)
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
