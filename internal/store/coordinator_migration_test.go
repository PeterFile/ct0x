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

func TestCoordinatorFrozenMigrationHistory(t *testing.T) {
	// Frozen fingerprints were captured from the approved 87b142d5 parent.
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
		"4b6c8c8252d7968a083fdffb19fa0216a17813e90716b485d6b64c78eb3d27fe",
	}
	if len(migrations) < 11 || migrations[10] != coordinatorSchemaV11 {
		t.Fatal("v11 coordinator migration was not appended")
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

func coordinatorV10Fixture(t *testing.T) (*Store, string, string, string, string, func(*Store)) {
	t.Helper()
	old, path, check := messageSequenceV7Fixture(t)
	for version := 8; version <= 10; version++ {
		if _, err := old.db.Exec(migrations[version-1] + fmt.Sprintf("INSERT INTO schema_migrations VALUES(%d);", version)); err != nil {
			t.Fatal(err)
		}
	}
	messageSequenceLedger(t, old, 10)
	var groupID, unknownMember string
	if err := old.db.QueryRow("SELECT group_id,member_id FROM turns WHERE state='unknown'").Scan(&groupID, &unknownMember); err != nil {
		t.Fatal(err)
	}
	trigger := coordinationAdmissionSource(t, old, domain.ThreadRef{GroupID: groupID, TopicID: "coordinator-migration"})
	rows := coordinationAdmissionRows(createTestTask(t, old, groupID), trigger)
	if err := coordinationAdmissionCommit(old, rows); err != nil {
		t.Fatal(err)
	}
	cancelMember := member(t, old, groupID)
	delivery := queueDelivery(t, old, groupID, cancelMember.ID, "cancel-requested")
	turn, err := old.ClaimTurn(ctx, ClaimTurnInput{GroupID: groupID, DeliveryID: delivery.ID, ResourceKeys: []string{"coordinator-cancel-held"}, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := old.RequestTurnCancel(ctx, groupID, turn.ID)
	if err != nil || cancelled.State != domain.TurnStateCancelRequested {
		t.Fatalf("fixture must contain a real cancel_requested Turn: %+v, %v", cancelled, err)
	}
	return old, path, groupID, unknownMember, cancelMember.ID, func(s *Store) {
		t.Helper()
		check(s) // Real unknown outcome, private candidate, receipts and retained resource claim.
		got, err := s.GetTurn(ctx, groupID, turn.ID)
		if err != nil || !reflect.DeepEqual(got, cancelled) {
			t.Fatalf("migration/configuration changed cancel_requested Turn: %+v, %v", got, err)
		}
		var held string
		if err := s.db.QueryRow("SELECT turn_id FROM resource_claims WHERE resource_key='coordinator-cancel-held'").Scan(&held); err != nil || held != turn.ID {
			t.Fatalf("migration/configuration released cancellation's resource: %q, %v", held, err)
		}
	}
}

func TestCoordinatorV10UpgradeReopenAndFutureAppend(t *testing.T) {
	original := migrations
	migrations = append(append([]string(nil), migrations...), `
CREATE TABLE coordinator_existing_future_fixture(id TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE INDEX coordinator_existing_future_value ON coordinator_existing_future_fixture(value);
`)
	defer func() { migrations = original }()
	old, path, groupID, a, b, check := coordinatorV10Fixture(t)
	before := messageSequenceSnapshot(t, old)
	objects := messageSequenceRows(t, old.db, "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name")
	// Compare only v11's delta, before Open applies legitimate later migrations.
	if _, err := old.db.Exec(migrations[10] + "INSERT INTO schema_migrations VALUES(11);"); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, old, 11)
	currentObjects := messageSequenceRows(t, old.db, `SELECT type,name,tbl_name,sql FROM sqlite_master WHERE tbl_name NOT IN
	 ('group_coordinator_binding_revisions','group_dispatch_policy_revisions','group_coordinator_bindings','group_dispatch_policies')
	 ORDER BY type,name`)
	if !reflect.DeepEqual(currentObjects, objects) {
		t.Fatalf("v11 modified frozen schema objects: %+v; want %+v", currentObjects, objects)
	}
	newTables := messageSequenceRows(t, old.db, `SELECT name FROM sqlite_master WHERE type='table' AND name IN
	 ('group_coordinator_binding_revisions','group_dispatch_policy_revisions','group_coordinator_bindings','group_dispatch_policies') ORDER BY name`)
	if !reflect.DeepEqual(newTables, [][]any{{"group_coordinator_binding_revisions"}, {"group_coordinator_bindings"}, {"group_dispatch_policies"}, {"group_dispatch_policy_revisions"}}) {
		t.Fatalf("v11 must add exactly the four typed tables: %v", newTables)
	}
	coordinatorEmpty(t, old)
	messageSequencePreserved(t, old, before)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s := queueHandle(t, path, false)
	messageSequenceLedger(t, s, len(migrations))
	check(s)
	messageSequencePreserved(t, s, before)
	coordinatorEmpty(t, s)
	if got := messageSequenceRows(t, s.db, `SELECT name FROM sqlite_master WHERE name IN
	 ('coordinator_existing_future_fixture','coordinator_existing_future_value')`); len(got) != 2 {
		t.Fatalf("preexisting future migration did not apply: %v", got)
	}
	coordinatorMustCommit(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{groupID, 1, a}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{groupID, 1, fmt.Sprintf("[%q]", b)}},
		coordinatorSQL{coordinatorBindingInsert, []any{groupID, 1}},
		coordinatorSQL{coordinatorPolicyInsert, []any{groupID, 1}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{groupID, 2, "[]"}},
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{groupID, 2, b}},
		coordinatorSQL{"UPDATE group_dispatch_policies SET revision=2 WHERE group_id=?", []any{groupID}},
		coordinatorSQL{"UPDATE group_coordinator_bindings SET revision=2 WHERE group_id=?", []any{groupID}},
	)
	messageSequencePreserved(t, s, before) // Includes messages/log, Tasks, Mandates, receipts, Deliveries, Turns and locks.
	check(s)
	committed := messageSequenceSnapshot(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	migrations = append(append([]string(nil), migrations...), "CREATE TABLE coordinator_later_future_fixture(id TEXT PRIMARY KEY);")
	var reopenedObjects [][]any
	for i := range 2 {
		reopened := queueHandle(t, path, false)
		messageSequenceLedger(t, reopened, len(migrations))
		messageSequencePreserved(t, reopened, committed)
		check(reopened)
		objects := messageSequenceRows(t, reopened.db, "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name")
		if i == 0 {
			reopenedObjects = objects
		} else if !reflect.DeepEqual(objects, reopenedObjects) {
			t.Fatal("idempotent reopen changed schema objects")
		}
		if got := messageSequenceRows(t, reopened.db, "SELECT name FROM sqlite_master WHERE name='coordinator_later_future_fixture'"); len(got) != 1 {
			t.Fatalf("later future migration was not applied exactly once: %v", got)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCoordinatorV11MigrationFailureRollsBackAndRetries(t *testing.T) {
	old, path, _, _, _, check := coordinatorV10Fixture(t)
	// Collide with the fourth table, after the first three v11 tables were created.
	coordinatorMustCommit(t, old, coordinatorSQL{"CREATE TABLE group_dispatch_policies(collision TEXT); INSERT INTO group_dispatch_policies VALUES('preserved');", nil})
	before := messageSequenceSnapshot(t, old)
	objects := messageSequenceRows(t, old.db, "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name")
	if err := migrate(old.db); err == nil || !strings.Contains(err.Error(), "迁移版本 11") {
		t.Fatalf("v11 collision did not exercise migration rollback: %v", err)
	}
	messageSequenceLedger(t, old, 10)
	messageSequencePreserved(t, old, before)
	if got := messageSequenceRows(t, old.db, "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name"); !reflect.DeepEqual(got, objects) {
		t.Fatalf("failed v11 left partial schema objects: %v; want %v", got, objects)
	}
	check(old)
	coordinatorMustCommit(t, old, coordinatorSQL{"DROP TABLE group_dispatch_policies", nil})
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s := queueHandle(t, path, false)
	messageSequenceLedger(t, s, len(migrations))
	coordinatorEmpty(t, s)
	check(s)
}
