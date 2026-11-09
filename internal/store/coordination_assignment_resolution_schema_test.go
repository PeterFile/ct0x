package store

import (
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"

	"modernc.org/sqlite"
)

const resolutionDecisionColumns = "id,request_id,task_id,group_id,coordinator_member_id,coordinator_turn_id,kind,worker_member_id,assignment_id,reason,created_at"
const resolutionDecisionInsert = "INSERT INTO coordination_decisions(" + resolutionDecisionColumns + ") VALUES(?,?,?,?,?,?,?,?,?,?,?)"

// This is the exact first-reference predicate used by the migration and both receipt guards.
const resolutionSourceConflict = `SELECT 1 FROM deliveries d WHERE d.id=? AND
 ((d.turn_id='' AND EXISTS(SELECT 1 FROM turns t WHERE t.delivery_id=d.id)) OR
 (d.turn_id<>'' AND (NOT EXISTS(SELECT 1 FROM turns t WHERE t.id=d.turn_id
 AND t.delivery_id=d.id AND t.group_id=d.group_id AND t.conversation_id=d.conversation_id AND t.member_id=d.recipient_id)
 OR EXISTS(SELECT 1 FROM turns t WHERE t.delivery_id=d.id AND t.id<>d.turn_id))))`

func resolutionOpen(t *testing.T, path string) *Store {
	t.Helper()
	if len(migrations) < 14 {
		t.Fatal("v14 resolution migration is missing")
	}
	original := migrations
	migrations = original[:14]
	defer func() { migrations = original }()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	messageSequenceLedger(t, s, 14)
	return s
}

func resolutionStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolution-v14.sqlite")
	return resolutionOpen(t, path), path
}

type resolutionRows struct {
	assignmentSQLRows
	decision []any
	turn     domain.Turn
}

func resolutionCandidate(t *testing.T, s *Store, kind string) resolutionRows {
	t.Helper()
	g, c, w := dispatchConfiguredGroup(t, s)
	a := assignmentCandidate(t, s, g, c, w, "resolution")
	a.request.state = dispatchReceipt(t, s, a.request)
	coordinatorMustCommit(t, s, dispatchUpdate(a.request.state))
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: g, DeliveryID: a.request.state[11].(string), MaxActive: 20, ResourceKeys: []string{"resolution-" + a.request.request[0].(string)}})
	if err != nil {
		t.Fatal(err)
	}
	decision := []any{a.assignment[2], a.request.request[0], a.assignment[3], g, c, turn.ID, kind, w, a.assignment[0], " 明确决定 ", dispatchStamp}
	if kind == "decline" {
		decision[7], decision[8] = nil, nil
	}
	return resolutionRows{a, decision, turn}
}

func resolutionStatements(r resolutionRows) []coordinatorSQL {
	statements := []coordinatorSQL{{resolutionDecisionInsert, r.decision}}
	status := "declined"
	if r.decision[6] == "assign" {
		statements = append(statements, assignmentPair(r.assignmentSQLRows)...)
		status = "assigned"
	}
	return append(statements, coordinatorSQL{`UPDATE coordination_dispatch_state
	 SET status=?,generation=generation+1,decision_id=?,updated_at=? WHERE request_id=?`, []any{status, r.decision[0], dispatchStamp, r.decision[1]}})
}

func TestCoordinationAssignmentResolutionV14CommitAndReplay(t *testing.T) {
	for _, kind := range []string{"assign", "decline"} {
		t.Run(kind, func(t *testing.T) {
			s, path := resolutionStore(t)
			r := resolutionCandidate(t, s, kind)
			before := messageSequenceSnapshot(t, s)
			coordinatorMustCommit(t, s, resolutionStatements(r)...)
			for _, table := range []string{"coordination_decisions", "coordination_assignments", "coordination_assignment_dispatch", "coordination_dispatch_state"} {
				delete(before, table)
			}
			messageSequencePreserved(t, s, before)
			dispatchAssertFK(t, s)
			original := assignmentSnapshotOriginalTables(t, s, messageSequenceSnapshot(t, s))
			for range 2 {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s = resolutionOpen(t, path)
				if err := assignmentCheckOriginalTables(t, s, original); err != nil {
					t.Fatal(err)
				}
				got, err := s.MaterializeDispatchRequest(ctx, r.decision[1].(string), 1, materializeAt)
				if err != nil || got.Disposition != map[string]string{"assign": "assigned", "decline": "declined"}[kind] || got.Generation != 3 || got.Delivery == nil || got.Delivery.DeliveryID != r.request.state[11] {
					t.Fatalf("resolved replay must retain receipt and return current projection: %+v, %v", got, err)
				}
				if err := assignmentCheckOriginalTables(t, s, original); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func resolutionPlan(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "\n")
	t.Logf("SQL: %s\nPLAN:\n%s", query, plan)
	return plan
}

func TestCoordinationAssignmentResolutionV13SourcePlanBefore(t *testing.T) {
	s, _ := assignmentV13Store(t)
	var version string
	if err := s.db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("production modernc SQLite=%s", version)
	plan := resolutionPlan(t, s, resolutionSourceConflict, "missing")
	if !strings.Contains(plan, "SCAN t") {
		t.Fatal("before evidence changed: review the actual effective delivery lookup index before adding one")
	}
}

func TestCoordinationAssignmentResolutionV14PartialCommit(t *testing.T) {
	s, _ := resolutionStore(t)
	r := resolutionCandidate(t, s, "assign")
	all := resolutionStatements(r)
	for _, indexes := range [][]int{{0}, {0, 1}, {0, 1, 2}, {0, 3}, {0, 1, 3}, {1, 2, 3}, {3}} {
		t.Run(fmt.Sprint(indexes), func(t *testing.T) {
			var statements []coordinatorSQL
			for _, i := range indexes {
				statements = append(statements, all[i])
			}
			dispatchReject(t, s, "", statements...)
		})
	}
	before := messageSequenceSnapshot(t, s)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(resolutionDecisionInsert, r.decision...); err != nil {
		t.Fatalf("Decision must defer only the reverse state relation: %v", err)
	}
	coordinationAdmissionRequireFK(t, tx.Commit())
	_ = tx.Rollback()
	messageSequencePreserved(t, s, before)
	coordinatorMustCommit(t, s, all...)
}

func TestCoordinationAssignmentResolutionV14DecisionProvenanceAndShape(t *testing.T) {
	s, _ := resolutionStore(t)
	a, b := resolutionCandidate(t, s, "assign"), resolutionCandidate(t, s, "decline")
	for column, values := range map[string][]any{
		"id":         {nil, "", " padded", []byte("blob")},
		"request_id": {b.decision[1], "missing"}, "task_id": {b.decision[2]}, "group_id": {b.decision[3]},
		"coordinator_member_id": {a.decision[7], b.decision[4]}, "coordinator_turn_id": {b.decision[5], "missing"},
		"kind":             {nil, "assigned", "declined", "", []byte("assign")},
		"worker_member_id": {nil, "", " padded", []byte("blob"), b.decision[4]},
		"assignment_id":    {nil, "", " padded", []byte("blob")}, "reason": {nil, "", " ", []byte("blob")},
		"created_at": {nil, "", "2026-02-30T03:04:05.123456789Z", "2026-01-02T03:04:05Z", []byte(dispatchStamp)},
	} {
		for _, value := range values {
			t.Run(fmt.Sprintf("%s/%v", column, value), func(t *testing.T) {
				bad := a
				bad.decision = dispatchSet(resolutionDecisionColumns, a.decision, column, value)
				dispatchReject(t, s, "", resolutionStatements(bad)...)
			})
		}
	}
	for _, column := range []string{"worker_member_id", "assignment_id"} {
		bad := b
		bad.decision = dispatchSet(resolutionDecisionColumns, b.decision, column, rand.Text())
		dispatchReject(t, s, "", resolutionStatements(bad)...)
	}
	coordinatorMustCommit(t, s, resolutionStatements(a)...)
	coordinatorMustCommit(t, s, resolutionStatements(b)...)
	got := messageSequenceRows(t, s.db, "SELECT name FROM pragma_table_info('coordination_decisions') ORDER BY cid")
	want := [][]any{}
	for _, column := range strings.Split(resolutionDecisionColumns, ",") {
		want = append(want, []any{column})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Decision exact final columns: %v; want %v", got, want)
	}
	for _, recursive := range []int{0, 1} {
		dispatchRecursive(t, s, recursive)
		var covered []string
		for _, index := range messageSequenceRows(t, s.db, `SELECT name FROM pragma_index_list('coordination_decisions') WHERE "unique"=1`) {
			candidate := slices.Clone(b.decision)
			var names []string
			for _, row := range messageSequenceRows(t, s.db, fmt.Sprintf("SELECT name FROM pragma_index_info('%s') ORDER BY seqno", index[0])) {
				column := row[0].(string)
				names = append(names, column)
				i := slices.Index(strings.Split(resolutionDecisionColumns, ","), column)
				candidate[i] = a.decision[i]
			}
			covered = append(covered, strings.Join(names, ","))
			for _, form := range []string{"INSERT INTO", "INSERT OR IGNORE INTO", "INSERT OR REPLACE INTO"} {
				dispatchReject(t, s, "决定不可替换", coordinatorSQL{strings.Replace(resolutionDecisionInsert, "INSERT INTO", form, 1), candidate})
			}
		}
		t.Logf("recursive=%d all Decision UNIQUE victims: %v", recursive, covered)
		for _, query := range []string{"UPDATE coordination_decisions SET reason=reason", "UPDATE OR REPLACE coordination_decisions SET id='new'", "DELETE FROM coordination_decisions"} {
			dispatchReject(t, s, "决定不可", coordinatorSQL{query, nil})
		}
	}
}

func TestCoordinationAssignmentResolutionV14StateUniqueVictims(t *testing.T) {
	columns := strings.Split(dispatchStateColumns+",decision_id", ",")
	insert := "INSERT INTO coordination_dispatch_state(" + strings.Join(columns, ",") + ") VALUES(" + strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",") + ")"
	var fields []string
	for _, column := range columns {
		fields = append(fields, column+"=?")
	}
	for _, kind := range []string{"assign", "decline"} {
		for _, recursive := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/recursive=%d", kind, recursive), func(t *testing.T) {
				s, _ := resolutionStore(t)
				r := resolutionCandidate(t, s, kind)
				coordinatorMustCommit(t, s, resolutionStatements(r)...)
				original := messageSequenceRows(t, s.db, "SELECT "+strings.Join(columns, ",")+" FROM coordination_dispatch_state")[0]
				b := dispatchCandidate(t, s, r.turn.GroupID, r.turn.MemberID, "unique-v14")
				dispatchRecursive(t, s, recursive)
				var keys []string
				victims := map[string][]any{}
				for _, index := range messageSequenceRows(t, s.db, `SELECT name FROM pragma_index_list('coordination_dispatch_state') WHERE "unique"=1`) {
					candidate := append(slices.Clone(b.state), nil)
					var names []string
					for _, row := range messageSequenceRows(t, s.db, fmt.Sprintf("SELECT name FROM pragma_index_info('%s') ORDER BY seqno", index[0])) {
						column := row[0].(string)
						i := slices.Index(columns, column)
						if i < 0 {
							t.Fatalf("uncovered v14 state UNIQUE column: %s", column)
						}
						names = append(names, column)
						candidate[i] = original[i]
					}
					key := strings.Join(names, ",")
					keys = append(keys, key)
					victims[key] = candidate
					for _, form := range []string{"INSERT INTO", "INSERT OR IGNORE INTO", "INSERT OR REPLACE INTO"} {
						var prefix []coordinatorSQL
						if candidate[0] != original[0] {
							prefix = append(prefix, coordinatorSQL{dispatchRequestInsert, b.request})
						}
						guard := "状态不可替换"
						if kind == "assign" && key == "task_id" {
							guard = "所有权" // Assigned left the partial slot; Assignment now owns the Task.
						}
						dispatchReject(t, s, guard, append(prefix, coordinatorSQL{strings.Replace(insert, "INSERT INTO", form, 1), candidate})...)
					}
				}
				slices.Sort(keys)
				if !slices.Equal(keys, []string{"decision_id,request_id,task_id,group_id", "delivery_id", "message_id", "request_id", "request_id,task_id,group_id", "task_id"}) {
					t.Fatalf("uncovered v14 state UNIQUE keys: %v", keys)
				}
				t.Logf("actual v14 state UNIQUE victims: %v", keys)
				coordinatorMustCommit(t, s, dispatchPair(b)...)
				for key, candidate := range victims {
					candidate[3] = int64(2)
					guard := "状态不可替换"
					if kind == "assign" && key == "task_id" {
						guard = "身份" // No indexed victim exists; pending state identity still cannot drift.
					}
					dispatchReject(t, s, guard, coordinatorSQL{"UPDATE OR REPLACE coordination_dispatch_state SET " + strings.Join(fields, ",") + " WHERE request_id=?", append(slices.Clone(candidate), b.state[0])})
				}
				dispatchAssertFK(t, s)
			})
		}
	}
}

func TestCoordinationAssignmentResolutionV14RequestFixtureUpdateAndResolvedReplay(t *testing.T) {
	for _, kind := range []string{"assign", "decline"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := resolutionStore(t)
			g, c, w := dispatchConfiguredGroup(t, s)
			template := dispatchCandidate(t, s, g, c, "requested-resolution")
			request, err := s.RequestDispatch(ctx, g, template.request[3].(string), 1, 1, "明确请求", "original")
			if err != nil {
				t.Fatal(err)
			}
			saved := requestDispatchSavedRows(t, s, request.ID)
			if len(saved.request) != len(strings.Split(dispatchRequestColumns, ",")) || len(saved.state) != len(strings.Split(dispatchStateColumns, ",")) {
				t.Fatalf("legacy fixture must use frozen columns: request=%d state=%d", len(saved.request), len(saved.state))
			}
			receipt := dispatchReceipt(t, s, saved)
			update := dispatchUpdate(receipt)
			result, err := s.db.ExecContext(ctx, update.query, update.args...)
			if err != nil {
				t.Fatal(err)
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				t.Fatalf("fixture receipt UPDATE must affect exactly one real row: %d, %v", n, err)
			}
			t.Log("fixture receipt UPDATE RowsAffected=1")
			persisted := requestDispatchSavedRows(t, s, request.ID)
			if !reflect.DeepEqual(persisted.request, saved.request) || !reflect.DeepEqual(persisted.state, receipt) {
				t.Fatalf("fixture request/state round-trip changed: %+v; want receipt %v", persisted, receipt)
			}
			view, err := s.GetDispatchRequest(ctx, g, request.ID)
			wantReceipt := &domain.DispatchDeliveryReceipt{
				MessageID: receipt[10].(string), DeliveryID: receipt[11].(string), ConversationID: receipt[12].(string),
				CoordinatorMemberID: c, MaterializedFromGeneration: 1, CreatedAt: request.CreatedAt,
			}
			if err != nil || view.Request != request || view.State.Generation != 2 || string(view.State.Status) != "enqueued" || !reflect.DeepEqual(view.State.Delivery, wantReceipt) {
				t.Fatalf("current state/receipt did not round-trip: %+v, %v", view, err)
			}
			turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: g, DeliveryID: wantReceipt.DeliveryID, MaxActive: 20})
			if err != nil {
				t.Fatal(err)
			}
			assignmentID, decisionID := rand.Text(), rand.Text()
			a := assignmentSQLRows{
				request:    persisted,
				assignment: []any{assignmentID, request.ID, decisionID, request.TaskID, g, w, dispatchStamp},
				state:      []any{assignmentID, request.TaskID, g, w, int64(1), "pending", int64(0), dispatchStamp, dispatchStamp, "", "", nil, nil, nil, nil, nil, nil},
			}
			decision := []any{decisionID, request.ID, request.TaskID, g, c, turn.ID, kind, w, assignmentID, "明确决定", dispatchStamp}
			if kind == "decline" {
				decision[7], decision[8] = nil, nil
			}
			coordinatorMustCommit(t, s, resolutionStatements(resolutionRows{a, decision, turn})...)
			resolved, err := s.GetDispatchRequest(ctx, g, request.ID)
			if err != nil || resolved.State.Generation != 3 || string(resolved.State.Status) != map[string]string{"assign": "assigned", "decline": "declined"}[kind] || !reflect.DeepEqual(resolved.State.Delivery, wantReceipt) {
				t.Fatalf("resolved projection lost original receipt: %+v, %v", resolved, err)
			}
			before := messageSequenceSnapshot(t, s)
			got, newKeyErr := s.RequestDispatch(ctx, g, request.TaskID, 1, 1, "明确请求", "new-key")
			requestDispatchZero(t, got, newKeyErr, nil)
			var sqliteErr *sqlite.Error
			sqliteCode := 0
			if errors.As(newKeyErr, &sqliteErr) {
				sqliteCode = sqliteErr.Code()
			}
			// Observe classification only; a new error contract belongs to the later N3 integration.
			t.Logf("resolved=%s new-key ErrConflict=%t SQLiteCode=%d error=%v", kind, errors.Is(newKeyErr, ErrConflict), sqliteCode, newKeyErr)
			messageSequencePreserved(t, s, before)
			if replay, err := s.RequestDispatch(ctx, g, request.TaskID, 1, 1, "明确请求", "original"); err != nil || replay != request {
				t.Fatalf("same-key replay lost the original request receipt: %+v, %v", replay, err)
			}
			messageSequencePreserved(t, s, before)
			dispatchAssertFK(t, s)
		})
	}
}
