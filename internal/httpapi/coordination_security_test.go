package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func TestCoordinationHTTPRejectsBodiesAndRequiredKeysWithoutWrites(t *testing.T) {
	a := openCoordinationAPI(t, "")
	g := newGroup(t, a, "group")
	request := coordinationHTTPSource(t, a, g.ID, "")
	body := coordinationHTTPBody(t, request)
	path := coordinationHTTPBase(g.ID) + "/requests"
	raw := messageSQL(t, a)
	before := coordinationHTTPSnapshot(t, raw)
	for i, invalid := range []string{
		`null`, `[]`, `{`, body + ` {}`, `{"topic_id":42}`, `{"mode":"create_new","new_task":[]}`,
		strings.Replace(body, `"mode":"create_new"`, `"mode":"create_new","Mode":"create_new"`, 1),
		strings.Replace(body, `"title":" 目标 "`, `"title":" 目标 ","title":"again"`, 1),
		strings.Replace(body, `"dependency_ids":null`, `"dependency_ids":[42]`, 1),
	} {
		t.Run(fmt.Sprintf("json-%d", i), func(t *testing.T) {
			w := a.request("POST", path, invalid, "rejected")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
			coordinationHTTPUnchanged(t, raw, before)
		})
	}
	for _, field := range []string{"Actor", "actor_id", "Policy", "policy_revision", "Strength", "Sequence", "message_sequence", "Turn", "turn_id", "Scope", "grants", "caller", "group_id", "mandate"} {
		for _, nested := range []bool{false, true} {
			invalid := strings.TrimSuffix(body, "}") + fmt.Sprintf(`,%q:"forged"}`, field)
			if nested {
				invalid = strings.Replace(body, `"title":`, fmt.Sprintf(`%q:"forged","title":`, field), 1)
			}
			w := a.request("POST", path, invalid, "rejected")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
			coordinationHTTPUnchanged(t, raw, before)
		}
	}
	linkBody := fmt.Sprintf(`{"message_id":%q,"mode":"link_existing","reason":"explicit","existing_task":{"task_id":"task","expected_revision":1}}`, request.MessageID)
	for _, field := range []string{"Actor", "Policy", "grants", "Turn", "title", "description", "acceptance", "assignee_id", "dependency_ids"} {
		invalid := strings.Replace(linkBody, `"task_id":`, fmt.Sprintf(`%q:"forged","task_id":`, field), 1)
		w := a.request("POST", path, invalid, "rejected")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
		coordinationHTTPUnchanged(t, raw, before)
	}
	for _, keys := range [][]string{
		nil, {""}, {" "}, {"a", "a"}, {"a", "b"}, {"a,b"}, {"a, b"}, {strings.Repeat("x", 129)}, {"line\nkey"}, {"中文"},
	} {
		w := a.request("POST", path, body, keys...)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		coordinationHTTPUnchanged(t, raw, before)
	}
	for _, invalid := range []string{
		`{}`, strings.Replace(body, `"reason":" 用户明确开展此工作 "`, `"reason":" "`, 1),
		strings.Replace(body, `"mode":"create_new"`, `"mode":"attach"`, 1),
		strings.Replace(body, `"new_task":{`, `"existing_task":{"task_id":"task","expected_revision":1},"new_task":{`, 1),
		strings.Replace(body, `"acceptance":" 验收 "`, `"acceptance":" "`, 1),
	} {
		w := a.request("POST", path, invalid, "rejected")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		coordinationHTTPUnchanged(t, raw, before)
	}
	w := a.request("POST", path, `{"reason":"`+strings.Repeat("x", MaxJSONBodyBytes)+`"}`, "rejected")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 413, "body_too_large")
	coordinationHTTPUnchanged(t, raw, before)
	r := httptest.NewRequest("POST", "http://"+DefaultAddress+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Idempotency-Key", "rejected")
	w = httptest.NewRecorder()
	a.handler.ServeHTTP(w, r)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 415, "unsupported_media_type")
	coordinationHTTPUnchanged(t, raw, before)
	// The rejected key is reusable; the maximum valid visible-ASCII key also works.
	decoded[domain.AdmissionReceipt](t, a.request("POST", path, body, "rejected"), 201)
	next := coordinationHTTPSource(t, a, g.ID, "")
	decoded[domain.AdmissionReceipt](t, a.request("POST", path, coordinationHTTPBody(t, next), strings.Repeat("x", 128)), 201)
}

func TestCoordinationHTTPHostOriginAndMethodsPreserveState(t *testing.T) {
	a := openCoordinationAPI(t, "")
	g := newGroup(t, a, "group")
	request := coordinationHTTPSource(t, a, g.ID, "")
	body := coordinationHTTPBody(t, request)
	path := coordinationHTTPBase(g.ID) + "/requests"
	raw := messageSQL(t, a)
	before := coordinationHTTPSnapshot(t, raw)
	for _, test := range []struct {
		host, origin, site, code string
	}{
		{"evil.invalid", "", "", "invalid_host"},
		{DefaultAddress, "http://evil.invalid", "cross-site", "invalid_origin"},
		{DefaultAddress, "null", "", "invalid_origin"},
		{DefaultAddress, "", "same-origin", "invalid_origin"},
		{DefaultAddress, "http://" + DefaultAddress, "cross-site", "invalid_origin"},
		{DefaultAddress, "http://" + DefaultAddress + "/", "same-origin", "invalid_origin"},
	} {
		r := httptest.NewRequest("POST", "http://"+DefaultAddress+path, strings.NewReader(body))
		r.Host = test.host
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "boundary")
		r.Header.Set("X-Forwarded-Host", DefaultAddress)
		r.Header.Set("X-Forwarded-Proto", "http")
		if test.origin != "" {
			r.Header.Set("Origin", test.origin)
		}
		if test.site != "" {
			r.Header.Set("Sec-Fetch-Site", test.site)
		}
		w := httptest.NewRecorder()
		a.handler.ServeHTTP(w, r)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, test.code)
		coordinationHTTPUnchanged(t, raw, before)
	}
	// Both the real same-origin browser contract and Origin-free local software remain supported.
	var receipt domain.AdmissionReceipt
	for _, browser := range []bool{true, false} {
		r := httptest.NewRequest("POST", "http://"+DefaultAddress+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "boundary")
		if browser {
			r.Header.Set("Origin", "http://"+DefaultAddress)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}
		w := httptest.NewRecorder()
		a.handler.ServeHTTP(w, r)
		receipt = decoded[domain.AdmissionReceipt](t, w, 201)
	}
	before = coordinationHTTPSnapshot(t, raw)
	for _, target := range append(coordinationHTTPGetPaths(receipt), path) {
		for _, method := range []string{"HEAD", "OPTIONS", "PUT", "PATCH", "DELETE"} {
			w := a.request(method, target, body, "method")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 405, "method_not_allowed")
		}
	}
	w := a.request("GET", path, "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 405, "method_not_allowed")
	for _, target := range coordinationHTTPGetPaths(receipt) {
		w := a.request("POST", target, body, "method")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 405, "method_not_allowed")
	}
	// Browser-origin filtering on this server is a write boundary, not authentication.
	r := httptest.NewRequest("GET", "http://"+DefaultAddress+coordinationHTTPGetPaths(receipt)[2], nil)
	r.Header.Set("Origin", "http://evil.invalid")
	w = httptest.NewRecorder()
	a.handler.ServeHTTP(w, r)
	decoded[domain.AdmissionReceipt](t, w, 200)
	coordinationHTTPUnchanged(t, raw, before)
}

func TestCoordinationHTTPConflictsAreMeaningfulAndRollBack(t *testing.T) {
	a := openCoordinationAPI(t, "")
	g, other := newGroup(t, a, "group"), newGroup(t, a, "other")
	request := coordinationHTTPSource(t, a, g.ID, "")
	body := coordinationHTTPBody(t, request)
	path := coordinationHTTPBase(g.ID) + "/requests"
	decoded[domain.AdmissionReceipt](t, a.request("POST", path, body, "original"), 201)
	target, err := platform.NewLocalUser(a.db).CreateTask(context.Background(), g.ID, platform.TaskInput{Title: "manual", Acceptance: "criteria"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ActivateTaskAttempt(context.Background(), store.ActivateTaskAttemptInput{GroupID: g.ID, TaskID: target.ID, ActorID: "user", ExpectedRevision: target.Revision}); err != nil {
		t.Fatal(err)
	}
	link := coordinationHTTPSource(t, a, g.ID, "")
	link.Mode, link.NewTask, link.ExistingTask = domain.RoutingDecisionLinkExisting, nil, &domain.ExistingTaskRequest{TaskID: target.ID, ExpectedRevision: target.Revision}
	raw := messageSQL(t, a)
	before := coordinationHTTPSnapshot(t, raw)
	for _, test := range []struct{ body, key string }{
		{body, "different-key"}, {strings.Replace(body, " 用户明确开展此工作 ", "changed", 1), "original"}, {coordinationHTTPBody(t, link), "link"},
	} {
		w := a.request("POST", path, test.body, test.key)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
		if !strings.Contains(w.Body.String(), "工作请求与已有记录或任务状态冲突") || strings.Contains(w.Body.String(), "幂等键已用于不同") || strings.Contains(w.Body.String(), "更换") {
			t.Fatalf("misleading conflict advice: %s", w.Body)
		}
		coordinationHTTPUnchanged(t, raw, before)
	}
	for _, targetPath := range []string{coordinationHTTPBase(other.ID) + "/requests", "/api/v1/groups/missing/coordination/requests"} {
		w := a.request("POST", targetPath, body, "original")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	wrongTopic := request
	wrongTopic.TopicID = "foreign topic"
	w := a.request("POST", path, coordinationHTTPBody(t, wrongTopic), "original")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	coordinationHTTPUnchanged(t, raw, before)
}

func TestCoordinationHTTPErrorsSanitizedAndContextOutcomeUnknown(t *testing.T) {
	a := openCoordinationAPI(t, "")
	g := newGroup(t, a, "group")
	request := coordinationHTTPSource(t, a, g.ID, "")
	body := coordinationHTTPBody(t, request)
	path := coordinationHTTPBase(g.ID) + "/requests"
	receipt := decoded[domain.AdmissionReceipt](t, a.request("POST", path, body, "original"), 201)
	paths := append(coordinationHTTPGetPaths(receipt), path)
	raw := messageSQL(t, a)
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
		for _, target := range paths {
			method := "GET"
			if target == path {
				method = "POST"
			}
			r := httptest.NewRequest(method, "http://"+DefaultAddress+target, strings.NewReader(body)).WithContext(ctx)
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
	coordinationHTTPUnchanged(t, raw, before)
	fresh := coordinationHTTPSource(t, a, g.ID, "")
	before = coordinationHTTPSnapshot(t, raw)
	if _, err := raw.Exec(`CREATE TRIGGER coordination_http_fail BEFORE INSERT ON routing_decisions BEGIN SELECT RAISE(ABORT,'private SQL /secret/database.db'); END`); err != nil {
		t.Fatal(err)
	}
	w := a.request("POST", path, coordinationHTTPBody(t, fresh), "failed")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 500, "internal_error")
	if strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "SQL") || strings.Contains(w.Body.String(), "/secret") {
		t.Fatalf("private failure leaked: %s", w.Body)
	}
	coordinationHTTPUnchanged(t, raw, before)
	if _, err := raw.Exec("DROP TRIGGER coordination_http_fail"); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, target := range paths {
		method := http.MethodGet
		if target == path {
			method = http.MethodPost
		}
		w := a.request(method, target, body, "original")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 500, "internal_error")
		for _, private := range []string{a.path, "sql", "closed", "SELECT", "routing_decisions"} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatalf("internal failure leaked %q: %s", private, w.Body)
			}
		}
	}
}
