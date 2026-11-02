package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/provider"
)

var memberHistoryToolHTTPRequests = []struct{ operation, body string }{
	{"search_history", `{"query":"initial"}`},
	{"read_history", `{"message_id":"history-validation"}`},
}

func TestMemberHistoryToolHTTPStrictInputs(t *testing.T) {
	f := newMemberHistoryToolHTTP(t)
	memberHistoryToolMessage(t, f, "history-validation", f.groupID, "trusted-topic", "中😀")
	before := memberHistoryToolState(t, f)
	for _, request := range memberHistoryToolHTTPRequests {
		for _, field := range []string{"actor_id", "sender_id", "group_id", "member_id", "conversation_id", "topic_id", "caller", "caller_id", "turn_id", "claim", "capability", "token", "role", "grants", "provider_id", "model", "workspace", "idempotency_key"} {
			t.Run(request.operation+"/selector/"+field, func(t *testing.T) {
				body := strings.TrimSuffix(request.body, "}") + fmt.Sprintf(`,%q:"forged"}`, field)
				w := f.request(t, request.operation, body)
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
			})
		}
	}
	for _, input := range []struct {
		operation string
		bodies    []string
	}{
		{"search_history", []string{
			``, `null`, `[]`, `{`, `{"query":"initial"} {}`, `{"query":"initial"}x`,
			`{"query":null}`, `{"query":42}`, `{"query":[]}`, `{"query":true}`,
			`{"query":"initial","limit":null}`, `{"query":"initial","limit":"1"}`,
			`{"query":"initial","limit":1.5}`, `{"query":"initial","limit":1e0}`,
			`{"query":"initial","limit":true}`, `{"query":"initial","limit":9223372036854775808}`,
			`{"query":"initial","before":null}`, `{"query":"initial","before":42}`,
			`{"query":"initial","query":"other"}`, `{"query":"initial","QUERY":"other"}`,
			`{"query":"initial","\u0071uery":"other"}`, `{"query":"initial","limit":1,"LIMIT":2}`,
			`{"query":"initial","before":"","Before":""}`, "{\"query\":\"\xff\"}",
		}},
		{"read_history", []string{
			``, `null`, `[]`, `{`, `{"message_id":"history-validation"} {}`,
			`{"message_id":null}`, `{"message_id":42}`, `{"message_id":{}}`,
			`{"message_id":"history-validation","offset":null}`, `{"message_id":"history-validation","offset":"0"}`,
			`{"message_id":"history-validation","offset":0.5}`, `{"message_id":"history-validation","offset":true}`,
			`{"message_id":"history-validation","offset":9223372036854775808}`,
			`{"message_id":"history-validation","length":null}`, `{"message_id":"history-validation","length":"1"}`,
			`{"message_id":"history-validation","length":1.5}`, `{"message_id":"history-validation","length":false}`,
			`{"message_id":"history-validation","message_id":"other"}`, `{"message_id":"history-validation","MESSAGE_ID":"other"}`,
			`{"message_id":"history-validation","offset":0,"OFFSET":1}`, `{"message_id":"history-validation","length":1,"Length":2}`,
			"{\"message_id\":\"\xff\"}",
		}},
	} {
		for i, body := range input.bodies {
			t.Run(fmt.Sprintf("%s/json/%d", input.operation, i), func(t *testing.T) {
				w := f.request(t, input.operation, body)
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
			})
		}
	}
	for _, input := range []struct{ operation, body string }{
		{"search_history", `{}`}, {"search_history", `{"limit":1}`},
		{"search_history", `{"query":""}`}, {"search_history", `{"query":" \u3000\n"}`},
		{"search_history", `{"query":"x\u0000y"}`},
		{"search_history", fmt.Sprintf(`{"query":%q}`, strings.Repeat("界", 257))},
		{"search_history", fmt.Sprintf(`{"query":%q}`, strings.Repeat(" ", 4096)+"x")},
		{"search_history", `{"query":"initial","limit":0}`}, {"search_history", `{"query":"initial","limit":-1}`},
		{"search_history", `{"query":"initial","limit":51}`}, {"search_history", `{"query":"initial","before":"invalid"}`},
		{"read_history", `{}`}, {"read_history", `{"offset":0}`},
		{"read_history", `{"message_id":""}`}, {"read_history", `{"message_id":" \n"}`},
		{"read_history", `{"message_id":"x\u0000y"}`},
		{"read_history", fmt.Sprintf(`{"message_id":%q}`, strings.Repeat("x", 257))},
		{"read_history", `{"message_id":"history-validation","offset":-1}`},
		{"read_history", `{"message_id":"history-validation","offset":3}`},
		{"read_history", `{"message_id":"history-validation","offset":9223372036854775807}`},
		{"read_history", `{"message_id":"history-validation","length":0}`},
		{"read_history", `{"message_id":"history-validation","length":-1}`},
		{"read_history", `{"message_id":"history-validation","length":4097}`},
	} {
		w := f.request(t, input.operation, input.body)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, request := range memberHistoryToolHTTPRequests {
		for _, types := range [][]string{nil, {"text/plain"}, {"application/json", "application/json"}} {
			r := f.newRequest(request.operation, request.body)
			r.Header.Del("Content-Type")
			for _, mediaType := range types {
				r.Header.Add("Content-Type", mediaType)
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 415, "unsupported_media_type")
		}
	}
	if w := f.request(t, "search_history", `{"query":"initial","limit":1,"before":""}`); w.Code != 200 {
		t.Fatalf("minimum explicit search limit rejected: %d %s", w.Code, w.Body)
	}
	if w := f.request(t, "read_history", `{"message_id":"history-validation","offset":0,"length":1}`); w.Code != 200 {
		t.Fatalf("minimum explicit length or zero offset rejected: %d %s", w.Code, w.Body)
	}
	if memberHistoryToolState(t, f) != before {
		t.Fatal("input rejection or boundary reads changed durable state")
	}
}

func TestMemberHistoryToolHTTPAuthorityAndLifecycle(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "wrong", "malformed", "duplicate", "claim-token", "revoked", "grant-denied", "search-only", "read-only", "cancel", "unknown", "recovery", "terminal"} {
		t.Run(mode, func(t *testing.T) {
			var operations []string
			switch mode {
			case "grant-denied":
				operations = []string{"list_members"}
			case "search-only":
				operations = []string{"search_history"}
			case "read-only":
				operations = []string{"read_history"}
			}
			f := newMemberHistoryToolHTTP(t, operations...)
			memberHistoryToolMessage(t, f, "history-validation", f.groupID, "trusted-topic", "中😀")
			ctx := context.Background()
			var err error
			switch mode {
			case "revoked":
				f.tools.Revoke(f.token)
			case "cancel":
				_, err = f.db.RequestTurnCancel(ctx, f.claim.GroupID, f.claim.TurnID)
			case "unknown":
				turn, receiptErr := f.db.ApplyTurnReceipt(ctx, f.claim, provider.ExecutionReceipt{Outcome: provider.OutcomeUnknown})
				err = receiptErr
				if err == nil && turn.State != "unknown" {
					t.Fatal("fixture did not enter unknown authority state")
				}
			case "recovery":
				_, err = f.db.RecoverInFlight(ctx)
			case "terminal":
				_, err = f.db.ApplyTurnReceipt(ctx, f.claim, provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded, Started: true, Accepted: true, NativeSessionID: "native", DirectChildReaped: true, OutputComplete: true})
			}
			if err != nil {
				t.Fatal(err)
			}
			before := memberHistoryToolState(t, f)
			for _, request := range memberHistoryToolHTTPRequests {
				r := f.newRequest(request.operation, request.body)
				switch mode {
				case "missing":
					r.Header.Del("Authorization")
				case "wrong":
					r.Header.Set("Authorization", "Bearer unknown-capability")
				case "malformed":
					r.Header.Set("Authorization", "bearer "+f.token)
				case "duplicate":
					r.Header.Add("Authorization", "Bearer "+f.token)
				case "claim-token":
					r.Header.Set("Authorization", "Bearer "+f.claim.Token)
				}
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
				allowed := mode == "valid" || mode == "search-only" && request.operation == "search_history" || mode == "read-only" && request.operation == "read_history"
				if allowed {
					if w.Code != 200 {
						t.Fatalf("granted operation denied: %d %s", w.Code, w.Body)
					}
				} else {
					assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
				}
			}
			if memberHistoryToolState(t, f) != before {
				t.Fatal("history changed delivery, receipt, task or claim authority")
			}
		})
	}
}

func TestMemberHistoryToolHTTPRequestContextClassification(t *testing.T) {
	for _, credential := range []string{"valid", "invalid", "revoked", "missing-grant"} {
		t.Run(credential, func(t *testing.T) {
			var operations []string
			if credential == "missing-grant" {
				operations = []string{"list_members"}
			}
			f := newMemberHistoryToolHTTP(t, operations...)
			token := f.token
			if credential == "invalid" {
				token = "unknown-capability"
			} else if credential == "revoked" {
				f.tools.Revoke(token)
			}
			before := memberHistoryToolState(t, f)
			for _, state := range []string{"canceled", "deadline"} {
				requestCtx, cancel := context.WithCancel(context.Background())
				wantStatus, wantCode := http.StatusRequestTimeout, "request_canceled"
				if state == "deadline" {
					cancel()
					requestCtx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
					wantStatus, wantCode = http.StatusGatewayTimeout, "deadline_exceeded"
				}
				cancel()
				if credential != "valid" {
					wantStatus, wantCode = http.StatusForbidden, "forbidden"
				}
				for _, request := range memberHistoryToolHTTPRequests {
					r := f.newRequest(request.operation, request.body).WithContext(requestCtx)
					r.Header.Set("Authorization", "Bearer "+token)
					w := httptest.NewRecorder()
					f.handler.ServeHTTP(w, r)
					f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
					assertError(t, w.Code, w.Header(), w.Body.Bytes(), wantStatus, wantCode)
				}
			}
			if memberHistoryToolState(t, f) != before {
				t.Fatal("request cancellation or failed authorization changed durable state")
			}
		})
	}
}

func TestMemberHistoryToolHTTPPreservesNativeAndOuterGuards(t *testing.T) {
	f := newMemberHistoryToolHTTP(t)
	memberHistoryToolMessage(t, f, "history-validation", f.groupID, "trusted-topic", "中😀")
	before := memberHistoryToolState(t, f)
	for i, request := range memberHistoryToolHTTPRequests {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
			r := f.newRequest(request.operation, request.body)
			r.Method = method
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 405, "method_not_allowed")
			if w.Header().Get("Allow") != "POST" {
				t.Fatal("history permits a method other than POST")
			}
		}
		for _, query := range []string{"?", "?limit=1", "?before=forged", "?token=forged", "?topic_id=forged"} {
			w := f.request(t, request.operation+query, request.body)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		}
		for _, mode := range []string{"wrong-host", "same-origin", "foreign-origin", "fetch-metadata"} {
			r := f.newRequest(request.operation, request.body)
			code := "invalid_origin"
			switch mode {
			case "wrong-host":
				r.Host, code = "attacker.example", "invalid_host"
			case "same-origin":
				r.Header.Set("Origin", "http://"+DefaultAddress)
				code = "forbidden"
			case "foreign-origin":
				r.Header.Set("Origin", "http://attacker.example")
			case "fetch-metadata":
				r.Header.Set("Sec-Fetch-Mode", "cors")
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), mode == "same-origin")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, code)
		}
		for _, key := range []string{"origin", "sec-fetch-arbitrary"} {
			r := f.newRequest(request.operation, request.body)
			r.Header[key] = []string{""}
			w := httptest.NewRecorder()
			f.routes[4+i].Handler(w, r)
			f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
		}
	}
	if memberHistoryToolState(t, f) != before {
		t.Fatal("transport rejections changed durable state")
	}
}
