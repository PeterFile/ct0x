package coordination

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

// These helpers only prepare real, guarded Store transactions; run owns all scanning.
type intentFixture struct {
	s      *store.Store
	db     *sql.DB
	thread domain.ThreadRef
	coord  string
	worker string
}

func intentConfigured(t *testing.T) intentFixture {
	t.Helper()
	s, db, thread := engineLocalFixture(t)
	ctx := context.Background()
	coord, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "协调员", Role: "ordinary"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := s.CreateMember(ctx, store.CreateMemberInput{GroupID: thread.GroupID, Name: "执行者", Role: "ordinary"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetCoordinator(ctx, thread.GroupID, coord.ID, 0, "binding"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDispatchPolicy(ctx, thread.GroupID, []string{worker.ID}, 0, "policy"); err != nil {
		t.Fatal(err)
	}
	return intentFixture{s: s, db: db, thread: thread, coord: coord.ID, worker: worker.ID}
}

func (f intentFixture) request(t *testing.T) domain.DispatchRequest {
	t.Helper()
	ctx := context.Background()
	m, err := f.s.CreateMessage(ctx, store.MessageRequest{GroupID: f.thread.GroupID, TopicID: f.thread.TopicID, ActorID: "user", Content: "明确授权"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := f.s.RequestWork(ctx, f.thread.GroupID, domain.RequestWorkRequest{TopicID: f.thread.TopicID, MessageID: m.ID,
		Mode: domain.RoutingDecisionCreateNew, Reason: "授权", NewTask: &domain.NewTaskRequest{Title: "目标", Acceptance: "验收"}}, "work-"+m.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.s.RequestDispatch(ctx, f.thread.GroupID, admitted.Task.ID, 1, 1, "安排", "request-"+m.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f intentFixture) assignment(t *testing.T) domain.Assignment {
	t.Helper()
	ctx := context.Background()
	r := f.request(t)
	m, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, time.Now())
	if err != nil || m.Delivery == nil {
		t.Fatalf("assignment source: %+v, %v", m, err)
	}
	d := m.Delivery
	turn, err := f.s.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: r.GroupID, DeliveryID: d.DeliveryID, MaxActive: 1, ResourceKeys: []string{"intent-setup"}})
	if err != nil {
		t.Fatal(err)
	}
	claim := store.TurnClaim{GroupID: r.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	member, err := f.s.BindMemberTool(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	scope := store.CoordinationToolScope{Member: member, Provenance: store.CoordinationRoleProvenance{
		Kind: store.CoordinationRoleCoordinator, GroupID: r.GroupID, TopicID: r.TopicID, MemberID: f.coord,
		DeliveryID: d.DeliveryID, MessageID: d.MessageID, ConversationID: d.ConversationID, RequestID: r.ID, TaskID: r.TaskID, TaskRevision: r.TaskRevision}}
	decision, err := f.s.DecideDispatch(ctx, scope, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker, Reason: "安排"}, "decision-"+r.ID)
	if err != nil || decision.Assignment == nil {
		t.Fatalf("assignment decision: %+v, %v, cause=%v", decision, err, errors.Unwrap(err))
	}
	if _, err := f.s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	return *decision.Assignment
}

func intentExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func intentCore(t *testing.T, s *store.Store, at time.Time) *intentPass {
	t.Helper()
	p, err := newIntentPass(New(s))
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return at }
	return p
}

func intentAudit(t *testing.T, db *sql.DB) {
	t.Helper()
	intentExec(t, db, `CREATE TABLE intent_audit(kind TEXT,item TEXT);
	 CREATE TRIGGER intent_request_audit AFTER UPDATE ON coordination_dispatch_state
	 WHEN OLD.status='pending' AND NEW.status='enqueued'
	 BEGIN INSERT INTO intent_audit VALUES('request',NEW.request_id); END;
	 CREATE TRIGGER intent_assignment_audit AFTER UPDATE ON coordination_assignment_dispatch
	 WHEN OLD.status='pending' AND NEW.status='enqueued'
	 BEGIN INSERT INTO intent_audit VALUES('assignment',NEW.assignment_id); END`)
}

func TestIntentPassBindingBudgetAndRotation(t *testing.T) {
	for _, e := range []*Engine{nil, New(nil)} {
		if p, err := newIntentPass(e); err == nil || p != nil {
			t.Errorf("nil Engine/Store accepted: %+v, %v", p, err)
		}
	}
	f := intentConfigured(t)
	for i := 0; i < 26; i++ {
		f.request(t)
		f.assignment(t)
	}
	intentAudit(t, f.db)
	p := intentCore(t, f.s, time.Now().Add(time.Second))
	if p.store != f.s {
		t.Fatal("pass did not borrow the exact Engine Store")
	}
	// Bound real two-kind quantity work, not production throughput.
	ctx, cancel := context.WithTimeout(context.Background(), 360*time.Second)
	defer cancel()
	if err := p.observe(ctx); err != nil {
		t.Fatal(err)
	}
	rows := engineLocalRows(t, f.db, "SELECT kind,item FROM intent_audit ORDER BY rowid")
	if len(rows) != 50 {
		t.Fatalf("two kinds must share a 50-observation budget, got %d receipts", len(rows))
	}
	for i, row := range rows {
		want := "request"
		if i%2 == 1 {
			want = "assignment"
		}
		if row[0] != want {
			t.Fatalf("kind rotation at %d: %v", i, row)
		}
	}
	if p.dispatchExhausted || p.assignmentExhausted || p.next != intentDispatch {
		t.Fatalf("full pages invented exhaustion or lost next kind: %+v", p)
	}
	cutoff := p.cutoff
	if err := p.observe(ctx); err != nil {
		t.Fatal(err)
	}
	rows = engineLocalRows(t, f.db, "SELECT kind,item FROM intent_audit ORDER BY rowid")
	if len(rows) != 52 || rows[50][0] != "request" || rows[51][0] != "assignment" || p.cutoff != cutoff || !p.dispatchExhausted || !p.assignmentExhausted {
		t.Fatalf("pass did not resume both original cursors: receipts=%d state=%+v", len(rows), p)
	}
}

func TestIntentPassCutoffFullPageAndEmptyBudget(t *testing.T) {
	f := intentConfigured(t)
	for i := 0; i < 50; i++ {
		f.request(t)
	}
	later := f.request(t)
	base := time.Now().UTC().Add(time.Second)
	if err := f.s.DeferDispatchRequest(context.Background(), later.ID, 1, base.Add(time.Minute), store.DispatchTransientSQLiteBusy); err != nil {
		t.Fatal(err)
	}
	original, err := f.s.ListDueDispatchRequests(context.Background(), base, store.DispatchDueOptions{})
	if err != nil || len(original.Items) != 50 {
		t.Fatalf("due setup: %+v, %v", original, err)
	}
	intentAudit(t, f.db)
	p := intentCore(t, f.s, base)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.observe(ctx); err != nil {
		t.Fatal(err)
	}
	rows := engineLocalRows(t, f.db, "SELECT item FROM intent_audit ORDER BY rowid")
	if len(rows) != 49 || !p.assignmentExhausted || p.dispatchExhausted || p.next != intentAssignment {
		t.Fatalf("empty probe must cost one and lend the other 49 observations: receipts=%d state=%+v", len(rows), p)
	}
	last := original.Items[48]
	if p.dispatchAfter == nil || *p.dispatchAfter != (store.DispatchDueCursor{RequestID: last.RequestID, NextAttemptAt: last.NextAttemptAt}) {
		t.Fatalf("budget boundary lost original key: %+v", p.dispatchAfter)
	}
	p.now = func() time.Time { return base.Add(time.Hour) }
	if err := p.observe(ctx); err != nil {
		t.Fatal(err)
	}
	rows = engineLocalRows(t, f.db, "SELECT item FROM intent_audit ORDER BY rowid")
	if len(rows) != 50 || p.cutoff != base || !p.dispatchExhausted || !p.assignmentExhausted {
		t.Fatalf("cutoff widened or full final page was not followed to empty: receipts=%d state=%+v", len(rows), p)
	}
	view, err := f.s.GetDispatchRequest(context.Background(), later.GroupID, later.ID)
	if err != nil || view.State.Status != domain.DispatchStatusPending || view.State.Generation != 2 {
		t.Fatalf("future candidate crossed the sweep cutoff: %+v, %v", view, err)
	}
	if err := p.observe(ctx); err != nil {
		t.Fatal(err)
	}
	view, err = f.s.GetDispatchRequest(context.Background(), later.GroupID, later.ID)
	if err != nil || view.State.Delivery == nil || view.State.Delivery.MaterializedFromGeneration != 2 || p.cutoff != base.Add(time.Hour) {
		t.Fatalf("common rewind did not admit the later original generation: %+v, %v", view, err)
	}
}

func TestIntentPassPersistentFailurePrefixDoesNotStarve51(t *testing.T) {
	f := intentConfigured(t)
	for i := 0; i < 51; i++ {
		f.request(t)
	}
	at := time.Now().UTC().Add(time.Second)
	page, err := f.s.ListDueDispatchRequests(context.Background(), at, store.DispatchDueOptions{})
	if err != nil || len(page.Items) != 50 || page.NextCursor == nil {
		t.Fatalf("prefix setup: %+v, %v", page, err)
	}
	tail, err := f.s.ListDueDispatchRequests(context.Background(), at, store.DispatchDueOptions{After: page.NextCursor})
	if err != nil || len(tail.Items) != 1 {
		t.Fatalf("tail setup: %+v, %v", tail, err)
	}
	intentExec(t, f.db, `CREATE TABLE intent_bad_prefix(id TEXT PRIMARY KEY)`)
	for _, item := range page.Items {
		intentExec(t, f.db, "INSERT INTO intent_bad_prefix VALUES(?)", item.RequestID)
	}
	intentExec(t, f.db, `CREATE TRIGGER intent_prefix_fault BEFORE UPDATE ON coordination_dispatch_state
	 WHEN EXISTS(SELECT 1 FROM intent_bad_prefix WHERE id=NEW.request_id)
	 BEGIN SELECT RAISE(ABORT,'persistent intent fault'); END`)
	p := intentCore(t, f.s, at)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = p.observe(ctx)
	if err == nil || strings.Count(err.Error(), "persistent intent fault") != 49 {
		t.Fatalf("first pass must expose exactly 49 real rolled-back failures: %v", err)
	}
	if p.dispatchAfter == nil || p.dispatchAfter.RequestID != page.Items[48].RequestID {
		t.Fatalf("failed prefix did not advance original cursor: %+v", p.dispatchAfter)
	}
	err = p.observe(ctx)
	if err == nil || strings.Count(err.Error(), "persistent intent fault") != 1 {
		t.Fatalf("resuming pass rescanned failed prefix or hid item 50: %v", err)
	}
	v, err := f.s.GetDispatchRequest(context.Background(), f.thread.GroupID, tail.Items[0].RequestID)
	if err != nil || v.State.Status != domain.DispatchStatusEnqueued || v.State.Delivery == nil {
		t.Fatalf("item 51 starved behind persistent failures: %+v, %v", v, err)
	}
	rows := engineLocalRows(t, f.db, `SELECT generation,status,attempt_count,last_error_code FROM coordination_dispatch_state
	 WHERE request_id IN (SELECT id FROM intent_bad_prefix)`)
	if len(rows) != 50 {
		t.Fatal("failure prefix lost rows")
	}
	for _, row := range rows {
		if !reflect.DeepEqual(row, []any{int64(1), "pending", int64(0), ""}) {
			t.Fatalf("nontransient failure was deferred or partially committed: %v", row)
		}
	}
}

func TestIntentPassHeldAndGenerationConflict(t *testing.T) {
	t.Run("held-both-kinds", func(t *testing.T) {
		f := intentConfigured(t)
		a := f.assignment(t)
		r := f.request(t)
		intentExec(t, f.db, "UPDATE members SET status='left' WHERE id=?", f.worker)
		before := engineCoordinatorSnapshot(t, f.db)
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		p := intentCore(t, f.s, time.Now().Add(time.Second))
		if err := p.observe(ctx); err != nil {
			t.Fatalf("held is a persisted disposition, not a pass error: %v", err)
		}
		d, err := f.s.GetDispatchRequest(context.Background(), r.GroupID, r.ID)
		if err != nil || d.State.Status != domain.DispatchStatusHeld || d.State.HeldReason != domain.DispatchHeldNoEligibleWorker || d.State.Generation != 2 || d.State.AttemptCount != 0 {
			t.Fatalf("request hold: %+v, %v", d, err)
		}
		v, err := f.s.GetAssignment(context.Background(), a.GroupID, a.ID)
		if err != nil || v.Dispatch.Status != domain.AssignmentDispatchStatusHeld || v.Dispatch.HeldReason != "worker_inactive" || v.Dispatch.Generation != 2 || v.Dispatch.AttemptCount != 0 {
			t.Fatalf("assignment hold: %+v, %v", v, err)
		}
		after := engineCoordinatorSnapshot(t, f.db)
		for _, table := range []string{"messages", "deliveries", "turns", "resource_claims"} {
			if !reflect.DeepEqual(before[table], after[table]) {
				t.Fatalf("held created or released %s", table)
			}
		}
	})
	t.Run("real-generation-race-keeps-original-key", func(t *testing.T) {
		f := intentConfigured(t)
		r := f.request(t)
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		base := time.Now().UTC().Add(time.Second)
		p := intentCore(t, f.s, base)
		calls := 0
		p.now = func() time.Time {
			calls++
			if calls == 2 {
				// A real competing transaction after the due read, before materialization.
				if err := f.s.DeferDispatchRequest(ctx, r.ID, 1, base, store.DispatchTransientSQLiteBusy); err != nil {
					t.Fatal(err)
				}
			}
			return base
		}
		err := p.observe(ctx)
		if !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), r.ID) || !strings.Contains(err.Error(), "世代 1") {
			t.Fatalf("original identity/conflict lost: %v", err)
		}
		if p.dispatchAfter == nil || *p.dispatchAfter != (store.DispatchDueCursor{RequestID: r.ID, NextAttemptAt: r.CreatedAt}) {
			t.Fatalf("cursor followed the mutated retry key: %+v", p.dispatchAfter)
		}
		v, err := f.s.GetDispatchRequest(context.Background(), r.GroupID, r.ID)
		if err != nil || v.State.Generation != 2 || v.State.AttemptCount != 1 || v.State.Delivery != nil {
			t.Fatalf("conflict caused another defer: %+v, %v", v, err)
		}
		p.now = func() time.Time { return base.Add(3 * time.Second) }
		if err := p.observe(ctx); err != nil {
			t.Fatal(err)
		}
		v, err = f.s.GetDispatchRequest(context.Background(), r.GroupID, r.ID)
		if err != nil || v.State.Delivery == nil || v.State.Delivery.MaterializedFromGeneration != 2 {
			t.Fatalf("later sweep did not use current original generation: %+v, %v", v, err)
		}
	})
}

// Structural evidence for the gate unit test only, not a Store fault injector.
type intentTestEvidence struct {
	cause    error
	phase    string
	rollback bool
	unknown  bool
}

func (e intentTestEvidence) Error() string              { return "test transaction evidence" }
func (e intentTestEvidence) Unwrap() error              { return e.cause }
func (e intentTestEvidence) Phase() string              { return e.phase }
func (e intentTestEvidence) RollbackConfirmed() bool    { return e.rollback }
func (e intentTestEvidence) CommitOutcomeUnknown() bool { return e.unknown }

func TestIntentPassTransientEvidenceGate(t *testing.T) {
	_, db, _ := engineLocalFixture(t)
	intentExec(t, db, "CREATE TABLE intent_locked(id); INSERT INTO intent_locked VALUES(1),(2)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT id FROM intent_locked")
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("missing active SQLite cursor")
	}
	_, locked := tx.Exec("DROP TABLE intent_locked")
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	rollback := tx.Rollback()
	if rollback != nil || locked == nil {
		t.Fatalf("real statement/rollback evidence: %v / %v", locked, rollback)
	}
	if code, ok := store.ClassifyDispatchTransient(locked); !ok || code != store.DispatchTransientSQLiteLocked {
		t.Fatalf("not a real SQLite LOCKED cause: %v", locked)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now())
	defer cancel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"observed-rollback", fmt.Errorf("wrapped: %w", intentTestEvidence{cause: locked, phase: "message", rollback: true}), true},
		{"begin-is-not-rollback", intentTestEvidence{cause: locked, phase: "begin", rollback: true}, false},
		{"input-is-not-rollback", intentTestEvidence{cause: locked, phase: "input", rollback: true}, false},
		{"missing-rollback", intentTestEvidence{cause: locked, phase: "message"}, false},
		{"commit-unknown", intentTestEvidence{cause: locked, phase: "commit", unknown: true, rollback: true}, false},
		{"unknown-despite-phase", intentTestEvidence{cause: locked, phase: "message", unknown: true, rollback: true}, false},
		{"cause-only", locked, false},
		{"message-only", errors.New("SQLITE_BUSY: database is locked"), false},
		{"cancelled", intentTestEvidence{cause: errors.Join(locked, context.Canceled), phase: "message", rollback: true}, false},
		{"deadline-before-defer", intentTestEvidence{cause: errors.Join(locked, expired.Err()), phase: "message", rollback: true}, false},
		{"conflict", intentTestEvidence{cause: errors.Join(locked, store.ErrConflict), phase: "receipt", rollback: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, ok := intentTransient(tc.err)
			if ok != tc.want || ok && code != store.DispatchTransientSQLiteLocked || !ok && code != "" {
				t.Fatalf("unsafe or missing defer eligibility: code=%q ok=%t want=%t", code, ok, tc.want)
			}
		})
	}
	t.Log("gate unit evidence uses real SQLite LOCKED and an observed raw transaction rollback; it is not a Store materialization transient")
}

func TestIntentPassCommitUnknownAndOriginalGenerationReplay(t *testing.T) {
	f := intentConfigured(t)
	a := f.assignment(t)
	r := f.request(t)
	intentExec(t, f.db, `CREATE TABLE intent_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED);
	 CREATE TRIGGER intent_request_commit AFTER UPDATE ON coordination_dispatch_state
	 WHEN NEW.status='enqueued' BEGIN INSERT INTO intent_commit_guard VALUES('missing'); END;
	 CREATE TRIGGER intent_assignment_commit AFTER UPDATE ON coordination_assignment_dispatch
	 WHEN NEW.status='enqueued' BEGIN INSERT INTO intent_commit_guard VALUES('missing'); END`)
	before := engineCoordinatorSnapshot(t, f.db)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	p := intentCore(t, f.s, time.Now().Add(time.Second))
	err := p.observe(ctx)
	if err == nil || !strings.Contains(err.Error(), r.ID) || !strings.Contains(err.Error(), a.ID) || !strings.Contains(err.Error(), "世代 1") {
		t.Fatalf("unknown commit/original identities not preserved: %v", err)
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != 2 {
		t.Fatalf("expected both original candidate error trees, got: %v", err)
	}
	for _, want := range []struct{ kind, id, prefix string }{
		{"dispatch", r.ID, fmt.Sprintf("物化分派请求 %s（世代 1）失败：", r.ID)},
		{"assignment", a.ID, fmt.Sprintf("物化工作分配 %s（世代 1）失败：", a.ID)},
	} {
		var observed error
		for _, failure := range joined.Unwrap() {
			if strings.HasPrefix(failure.Error(), want.prefix) {
				if observed != nil {
					t.Fatalf("duplicate %s original candidate error: %v", want.kind, err)
				}
				observed = failure
			}
		}
		var evidence interface {
			Phase() string
			RollbackConfirmed() bool
			CommitOutcomeUnknown() bool
			Unwrap() error
		}
		if observed == nil || !errors.As(observed, &evidence) || evidence.Phase() != "commit" ||
			!evidence.CommitOutcomeUnknown() || evidence.RollbackConfirmed() || evidence.Unwrap() == nil {
			t.Fatalf("%s original-generation commit evidence missing: candidate=%v full-tree=%v", want.kind, observed, err)
		}
		t.Logf("actual commit unknown: kind=%s original-id=%s generation=1 phase=%s unknown=%t rollback-confirmed=%t raw-cause=%v full-tree=%v", want.kind, want.id,
			evidence.Phase(), evidence.CommitOutcomeUnknown(), evidence.RollbackConfirmed(), evidence.Unwrap(), observed)
	}
	if p.dispatchAfter == nil || p.dispatchAfter.RequestID != r.ID || p.assignmentAfter == nil || p.assignmentAfter.AssignmentID != a.ID || !p.dispatchExhausted || !p.assignmentExhausted {
		t.Fatalf("unknown commit stopped cursor progress: %+v", p)
	}
	if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
		t.Fatal("unknown commit was deferred, treated as success, or partially persisted")
	}
	intentExec(t, f.db, "DROP TRIGGER intent_request_commit; DROP TRIGGER intent_assignment_commit")
	if err := p.observe(ctx); err != nil {
		t.Fatalf("pending original generations did not reconcile on a later sweep: %v", err)
	}
	d, err := f.s.GetDispatchRequest(context.Background(), r.GroupID, r.ID)
	if err != nil || d.State.Delivery == nil || d.State.Delivery.MaterializedFromGeneration != 1 || d.State.AttemptCount != 0 {
		t.Fatalf("request reconciliation: %+v, %v", d, err)
	}
	v, err := f.s.GetAssignment(context.Background(), a.GroupID, a.ID)
	if err != nil || v.Dispatch.Delivery == nil || v.Dispatch.Delivery.MaterializedFromGeneration != 1 || v.Dispatch.AttemptCount != 0 {
		t.Fatalf("assignment reconciliation: %+v, %v", v, err)
	}
	after := engineCoordinatorSnapshot(t, f.db)
	dReplay, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, time.Now().Add(time.Hour))
	if err != nil || !reflect.DeepEqual(dReplay.Delivery, d.State.Delivery) {
		t.Fatalf("request original-generation replay: %+v, %v", dReplay, err)
	}
	aReplay, err := f.s.MaterializeAssignment(ctx, a.ID, 1, time.Now().Add(time.Hour))
	if err != nil || !reflect.DeepEqual(aReplay.Delivery, v.Dispatch.Delivery) {
		t.Fatalf("assignment original-generation replay: %+v, %v", aReplay, err)
	}
	if !reflect.DeepEqual(after, engineCoordinatorSnapshot(t, f.db)) {
		t.Fatal("original-generation receipt replay duplicated or changed rows")
	}
	for _, table := range []string{"messages", "message_log", "deliveries"} {
		if len(after[table]) != len(before[table])+2 {
			t.Fatalf("reconciliation must add exactly two %s rows", table)
		}
	}
}

func TestIntentPassRealCommitBusyDoesNotDefer(t *testing.T) {
	f := intentConfigured(t)
	r := f.request(t)
	reader, err := f.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	var generation int
	if err := reader.QueryRow("SELECT generation FROM coordination_dispatch_state WHERE request_id=?", r.ID).Scan(&generation); err != nil || generation != 1 {
		t.Fatalf("reader lock: %d, %v", generation, err)
	}
	p := intentCore(t, f.s, time.Now().Add(time.Second))
	callErr := p.run(context.Background())
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	var evidence interface {
		Phase() string
		RollbackConfirmed() bool
		CommitOutcomeUnknown() bool
		Unwrap() error
	}
	if !errors.As(callErr, &evidence) {
		t.Fatalf("missing COMMIT transaction evidence: %v", callErr)
	}
	code, transient := store.ClassifyDispatchTransient(evidence.Unwrap())
	_, aggregateTransient := store.ClassifyDispatchTransient(callErr)
	if !transient || code != store.DispatchTransientSQLiteBusy || aggregateTransient || !errors.Is(callErr, context.DeadlineExceeded) ||
		evidence.Phase() != "commit" || !evidence.CommitOutcomeUnknown() || evidence.RollbackConfirmed() {
		t.Fatalf("lost actual unknown COMMIT BUSY or mixed deadline tree: %v cause=%v", callErr, evidence.Unwrap())
	}
	t.Logf("phase=%s original-cause=%v unknown=%t rollback=%t aggregate-transient=%t mixed=%v", evidence.Phase(), evidence.Unwrap(), evidence.CommitOutcomeUnknown(), evidence.RollbackConfirmed(), aggregateTransient, callErr)
	v, err := f.s.GetDispatchRequest(context.Background(), r.GroupID, r.ID)
	if err != nil || v.State.Generation != 1 || v.State.AttemptCount != 0 || v.State.Delivery != nil || p.dispatchAfter == nil || p.dispatchAfter.RequestID != r.ID {
		t.Fatalf("COMMIT BUSY was deferred or lost its cursor: %+v, %v state=%+v", v, err, p)
	}
}

// Count production deadline derivations without replacing any Store operation.
type intentBudgetParent struct {
	context.Context
	derivations int
}

func (c *intentBudgetParent) Deadline() (time.Time, bool) {
	c.derivations++
	return c.Context.Deadline()
}

// The observe-only fixture advances a logical deadline at a candidate boundary.
// Its real parent deadline still bounds the exercise if that boundary is missed.
type intentControlledDeadline struct{ context.Context }

func (c intentControlledDeadline) Err() error {
	if errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

func TestIntentPassSharedRunBudget(t *testing.T) {
	t.Run("controlled-mixed-deadline", func(t *testing.T) {
		f := intentConfigured(t)
		first, second := f.request(t), f.request(t)
		intentExec(t, f.db, `CREATE TRIGGER intent_budget_fault BEFORE UPDATE ON coordination_dispatch_state
		 WHEN NEW.request_id='`+first.ID+`'
		 BEGIN SELECT RAISE(ABORT,'shared budget SQL fault'); END`)
		before := engineCoordinatorSnapshot(t, f.db)
		at := time.Now().UTC().Add(time.Second)
		p := intentCore(t, f.s, at)
		parent, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		child, expire := context.WithCancelCause(parent)
		defer expire(context.Canceled)
		ctx := intentControlledDeadline{child}
		calls := 0
		p.now = func() time.Time {
			calls++
			if calls == 3 {
				// The real SQL failure and rollback precede logical expiry. No
				// successful transaction is required inside the production 1s.
				expire(context.DeadlineExceeded)
			}
			return at
		}
		err := p.observe(ctx)
		var evidence interface {
			Phase() string
			RollbackConfirmed() bool
			CommitOutcomeUnknown() bool
		}
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "shared budget SQL fault") ||
			!errors.As(err, &evidence) || evidence.Phase() != "receipt" || !evidence.RollbackConfirmed() || evidence.CommitOutcomeUnknown() {
			t.Fatalf("controlled deadline discarded real SQL/rollback evidence: %v", err)
		}
		if calls != 3 || parent.Err() != nil || p.cutoff != at ||
			p.dispatchAfter == nil || *p.dispatchAfter != (store.DispatchDueCursor{RequestID: first.ID, NextAttemptAt: first.CreatedAt}) ||
			!p.assignmentExhausted || p.dispatchExhausted {
			t.Fatalf("controlled expiry changed parent or original sweep: calls=%d parent=%v state=%+v", calls, parent.Err(), p)
		}
		if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
			t.Fatal("expiry started later Materialize/Defer or partially wrote")
		}
		t.Logf("controlled deadline after real SQL: first=%s next=%s cursor=%+v cutoff=%s mixed=%v", first.ID, second.ID, p.dispatchAfter, p.cutoff, err)
	})
	for _, earlierParent := range []bool{false, true} {
		name := "production-child"
		if earlierParent {
			name = "earlier-parent-deadline"
		}
		t.Run(name, func(t *testing.T) {
			f := intentConfigured(t)
			f.request(t)
			before := engineCoordinatorSnapshot(t, f.db)
			locked, err := f.db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer locked.Close()
			if _, err := locked.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
				t.Fatal(err)
			}
			defer locked.ExecContext(context.Background(), "ROLLBACK")
			parentCtx := context.Background()
			if earlierParent {
				var cancel context.CancelFunc
				parentCtx, cancel = context.WithDeadline(parentCtx, time.Now().Add(100*time.Millisecond))
				defer cancel()
			}
			parent := &intentBudgetParent{Context: parentCtx}
			at := time.Now().UTC().Add(time.Second)
			p := intentCore(t, f.s, at)
			err = p.run(parent)
			if !errors.Is(err, context.DeadlineExceeded) || parent.derivations != 1 || p.cutoff != at ||
				p.dispatchAfter != nil || p.assignmentAfter != nil || p.dispatchExhausted || p.assignmentExhausted || p.next != intentDispatch {
				t.Fatalf("production run lost its one inherited deadline or advanced a failed query: derivations=%d state=%+v err=%v", parent.derivations, p, err)
			}
			if earlierParent && !errors.Is(parent.Err(), context.DeadlineExceeded) || !earlierParent && parent.Err() != nil {
				t.Fatalf("wrong parent outcome: earlier=%t parent=%v pass=%v", earlierParent, parent.Err(), err)
			}
			if _, err := locked.ExecContext(context.Background(), "ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
				t.Fatal("deadline query performed Materialize/Defer or partial writes")
			}
			t.Logf("production query deadline: earlier-parent=%t derivations=%d parent=%v cursor=%+v cutoff=%s full-tree=%v", earlierParent, parent.derivations, parent.Err(), p.dispatchAfter, p.cutoff, err)
		})
	}
}

func TestIntentPassBudgetCancellationBoundaries(t *testing.T) {
	for _, kind := range []intentKind{intentDispatch, intentAssignment} {
		for _, boundary := range []string{"query-child-deadline", "query-parent-deadline", "before-materialize-parent-cancel"} {
			t.Run(fmt.Sprintf("%d/%s", kind, boundary), func(t *testing.T) {
				f := intentConfigured(t)
				if kind == intentDispatch {
					f.request(t)
				} else {
					f.assignment(t)
				}
				before := engineCoordinatorSnapshot(t, f.db)
				at := time.Now().UTC().Add(time.Second)
				p := intentCore(t, f.s, at)
				p.next = kind
				ctx, cancel := context.WithCancel(context.Background())
				var locked *sql.Conn
				if strings.HasPrefix(boundary, "query-") {
					var err error
					locked, err = f.db.Conn(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					defer locked.Close()
					if _, err := locked.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
						t.Fatal(err)
					}
					defer locked.ExecContext(context.Background(), "ROLLBACK")
				} else {
					calls := 0
					p.now = func() time.Time {
						calls++
						if calls == 2 {
							cancel() // Due read completed, Materialize has not begun.
						}
						return at
					}
				}
				if boundary == "query-parent-deadline" {
					cancel()
					ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(100*time.Millisecond))
				}
				defer cancel()
				err := p.run(ctx)
				want := context.DeadlineExceeded
				if boundary == "before-materialize-parent-cancel" {
					want = context.Canceled
				}
				if !errors.Is(err, want) || p.cutoff != at || p.dispatchAfter != nil || p.assignmentAfter != nil ||
					p.dispatchExhausted || p.assignmentExhausted {
					t.Fatalf("cancellation invented cursor, exhaustion, or a widened cutoff: err=%v state=%+v", err, p)
				}
				if boundary == "query-child-deadline" && ctx.Err() != nil || boundary != "query-child-deadline" && !errors.Is(ctx.Err(), want) {
					t.Fatalf("wrong parent cancellation/deadline: parent=%v pass=%v", ctx.Err(), err)
				}
				if locked != nil {
					if p.next != kind {
						t.Fatal("failed query rotated without an observation")
					}
					if _, err := locked.ExecContext(context.Background(), "ROLLBACK"); err != nil {
						t.Fatal(err)
					}
				}
				if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
					t.Fatal("cancelled query/Materialize boundary started writes or Defer")
				}
				t.Logf("kind=%d boundary=%s parent=%v cutoff=%s cause=%v", kind, boundary, ctx.Err(), p.cutoff, err)
			})
		}
	}
}

func TestIntentPassStoreDefaultBeginBusy(t *testing.T) {
	f := intentConfigured(t)
	r := f.request(t)
	before := engineCoordinatorSnapshot(t, f.db)
	writer, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.Exec("UPDATE groups SET name=name WHERE id=?", f.thread.GroupID); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, callErr := f.s.MaterializeDispatchRequest(context.Background(), r.ID, 1, time.Now())
	var evidence interface {
		Phase() string
		RollbackConfirmed() bool
		CommitOutcomeUnknown() bool
		Unwrap() error
	}
	if !errors.As(callErr, &evidence) {
		t.Fatalf("missing direct Store BEGIN evidence: %v", callErr)
	}
	code, transient := store.ClassifyDispatchTransient(evidence.Unwrap())
	if evidence.Phase() != "begin" || !evidence.RollbackConfirmed() || evidence.CommitOutcomeUnknown() ||
		!transient || code != store.DispatchTransientSQLiteBusy || errors.Is(callErr, context.DeadlineExceeded) || errors.Is(callErr, context.Canceled) {
		t.Fatalf("not a default5000 no-transaction SQLITE_BUSY cause: %v", callErr)
	}
	if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
		t.Fatal("direct BEGIN BUSY partially wrote or deferred")
	}
	t.Logf("unmodified Store.Open default5000: phase=%s cause=%v no-transaction=%t unknown=%t elapsed=%s", evidence.Phase(), evidence.Unwrap(), evidence.RollbackConfirmed(), evidence.CommitOutcomeUnknown(), time.Since(started))
}

func TestIntentPassCancellationAndQueryFailure(t *testing.T) {
	t.Run("cancel-between-items-and-resume", func(t *testing.T) {
		f := intentConfigured(t)
		for i := 0; i < 3; i++ {
			f.request(t)
		}
		base := time.Now().Add(time.Second)
		p := intentCore(t, f.s, base)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		before := engineCoordinatorSnapshot(t, f.db)
		if err := p.run(ctx); !errors.Is(err, context.Canceled) || !p.cutoff.IsZero() {
			t.Fatalf("pre-cancelled pass did work: %v %+v", err, p)
		}
		if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
			t.Fatal("pre-cancelled pass wrote rows")
		}
		ctx, cancel = context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		calls := 0
		p.now = func() time.Time {
			calls++
			if calls == 3 {
				cancel()
			}
			return base
		}
		if err := p.observe(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("mid-pass cancellation not exposed: %v", err)
		}
		rows := engineLocalRows(t, f.db, "SELECT request_id FROM coordination_dispatch_state WHERE status='enqueued'")
		if len(rows) != 1 || p.dispatchAfter == nil || p.dispatchAfter.RequestID != rows[0][0] {
			t.Fatalf("cancellation did extra work or lost completed cursor: %v %+v", rows, p)
		}
		p.now = func() time.Time { return base.Add(time.Hour) }
		resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer resumeCancel()
		if err := p.observe(resumeCtx); err != nil {
			t.Fatal(err)
		}
		if rows := engineLocalRows(t, f.db, "SELECT request_id FROM coordination_dispatch_state WHERE status='enqueued'"); len(rows) != 3 || p.cutoff != base {
			t.Fatalf("cancelled pass did not resume the original sweep: %v %+v", rows, p)
		}
	})
	t.Run("closed-db-is-not-exhaustion", func(t *testing.T) {
		f := intentConfigured(t)
		f.request(t)
		p := intentCore(t, f.s, time.Now().Add(time.Second))
		if err := f.s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := p.run(context.Background()); err == nil {
			t.Fatal("closed database query failure hidden")
		}
		if p.dispatchAfter != nil || p.assignmentAfter != nil || p.dispatchExhausted || p.assignmentExhausted || p.next != intentDispatch {
			t.Fatalf("query failure invented a key, exhaustion, or successful rotation: %+v", p)
		}
	})
}

func TestIntentPassPreservesUnknownAndCancelResources(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateUnknown, domain.TurnStateCancelRequested} {
		t.Run(string(state), func(t *testing.T) {
			s, r, scope, worker, db := engineDecideFixture(t)
			ctx := context.Background()
			decision, err := s.DecideDispatch(ctx, scope, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: worker, Reason: "安排"}, "decision")
			if err != nil || decision.Assignment == nil {
				t.Fatalf("real assignment fixture: %+v, %v", decision, err)
			}
			claim := scope.Member.Claim
			if err := s.RecordTurnEvent(ctx, claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "intent-native"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RequestTurnCancel(ctx, r.GroupID, claim.TurnID); err != nil {
				t.Fatal(err)
			}
			if state == domain.TurnStateUnknown {
				if _, err := s.ApplyTurnReceipt(ctx, claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown, Started: true, Accepted: true, NativeSessionID: "intent-native"}); err != nil {
					t.Fatal(err)
				}
			}
			before := engineCoordinatorSnapshot(t, db)
			turn, err := s.GetTurn(ctx, r.GroupID, claim.TurnID)
			if err != nil || turn.State != state || len(before["resource_claims"]) != 1 {
				t.Fatalf("uncertain execution fixture: %+v, %v", turn, err)
			}
			ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
			defer cancel()
			p := intentCore(t, s, time.Now().Add(time.Second))
			if err := p.observe(ctx); err != nil {
				t.Fatal(err)
			}
			v, err := s.GetAssignment(ctx, r.GroupID, decision.Assignment.ID)
			if err != nil || v.Dispatch.Delivery == nil {
				t.Fatalf("core did not perform real work beside uncertain execution: %+v, %v", v, err)
			}
			after := engineCoordinatorSnapshot(t, db)
			for _, table := range []string{"turns", "resource_claims", "task_attempts", "task_submissions"} {
				if !reflect.DeepEqual(before[table], after[table]) {
					t.Fatalf("intent pass changed uncertain execution/resource evidence: %s", table)
				}
			}
			got, err := s.GetTurn(ctx, r.GroupID, claim.TurnID)
			if err != nil || !reflect.DeepEqual(got, turn) {
				t.Fatalf("uncertain Turn changed: %+v, %v", got, err)
			}
		})
	}
}
