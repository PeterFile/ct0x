package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func dispatchExecutionPath(groupID, taskID, requestID string) string {
	return dispatchRequestHTTPPostPath(groupID, taskID) + "/" + url.PathEscape(requestID) + "/execution"
}

func dispatchExecutionRead(t *testing.T, a *groupAPI, request domain.DispatchRequest) map[string]any {
	t.Helper()
	return decoded[map[string]any](t, a.request("GET", dispatchExecutionPath(request.GroupID, request.TaskID, request.ID), ""), 200)
}

func dispatchExecutionRegister(t *testing.T, f dispatchRequestHTTPFixture) domain.DispatchRequest {
	t.Helper()
	return dispatchRequestHTTPValue[domain.DispatchRequest](t, f.a.request("POST", f.path, f.body, "execution"), 201)
}

func dispatchExecutionClaim(t *testing.T, f dispatchRequestHTTPFixture, deliveryID string) (domain.Turn, store.CoordinationToolScope) {
	t.Helper()
	ctx := context.Background()
	role, err := f.a.db.ResolveCoordinationDelivery(ctx, f.group.ID, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.a.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.group.ID, DeliveryID: deliveryID, MaxActive: 8, ResourceKeys: []string{"execution:" + role.Provenance.MemberID}})
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.a.db.BindMemberTool(ctx, store.TurnClaim{GroupID: f.group.ID, TurnID: turn.ID, Token: turn.ClaimToken})
	if err != nil {
		t.Fatal(err)
	}
	return turn, store.CoordinationToolScope{Member: member, Provenance: role.Provenance}
}

func dispatchExecutionAssigned(t *testing.T, f dispatchRequestHTTPFixture, request domain.DispatchRequest) (domain.Turn, domain.DecisionReceipt) {
	t.Helper()
	materialized, err := f.a.db.MaterializeDispatchRequest(context.Background(), request.ID, 1, time.Now().UTC())
	if err != nil || materialized.Delivery == nil {
		t.Fatalf("coordinator materialization: %+v, %v", materialized, err)
	}
	turn, scope := dispatchExecutionClaim(t, f, materialized.Delivery.DeliveryID)
	decision, err := f.a.db.DecideDispatch(context.Background(), scope, store.CoordinatorDecisionInput{Kind: "assign", WorkerMemberID: f.worker.ID, Reason: "PRIVATE decision reason"}, "decision")
	if err != nil {
		t.Fatal(err)
	}
	return turn, decision
}

func TestDispatchExecutionHTTPUnobservedAndExactScope(t *testing.T) {
	f := dispatchRequestHTTPSetup(t)
	request := dispatchExecutionRegister(t, f)
	other := dispatchRequestHTTPConfigure(t, f.a, newGroup(t, f.a, "other"))
	otherTask, err := f.a.db.CreateTask(context.Background(), store.CreateTaskInput{GroupID: f.group.ID, ActorID: "user", Title: "other task", Acceptance: "exact scope"})
	if err != nil {
		t.Fatal(err)
	}
	raw := messageSQL(t, f.a)
	before := coordinationHTTPSnapshot(t, raw)
	got := dispatchExecutionRead(t, f.a, request)
	want := map[string]any{"group_id": f.group.ID, "task_id": request.TaskID, "request_id": request.ID, "dispatch_status": "pending",
		"coordinator_turn": nil, "decision": nil, "assignment": nil, "assignment_dispatch_status": nil, "binding": nil, "worker_turn": nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("absence must be explicit, not inferred execution: %+v", got)
	}
	for _, scope := range [][3]string{{other.group.ID, request.TaskID, request.ID}, {f.group.ID, other.admission.Task.ID, request.ID},
		{f.group.ID, otherTask.ID, request.ID}, {f.group.ID, request.TaskID, "missing"}, {"missing", request.TaskID, request.ID}} {
		dispatchRequestHTTPError(t, f.a.request("GET", dispatchExecutionPath(scope[0], scope[1], scope[2]), ""), 404, "not_found")
	}
	coordinationHTTPUnchanged(t, raw, before)
}

func TestDispatchExecutionHTTPMaterializationAssignmentAndBindingAreNotExecution(t *testing.T) {
	f := dispatchRequestHTTPSetup(t)
	request := dispatchExecutionRegister(t, f)
	coordinator, decision := dispatchExecutionAssigned(t, f, request)
	got := dispatchExecutionRead(t, f.a, request)
	if got["dispatch_status"] != "assigned" || got["worker_turn"] != nil || got["binding"] != nil || got["assignment_dispatch_status"] != "pending" {
		t.Fatalf("assignment invented execution: %+v", got)
	}
	lead := got["coordinator_turn"].(map[string]any)
	if lead["id"] != coordinator.ID || lead["state"] != "starting" || lead["receipt"] != nil {
		t.Fatalf("claimed coordinator without receipt: %+v", lead)
	}
	if got["decision"].(map[string]any)["id"] != decision.Decision.ID || got["assignment"].(map[string]any)["id"] != decision.Assignment.ID {
		t.Fatal("missing actual immutable decision/assignment")
	}
	materialized, err := f.a.db.MaterializeAssignment(context.Background(), decision.Assignment.ID, 1, time.Now().UTC())
	if err != nil || materialized.Delivery == nil {
		t.Fatal(err)
	}
	if got := dispatchExecutionRead(t, f.a, request); got["worker_turn"] != nil || got["binding"] != nil {
		t.Fatalf("queued delivery invented a Turn: %+v", got)
	}
	worker, scope := dispatchExecutionClaim(t, f, materialized.Delivery.DeliveryID)
	got = dispatchExecutionRead(t, f.a, request)
	if got["binding"] != nil || got["worker_turn"].(map[string]any)["id"] != worker.ID {
		t.Fatal("claimed Turn must be observable even before attempt binding")
	}
	binding, err := f.a.db.BindAssignmentAttempt(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	got = dispatchExecutionRead(t, f.a, request)
	if got["binding"].(map[string]any)["attempt_id"] != binding.AttemptID || got["worker_turn"].(map[string]any)["state"] != "starting" {
		t.Fatalf("binding is identity, not running: %+v", got)
	}
}

func TestDispatchExecutionHTTPQueuedAndDeclined(t *testing.T) {
	f := dispatchRequestHTTPSetup(t)
	request := dispatchExecutionRegister(t, f)
	m, err := f.a.db.MaterializeDispatchRequest(context.Background(), request.ID, 1, time.Now().UTC())
	if err != nil || m.Delivery == nil {
		t.Fatal(err)
	}
	got := dispatchExecutionRead(t, f.a, request)
	if got["dispatch_status"] != "enqueued" || got["coordinator_turn"] != nil || got["decision"] != nil {
		t.Fatalf("coordinator queue is not a claimed execution: %+v", got)
	}
	coordinator, scope := dispatchExecutionClaim(t, f, m.Delivery.DeliveryID)
	decision, err := f.a.db.DecideDispatch(context.Background(), scope, store.CoordinatorDecisionInput{Kind: "decline", Reason: "PRIVATE decline"}, "decline")
	if err != nil {
		t.Fatal(err)
	}
	got = dispatchExecutionRead(t, f.a, request)
	if got["dispatch_status"] != "declined" || got["decision"].(map[string]any)["id"] != decision.Decision.ID || got["coordinator_turn"].(map[string]any)["id"] != coordinator.ID || got["assignment"] != nil || got["worker_turn"] != nil || got["binding"] != nil {
		t.Fatalf("decline invented worker execution: %+v", got)
	}
}

func TestDispatchExecutionHTTPDurableSafeReceiptsAndCancellation(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateRunning, domain.TurnStateCancelRequested, domain.TurnStateUnknown,
		domain.TurnStateNotStarted, domain.TurnStateSucceeded, domain.TurnStateFailed, domain.TurnStateAborted, domain.TurnStateTruncated} {
		t.Run(string(state), func(t *testing.T) {
			f := dispatchRequestHTTPSetup(t)
			request := dispatchExecutionRegister(t, f)
			_, decision := dispatchExecutionAssigned(t, f, request)
			m, err := f.a.db.MaterializeAssignment(context.Background(), decision.Assignment.ID, 1, time.Now().UTC())
			if err != nil || m.Delivery == nil {
				t.Fatal(err)
			}
			worker, scope := dispatchExecutionClaim(t, f, m.Delivery.DeliveryID)
			if _, err := f.a.db.BindAssignmentAttempt(context.Background(), scope); err != nil {
				t.Fatal(err)
			}
			claim := scope.Member.Claim
			if state != domain.TurnStateNotStarted {
				if err := f.a.db.RecordTurnEvent(context.Background(), claim, provider.Event{Kind: provider.AcceptedEvent, NativeSessionID: "native-worker"}); err != nil {
					t.Fatal(err)
				}
			}
			cancel := state == domain.TurnStateCancelRequested || state == domain.TurnStateUnknown || state == domain.TurnStateAborted
			if cancel {
				if _, err := f.a.db.RequestTurnCancel(context.Background(), request.GroupID, worker.ID); err != nil {
					t.Fatal(err)
				}
			}
			hasReceipt := state != domain.TurnStateRunning && state != domain.TurnStateCancelRequested
			if hasReceipt {
				receipt := provider.ExecutionReceipt{Started: true, Accepted: true, Outcome: provider.Outcome(state), Terminal: provider.Outcome(state),
					NativeSessionID: "native-worker", DirectChildReaped: true, OutputComplete: true, ExitCode: 0, LogsTruncated: true,
					StdoutPath: "/PRIVATE/stdout", StderrPath: "/PRIVATE/stderr", Reason: "PRIVATE reason prompt token"}
				if state == domain.TurnStateUnknown {
					receipt.Terminal, receipt.DeliveryFailed, receipt.OutputComplete, receipt.DirectChildReaped, receipt.ExitCode = provider.OutcomeSucceeded, true, false, false, -1
				} else if state == domain.TurnStateNotStarted {
					receipt = provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}
				} else if state == domain.TurnStateFailed {
					receipt.ExitCode = 2
				}
				if _, err := f.a.db.ApplyTurnReceipt(context.Background(), claim, receipt); err != nil {
					t.Fatal(err)
				}
			}
			raw := messageSQL(t, f.a)
			before := coordinationHTTPSnapshot(t, raw)
			got := dispatchExecutionRead(t, f.a, request)
			turn := got["worker_turn"].(map[string]any)
			if turn["id"] != worker.ID || turn["state"] != string(state) || turn["cancel_requested"] != cancel || (turn["receipt"] != nil) != hasReceipt {
				t.Fatalf("lost actual state/absence/cancel intent: %+v", turn)
			}
			if hasReceipt {
				receipt := turn["receipt"].(map[string]any)
				fields := strings.Fields("started accepted outcome terminal delivery_failed native_session_id direct_child_reaped output_complete exit_code logs_truncated")
				if len(receipt) != len(fields) || receipt["outcome"] != string(state) {
					t.Fatalf("unsafe receipt or guessed outcome: %+v", receipt)
				}
				for _, field := range fields {
					if _, ok := receipt[field]; !ok {
						t.Fatalf("missing safe field %s", field)
					}
				}
				if state == domain.TurnStateUnknown && (receipt["terminal"] != "succeeded" || receipt["output_complete"] != false || receipt["direct_child_reaped"] != false) {
					t.Fatalf("unknown native terminal is not success: %+v", receipt)
				}
			}
			encoded, _ := json.Marshal(got)
			for _, secret := range []string{"PRIVATE", "reason", "path", "prompt", "token", "claim_token", "receipt_json", "candidate_text"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("unsafe field escaped: %s", secret)
				}
			}
			coordinationHTTPUnchanged(t, raw, before)
			if err := f.a.db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openDispatchRequestAPI(t, f.a.path)
			if after := dispatchExecutionRead(t, reopened, request); !reflect.DeepEqual(got, after) {
				t.Fatalf("reopen lost durable evidence: before=%+v after=%+v", got, after)
			}
			coordinationHTTPUnchanged(t, raw, before)
		})
	}
}

func TestDispatchExecutionHTTPTransportSecurityAndOptIn(t *testing.T) {
	f := dispatchRequestHTTPSetup(t)
	request := dispatchExecutionRegister(t, f)
	path := dispatchExecutionPath(f.group.ID, request.TaskID, request.ID)
	raw := messageSQL(t, f.a)
	before := coordinationHTTPSnapshot(t, raw)
	for _, query := range []string{"actor=user", "group_id=foreign", "task_id=foreign", "turn_id=foreign", "%zz"} {
		dispatchRequestHTTPError(t, f.a.request("GET", path+"?"+query, ""), 400, "invalid_input")
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
		w := f.a.request(method, path, "")
		dispatchRequestHTTPError(t, w, 405, "method_not_allowed")
		if w.Header().Get("Allow") != "GET" {
			t.Fatalf("unexpected Allow: %v", w.Header())
		}
	}
	r := httptest.NewRequest("GET", "http://"+DefaultAddress+path, nil)
	r.Host = "evil.invalid"
	dispatchRequestHTTPError(t, dispatchRequestHTTPServe(f.a, r), 403, "invalid_host")
	// Preserve the transport's read-only origin semantics; no cross-origin CORS grant.
	for _, origin := range []string{"http://" + DefaultAddress, "http://evil.invalid", "null"} {
		r := httptest.NewRequest("GET", "http://"+DefaultAddress+path, nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		w := dispatchRequestHTTPServe(f.a, r)
		decoded[map[string]any](t, w, 200)
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("execution read must not add cross-origin access")
		}
	}
	a := openGroupAPI(t, "")
	w := a.request("GET", path, "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	coordinationHTTPUnchanged(t, raw, before)
}
