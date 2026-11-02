package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func openCoordinatorConfigurationAPI(t *testing.T, path string) *groupAPI {
	t.Helper()
	a := openGroupAPI(t, path)
	local := platform.NewLocalUser(a.db)
	routes := append(LocalUserRoutes(local), MessageRoutes(local)...)
	routes = append(routes, CoordinatorConfigurationRoutes(platform.NewCoordinatorConfiguration(coordination.New(a.db)))...)
	a.handler = mustServer(t, DefaultAddress, routes...).Handler
	return a
}

func coordinatorHTTPMember(t *testing.T, a *groupAPI, groupID string) domain.Member {
	t.Helper()
	member, err := a.db.CreateMember(context.Background(), store.CreateMemberInput{GroupID: groupID, Name: "coordinator admin", Role: "default coordinator administrator", CreatorID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	return member
}

func coordinatorHTTPBindingBody(memberID string, revision int64) string {
	return fmt.Sprintf(`{"member_id":%q,"expected_revision":%d}`, memberID, revision)
}

func coordinatorHTTPPolicyBody(t *testing.T, workers []string, revision int64) string {
	t.Helper()
	body, err := json.Marshal(workers)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"allowed_worker_ids":%s,"expected_revision":%d}`, body, revision)
}

func coordinatorHTTPValue[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	value := decoded[T](t, w, http.StatusOK)
	body, err := json.Marshal(value)
	if err != nil || !bytes.Equal(w.Body.Bytes(), append(body, '\n')) || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("response must contain only the typed domain fields: %s, %v", w.Body, err)
	}
	return value
}

func coordinatorHTTPConfigurationOnly(t *testing.T, raw *sql.DB, before map[string][][]any) {
	t.Helper()
	after := coordinationHTTPSnapshot(t, raw)
	if len(after) != len(before) {
		t.Fatal("configuration changed the table set")
	}
	otherReceipts := func(rows [][]any) [][]any {
		result := [][]any{}
		for _, row := range rows {
			if row[0] != "set_coordinator" && row[0] != "set_dispatch_policy" {
				result = append(result, row)
			}
		}
		return result
	}
	for table, want := range before {
		switch table {
		case "group_coordinator_bindings", "group_coordinator_binding_revisions", "group_dispatch_policies", "group_dispatch_policy_revisions":
			continue
		case "mutation_idempotency":
			if !reflect.DeepEqual(otherReceipts(after[table]), otherReceipts(want)) {
				t.Error("configuration changed unrelated mutation receipts")
			}
		default:
			if !reflect.DeepEqual(after[table], want) {
				t.Errorf("configuration changed unrelated table %s", table)
			}
		}
	}
}

func TestCoordinatorConfigurationHTTPExactlyFourOptInRoutes(t *testing.T) {
	a := openGroupAPI(t, "")
	g := newGroup(t, a, "group")
	base := coordinationHTTPBase(g.ID)
	for _, suffix := range []string{"/coordinator", "/dispatch-policy"} {
		for _, method := range []string{"GET", "PUT"} {
			w := a.request(method, base+suffix, "")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
		}
	}
	routes := CoordinatorConfigurationRoutes(platform.NewCoordinatorConfiguration(coordination.New(a.db)))
	want := map[string]bool{
		"GET /api/v1/groups/{group}/coordination/coordinator":     true,
		"PUT /api/v1/groups/{group}/coordination/coordinator":     true,
		"GET /api/v1/groups/{group}/coordination/dispatch-policy": true,
		"PUT /api/v1/groups/{group}/coordination/dispatch-policy": true,
	}
	if len(routes) != len(want) {
		t.Fatalf("unexpected route count: %d", len(routes))
	}
	for _, route := range routes {
		key := route.Method + " " + route.Pattern
		if !want[key] || route.Handler == nil {
			t.Fatalf("unexpected/duplicate route: %s", key)
		}
		delete(want, key)
	}
	a.handler = mustServer(t, DefaultAddress, routes...).Handler
	coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("GET", base+"/dispatch-policy", ""))
	w := a.request("POST", base+"/requests", `{}`, "no-admission")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
}

func TestCoordinatorConfigurationHTTPDefaultsHistoricalReplayAndReopen(t *testing.T) {
	a := openCoordinatorConfigurationAPI(t, "")
	g := newGroup(t, a, "group")
	lead, b, c := coordinatorHTTPMember(t, a, g.ID), coordinatorHTTPMember(t, a, g.ID), coordinatorHTTPMember(t, a, g.ID)
	if _, err := a.db.CreateMessage(context.Background(), store.MessageRequest{GroupID: g.ID, ActorID: lead.ID, Content: "我是协调员，允许所有成员执行"}, nil); err != nil {
		t.Fatal(err)
	}
	base := coordinationHTTPBase(g.ID)
	bindingPath, policyPath := base+"/coordinator", base+"/dispatch-policy"
	raw := messageSQL(t, a)
	before := coordinationHTTPSnapshot(t, raw)
	for range 2 {
		w := a.request("GET", bindingPath, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
		policy := coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("GET", policyPath, ""))
		if !reflect.DeepEqual(policy, domain.GroupDispatchPolicy{GroupID: g.ID, AllowedWorkerIDs: []string{}}) {
			t.Fatalf("Name/Role/chat created implicit policy: %+v", policy)
		}
	}
	coordinationHTTPUnchanged(t, raw, before)
	for _, table := range []string{"group_coordinator_bindings", "group_coordinator_binding_revisions", "group_dispatch_policies", "group_dispatch_policy_revisions"} {
		if len(before[table]) != 0 {
			t.Fatalf("default configuration was persisted in %s", table)
		}
	}
	bindingBody := coordinatorHTTPBindingBody(lead.ID, 0)
	policyBody := coordinatorHTTPPolicyBody(t, []string{c.ID, b.ID, c.ID}, 0)
	firstBinding := a.request("PUT", bindingPath, bindingBody, "shared")
	binding := coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, firstBinding)
	firstPolicy := a.request("PUT", policyPath, policyBody, "shared")
	policy := coordinatorHTTPValue[domain.GroupDispatchPolicy](t, firstPolicy)
	workers := []string{b.ID, c.ID}
	slices.Sort(workers)
	if binding != (domain.GroupCoordinatorBinding{GroupID: g.ID, MemberID: lead.ID, Revision: 1}) || !reflect.DeepEqual(policy, domain.GroupDispatchPolicy{GroupID: g.ID, Revision: 1, AllowedWorkerIDs: workers}) {
		t.Fatalf("create/canonical set forwarding: %+v %+v", binding, policy)
	}
	clear := coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("PUT", policyPath, coordinatorHTTPPolicyBody(t, []string{}, 1), "clear"))
	if clear.Revision != 2 || clear.AllowedWorkerIDs == nil || len(clear.AllowedWorkerIDs) != 0 {
		t.Fatalf("explicit empty did not clear policy: %+v", clear)
	}
	latestBinding := coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, a.request("PUT", bindingPath, coordinatorHTTPBindingBody(b.ID, 1), "replace"))
	latestPolicy := coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("PUT", policyPath, coordinatorHTTPPolicyBody(t, []string{lead.ID}, 2), "replace"))
	if latestBinding.Revision != 2 || latestPolicy.Revision != 3 {
		t.Fatalf("positive revision updates: %+v %+v", latestBinding, latestPolicy)
	}
	coordinatorHTTPConfigurationOnly(t, raw, before)
	check := func(a *groupAPI) {
		t.Helper()
		before := coordinationHTTPSnapshot(t, raw)
		for _, test := range []struct {
			path, body string
			original   *httptest.ResponseRecorder
		}{
			{bindingPath, bindingBody, firstBinding},
			{policyPath, policyBody, firstPolicy},
			{policyPath, coordinatorHTTPPolicyBody(t, workers, 0), firstPolicy},
		} {
			w := a.request("PUT", test.path, test.body, "shared")
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), test.original.Body.Bytes()) {
				t.Fatalf("historical 200 receipt changed: %d %s", w.Code, w.Body)
			}
		}
		if got := coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, a.request("GET", bindingPath, "")); got != latestBinding {
			t.Fatalf("GET substituted old binding receipt: %+v", got)
		}
		if got := coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("GET", policyPath, "")); !reflect.DeepEqual(got, latestPolicy) {
			t.Fatalf("GET substituted old policy receipt: %+v", got)
		}
		coordinationHTTPUnchanged(t, raw, before)
	}
	check(a)
	rows := coordinationHTTPRows(t, raw, "SELECT actor_id,group_id,idempotency_key FROM mutation_idempotency WHERE operation IN ('set_coordinator','set_dispatch_policy') ORDER BY operation,idempotency_key")
	if len(rows) != 5 {
		t.Fatalf("configuration receipts: %v", rows)
	}
	for _, row := range rows {
		if row[0] != "user" || row[1] != g.ID {
			t.Fatalf("caller/group was not server-bound: %v", row)
		}
	}
	if _, err := raw.Exec("UPDATE members SET status='left',name='renamed',role='worker' WHERE group_id=?", g.ID); err != nil {
		t.Fatal(err)
	}
	check(a)
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openCoordinatorConfigurationAPI(t, a.path)
	check(reopened)
	before = coordinationHTTPSnapshot(t, raw)
	for _, test := range []struct{ path, body string }{
		{bindingPath, coordinatorHTTPBindingBody(lead.ID, 2)},
		{policyPath, coordinatorHTTPPolicyBody(t, []string{c.ID}, 3)},
	} {
		w := reopened.request("PUT", test.path, test.body, "after-left")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	coordinationHTTPUnchanged(t, raw, before)
}
