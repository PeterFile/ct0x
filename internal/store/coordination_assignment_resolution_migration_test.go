package store

import (
	"crypto/sha256"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/provider"
)

func resolutionV13Fixture(t *testing.T) (*Store, string, func(*Store)) {
	t.Helper()
	s, path, check := assignmentV12Fixture(t)
	original := migrations
	migrations = original[:13]
	defer func() { migrations = original }()
	if err := migrate(s.db); err != nil {
		t.Fatal(err)
	}
	messageSequenceLedger(t, s, 13)
	return s, path, check
}

func TestCoordinationAssignmentResolutionV14PopulatedUpgrade(t *testing.T) {
	old, path, check := resolutionV13Fixture(t)
	before := assignmentSnapshotOriginalTables(t, old, messageSequenceSnapshot(t, old))
	sequence := messageSequenceTableRows(t, old, "sqlite_sequence")
	oldObjects := messageSequenceRows(t, old.db, assignmentSchemaQuery)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	var finalObjects [][]any
	for i := range 3 {
		s := resolutionOpen(t, path)
		if err := assignmentCheckOriginalTables(t, s, before); err != nil {
			t.Fatal(err)
		}
		check(s)
		if got := messageSequenceTableRows(t, s, "sqlite_sequence"); !reflect.DeepEqual(got, sequence) {
			t.Fatalf("sqlite_sequence highwater changed: %v; want %v", got, sequence)
		}
		dispatchAssertFK(t, s)
		current := messageSequenceRows(t, s.db, assignmentSchemaQuery)
		if i == 0 {
			finalObjects = current
			for _, object := range oldObjects {
				if slices.Contains([]any{"coordination_dispatch_requests", "coordination_dispatch_state", "deliveries"}, object[2]) || object[1] == "coordination_assignments_disabled" {
					continue
				}
				if !slices.ContainsFunc(current, func(v []any) bool { return reflect.DeepEqual(v, object) }) {
					t.Fatalf("unrelated historical schema object changed: %v", object)
				}
			}
		} else if !reflect.DeepEqual(current, finalObjects) {
			t.Fatal("twice reopen changed final schema")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCoordinationAssignmentResolutionV14ReceiptHistoryUpgrade(t *testing.T) {
	for _, mode := range []string{"unknown", "cancel_requested", "held-with-receipt"} {
		t.Run(mode, func(t *testing.T) {
			old, path := assignmentV13Store(t)
			g, c, _ := dispatchConfiguredGroup(t, old)
			r := dispatchCandidate(t, old, g, c, "receipt-history")
			coordinatorMustCommit(t, old, dispatchPair(r)...)
			materialized, err := old.MaterializeDispatchRequest(ctx, r.request[0].(string), 1, materializeAt)
			if err != nil || materialized.Delivery == nil || materialized.Disposition != "enqueued" {
				t.Fatalf("v13 materialization: %+v, %v", materialized, err)
			}
			if mode == "held-with-receipt" {
				// This shape is reserved by v12/v13 CHECKs but has no production transition command.
				t.Log("Counterfactual historical held receipt: restore the exact old guard after seeding, before migration")
				var guard string
				if err := old.db.QueryRow("SELECT sql FROM sqlite_master WHERE name='coordination_dispatch_state_update'").Scan(&guard); err != nil {
					t.Fatal(err)
				}
				coordinatorMustCommit(t, old,
					coordinatorSQL{"DROP TRIGGER coordination_dispatch_state_update", nil},
					coordinatorSQL{"UPDATE coordination_dispatch_state SET status='held',generation=generation+1,held_reason='task_stale' WHERE request_id=?", []any{r.request[0]}},
					coordinatorSQL{guard, nil},
				)
			} else {
				turn, err := old.ClaimTurn(ctx, ClaimTurnInput{GroupID: g, DeliveryID: materialized.Delivery.DeliveryID, MaxActive: 20, ResourceKeys: []string{"receipt-history-lock"}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := old.RequestTurnCancel(ctx, g, turn.ID); err != nil {
					t.Fatal(err)
				}
				if mode == "unknown" {
					receipt := queueReceipt(provider.OutcomeSucceeded)
					receipt.Outcome, receipt.OutputComplete = provider.OutcomeUnknown, false
					if _, err := old.ApplyTurnReceipt(ctx, TurnClaim{GroupID: g, TurnID: turn.ID, Token: turn.ClaimToken}, receipt); err != nil {
						t.Fatal(err)
					}
				}
			}
			coordinatorMustCommit(t, old,
				coordinatorSQL{"UPDATE tasks SET revision=revision+1 WHERE id=?", []any{r.request[3]}},
				coordinatorSQL{"UPDATE task_coordination SET authority_generation=authority_generation+1 WHERE task_id=?", []any{r.request[3]}},
				coordinatorSQL{coordinatorBindingRevisionInsert, []any{g, 2, c}},
				coordinatorSQL{coordinatorPolicyRevisionInsert, []any{g, 2, "[]"}},
				coordinatorSQL{"UPDATE group_coordinator_bindings SET revision=2 WHERE group_id=?", []any{g}},
				coordinatorSQL{"UPDATE group_dispatch_policies SET revision=2 WHERE group_id=?", []any{g}},
				coordinatorSQL{"UPDATE members SET status='left' WHERE group_id=?", []any{g}},
			)
			typed := assignmentTypedV12(t, old)
			before := assignmentSnapshotOriginalTables(t, old, messageSequenceSnapshot(t, old))
			want, err := old.MaterializeDispatchRequest(ctx, r.request[0].(string), 1, materializeAt)
			if err != nil {
				t.Fatal(err)
			}
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				s := resolutionOpen(t, path)
				if got := assignmentTypedV12(t, s); !reflect.DeepEqual(got, typed) {
					t.Fatalf("typed receipt history changed: %+v; want %+v", got, typed)
				}
				got, err := s.MaterializeDispatchRequest(ctx, r.request[0].(string), 1, materializeAt)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("historical receipt replay changed after migration: %+v; want %+v, %v", got, want, err)
				}
				if err := assignmentCheckOriginalTables(t, s, before); err != nil {
					t.Fatal(err)
				}
				dispatchAssertFK(t, s)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCoordinationAssignmentResolutionV14MigrationFaultRollback(t *testing.T) {
	if len(migrations) < 14 {
		t.Fatal("v14 migration missing")
	}
	for _, marker := range []string{
		"INSERT INTO coordination_dispatch_requests_new", "INSERT INTO coordination_dispatch_state_new",
		"DROP TABLE coordination_dispatch_state;", "DROP TABLE coordination_dispatch_requests;",
		"ALTER TABLE coordination_dispatch_requests_new RENAME TO coordination_dispatch_requests;",
		"ALTER TABLE coordination_dispatch_state_new RENAME TO coordination_dispatch_state;",
		"CREATE TRIGGER coordination_dispatch_state_update", "DROP TRIGGER coordination_assignments_disabled;", "deferred-commit",
	} {
		t.Run(marker, func(t *testing.T) {
			s, path, check := resolutionV13Fixture(t)
			original := migrations
			defer func() { migrations = original }()
			migrations = slices.Clone(original[:14])
			if marker == "deferred-commit" {
				coordinatorMustCommit(t, s, coordinatorSQL{"CREATE TABLE resolution_commit_fault(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)", nil})
				migrations[13] += "INSERT INTO resolution_commit_fault VALUES('missing');"
			} else {
				start := strings.Index(migrations[13], marker)
				if start < 0 {
					t.Fatalf("fault boundary missing: %s", marker)
				}
				end := start + strings.Index(migrations[13][start:], ";") + 1
				if strings.HasPrefix(marker, "CREATE TRIGGER") {
					end = start + strings.Index(migrations[13][start:], "\nEND;") + len("\nEND;")
				}
				migrations[13] = migrations[13][:end] + "\nINSERT INTO resolution_missing_fault VALUES(1);\n" + migrations[13][end:]
			}
			before := messageSequenceSnapshot(t, s)
			objects := messageSequenceRows(t, s.db, assignmentSchemaQuery)
			sequence := messageSequenceTableRows(t, s, "sqlite_sequence")
			err := migrate(s.db)
			if err == nil {
				t.Fatal("injected migration fault committed")
			}
			if marker == "deferred-commit" {
				coordinationAdmissionRequireFK(t, err)
			} else if !strings.Contains(err.Error(), "resolution_missing_fault") || !strings.Contains(err.Error(), "迁移版本 14") {
				t.Fatalf("did not reach intended fault: %v", err)
			}
			t.Logf("real runner rollback at %s: %v", marker, err)
			messageSequenceLedger(t, s, 13)
			messageSequencePreserved(t, s, before)
			check(s)
			if got := messageSequenceRows(t, s.db, assignmentSchemaQuery); !reflect.DeepEqual(got, objects) {
				t.Fatal("migration fault leaked schema changes")
			}
			if got := messageSequenceTableRows(t, s, "sqlite_sequence"); !reflect.DeepEqual(got, sequence) {
				t.Fatal("migration fault changed sequence highwater")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			migrations = original
			reopened := resolutionOpen(t, path)
			check(reopened)
			dispatchAssertFK(t, reopened)
		})
	}
}

func TestCoordinationAssignmentResolutionV14CounterfactualNonemptyPreflight(t *testing.T) {
	for _, table := range []string{"coordination_assignments", "coordination_assignment_dispatch", "coordination_assignment_attempts"} {
		t.Run(table, func(t *testing.T) {
			s, _ := assignmentV13Store(t)
			// Counterfactual corrupted foundation: replace only this empty table with a malformed,
			// populated lookalike. Never disable FKs or pretend v13 admitted this state.
			t.Log("Counterfactual malformed v13 table; preflight must reject before interpreting its rows")
			definition := "(counterfactual TEXT); INSERT INTO " + table + " VALUES('nonempty');"
			if table == "coordination_assignment_dispatch" {
				// Retain the parent key so SQLite can prepare writes with the reciprocal FK still ON.
				definition = "(assignment_id TEXT,task_id TEXT,group_id TEXT,worker_member_id TEXT, UNIQUE(assignment_id,task_id,group_id,worker_member_id)); INSERT INTO " + table + " VALUES('counterfactual','task','group','worker');"
			}
			coordinatorMustCommit(t, s, coordinatorSQL{"DROP TABLE " + table + "; CREATE TABLE " + table + definition, nil})
			before := messageSequenceSnapshot(t, s)
			objects := messageSequenceRows(t, s.db, assignmentSchemaQuery)
			err := migrate(s.db)
			if err == nil || !strings.Contains(err.Error(), "迁移版本 14") || !strings.Contains(err.Error(), "CHECK constraint failed: ok=1") {
				t.Fatalf("nonempty foundation did not hit the explicit preflight: %v", err)
			}
			messageSequenceLedger(t, s, 13)
			messageSequencePreserved(t, s, before)
			if got := messageSequenceRows(t, s.db, assignmentSchemaQuery); !reflect.DeepEqual(got, objects) {
				t.Fatal("preflight failure changed original schema")
			}
		})
	}
}

func TestCoordinationAssignmentResolutionV14CounterfactualReceiptPreflight(t *testing.T) {
	for _, mode := range []string{"empty-pointer-existing-turn", "correct-pointer-extra-turn", "missing-pointer-target"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := assignmentV13Store(t)
			g, c, _ := dispatchConfiguredGroup(t, s)
			r := dispatchCandidate(t, s, g, c, "counterfactual-receipt")
			coordinatorMustCommit(t, s, dispatchPair(r)...)
			r.state = dispatchReceipt(t, s, r)
			coordinatorMustCommit(t, s, dispatchUpdate(r.state))
			d := queueDelivery(t, s, g, c, "counterfactual-ordinary")
			d.ID, d.ConversationID = r.state[11].(string), r.state[12].(string)
			id := "counterfactual-target"
			if mode != "missing-pointer-target" {
				coordinatorMustCommit(t, s, resolutionOrdinaryTurn(g, d, id, "not_started"))
			}
			if mode != "empty-pointer-existing-turn" {
				coordinatorMustCommit(t, s, coordinatorSQL{"UPDATE deliveries SET turn_id=? WHERE id=?", []any{id, d.ID}})
			}
			if mode == "correct-pointer-extra-turn" {
				coordinatorMustCommit(t, s, resolutionOrdinaryTurn(g, d, "counterfactual-extra", "not_started"))
			}
			t.Log("Counterfactual raw-SQL N2 provenance corruption, not evidence of a production ClaimTurn failure")
			before := messageSequenceSnapshot(t, s)
			objects := messageSequenceRows(t, s.db, assignmentSchemaQuery)
			err := migrate(s.db)
			if err == nil || !strings.Contains(err.Error(), "迁移版本 14") || !strings.Contains(err.Error(), "CHECK constraint failed: ok=1") {
				t.Fatalf("corrupt old receipt did not hit the provenance preflight: %v", err)
			}
			messageSequenceLedger(t, s, 13)
			messageSequencePreserved(t, s, before)
			if got := messageSequenceRows(t, s.db, assignmentSchemaQuery); !reflect.DeepEqual(got, objects) {
				t.Fatal("failed receipt preflight changed schema")
			}
		})
	}
}

func TestCoordinationAssignmentResolutionV14FutureAppendProjection(t *testing.T) {
	s, path := resolutionStore(t)
	r := resolutionCandidate(t, s, "assign")
	coordinatorMustCommit(t, s, resolutionStatements(r)...)
	before := assignmentSnapshotOriginalTables(t, s, messageSequenceSnapshot(t, s))
	objects := messageSequenceRows(t, s.db, assignmentSchemaQuery)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	original := migrations
	defer func() { migrations = original }()
	const future = `ALTER TABLE coordination_dispatch_state ADD COLUMN resolution_future_nullable TEXT;
	 CREATE TABLE resolution_future_sentinel(id TEXT); INSERT INTO resolution_future_sentinel VALUES('applied');`
	migrations = append(slices.Clone(original), future+"INSERT INTO resolution_future_missing VALUES(1);")
	failed, err := Open(path)
	if err == nil {
		_ = failed.Close()
		t.Fatal("future migration error swallowed")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("迁移版本 %d", len(migrations))) || !strings.Contains(err.Error(), "resolution_future_missing") {
		t.Fatalf("unexpected future failure: %v", err)
	}
	rollback := resolutionOpen(t, path)
	if err := assignmentCheckOriginalTables(t, rollback, before); err != nil {
		t.Fatal(err)
	}
	if got := messageSequenceRows(t, rollback.db, assignmentSchemaQuery); !reflect.DeepEqual(got, objects) {
		t.Fatal("failed future append leaked schema")
	}
	_ = rollback.Close()
	migrations = append(slices.Clone(original), future)
	for range 2 {
		reopened := queueHandle(t, path, false)
		messageSequenceLedger(t, reopened, len(migrations))
		if err := assignmentCheckOriginalTables(t, reopened, before); err != nil {
			t.Fatal(err)
		}
		if got := messageSequenceTableRows(t, reopened, "resolution_future_sentinel"); !reflect.DeepEqual(got, [][]any{{"applied"}}) {
			t.Fatal("future chain did not execute")
		}
		_ = reopened.Close()
	}
}

func TestCoordinationAssignmentResolutionV14FrozenHistoryAndFinalFK(t *testing.T) {
	if len(migrations) < 14 {
		t.Fatal("v14 resolution missing")
	}
	source, err := os.ReadFile("migrations.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := assignmentCheckMigrationSource(source); err != nil {
		t.Fatal(err)
	}
	if hash := fmt.Sprintf("%x", sha256.Sum256([]byte(migrations[12]))); hash != "517f592c721740e49a31af2b4ccdbb660c3670714d1ecd6d0b34050157f5d215" {
		t.Fatalf("v13 SQL changed: %s", hash)
	}
	t.Logf("v14 SQL SHA256=%x", sha256.Sum256([]byte(migrations[13])))
	s, _ := resolutionStore(t)
	t.Logf("final coordination schema objects: %v", messageSequenceRows(t, s.db, `SELECT type,name,tbl_name FROM sqlite_master
	 WHERE name LIKE 'coordination_%' OR name='turns_coordination_delivery' ORDER BY type,name`))
	for _, table := range []string{"coordination_dispatch_requests", "coordination_dispatch_state", "coordination_decisions", "coordination_assignments", "coordination_assignment_dispatch", "coordination_assignment_attempts"} {
		t.Logf("final columns %s: %v", table, assignmentOriginalColumns(t, s, table))
		got := messageSequenceRows(t, s.db, "SELECT * FROM pragma_foreign_key_list('"+table+"')")
		t.Logf("final FK %s: %v", table, got)
		for _, row := range got {
			if strings.Contains(row[2].(string), "_new") {
				t.Fatalf("temporary final FK target on %s: %v", table, row)
			}
		}
	}
	if got := messageSequenceRows(t, s.db, `SELECT name FROM sqlite_master WHERE name='coordination_assignments_disabled'`); len(got) != 0 {
		t.Fatal("complete v14 left the temporary gate installed")
	}
	plan := resolutionPlan(t, s, resolutionSourceConflict, "missing")
	if strings.Contains(plan, "SCAN t") || strings.Contains(plan, "TEMP B-TREE") || !strings.Contains(plan, "turns_coordination_delivery") {
		t.Fatalf("final bounded source lookup lacks effective delivery index: %s", plan)
	}
}
