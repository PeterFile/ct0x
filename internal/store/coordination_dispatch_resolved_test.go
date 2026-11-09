package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"

	"modernc.org/sqlite"
)

const resolvedDispatchReason = "安排已决定任务"
const resolvedDispatchKey = "resolved-original"
const resolvedDispatchSchemaSQL = "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name"

type resolvedDispatchFixture struct {
	requestDispatchFixture
	request domain.DispatchRequest
	view    domain.DispatchView
	schema  [][]any
}

func resolvedDispatchSchema(t *testing.T, s *Store) [][]any {
	t.Helper()
	messageSequenceLedger(t, s, 14)
	var enabled int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("production foreign keys must remain enabled: %d, %v", enabled, err)
	}
	dispatchAssertFK(t, s)
	return messageSequenceRows(t, s.db, resolvedDispatchSchemaSQL)
}

// Use real Store APIs through the durable coordinator Turn; only the Decision is SQL.
// This is a legal v14 fixture, not an N3 API or runtime implementation.
func resolvedDispatchSetup(t *testing.T, kind string) resolvedDispatchFixture {
	t.Helper()
	f := requestDispatchSetup(t)
	schema := resolvedDispatchSchema(t, f.s)
	request, err := f.s.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, resolvedDispatchReason, resolvedDispatchKey)
	if err != nil {
		t.Fatal(err)
	}
	at := request.CreatedAt.Add(time.Second)
	receipt, err := f.s.MaterializeDispatchRequest(ctx, request.ID, 1, at)
	if err != nil {
		t.Fatal(err)
	}
	materializeAssertReceipt(t, f.s, request, receipt, 1, at)
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: receipt.Delivery.DeliveryID,
		MaxActive: 2, ResourceKeys: []string{"resolved-" + request.TaskID}})
	if err != nil {
		t.Fatal(err)
	}
	queueCounts(t, f.s, 1, 1, 1)
	provenance := messageSequenceRows(t, f.s.db, `SELECT t.id,t.delivery_id,d.turn_id,t.state FROM turns t
	 JOIN deliveries d ON d.id=t.delivery_id`)
	if !reflect.DeepEqual(provenance, [][]any{{turn.ID, receipt.Delivery.DeliveryID, turn.ID, "starting"}}) {
		t.Fatalf("coordinator Turn must be durably bidirectional: %v", provenance)
	}
	before := messageSequenceSnapshot(t, f.s)
	assignmentID, decisionID := rand.Text(), rand.Text()
	stamp := at.Add(time.Second).Format(timestampLayout)
	decision := []any{decisionID, request.ID, request.TaskID, f.groupID, f.coord, turn.ID, kind, f.worker, assignmentID, "明确决定", stamp}
	statements := []coordinatorSQL{{resolutionDecisionInsert, decision}}
	status := "declined"
	if kind == "assign" {
		a := assignmentSQLRows{
			assignment: []any{assignmentID, request.ID, decisionID, request.TaskID, f.groupID, f.worker, stamp},
			state: []any{assignmentID, request.TaskID, f.groupID, f.worker, int64(1), "pending", int64(0), stamp, stamp,
				"", "", nil, nil, nil, nil, nil, nil},
		}
		statements = append(statements, assignmentPair(a)...)
		status = "assigned"
	} else if kind == "decline" {
		decision[7], decision[8] = nil, nil
	} else {
		t.Fatalf("unknown fixture decision: %s", kind)
	}
	statements = append(statements, coordinatorSQL{`UPDATE coordination_dispatch_state
	 SET status=?,generation=generation+1,decision_id=?,updated_at=? WHERE request_id=?`, []any{status, decisionID, stamp, request.ID}})
	coordinatorMustCommit(t, f.s, statements...)
	for _, table := range []string{"coordination_decisions", "coordination_assignments", "coordination_assignment_dispatch", "coordination_dispatch_state"} {
		delete(before, table)
	}
	messageSequencePreserved(t, f.s, before)
	counts := messageSequenceRows(t, f.s.db, `SELECT (SELECT count(*) FROM coordination_decisions),
	 (SELECT count(*) FROM coordination_assignments),(SELECT count(*) FROM coordination_assignment_dispatch)`)
	wantAssignments := int64(0)
	if kind == "assign" {
		wantAssignments = 1
	}
	if !reflect.DeepEqual(counts, [][]any{{int64(1), wantAssignments, wantAssignments}}) {
		t.Fatalf("resolution left partial ownership: %v", counts)
	}
	view, err := f.s.GetDispatchRequest(ctx, f.groupID, request.ID)
	if err != nil || view.Request != request || view.State.Generation != 3 || string(view.State.Status) != status || !reflect.DeepEqual(view.State.Delivery, receipt.Delivery) {
		t.Fatalf("resolution lost the exact request/delivery receipt: %+v, %v", view, err)
	}
	if !reflect.DeepEqual(resolvedDispatchSchema(t, f.s), schema) {
		t.Fatal("legal fixture changed the production schema")
	}
	return resolvedDispatchFixture{f, request, view, schema}
}

func resolvedDispatchPreserved(t *testing.T, s *Store, before map[string][][]any, schema [][]any) {
	t.Helper()
	messageSequencePreserved(t, s, before)
	if !reflect.DeepEqual(messageSequenceSnapshot(t, s), before) {
		t.Fatal("request changed the complete persistent snapshot")
	}
	if !reflect.DeepEqual(resolvedDispatchSchema(t, s), schema) {
		t.Fatal("request changed production guards, foreign keys, or indexes")
	}
}

func TestRequestDispatchResolvedConflictAndReplay(t *testing.T) {
	for _, kind := range []string{"assign", "decline"} {
		t.Run(kind, func(t *testing.T) {
			f := resolvedDispatchSetup(t, kind)
			otherGroup := group(t, f.s).ID
			before := messageSequenceSnapshot(t, f.s)
			for _, phase := range []string{"committed", "reopened"} {
				if phase == "reopened" {
					if err := f.s.Close(); err != nil {
						t.Fatal(err)
					}
					f.s = queueHandle(t, f.path, false)
				}
				got, err := f.s.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, resolvedDispatchReason, "resolved-new-key")
				var sqliteErr *sqlite.Error
				code := 0
				if errors.As(err, &sqliteErr) {
					code = sqliteErr.Code()
				}
				t.Logf("%s resolved=%s zero=%t ErrConflict=%t SQLiteCode=%d error=%v", phase, kind, got == (domain.DispatchRequest{}), errors.Is(err, ErrConflict), code, err)
				// Keep checking persistence and both cases even during the intentional behavior RED.
				if got != (domain.DispatchRequest{}) || !errors.Is(err, ErrConflict) {
					t.Errorf("new key must return zero and ErrConflict: %+v, %v", got, err)
				}
				if got, err := f.s.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, resolvedDispatchReason, resolvedDispatchKey); err != nil || got != f.request {
					t.Fatalf("resolved replay changed the immutable request: %+v, %v", got, err)
				}
				got, err = f.s.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, resolvedDispatchReason+" ", resolvedDispatchKey)
				requestDispatchZero(t, got, err, ErrConflict)
				for _, groupID := range []string{"missing-resolved-group", otherGroup} {
					for _, key := range []string{resolvedDispatchKey, "resolved-new-key"} {
						got, err := f.s.RequestDispatch(ctx, groupID, f.task.ID, 1, 1, resolvedDispatchReason, key)
						requestDispatchZero(t, got, err, ErrNotFound)
					}
					view, err := f.s.GetDispatchRequest(ctx, groupID, f.request.ID)
					requestDispatchZero(t, view, err, ErrNotFound)
				}
				view, err := f.s.GetDispatchRequest(ctx, f.groupID, f.request.ID)
				if err != nil || !reflect.DeepEqual(view, f.view) {
					t.Fatalf("resolved read changed the original delivery: %+v, %v", view, err)
				}
				resolvedDispatchPreserved(t, f.s, before, f.schema)
			}
			// Historical replay must precede every new-key eligibility and occupancy check.
			if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, nil, 1, "resolved-policy-changed"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.SetCoordinator(ctx, f.groupID, member(t, f.s, f.groupID).ID, 1, "resolved-binding-changed"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec("UPDATE members SET status='left' WHERE group_id=?", f.groupID); err != nil {
				t.Fatal(err)
			}
			before = messageSequenceSnapshot(t, f.s)
			if got, err := f.s.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, resolvedDispatchReason, resolvedDispatchKey); err != nil || got != f.request {
				t.Fatalf("current eligibility hid the historical resolved receipt: %+v, %v", got, err)
			}
			got, err := f.s.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "changed", resolvedDispatchKey)
			requestDispatchZero(t, got, err, ErrConflict)
			resolvedDispatchPreserved(t, f.s, before, f.schema)
		})
	}
}

func TestRequestDispatchResolvedOtherAbortPreservesCause(t *testing.T) {
	f := requestDispatchSetup(t)
	schema := resolvedDispatchSchema(t, f.s)
	const trigger = "request_dispatch_resolved_fault"
	if _, err := f.s.db.Exec("CREATE TRIGGER " + trigger + ` AFTER INSERT ON coordination_dispatch_state
	 BEGIN SELECT RAISE(ABORT,'注入非业务分派故障'); END`); err != nil {
		t.Fatal(err)
	}
	before := messageSequenceSnapshot(t, f.s)
	got, err := f.s.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "故障测试", "resolved-fault-retry")
	requestDispatchZero(t, got, err, nil)
	var sqliteErr *sqlite.Error
	if errors.Is(err, ErrConflict) || !errors.As(err, &sqliteErr) || sqliteErr.Code() != 1811 || !strings.Contains(err.Error(), "注入非业务分派故障") {
		t.Fatalf("unrelated SQLite ABORT lost its cause or became a business conflict: %v", err)
	}
	t.Logf("fresh-task unrelated ABORT remains SQLiteCode=%d ErrConflict=%t: %v", sqliteErr.Code(), errors.Is(err, ErrConflict), err)
	messageSequencePreserved(t, f.s, before)
	if _, err := f.s.db.Exec("DROP TRIGGER " + trigger); err != nil {
		t.Fatal(err)
	}
	resolvedDispatchPreserved(t, f.s, before, schema)
	if _, err := f.s.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "故障撤除后重试", "resolved-fault-retry"); err != nil {
		t.Fatalf("unrelated ABORT burned the fresh task or key: %v", err)
	}
	requestDispatchOnlyRows(t, before, messageSequenceSnapshot(t, f.s))
	dispatchAssertFK(t, f.s)
}

func resolvedDispatchBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && (sqliteErr.Code()&255 == 5 || sqliteErr.Code()&255 == 6)
}

func TestRequestDispatchResolvedBusyPreservesCause(t *testing.T) {
	f := requestDispatchSetup(t)
	second := queueHandle(t, f.path, false)
	schema := resolvedDispatchSchema(t, second)
	before := messageSequenceSnapshot(t, f.s)
	if _, err := second.db.Exec("PRAGMA busy_timeout=1"); err != nil {
		t.Fatal(err)
	}
	lock, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	got, err := second.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "竞争写锁", "resolved-busy-retry")
	requestDispatchZero(t, got, err, nil)
	if !resolvedDispatchBusy(err) || errors.Is(err, ErrConflict) {
		t.Fatalf("busy must retain SQLite cause, not claim a definite conflict: %v", err)
	}
	t.Logf("held real writer lock preserves busy cause: %v", err)
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	resolvedDispatchPreserved(t, second, before, schema)
	if _, err := second.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "写锁释放后重试", "resolved-busy-retry"); err != nil {
		t.Fatalf("busy burned the fresh task or key: %v", err)
	}
	requestDispatchOnlyRows(t, before, messageSequenceSnapshot(t, second))
	dispatchAssertFK(t, second)
}

func TestRequestDispatchResolvedConcurrentHandles(t *testing.T) {
	for _, kind := range []string{"fresh", "assign", "decline"} {
		t.Run(kind, func(t *testing.T) {
			var f requestDispatchFixture
			if kind == "fresh" {
				f = requestDispatchSetup(t)
			} else {
				f = resolvedDispatchSetup(t, kind).requestDispatchFixture
			}
			second := queueHandle(t, f.path, false)
			schema := resolvedDispatchSchema(t, f.s)
			before := messageSequenceSnapshot(t, f.s)
			bounded, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			type result struct {
				value domain.DispatchRequest
				err   error
			}
			start, results := make(chan struct{}), make(chan result, 2)
			for i, handle := range []*Store{f.s, second} {
				go func() {
					<-start
					value, err := handle.RequestDispatch(bounded, f.groupID, f.task.ID, 1, 1, "并发新键", fmt.Sprintf("resolved-race-%d", i))
					results <- result{value, err}
				}()
			}
			close(start)
			winners := 0
			for range 2 {
				select {
				case got := <-results:
					if got.err == nil {
						winners++
						if got.value.ID == "" || got.value.TaskID != f.task.ID {
							t.Errorf("invalid successful request: %+v", got.value)
						}
					} else {
						requestDispatchZero(t, got.value, got.err, nil)
						busy := resolvedDispatchBusy(got.err)
						if busy && errors.Is(got.err, ErrConflict) || !busy && !errors.Is(got.err, ErrConflict) {
							t.Errorf("unexpected or misclassified concurrent rejection: %v", got.err)
						}
						t.Logf("%s competing handle: busy=%t ErrConflict=%t error=%v", kind, busy, errors.Is(got.err, ErrConflict), got.err)
					}
				case <-bounded.Done():
					t.Fatal(bounded.Err())
				}
			}
			if kind == "fresh" {
				if winners != 1 {
					t.Fatalf("fresh task must commit exactly one slot, got %d", winners)
				}
				requestDispatchOnlyRows(t, before, messageSequenceSnapshot(t, second))
			} else {
				if winners != 0 {
					t.Fatalf("resolved task accepted %d new requests", winners)
				}
				resolvedDispatchPreserved(t, second, before, schema)
			}
			var slots int
			if err := second.db.QueryRow(`SELECT
			 (SELECT count(*) FROM coordination_dispatch_state WHERE task_id=? AND (status IN ('pending','enqueued','held') OR status='declined'))+
			 (SELECT count(*) FROM coordination_assignments WHERE task_id=?)`, f.task.ID, f.task.ID).Scan(&slots); err != nil || slots != 1 {
				t.Fatalf("task must retain exactly one effective durable slot: %d, %v", slots, err)
			}
			if !reflect.DeepEqual(resolvedDispatchSchema(t, second), schema) {
				t.Fatal("concurrent requests changed the production schema")
			}
		})
	}
}

func TestRequestDispatchResolvedActualSQLPlans(t *testing.T) {
	f := resolvedDispatchSetup(t, "assign")
	// Read the actual production literal rather than EXPLAIN a test-only approximation.
	source, err := os.ReadFile("coordination_dispatch.go")
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile("(?s)`(SELECT EXISTS\\(SELECT 1 FROM coordination_dispatch_state.*?)`").FindAllStringSubmatch(string(source), -1)
	if len(matches) != 1 {
		t.Fatalf("expected one actual production occupancy query, got %d", len(matches))
	}
	const original = `SELECT EXISTS(SELECT 1 FROM coordination_dispatch_state
	 WHERE task_id=? AND group_id=? AND status IN ('pending','held','enqueued'))`
	var version string
	if err := f.s.db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual Go=%s GOOS=%s GOARCH=%s GOMAXPROCS=%d TMPDIR=%s GOTMPDIR=%s", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0), os.Getenv("TMPDIR"), os.Getenv("GOTMPDIR"))
	t.Logf("production modernc SQLite=%s schema_sha256=%x; fixture-scale EXPLAIN only, no timing/benchmark claim", version, sha256.Sum256([]byte(fmt.Sprint(f.schema))))
	for _, row := range f.schema {
		name, table := row[1].(string), row[2].(string)
		if strings.HasPrefix(name, "coordination_") || strings.HasPrefix(table, "coordination_") || name == "turns_coordination_delivery" {
			t.Logf("runtime schema object type=%s name=%s table=%s SQL=%v", row[0], name, table, row[3])
		}
	}
	for _, table := range []string{"coordination_dispatch_requests", "coordination_dispatch_state", "coordination_decisions", "coordination_assignments", "coordination_assignment_dispatch", "coordination_assignment_attempts"} {
		t.Logf("runtime exact FKs %s: %v", table, messageSequenceRows(t, f.s.db, "PRAGMA foreign_key_list('"+table+"')"))
		t.Logf("runtime indexes %s: %v", table, messageSequenceRows(t, f.s.db, "PRAGMA index_list('"+table+"')"))
	}
	assignmentIndex := messageSequenceRows(t, f.s.db, `SELECT il.name FROM pragma_index_list('coordination_assignments') il
	 JOIN pragma_index_info(il.name) ii WHERE il."unique"=1 GROUP BY il.name HAVING count(*)=1 AND max(ii.name)='task_id'`)
	if len(assignmentIndex) != 1 {
		t.Fatalf("expected the existing global assignment task unique index: %v", assignmentIndex)
	}
	before := messageSequenceSnapshot(t, f.s)
	for _, taskID := range []string{f.task.ID, "missing-task"} {
		t.Logf("original production occupancy SQL task=%s", taskID)
		resolutionPlan(t, f.s, original, taskID, f.groupID)
		t.Logf("changed actual production occupancy SQL task=%s", taskID)
		plan := resolutionPlan(t, f.s, matches[0][1], taskID, f.groupID, taskID)
		if !strings.Contains(plan, "SEARCH coordination_dispatch_state USING INDEX coordination_dispatch_task_unfinished (task_id=?)") ||
			!strings.Contains(plan, "SEARCH coordination_assignments USING COVERING INDEX "+assignmentIndex[0][0].(string)+" (task_id=?)") {
			t.Fatalf("occupancy must use both existing task indexes: %s", plan)
		}
		for _, line := range strings.Split(plan, "\n") {
			if strings.HasPrefix(line, "SCAN ") && line != "SCAN CONSTANT ROW" || strings.Contains(line, "TEMP B-TREE") {
				t.Fatalf("actual production occupancy introduced a scan/sort: %s", plan)
			}
		}
	}
	resolvedDispatchPreserved(t, f.s, before, f.schema)
}
