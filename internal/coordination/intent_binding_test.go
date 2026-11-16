package coordination

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func TestTrustedIntentPassConstructor(t *testing.T) {
	f := intentConfigured(t)
	r := f.request(t)
	engine := New(f.s)
	before := engineCoordinatorSnapshot(t, f.db)
	var reports []error
	report := func(err error) { reports = append(reports, err) }
	for _, tc := range []struct {
		name   string
		engine *Engine
		report func(error)
	}{
		{"nil-engine", nil, report},
		{"nil-store", New(nil), report},
		{"nil-reporter", engine, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callback, err := NewTrustedIntentPass(tc.engine, tc.report)
			if !errors.Is(err, store.ErrInvalid) || callback != nil {
				t.Fatalf("invalid constructor: callback nil=%t, err=%v", callback == nil, err)
			}
		})
	}
	callback, err := NewTrustedIntentPass(engine, report)
	if err != nil || callback == nil || len(reports) != 0 {
		t.Fatalf("constructor: callback nil=%t, reports=%v, err=%v", callback == nil, reports, err)
	}
	if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
		t.Fatal("constructing a callback changed the preloaded pending request or other tables")
	}
	materialized, err := engine.MaterializeDispatchRequest(context.Background(), r.ID, 1, time.Now())
	engineCoordinatorRequireZero(t, materialized, err, ErrDispatchRuntimeNotReady)
	decision, err := engine.DecideDispatch(context.Background(), store.CoordinationToolScope{}, store.CoordinatorDecisionInput{}, "closed")
	engineCoordinatorRequireZero(t, decision, err, ErrDispatchRuntimeNotReady)
	if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) || len(reports) != 0 {
		t.Fatal("constructing the trusted binding opened a public gate or reported work")
	}
	// Construction must not require even a readable Store.
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	callback, err = NewTrustedIntentPass(engine, report)
	if err != nil || callback == nil || len(reports) != 0 {
		t.Fatalf("closed Store was queried during construction: callback nil=%t, reports=%v, err=%v", callback == nil, reports, err)
	}
	if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
		t.Fatal("closed-Store construction changed persisted state")
	}
}

// Invoke only the returned callback and retain the actual reported error tree.
func trustedIntentCallback(t *testing.T, engine *Engine) func(context.Context) error {
	t.Helper()
	var reports []error
	callback, err := NewTrustedIntentPass(engine, func(err error) { reports = append(reports, err) })
	if err != nil || callback == nil || len(reports) != 0 {
		t.Fatalf("constructor: callback nil=%t, reports=%v, err=%v", callback == nil, reports, err)
	}
	return func(ctx context.Context) error {
		t.Helper()
		reports = nil
		callback(ctx)
		t.Logf("callback parent-context=%v reports=%d", ctx.Err(), len(reports))
		if len(reports) == 0 {
			return nil
		}
		if len(reports) != 1 || reports[0] == nil {
			t.Fatalf("one callback must report its complete nonnil error once: %v", reports)
		}
		trustedIntentLogError(t, reports[0])
		return reports[0]
	}
}

type trustedIntentTransactionEvidence interface {
	error
	Phase() string
	RollbackConfirmed() bool
	CommitOutcomeUnknown() bool
	Unwrap() error
}

func trustedIntentErrorChildren(err error) []error {
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		return e.Unwrap()
	case interface{ Unwrap() error }:
		if cause := e.Unwrap(); cause != nil {
			return []error{cause}
		}
	}
	return nil
}

func trustedIntentWalkError(err error, visit func(error, int)) {
	var walk func(error, int)
	walk = func(node error, depth int) {
		if node == nil {
			return
		}
		visit(node, depth)
		for _, cause := range trustedIntentErrorChildren(node) {
			walk(cause, depth+1)
		}
	}
	walk(err, 0)
}

func trustedIntentLogError(t *testing.T, err error) {
	t.Helper()
	trustedIntentWalkError(err, func(node error, depth int) {
		t.Logf("error depth=%d type=%T value=%v", depth, node, node)
		if evidence, ok := node.(trustedIntentTransactionEvidence); ok {
			t.Logf("transaction type=%T phase=%s rollback=%t unknown=%t cause=%T: %v", node,
				evidence.Phase(), evidence.RollbackConfirmed(), evidence.CommitOutcomeUnknown(), evidence.Unwrap(), evidence.Unwrap())
		}
		if len(trustedIntentErrorChildren(node)) != 0 {
			return
		}
		category := "unclassified"
		switch node {
		case context.Canceled:
			category = "context-cancelled"
		case context.DeadlineExceeded:
			category = "context-deadline"
		case sql.ErrTxDone:
			category = "transaction-done"
		case store.ErrInvalid, store.ErrConflict, store.ErrNotFound:
			category = "business"
		}
		if code, ok := node.(interface{ Code() int }); ok {
			category = "sqlite"
			t.Logf("leaf SQLite code=%d", code.Code())
		}
		t.Logf("leaf category=%s type=%T value=%v", category, node, node)
	})
}

func trustedIntentRequireLeaves(t *testing.T, err error, allow func(error) bool) {
	t.Helper()
	trustedIntentWalkError(err, func(node error, _ int) {
		if len(trustedIntentErrorChildren(node)) == 0 && !allow(node) {
			t.Fatalf("unexpected error leaf %T: %v; complete callback tree=%v", node, node, err)
		}
	})
}

func trustedIntentRequireCancellationTree(t *testing.T, err error, allowTxDone bool) {
	t.Helper()
	seen := false
	trustedIntentRequireLeaves(t, err, func(leaf error) bool {
		switch leaf {
		case context.DeadlineExceeded, context.Canceled:
			seen = true
			return true
		case sql.ErrTxDone:
			return allowTxDone
		}
		return false
	})
	if !seen {
		t.Fatalf("missing actual cancellation/deadline leaf: %v", err)
	}
}

func trustedIntentRequireOutput(t *testing.T, f intentFixture, messageID, deliveryID, conversationID, senderID, recipientID string, references ...string) {
	t.Helper()
	if messageID == "" || deliveryID == "" || conversationID == "" {
		t.Fatal("empty output receipt identity")
	}
	m, err := f.s.GetMessage(context.Background(), f.thread.GroupID, messageID)
	if err != nil || m.ID != messageID || m.GroupID != f.thread.GroupID || m.TopicID != f.thread.TopicID ||
		m.SenderID != senderID || len(m.Deliveries) != 1 {
		t.Fatalf("persisted output provenance: %+v, %v", m, err)
	}
	for _, reference := range references {
		if !strings.Contains(m.Content, reference) {
			t.Fatalf("output %s lost provenance %q: %q", messageID, reference, m.Content)
		}
	}
	d := m.Deliveries[0]
	if d.ID != deliveryID || d.MessageID != messageID || d.ConversationID != conversationID ||
		d.RecipientID != recipientID || d.State != domain.DeliveryStateQueued || d.TurnID != "" || d.Error != "" {
		t.Fatalf("output receipt does not identify one queued, unclaimed delivery: %+v", d)
	}
	c, err := f.s.GetConversation(context.Background(), f.thread.GroupID, conversationID)
	if err != nil || c.ID != conversationID || c.GroupID != f.thread.GroupID || c.TopicID != f.thread.TopicID || c.MemberID != recipientID {
		t.Fatalf("output conversation provenance: %+v, %v", c, err)
	}
	var messages, logs, deliveries int
	var logged bool
	// This observer connection does not borrow the callback's cancelled context.
	err = f.db.QueryRow(`SELECT (SELECT count(*) FROM messages WHERE id=?),
	 (SELECT count(*) FROM message_log WHERE message_id=?),
	 (SELECT count(*) FROM deliveries WHERE message_id=?),
	 EXISTS(SELECT 1 FROM message_log WHERE message_id=? AND group_id=? AND topic_id=? AND sequence>0)`,
		messageID, messageID, messageID, messageID, f.thread.GroupID, f.thread.TopicID).Scan(&messages, &logs, &deliveries, &logged)
	if err != nil || messages != 1 || logs != 1 || deliveries != 1 || !logged {
		t.Fatalf("output must have exactly one durable message/log/delivery: %d/%d/%d logged=%t err=%v", messages, logs, deliveries, logged, err)
	}
}

func trustedIntentRequireEffects(t *testing.T, before, after map[string][][]any, outputs int) {
	t.Helper()
	for _, table := range []string{"messages", "message_log", "deliveries"} {
		if len(after[table]) != len(before[table])+outputs {
			t.Fatalf("materialization must add exactly %d %s rows", outputs, table)
		}
		for _, original := range before[table] {
			found := false
			for _, row := range after[table] {
				if reflect.DeepEqual(original, row) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("materialization changed an existing %s row: %v", table, original)
			}
		}
	}
	for _, table := range []string{"turns", "resource_claims", "task_attempts", "task_submissions", "coordination_assignment_attempts"} {
		if !reflect.DeepEqual(before[table], after[table]) {
			t.Fatalf("materialization unexpectedly claimed or executed work: %s", table)
		}
	}
}

func TestTrustedIntentPassAssignmentBudgetSafety(t *testing.T) {
	for _, tc := range []struct {
		name         string
		parentBudget time.Duration
	}{
		{"production-child-budget", 30 * time.Second},
		{"earlier-parent-deadline", 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := intentConfigured(t)
			a := f.assignment(t)
			initial, err := f.s.GetAssignment(context.Background(), a.GroupID, a.ID)
			if err != nil || initial.Assignment != a || initial.Dispatch.AssignmentID != a.ID || initial.Dispatch.Generation != 1 ||
				initial.Dispatch.Status != domain.AssignmentDispatchStatusPending || initial.Dispatch.Delivery != nil || initial.Dispatch.AttemptCount != 0 {
				t.Fatalf("genuine pending assignment setup: %+v, %v", initial, err)
			}
			source, err := f.s.GetDispatchRequest(context.Background(), a.GroupID, a.RequestID)
			if err != nil || source.Request.ID != a.RequestID || source.Request.TaskID != a.TaskID ||
				source.Request.GroupID != a.GroupID || source.Request.TopicID != f.thread.TopicID ||
				source.Request.CoordinatorMemberID != f.coord || source.State.Delivery == nil || a.WorkerMemberID != f.worker || a.DecisionID == "" {
				t.Fatalf("assignment source provenance: %+v, %v", source, err)
			}
			at := time.Now().UTC()
			due, err := f.s.ListDueAssignments(context.Background(), at, store.AssignmentDueOptions{})
			if err != nil || len(due.Items) != 1 || due.Items[0].AssignmentID != a.ID || due.Items[0].Generation != initial.Dispatch.Generation {
				t.Fatalf("assignment must be genuinely due before the production callback: %+v, %v", due, err)
			}
			requests, err := f.s.ListDueDispatchRequests(context.Background(), at, store.DispatchDueOptions{})
			if err != nil || len(requests.Items) != 0 {
				t.Fatalf("unexpected competing pending request: %+v, %v", requests, err)
			}
			before := engineCoordinatorSnapshot(t, f.db)
			callback := trustedIntentCallback(t, New(f.s))
			ctx, cancel := context.WithTimeout(context.Background(), tc.parentBudget)
			defer cancel()
			started := time.Now()
			reported := callback(ctx) // Only the real binding supplies run's shared one-second child.
			elapsed, parentErr := time.Since(started), ctx.Err()
			v, err := f.s.GetAssignment(context.Background(), a.GroupID, a.ID)
			if err != nil {
				t.Fatalf("independent assignment readback: %v", err)
			}
			sourceAfter, err := f.s.GetDispatchRequest(context.Background(), a.GroupID, a.RequestID)
			if err != nil || !reflect.DeepEqual(source, sourceAfter) || v.Assignment != a || v.Dispatch.AssignmentID != a.ID {
				t.Fatalf("callback changed original assignment/source identity: assignment=%+v source=%+v err=%v", v, sourceAfter, err)
			}
			after := engineCoordinatorSnapshot(t, f.db)
			t.Logf("assignment=%s original-generation=%d elapsed=%s parent-context=%v state=%+v receipt=%+v reported=%v",
				a.ID, initial.Dispatch.Generation, elapsed, parentErr, v.Dispatch, v.Dispatch.Delivery, reported)
			if tc.name == "production-child-budget" && parentErr != nil {
				t.Fatalf("production child cancelled or outlived the healthy parent watchdog: %v", parentErr)
			}
			switch v.Dispatch.Status {
			case domain.AssignmentDispatchStatusPending:
				if !reflect.DeepEqual(initial, v) || !reflect.DeepEqual(before, after) {
					t.Fatal("cancelled/pending assignment changed its original generation or persisted partial work")
				}
				if reported == nil {
					t.Fatal("nil callback with a still-pending due assignment is not verified execution")
				}
			case domain.AssignmentDispatchStatusEnqueued:
				d := v.Dispatch.Delivery
				if d == nil || d.WorkerMemberID != a.WorkerMemberID || d.MaterializedFromGeneration != initial.Dispatch.Generation || d.CreatedAt.IsZero() ||
					v.Dispatch.Generation != initial.Dispatch.Generation+1 || v.Dispatch.AttemptCount != 0 || v.Dispatch.LastErrorCode != "" || v.Dispatch.HeldReason != "" ||
					!v.Dispatch.NextAttemptAt.Equal(initial.Dispatch.NextAttemptAt) {
					t.Fatalf("invalid original-generation assignment receipt: %+v", v)
				}
				trustedIntentRequireOutput(t, f, d.MessageID, d.DeliveryID, d.ConversationID, source.Request.CoordinatorMemberID, a.WorkerMemberID,
					"平台根据已保存的协调决定", "Assignment: "+a.ID, "DispatchRequest: "+a.RequestID, "Task: "+a.TaskID, "Mandate: "+source.Request.MandateID)
				trustedIntentRequireEffects(t, before, after, 1)
			default:
				t.Fatalf("fresh eligible assignment has an undeclared durable outcome: %+v", v)
			}
			transactions, commitUnknown := 0, false
			trustedIntentWalkError(reported, func(node error, _ int) {
				evidence, ok := node.(trustedIntentTransactionEvidence)
				if !ok {
					return
				}
				transactions++
				if evidence.Unwrap() == nil || !strings.Contains(reported.Error(), a.ID) || !strings.Contains(reported.Error(), "世代 1") {
					t.Fatalf("typed assignment error lost its original identity or full cause: %v", reported)
				}
				switch evidence.Phase() {
				case "commit":
					if !evidence.CommitOutcomeUnknown() || evidence.RollbackConfirmed() {
						t.Fatal("commit error was reclassified as a known or rolled-back result")
					}
					commitUnknown = true
				case "begin", "source", "eligibility", "payload", "message", "receipt", "readback":
					if evidence.CommitOutcomeUnknown() || v.Dispatch.Delivery != nil {
						t.Fatalf("noncommit assignment failure has contradictory flags or durable output: phase=%s state=%+v", evidence.Phase(), v.Dispatch)
					}
					if evidence.Phase() == "begin" {
						if !evidence.RollbackConfirmed() {
							t.Fatal("Store begin failure lost its no-transaction flag")
						}
						t.Log("begin failure: Store's true flag means no transaction, not an observed successful Rollback")
					}
				default:
					t.Fatalf("unexpected assignment transaction phase %q", evidence.Phase())
				}
			})
			if reported != nil {
				// Every leaf is checked. A joined SQL/business/rollback failure is
				// not made harmless merely by also containing a deadline.
				trustedIntentRequireCancellationTree(t, reported, commitUnknown)
			}
			if transactions == 0 && v.Dispatch.Delivery == nil {
				t.Log("no typed assignment transaction error: candidate/transaction start is unproven; only cancellation and unchanged durable state were observed")
			}
			if v.Dispatch.Delivery == nil {
				t.Logf("pending is observed state, not rollback proof; typed-errors=%d commit-unknown=%t (unreconciled if true)", transactions, commitUnknown)
				return
			}
			// This is historical receipt replay only: the production callback has
			// already persisted the original-generation receipt. Never materialize
			// a pending candidate through a longer-budget bypass here.
			replay, err := f.s.MaterializeAssignment(context.Background(), a.ID, initial.Dispatch.Generation, time.Now().Add(time.Hour))
			if err != nil {
				trustedIntentLogError(t, err)
				t.Fatalf("original-generation receipt reconciliation: %v", err)
			}
			if replay.Disposition != string(domain.AssignmentDispatchStatusEnqueued) || replay.Generation != v.Dispatch.Generation ||
				!reflect.DeepEqual(replay.Delivery, v.Dispatch.Delivery) || replay.HeldReason != "" {
				t.Fatalf("original-generation replay did not return the genuine immutable receipt: %+v", replay)
			}
			readback, err := f.s.GetAssignment(context.Background(), a.GroupID, a.ID)
			if err != nil || !reflect.DeepEqual(v, readback) || !reflect.DeepEqual(after, engineCoordinatorSnapshot(t, f.db)) {
				t.Fatalf("receipt replay changed durable state: %+v, %v", readback, err)
			}
			t.Logf("production callback receipt reconciled at original generation; original commit-unknown evidence=%t", commitUnknown)
		})
	}
}

func TestTrustedIntentPassReportsCompleteErrors(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer stop()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"pre-cancelled", cancelled, context.Canceled},
		{"pre-expired", expired, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := intentConfigured(t)
			f.assignment(t)
			f.request(t)
			callback := trustedIntentCallback(t, New(f.s))
			before := engineCoordinatorSnapshot(t, f.db)
			reported := callback(tc.ctx)
			if !errors.Is(reported, tc.want) {
				t.Fatalf("context error must be reported synchronously once: %v", reported)
			}
			trustedIntentRequireLeaves(t, reported, func(leaf error) bool { return leaf == tc.want })
			if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
				t.Fatal("pre-cancelled or pre-expired callback changed state")
			}
		})
	}
	t.Run("real-statement-errors", func(t *testing.T) {
		f := intentConfigured(t)
		first, second := f.request(t), f.request(t)
		intentExec(t, f.db, `CREATE TABLE trusted_intent_faults(id TEXT PRIMARY KEY, fault TEXT NOT NULL)`)
		intentExec(t, f.db, `INSERT INTO trusted_intent_faults VALUES(?,'first'),(?,'second')`, first.ID, second.ID)
		intentExec(t, f.db, `CREATE TRIGGER trusted_intent_first_fault BEFORE UPDATE ON coordination_dispatch_state
		 WHEN NEW.status='enqueued' AND EXISTS(SELECT 1 FROM trusted_intent_faults WHERE id=NEW.request_id AND fault='first')
		 BEGIN SELECT RAISE(ABORT,'first trusted intent fault'); END;
		 CREATE TRIGGER trusted_intent_second_fault BEFORE UPDATE ON coordination_dispatch_state
		 WHEN NEW.status='enqueued' AND EXISTS(SELECT 1 FROM trusted_intent_faults WHERE id=NEW.request_id AND fault='second')
		 BEGIN SELECT RAISE(ABORT,'second trusted intent fault'); END`)
		before := engineCoordinatorSnapshot(t, f.db)
		callback := trustedIntentCallback(t, New(f.s))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		seen, calls := map[string]bool{}, 0
		for len(seen) < 2 {
			if ctx.Err() != nil {
				t.Fatalf("statement evidence watchdog: calls=%d seen=%v", calls, seen)
			}
			reported := callback(ctx)
			calls++
			if reported == nil {
				continue // An exhaustion probe is not evidence that either fault was reached.
			}
			trustedIntentRequireLeaves(t, reported, func(leaf error) bool {
				if leaf == context.DeadlineExceeded || leaf == context.Canceled {
					return true
				}
				if leaf == sql.ErrTxDone {
					return errors.Is(reported, context.DeadlineExceeded) || errors.Is(reported, context.Canceled)
				}
				code, ok := leaf.(interface{ Code() int })
				return ok && code.Code()&255 == 19 && (strings.Contains(leaf.Error(), "first trusted intent fault") || strings.Contains(leaf.Error(), "second trusted intent fault"))
			})
			var aggregate interface{ Unwrap() []error }
			if !errors.As(reported, &aggregate) {
				t.Fatalf("real aggregate was absent, flattened or replaced: %v", reported)
			}
			// Inspect each real callback tree; the two faults need not share a run.
			for _, cause := range aggregate.Unwrap() {
				for _, want := range []struct{ id, fault string }{
					{first.ID, "first trusted intent fault"},
					{second.ID, "second trusted intent fault"},
				} {
					if !strings.Contains(cause.Error(), want.fault) {
						continue
					}
					var evidence interface {
						Phase() string
						RollbackConfirmed() bool
						CommitOutcomeUnknown() bool
					}
					if !strings.Contains(cause.Error(), want.id) || !errors.As(cause, &evidence) ||
						evidence.Phase() != "receipt" || !evidence.RollbackConfirmed() || evidence.CommitOutcomeUnknown() {
						t.Fatalf("statement cause or real rollback evidence lost for %s: %v", want.id, cause)
					}
					seen[want.id] = true
					t.Logf("callback=%d request=%s phase=%s rollback=%t unknown=%t tree=%v", calls, want.id,
						evidence.Phase(), evidence.RollbackConfirmed(), evidence.CommitOutcomeUnknown(), reported)
				}
			}
		}
		if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
			t.Fatal("persistent statement failures changed state or caused a false Defer")
		}
	})
	t.Run("real-commit-unknown-tree", func(t *testing.T) {
		f := intentConfigured(t)
		r := f.request(t)
		intentExec(t, f.db, `CREATE TABLE trusted_intent_commit_guard(id TEXT REFERENCES groups(id) DEFERRABLE INITIALLY DEFERRED);
		 CREATE TRIGGER trusted_intent_commit_fault AFTER UPDATE ON coordination_dispatch_state
		 WHEN NEW.status='enqueued' BEGIN INSERT INTO trusted_intent_commit_guard VALUES('missing'); END`)
		before := engineCoordinatorSnapshot(t, f.db)
		callback := trustedIntentCallback(t, New(f.s))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for calls := 1; ; calls++ {
			if ctx.Err() != nil {
				t.Fatal("unknown commit evidence watchdog expired")
			}
			reported := callback(ctx)
			if reported == nil {
				continue // Only an actual commit error can satisfy this evidence test.
			}
			trustedIntentRequireLeaves(t, reported, func(leaf error) bool {
				if leaf == context.DeadlineExceeded || leaf == context.Canceled || leaf == sql.ErrTxDone {
					return true
				}
				code, ok := leaf.(interface{ Code() int })
				return ok && code.Code()&255 == 19 && strings.Contains(leaf.Error(), "FOREIGN KEY constraint failed")
			})
			var evidence trustedIntentTransactionEvidence
			if !errors.As(reported, &evidence) || evidence.Phase() != "commit" {
				trustedIntentRequireCancellationTree(t, reported, true)
				continue
			}
			var aggregate interface{ Unwrap() []error }
			if !errors.As(reported, &aggregate) || !errors.Is(reported, sql.ErrTxDone) ||
				!strings.Contains(reported.Error(), r.ID) || !strings.Contains(reported.Error(), "FOREIGN KEY constraint failed") ||
				!evidence.CommitOutcomeUnknown() || evidence.RollbackConfirmed() {
				t.Fatalf("unknown commit or joined rollback cause was lost: %v", reported)
			}
			t.Logf("callback=%d request=%s phase=%s rollback=%t unknown=%t tree=%v nested-causes=%v", calls, r.ID,
				evidence.Phase(), evidence.RollbackConfirmed(), evidence.CommitOutcomeUnknown(), reported, evidence.Unwrap())
			break
		}
		if !reflect.DeepEqual(before, engineCoordinatorSnapshot(t, f.db)) {
			t.Fatal("unknown commit was treated as success, deferred or partially persisted")
		}
	})
}

func TestTrustedIntentPassRetainsSweepAcrossCallbacks(t *testing.T) {
	f := intentConfigured(t)
	f.request(t)
	f.request(t)
	page, err := f.s.ListDueDispatchRequests(context.Background(), time.Now().UTC(), store.DispatchDueOptions{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("candidate setup: %+v, %v", page, err)
	}
	first, tail := page.Items[0], page.Items[1]
	intentExec(t, f.db, `CREATE TRIGGER trusted_intent_prefix_fault BEFORE UPDATE ON coordination_dispatch_state
	 WHEN NEW.request_id='`+first.RequestID+`' BEGIN SELECT RAISE(ABORT,'persistent trusted intent fault'); END`)
	// A real next-kind query failure pauses the sweep without a timing gate or 51-item fixture.
	intentExec(t, f.db, `ALTER TABLE coordination_assignment_dispatch RENAME TO trusted_intent_hidden_assignments`)
	callback := trustedIntentCallback(t, New(f.s))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seen, calls := 0, 0
	for {
		if ctx.Err() != nil {
			t.Fatalf("candidate/query progress watchdog: calls=%d seen=%d", calls, seen)
		}
		reported := callback(ctx)
		calls++
		if reported == nil {
			t.Fatal("real pending candidate/query faults were not reported")
		}
		trustedIntentRequireLeaves(t, reported, func(leaf error) bool {
			if leaf == context.DeadlineExceeded || leaf == context.Canceled {
				return true
			}
			if leaf == sql.ErrTxDone {
				return errors.Is(reported, context.DeadlineExceeded) || errors.Is(reported, context.Canceled)
			}
			code, ok := leaf.(interface{ Code() int })
			return ok && (code.Code()&255 == 19 && strings.Contains(leaf.Error(), "persistent trusted intent fault") ||
				code.Code()&255 == 1 && strings.Contains(leaf.Error(), "no such table: coordination_assignment_dispatch"))
		})
		seen += strings.Count(reported.Error(), first.RequestID)
		if seen > 1 {
			t.Fatal("callback reconstructed its pass and rescanned the failed original candidate")
		}
		if strings.Contains(reported.Error(), "no such table: coordination_assignment_dispatch") {
			if seen != 1 {
				t.Fatalf("query pause did not retain an observed original candidate: %v", reported)
			}
			t.Logf("query pause after original candidate %s: %v", first.RequestID, reported)
			break
		}
	}
	v, err := f.s.GetDispatchRequest(context.Background(), f.thread.GroupID, tail.RequestID)
	if err != nil || v.State.Status != domain.DispatchStatusPending || v.State.Delivery != nil {
		t.Fatalf("query failure did not leave a genuine pending tail: %+v, %v", v, err)
	}
	intentExec(t, f.db, `ALTER TABLE trusted_intent_hidden_assignments RENAME TO coordination_assignment_dispatch`)
	tailObserved := false
	for {
		if ctx.Err() != nil {
			t.Fatalf("tail receipt watchdog: calls=%d", calls)
		}
		reported := callback(ctx)
		calls++
		trustedIntentRequireLeaves(t, reported, func(leaf error) bool {
			if leaf == context.DeadlineExceeded || leaf == context.Canceled {
				return true
			}
			if leaf == sql.ErrTxDone {
				return errors.Is(reported, context.DeadlineExceeded) || errors.Is(reported, context.Canceled)
			}
			code, ok := leaf.(interface{ Code() int })
			return ok && code.Code()&255 == 19 && strings.Contains(leaf.Error(), "persistent trusted intent fault")
		})
		if reported != nil {
			if !tailObserved && strings.Contains(reported.Error(), first.RequestID) {
				t.Fatalf("resumed callback rescanned the failed prefix before observing the tail: %v", reported)
			}
			if strings.Contains(reported.Error(), tail.RequestID) {
				tailObserved = true
			}
		}
		v, err = f.s.GetDispatchRequest(context.Background(), f.thread.GroupID, tail.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("callback=%d tail=%s observed=%t reported=%v", calls, v.State.Status, tailObserved, reported)
		if v.State.Status == domain.DispatchStatusEnqueued && v.State.Delivery != nil &&
			v.State.Delivery.MaterializedFromGeneration == tail.Generation {
			break
		}
		// Once the tail has a real failed attempt, a completed sweep may rewind
		// and retry the pending original generations. Do not mistake that for
		// reconstruction of the pass before its first tail observation.
	}
	rows := engineLocalRows(t, f.db, `SELECT generation,status,attempt_count,last_error_code FROM coordination_dispatch_state
	 WHERE request_id='`+first.RequestID+`'`)
	if !reflect.DeepEqual(rows, [][]any{{int64(first.Generation), "pending", int64(0), ""}}) {
		t.Fatalf("original failure was deferred or partially committed: %v", rows)
	}
	t.Logf("same callback retained original candidate %s and reached receipt %s across %d calls", first.RequestID, tail.RequestID, calls)
}
