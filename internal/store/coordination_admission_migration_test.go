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

func TestCoordinationAdmissionFrozenMigrationHistory(t *testing.T) {
	// Frozen SQL and runner fingerprints come from the approved 79c32182 parent.
	want := []string{
		"0c4b8cb8a1b1dde7a98596f45e8c6996d892392deac0481a83bc9f73f8eca64c",
		"15f9694d11b3e60c893dd633e815a97ada9a1b219c621c157eb7eab76dff1b28",
		"6161295e75c2dcd7435d451f2a8359fb1aff445da0209aa2e42f599f25bb8399",
		"a5e0d499caaa68a606af016f72b1022679639eab38594a12759976075a761714",
		"f843e64ea2d1b7611c781f513a102e3d52fbb67317c7a8e23953a2d28a9538e4",
		"d73117e5319acdd6884ff57df747dd67cd2f2958efac5318799cd1677a1ab32c",
		"f68370d3140ed156c84567f4cd99f82381293b656486ba1dd18691cfdf08a77f",
		"f8f82d4a122e630608f5e78760882a1ac025ed38bd6f76edb7013cff1c6f8e5f",
		"efa3930f60093437d002bf41c748f987066821fa6036aacaec60de78e30ee659",
	}
	if len(migrations) < 10 || !strings.Contains(migrations[9], "CREATE TABLE routing_decisions (") {
		t.Fatal("v10 admission migration was not appended")
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

func TestCoordinationAdmissionV9UpgradeReopenAndFutureAppend(t *testing.T) {
	if len(migrations) < 10 {
		t.Fatal("v10 admission migration is missing")
	}
	original := migrations
	migrations = append(append([]string(nil), migrations...), `
CREATE TABLE coordination_admission_existing_future_fixture(id TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE INDEX coordination_admission_existing_future_value ON coordination_admission_existing_future_fixture(value);
`)
	defer func() { migrations = original }()
	old, path, check := messageSequenceV7Fixture(t)
	for version := 8; version <= 9; version++ {
		if _, err := old.db.Exec(migrations[version-1] + fmt.Sprintf("INSERT INTO schema_migrations VALUES(%d);", version)); err != nil {
			t.Fatal(err)
		}
	}
	messageSequenceLedger(t, old, 9)
	var messageID string
	var thread domain.ThreadRef
	if err := old.db.QueryRow("SELECT id,group_id,topic_id FROM messages LIMIT 1").Scan(&messageID, &thread.GroupID, &thread.TopicID); err != nil {
		t.Fatal(err)
	}
	trigger, err := old.RecordLocalMessageTrigger(ctx, thread, messageID)
	if err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, old)
	objects := messageSequenceRows(t, old.db, "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name")
	// Isolate the explicit v10 delta BEFORE Open applies any legitimate future objects.
	if _, err := old.db.Exec(migrations[9] + "INSERT INTO schema_migrations VALUES(10);"); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, old, 10)
	currentObjects := messageSequenceRows(t, old.db, `SELECT type,name,tbl_name,sql FROM sqlite_master
	 WHERE tbl_name NOT IN ('routing_decisions','mandates','task_coordination')
	 AND name<>'coordination_triggers_admission_identity' ORDER BY type,name`)
	if !reflect.DeepEqual(currentObjects, objects) {
		t.Fatalf("v10 changed frozen schema objects: %+v; want %+v", currentObjects, objects)
	}
	newTables := messageSequenceRows(t, old.db, `SELECT name FROM sqlite_master
	 WHERE type='table' AND name IN ('routing_decisions','mandates','task_coordination') ORDER BY name`)
	if !reflect.DeepEqual(newTables, [][]any{{"mandates"}, {"routing_decisions"}, {"task_coordination"}}) {
		t.Fatalf("v10 must add exactly the three admission tables: %v", newTables)
	}
	index := messageSequenceRows(t, old.db, "SELECT name FROM pragma_index_info('coordination_triggers_admission_identity') ORDER BY seqno")
	if !reflect.DeepEqual(index, [][]any{{"id"}, {"group_id"}, {"topic_id"}}) {
		t.Fatalf("v10 trigger scope identity index is missing: %v", index)
	}
	mandateIndex := messageSequenceRows(t, old.db, "SELECT name FROM pragma_index_info('mandates_group_task') ORDER BY seqno")
	if !reflect.DeepEqual(mandateIndex, [][]any{{"group_id"}, {"task_id"}}) {
		t.Fatalf("v10 prior-Mandate scope index is missing: %v", mandateIndex)
	}
	messageSequencePreserved(t, old, before)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s := queueHandle(t, path, false)
	messageSequenceLedger(t, s, len(migrations))
	check(s)
	messageSequencePreserved(t, s, before)
	if got, err := s.RecordLocalMessageTrigger(ctx, thread, messageID); err != nil || !reflect.DeepEqual(got, trigger) {
		t.Fatalf("v10/future upgrade changed the v9 capture: %+v, %v", got, err)
	}
	futureObjects := messageSequenceRows(t, s.db, `SELECT name FROM sqlite_master WHERE name IN
	 ('coordination_admission_existing_future_fixture','coordination_admission_existing_future_value') ORDER BY name`)
	if len(futureObjects) != 2 {
		t.Fatalf("preexisting future migration did not apply on first Open: %v", futureObjects)
	}
	rows := coordinationAdmissionRows(createTestTask(t, s, thread.GroupID), trigger)
	rows.decision[4] = "link_existing"
	if err := coordinationAdmissionCommit(s, rows); err != nil {
		t.Fatal(err)
	}
	committed := messageSequenceSnapshot(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	migrations = append(append([]string(nil), migrations...), "CREATE TABLE coordination_admission_later_future_fixture(id TEXT PRIMARY KEY);")
	for range 2 {
		reopened := queueHandle(t, path, false)
		messageSequenceLedger(t, reopened, len(migrations))
		check(reopened)
		messageSequencePreserved(t, reopened, committed)
		if got, err := reopened.GetTrigger(ctx, thread, trigger.ID); err != nil || !reflect.DeepEqual(got, trigger) {
			t.Fatalf("reopen changed original Trigger: %+v, %v", got, err)
		}
		if tables := messageSequenceRows(t, reopened.db, `SELECT name FROM sqlite_master WHERE type='table'
		 AND name='coordination_admission_later_future_fixture'`); !reflect.DeepEqual(tables, [][]any{{"coordination_admission_later_future_fixture"}}) {
			t.Fatalf("later future append was not applied exactly once: %v", tables)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
