package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func openDispatchRequestAPI(t *testing.T, path string) *groupAPI {
	t.Helper()
	a := openGroupAPI(t, path)
	routes := LocalUserRoutes(platform.NewLocalUser(a.db))
	routes = append(routes, DispatchRequestRoutes(platform.NewDispatchRequests(coordination.New(a.db)))...)
	a.handler = mustServer(t, DefaultAddress, routes...).Handler
	return a
}

type dispatchRequestHTTPFixture struct {
	a            *groupAPI
	group        domain.Group
	lead, worker domain.Member
	admission    domain.AdmissionReceipt
	body, path   string
}

func dispatchRequestHTTPSetup(t *testing.T) dispatchRequestHTTPFixture {
	t.Helper()
	a := openDispatchRequestAPI(t, "")
	g := newGroup(t, a, "group")
	return dispatchRequestHTTPConfigure(t, a, g)
}

func dispatchRequestHTTPConfigure(t *testing.T, a *groupAPI, g domain.Group) dispatchRequestHTTPFixture {
	t.Helper()
	ctx := context.Background()
	lead, worker := coordinatorHTTPMember(t, a, g.ID), coordinatorHTTPMember(t, a, g.ID)
	engine := coordination.New(a.db)
	if _, err := engine.SetCoordinator(ctx, g.ID, lead.ID, 0, "config"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SetDispatchPolicy(ctx, g.ID, []string{worker.ID}, 0, "config"); err != nil {
		t.Fatal(err)
	}
	admission, err := engine.RequestWork(ctx, g.ID, coordinationHTTPSource(t, a, g.ID, " exact\ntopic\x00 "), "admission")
	if err != nil {
		t.Fatal(err)
	}
	return dispatchRequestHTTPFixture{a, g, lead, worker, admission,
		dispatchRequestHTTPBody(1, 1, " 请协调\x00此任务 \r\n "), dispatchRequestHTTPPostPath(g.ID, admission.Task.ID)}
}

func dispatchRequestHTTPBody(revision, generation int64, reason string) string {
	body, err := json.Marshal(map[string]any{"expected_task_revision": revision, "expected_authority_generation": generation, "reason": reason})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func dispatchRequestHTTPPostPath(groupID, taskID string) string {
	return "/api/v1/groups/" + url.PathEscape(groupID) + "/tasks/" + url.PathEscape(taskID) + "/dispatch-requests"
}

func dispatchRequestHTTPGetPath(groupID, requestID string) string {
	return "/api/v1/groups/" + url.PathEscape(groupID) + "/coordination/dispatch-requests/" + url.PathEscape(requestID)
}

func dispatchRequestHTTPValue[T any](t *testing.T, w *httptest.ResponseRecorder, status int) T {
	t.Helper()
	value := decoded[T](t, w, status)
	body, err := json.Marshal(value)
	if err != nil || !bytes.Equal(w.Body.Bytes(), append(body, '\n')) || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("response must be the unmodified domain value: %s, %v", w.Body, err)
	}
	return value
}

func dispatchRequestHTTPError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), status, code)
	message := map[int]string{404: "记录不存在或不可访问", 409: "分派请求与当前任务、授权或协调配置冲突", 500: "内部服务错误"}[status]
	if message != "" {
		var envelope struct{ Error struct{ Message string } }
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Error.Message != message {
			t.Fatalf("error disclosed details or blamed the key: %s", w.Body)
		}
	}
}

func dispatchRequestHTTPAdded(t *testing.T, raw *sql.DB, before map[string][][]any, count int) {
	t.Helper()
	after := coordinationHTTPSnapshot(t, raw)
	if len(before) != len(after) {
		t.Fatal("dispatch changed the table set")
	}
	for table, rows := range before {
		switch table {
		case "coordination_dispatch_requests", "coordination_dispatch_state", "mutation_idempotency":
			if len(after[table]) != len(rows)+count {
				t.Errorf("%s must gain exactly %d rows", table, count)
			}
			for _, row := range rows {
				found := false
				for _, current := range after[table] {
					found = found || reflect.DeepEqual(row, current)
				}
				if !found {
					t.Errorf("dispatch changed an existing %s row", table)
				}
			}
		default:
			if !reflect.DeepEqual(rows, after[table]) {
				t.Errorf("dispatch changed unrelated table %s", table)
			}
		}
	}
}

func TestDispatchRequestHTTPRegistrationAndView(t *testing.T) {
	t.Run("POST", func(t *testing.T) {
		f := dispatchRequestHTTPSetup(t)
		ctx := context.Background()
		engine := coordination.New(f.a.db)
		if _, err := engine.SetCoordinator(ctx, f.group.ID, f.lead.ID, 1, "binding-2"); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.SetDispatchPolicy(ctx, f.group.ID, []string{f.worker.ID}, 1, "policy-2"); err != nil {
			t.Fatal(err)
		}
		raw := messageSQL(t, f.a)
		if _, err := raw.Exec("UPDATE task_coordination SET authority_generation=7 WHERE task_id=?", f.admission.Task.ID); err != nil {
			t.Fatal(err)
		}
		before := coordinationHTTPSnapshot(t, raw)
		reason, key := " 原因\t\x00\r\n ", "!~"+strings.Repeat("X", 126)
		request := dispatchRequestHTTPValue[domain.DispatchRequest](t, f.a.request("POST", f.path, dispatchRequestHTTPBody(1, 7, reason), key), 201)
		want := domain.DispatchRequest{ID: request.ID, GroupID: f.group.ID, TopicID: f.admission.Mandate.TopicID, TaskID: f.admission.Task.ID,
			TaskRevision: 1, AuthorityGeneration: 7, MandateID: f.admission.Mandate.ID, CoordinatorMemberID: f.lead.ID,
			BindingRevision: 2, PolicyRevision: 2, RequestedBy: "user", Reason: reason, CreatedAt: request.CreatedAt}
		if request != want || request.ID == "" || request.CreatedAt.IsZero() || request.CreatedAt.Location() != time.UTC {
			t.Fatalf("incorrect immutable registration receipt: %+v; want %+v", request, want)
		}
		view, err := f.a.db.GetDispatchRequest(ctx, f.group.ID, request.ID)
		if err != nil || view.Request != request || !reflect.DeepEqual(view.State, domain.DispatchState{RequestID: request.ID, Generation: 1, Status: domain.DispatchStatusPending, NextAttemptAt: request.CreatedAt}) {
			t.Fatalf("registration must not deliver or execute: %+v, %v", view, err)
		}
		dispatchRequestHTTPAdded(t, raw, before, 1)
		var operation, actor, groupID, savedKey, recordID string
		if err := raw.QueryRow("SELECT operation,actor_id,group_id,idempotency_key,record_id FROM mutation_idempotency WHERE operation='request_dispatch'").Scan(&operation, &actor, &groupID, &savedKey, &recordID); err != nil || operation != "request_dispatch" || actor != "user" || groupID != f.group.ID || savedKey != key || recordID != request.ID {
			t.Fatalf("server-bound receipt/key changed: %q %q %q %q %q, %v", operation, actor, groupID, savedKey, recordID, err)
		}
	})
	t.Run("GET", func(t *testing.T) {
		f := dispatchRequestHTTPSetup(t)
		request, err := f.a.db.RequestDispatch(context.Background(), f.group.ID, f.admission.Task.ID, 1, 1, "安排", "seed")
		if err != nil {
			t.Fatal(err)
		}
		raw := messageSQL(t, f.a)
		before := coordinationHTTPSnapshot(t, raw)
		got := dispatchRequestHTTPValue[domain.DispatchView](t, f.a.request("GET", dispatchRequestHTTPGetPath(f.group.ID, request.ID), ""), 200)
		want, err := f.a.db.GetDispatchRequest(context.Background(), f.group.ID, request.ID)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("GET substituted a guessed view: %+v, %v; want %+v", got, err, want)
		}
		coordinationHTTPUnchanged(t, raw, before)
	})
}

func TestDispatchRequestHTTPImmutableReplayCurrentStateAndReopen(t *testing.T) {
	f := dispatchRequestHTTPSetup(t)
	ctx := context.Background()
	first := f.a.request("POST", f.path, f.body, "history")
	original := dispatchRequestHTTPValue[domain.DispatchRequest](t, first, 201)
	raw := messageSQL(t, f.a)
	check := func(a *groupAPI, status domain.DispatchStatus, generation int64) {
		t.Helper()
		before := coordinationHTTPSnapshot(t, raw)
		reasonJSON, err := json.Marshal(original.Reason)
		if err != nil {
			t.Fatal(err)
		}
		semantic := fmt.Sprintf(" {\n\"REASON\":%s,\"EXPECTED_TASK_REVISION\":1,\"EXPECTED_AUTHORITY_GENERATION\":1}\n", reasonJSON)
		for _, body := range []string{f.body, semantic} {
			w := a.request("POST", f.path, body, "history")
			if w.Code != 201 || !bytes.Equal(w.Body.Bytes(), first.Body.Bytes()) {
				t.Fatalf("same URL/key/semantic payload revalidated or changed history: %d %s", w.Code, w.Body)
			}
		}
		view := dispatchRequestHTTPValue[domain.DispatchView](t, a.request("GET", dispatchRequestHTTPGetPath(f.group.ID, original.ID), ""), 200)
		if view.Request != original || view.State.Status != status || view.State.Generation != generation || view.State.Delivery != nil {
			t.Fatalf("current state mixed with immutable authority/execution: %+v", view)
		}
		if status == domain.DispatchStatusHeld && view.State.HeldReason != domain.DispatchHeldNoEligibleWorker {
			t.Fatalf("GET lost structured held reason: %+v", view)
		}
		for _, body := range []string{dispatchRequestHTTPBody(1, 1, original.Reason+" "), dispatchRequestHTTPBody(2, 1, original.Reason), dispatchRequestHTTPBody(1, 2, original.Reason)} {
			dispatchRequestHTTPError(t, a.request("POST", f.path, body, "history"), 409, "conflict")
		}
		coordinationHTTPUnchanged(t, raw, before)
	}
	check(f.a, domain.DispatchStatusPending, 1)
	attempt, err := f.a.db.ActivateTaskAttempt(ctx, store.ActivateTaskAttemptInput{GroupID: f.group.ID, TaskID: f.admission.Task.ID, ActorID: "user", ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	check(f.a, domain.DispatchStatusPending, 1)
	submission, err := f.a.db.SubmitTask(ctx, store.SubmitTaskInput{GroupID: f.group.ID, TaskID: f.admission.Task.ID, AttemptID: attempt.ID, ActorID: "user", ExpectedRevision: 1, Summary: "result"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.db.ReviewTask(ctx, store.ReviewTaskInput{GroupID: f.group.ID, TaskID: f.admission.Task.ID, SubmissionID: submission.ID, ActorID: "user", ExpectedRevision: 1, Outcome: domain.ReviewOutcomeRejected, Reason: "revise"}); err != nil {
		t.Fatal(err)
	}
	engine := coordination.New(f.a.db)
	next := coordinatorHTTPMember(t, f.a, f.group.ID)
	if _, err := engine.SetCoordinator(ctx, f.group.ID, next.ID, 1, "replace"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SetDispatchPolicy(ctx, f.group.ID, nil, 1, "clear"); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE members SET status='left',name='renamed',role='worker' WHERE group_id=?", f.group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE task_coordination SET authority_generation=2 WHERE task_id=?", f.admission.Task.ID); err != nil {
		t.Fatal(err)
	}
	var currentGeneration, departedMembers int64
	if err := raw.QueryRow("SELECT authority_generation,(SELECT count(*) FROM members WHERE group_id=? AND status='left') FROM task_coordination WHERE task_id=?", f.group.ID, f.admission.Task.ID).Scan(&currentGeneration, &departedMembers); err != nil || currentGeneration != 2 || departedMembers != 3 {
		t.Fatalf("fixture did not change authority/membership: generation=%d departed=%d, %v", currentGeneration, departedMembers, err)
	}
	currentTask, err := f.a.db.GetTask(ctx, f.group.ID, f.admission.Task.ID)
	if err != nil || currentTask.Revision != 2 || currentTask.Status != domain.TaskStatusOpen {
		t.Fatalf("fixture did not advance the Task revision: %+v, %v", currentTask, err)
	}
	if _, err := raw.Exec("UPDATE coordination_dispatch_state SET generation=2,status='held',held_reason='no_eligible_worker' WHERE request_id=?", original.ID); err != nil {
		t.Fatal(err)
	}
	check(f.a, domain.DispatchStatusHeld, 2)
	before := coordinationHTTPSnapshot(t, raw)
	dispatchRequestHTTPError(t, f.a.request("POST", f.path, f.body, "new-intent"), 409, "conflict")
	coordinationHTTPUnchanged(t, raw, before)
	if err := f.a.db.Close(); err != nil {
		t.Fatal(err)
	}
	check(openDispatchRequestAPI(t, f.a.path), domain.DispatchStatusHeld, 2)
}

func TestDispatchRequestHTTPPathScopeAndSameKeyIsolation(t *testing.T) {
	f := dispatchRequestHTTPSetup(t)
	other := dispatchRequestHTTPConfigure(t, f.a, newGroup(t, f.a, "other"))
	raw := messageSQL(t, f.a)
	before := coordinationHTTPSnapshot(t, raw)
	for _, path := range []string{dispatchRequestHTTPPostPath(other.group.ID, f.admission.Task.ID), dispatchRequestHTTPPostPath(f.group.ID, other.admission.Task.ID), dispatchRequestHTTPPostPath(f.group.ID, "missing-task"), dispatchRequestHTTPPostPath("missing-group", f.admission.Task.ID)} {
		dispatchRequestHTTPError(t, f.a.request("POST", path, f.body, "shared"), 404, "not_found")
	}
	coordinationHTTPUnchanged(t, raw, before)
	own := dispatchRequestHTTPValue[domain.DispatchRequest](t, f.a.request("POST", f.path, f.body, "shared"), 201)
	foreign := dispatchRequestHTTPValue[domain.DispatchRequest](t, f.a.request("POST", other.path, other.body, "shared"), 201)
	if own.ID == foreign.ID || own.GroupID != f.group.ID || own.TaskID != f.admission.Task.ID || foreign.GroupID != other.group.ID || foreign.TaskID != other.admission.Task.ID {
		t.Fatalf("path scope or key crossed groups: %+v %+v", own, foreign)
	}
	dispatchRequestHTTPAdded(t, raw, before, 2)
	// A missing current group must be checked before a conflicting historical receipt.
	if _, err := raw.Exec("INSERT INTO mutation_idempotency VALUES('request_dispatch','user','missing-group','shared',?,?)", []byte("private-payload"), own.ID); err != nil {
		t.Fatal(err)
	}
	// A stored receipt reference is still read through the exact group scope.
	if _, err := raw.Exec("INSERT INTO mutation_idempotency SELECT operation,actor_id,group_id,'foreign-history',payload,? FROM mutation_idempotency WHERE operation='request_dispatch' AND group_id=? AND idempotency_key='shared'", foreign.ID, f.group.ID); err != nil {
		t.Fatal(err)
	}
	before = coordinationHTTPSnapshot(t, raw)
	for _, scope := range [][2]string{{other.group.ID, own.ID}, {f.group.ID, foreign.ID}, {"missing-group", own.ID}, {f.group.ID, "missing-request"}, {f.group.ID, f.admission.Task.ID}} {
		dispatchRequestHTTPError(t, f.a.request("GET", dispatchRequestHTTPGetPath(scope[0], scope[1]), ""), 404, "not_found")
	}
	dispatchRequestHTTPError(t, f.a.request("POST", dispatchRequestHTTPPostPath("missing-group", own.TaskID), f.body, "shared"), 404, "not_found")
	dispatchRequestHTTPError(t, f.a.request("POST", f.path, f.body, "foreign-history"), 404, "not_found")
	dispatchRequestHTTPError(t, f.a.request("POST", dispatchRequestHTTPPostPath(f.group.ID, foreign.TaskID), f.body, "shared"), 409, "conflict")
	for _, bad := range []string{" " + f.group.ID, f.group.ID + "\n", f.group.ID + "\x00"} {
		dispatchRequestHTTPError(t, f.a.request("POST", dispatchRequestHTTPPostPath(bad, own.TaskID), f.body, "fresh"), 400, "invalid_input")
		dispatchRequestHTTPError(t, f.a.request("GET", dispatchRequestHTTPGetPath(bad, own.ID), ""), 400, "invalid_input")
	}
	for _, bad := range []string{" " + own.TaskID, own.TaskID + "\n", own.TaskID + "\x00"} {
		dispatchRequestHTTPError(t, f.a.request("POST", dispatchRequestHTTPPostPath(f.group.ID, bad), f.body, "fresh"), 400, "invalid_input")
	}
	dispatchRequestHTTPError(t, f.a.request("GET", dispatchRequestHTTPGetPath(f.group.ID, " "+own.ID), ""), 400, "invalid_input")
	for _, request := range []domain.DispatchRequest{own, foreign} {
		got := dispatchRequestHTTPValue[domain.DispatchView](t, f.a.request("GET", dispatchRequestHTTPGetPath(request.GroupID, request.ID), ""), 200)
		if got.Request != request {
			t.Fatalf("exact group GET changed receipt: %+v", got)
		}
	}
	coordinationHTTPUnchanged(t, raw, before)
}

func TestDispatchRequestHTTPOpaquePathsAndFullPositiveInt64(t *testing.T) {
	a := openDispatchRequestAPI(t, "")
	g := newGroup(t, a, "opaque group")
	raw := messageSQL(t, a)
	groupID, taskID := "group:opaque|群\tinside", "task:opaque|任务\tinside"
	if _, err := raw.Exec("UPDATE groups SET id=? WHERE id=?", groupID, g.ID); err != nil {
		t.Fatal(err)
	}
	g.ID = groupID
	f := dispatchRequestHTTPConfigure(t, a, g)
	ctx := context.Background()
	task, err := a.db.CreateTask(ctx, store.CreateTaskInput{GroupID: groupID, ActorID: "user", Title: "maximum revision", Acceptance: "exact int64"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE tasks SET id=?,revision=? WHERE id=?", taskID, int64(math.MaxInt64), task.ID); err != nil {
		t.Fatal(err)
	}
	input := coordinationHTTPSource(t, a, groupID, "opaque topic")
	input.Mode, input.NewTask = domain.RoutingDecisionLinkExisting, nil
	input.ExistingTask = &domain.ExistingTaskRequest{TaskID: taskID, ExpectedRevision: math.MaxInt64}
	admission, err := coordination.New(a.db).RequestWork(ctx, groupID, input, "max-admission")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE task_coordination SET authority_generation=? WHERE task_id=?", int64(math.MaxInt64), taskID); err != nil {
		t.Fatal(err)
	}
	before := coordinationHTTPSnapshot(t, raw)
	path, body := dispatchRequestHTTPPostPath(groupID, taskID), dispatchRequestHTTPBody(math.MaxInt64, math.MaxInt64, "exact maximum")
	request := dispatchRequestHTTPValue[domain.DispatchRequest](t, a.request("POST", path, body, "max"), 201)
	if request.TaskID != taskID || request.GroupID != groupID || request.TaskRevision != math.MaxInt64 || request.AuthorityGeneration != math.MaxInt64 || request.MandateID != admission.Mandate.ID || request.CoordinatorMemberID != f.lead.ID {
		t.Fatalf("opaque path or full int64 was normalized/narrowed: %+v", request)
	}
	dispatchRequestHTTPAdded(t, raw, before, 1)
	before = coordinationHTTPSnapshot(t, raw)
	if got := dispatchRequestHTTPValue[domain.DispatchRequest](t, a.request("POST", path, body, "max"), 201); got != request {
		t.Fatal("maximum int64 replay changed")
	}
	if got := dispatchRequestHTTPValue[domain.DispatchView](t, a.request("GET", dispatchRequestHTTPGetPath(groupID, request.ID), ""), 200); got.Request != request {
		t.Fatal("opaque exact GET changed")
	}
	coordinationHTTPUnchanged(t, raw, before)
}

func TestDispatchRequestHTTPUnregisteredRoutesRemainOptIn(t *testing.T) {
	a := openGroupAPI(t, "")
	g := newGroup(t, a, "group")
	raw := messageSQL(t, a)
	before := coordinationHTTPSnapshot(t, raw)
	for _, target := range []struct{ method, path string }{{"POST", dispatchRequestHTTPPostPath(g.ID, "task")}, {"GET", dispatchRequestHTTPGetPath(g.ID, "request")}} {
		w := a.request(target.method, target.path, dispatchRequestHTTPBody(1, 1, "安排"), "opt-in")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	routes := DispatchRequestRoutes(platform.NewDispatchRequests(coordination.New(a.db)))
	want := map[string]bool{
		"POST /api/v1/groups/{group}/tasks/{task}/dispatch-requests":          true,
		"GET /api/v1/groups/{group}/coordination/dispatch-requests/{request}": true,
	}
	if len(routes) != len(want) {
		t.Fatalf("expected exactly two opt-in routes, got %d", len(routes))
	}
	for _, route := range routes {
		key := route.Method + " " + route.Pattern
		if !want[key] || route.Handler == nil {
			t.Fatalf("unexpected or duplicate route: %s", key)
		}
		delete(want, key)
	}
	coordinationHTTPUnchanged(t, raw, before)
}
