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
)

func dispatchRequestHTTPServe(a *groupAPI, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	a.handler.ServeHTTP(w, r)
	return w
}

func TestDispatchRequestHTTPStrictInputWithoutWrites(t *testing.T) {
	f := dispatchRequestHTTPSetup(t)
	raw := messageSQL(t, f.a)
	before := coordinationHTTPSnapshot(t, raw)
	body := dispatchRequestHTTPBody(1, 1, "arrange")
	for _, field := range []string{"expected_task_revision", "expected_authority_generation"} {
		t.Run(field, func(t *testing.T) {
			for _, value := range []string{"null", "0", "-1", "-9223372036854775808"} {
				invalid := strings.Replace(body, `"`+field+`":1`, `"`+field+`":`+value, 1)
				dispatchRequestHTTPError(t, f.a.request("POST", f.path, invalid, "reusable"), 400, "invalid_input")
			}
			missing := strings.Replace(body, `"`+field+`":1,`, "", 1)
			dispatchRequestHTTPError(t, f.a.request("POST", f.path, missing, "reusable"), 400, "invalid_input")
			for _, value := range []string{"1.5", "1.0", "1e0", "9223372036854775808", "-9223372036854775809", `"1"`, "true", "[]", "{}"} {
				invalid := strings.Replace(body, `"`+field+`":1`, `"`+field+`":`+value, 1)
				dispatchRequestHTTPError(t, f.a.request("POST", f.path, invalid, "reusable"), 400, "invalid_json")
			}
			// Positive int64 snapshots above JS precision reach Store's conflict check.
			for _, value := range []string{"9007199254740993", "9223372036854775807"} {
				stale := strings.Replace(body, `"`+field+`":1`, `"`+field+`":`+value, 1)
				dispatchRequestHTTPError(t, f.a.request("POST", f.path, stale, "reusable"), 409, "conflict")
			}
			coordinationHTTPUnchanged(t, raw, before)
		})
	}
	t.Run("reason", func(t *testing.T) {
		for _, invalid := range []string{`{}`, `{"expected_task_revision":1,"expected_authority_generation":1}`, strings.Replace(body, `"arrange"`, `null`, 1), dispatchRequestHTTPBody(1, 1, ""), dispatchRequestHTTPBody(1, 1, " \r\n\t\u2003 ")} {
			dispatchRequestHTTPError(t, f.a.request("POST", f.path, invalid, "reusable"), 400, "invalid_input")
		}
		coordinationHTTPUnchanged(t, raw, before)
	})
	t.Run("JSON", func(t *testing.T) {
		for _, invalid := range []string{"", `null`, `[]`, `42`, `"object"`, `{`, body + ` {}`, body + ` null`, strings.Replace(body, `"arrange"`, `42`, 1), "{\"reason\":\"\xff\",\"expected_task_revision\":1,\"expected_authority_generation\":1}"} {
			dispatchRequestHTTPError(t, f.a.request("POST", f.path, invalid, "reusable"), 400, "invalid_json")
		}
		for _, field := range []string{"expected_task_revision", "expected_authority_generation", "reason"} {
			value := "1"
			if field == "reason" {
				value = `"arrange"`
			}
			for _, duplicate := range []string{field, strings.ToUpper(field)} {
				invalid := strings.TrimSuffix(body, "}") + fmt.Sprintf(`,%q:%s}`, duplicate, value)
				dispatchRequestHTTPError(t, f.a.request("POST", f.path, invalid, "reusable"), 400, "invalid_json")
			}
		}
		for _, field := range []string{"group", "group_id", "task", "task_id", "topic_id", "source", "message_id", "mandate_id", "Mandate", "coordinator_member_id", "binding", "binding_revision", "policy", "policy_revision", "requested_by", "actor", "actor_id", "caller", "role", "grants", "runtime", "runtime_ready", "state", "delivery", "task_revision", "authority_generation", "unknown"} {
			invalid := strings.TrimSuffix(body, "}") + fmt.Sprintf(`,%q:"forged"}`, field)
			dispatchRequestHTTPError(t, f.a.request("POST", f.path, invalid, "reusable"), 400, "invalid_json")
		}
		coordinationHTTPUnchanged(t, raw, before)
	})
	t.Run("key", func(t *testing.T) {
		for _, keys := range [][]string{nil, {""}, {" "}, {" key"}, {"key "}, {"a", "a"}, {"a", "b"}, {"a,b"}, {"a, b"}, {strings.Repeat("x", 129)}, {"line\nkey"}, {"tab\tkey"}, {"中文"}, {"\x7f"}, {"\x00"}} {
			dispatchRequestHTTPError(t, f.a.request("POST", f.path, body, keys...), 400, "invalid_input")
		}
		coordinationHTTPUnchanged(t, raw, before)
	})
	t.Run("content-type-and-size", func(t *testing.T) {
		for _, types := range [][]string{nil, {"text/plain"}, {"application/problem+json"}, {"application/json; charset=latin-1"}, {"application/json", "application/json"}} {
			r := httptest.NewRequest("POST", "http://"+DefaultAddress+f.path, strings.NewReader(body))
			r.Header.Set("Idempotency-Key", "reusable")
			for _, media := range types {
				r.Header.Add("Content-Type", media)
			}
			dispatchRequestHTTPError(t, dispatchRequestHTTPServe(f.a, r), 415, "unsupported_media_type")
		}
		dispatchRequestHTTPError(t, f.a.request("POST", f.path, dispatchRequestHTTPBody(1, 1, strings.Repeat("x", MaxJSONBodyBytes)), "reusable"), 413, "body_too_large")
		coordinationHTTPUnchanged(t, raw, before)
	})
	// Malformed requests did not consume a key or the Task's unfinished slot.
	r := httptest.NewRequest("POST", "http://"+DefaultAddress+f.path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json; charset=UTF-8")
	r.Header.Set("Idempotency-Key", "reusable")
	request := dispatchRequestHTTPValue[domain.DispatchRequest](t, dispatchRequestHTTPServe(f.a, r), 201)
	dispatchRequestHTTPAdded(t, raw, before, 1)
	before = coordinationHTTPSnapshot(t, raw)
	if got := dispatchRequestHTTPValue[domain.DispatchRequest](t, f.a.request("POST", f.path, body, "reusable"), 201); got != request {
		t.Fatal("valid retry changed original request")
	}
	coordinationHTTPUnchanged(t, raw, before)
}

func TestDispatchRequestHTTPQueriesMethodsAndNoDiscoveryRoutes(t *testing.T) {
	f := dispatchRequestHTTPSetup(t)
	request := dispatchRequestHTTPValue[domain.DispatchRequest](t, f.a.request("POST", f.path, f.body, "original"), 201)
	getPath := dispatchRequestHTTPGetPath(f.group.ID, request.ID)
	raw := messageSQL(t, f.a)
	before := coordinationHTTPSnapshot(t, raw)
	for _, target := range []struct{ method, path string }{{"POST", f.path}, {"GET", getPath}} {
		for _, query := range []string{"group_id=forged", "task_id=forged", "topic_id=", "expected_task_revision=1", "actor=user", "x=1&x=2", "%zz", "x=1;y=2"} {
			r := httptest.NewRequest(target.method, "http://"+DefaultAddress+target.path+"?"+query, strings.NewReader(f.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "original")
			originalURL, originalQuery := r.URL, r.URL.RawQuery
			dispatchRequestHTTPError(t, dispatchRequestHTTPServe(f.a, r), 400, "invalid_input")
			if r.URL != originalURL || r.URL.RawQuery != originalQuery {
				t.Fatal("route rewrote caller URL/query")
			}
		}
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
			if method == target.method {
				continue
			}
			w := f.a.request(method, target.path, f.body, "original")
			dispatchRequestHTTPError(t, w, 405, "method_not_allowed")
			if w.Header().Get("Allow") != target.method {
				t.Fatalf("unexpected allowed methods: %v", w.Header())
			}
		}
	}
	for _, target := range []struct{ method, path string }{{"GET", coordinationHTTPBase(f.group.ID) + "/dispatch-requests"}, {"GET", f.path}, {"POST", getPath + "/cancel"}, {"POST", getPath + "/reopen"}} {
		w := f.a.request(target.method, target.path, f.body, "original")
		// The known POST task route rejects GET; unregistered discovery/actions are 404.
		status, code := 404, "not_found"
		if target.path == f.path {
			status, code = 405, "method_not_allowed"
		}
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), status, code)
	}
	coordinationHTTPUnchanged(t, raw, before)
}

func TestDispatchRequestHTTPHostOriginAndFetchMetadata(t *testing.T) {
	f := dispatchRequestHTTPSetup(t)
	raw := messageSQL(t, f.a)
	before := coordinationHTTPSnapshot(t, raw)
	for _, boundary := range []struct {
		host     string
		origins  []string
		metadata map[string]string
		code     string
	}{
		{"evil.invalid", nil, nil, "invalid_host"},
		{DefaultAddress, []string{"http://evil.invalid"}, nil, "invalid_origin"},
		{DefaultAddress, []string{"null"}, nil, "invalid_origin"},
		{DefaultAddress, []string{"http://" + DefaultAddress, "http://" + DefaultAddress}, nil, "invalid_origin"},
		{DefaultAddress, nil, map[string]string{"Sec-Fetch-Site": "same-origin"}, "invalid_origin"},
		{DefaultAddress, nil, map[string]string{"Sec-Fetch-Mode": "navigate"}, "invalid_origin"},
		{DefaultAddress, []string{"http://" + DefaultAddress}, map[string]string{"Sec-Fetch-Site": "cross-site"}, "invalid_origin"},
	} {
		r := httptest.NewRequest("POST", "http://"+DefaultAddress+f.path, strings.NewReader(f.body))
		r.Host = boundary.host
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "!")
		r.Header.Set("X-Forwarded-Host", DefaultAddress)
		r.Header.Set("X-Forwarded-Proto", "http")
		for _, origin := range boundary.origins {
			r.Header.Add("Origin", origin)
		}
		for key, value := range boundary.metadata {
			r.Header.Set(key, value)
		}
		dispatchRequestHTTPError(t, dispatchRequestHTTPServe(f.a, r), 403, boundary.code)
	}
	coordinationHTTPUnchanged(t, raw, before)
	var original domain.DispatchRequest
	for _, browser := range []bool{true, false} {
		r := httptest.NewRequest("POST", "http://"+DefaultAddress+f.path, strings.NewReader(f.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "!")
		if browser {
			r.Header.Set("Origin", "http://"+DefaultAddress)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}
		got := dispatchRequestHTTPValue[domain.DispatchRequest](t, dispatchRequestHTTPServe(f.a, r), 201)
		if browser {
			original = got
		} else if got != original {
			t.Fatal("local client replay changed browser registration")
		}
	}
	dispatchRequestHTTPAdded(t, raw, before, 1)
	before = coordinationHTTPSnapshot(t, raw)
	getPath := dispatchRequestHTTPGetPath(f.group.ID, original.ID)
	r := httptest.NewRequest("GET", "http://"+DefaultAddress+getPath, nil)
	r.Host = "evil.invalid"
	dispatchRequestHTTPError(t, dispatchRequestHTTPServe(f.a, r), 403, "invalid_host")
	r = httptest.NewRequest("GET", "http://"+DefaultAddress+getPath, nil)
	r.Header.Set("Origin", "http://evil.invalid")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if got := dispatchRequestHTTPValue[domain.DispatchView](t, dispatchRequestHTTPServe(f.a, r), 200); got.Request != original {
		t.Fatal("read transport changed existing origin semantics")
	}
	var savedKey string
	if err := raw.QueryRow("SELECT idempotency_key FROM mutation_idempotency WHERE operation='request_dispatch'").Scan(&savedKey); err != nil || savedKey != "!" {
		t.Fatalf("one-byte visible key was rewritten: %q, %v", savedKey, err)
	}
	coordinationHTTPUnchanged(t, raw, before)
}

func TestDispatchRequestHTTPUnknownContextAndSanitizedStorageErrors(t *testing.T) {
	f := dispatchRequestHTTPSetup(t)
	raw := messageSQL(t, f.a)
	before := coordinationHTTPSnapshot(t, raw)
	if _, err := raw.Exec("CREATE TRIGGER dispatch_request_http_failure BEFORE INSERT ON coordination_dispatch_state BEGIN SELECT RAISE(ABORT,'private SQL /secret/workspace.db'); END"); err != nil {
		t.Fatal(err)
	}
	dispatchRequestHTTPError(t, f.a.request("POST", f.path, f.body, "original"), 500, "internal_error")
	coordinationHTTPUnchanged(t, raw, before)
	if _, err := raw.Exec("DROP TRIGGER dispatch_request_http_failure"); err != nil {
		t.Fatal(err)
	}
	first := f.a.request("POST", f.path, f.body, "original")
	request := dispatchRequestHTTPValue[domain.DispatchRequest](t, first, 201)
	dispatchRequestHTTPAdded(t, raw, before, 1)
	getPath := dispatchRequestHTTPGetPath(f.group.ID, request.ID)
	before = coordinationHTTPSnapshot(t, raw)
	for _, expired := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		status, code := 408, "request_canceled"
		if expired {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			status, code = 504, "deadline_exceeded"
		}
		cancel()
		for _, target := range []struct{ method, path string }{{"POST", f.path}, {"GET", getPath}} {
			r := httptest.NewRequest(target.method, "http://"+DefaultAddress+target.path, strings.NewReader(f.body)).WithContext(ctx)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "original")
			w := dispatchRequestHTTPServe(f.a, r)
			dispatchRequestHTTPError(t, w, status, code)
			if !strings.Contains(w.Body.String(), "操作结果尚未确认") || !strings.Contains(w.Body.String(), "沿用原幂等键") {
				t.Fatalf("context error claimed a definite write outcome: %s", w.Body)
			}
		}
	}
	if w := f.a.request("POST", f.path, f.body, "original"); w.Code != 201 || w.Body.String() != first.Body.String() {
		t.Fatalf("unknown-result retry replaced the original receipt: %d %s", w.Code, w.Body)
	}
	coordinationHTTPUnchanged(t, raw, before)
	if err := f.a.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct{ method, path string }{{"POST", f.path}, {"GET", getPath}} {
		dispatchRequestHTTPError(t, f.a.request(target.method, target.path, f.body, "original"), 500, "internal_error")
	}
	coordinationHTTPUnchanged(t, raw, before)
}
