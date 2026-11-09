package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"modernc.org/sqlite"
)

func coordinationDecideFixture(t *testing.T) (requestDispatchFixture, domain.DispatchRequest, MemberToolScope) {
	t.Helper()
	f, r := materializePending(t)
	m, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
	if err != nil || m.Delivery == nil {
		t.Fatalf("materialize real coordinator delivery: %+v, %v", m, err)
	}
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: m.Delivery.DeliveryID, MaxActive: 8, ResourceKeys: []string{"decide-resource"}})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: r.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	return f, r, scope
}

// Reflection keeps the missing-target RED compilable before the contract types exist.
func TestCoordinationDecideAtomicAssign(t *testing.T) {
	f, r, member := coordinationDecideFixture(t)
	before := messageSequenceSnapshot(t, f.s)
	method := reflect.ValueOf(f.s).MethodByName("DecideDispatch")
	if !method.IsValid() {
		t.Fatal("Store lacks atomic DecideDispatch for a legally claimed coordinator delivery")
	}
	scope := reflect.New(method.Type().In(1)).Elem()
	scope.FieldByName("Member").Set(reflect.ValueOf(member))
	p := scope.FieldByName("Provenance")
	for name, value := range map[string]string{"Kind": "coordinator", "GroupID": r.GroupID, "DeliveryID": before["turns"][0][2].(string), "MessageID": before["coordination_dispatch_state"][0][10].(string), "ConversationID": member.ConversationID, "MemberID": member.MemberID, "TopicID": r.TopicID, "RequestID": r.ID, "TaskID": r.TaskID} {
		p.FieldByName(name).SetString(value)
	}
	p.FieldByName("TaskRevision").SetInt(r.TaskRevision)
	input := reflect.New(method.Type().In(2)).Elem()
	input.FieldByName("Kind").SetString("assign")
	input.FieldByName("WorkerMemberID").SetString(f.worker)
	input.FieldByName("Reason").SetString(" 原始理由\x00 ")
	result := method.Call([]reflect.Value{reflect.ValueOf(ctx), scope, input, reflect.ValueOf("decision")})
	if !result[1].IsNil() {
		t.Fatalf("assign failed: %v; cause=%v", result[1].Interface(), errors.Unwrap(result[1].Interface().(error)))
	}
	decision := result[0].FieldByName("Decision")
	if decision.FieldByName("RequestID").String() != r.ID || decision.FieldByName("Kind").String() != "assign" || result[0].FieldByName("Assignment").IsNil() {
		t.Fatalf("missing immutable assign receipt: %+v", result[0].Interface())
	}
	after := messageSequenceSnapshot(t, f.s)
	for table, rows := range before {
		switch table {
		case "coordination_decisions", "coordination_assignments", "coordination_assignment_dispatch", "mutation_idempotency":
			if len(after[table]) != len(rows)+1 {
				t.Errorf("%s must gain exactly one row", table)
			}
		case "coordination_dispatch_state":
			if after[table][0][4] != "assigned" || after[table][0][3] != int64(3) {
				t.Errorf("request not resolved once: %v", after[table])
			}
		default:
			if !reflect.DeepEqual(rows, after[table]) {
				t.Errorf("decide changed unrelated table %s", table)
			}
		}
	}
	dispatchAssertFK(t, f.s)
}

func TestCoordinationDecideInputAndSelectedWorker(t *testing.T) {
	f, _, scope := coordinationDecideScope(t)
	valid := CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: "安排"}
	before := coordinationDecideSnapshot(t, f.s)
	for _, input := range []CoordinatorDecisionInput{{}, {Kind: "assigned", Reason: "理由"}, {Kind: "decline", WorkerMemberID: " ", Reason: "理由"}, {Kind: "assign", Reason: "理由"}, {Kind: "assign", WorkerMemberID: " padded", Reason: "理由"}, {Kind: "decline", Reason: " \t\n "}} {
		got, err := f.s.DecideDispatch(ctx, scope, input, "input")
		requestDispatchZero(t, got, err, ErrInvalid)
	}
	for _, key := range []string{"", "a,b", "with space", "\x7f", "中", strings.Repeat("x", 129)} {
		got, err := f.s.DecideDispatch(ctx, scope, valid, key)
		requestDispatchZero(t, got, err, ErrInvalid)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err := f.s.DecideDispatch(cancelled, scope, valid, "context")
	requestDispatchZero(t, got, err, context.Canceled)
	coordinationDecidePreserved(t, f.s, before)
	other := member(t, f.s, f.groupID).ID
	foreign := member(t, f.s, group(t, f.s).ID).ID
	for _, selected := range []string{f.coord, other, foreign, "missing"} {
		bad := valid
		bad.WorkerMemberID = selected
		got, err := f.s.DecideDispatch(ctx, scope, bad, "selected")
		requestDispatchZero(t, got, err, ErrConflict)
	}
	coordinationDecideAssign(t, f, scope, strings.Repeat("x", 128))
}

func TestCoordinationDecideCurrentEligibilityAndDecline(t *testing.T) {
	for _, kind := range []string{"assign", "decline"} {
		for _, change := range []string{"task-revision", "active-attempt", "submitted", "accepted", "authority", "missing-current", "binding", "policy", "coordinator-left", "worker-left", "task-assignee", "mandate-assignee", "history-revision", "history-trigger", "current-mandate", "current-topic"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				f, r, scope := coordinationDecideScope(t)
				// Borrowed fixture marks damaged immutable metadata as counterfactual and leaves static guards otherwise intact.
				code := materializeChangeEligibility(t, f, change)
				input := CoordinatorDecisionInput{Kind: kind, Reason: "决定"}
				if kind == "assign" {
					input.WorkerMemberID = f.worker
				}
				before := coordinationDecideSnapshot(t, f.s)
				got, err := f.s.DecideDispatch(ctx, scope, input, "eligibility")
				if kind == "decline" && code == domain.DispatchHeldNoEligibleWorker {
					if err != nil || got.Decision.Kind != domain.CoordinatorDecisionKindDecline || got.Assignment != nil {
						t.Fatalf("decline must allow only no_eligible_worker: %+v, %v", got, err)
					}
					if rows := messageSequenceRows(t, f.s.db, "SELECT worker_member_id,assignment_id FROM coordination_decisions WHERE request_id='"+r.ID+"'"); !reflect.DeepEqual(rows, [][]any{{nil, nil}}) {
						t.Fatalf("decline did not persist SQL NULL: %v", rows)
					}
				} else {
					want := ErrConflict
					if change == "coordinator-left" {
						want = ErrMemberToolDenied
					}
					requestDispatchZero(t, got, err, want)
					coordinationDecidePreserved(t, f.s, before)
				}
			})
		}
	}
	for _, stale := range []string{"task-revision", "authority", "binding", "policy"} {
		t.Run("no-worker-plus-"+stale, func(t *testing.T) {
			f, _, scope := coordinationDecideScope(t)
			materializeChangeEligibility(t, f, stale)
			materializeChangeEligibility(t, f, "worker-left")
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "decline", Reason: "拒绝"}, "decline")
			requestDispatchZero(t, got, err, ErrConflict)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestCoordinationDecideHistoricalReplayAndNamespaces(t *testing.T) {
	for _, kind := range []string{"assign", "decline"} {
		for _, change := range []string{"accepted", "worker-left", "policy", "binding", "authority", "intent-held", "intent-enqueued"} {
			if kind == "decline" && strings.HasPrefix(change, "intent-") {
				continue
			}
			t.Run(kind+"/"+change, func(t *testing.T) {
				f, r, scope := coordinationDecideScope(t)
				input := CoordinatorDecisionInput{Kind: kind, Reason: "不可变历史"}
				if kind == "assign" {
					input.WorkerMemberID = f.worker
				}
				original, err := f.s.DecideDispatch(ctx, scope, input, "history")
				if err != nil {
					t.Fatalf("setup: %v, cause=%v", err, errors.Unwrap(err))
				}
				if change == "intent-held" {
					coordinationDecideExec(t, f.s, "UPDATE coordination_assignment_dispatch SET status='held',generation=2,updated_at=?,held_reason='worker_inactive' WHERE assignment_id=?", time.Now().UTC().Format(timestampLayout), original.Assignment.ID)
				} else if change == "intent-enqueued" {
					coordinationDecideWorker(t, f, r, original, true)
				} else {
					materializeChangeEligibility(t, f, change)
				}
				before := coordinationDecideSnapshot(t, f.s)
				second := queueHandle(t, f.path, false)
				got, err := second.DecideDispatch(ctx, scope, input, "history")
				if err != nil || !reflect.DeepEqual(got, original) {
					t.Fatalf("historical replay rechecked current eligibility/intent: %+v, %v", got, err)
				}
				bad := input
				bad.Reason += " changed"
				got, err = second.DecideDispatch(ctx, scope, bad, "history")
				requestDispatchZero(t, got, err, ErrConflict)
				got, err = second.DecideDispatch(ctx, scope, input, "new-key")
				requestDispatchZero(t, got, err, ErrConflict)
				coordinationDecidePreserved(t, second, before)
			})
		}
	}
	f, _, scope := coordinationDecideScope(t)
	for _, namespace := range []mutationScope{{operation: "other_operation", actorID: f.coord, groupID: f.groupID, key: "shared"}, {operation: "decide_dispatch", actorID: "user", groupID: f.groupID, key: "shared"}, {operation: "decide_dispatch", actorID: f.coord, groupID: "other-group", key: "shared"}} {
		coordinationDecideExec(t, f.s, "INSERT INTO mutation_idempotency(operation,actor_id,group_id,idempotency_key,payload,record_id) VALUES(?,?,?,?,?,?)", namespace.operation, namespace.actorID, namespace.groupID, namespace.key, []byte("other"), "other-record")
	}
	original := coordinationDecideAssign(t, f, scope, "shared")
	// Counterfactual ledger corruption: same payload pointing at a different record must not disclose history.
	coordinationDecideExec(t, f.s, "UPDATE mutation_idempotency SET record_id='different-request-decision' WHERE operation='decide_dispatch' AND actor_id=? AND group_id=? AND idempotency_key='shared'", f.coord, f.groupID)
	got, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: original.Decision.Reason}, "shared")
	requestDispatchZero(t, got, err, ErrConflict)
}

func TestCoordinationDecideWriteAbortIgnoreAndKnownCommitRefusal(t *testing.T) {
	for _, kind := range []string{"assign", "decline"} {
		for _, point := range []string{"decision", "assignment", "intent", "state", "receipt"} {
			if kind == "decline" && (point == "assignment" || point == "intent") {
				continue
			}
			for _, mode := range []string{"abort", "ignore"} {
				t.Run(kind+"/"+point+"/"+mode, func(t *testing.T) {
					f, _, scope := coordinationDecideScope(t)
					observer := queueHandle(t, f.path, false)
					table := map[string]string{"decision": "coordination_decisions", "assignment": "coordination_assignments", "intent": "coordination_assignment_dispatch", "state": "coordination_dispatch_state", "receipt": "mutation_idempotency"}[point]
					op, timing, body := "INSERT", "AFTER", "SELECT RAISE(ABORT,'PRIVATE SQL FAULT');"
					if point == "state" {
						op = "UPDATE"
					}
					if mode == "ignore" {
						timing, body = "BEFORE", "SELECT RAISE(IGNORE);"
					}
					coordinationDecideExec(t, f.s, "CREATE TRIGGER decide_write_fault "+timing+" "+op+" ON "+table+" BEGIN "+body+" END")
					before := coordinationDecideSnapshot(t, f.s)
					input := CoordinatorDecisionInput{Kind: kind, Reason: "PRIVATE PAYLOAD"}
					if kind == "assign" {
						input.WorkerMemberID = f.worker
					}
					got, err := f.s.DecideDispatch(ctx, scope, input, "write-key")
					requestDispatchZero(t, got, err, nil)
					if mode == "abort" {
						var cause *sqlite.Error
						if !errors.As(err, &cause) {
							t.Fatalf("SQL cause lost: %v", err)
						}
					} else if !errors.Is(err, ErrConflict) {
						t.Fatalf("ignored write not a conflict: %v", err)
					}
					if strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), scope.Member.Claim.Token) {
						t.Fatal("unsafe public error")
					}
					coordinationDecidePreserved(t, f.s, before)
					coordinationDecidePreserved(t, observer, before)
					coordinationDecideExec(t, f.s, "DROP TRIGGER decide_write_fault")
					if _, err := observer.DecideDispatch(ctx, scope, input, "write-key"); err != nil {
						t.Fatalf("rollback burned key: %v, cause=%v", err, errors.Unwrap(err))
					}
					dispatchAssertFK(t, observer)
				})
			}
		}
	}
	f, r, scope := coordinationDecideScope(t)
	coordinationDecideExec(t, f.s, "CREATE TABLE decide_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)")
	coordinationDecideExec(t, f.s, "CREATE TRIGGER decide_commit_fault AFTER INSERT ON mutation_idempotency BEGIN INSERT INTO decide_commit_guard VALUES('missing'); END")
	before := coordinationDecideSnapshot(t, f.s)
	got, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: "安排"}, "commit-key")
	requestDispatchZero(t, got, err, nil)
	var cause *sqlite.Error
	if !errors.As(err, &cause) || cause.Code() != 787 || !strings.Contains(err.Error(), "结果未确认") {
		t.Fatalf("Commit classification/cause: %v, cause=%v", err, errors.Unwrap(err))
	}
	observer := queueHandle(t, f.path, false)
	coordinationDecidePreserved(t, observer, before)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, f.path, false)
	coordinationDecidePreserved(t, reopened, before)
	coordinationDecideExec(t, reopened, "DROP TRIGGER decide_commit_fault")
	got, err = reopened.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: "安排"}, "commit-key")
	if err != nil || got.Decision.RequestID != r.ID {
		t.Fatalf("known FK refusal burned key: %+v, %v", got, err)
	}
}

func TestCoordinationDecideFinalReadbackCASAndReadonlyFences(t *testing.T) {
	for _, fault := range []string{"decision", "assignment", "intent", "state", "receipt", "cas", "token", "cancel", "member", "task", "authority", "worker", "provenance", "history"} {
		t.Run(fault, func(t *testing.T) {
			f, r, scope := coordinationDecideScope(t)
			point, op := "mutation_idempotency", "INSERT"
			body := "DELETE FROM mutation_idempotency WHERE operation='decide_dispatch';"
			switch fault {
			case "decision":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_decisions_no_update")
				body = "UPDATE coordination_decisions SET reason='replacement';"
			case "assignment":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignments_no_update")
				body = "UPDATE coordination_assignments SET created_at='2020-01-01T00:00:00.000000000Z';"
			case "intent":
				body = "UPDATE coordination_assignment_dispatch SET generation=generation+1,status='held',held_reason='worker_inactive';"
			case "state":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_dispatch_state_update")
				body = "UPDATE coordination_dispatch_state SET updated_at='2020-01-01T00:00:00.000000000Z';"
			case "cas":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_dispatch_state_update")
				point = "coordination_decisions"
				body = "UPDATE coordination_dispatch_state SET generation=generation+1;"
			case "token":
				body = "UPDATE turns SET claim_token='rotated' WHERE id='" + scope.Member.Claim.TurnID + "';"
			case "cancel":
				body = "UPDATE turns SET cancel_requested_at='2020-01-01T00:00:00.000000000Z' WHERE id='" + scope.Member.Claim.TurnID + "';"
			case "member":
				body = "UPDATE members SET status='left' WHERE id='" + f.coord + "';"
			case "task":
				body = "UPDATE tasks SET revision=revision+1 WHERE id='" + r.TaskID + "';"
			case "authority":
				body = "UPDATE task_coordination SET authority_generation=authority_generation+1 WHERE task_id='" + r.TaskID + "';"
			case "worker":
				body = "UPDATE members SET status='left' WHERE id='" + f.worker + "';"
			case "provenance":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_dispatch_deliveries_identity")
				body = "UPDATE deliveries SET turn_id='' WHERE id='" + scope.Provenance.DeliveryID + "';"
			case "history":
				d := queueDelivery(t, f.s, r.GroupID, f.worker, "history-injection")
				turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: d.ID, MaxActive: 8, ResourceKeys: []string{"history-lock"}})
				if err != nil {
					t.Fatal(err)
				}
				body = fmt.Sprintf("INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,turn_id,created_at) VALUES('injected-attempt','%s','%s',1,'%s','%s','2020-01-01T00:00:00.000000000Z');", r.TaskID, r.GroupID, f.worker, turn.ID)
			}
			// Counterfactual replacement fields bypass only the named immutable guards above; lifecycle/eligibility mutations are legal SQL.
			coordinationDecideExec(t, f.s, "CREATE TRIGGER decide_final_fault AFTER "+op+" ON "+point+" BEGIN "+body+" END")
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: "安排"}, "final-key")
			requestDispatchZero(t, got, err, nil)
			coordinationDecidePreserved(t, f.s, before)
			observer := queueHandle(t, f.path, false)
			coordinationDecidePreserved(t, observer, before)
			coordinationDecideExec(t, f.s, "DROP TRIGGER decide_final_fault")
			if _, err := observer.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: "安排"}, "final-key"); err != nil {
				t.Fatalf("failed final readback burned key: %v, cause=%v", err, errors.Unwrap(err))
			}
		})
	}
}

// These wrappers alter only the acknowledgment boundary around a real SQLite transaction.
// They do not replace any Store, SQL, constraint, locking, or persistence behavior.
type coordinationDecideDriver struct {
	base      driver.Driver
	mode      string
	cause     error
	afterAuth func()
	begins    []driver.TxOptions
	queries   []string
	inTx      bool
}
type coordinationDecideConn struct {
	driver.Conn
	owner *coordinationDecideDriver
}
type coordinationDecideTx struct {
	driver.Tx
	owner *coordinationDecideDriver
}
type coordinationDecideRows struct {
	driver.Rows
	after func()
}

func (d *coordinationDecideDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &coordinationDecideConn{conn, d}, nil
}
func (c *coordinationDecideConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.owner.begins = append(c.owner.begins, opts)
	c.owner.inTx = true
	return &coordinationDecideTx{tx, c.owner}, nil
}
func (c *coordinationDecideConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
func (c *coordinationDecideConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if !c.owner.inTx {
		return nil, errors.New("query escaped its transaction")
	}
	c.owner.queries = append(c.owner.queries, query)
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if c.owner.afterAuth != nil && strings.Contains(query, "SELECT 1 FROM groups g") {
		after := c.owner.afterAuth
		c.owner.afterAuth = nil
		return &coordinationDecideRows{rows, after}, nil
	}
	return rows, nil
}
func (r *coordinationDecideRows) Next(values []driver.Value) error {
	err := r.Rows.Next(values)
	if err == nil && r.after != nil {
		after := r.after
		r.after = nil
		after()
	}
	return err
}
func (t *coordinationDecideTx) Rollback() error { t.owner.inTx = false; return t.Tx.Rollback() }
func (t *coordinationDecideTx) Commit() error {
	t.owner.inTx = false
	if t.owner.mode == "refuse" {
		if err := t.Tx.Rollback(); err != nil {
			return err
		}
		return t.owner.cause
	}
	if err := t.Tx.Commit(); err != nil {
		return err
	}
	if t.owner.mode == "ack-error" {
		return t.owner.cause
	}
	return nil
}
func coordinationDecideWrapped(t *testing.T, path string, mode string, cause error) (*Store, *coordinationDecideDriver) {
	t.Helper()
	d := &coordinationDecideDriver{base: &sqlite.Driver{}, mode: mode, cause: cause}
	name := fmt.Sprintf("coordination-decide-%p", d)
	sql.Register(name, d)
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"}
	db, err := sql.Open(name, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	t.Cleanup(func() { s.Close() })
	return s, d
}

func TestCoordinationDecideCommitUnknownAndOriginalKeyOwnership(t *testing.T) {
	for _, mode := range []string{"ack-error", "refuse", "refuse-competing-key"} {
		t.Run(mode, func(t *testing.T) {
			f, r, scope := coordinationDecideScope(t)
			cause := errors.Join(errors.New("PRIVATE COMMIT ACK"), sql.ErrNoRows)
			wrappedMode := mode
			if mode == "refuse-competing-key" {
				wrappedMode = "refuse"
			}
			wrapped, _ := coordinationDecideWrapped(t, f.path, wrappedMode, cause)
			before := coordinationDecideSnapshot(t, f.s)
			input := CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: "同内容"}
			got, err := wrapped.DecideDispatch(ctx, scope, input, "original-key")
			requestDispatchZero(t, got, err, cause)
			if !errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "结果未确认") || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("unknown commit cause/text: %v, cause=%v", err, errors.Unwrap(err))
			}
			if err := wrapped.Close(); err != nil {
				t.Fatal(err)
			}
			observer := queueHandle(t, f.path, false)
			var ledger int
			if err := observer.db.QueryRow("SELECT count(*) FROM mutation_idempotency WHERE operation='decide_dispatch' AND idempotency_key='original-key'").Scan(&ledger); err != nil {
				t.Fatal(err)
			}
			actual, readErr := observer.GetDispatchDecision(ctx, r.GroupID, r.ID)
			if mode == "ack-error" {
				if readErr != nil || ledger != 1 || actual.Assignment == nil {
					t.Fatalf("committed business/key receipt not found: %+v, %v, ledger=%d", actual, readErr, ledger)
				}
				committed := coordinationDecideSnapshot(t, observer)
				replayed, err := observer.DecideDispatch(ctx, scope, input, "original-key")
				if err != nil || !reflect.DeepEqual(replayed, actual) {
					t.Fatalf("authorized original-key replay: %+v, %v", replayed, err)
				}
				coordinationDecidePreserved(t, observer, committed)
			} else {
				requestDispatchZero(t, actual, readErr, ErrNotFound)
				if ledger != 0 {
					t.Fatal("precommit refusal persisted original key")
				}
				coordinationDecidePreserved(t, observer, before)
				key := "original-key"
				if mode == "refuse-competing-key" {
					key = "other-key"
				}
				created, err := observer.DecideDispatch(ctx, scope, input, key)
				if err != nil {
					t.Fatalf("authorized retry/competition: %v", err)
				}
				business, err := observer.GetDispatchDecision(ctx, r.GroupID, r.ID)
				if err != nil || !reflect.DeepEqual(business, created) {
					t.Fatalf("GET business state: %+v, %v", business, err)
				}
				if mode == "refuse-competing-key" {
					if err := observer.db.QueryRow("SELECT count(*) FROM mutation_idempotency WHERE operation='decide_dispatch' AND idempotency_key='original-key'").Scan(&ledger); err != nil || ledger != 0 {
						t.Fatalf("GET falsely established original-key ownership: %d, %v", ledger, err)
					}
					before := coordinationDecideSnapshot(t, observer)
					got, err := observer.DecideDispatch(ctx, scope, input, "original-key")
					requestDispatchZero(t, got, err, ErrConflict)
					coordinationDecidePreserved(t, observer, before)
				}
			}
			dispatchAssertFK(t, observer)
		})
	}
}

func TestCoordinationDecideTwoHandleRacesAndResolvedSlot(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		for _, mode := range []string{"same-key", "different-key", "new-request"} {
			t.Run(fmt.Sprintf("deferred=%t/%s", deferred, mode), func(t *testing.T) {
				f, r, scope := coordinationDecideScope(t)
				a, b := queueHandle(t, f.path, deferred), queueHandle(t, f.path, deferred)
				input := CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: "安排"}
				type result struct {
					receipt domain.DecisionReceipt
					err     error
				}
				start := make(chan struct{})
				results := make(chan result, 2)
				go func() { <-start; v, e := a.DecideDispatch(ctx, scope, input, "one"); results <- result{v, e} }()
				go func() {
					<-start
					if mode == "new-request" {
						_, e := b.RequestDispatch(ctx, r.GroupID, r.TaskID, 1, 1, "另一个请求", "request-race")
						results <- result{err: e}
						return
					}
					key := "one"
					if mode == "different-key" {
						key = "two"
					}
					v, e := b.DecideDispatch(ctx, scope, input, key)
					results <- result{v, e}
				}()
				close(start)
				x, y := <-results, <-results
				if mode == "same-key" {
					if x.err != nil || y.err != nil || !reflect.DeepEqual(x.receipt, y.receipt) {
						t.Fatalf("same-key race: %+v / %+v", x, y)
					}
				} else {
					successes := 0
					for _, out := range []result{x, y} {
						if out.err == nil {
							successes++
						} else if !errors.Is(out.err, ErrConflict) {
							t.Fatalf("unexpected race error: %v, cause=%v", out.err, errors.Unwrap(out.err))
						}
					}
					if successes != 1 {
						t.Fatalf("race has %d successes: %+v / %+v", successes, x, y)
					}
				}
				var decisions, assignments int
				if err := a.db.QueryRow("SELECT (SELECT count(*) FROM coordination_decisions),(SELECT count(*) FROM coordination_assignments)").Scan(&decisions, &assignments); err != nil || decisions != 1 || assignments != 1 {
					t.Fatalf("duplicate/partial work: %d/%d, %v", decisions, assignments, err)
				}
				dispatchAssertFK(t, a)
			})
		}
	}
}

func TestCoordinationDecideHistoricalNonterminalPlanAndScale(t *testing.T) {
	for _, state := range []string{"unknown", "cancel_requested"} {
		t.Run(state, func(t *testing.T) {
			s, path, thread, _ := requestWorkFixture(t)
			coord, worker := member(t, s, thread.GroupID).ID, member(t, s, thread.GroupID).ID
			if _, err := s.SetCoordinator(ctx, thread.GroupID, coord, 0, "config"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetDispatchPolicy(ctx, thread.GroupID, []string{worker}, 0, "config"); err != nil {
				t.Fatal(err)
			}
			task, err := s.CreateTask(ctx, taskInput(thread.GroupID))
			if err != nil {
				t.Fatal(err)
			}
			f := requestDispatchFixture{s: s, path: path, groupID: thread.GroupID, worker: worker, coord: coord, task: task}
			d := queueDelivery(t, f.s, f.groupID, f.worker, "old-worker")
			turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, MaxActive: 8, ResourceKeys: []string{"old-history-resource"}})
			if err != nil {
				t.Fatal(err)
			}
			claim := TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken}
			bound, err := f.s.BindMemberTool(ctx, claim)
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := f.s.ActivateTaskAttempt(ctx, ActivateTaskAttemptInput{GroupID: f.groupID, TaskID: f.task.ID, ExpectedRevision: 1, ActorID: f.worker, MemberScope: &bound, IdempotencyKey: "old-attempt"})
			if err != nil {
				t.Fatal(err)
			}
			submission, err := f.s.SubmitTask(ctx, SubmitTaskInput{GroupID: f.groupID, TaskID: f.task.ID, AttemptID: attempt.ID, ExpectedRevision: 1, ActorID: f.worker, MemberScope: &bound, Summary: "历史待审结果", IdempotencyKey: "old-submit"})
			if err != nil {
				t.Fatal(err)
			}
			review := taskReviewInput(submission)
			review.Outcome = domain.ReviewOutcomeRejected
			if _, err := f.s.ReviewTask(ctx, review); err != nil {
				t.Fatal(err)
			}
			queueEvent(t, f.s, claim, provider.AcceptedEvent, "", "")
			if state == "unknown" {
				receipt := queueReceipt(provider.OutcomeUnknown)
				receipt.OutputComplete = false
				if _, err := f.s.ApplyTurnReceipt(ctx, claim, receipt); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.s.RequestTurnCancel(ctx, f.groupID, turn.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.task, err = f.s.GetTask(ctx, f.groupID, f.task.ID)
			if err != nil || f.task.Revision != 2 || f.task.ActiveAttemptID != "" {
				t.Fatalf("review did not reopen current task: %+v, %v", f.task, err)
			}
			message, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: thread.TopicID, ActorID: "user", Content: "首次授权已有历史的任务"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.s.RequestWork(ctx, f.groupID, domain.RequestWorkRequest{TopicID: thread.TopicID, MessageID: message.ID, Mode: domain.RoutingDecisionLinkExisting, Reason: "当前版本首次授权", ExistingTask: &domain.ExistingTaskRequest{TaskID: f.task.ID, ExpectedRevision: 2}}, "admit-history")
			if err != nil {
				t.Fatal(err)
			}
			current, err := f.s.GetTaskCoordination(ctx, f.groupID, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			r, err := f.s.RequestDispatch(ctx, f.groupID, f.task.ID, 2, current.Coordination.AuthorityGeneration, "安排", "new-revision")
			if err != nil {
				t.Fatal(err)
			}
			m, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, time.Now())
			if err != nil || m.Delivery == nil {
				t.Fatalf("materialize: %+v, %v", m, err)
			}
			coordinator, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: m.Delivery.DeliveryID, MaxActive: 8})
			if err != nil {
				t.Fatal(err)
			}
			coordinatorScope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: r.GroupID, TurnID: coordinator.ID, Token: coordinator.ClaimToken})
			if err != nil {
				t.Fatal(err)
			}
			scope := CoordinationToolScope{Member: coordinatorScope, Provenance: CoordinationRoleProvenance{Kind: CoordinationRoleCoordinator, GroupID: r.GroupID, DeliveryID: m.Delivery.DeliveryID, MessageID: m.Delivery.MessageID, ConversationID: coordinatorScope.ConversationID, MemberID: coordinatorScope.MemberID, TopicID: coordinatorScope.TopicID, RequestID: r.ID, TaskID: r.TaskID, TaskRevision: r.TaskRevision}}
			var coordinatorAttempts int
			if err := f.s.db.QueryRow("SELECT count(*) FROM task_attempts WHERE turn_id=?", coordinator.ID).Scan(&coordinatorAttempts); err != nil || coordinatorAttempts != 0 {
				t.Fatalf("coordinator incorrectly counted as worker history: %d, %v", coordinatorAttempts, err)
			}
			// Find the actual production query, not a separately maintained test copy.
			file, err := parser.ParseFile(token.NewFileSet(), "coordination_decide.go", nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			query := ""
			ast.Inspect(file, func(n ast.Node) bool {
				literal, ok := n.(*ast.BasicLit)
				if ok && literal.Kind == token.STRING {
					value, e := strconv.Unquote(literal.Value)
					if e == nil && strings.Contains(value, "FROM task_attempts a JOIN turns t") {
						query = value
					}
				}
				return true
			})
			if query == "" {
				t.Fatal("production historical predicate not found")
			}
			plan := resolutionPlan(t, f.s, query, r.TaskID, r.MandateID, r.GroupID, r.PolicyRevision, f.worker, r.CoordinatorMemberID, r.TaskID, "", r.GroupID, r.TaskID)
			if !strings.Contains(plan, "task_attempts_group_task_created_id") || strings.Contains(strings.ToUpper(query), "LIMIT") {
				t.Fatal("history lookup lost task index or truncates history")
			}
			// One legal terminal Turn supplies many guard-checked history rows, newer than the old nonterminal attempt.
			terminalMember := member(t, f.s, r.GroupID).ID
			terminalDelivery := queueDelivery(t, f.s, r.GroupID, terminalMember, "terminal-history")
			terminal, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: terminalDelivery.ID, MaxActive: 8})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.ApplyTurnReceipt(ctx, TurnClaim{GroupID: r.GroupID, TurnID: terminal.ID, Token: terminal.ClaimToken}, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
				t.Fatal(err)
			}
			for _, count := range []int{0, 64, 4096} {
				if count > 0 {
					coordinationDecideExec(t, f.s, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<?)
				 INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,turn_id,created_at)
				 SELECT printf('scale-%d-%06d',?,i),?,?,?,?,?,'2030-01-01T00:00:00.000000000Z' FROM n`, count, count, r.TaskID, r.GroupID, r.TaskRevision, terminalMember, terminal.ID)
				}
				before := coordinationDecideSnapshot(t, f.s)
				started := time.Now()
				got, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: "不可绕过旧执行"}, "history-fence")
				elapsed := time.Since(started)
				requestDispatchZero(t, got, err, ErrConflict)
				coordinationDecidePreserved(t, f.s, before)
				var total int
				if err := f.s.db.QueryRow("SELECT count(*) FROM task_attempts WHERE group_id=? AND task_id=?", r.GroupID, r.TaskID).Scan(&total); err != nil {
					t.Fatal(err)
				}
				t.Logf("state=%s task_history_rows=%d old_nonterminal_attempt=%s coordinator_has_no_attempt=true elapsed=%s (not a hard work bound)", state, total, attempt.ID, elapsed)
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "decline", Reason: "不创建执行"}, "decline-with-history")
			if err != nil || got.Assignment != nil {
				t.Fatalf("decline should not grant work or release unknown claims: %+v, %v", got, err)
			}
			for _, table := range []string{"turns", "task_attempts", "resource_claims", "deliveries"} {
				if !reflect.DeepEqual(before[table], coordinationDecideSnapshot(t, f.s)[table]) {
					t.Fatalf("decline changed %s", table)
				}
			}
			dispatchAssertFK(t, f.s)
		})
	}
}

func coordinationDecideBindRequest(t *testing.T, s *Store, r domain.DispatchRequest) CoordinationToolScope {
	t.Helper()
	materialized, err := s.MaterializeDispatchRequest(ctx, r.ID, 1, time.Now())
	if err != nil || materialized.Delivery == nil {
		t.Fatalf("legal materialization: %+v, %v", materialized, err)
	}
	d := materialized.Delivery
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: d.DeliveryID, MaxActive: 8, ResourceKeys: []string{"legal-scope-resource"}})
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.BindMemberTool(ctx, TurnClaim{GroupID: r.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	return CoordinationToolScope{Member: member, Provenance: CoordinationRoleProvenance{Kind: CoordinationRoleCoordinator, GroupID: r.GroupID,
		DeliveryID: d.DeliveryID, MessageID: d.MessageID, ConversationID: d.ConversationID, MemberID: r.CoordinatorMemberID, TopicID: r.TopicID,
		RequestID: r.ID, TaskID: r.TaskID, TaskRevision: r.TaskRevision}}
}

func TestCoordinationDecideAssigneeIntersectionDependenciesAndOpaqueTopic(t *testing.T) {
	for _, mode := range []string{"matching", "wrong-selected", "task-intersection", "mandate-intersection", "empty-topic", "opaque-topic", "invalid-utf8-topic"} {
		t.Run(mode, func(t *testing.T) {
			s, _, thread, input := requestWorkFixture(t)
			coord, first, second := member(t, s, thread.GroupID).ID, member(t, s, thread.GroupID).ID, member(t, s, thread.GroupID).ID
			if _, err := s.SetCoordinator(ctx, thread.GroupID, coord, 0, "config"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetDispatchPolicy(ctx, thread.GroupID, []string{first, second}, 0, "config"); err != nil {
				t.Fatal(err)
			}
			parent := createTestTask(t, s, thread.GroupID)
			input.NewTask.AssigneeID = first
			input.NewTask.DependencyIDs = []string{parent.ID}
			if mode == "empty-topic" || mode == "opaque-topic" || mode == "invalid-utf8-topic" {
				input.TopicID = ""
				if mode == "opaque-topic" {
					input.TopicID = " \n\t\x00opaque "
				}
				if mode == "invalid-utf8-topic" {
					input.TopicID = string([]byte{0xff})
				}
				m, err := s.CreateMessage(ctx, MessageRequest{GroupID: thread.GroupID, TopicID: input.TopicID, ActorID: "user", Content: "授权"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				input.MessageID = m.ID
			}
			a, err := s.RequestWork(ctx, thread.GroupID, input, "work")
			if err != nil {
				t.Fatal(err)
			}
			r, err := s.RequestDispatch(ctx, thread.GroupID, a.Task.ID, 1, 1, "安排", "dispatch")
			if err != nil {
				t.Fatal(err)
			}
			scope := coordinationDecideBindRequest(t, s, r)
			if scope.Member.TopicID != input.TopicID || scope.Provenance.TopicID != input.TopicID {
				t.Fatal("opaque topic was normalized")
			}
			selected := first
			if mode == "wrong-selected" {
				selected = second
			}
			if mode == "task-intersection" {
				coordinationDecideExec(t, s, "UPDATE tasks SET assignee_id=? WHERE id=?", second, r.TaskID)
			}
			if mode == "mandate-intersection" {
				coordinationDecideExec(t, s, "UPDATE tasks SET assignee_id=NULL WHERE id=?", r.TaskID)
				selected = second
			}
			before := coordinationDecideSnapshot(t, s)
			got, err := s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: selected, Reason: "记录但不激活"}, "assignment")
			if mode == "wrong-selected" || mode == "task-intersection" || mode == "mandate-intersection" {
				requestDispatchZero(t, got, err, ErrConflict)
				coordinationDecidePreserved(t, s, before)
			} else {
				if err != nil || got.Assignment == nil {
					t.Fatalf("unaccepted dependency should permit an Assignment: %+v, %v", got, err)
				}
				after := coordinationDecideSnapshot(t, s)
				for _, table := range []string{"task_attempts", "tasks", "task_dependencies", "turns", "resource_claims", "messages", "deliveries"} {
					if !reflect.DeepEqual(before[table], after[table]) {
						t.Fatalf("decision activated or changed %s", table)
					}
				}
				view, err := s.GetAssignment(ctx, r.GroupID, got.Assignment.ID)
				if err != nil || view.Request.TopicID != input.TopicID || view.Task.DependencyIDs[0] != parent.ID {
					t.Fatalf("exact dependent/opaque view: %+v, %v", view, err)
				}
			}
		})
	}
}

func TestCoordinationDecideGenerationBoundaryAndDeclinedOwnership(t *testing.T) {
	for _, generation := range []int64{9223372036854775806, 9223372036854775807} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			f, r, scope := coordinationDecideScope(t)
			// Counterfactual counter boundary skips only the state transition guard; values still pass v14 CHECK/FK.
			coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_dispatch_state_update")
			coordinationDecideExec(t, f.s, "UPDATE coordination_dispatch_state SET generation=?,attempt_count=9223372036854775807 WHERE request_id=?", generation, r.ID)
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "decline", Reason: "保留 slot"}, "decline")
			if generation == 9223372036854775807 {
				requestDispatchZero(t, got, err, ErrInvalid)
				coordinationDecidePreserved(t, f.s, before)
				return
			}
			if err != nil || got.Assignment != nil {
				t.Fatalf("last increment: %+v, %v", got, err)
			}
			state, err := f.s.GetDispatchRequest(ctx, r.GroupID, r.ID)
			if err != nil || state.State.Generation != 9223372036854775807 || state.State.AttemptCount != 9223372036854775807 {
				t.Fatalf("overflow or counter changed: %+v, %v", state, err)
			}
			before = coordinationDecideSnapshot(t, f.s)
			_, err = f.s.RequestDispatch(ctx, r.GroupID, r.TaskID, 1, 1, "新请求", "new-request")
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("decline released Task slot: %v", err)
			}
			coordinationDecidePreserved(t, f.s, before)
			if _, err := f.s.RequestTurnCancel(ctx, r.GroupID, scope.Member.Claim.TurnID); err != nil {
				t.Fatal(err)
			}
			before = coordinationDecideSnapshot(t, f.s)
			replayed, err := f.s.GetDispatchDecision(ctx, r.GroupID, r.ID)
			if err != nil || !reflect.DeepEqual(replayed, got) {
				t.Fatalf("trusted cancelled history: %+v, %v", replayed, err)
			}
			coordinationDecidePreserved(t, f.s, before)
			if len(before["resource_claims"]) != 1 {
				t.Fatal("decline/cancel released coordinator resource")
			}
		})
	}
}

func TestCoordinationDecideDeclineLastWriteAndForeignReceipt(t *testing.T) {
	for _, fault := range []string{"token", "task"} {
		t.Run("decline-"+fault, func(t *testing.T) {
			f, r, scope := coordinationDecideScope(t)
			materializeChangeEligibility(t, f, "worker-left")
			body := "UPDATE turns SET claim_token='rotated' WHERE id='" + scope.Member.Claim.TurnID + "';"
			want := ErrMemberToolDenied
			if fault == "task" {
				body = "UPDATE tasks SET revision=revision+1 WHERE id='" + r.TaskID + "';"
				want = ErrConflict
			}
			coordinationDecideExec(t, f.s, "CREATE TRIGGER decide_decline_last AFTER INSERT ON mutation_idempotency BEGIN "+body+" END")
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "decline", Reason: "没有 worker 也不能吞其他失效"}, "decline-last")
			requestDispatchZero(t, got, err, want)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
	f, r, scope := coordinationDecideScope(t)
	original := coordinationDecideAssign(t, f, scope, "original")
	otherCoordinator := member(t, f.s, f.groupID).ID
	if _, err := f.s.SetCoordinator(ctx, f.groupID, otherCoordinator, 1, "other-coordinator"); err != nil {
		t.Fatal(err)
	}
	message, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: r.GroupID, TopicID: r.TopicID, ActorID: "user", Content: "另一任务"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := f.s.RequestWork(ctx, r.GroupID, domain.RequestWorkRequest{TopicID: r.TopicID, MessageID: message.ID, Mode: domain.RoutingDecisionCreateNew, Reason: "另一个授权", NewTask: &domain.NewTaskRequest{Title: "另一个目标", Acceptance: "独立验收"}}, "other-work")
	if err != nil {
		t.Fatal(err)
	}
	otherRequest, err := f.s.RequestDispatch(ctx, r.GroupID, admission.Task.ID, 1, 1, "安排另一任务", "other-request")
	if err != nil {
		t.Fatal(err)
	}
	otherScope := coordinationDecideBindRequest(t, f.s, otherRequest)
	other, err := f.s.DecideDispatch(ctx, otherScope, CoordinatorDecisionInput{Kind: "decline", Reason: "另一决定"}, "other-decision")
	if err != nil {
		t.Fatal(err)
	}
	// Counterfactual same-payload ledger tampering now names a real different request/Turn/Decision.
	coordinationDecideExec(t, f.s, "UPDATE mutation_idempotency SET record_id=? WHERE operation='decide_dispatch' AND actor_id=? AND group_id=? AND idempotency_key='original'", other.Decision.ID, f.coord, f.groupID)
	before := coordinationDecideSnapshot(t, f.s)
	input := CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: original.Decision.Reason}
	got, err := f.s.DecideDispatch(ctx, scope, input, "original")
	requestDispatchZero(t, got, err, ErrConflict)
	coordinationDecidePreserved(t, f.s, before)
	coordinationDecideExec(t, f.s, "UPDATE mutation_idempotency SET record_id=? WHERE operation='decide_dispatch' AND actor_id=? AND group_id=? AND idempotency_key='original'", original.Decision.ID, f.coord, f.groupID)
	got, err = f.s.DecideDispatch(ctx, scope, input, "original")
	if err != nil || !reflect.DeepEqual(got, original) {
		t.Fatalf("original history/Turn binding: %+v, %v", got, err)
	}
}

func TestCoordinationDecideSQLFailureIsNotNoWorkerOrNoRole(t *testing.T) {
	for _, table := range []string{"coordination_assignment_dispatch", "group_dispatch_policy_revisions"} {
		t.Run(table, func(t *testing.T) {
			f, _, scope := coordinationDecideScope(t)
			// Counterfactual missing metadata table tests real SQL error preservation after the first scope fence.
			coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=OFF")
			coordinationDecideExec(t, f.s, "DROP TABLE "+table)
			coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=ON")
			got, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "decline", Reason: "不吞 SQL 错误"}, "sql-fault")
			requestDispatchZero(t, got, err, nil)
			var cause *sqlite.Error
			if !errors.As(err, &cause) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrMemberToolDenied) || strings.Contains(err.Error(), table) {
				t.Fatalf("SQL failure disguised or disclosed: %v, cause=%v", err, errors.Unwrap(err))
			}
		})
	}
	for _, mode := range []string{"ABORT,'PRIVATE SQL'", "IGNORE"} {
		t.Run(mode, func(t *testing.T) {
			f, _, scope := coordinationDecideScope(t)
			coordinationDecideExec(t, f.s, "CREATE TRIGGER decide_first_scope_fault BEFORE UPDATE OF claim_token ON turns BEGIN SELECT RAISE("+mode+"); END")
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "decline", Reason: "首写兼容例外"}, "sql-fault")
			requestDispatchZero(t, got, err, ErrMemberToolDenied)
			var cause *sqlite.Error
			if errors.As(err, &cause) || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("shared first-fence Denied compatibility changed")
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}
