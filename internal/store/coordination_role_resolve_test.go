package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"modernc.org/sqlite"
)

// Draft against the N4a interface, not acceptance of the B3 WIP implementation.
// TODO(root/B3): mechanically align helpers and readiness classifications with
// the reviewed B3 baseline before compiling or obtaining the first real RED.
func coordinationRoleQueued(t *testing.T, kind CoordinationRoleKind) (requestDispatchFixture, domain.DispatchRequest, CoordinationRoleProvenance) {
	t.Helper()
	if kind == CoordinationRoleCoordinator {
		f, r := materializePending(t)
		materialized, err := f.s.MaterializeDispatchRequest(ctx, r.ID, 1, materializeAt)
		if err != nil || materialized.Delivery == nil {
			t.Fatalf("coordinator materialization: %v", err)
		}
		d := materialized.Delivery
		return f, r, CoordinationRoleProvenance{Kind: kind, GroupID: r.GroupID,
			DeliveryID: d.DeliveryID, MessageID: d.MessageID, ConversationID: d.ConversationID,
			MemberID: r.CoordinatorMemberID, TopicID: r.TopicID, RequestID: r.ID,
			TaskID: r.TaskID, TaskRevision: r.TaskRevision}
	}
	if kind != CoordinationRoleWorker {
		t.Fatalf("unsupported fixture role %q", kind)
	}
	f, r, decision, _ := assignmentPending(t)
	materialized, err := f.s.MaterializeAssignment(ctx, decision.Assignment.ID, 1, time.Now().UTC())
	if err != nil || materialized.Delivery == nil {
		t.Fatalf("worker materialization: %v", err)
	}
	d := materialized.Delivery
	return f, r, CoordinationRoleProvenance{Kind: kind, GroupID: r.GroupID,
		DeliveryID: d.DeliveryID, MessageID: d.MessageID, ConversationID: d.ConversationID,
		MemberID: f.worker, TopicID: r.TopicID, RequestID: r.ID, TaskID: r.TaskID,
		TaskRevision: r.TaskRevision, AssignmentID: decision.Assignment.ID}
}

func coordinationRoleClaim(t *testing.T, f requestDispatchFixture, p CoordinationRoleProvenance) CoordinationToolScope {
	t.Helper()
	turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: p.GroupID, DeliveryID: p.DeliveryID,
		MaxActive: 8, ResourceKeys: []string{"role-resolve-" + p.DeliveryID}})
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.s.BindMemberTool(ctx, TurnClaim{GroupID: p.GroupID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	return CoordinationToolScope{Member: member, Provenance: p}
}

func coordinationRoleReadOnlyQueries(t *testing.T, s *Store, queries []assignmentQuery, postclaim bool) {
	t.Helper()
	const fence = "UPDATE turns SET claim_token=claim_token"
	if len(queries) == 0 || postclaim && !strings.HasPrefix(strings.TrimSpace(queries[0].query), fence) {
		t.Fatal("resolver did not execute SQL, or postclaim live scope was not the first SQL")
	}
	for _, q := range queries {
		statement := strings.TrimSpace(q.query)
		if postclaim && strings.HasPrefix(statement, fence) {
			continue
		}
		upper := strings.ToUpper(statement)
		if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
			t.Fatal("resolver attempted a write other than the postclaim live scope fence")
		}
		plan := resolutionPlan(t, s, q.query, q.args...)
		if strings.Contains(plan, "task_attempts_group_task_created_id") ||
			strings.Contains(plan, "messages_group_created_id") ||
			strings.Contains(strings.ToLower(q.query), "message_log") {
			t.Fatal("resolver entered Task history or channel history instead of indexed identity lookups")
		}
		for _, table := range []string{"messages", "task_attempts"} {
			if !strings.Contains(strings.ToLower(q.query), table) {
				continue
			}
			keyed := false
			for _, line := range strings.Split(plan, "\n") {
				keyed = keyed || strings.Contains(line, "sqlite_autoindex_"+table+"_") && strings.Contains(line, "(id=?")
			}
			if !keyed {
				t.Fatalf("resolver accessed %s without an exact identity index lookup", table)
			}
		}
	}
}

func TestResolveCoordinationDeliveryOrdinaryRequiresVerifiedAbsence(t *testing.T) {
	f, r := materializePending(t)
	for _, memberID := range []string{f.coord, f.worker} {
		// Role text, a current coordinator binding and request-shaped body are not proof.
		coordinationDecideExec(t, f.s, "UPDATE members SET role='coordinator worker decide_dispatch submit_result' WHERE id=?", memberID)
		message, err := f.s.CreateMessage(ctx, MessageRequest{GroupID: f.groupID, TopicID: r.TopicID,
			ActorID: "user", Content: "DispatchRequest: " + r.ID + "\nAssignment: claimed-by-body"}, []string{memberID})
		if err != nil {
			t.Fatal(err)
		}
		d := message.Deliveries[0]
		before := coordinationDecideSnapshot(t, f.s)
		recorder := assignmentQueryStore(t, f.path)
		got, err := recorder.s.ResolveCoordinationDelivery(ctx, f.groupID, d.ID)
		want := CoordinationRoleState{Provenance: CoordinationRoleProvenance{Kind: CoordinationRoleOrdinary,
			GroupID: f.groupID, DeliveryID: d.ID, MessageID: message.ID, ConversationID: d.ConversationID,
			MemberID: memberID, TopicID: r.TopicID}, ProviderID: "pi", Model: "model", Blocked: CoordinationReady}
		if err != nil || got != want {
			t.Fatalf("verified ordinary identity: %+v, %v", got, err)
		}
		coordinationRoleReadOnlyQueries(t, f.s, recorder.queries, false)
		coordinationDecidePreserved(t, f.s, before)
	}
	foreign := group(t, f.s).ID
	for _, input := range []struct {
		group, delivery string
		want            error
	}{
		{"", "delivery", ErrInvalid}, {f.groupID, " padded", ErrInvalid},
		{f.groupID, "missing", ErrNotFound}, {foreign, "missing", ErrNotFound},
	} {
		before := coordinationDecideSnapshot(t, f.s)
		got, err := f.s.ResolveCoordinationDelivery(ctx, input.group, input.delivery)
		requestDispatchZero(t, got, err, input.want)
		coordinationDecidePreserved(t, f.s, before)
	}
	before := coordinationDecideSnapshot(t, f.s)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	got, err := f.s.ResolveCoordinationDelivery(cancelled, f.groupID, "missing")
	requestDispatchZero(t, got, err, context.Canceled)
	coordinationDecidePreserved(t, f.s, before)
}

func TestResolveCoordinationDeliveryUsesBothUniqueReceiptCandidates(t *testing.T) {
	for _, kind := range []CoordinationRoleKind{CoordinationRoleCoordinator, CoordinationRoleWorker} {
		t.Run(string(kind), func(t *testing.T) {
			f, _, p := coordinationRoleQueued(t, kind)
			before := coordinationDecideSnapshot(t, f.s)
			recorder := assignmentQueryStore(t, f.path)
			got, err := recorder.s.ResolveCoordinationDelivery(ctx, p.GroupID, p.DeliveryID)
			want := CoordinationRoleState{Provenance: p, ProviderID: "pi", Model: "model", Blocked: CoordinationReady}
			if err != nil || got != want {
				t.Fatalf("receipt-derived role: %+v, %v", got, err)
			}
			encoded, err := json.Marshal(got)
			if err != nil || string(encoded) != "{}" {
				t.Fatal("role state escaped trusted process-local JSON boundaries")
			}
			coordinationRoleReadOnlyQueries(t, f.s, recorder.queries, false)
			for _, table := range []string{"coordination_dispatch_state", "coordination_assignment_dispatch"} {
				indexed := false
				for _, q := range recorder.queries {
					for _, line := range strings.Split(resolutionPlan(t, f.s, q.query, q.args...), "\n") {
						if strings.Contains(line, "sqlite_autoindex_"+table+"_") && strings.Contains(line, "(delivery_id=?)") {
							indexed = true
						}
					}
				}
				if !indexed {
					t.Fatalf("actual resolver did not use %s's unique delivery lookup", table)
				}
			}
			coordinationDecidePreserved(t, f.s, before)
			foreign := group(t, f.s).ID
			got, err = f.s.ResolveCoordinationDelivery(ctx, foreign, p.DeliveryID)
			requestDispatchZero(t, got, err, ErrNotFound)
			coordinationRoleClaim(t, f, p)
			before = coordinationDecideSnapshot(t, f.s)
			got, err = f.s.ResolveCoordinationDelivery(ctx, p.GroupID, p.DeliveryID)
			requestDispatchZero(t, got, err, ErrMemberToolDenied)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestResolveCoordinationDeliveryBlockedKeepsImmutableRole(t *testing.T) {
	for _, tc := range []struct {
		change string
		code   CoordinationReadinessCode
	}{
		{"task-revision", CoordinationTaskStale}, {"authority", CoordinationAuthorityStale},
		{"binding", CoordinationBindingStale}, {"policy", CoordinationPolicyStale},
		{"coordinator-left", CoordinationMemberInactive},
	} {
		t.Run(tc.change, func(t *testing.T) {
			f, _, p := coordinationRoleQueued(t, CoordinationRoleCoordinator)
			materializeChangeEligibility(t, f, tc.change)
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.ResolveCoordinationDelivery(ctx, p.GroupID, p.DeliveryID)
			want := CoordinationRoleState{Provenance: p, ProviderID: "pi", Model: "model", Blocked: tc.code}
			if err != nil || got != want {
				t.Fatalf("readiness erased or rewrote role: %+v, %v", got, err)
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestResolveCoordinationWorkerDependenciesAndSameTurnBinding(t *testing.T) {
	f, r, p := coordinationRoleQueued(t, CoordinationRoleWorker)
	parent := createTestTask(t, f.s, r.GroupID)
	// A valid dependency edge added after materialization changes readiness, not provenance.
	coordinationDecideExec(t, f.s, "INSERT INTO task_dependencies(group_id,task_id,dependency_id) VALUES(?,?,?)", r.GroupID, r.TaskID, parent.ID)
	check := func(code CoordinationReadinessCode) {
		t.Helper()
		before := coordinationDecideSnapshot(t, f.s)
		got, err := f.s.ResolveCoordinationDelivery(ctx, p.GroupID, p.DeliveryID)
		want := CoordinationRoleState{Provenance: p, ProviderID: "pi", Model: "model", Blocked: code}
		if err != nil || got != want {
			t.Fatalf("accepted-only dependency readiness: %+v, %v", got, err)
		}
		coordinationDecidePreserved(t, f.s, before)
	}
	check(CoordinationDependencyNotAccepted)
	attempt, err := f.s.ActivateTaskAttempt(ctx, taskAttemptInput(parent))
	if err != nil {
		t.Fatal(err)
	}
	submission, err := f.s.SubmitTask(ctx, taskSubmissionInput(attempt))
	if err != nil {
		t.Fatal(err)
	}
	check(CoordinationDependencyNotAccepted)
	if _, err := f.s.ReviewTask(ctx, taskReviewInput(submission)); err != nil {
		t.Fatal(err)
	}
	check(CoordinationReady)

	scope := coordinationRoleClaim(t, f, p)
	recorder := assignmentQueryStore(t, f.path)
	before := coordinationDecideSnapshot(t, f.s)
	got, err := recorder.s.ResolveCoordinationExecution(ctx, scope.Member, p)
	want := CoordinationRoleState{Provenance: p, ProviderID: "pi", Model: "model", Blocked: CoordinationReady}
	if err != nil || got != want {
		t.Fatalf("unbound postclaim identity: %+v, %v", got, err)
	}
	coordinationRoleReadOnlyQueries(t, f.s, recorder.queries, true)
	coordinationDecidePreserved(t, f.s, before)

	binding, err := f.s.BindAssignmentAttempt(ctx, scope)
	if err != nil || binding.AssignmentID != p.AssignmentID || binding.GroupID != p.GroupID ||
		binding.TaskID != p.TaskID || binding.TaskRevision != p.TaskRevision || binding.WorkerMemberID != p.MemberID ||
		binding.TurnID != scope.Member.Claim.TurnID || binding.AttemptID == "" {
		t.Fatalf("real same-Turn binding failed: %v", err)
	}
	scope.AttemptID = binding.AttemptID
	recorder.queries = nil
	before = coordinationDecideSnapshot(t, f.s)
	want.AttemptID = binding.AttemptID
	got, err = recorder.s.ResolveCoordinationExecution(ctx, scope.Member, p)
	if err != nil || got != want {
		t.Fatalf("resolver did not recover the exact existing Attempt: %+v, %v", got, err)
	}
	coordinationRoleReadOnlyQueries(t, f.s, recorder.queries, true)
	coordinationDecidePreserved(t, f.s, before)
	second := assignmentSecondWorkerTurn(t, f.s, scope)
	before = coordinationDecideSnapshot(t, f.s)
	got, err = f.s.ResolveCoordinationExecution(ctx, second.Member, p)
	requestDispatchZero(t, got, err, ErrMemberToolDenied)
	coordinationDecidePreserved(t, f.s, before)
}

func TestResolveCoordinationExecutionComparesEveryExpectedField(t *testing.T) {
	for _, kind := range []CoordinationRoleKind{CoordinationRoleCoordinator, CoordinationRoleWorker} {
		t.Run(string(kind), func(t *testing.T) {
			f, _, p := coordinationRoleQueued(t, kind)
			scope := coordinationRoleClaim(t, f, p)
			before := coordinationDecideSnapshot(t, f.s)
			recorder := assignmentQueryStore(t, f.path)
			got, err := recorder.s.ResolveCoordinationExecution(ctx, scope.Member, p)
			if err != nil || got != (CoordinationRoleState{Provenance: p, ProviderID: "pi", Model: "model", Blocked: CoordinationReady}) {
				t.Fatalf("postclaim role: %+v, %v", got, err)
			}
			coordinationRoleReadOnlyQueries(t, f.s, recorder.queries, true)
			fields := reflect.TypeOf(p)
			for i := 0; i < fields.NumField(); i++ {
				t.Run(fields.Field(i).Name, func(t *testing.T) {
					bad := p
					field := reflect.ValueOf(&bad).Elem().Field(i)
					if field.Kind() == reflect.String {
						field.SetString(field.String() + "-wrong")
					} else {
						field.SetInt(field.Int() + 1)
					}
					got, err := f.s.ResolveCoordinationExecution(ctx, scope.Member, bad)
					requestDispatchZero(t, got, err, ErrMemberToolDenied)
				})
			}
			for _, expected := range []CoordinationRoleProvenance{{}, {Kind: CoordinationRoleOrdinary}} {
				got, err := f.s.ResolveCoordinationExecution(ctx, scope.Member, expected)
				requestDispatchZero(t, got, err, ErrMemberToolDenied)
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestResolveCoordinationDeliveryCorruptionNeverBecomesOrdinary(t *testing.T) {
	for _, fault := range []string{"failed-delivery", "receipt-member", "missing-request", "dual-with-broken-coordinator", "ordinary-conversation"} {
		t.Run(fault, func(t *testing.T) {
			f, r, p := coordinationRoleQueued(t, CoordinationRoleWorker)
			switch fault {
			case "failed-delivery":
				coordinationDecideExec(t, f.s, "UPDATE deliveries SET state='failed' WHERE id=?", p.DeliveryID)
			case "receipt-member":
				// Counterfactual corruption bypasses only the named receipt guard and FK.
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_dispatch_update")
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=OFF")
				coordinationDecideExec(t, f.s, "UPDATE coordination_assignment_dispatch SET delivered_member_id=? WHERE assignment_id=?", f.coord, p.AssignmentID)
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=ON")
			case "missing-request":
				// Counterfactual missing parent must not make its raw receipt disappear.
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_dispatch_requests_no_delete")
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=OFF")
				coordinationDecideExec(t, f.s, "DELETE FROM coordination_dispatch_requests WHERE id=?", r.ID)
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=ON")
			case "dual-with-broken-coordinator":
				// Counterfactual second candidate is invalid but must still count as a relation.
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_dispatch_state_update")
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_dispatch_receipt_source")
				coordinationDecideExec(t, f.s, `UPDATE coordination_dispatch_state SET message_id=?,delivery_id=?,conversation_id=?,delivered_member_id=? WHERE request_id=?`, p.MessageID, p.DeliveryID, p.ConversationID, p.MemberID, r.ID)
			case "ordinary-conversation":
				d := queueDelivery(t, f.s, f.groupID, f.worker, "ordinary-topic")
				p.DeliveryID = d.ID
				// Counterfactual ordinary parent mismatch tests identity before absence fallback.
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=OFF")
				coordinationDecideExec(t, f.s, "UPDATE conversations SET topic_id='wrong-topic' WHERE id=?", d.ConversationID)
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=ON")
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.ResolveCoordinationDelivery(ctx, p.GroupID, p.DeliveryID)
			requestDispatchZero(t, got, err, ErrMemberToolDenied)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestResolveCoordinationExecutionDamagedRelationCannotDowngrade(t *testing.T) {
	for _, fault := range []string{"backpointer", "receipt-removed", "binding-other-turn"} {
		t.Run(fault, func(t *testing.T) {
			f, _, scope := assignmentBoundWorker(t)
			p := scope.Provenance
			// Counterfactual negative damage follows a fully legal bound worker fixture.
			switch fault {
			case "backpointer":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_dispatch_deliveries_identity")
				coordinationDecideExec(t, f.s, "UPDATE deliveries SET turn_id='' WHERE id=?", p.DeliveryID)
			case "receipt-removed":
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_dispatch_no_delete")
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=OFF")
				coordinationDecideExec(t, f.s, "DELETE FROM coordination_assignment_dispatch WHERE assignment_id=?", p.AssignmentID)
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=ON")
			case "binding-other-turn":
				other := member(t, f.s, f.groupID).ID
				d := queueDelivery(t, f.s, f.groupID, other, p.TopicID)
				turn, err := f.s.ClaimTurn(ctx, ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, MaxActive: 8})
				if err != nil {
					t.Fatal(err)
				}
				coordinationDecideExec(t, f.s, "DROP TRIGGER coordination_assignment_attempts_no_update")
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=OFF")
				coordinationDecideExec(t, f.s, "UPDATE coordination_assignment_attempts SET turn_id=? WHERE assignment_id=?", turn.ID, p.AssignmentID)
				coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=ON")
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.ResolveCoordinationExecution(ctx, scope.Member, p)
			requestDispatchZero(t, got, err, ErrMemberToolDenied)
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestResolveCoordinationExecutionLiveFenceAndLifecycle(t *testing.T) {
	for _, fault := range []string{"old-token", "cancel", "unknown", "recovery", "fence-rotation"} {
		t.Run(fault, func(t *testing.T) {
			f, _, p := coordinationRoleQueued(t, CoordinationRoleWorker)
			scope := coordinationRoleClaim(t, f, p)
			switch fault {
			case "old-token":
				scope.Member.Claim.Token = "old-claim"
			case "cancel":
				if _, err := f.s.RequestTurnCancel(ctx, f.groupID, scope.Member.Claim.TurnID); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				if _, err := f.s.ApplyTurnReceipt(ctx, scope.Member.Claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown}); err != nil {
					t.Fatal(err)
				}
			case "recovery":
				if _, err := f.s.RecoverInFlight(ctx); err != nil {
					t.Fatal(err)
				}
			case "fence-rotation":
				// Counterfactual drift inside the first SQL must be rechecked before role reads succeed.
				coordinationDecideExec(t, f.s, fmt.Sprintf(`CREATE TRIGGER role_resolve_rotate AFTER UPDATE OF claim_token ON turns
 WHEN NEW.id='%s' BEGIN UPDATE turns SET claim_token='rotated' WHERE id=NEW.id; END`, scope.Member.Claim.TurnID))
			}
			before := coordinationDecideSnapshot(t, f.s)
			recorder := assignmentQueryStore(t, f.path)
			got, err := recorder.s.ResolveCoordinationExecution(ctx, scope.Member, p)
			requestDispatchZero(t, got, err, ErrMemberToolDenied)
			if len(recorder.queries) == 0 || !strings.HasPrefix(strings.TrimSpace(recorder.queries[0].query), "UPDATE turns SET claim_token=claim_token") {
				t.Fatal("failed postclaim resolver read before the live scope fence")
			}
			coordinationDecidePreserved(t, f.s, before)
			if fault == "recovery" {
				turn, err := f.s.GetTurn(ctx, f.groupID, scope.Member.Claim.TurnID)
				if err != nil {
					t.Fatal(err)
				}
				scope.Member.Claim.Token = turn.ClaimToken
				got, err = f.s.ResolveCoordinationExecution(ctx, scope.Member, p)
				requestDispatchZero(t, got, err, ErrMemberToolDenied)
			}
		})
	}
}

func TestResolveCoordinationIdentityDoesNotDenyHistoricalDecisionReplay(t *testing.T) {
	for _, kind := range []string{"assign", "decline"} {
		t.Run(kind, func(t *testing.T) {
			f, _, p := coordinationRoleQueued(t, CoordinationRoleCoordinator)
			scope := coordinationRoleClaim(t, f, p)
			input := CoordinatorDecisionInput{Kind: kind, Reason: "历史决定"}
			if kind == "assign" {
				input.WorkerMemberID = f.worker
			}
			original, err := f.s.DecideDispatch(ctx, scope, input, "role-history")
			if err != nil {
				t.Fatal(err)
			}
			before := coordinationDecideSnapshot(t, f.s)
			got, err := f.s.ResolveCoordinationExecution(ctx, scope.Member, p)
			if err != nil || got.Provenance != p || got.AttemptID != "" || got.Blocked != CoordinationAlreadyDecided {
				t.Fatalf("decided coordinator identity was lost: %+v, %v", got, err)
			}
			replayed, err := f.s.DecideDispatch(ctx, scope, input, "role-history")
			if err != nil || !reflect.DeepEqual(replayed, original) {
				t.Fatalf("startup readiness blocked historical replay: %v", err)
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}

func TestResolveCoordinationSQLFailuresAreNotAbsenceOrReadiness(t *testing.T) {
	for _, postclaim := range []bool{false, true} {
		t.Run(fmt.Sprintf("postclaim=%t", postclaim), func(t *testing.T) {
			f, _, p := coordinationRoleQueued(t, CoordinationRoleCoordinator)
			var scope CoordinationToolScope
			if postclaim {
				scope = coordinationRoleClaim(t, f, p)
			}
			// Counterfactual missing receipt table tests a real SQLite error, not a Store mock.
			coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=OFF")
			coordinationDecideExec(t, f.s, "DROP TABLE coordination_assignment_dispatch")
			coordinationDecideExec(t, f.s, "PRAGMA foreign_keys=ON")
			before := coordinationDecideSnapshot(t, f.s)
			var got CoordinationRoleState
			var err error
			if postclaim {
				got, err = f.s.ResolveCoordinationExecution(ctx, scope.Member, p)
			} else {
				got, err = f.s.ResolveCoordinationDelivery(ctx, p.GroupID, p.DeliveryID)
			}
			requestDispatchZero(t, got, err, nil)
			var cause *sqlite.Error
			if !errors.As(err, &cause) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrMemberToolDenied) {
				t.Fatalf("receipt SQL error was converted to absence/denial: %v", err)
			}
			if strings.Contains(err.Error(), "SELECT ") || strings.Contains(err.Error(), "no such table") {
				t.Fatal("resolver exposed raw SQL diagnostics")
			}
			coordinationDecidePreserved(t, f.s, before)
		})
	}
}
