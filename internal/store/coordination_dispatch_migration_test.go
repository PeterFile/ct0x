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

func TestCoordinationDispatchV12MigrationFrozenHistory(t *testing.T) {
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
		"899b3bbe85055d44ecefbb5176c8c98d157ed1edbba10815ba2765bc61f1cc41",
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
		t.Fatalf("frozen runner changed: %s", got)
	}
	if len(migrations) < 12 {
		t.Fatal("v12 dispatch migration was not appended")
	}
}

func dispatchV11Fixture(t *testing.T) (*Store, string, string, string, func(*Store)) {
	t.Helper()
	s, path, groupID, coordinator, worker, check := coordinatorV10Fixture(t)
	if _, err := s.db.Exec(migrations[10] + "INSERT INTO schema_migrations VALUES(11);"); err != nil {
		t.Fatal(err)
	}
	coordinatorMustCommit(t, s,
		coordinatorSQL{coordinatorBindingRevisionInsert, []any{groupID, 1, coordinator}},
		coordinatorSQL{coordinatorPolicyRevisionInsert, []any{groupID, 1, fmt.Sprintf("[%q]", worker)}},
		coordinatorSQL{coordinatorBindingInsert, []any{groupID, 1}},
		coordinatorSQL{coordinatorPolicyInsert, []any{groupID, 1}},
	)
	// A real rejected Task retains its historical Mandate revision 1 under current revision 2.
	rows := dispatchCandidate(t, s, groupID, coordinator, "rejected-before-v12")
	task, err := s.GetTask(ctx, groupID, rows.request[3].(string))
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(task))
	if err != nil {
		t.Fatal(err)
	}
	submit := taskSubmissionInput(attempt)
	submit.IdempotencyKey = "dispatch-legacy-submit"
	submission, err := s.SubmitTask(ctx, submit)
	if err != nil {
		t.Fatal(err)
	}
	review := taskReviewInput(submission)
	review.Outcome, review.IdempotencyKey = domain.ReviewOutcomeRejected, "dispatch-legacy-review"
	if _, err := s.ReviewTask(ctx, review); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, s, 11)
	return s, path, groupID, coordinator, check
}

func TestCoordinationDispatchV12MigrationPopulatedUpgradeReopenAndAppend(t *testing.T) {
	old, path, g, coordinator, check := dispatchV11Fixture(t)
	before := messageSequenceSnapshot(t, old)
	objects := messageSequenceRows(t, old.db, "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name")
	allMigrations := migrations
	migrations = migrations[:12]
	defer func() { migrations = allMigrations }()
	if err := migrate(old.db); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, old, len(migrations))
	migrations = allMigrations
	messageSequencePreserved(t, old, before)
	check(old)
	newObjects := messageSequenceRows(t, old.db, "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name")
	var addedTables []string
	for _, object := range newObjects {
		found := false
		for _, original := range objects {
			if reflect.DeepEqual(object, original) {
				found = true
				break
			}
		}
		if !found {
			switch object[0] {
			case "table":
				addedTables = append(addedTables, object[1].(string))
			case "trigger":
				if object[2] != "deliveries" && object[2] != "coordination_dispatch_requests" && object[2] != "coordination_dispatch_state" {
					t.Fatalf("unexpected v12 parent guard: %v", object)
				}
			case "index":
				if object[2] != "coordination_dispatch_requests" && object[2] != "coordination_dispatch_state" {
					t.Fatalf("v12 added a write index to an old table: %v", object)
				}
			default:
				t.Fatalf("unexpected schema object: %v", object)
			}
		}
	}
	if !reflect.DeepEqual(addedTables, []string{"coordination_dispatch_requests", "coordination_dispatch_state"}) {
		t.Fatalf("v12 must add exactly two typed tables: %v", addedTables)
	}
	for _, original := range objects {
		found := false
		for _, current := range newObjects {
			if reflect.DeepEqual(current, original) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("v12 changed an old schema object: %v", original)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s := queueHandle(t, path, false)
	check(s)
	messageSequencePreserved(t, s, before)
	for _, status := range []string{"pending", "backoff", "held", "enqueued"} {
		rows := dispatchCandidate(t, s, g, coordinator, "append-"+status)
		coordinatorMustCommit(t, s, dispatchPair(rows)...)
		switch status {
		case "backoff":
			rows.state[3], rows.state[5], rows.state[6], rows.state[8] = int64(2), int64(1), "2026-01-02T03:04:07.123456789Z", "sqlite_busy"
			coordinatorMustCommit(t, s, dispatchUpdate(rows.state))
		case "held":
			rows.state[3], rows.state[4], rows.state[9] = int64(2), "held", "no_eligible_worker"
			coordinatorMustCommit(t, s, dispatchUpdate(rows.state))
		case "enqueued":
			coordinatorMustCommit(t, s, dispatchUpdate(dispatchReceipt(t, s, rows)))
		}
	}
	committed := messageSequenceSnapshot(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	original := migrations
	migrations = append(append([]string(nil), migrations...), "CREATE TABLE dispatch_future_fixture(id TEXT PRIMARY KEY, value TEXT NOT NULL);")
	defer func() { migrations = original }()
	var schema [][]any
	for i := range 2 {
		reopened := queueHandle(t, path, false)
		messageSequenceLedger(t, reopened, len(migrations))
		messageSequencePreserved(t, reopened, committed)
		check(reopened)
		dispatchAssertFK(t, reopened)
		current := messageSequenceRows(t, reopened.db, "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name")
		if i == 0 {
			schema = current
		} else if !reflect.DeepEqual(current, schema) {
			t.Fatal("idempotent reopen changed future schema")
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCoordinationDispatchV12MigrationFailureRollbackAndRetry(t *testing.T) {
	old, path, _, _, check := dispatchV11Fixture(t)
	// Collide with the final parent guard after both tables, indexes and earlier guards.
	coordinatorMustCommit(t, old, coordinatorSQL{`CREATE TRIGGER coordination_dispatch_deliveries_no_delete
	 BEFORE DELETE ON deliveries WHEN 0 BEGIN SELECT 1; END;`, nil})
	before := messageSequenceSnapshot(t, old)
	objects := messageSequenceRows(t, old.db, "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name")
	if err := migrate(old.db); err == nil || !strings.Contains(err.Error(), "迁移版本 12") {
		t.Fatalf("v12 late schema failure not exercised: %v", err)
	} else {
		t.Logf("injected v12 failure: %v", err)
	}
	messageSequenceLedger(t, old, 11)
	messageSequencePreserved(t, old, before)
	if got := messageSequenceRows(t, old.db, "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name"); !reflect.DeepEqual(got, objects) {
		t.Fatalf("failed v12 left partial schema: %v; want %v", got, objects)
	}
	check(old)
	coordinatorMustCommit(t, old, coordinatorSQL{"DROP TRIGGER coordination_dispatch_deliveries_no_delete", nil})
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, path, false)
	messageSequenceLedger(t, reopened, len(migrations))
	messageSequencePreserved(t, reopened, before)
	check(reopened)
	dispatchAssertFK(t, reopened)
}
