package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"modernc.org/sqlite"
)

func TestActivateAssignmentReservationGate(t *testing.T) {
	for _, state := range []string{"pending", "enqueued", "held", "assigned", "declined"} {
		t.Run(state, func(t *testing.T) {
			var f requestDispatchFixture
			var r domain.DispatchRequest
			if state == "enqueued" || state == "assigned" || state == "declined" {
				var scope CoordinationToolScope
				f, r, scope = coordinationDecideScope(t)
				if state == "assigned" {
					coordinationDecideAssign(t, f, scope, "assignment")
				} else if state == "declined" {
					if _, err := f.s.DecideDispatch(ctx, scope, CoordinatorDecisionInput{Kind: "decline", Reason: "不分配"}, "decline"); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				f = requestDispatchSetup(t)
				var err error
				r, err = f.s.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "请求", "request")
				if err != nil {
					t.Fatal(err)
				}
				if state == "held" {
					coordinationDecideExec(t, f.s, "UPDATE coordination_dispatch_state SET status='held',generation=generation+1,held_reason='no_eligible_worker' WHERE request_id=?", r.ID)
				}
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.ActivateTaskAttempt(ctx, ActivateTaskAttemptInput{GroupID: r.GroupID, TaskID: r.TaskID, ExpectedRevision: r.TaskRevision, ActorID: "user", IdempotencyKey: "reserved"})
			requestDispatchZero(t, got, err, ErrConflict)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestActivateAssignmentKeyedLedgerAtomicity(t *testing.T) {
	for _, fault := range []string{"ignore", "abort", "receipt-missing", "receipt-payload", "receipt-result"} {
		t.Run(fault, func(t *testing.T) {
			s, path := openTestStore(t)
			task := createTestTask(t, s, group(t, s).ID)
			input := taskAttemptInput(task)
			input.IdempotencyKey = "activate-ledger"
			var owners int
			if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM coordination_dispatch_state WHERE task_id=?) +
 (SELECT count(*) FROM coordination_assignments WHERE task_id=?)`, task.ID, task.ID).Scan(&owners); err != nil || owners != 0 {
				t.Fatalf("ledger fixture is not unreserved: %d, %v", owners, err)
			}
			timing, body := "AFTER", "DELETE FROM mutation_idempotency WHERE operation=NEW.operation AND actor_id=NEW.actor_id AND group_id=NEW.group_id AND idempotency_key=NEW.idempotency_key;"
			switch fault {
			case "ignore":
				timing, body = "BEFORE", "SELECT RAISE(IGNORE);"
			case "abort":
				timing, body = "BEFORE", "SELECT RAISE(ABORT,'activate ledger abort');"
			case "receipt-payload":
				body = "UPDATE mutation_idempotency SET payload='changed-payload' WHERE operation=NEW.operation AND actor_id=NEW.actor_id AND group_id=NEW.group_id AND idempotency_key=NEW.idempotency_key;"
			case "receipt-result":
				body = "UPDATE mutation_idempotency SET record_id='changed-result' WHERE operation=NEW.operation AND actor_id=NEW.actor_id AND group_id=NEW.group_id AND idempotency_key=NEW.idempotency_key;"
			}
			coordinationDecideExec(t, s, "CREATE TRIGGER activate_ledger_fault "+timing+" INSERT ON mutation_idempotency WHEN NEW.operation='activate_task_attempt' BEGIN "+body+" END")
			before := coordinationDecideSnapshot(t, s)
			got, err := s.ActivateTaskAttempt(ctx, input)
			assignmentTransactionEvidence(t, "generic Activate "+fault, err)
			if err == nil || got != (domain.TaskAttempt{}) {
				t.Errorf("keyed Activate falsely succeeded: %+v, %v", got, err)
			}
			if fault == "abort" {
				var cause *sqlite.Error
				if !errors.As(err, &cause) || cause.Code()&0xff != 19 {
					t.Errorf("real SQLite ABORT cause lost: %v", err)
				}
			}
			assignmentReopenPreserved(t, s, path, before)
		})
	}
}

func assignmentReopenPreserved(t *testing.T, s *Store, path string, before map[string][][]any) {
	t.Helper()
	after := coordinationDecideSnapshot(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := queueHandle(t, path, false)
	observed := coordinationDecideSnapshot(t, reopened)
	t.Logf("complete typed snapshots: before_after_equal=%t after_reopen_equal=%t attempts=%d/%d submissions=%d/%d ledger=%d/%d claims=%d/%d",
		reflect.DeepEqual(before, after), reflect.DeepEqual(after, observed), len(before["task_attempts"]), len(after["task_attempts"]),
		len(before["task_submissions"]), len(after["task_submissions"]), len(before["mutation_idempotency"]), len(after["mutation_idempotency"]),
		len(before["resource_claims"]), len(after["resource_claims"]))
	if !reflect.DeepEqual(after, observed) {
		t.Error("trusted reopen changed the complete persisted snapshot")
	}
	coordinationDecidePreserved(t, reopened, before)
	dispatchAssertFK(t, reopened)
}

func assignmentTransactionEvidence(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil {
		t.Logf("%s: success", label)
		return
	}
	var failure *assignmentTransactionError
	if errors.As(err, &failure) {
		t.Logf("%s: phase=%s rollback_confirmed=%t commit_outcome_unknown=%t", label, failure.Phase(), failure.RollbackConfirmed(), failure.CommitOutcomeUnknown())
	} else {
		t.Logf("%s: Assignment transaction phase/proof unavailable", label)
	}
	var cause *sqlite.Error
	if errors.As(err, &cause) {
		t.Logf("%s: SQLite code=%d", label, cause.Code())
	}
	var walk func(error, int)
	walk = func(e error, depth int) {
		t.Logf("%s: chain[%d] %T: %v", label, depth, e, e)
		if joined, ok := e.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				walk(child, depth+1)
			}
		} else if child := errors.Unwrap(e); child != nil {
			walk(child, depth+1)
		}
	}
	walk(err, 0)
}

// Observe real SQLite transaction boundaries. The only injected driver fault is
// loss of the Rollback acknowledgment after SQLite itself returned success.
// This does not simulate a native SQLite rollback I/O failure.
type assignmentBoundaryEvent struct {
	label                           string
	beginErr, rollbackErr, queryErr error
	rollbackCalls                   int
	firstSQL                        string
}
type assignmentBoundaryDriver struct {
	base        driver.Driver
	rollbackAck error
	events      []*assignmentBoundaryEvent
}
type assignmentBoundaryConn struct {
	driver.Conn
	owner *assignmentBoundaryDriver
	event *assignmentBoundaryEvent
}
type assignmentBoundaryTx struct {
	driver.Tx
	event *assignmentBoundaryEvent
	ack   error
}
type assignmentBoundaryRows struct {
	driver.Rows
	event *assignmentBoundaryEvent
}
type assignmentBoundaryLabel struct{}

func (d *assignmentBoundaryDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &assignmentBoundaryConn{Conn: conn, owner: d}, nil
}
func (c *assignmentBoundaryConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	label, _ := ctx.Value(assignmentBoundaryLabel{}).(string)
	event := &assignmentBoundaryEvent{label: label}
	c.owner.events = append(c.owner.events, event)
	c.event = event
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	event.beginErr = err
	if err != nil {
		return nil, err
	}
	return &assignmentBoundaryTx{Tx: tx, event: event, ack: c.owner.rollbackAck}, nil
}
func (c *assignmentBoundaryConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if c.event != nil && c.event.firstSQL == "" {
		c.event.firstSQL = q
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func (c *assignmentBoundaryConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if c.event != nil && c.event.firstSQL == "" {
		c.event.firstSQL = q
	}
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		if c.event != nil {
			c.event.queryErr = err
		}
		return nil, err
	}
	return &assignmentBoundaryRows{Rows: rows, event: c.event}, nil
}
func (r *assignmentBoundaryRows) Next(values []driver.Value) error {
	err := r.Rows.Next(values)
	if err != nil && err != io.EOF && r.event != nil {
		r.event.queryErr = err
	}
	return err
}
func (tx *assignmentBoundaryTx) Rollback() error {
	tx.event.rollbackCalls++
	tx.event.rollbackErr = tx.Tx.Rollback()
	if tx.event.rollbackErr != nil {
		return tx.event.rollbackErr
	}
	return tx.ack
}
func assignmentBoundaryStore(t *testing.T, path string, ack error) (*Store, *assignmentBoundaryDriver) {
	t.Helper()
	d := &assignmentBoundaryDriver{base: &sqlite.Driver{}, rollbackAck: ack}
	name := fmt.Sprintf("assignment-boundary-%p", d)
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

func TestBindAssignmentScopeRollbackCertainty(t *testing.T) {
	assignmentScopeRollback(t, false)
}

func assignmentScopeRollback(t *testing.T, submit bool) {
	t.Helper()
	for _, mode := range []string{"invalid-scope", "sql-abort", "rollback-ack-error"} {
		t.Run(mode, func(t *testing.T) {
			f, _, worker := assignmentAttemptWorker(t)
			if submit {
				binding, err := f.s.BindAssignmentAttempt(ctx, worker)
				if err != nil {
					t.Fatal(err)
				}
				worker.AttemptID = binding.AttemptID
			}
			if mode == "invalid-scope" {
				worker.Member.Claim.Token = "expired"
			} else {
				coordinationDecideExec(t, f.s, fmt.Sprintf(`CREATE TRIGGER scope_abort BEFORE UPDATE OF claim_token ON turns
 WHEN NEW.id='%s' BEGIN SELECT RAISE(ABORT,'PRIVATE scope failure'); END`, worker.Member.Claim.TurnID))
			}
			var ack error
			if mode == "rollback-ack-error" {
				ack = errors.New("rollback acknowledgment unavailable")
			}
			wrapped, observed := assignmentBoundaryStore(t, f.path, ack)
			before := coordinationDecideSnapshot(t, f.s)
			var err error
			if submit {
				var result domain.Submission
				result, err = wrapped.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: "拒绝前置失效授权"}, "scope")
				requestDispatchZero(t, result, err, ErrMemberToolDenied)
			} else {
				var result domain.AssignmentAttemptBinding
				result, err = wrapped.BindAssignmentAttempt(ctx, worker)
				requestDispatchZero(t, result, err, ErrMemberToolDenied)
			}
			assignmentTransactionEvidence(t, mode, err)
			var failure *assignmentTransactionError
			if !errors.As(err, &failure) || failure.Phase() != "scope" || failure.CommitOutcomeUnknown() || failure.RollbackConfirmed() != (ack == nil) {
				t.Errorf("early scope failure claimed an unobserved rollback: %v", err)
			}
			if ack != nil && !errors.Is(err, ack) {
				t.Error("rollback acknowledgment error was discarded")
			}
			if len(observed.events) != 1 {
				t.Fatalf("unexpected transaction count: %d", len(observed.events))
			}
			event := observed.events[0]
			if event.rollbackCalls != 1 || event.rollbackErr != nil || !strings.HasPrefix(event.firstSQL, "UPDATE turns SET claim_token=claim_token") {
				t.Fatalf("real scope/rollback boundary not exercised: %+v", event)
			}
			if mode != "invalid-scope" {
				var raw *sqlite.Error
				if !errors.As(event.queryErr, &raw) || raw.Code()&0xff != 19 {
					t.Fatalf("real first-SQL ABORT not observed: %v", event.queryErr)
				}
				t.Logf("real first-scope SQLite code=%d remains masked as ErrMemberToolDenied; real Rollback returned nil; injected_ack_error=%t", raw.Code(), ack != nil)
				if errors.As(err, &raw) {
					t.Error("scope SQL cause escaped the existing denial mask")
				}
			}
			if err := wrapped.Close(); err != nil {
				t.Fatal(err)
			}
			assignmentReopenPreserved(t, f.s, f.path, before)
		})
	}
}

type assignmentAttemptAPI interface {
	BindAssignmentAttempt(context.Context, CoordinationToolScope) (domain.AssignmentAttemptBinding, error)
}

func TestBindAssignmentRealWorkerAndReplay(t *testing.T) {
	f, r, decision, _ := assignmentPending(t)
	worker := coordinationDecideWorker(t, f, r, decision, false)
	api, ok := any(f.s).(assignmentAttemptAPI)
	if !ok {
		t.Fatal("Store lacks the postclaim Assignment binding primitive")
	}
	binding, err := api.BindAssignmentAttempt(ctx, worker)
	if err != nil || binding.AssignmentID != decision.Assignment.ID || binding.TaskID != r.TaskID || binding.GroupID != r.GroupID || binding.TaskRevision != r.TaskRevision || binding.WorkerMemberID != f.worker || binding.TurnID != worker.Member.Claim.TurnID || binding.AttemptID == "" {
		t.Fatalf("real worker binding: %+v, %v", binding, err)
	}
	worker.AttemptID = binding.AttemptID
	for _, attempt := range []string{"", binding.AttemptID} {
		worker.AttemptID = attempt
		replayed, err := api.BindAssignmentAttempt(ctx, worker)
		if err != nil || replayed != binding {
			t.Fatalf("same-Turn binding changed: %+v, %v", replayed, err)
		}
	}
	var count int
	if err := f.s.db.QueryRow("SELECT count(*) FROM mutation_idempotency WHERE operation='activate_task_attempt'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("internal binding used a public key namespace: %d, %v", count, err)
	}
	dispatchAssertFK(t, f.s)
}

func assignmentAttemptWorker(t *testing.T) (requestDispatchFixture, domain.DispatchRequest, CoordinationToolScope) {
	t.Helper()
	f, r, decision, _ := assignmentPending(t)
	return f, r, coordinationDecideWorker(t, f, r, decision, false)
}

func TestActivateAssignmentNoSlotLegacyDependencies(t *testing.T) {
	f := requestDispatchSetup(t)
	parent := createTestTask(t, f.s, f.groupID)
	input := taskInput(f.groupID)
	input.DependencyIDs = []string{parent.ID}
	child, err := f.s.CreateTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var owners int
	if err := f.s.db.QueryRow(`SELECT (SELECT count(*) FROM coordination_dispatch_state WHERE task_id=?) +
 (SELECT count(*) FROM coordination_assignments WHERE task_id=?)`, child.ID, child.ID).Scan(&owners); err != nil || owners != 0 {
		t.Fatalf("legacy dependency test has an N3 owner: %d, %v", owners, err)
	}
	attempt, err := f.s.ActivateTaskAttempt(ctx, taskAttemptInput(parent))
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"open", "submitted"} {
		before := coordinationDecideSnapshot(t, f.s)
		got, err := f.s.ActivateTaskAttempt(ctx, taskAttemptInput(child))
		requestDispatchZero(t, got, err, ErrConflict)
		coordinationDecidePreserved(t, f.s, before)
		if status == "open" {
			if _, err := f.s.SubmitTask(ctx, taskSubmissionInput(attempt)); err != nil {
				t.Fatal(err)
			}
		}
	}
	parentNow, err := f.s.GetTask(ctx, f.groupID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := f.s.GetTaskSubmission(ctx, f.groupID, parent.ID, parentNow.CurrentSubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ReviewTask(ctx, taskReviewInput(submission)); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.ActivateTaskAttempt(ctx, taskAttemptInput(child)); err != nil || got.TaskID != child.ID {
		t.Fatalf("unreserved accepted-only legacy activation: %+v, %v", got, err)
	}
}

func TestActivateAssignmentHistoricalKeyPointerAndRejectedRevision(t *testing.T) {
	f, r := materializePending(t)
	input := ActivateTaskAttemptInput{GroupID: r.GroupID, TaskID: r.TaskID, ExpectedRevision: r.TaskRevision, ActorID: "user", IdempotencyKey: "historical-activate"}
	original, err := dispatchHistoricalAttempt(t, f.s, input)
	if err != nil {
		t.Fatal(err)
	}
	before := coordinationDecideSnapshot(t, f.s)
	if got, err := f.s.ActivateTaskAttempt(ctx, input); err != nil || got != original {
		t.Fatalf("ownership rewrote an original key receipt: %+v, %v", got, err)
	}
	coordinationDecidePreserved(t, f.s, before)
	input.IdempotencyKey, input.ExpectedActiveAttemptID = "new-key", original.ID
	got, err := f.s.ActivateTaskAttempt(ctx, input)
	requestDispatchZero(t, got, err, ErrConflict)
	coordinationDecidePreserved(t, f.s, before)
	submission, err := f.s.SubmitTask(ctx, taskSubmissionInput(original))
	if err != nil {
		t.Fatal(err)
	}
	review := taskReviewInput(submission)
	review.Outcome = domain.ReviewOutcomeRejected
	if _, err := f.s.ReviewTask(ctx, review); err != nil {
		t.Fatal(err)
	}
	input.ExpectedRevision, input.ExpectedActiveAttemptID = r.TaskRevision+1, ""
	before = coordinationDecideSnapshot(t, f.s)
	got, err = f.s.ActivateTaskAttempt(ctx, input)
	requestDispatchZero(t, got, err, ErrConflict)
	coordinationDecidePreserved(t, f.s, before)
}

func TestActivateAssignmentUserAndMemberCannotSupersedeBinding(t *testing.T) {
	f, r, worker := assignmentBoundWorker(t)
	for _, actor := range []string{"user", worker.Member.MemberID} {
		for _, active := range []string{"", worker.AttemptID} {
			input := ActivateTaskAttemptInput{GroupID: r.GroupID, TaskID: r.TaskID, ExpectedRevision: r.TaskRevision, ExpectedActiveAttemptID: active, ActorID: actor, IdempotencyKey: "public-key"}
			if actor != "user" {
				input.MemberScope = &worker.Member
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.ActivateTaskAttempt(ctx, input)
			requestDispatchZero(t, got, err, ErrConflict)
			coordinationDecidePreserved(t, f.s, before)
		}
	}
}

func TestActivateAssignmentMissingLegacyTableFailsClosed(t *testing.T) {
	s, _, g, _, _ := dispatchV11Fixture(t)
	task := createTestTask(t, s, g)
	before := coordinationDecideSnapshot(t, s)
	got, err := s.ActivateTaskAttempt(ctx, taskAttemptInput(task))
	requestDispatchZero(t, got, err, nil)
	coordinationDecidePreserved(t, s, before)
}

func TestBindAssignmentRegistrySnapshotAndWrongTurn(t *testing.T) {
	f, _, worker := assignmentAttemptWorker(t)
	fields := reflect.TypeOf(worker.Provenance)
	for i := 0; i < fields.NumField(); i++ {
		t.Run(fields.Field(i).Name, func(t *testing.T) {
			bad := worker
			field := reflect.ValueOf(&bad.Provenance).Elem().Field(i)
			if field.Kind() == reflect.String {
				field.SetString(field.String() + "-wrong")
			} else {
				field.SetInt(field.Int() + 1)
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.BindAssignmentAttempt(ctx, bad)
			requestDispatchZero(t, got, err, ErrMemberToolDenied)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
	otherMember := member(t, f.s, f.groupID).ID
	delivery := queueDelivery(t, f.s, f.groupID, otherMember, "other-turn")
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: delivery.ID, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	otherScope, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: f.groupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	bad := worker
	bad.Member = otherScope
	got, err := f.s.BindAssignmentAttempt(ctx, bad)
	requestDispatchZero(t, got, err, ErrMemberToolDenied)
	bad = worker
	bad.Member.Claim.GroupID = group(t, f.s).ID
	got, err = f.s.BindAssignmentAttempt(ctx, bad)
	requestDispatchZero(t, got, err, ErrMemberToolDenied)
	bad = worker
	bad.AttemptID = "model-selected-attempt"
	got, err = f.s.BindAssignmentAttempt(ctx, bad)
	requestDispatchZero(t, got, err, ErrMemberToolDenied)
}

func TestBindAssignmentInitialLiveScopeEligibility(t *testing.T) {
	assignmentAttemptEligibility(t, false, []string{"old-token", "cancel", "unknown", "terminal", "candidate"})
}

func TestBindAssignmentReplayLiveScopeEligibility(t *testing.T) {
	assignmentAttemptEligibility(t, true, []string{"old-token", "cancel", "unknown", "terminal", "candidate"})
}

func TestBindAssignmentInitialAuthorityEligibility(t *testing.T) {
	assignmentAttemptEligibility(t, false, []string{"authority", "binding", "policy"})
}

func TestBindAssignmentReplayAuthorityEligibility(t *testing.T) {
	assignmentAttemptEligibility(t, true, []string{"authority", "binding", "policy"})
}

func TestBindAssignmentInitialTaskEligibility(t *testing.T) {
	assignmentAttemptEligibility(t, false, []string{"pointer", "revision", "assignee"})
}

func TestBindAssignmentReplayTaskEligibility(t *testing.T) {
	assignmentAttemptEligibility(t, true, []string{"pointer", "revision", "assignee"})
}

func TestBindAssignmentInitialMembershipEligibility(t *testing.T) {
	assignmentAttemptEligibility(t, false, []string{"worker-left", "coordinator-left"})
}

func TestBindAssignmentReplayMembershipEligibility(t *testing.T) {
	assignmentAttemptEligibility(t, true, []string{"worker-left", "coordinator-left"})
}

func assignmentAttemptEligibility(t *testing.T, replay bool, changes []string) {
	t.Helper()
	for _, change := range changes {
		t.Run(change, func(t *testing.T) {
			f, r, worker := assignmentAttemptWorker(t)
			if replay {
				bound, err := f.s.BindAssignmentAttempt(ctx, worker)
				if err != nil {
					t.Fatal(err)
				}
				worker.AttemptID = bound.AttemptID
			}
			switch change {
			case "old-token":
				worker.Member.Claim.Token = "old-claim"
			case "cancel":
				if _, err := f.s.RequestTurnCancel(ctx, f.groupID, worker.Member.Claim.TurnID); err != nil {
					t.Fatal(err)
				}
			case "unknown", "terminal":
				state := "unknown"
				if change == "terminal" {
					state = "failed"
				}
				coordinationDecideExec(t, f.s, "UPDATE turns SET state=? WHERE id=?", state, worker.Member.Claim.TurnID)
			case "candidate":
				coordinationDecideExec(t, f.s, "UPDATE turns SET candidate_terminal='succeeded' WHERE id=?", worker.Member.Claim.TurnID)
			case "authority":
				coordinationDecideExec(t, f.s, "UPDATE task_coordination SET authority_generation=authority_generation+1 WHERE task_id=?", r.TaskID)
			case "binding":
				if _, err := f.s.SetCoordinator(ctx, f.groupID, f.coord, 1, "new-binding"); err != nil {
					t.Fatal(err)
				}
			case "policy":
				if _, err := f.s.SetDispatchPolicy(ctx, f.groupID, nil, 1, "new-policy"); err != nil {
					t.Fatal(err)
				}
			case "pointer":
				if replay {
					coordinationDecideExec(t, f.s, "UPDATE tasks SET active_attempt_id=NULL WHERE id=?", r.TaskID)
				} else if _, err := dispatchHistoricalAttempt(t, f.s, taskAttemptInput(f.task)); err != nil {
					t.Fatal(err)
				}
			case "revision":
				if replay {
					submission, err := f.s.SubmitAssignmentResult(ctx, worker, AssignmentResultInput{Summary: "等待本地评审"}, "revision-result")
					if err != nil {
						t.Fatal(err)
					}
					review := taskReviewInput(submission)
					review.Outcome = domain.ReviewOutcomeRejected
					if _, err := f.s.ReviewTask(ctx, review); err != nil {
						t.Fatal(err)
					}
					current, err := f.s.GetTask(ctx, r.GroupID, r.TaskID)
					if err != nil || current.Revision != r.TaskRevision+1 || current.ActiveAttemptID != "" || current.CurrentSubmissionID != "" {
						t.Fatalf("real rejection did not advance the generation: %+v, %v", current, err)
					}
					view, err := f.s.GetAssignment(ctx, r.GroupID, worker.Provenance.AssignmentID)
					if err != nil || view.Binding == nil || view.Binding.TaskRevision != r.TaskRevision || view.Binding.AttemptID != worker.AttemptID {
						t.Fatalf("rejection rewrote the historical binding: %+v, %v", view.Binding, err)
					}
				} else {
					coordinationDecideExec(t, f.s, "UPDATE tasks SET revision=revision+1 WHERE id=?", r.TaskID)
				}
			case "worker-left":
				coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.worker)
			case "coordinator-left":
				coordinationDecideExec(t, f.s, "UPDATE members SET status='left' WHERE id=?", f.coord)
			case "assignee":
				coordinationDecideExec(t, f.s, "UPDATE tasks SET assignee_id=? WHERE id=?", f.coord, r.TaskID)
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.BindAssignmentAttempt(ctx, worker)
			requestDispatchZero(t, got, err, nil)
			if strings.Contains(err.Error(), worker.Member.Claim.Token) {
				t.Fatal("error disclosed claim token")
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestBindAssignmentAcceptedDependencies(t *testing.T) {
	f, r, worker := assignmentAttemptWorker(t)
	parent := createTestTask(t, f.s, f.groupID)
	coordinationDecideExec(t, f.s, "INSERT INTO task_dependencies(group_id,task_id,dependency_id) VALUES(?,?,?)", f.groupID, r.TaskID, parent.ID)
	attempt, err := f.s.ActivateTaskAttempt(ctx, taskAttemptInput(parent))
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.s.BindAssignmentAttempt(ctx, worker)
	requestDispatchZero(t, got, err, ErrConflict)
	submission, err := f.s.SubmitTask(ctx, taskSubmissionInput(attempt))
	if err != nil {
		t.Fatal(err)
	}
	got, err = f.s.BindAssignmentAttempt(ctx, worker)
	requestDispatchZero(t, got, err, ErrConflict)
	if _, err := f.s.ReviewTask(ctx, taskReviewInput(submission)); err != nil {
		t.Fatal(err)
	}
	binding, err := f.s.BindAssignmentAttempt(ctx, worker)
	if err != nil || binding.TaskID != r.TaskID {
		t.Fatalf("real Bind did not require accepted-only dependencies: %+v, %v", binding, err)
	}
}

func TestBindAssignmentDualRoleAndBrokenBackpointer(t *testing.T) {
	for _, fault := range []string{"dual-role", "backpointer", "missing-table"} {
		t.Run(fault, func(t *testing.T) {
			f, r, decision, coordinator := assignmentPending(t)
			worker := coordinationDecideWorker(t, f, r, decision, false)
			switch fault {
			case "dual-role":
				// Counterfactual receipt corruption, never a positive capability fixture.
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_dispatch_update")
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_receipt_source")
				p := coordinator.Provenance
				coordinationDecideExec(t, f.s, `UPDATE coordination_assignment_dispatch SET delivery_id=?,message_id=?,conversation_id=?,delivered_member_id=? WHERE assignment_id=?`, p.DeliveryID, p.MessageID, p.ConversationID, p.MemberID, decision.Assignment.ID)
				worker = coordinator
				worker.Provenance.Kind, worker.Provenance.AssignmentID = CoordinationRoleWorker, decision.Assignment.ID
			case "backpointer":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_dispatch_deliveries_identity")
				coordinationDecideExec(t, f.s, "UPDATE deliveries SET turn_id='' WHERE id=?", worker.Provenance.DeliveryID)
			case "missing-table":
				coordinationDecideExec(t, f.s, "DROP TABLE coordination_assignment_attempts")
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.BindAssignmentAttempt(ctx, worker)
			requestDispatchZero(t, got, err, nil)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func assignmentDefaultTimeout(t *testing.T, stores ...*Store) {
	t.Helper()
	for _, s := range stores {
		var timeout int
		if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != 5000 {
			t.Fatalf("expected unchanged default 5s lock wait: %d, %v", timeout, err)
		}
	}
}

func assignmentRequireBeginBusy(t *testing.T, err error) {
	t.Helper()
	var cause *sqlite.Error
	var failure *assignmentTransactionError
	if !errors.As(err, &cause) || cause.Code()&0xff != 5 || !errors.As(err, &failure) ||
		failure.Phase() != "begin" || !failure.RollbackConfirmed() || failure.CommitOutcomeUnknown() {
		t.Fatalf("not a proven BEGIN BUSY/no-transaction outcome: %v", err)
	}
}

func TestBindAssignmentTwoHandlesAndPublicActivate(t *testing.T) {
	f, r, worker := assignmentAttemptWorker(t)
	second, observed := assignmentBoundaryStore(t, f.path, nil)
	assignmentDefaultTimeout(t, f.s, second)
	before := coordinationDecideSnapshot(t, f.s)
	type result struct {
		label   string
		binding domain.AssignmentAttemptBinding
		err     error
	}
	start, results := make(chan struct{}), make(chan result, 2)
	for i, s := range []*Store{f.s, second} {
		label := []string{"bind-primary", "bind-secondary"}[i]
		go func(label string, s *Store) {
			<-start
			binding, err := s.BindAssignmentAttempt(context.WithValue(ctx, assignmentBoundaryLabel{}, label), worker)
			results <- result{label, binding, err}
		}(label, s)
	}
	type activation struct {
		attempt domain.TaskAttempt
		err     error
	}
	activate := make(chan activation, 1)
	go func() {
		<-start
		attempt, err := second.ActivateTaskAttempt(context.WithValue(ctx, assignmentBoundaryLabel{}, "public-activate"), ActivateTaskAttemptInput{GroupID: r.GroupID, TaskID: r.TaskID, ExpectedRevision: r.TaskRevision, ActorID: "user"})
		activate <- activation{attempt, err}
	}()
	close(start)
	x, y, public := <-results, <-results, <-activate
	var winner domain.AssignmentAttemptBinding
	for _, got := range []result{x, y} {
		assignmentTransactionEvidence(t, got.label, got.err)
		if got.err != nil {
			requestDispatchZero(t, got.binding, got.err, nil)
			assignmentRequireBeginBusy(t, got.err)
		} else {
			if got.binding.AttemptID == "" || winner.AttemptID != "" && got.binding != winner {
				t.Fatal("successful concurrent bindings disagree")
			}
			winner = got.binding
		}
	}
	if winner.AttemptID == "" {
		t.Fatal("no actual Bind winner")
	}
	var publicEvent *assignmentBoundaryEvent
	for _, event := range observed.events {
		t.Logf("observed %s: begin_error=%v first_sql_present=%t rollback_calls=%d rollback_error=%v", event.label, event.beginErr, event.firstSQL != "", event.rollbackCalls, event.rollbackErr)
		if event.label == "public-activate" {
			publicEvent = event
		}
	}
	assignmentTransactionEvidence(t, "public-activate", public.err)
	requestDispatchZero(t, public.attempt, public.err, nil)
	if publicEvent == nil {
		t.Fatal("public Activate transaction boundary was not observed")
	}
	if errors.Is(public.err, ErrConflict) {
		if publicEvent.beginErr != nil || publicEvent.rollbackCalls != 1 || publicEvent.rollbackErr != nil {
			t.Fatal("public ownership rejection lacks its actual successful rollback")
		}
	} else {
		var cause *sqlite.Error
		if !errors.As(public.err, &cause) || cause.Code()&0xff != 5 || !errors.Is(public.err, publicEvent.beginErr) ||
			publicEvent.firstSQL != "" || publicEvent.rollbackCalls != 0 {
			t.Fatal("public Activate failed without observed BEGIN BUSY/no-transaction proof")
		}
	}
	after := coordinationDecideSnapshot(t, f.s)
	if len(after["coordination_assignment_attempts"]) != 1 || len(after["task_attempts"]) != len(before["task_attempts"])+1 || !reflect.DeepEqual(before["resource_claims"], after["resource_claims"]) {
		t.Fatal("binding race added duplicate work or released claims")
	}
	view, err := second.GetAssignment(ctx, r.GroupID, worker.Provenance.AssignmentID)
	if err != nil || view.Binding == nil || *view.Binding != winner {
		t.Fatalf("trusted winner receipt reconciliation failed: %+v, %v", view, err)
	}
	// Explicit caller reconciliation after the winner's trusted receipt read;
	// this is one revalidation call, not an automatic retry or native execution.
	worker.AttemptID = winner.AttemptID
	reconciled, err := second.BindAssignmentAttempt(ctx, worker)
	if err != nil || reconciled != winner {
		t.Fatalf("same-Turn receipt revalidation changed the winner: %+v, %v", reconciled, err)
	}
	coordinationDecidePreserved(t, f.s, after)
	t.Log("default 5000ms: only success or observed BEGIN BUSY accepted; one binding; explicit same-Turn receipt reconciliation; claims unchanged")
}

func TestBindAssignmentDefaultBusyReconcilesReceipt(t *testing.T) {
	f, r, worker := assignmentAttemptWorker(t)
	second, observed := assignmentBoundaryStore(t, f.path, nil)
	assignmentDefaultTimeout(t, f.s, second)
	before := coordinationDecideSnapshot(t, f.s)
	lock, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	got, err := second.BindAssignmentAttempt(ctx, worker)
	assignmentTransactionEvidence(t, "default-5s-held-real-writer", err)
	if e := lock.Rollback(); e != nil {
		t.Fatal(e)
	}
	requestDispatchZero(t, got, err, nil)
	assignmentRequireBeginBusy(t, err)
	if len(observed.events) != 1 || observed.events[0].beginErr == nil || observed.events[0].firstSQL != "" || observed.events[0].rollbackCalls != 0 {
		t.Fatal("default-5s contention did not fail before a transaction existed")
	}
	coordinationDecidePreserved(t, f.s, before)
	winner, err := f.s.BindAssignmentAttempt(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	view, err := second.GetAssignment(ctx, r.GroupID, worker.Provenance.AssignmentID)
	if err != nil || view.Binding == nil || *view.Binding != winner {
		t.Fatalf("trusted winner receipt: %+v, %v", view, err)
	}
	after := coordinationDecideSnapshot(t, f.s)
	worker.AttemptID = winner.AttemptID
	reconciled, err := second.BindAssignmentAttempt(ctx, worker)
	if err != nil || reconciled != winner {
		t.Fatalf("explicit receipt reconciliation after default BUSY: %+v, %v", reconciled, err)
	}
	coordinationDecidePreserved(t, f.s, after)
	if len(after["task_attempts"]) != len(before["task_attempts"])+1 || len(after["coordination_assignment_attempts"]) != 1 || !reflect.DeepEqual(before["resource_claims"], after["resource_claims"]) {
		t.Fatal("default BUSY/reconciliation duplicated work or released claims")
	}
}

func TestActivateAndRequestDispatchTwoHandles(t *testing.T) {
	f := requestDispatchSetup(t)
	second := queueHandle(t, f.path, false)
	start, results := make(chan struct{}), make(chan error, 2)
	go func() {
		<-start
		_, err := f.s.ActivateTaskAttempt(ctx, taskAttemptInput(f.task))
		results <- err
	}()
	go func() {
		<-start
		_, err := second.RequestDispatch(ctx, f.groupID, f.task.ID, 1, 1, "竞争", "racing-request")
		results <- err
	}()
	close(start)
	x, y := <-results, <-results
	if (x == nil) == (y == nil) || x != nil && !errors.Is(x, ErrConflict) || y != nil && !errors.Is(y, ErrConflict) {
		t.Fatalf("Activate/RequestDispatch must have one winner: %v, %v", x, y)
	}
	dispatchAssertFK(t, f.s)
}

// A terminal first Turn permits a new ordinary Turn for the same member, never
// another Turn for the consumed Assignment delivery or adoption of its binding.
func assignmentSecondWorkerTurn(t *testing.T, s *Store, worker CoordinationToolScope) CoordinationToolScope {
	t.Helper()
	queueEvent(t, s, worker.Member.Claim, provider.AcceptedEvent, "", "")
	if _, err := s.ApplyTurnReceipt(ctx, worker.Member.Claim, queueReceipt(provider.OutcomeFailed)); err != nil {
		t.Fatal(err)
	}
	p := worker.Provenance
	delivery := queueDelivery(t, s, p.GroupID, p.MemberID, p.TopicID)
	turn, err := s.ClaimTurn(ctx, ClaimTurnInput{GroupID: p.GroupID, DeliveryID: delivery.ID, MaxActive: 8})
	if err != nil {
		t.Fatal(err)
	}
	memberScope, err := s.BindMemberTool(ctx, TurnClaim{GroupID: p.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	if memberScope.MemberID != worker.Member.MemberID || turn.ID == worker.Member.Claim.TurnID {
		t.Fatal("fixture did not create a second live Turn for the same worker")
	}
	worker.Member = memberScope
	return worker
}

func TestBindAssignmentSecondTurnCannotAdoptBinding(t *testing.T) {
	f, _, worker := assignmentBoundWorker(t)
	second := assignmentSecondWorkerTurn(t, f.s, worker)
	for _, attempt := range []string{"", worker.AttemptID} {
		second.AttemptID = attempt
		before := coordinationDecideSnapshot(t, f.s)
		got, err := f.s.BindAssignmentAttempt(ctx, second)
		requestDispatchZero(t, got, err, ErrMemberToolDenied)
		coordinationDecidePreserved(t, f.s, before)
	}
	dispatchAssertFK(t, f.s)
}

func TestBindAssignmentReplayAcceptedDependencies(t *testing.T) {
	f, r, worker := assignmentBoundWorker(t)
	original, err := f.s.BindAssignmentAttempt(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	parent := createTestTask(t, f.s, r.GroupID)
	coordinationDecideExec(t, f.s, "INSERT INTO task_dependencies(group_id,task_id,dependency_id) VALUES(?,?,?)", r.GroupID, r.TaskID, parent.ID)
	before := coordinationDecideSnapshot(t, f.s)
	got, err := f.s.BindAssignmentAttempt(ctx, worker)
	requestDispatchZero(t, got, err, ErrConflict)
	coordinationDecidePreserved(t, f.s, before)
	attempt, err := f.s.ActivateTaskAttempt(ctx, taskAttemptInput(parent))
	if err != nil {
		t.Fatal(err)
	}
	submission, err := f.s.SubmitTask(ctx, taskSubmissionInput(attempt))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ReviewTask(ctx, taskReviewInput(submission)); err != nil {
		t.Fatal(err)
	}
	got, err = f.s.BindAssignmentAttempt(ctx, worker)
	if err != nil || got != original {
		t.Fatalf("accepted dependencies did not restore the same binding: %+v, %v", got, err)
	}
	dispatchAssertFK(t, f.s)
}

func TestBindAssignmentLiveScopeRecheck(t *testing.T) {
	f, _, worker := assignmentAttemptWorker(t)
	coordinationDecideExec(t, f.s, fmt.Sprintf(`CREATE TRIGGER live_bind_drift AFTER UPDATE OF claim_token ON turns
 WHEN NEW.id='%s' BEGIN UPDATE turns SET claim_token='rotated' WHERE id=NEW.id; END`, worker.Member.Claim.TurnID))
	before := coordinationDecideSnapshot(t, f.s)
	recorder := assignmentQueryStore(t, f.path)
	got, err := recorder.s.BindAssignmentAttempt(ctx, worker)
	requestDispatchZero(t, got, err, ErrMemberToolDenied)
	coordinationDecidePreserved(t, f.s, before)
	if len(recorder.queries) == 0 || !strings.HasPrefix(recorder.queries[0].query, "UPDATE turns SET claim_token=claim_token") {
		t.Fatal("live claim fence was not the first SQL")
	}
	for _, q := range recorder.queries {
		if strings.Contains(q.query, "INSERT INTO task_attempts") {
			t.Fatal("scope revoked by the first SQL reached activation")
		}
	}
}

func TestBindAssignmentCompetingRequestDispatch(t *testing.T) {
	f, r, worker := assignmentAttemptWorker(t)
	second := queueHandle(t, f.path, false)
	assignmentDefaultTimeout(t, f.s, second)
	start, result := make(chan struct{}), make(chan error, 1)
	go func() {
		<-start
		_, err := second.RequestDispatch(ctx, r.GroupID, r.TaskID, r.TaskRevision, r.AuthorityGeneration, "不重复分配", "second-request")
		result <- err
	}()
	close(start)
	binding, err := f.s.BindAssignmentAttempt(ctx, worker)
	requestErr := <-result
	if err != nil || binding.AttemptID == "" || !errors.Is(requestErr, ErrConflict) {
		t.Fatalf("Bind/RequestDispatch lost exclusion: %+v, %v / %v", binding, err, requestErr)
	}
}

func TestBindAssignmentAttemptInsertAtomicity(t *testing.T) {
	assignmentAttemptWritePoint(t, "task_attempts", "INSERT")
}

func TestBindAssignmentPointerAtomicity(t *testing.T) {
	assignmentAttemptWritePoint(t, "tasks", "UPDATE OF active_attempt_id")
}

func TestBindAssignmentBindingAtomicity(t *testing.T) {
	assignmentAttemptWritePoint(t, "coordination_assignment_attempts", "INSERT")
}

func assignmentAttemptWritePoint(t *testing.T, table, operation string) {
	t.Helper()
	for _, fault := range []string{"ignore", "abort", "authority", "policy", "token", "cancel", "history", "dependency", "pointer", "commit"} {
		t.Run(fault, func(t *testing.T) {
			f, r, worker := assignmentAttemptWorker(t)
			timing, body := "AFTER", "SELECT RAISE(ABORT,'injected binding fault');"
			switch fault {
			case "ignore":
				timing, body = "BEFORE", "SELECT RAISE(IGNORE);"
			case "authority":
				body = fmt.Sprintf("UPDATE task_coordination SET authority_generation=authority_generation+1 WHERE task_id='%s';", r.TaskID)
			case "policy":
				body = fmt.Sprintf("UPDATE group_dispatch_policies SET revision=2 WHERE group_id='%s';", r.GroupID)
				coordinationDecideExec(t, f.s, coordinatorPolicyRevisionInsert, r.GroupID, 2, "[]")
			case "token":
				body = fmt.Sprintf("UPDATE turns SET claim_token='rotated' WHERE id='%s';", worker.Member.Claim.TurnID)
			case "cancel":
				body = fmt.Sprintf("UPDATE turns SET cancel_requested_at='%s' WHERE id='%s';", time.Now().UTC().Format(timestampLayout), worker.Member.Claim.TurnID)
			case "history":
				other := member(t, f.s, r.GroupID).ID
				delivery := queueDelivery(t, f.s, r.GroupID, other, r.TopicID)
				turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: delivery.ID, MaxActive: 8})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.RequestTurnCancel(ctx, r.GroupID, turn.ID); err != nil {
					t.Fatal(err)
				}
				body = fmt.Sprintf(`INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,turn_id,created_at)
 SELECT 'late-live-history','%s','%s',%d,'%s','%s','%s'
 WHERE NOT EXISTS(SELECT 1 FROM task_attempts WHERE id='late-live-history');`, r.TaskID, r.GroupID, r.TaskRevision, other, turn.ID, time.Now().UTC().Format(timestampLayout))
			case "dependency":
				parent := createTestTask(t, f.s, r.GroupID)
				body = fmt.Sprintf("INSERT INTO task_dependencies(group_id,task_id,dependency_id) VALUES('%s','%s','%s');", r.GroupID, r.TaskID, parent.ID)
			case "pointer":
				old, err := dispatchHistoricalAttempt(t, f.s, taskAttemptInput(f.task))
				if err != nil {
					t.Fatal(err)
				}
				coordinationDecideExec(t, f.s, "UPDATE tasks SET active_attempt_id=NULL WHERE id=?", r.TaskID)
				body = fmt.Sprintf("UPDATE tasks SET active_attempt_id='%s' WHERE id='%s';", old.ID, r.TaskID)
			case "commit":
				coordinationDecideExec(t, f.s, "CREATE TABLE binding_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED)")
				body = "INSERT INTO binding_commit_guard VALUES('missing');"
			}
			coordinationDecideExec(t, f.s, "CREATE TRIGGER binding_fault "+timing+" "+operation+" ON "+table+" BEGIN "+body+" END")
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.BindAssignmentAttempt(ctx, worker)
			requestDispatchZero(t, got, err, nil)
			var failure *assignmentTransactionError
			if !errors.As(err, &failure) || failure.CommitOutcomeUnknown() != (fault == "commit") {
				t.Fatalf("binding outcome certainty lost: %v", err)
			}
			coordinationDecidePreserved(t, f.s, before)
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := queueHandle(t, f.path, false)
			coordinationDecidePreserved(t, reopened, before)
			dispatchAssertFK(t, reopened)
		})
	}
}

func TestBindAssignmentRealBusyAndCommitUnknown(t *testing.T) {
	for _, mode := range []string{"begin-busy", "refuse", "ack-error"} {
		t.Run(mode, func(t *testing.T) {
			f, r, worker := assignmentAttemptWorker(t)
			second := queueHandle(t, f.path, false)
			coordinationDecideExec(t, second, "PRAGMA busy_timeout=0")
			lock, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, busy := second.db.BeginTx(ctx, nil)
			if busy == nil {
				t.Fatal("fixture did not observe real SQLite BUSY")
			}
			if mode == "begin-busy" {
				got, err := second.BindAssignmentAttempt(ctx, worker)
				requestDispatchZero(t, got, err, nil)
				var failure *assignmentTransactionError
				if !errors.As(err, &failure) || failure.Phase() != "begin" || failure.CommitOutcomeUnknown() {
					t.Fatalf("BEGIN BUSY classification: %v", err)
				}
			}
			if err := lock.Rollback(); err != nil {
				t.Fatal(err)
			}
			before := coordinationDecideSnapshot(t, f.s)
			if mode != "begin-busy" {
				wrapped, _ := coordinationDecideWrapped(t, f.path, mode, busy)
				got, err := wrapped.BindAssignmentAttempt(ctx, worker)
				requestDispatchZero(t, got, err, busy)
				var failure *assignmentTransactionError
				if !errors.As(err, &failure) || !failure.CommitOutcomeUnknown() || failure.RollbackConfirmed() || failure.Phase() != "commit" {
					t.Fatalf("unknown Commit became retryable/known: %v", err)
				}
				view, err := second.GetAssignment(ctx, r.GroupID, worker.Provenance.AssignmentID)
				if err != nil || (view.Binding != nil) != (mode == "ack-error") {
					t.Fatalf("trusted reconciliation: %+v, %v", view.Binding, err)
				}
			}
			after := coordinationDecideSnapshot(t, f.s)
			if !reflect.DeepEqual(before["resource_claims"], after["resource_claims"]) || !reflect.DeepEqual(before["coordination_assignment_dispatch"], after["coordination_assignment_dispatch"]) {
				t.Fatal("BUSY/unknown automatically released claims or deferred intent")
			}
		})
	}
}

func TestBindAssignmentHistoricalNonterminalUnbounded(t *testing.T) {
	for _, state := range []string{"unknown", "cancel_requested"} {
		t.Run(state, func(t *testing.T) {
			f, r, decision, old := assignmentLateHistoryFixture(t, state)
			// Materialize/claim before late history is discovered, then recheck in Bind.
			worker := coordinationDecideWorker(t, f, r, decision, false)
			tx, err := f.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for _, q := range []coordinatorSQL{
				{"UPDATE tasks SET revision=1 WHERE id=?", []any{r.TaskID}},
				{`INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,turn_id,created_at)
 VALUES('early-worker-history',?,?,1,?,?,'2000-01-01T00:00:00.000000000Z')`, []any{r.TaskID, r.GroupID, old.MemberID, old.ID}},
				{"UPDATE tasks SET revision=2 WHERE id=?", []any{r.TaskID}},
			} {
				if _, err := tx.Exec(q.query, q.args...); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			terminalMember := member(t, f.s, r.GroupID).ID
			delivery := queueDelivery(t, f.s, r.GroupID, terminalMember, "terminal-history")
			terminal, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: r.GroupID, DeliveryID: delivery.ID, MaxActive: 8})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.ApplyTurnReceipt(ctx, TurnClaim{GroupID: r.GroupID, TurnID: terminal.ID, Token: terminal.ClaimToken}, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
				t.Fatal(err)
			}
			coordinationDecideExec(t, f.s, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<4096)
 INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,turn_id,created_at)
 SELECT printf('new-terminal-%06d',i),?,?,2,?,?,'2030-01-01T00:00:00.000000000Z' FROM n`, r.TaskID, r.GroupID, terminalMember, terminal.ID)
			recorder := assignmentQueryStore(t, f.path)
			before := coordinationDecideSnapshot(t, f.s)
			started := time.Now()
			got, err := recorder.s.BindAssignmentAttempt(ctx, worker)
			requestDispatchZero(t, got, err, ErrConflict)
			coordinationDecidePreserved(t, f.s, before)
			if len(recorder.queries) == 0 || !strings.HasPrefix(recorder.queries[0].query, "UPDATE turns SET claim_token=claim_token") {
				t.Fatal("live scope was not the first SQL")
			}
			found := false
			for _, q := range recorder.queries {
				if strings.Contains(q.query, "ha.id<>:own") {
					plan := resolutionPlan(t, f.s, q.query, q.args...)
					if !strings.Contains(plan, "task_attempts_group_task_created_id") || strings.Contains(strings.ToUpper(q.query), "LIMIT") || strings.Contains(plan, "SCAN b") {
						t.Fatal("history was truncated, lost the Task index, or scanned bindings")
					}
					found = true
				}
			}
			if !found {
				t.Fatal("actual Bind history SQL was not captured")
			}
			t.Logf("Bind history state=%s later_terminal_rows=4096 earlier_revision=1 captured_revision=2 other_worker=true elapsed=%s; not a benchmark or hard work bound", state, time.Since(started))
			dispatchAssertFK(t, f.s)
		})
	}
}

func TestBindAssignmentOwnershipPredicatePlan(t *testing.T) {
	f, _, worker := assignmentAttemptWorker(t)
	recorder := assignmentQueryStore(t, f.path)
	if _, err := recorder.s.BindAssignmentAttempt(ctx, worker); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, q := range recorder.queries {
		kind, old, exact := "", "", ""
		if strings.Contains(q.query, "WHERE task_id=? AND group_id=? AND") && strings.Contains(q.query, "coordination_dispatch_state") {
			kind = "public-ownership"
			old, exact = "status IN ('pending','enqueued','held','declined')", "(status IN ('pending','enqueued','held') OR status='declined')"
		} else if strings.Contains(q.query, "ha.id<>:own") {
			kind = "assignment-start"
			old, exact = "other.status IN ('pending','enqueued','held','declined')", "(other.status IN ('pending','enqueued','held') OR other.status='declined')"
		}
		if kind == "" || seen[kind] {
			continue
		}
		seen[kind] = true
		t.Logf("actual production %s SQL=%s args=%v", kind, q.query, q.args)
		current := resolutionPlan(t, f.s, q.query, q.args...)
		t.Logf("actual production %s plan:\n%s", kind, current)
		comparison := resolutionPlan(t, f.s, strings.ReplaceAll(q.query, old, exact), q.args...)
		t.Logf("exact existing OR comparison %s plan:\n%s", kind, comparison)
		if !strings.Contains(comparison, "coordination_dispatch_task_unfinished") {
			t.Errorf("existing OR comparison did not use the partial index: %s", kind)
		}
		if !strings.Contains(current, "coordination_dispatch_task_unfinished") {
			t.Errorf("actual production predicate did not use the existing partial index: %s", kind)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("did not capture both actual ownership predicates: %v", seen)
	}
}

func TestBindAssignmentDependencyScaleAndExplain(t *testing.T) {
	f, r, worker := assignmentAttemptWorker(t)
	tx, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stamp := time.Now().UTC().Format(timestampLayout)
	if _, err := tx.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<1024)
 INSERT INTO tasks(id,group_id,title,acceptance,status,revision,created_at)
 SELECT printf('scale-dep-%04d',i),?,'dependency','accepted evidence','open',1,? FROM n`, r.GroupID, stamp); err != nil {
		t.Fatal(err)
	}
	// Guard-checked bulk historical evidence, not 1023 calls to an unrelated runtime.
	for _, q := range []string{
		`INSERT INTO task_attempts(id,task_id,group_id,task_revision,actor_id,created_at)
 SELECT 'attempt-'||id,id,group_id,1,'user',created_at FROM tasks WHERE id LIKE 'scale-dep-%' AND id<>'scale-dep-1024'`,
		`UPDATE tasks SET active_attempt_id='attempt-'||id WHERE id LIKE 'scale-dep-%' AND id<>'scale-dep-1024'`,
		`INSERT INTO task_submissions(id,task_id,group_id,task_revision,attempt_id,summary,submitted_by,created_at)
 SELECT 'submission-'||id,id,group_id,1,active_attempt_id,'evidence','user',created_at FROM tasks WHERE id LIKE 'scale-dep-%' AND id<>'scale-dep-1024'`,
		`UPDATE tasks SET status='submitted',current_submission_id='submission-'||id WHERE id LIKE 'scale-dep-%' AND id<>'scale-dep-1024'`,
		`INSERT INTO task_review_decisions(id,submission_id,task_id,group_id,task_revision,outcome,reviewed_by,created_at)
 SELECT 'review-'||id,current_submission_id,id,group_id,1,'accepted','user',created_at FROM tasks WHERE id LIKE 'scale-dep-%' AND id<>'scale-dep-1024'`,
		`UPDATE tasks SET status='accepted',active_attempt_id=NULL WHERE id LIKE 'scale-dep-%' AND id<>'scale-dep-1024'`,
	} {
		if _, err := tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec("INSERT INTO task_dependencies(group_id,task_id,dependency_id) SELECT group_id,?,id FROM tasks WHERE id LIKE 'scale-dep-%'", r.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	recorder := assignmentQueryStore(t, f.path)
	got, err := recorder.s.BindAssignmentAttempt(ctx, worker)
	requestDispatchZero(t, got, err, ErrConflict)
	found := false
	for _, q := range recorder.queries {
		if strings.Contains(q.query, "ha.id<>:own") {
			plan := resolutionPlan(t, f.s, q.query, q.args...)
			if !strings.Contains(plan, "SEARCH dep USING COVERING INDEX sqlite_autoindex_task_dependencies_1") {
				t.Fatal("actual dependency fence lost its group/task index")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("actual dependency SQL not captured")
	}
	last, err := f.s.GetTask(ctx, r.GroupID, "scale-dep-1024")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := f.s.ActivateTaskAttempt(ctx, taskAttemptInput(last))
	if err != nil {
		t.Fatal(err)
	}
	submission, err := f.s.SubmitTask(ctx, taskSubmissionInput(attempt))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ReviewTask(ctx, taskReviewInput(submission)); err != nil {
		t.Fatal(err)
	}
	binding, err := recorder.s.BindAssignmentAttempt(ctx, worker)
	if err != nil || binding.TaskID != r.TaskID {
		t.Fatalf("1024 accepted dependencies did not permit real Bind: %+v, %v", binding, err)
	}
	t.Log("1024 dependencies: final unaccepted edge blocked; all accepted allowed; actual SQL EXPLAIN retained; no benchmark")
	dispatchAssertFK(t, f.s)
}
