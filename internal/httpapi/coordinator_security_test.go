package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
)

func TestCoordinatorConfigurationHTTPStrictBodiesKeysQueriesAndMethods(t *testing.T) {
	a := openCoordinatorConfigurationAPI(t, "")
	g := newGroup(t, a, "group")
	lead, worker := coordinatorHTTPMember(t, a, g.ID), coordinatorHTTPMember(t, a, g.ID)
	raw := messageSQL(t, a)
	before := coordinationHTTPSnapshot(t, raw)
	for _, test := range []struct {
		suffix, body string
		invalidJSON  []string
		invalidInput []string
	}{
		{
			"/coordinator", coordinatorHTTPBindingBody(lead.ID, 0),
			[]string{`{"member_id":42,"expected_revision":0}`, `{"member_id":[],"expected_revision":0}`, `{"member_id":{"actor_id":"user","actor_id":"forged"},"expected_revision":0}`, `{"member_id":"a","member_id":"b","expected_revision":0}`},
			[]string{`{"expected_revision":0}`, `{"member_id":null,"expected_revision":0}`, coordinatorHTTPBindingBody(" "+lead.ID, 0)},
		},
		{
			"/dispatch-policy", coordinatorHTTPPolicyBody(t, []string{worker.ID}, 0),
			[]string{`{"allowed_worker_ids":"worker","expected_revision":0}`, `{"allowed_worker_ids":[42],"expected_revision":0}`, `{"allowed_worker_ids":[{"claim":"a","claim":"b"}],"expected_revision":0}`, `{"allowed_worker_ids":[],"allowed_worker_ids":[],"expected_revision":0}`},
			[]string{`{"expected_revision":0}`, `{"allowed_worker_ids":null,"expected_revision":0}`, `{"allowed_worker_ids":[null],"expected_revision":0}`, coordinatorHTTPPolicyBody(t, []string{" " + worker.ID}, 0)},
		},
	} {
		t.Run(test.suffix, func(t *testing.T) {
			path := coordinationHTTPBase(g.ID) + test.suffix
			check := func(w *httptest.ResponseRecorder, status int, code string) {
				t.Helper()
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), status, code)
				coordinationHTTPUnchanged(t, raw, before)
			}
			invalidJSON := append(test.invalidJSON,
				"", `null`, `[]`, `{`, test.body+` {}`,
				strings.Replace(test.body, `"expected_revision":0`, `"expected_revision":"0"`, 1),
				strings.Replace(test.body, `"expected_revision":0`, `"expected_revision":0.5`, 1),
				strings.Replace(test.body, `"expected_revision":0`, `"expected_revision":[],"extra":{}`, 1),
				strings.Replace(test.body, `"expected_revision":0`, `"expected_revision":0,"Expected_Revision":0`, 1),
				"{\"member_id\":\"\xff\",\"expected_revision\":0}",
			)
			for _, body := range invalidJSON {
				check(a.request("PUT", path, body, "rejected"), 400, "invalid_json")
			}
			for _, field := range []string{"actor_id", "actor", "group_id", "group", "caller", "claim", "role", "Role", "scope", "grants"} {
				body := strings.TrimSuffix(test.body, "}") + fmt.Sprintf(`,%q:"forged"}`, field)
				check(a.request("PUT", path, body, "rejected"), 400, "invalid_json")
			}
			invalidInput := append(test.invalidInput,
				`{}`,
				strings.Replace(test.body, `,"expected_revision":0`, "", 1),
				strings.Replace(test.body, `"expected_revision":0`, `"expected_revision":null`, 1),
				strings.Replace(test.body, `"expected_revision":0`, `"expected_revision":-1`, 1),
			)
			for _, body := range invalidInput {
				check(a.request("PUT", path, body, "rejected"), 400, "invalid_input")
			}
			for _, keys := range [][]string{nil, {""}, {" "}, {"a", "a"}, {"a", "b"}, {"a,b"}, {"a, b"}, {strings.Repeat("x", 129)}, {"line\nkey"}, {"中文"}, {"\x7f"}} {
				check(a.request("PUT", path, test.body, keys...), 400, "invalid_input")
			}
			for _, method := range []string{"GET", "PUT"} {
				for _, query := range []string{"x=1", "actor_id=user", "expected_revision=0", "x=1&x=2", "topic_id=", "%zz", "x=1;y=2"} {
					r := httptest.NewRequest(method, "http://"+DefaultAddress+path+"?"+query, strings.NewReader(test.body))
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Idempotency-Key", "rejected")
					originalURL, originalQuery := r.URL, r.URL.RawQuery
					w := httptest.NewRecorder()
					a.handler.ServeHTTP(w, r)
					check(w, 400, "invalid_input")
					if r.URL != originalURL || r.URL.RawQuery != originalQuery {
						t.Fatal("route mutated caller URL/query")
					}
				}
			}
			for _, method := range []string{"POST", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
				w := a.request(method, path, test.body, "rejected")
				check(w, 405, "method_not_allowed")
				if w.Header().Get("Allow") != "GET, PUT" {
					t.Fatalf("unexpected allowed methods: %v", w.Header())
				}
			}
			for _, types := range [][]string{nil, {"text/plain"}, {"application/json; charset=latin-1"}, {"application/json", "application/json"}} {
				r := httptest.NewRequest("PUT", "http://"+DefaultAddress+path, strings.NewReader(test.body))
				r.Header.Set("Idempotency-Key", "rejected")
				for _, media := range types {
					r.Header.Add("Content-Type", media)
				}
				w := httptest.NewRecorder()
				a.handler.ServeHTTP(w, r)
				check(w, 415, "unsupported_media_type")
			}
			check(a.request("PUT", path, `{"oversized":"`+strings.Repeat("x", MaxJSONBodyBytes)+`"}`, "rejected"), 413, "body_too_large")
		})
	}
	// Every rejection above preserved all rows, including the reusable key.
	base := coordinationHTTPBase(g.ID)
	coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, a.request("PUT", base+"/coordinator", coordinatorHTTPBindingBody(lead.ID, 0), "rejected"))
	coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("PUT", base+"/dispatch-policy", coordinatorHTTPPolicyBody(t, []string{worker.ID}, 0), "rejected"))
	key := strings.Repeat("X", 128)
	coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, a.request("PUT", base+"/coordinator", coordinatorHTTPBindingBody(lead.ID, 1), key))
	coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("PUT", base+"/dispatch-policy", coordinatorHTTPPolicyBody(t, []string{worker.ID}, 1), key))
	rows := coordinationHTTPRows(t, raw, "SELECT idempotency_key FROM mutation_idempotency WHERE idempotency_key='"+key+"' ORDER BY operation")
	if len(rows) != 2 || rows[0][0] != key || rows[1][0] != key {
		t.Fatalf("valid key bytes changed: %v", rows)
	}
	coordinatorHTTPConfigurationOnly(t, raw, before)
}

func TestCoordinatorConfigurationHTTPNeutralConflictsAndReusableRejectedKeys(t *testing.T) {
	a := openCoordinatorConfigurationAPI(t, "")
	g := newGroup(t, a, "group")
	lead, worker, next := coordinatorHTTPMember(t, a, g.ID), coordinatorHTTPMember(t, a, g.ID), coordinatorHTTPMember(t, a, g.ID)
	base := coordinationHTTPBase(g.ID)
	bindingPath, policyPath := base+"/coordinator", base+"/dispatch-policy"
	coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, a.request("PUT", bindingPath, coordinatorHTTPBindingBody(lead.ID, 0), "original"))
	coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("PUT", policyPath, coordinatorHTTPPolicyBody(t, []string{worker.ID}, 0), "original"))
	raw := messageSQL(t, a)
	before := coordinationHTTPSnapshot(t, raw)
	for _, test := range []struct{ path, body, key string }{
		{bindingPath, coordinatorHTTPBindingBody(lead.ID, 0), "stale"},
		{policyPath, coordinatorHTTPPolicyBody(t, []string{worker.ID}, 0), "stale"},
		{bindingPath, coordinatorHTTPBindingBody(worker.ID, 1), "overlap-coordinator"},
		{policyPath, coordinatorHTTPPolicyBody(t, []string{lead.ID}, 1), "overlap-policy"},
		{bindingPath, coordinatorHTTPBindingBody(next.ID, 0), "original"},
		{policyPath, coordinatorHTTPPolicyBody(t, []string{}, 0), "original"},
	} {
		w := a.request("PUT", test.path, test.body, test.key)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
		var envelope struct {
			Error struct{ Message string }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Error.Message != "协调配置与当前版本或成员配置冲突" {
			t.Fatalf("conflict must not blame the key or recommend replacement: %s", w.Body)
		}
		coordinationHTTPUnchanged(t, raw, before)
	}
	coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, a.request("PUT", bindingPath, coordinatorHTTPBindingBody(lead.ID, 1), "stale"))
	coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("PUT", policyPath, coordinatorHTTPPolicyBody(t, []string{worker.ID}, 1), "stale"))
	coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, a.request("PUT", bindingPath, coordinatorHTTPBindingBody(next.ID, 2), "overlap-coordinator"))
	coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("PUT", policyPath, coordinatorHTTPPolicyBody(t, []string{}, 2), "overlap-policy"))
	coordinatorHTTPConfigurationOnly(t, raw, before)
}

func TestCoordinatorConfigurationHTTPNotFoundAndCrossGroupIsolation(t *testing.T) {
	a := openCoordinatorConfigurationAPI(t, "")
	g, other := newGroup(t, a, "group"), newGroup(t, a, "other")
	lead, worker := coordinatorHTTPMember(t, a, g.ID), coordinatorHTTPMember(t, a, g.ID)
	foreign, left := coordinatorHTTPMember(t, a, other.ID), coordinatorHTTPMember(t, a, g.ID)
	raw := messageSQL(t, a)
	if _, err := raw.Exec("UPDATE members SET status='left' WHERE id=?", left.ID); err != nil {
		t.Fatal(err)
	}
	before := coordinationHTTPSnapshot(t, raw)
	for _, groupID := range []string{g.ID, other.ID, "missing"} {
		for _, suffix := range []string{"/coordinator", "/dispatch-policy"} {
			path := coordinationHTTPBase(groupID) + suffix
			if suffix == "/coordinator" || groupID == "missing" {
				w := a.request("GET", path, "")
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
			}
			ids := []string{foreign.ID, left.ID, "missing"}
			if groupID != g.ID {
				ids = []string{lead.ID}
			}
			for _, id := range ids {
				body := coordinatorHTTPBindingBody(id, 0)
				if suffix == "/dispatch-policy" {
					body = coordinatorHTTPPolicyBody(t, []string{id}, 0)
				}
				w := a.request("PUT", path, body, "rejected")
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
				if strings.Contains(w.Body.String(), id) || strings.Contains(w.Body.String(), groupID) {
					t.Fatalf("not-found response leaked identity: %s", w.Body)
				}
			}
		}
	}
	coordinationHTTPUnchanged(t, raw, before)
	base := coordinationHTTPBase(g.ID)
	coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, a.request("PUT", base+"/coordinator", coordinatorHTTPBindingBody(lead.ID, 0), "rejected"))
	coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("PUT", base+"/dispatch-policy", coordinatorHTTPPolicyBody(t, []string{worker.ID}, 0), "rejected"))
	otherBase := coordinationHTTPBase(other.ID)
	before = coordinationHTTPSnapshot(t, raw)
	w := a.request("GET", otherBase+"/coordinator", "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	policy := coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("GET", otherBase+"/dispatch-policy", ""))
	if policy.GroupID != other.ID || policy.Revision != 0 || len(policy.AllowedWorkerIDs) != 0 {
		t.Fatalf("GET exposed another group's policy: %+v", policy)
	}
	coordinationHTTPUnchanged(t, raw, before)
	// The same exact key remains scoped to the URL group and operation.
	coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, a.request("PUT", otherBase+"/coordinator", coordinatorHTTPBindingBody(foreign.ID, 0), "rejected"))
	coordinatorHTTPValue[domain.GroupDispatchPolicy](t, a.request("PUT", otherBase+"/dispatch-policy", coordinatorHTTPPolicyBody(t, []string{}, 0), "rejected"))
}

func TestCoordinatorConfigurationHTTPHostOriginBoundary(t *testing.T) {
	a := openCoordinatorConfigurationAPI(t, "")
	g := newGroup(t, a, "group")
	lead, worker := coordinatorHTTPMember(t, a, g.ID), coordinatorHTTPMember(t, a, g.ID)
	raw := messageSQL(t, a)
	for _, test := range []struct{ suffix, body string }{
		{"/coordinator", coordinatorHTTPBindingBody(lead.ID, 0)},
		{"/dispatch-policy", coordinatorHTTPPolicyBody(t, []string{worker.ID}, 0)},
	} {
		path := coordinationHTTPBase(g.ID) + test.suffix
		before := coordinationHTTPSnapshot(t, raw)
		for _, boundary := range []struct{ host, origin, site, code string }{
			{"evil.invalid", "", "", "invalid_host"},
			{DefaultAddress, "http://evil.invalid", "cross-site", "invalid_origin"},
			{DefaultAddress, "null", "", "invalid_origin"},
			{DefaultAddress, "", "same-origin", "invalid_origin"},
			{DefaultAddress, "http://" + DefaultAddress, "cross-site", "invalid_origin"},
		} {
			for _, method := range []string{"GET", "PUT"} {
				if method == "GET" && boundary.code != "invalid_host" {
					continue
				}
				r := httptest.NewRequest(method, "http://"+DefaultAddress+path, strings.NewReader(test.body))
				r.Host = boundary.host
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Idempotency-Key", "boundary")
				r.Header.Set("X-Forwarded-Host", DefaultAddress)
				if boundary.origin != "" {
					r.Header.Set("Origin", boundary.origin)
				}
				if boundary.site != "" {
					r.Header.Set("Sec-Fetch-Site", boundary.site)
				}
				w := httptest.NewRecorder()
				a.handler.ServeHTTP(w, r)
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, boundary.code)
				coordinationHTTPUnchanged(t, raw, before)
			}
		}
		for _, browser := range []bool{true, false} {
			r := httptest.NewRequest("PUT", "http://"+DefaultAddress+path, strings.NewReader(test.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "boundary")
			if browser {
				r.Header.Set("Origin", "http://"+DefaultAddress)
				r.Header.Set("Sec-Fetch-Site", "same-origin")
			}
			w := httptest.NewRecorder()
			a.handler.ServeHTTP(w, r)
			if test.suffix == "/coordinator" {
				coordinatorHTTPValue[domain.GroupCoordinatorBinding](t, w)
			} else {
				coordinatorHTTPValue[domain.GroupDispatchPolicy](t, w)
			}
		}
	}
}

func TestCoordinatorConfigurationHTTPContextUnknownAndSafeStorageErrors(t *testing.T) {
	a := openCoordinatorConfigurationAPI(t, "")
	g := newGroup(t, a, "group")
	lead, worker := coordinatorHTTPMember(t, a, g.ID), coordinatorHTTPMember(t, a, g.ID)
	raw := messageSQL(t, a)
	for _, test := range []struct{ suffix, body, table string }{
		{"/coordinator", coordinatorHTTPBindingBody(lead.ID, 0), "group_coordinator_binding_revisions"},
		{"/dispatch-policy", coordinatorHTTPPolicyBody(t, []string{worker.ID}, 0), "group_dispatch_policy_revisions"},
	} {
		path := coordinationHTTPBase(g.ID) + test.suffix
		original := a.request("PUT", path, test.body, "original")
		if original.Code != 200 {
			t.Fatalf("fixture write: %d %s", original.Code, original.Body)
		}
		before := coordinationHTTPSnapshot(t, raw)
		for _, expired := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			status, code := 408, "request_canceled"
			if expired {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				status, code = 504, "deadline_exceeded"
			}
			cancel()
			for _, method := range []string{"GET", "PUT"} {
				r := httptest.NewRequest(method, "http://"+DefaultAddress+path, strings.NewReader(test.body)).WithContext(ctx)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Idempotency-Key", "original")
				w := httptest.NewRecorder()
				a.handler.ServeHTTP(w, r)
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), status, code)
				if !strings.Contains(w.Body.String(), "操作结果尚未确认") || !strings.Contains(w.Body.String(), "沿用原幂等键") {
					t.Fatalf("context error claimed definite outcome: %s", w.Body)
				}
			}
		}
		w := a.request("PUT", path, test.body, "original")
		if w.Code != 200 || w.Body.String() != original.Body.String() {
			t.Fatalf("original key/payload retry changed receipt: %d %s", w.Code, w.Body)
		}
		coordinationHTTPUnchanged(t, raw, before)
		if _, err := raw.Exec("CREATE TRIGGER coordinator_http_failure BEFORE INSERT ON " + test.table + " BEGIN SELECT RAISE(ABORT,'private SQL /secret/config.db'); END"); err != nil {
			t.Fatal(err)
		}
		body := strings.Replace(test.body, `"expected_revision":0`, `"expected_revision":1`, 1)
		w = a.request("PUT", path, body, "failed")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 500, "internal_error")
		for _, private := range []string{"private", "SQL", "/secret", test.table, "更换"} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatalf("unsafe internal error: %s", w.Body)
			}
		}
		coordinationHTTPUnchanged(t, raw, before)
		if _, err := raw.Exec("DROP TRIGGER coordinator_http_failure"); err != nil {
			t.Fatal(err)
		}
		if w := a.request("PUT", path, body, "failed"); w.Code != 200 {
			t.Fatalf("storage failure burned original key: %d %s", w.Code, w.Body)
		}
		coordinatorHTTPConfigurationOnly(t, raw, before)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/coordinator", "/dispatch-policy"} {
		body := coordinatorHTTPBindingBody(lead.ID, 0)
		if suffix == "/dispatch-policy" {
			body = coordinatorHTTPPolicyBody(t, []string{worker.ID}, 0)
		}
		for _, method := range []string{"GET", "PUT"} {
			w := a.request(method, coordinationHTTPBase(g.ID)+suffix, body, "original")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 500, "internal_error")
			for _, private := range []string{a.path, "sql", "closed", "SELECT", "group_", "更换"} {
				if strings.Contains(w.Body.String(), private) {
					t.Fatalf("closed storage leaked details: %s", w.Body)
				}
			}
		}
	}
}
