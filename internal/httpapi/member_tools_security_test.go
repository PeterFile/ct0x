package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/provider"
)

var memberToolHTTPRequests = []struct{ operation, body string }{
	{"list_members", `{}`},
	{"read_channel", `{}`},
	{"send_message", `{"content":"native message"}`},
	{"create_member", `{"name":"Analyst","role":"Analyze data","reason":"Explain dataset A and its outliers"}`},
}

func TestMemberToolHTTPExactlyFourPrivateRoutes(t *testing.T) {
	f := newMemberToolHTTP(t)
	if len(f.routes) != 4 {
		t.Fatalf("got %d routes, want exactly four", len(f.routes))
	}
	want := make(map[string]bool)
	for _, request := range memberToolHTTPRequests {
		want["/api/v1/member-tools/"+request.operation] = true
	}
	for _, route := range f.routes {
		if route.Method != http.MethodPost || !want[route.Pattern] {
			t.Fatalf("unexpected route: %s %s", route.Method, route.Pattern)
		}
		delete(want, route.Pattern)
	}
	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		r := f.newRequest("send_message", `{}`)
		r.Method = method
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 405, "method_not_allowed")
		if w.Header().Get("Allow") != "POST" {
			t.Fatal("private endpoint permits another method")
		}
	}
	r := f.newRequest("send_message/forged-group", `{}`)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
}

func TestMemberToolHTTPAuthorizationUniformlyForbidden(t *testing.T) {
	f := newMemberToolHTTP(t)
	before := f.counts(t)
	var forbidden []byte
	for _, request := range memberToolHTTPRequests {
		for i, values := range [][]string{
			nil, {""}, {"Bearer"}, {"Bearer "}, {"bearer " + f.token}, {"BEARER " + f.token},
			{"Bearer\t" + f.token}, {"Bearer  " + f.token}, {"Bearer " + f.token + " "},
			{"Bearer \"" + f.token + "\""}, {"Bearer " + f.token + ";parameter=x"},
			{"Bearer " + f.token, "Bearer " + f.token}, {"Bearer " + f.token + ", Bearer " + f.token},
			{"Bearer " + strings.Repeat("a", 257)}, {"Bearer unknown-capability"}, {"Bearer " + f.claim.Token},
		} {
			t.Run(fmt.Sprintf("%s/%d", request.operation, i), func(t *testing.T) {
				r := f.newRequest(request.operation, request.body, "auth")
				r.Header.Del("Authorization")
				for _, value := range values {
					r.Header.Add("Authorization", value)
				}
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
				if forbidden == nil {
					forbidden = bytes.Clone(w.Body.Bytes())
				} else if !bytes.Equal(forbidden, w.Body.Bytes()) {
					t.Fatal("authorization failures reveal which credential check failed")
				}
			})
		}
	}
	r := f.newRequest("send_message", `{"content":"denied"}`, "auth")
	r.Header["authorization"] = []string{"Bearer " + f.token}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
	if f.counts(t) != before {
		t.Fatal("rejected credentials changed durable data")
	}
}

func TestMemberToolHTTPRejectsAnyBrowserMetadata(t *testing.T) {
	f := newMemberToolHTTP(t)
	for i, route := range f.routes {
		request := memberToolHTTPRequests[i]
		for _, header := range []http.Header{
			{"Origin": {"http://" + DefaultAddress}}, {"Origin": {""}}, {"origin": nil},
			{"Origin": {"http://attacker.example"}}, {"Sec-Fetch-Mode": {"cors"}},
			{"Sec-Fetch-Site": {"same-origin"}}, {"Sec-Fetch-User": {"?1"}},
			{"sec-fetch-arbitrary": {""}}, {"Origin": {"http://" + DefaultAddress}, "Sec-Fetch-Site": {"same-origin"}},
		} {
			r := f.newRequest(request.operation, request.body, "browser")
			for key, values := range header {
				r.Header[key] = values
			}
			r.Header.Set("User-Agent", "node")
			w := httptest.NewRecorder()
			// Exercise the route's stricter boundary, even when the outer guard would reject first.
			route.Handler(w, r)
			f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
		}
	}
	if f.counts(t) != ([6]int{3, 1, 1, 1, 0, 1}) {
		t.Fatal("browser-looking requests persisted side effects")
	}
	decoded[memberToolHTTPSendResult](t, f.request(t, "send_message", `{"content":"native succeeds"}`, "native"), 201)
}

func TestMemberToolHTTPRejectsSelectorsTypesAndMissingKeys(t *testing.T) {
	f := newMemberToolHTTP(t)
	before := f.counts(t)
	for _, request := range memberToolHTTPRequests {
		for _, field := range []string{"actor_id", "SenderID", "group_id", "member_id", "conversation_id", "topic_id", "provider_id", "model", "workspace", "TurnID", "claim", "grants", "token", "idempotency_key"} {
			body := strings.TrimSuffix(request.body, "}")
			if body != "{" {
				body += ","
			}
			body += fmt.Sprintf(`%q:"forged"}`, field)
			w := f.request(t, request.operation, body, "reject")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
		}
		for _, query := range []string{"?token=forged", "?group=forged", "?sender=forged", "?topic_id=forged", "?workspace=/private", "?path=/private", "?before=forged", "?limit=1", "?"} {
			w := f.request(t, request.operation+query, request.body, "reject")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		}
	}
	for _, request := range []struct{ operation, body string }{
		{"list_members", `null`}, {"read_channel", `[]`}, {"send_message", `{`}, {"create_member", `{} {}`},
		{"list_members", `{"limit":"1"}`}, {"list_members", `{"limit":1.5}`}, {"list_members", `{"limit":null}`}, {"read_channel", `{"before":42}`},
		{"read_channel", `{"before":null}`}, {"send_message", `{"content":42}`}, {"send_message", `{"content":null}`},
		{"send_message", `{"content":"one","Content":"two"}`}, {"list_members", `{"limit":1,"LIMIT":2}`},
		{"send_message", `{"content":"ok","recipient_ids":"member"}`}, {"send_message", `{"content":"ok","recipient_ids":null}`},
		{"send_message", `{"content":"ok","recipient_ids":[null]}`}, {"create_member", `{"name":"n","role":{},"reason":"why"}`},
		{"create_member", `{"name":"n","role":"r","reason":"a","Reason":"b"}`},
		{"send_message", "{\"content\":\"\xff\"}"},
	} {
		w := f.request(t, request.operation, request.body, "reject")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
	}
	for _, body := range []string{`{"limit":0}`, `{"limit":-1}`, `{"limit":201}`, `{"before":""}`, `{"before":"invalid"}`} {
		w := f.request(t, "list_members", body)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, types := range [][]string{nil, {"text/plain"}, {"application/json", "application/json"}} {
		r := f.newRequest("send_message", `{"content":"invalid media type"}`, "reject")
		r.Header.Del("Content-Type")
		for _, value := range types {
			r.Header.Add("Content-Type", value)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 415, "unsupported_media_type")
	}
	for _, request := range []struct{ operation, body string }{
		{"send_message", `{}`}, {"send_message", `{"content":" "}`},
		{"create_member", `{"name":"n","role":"r"}`},
	} {
		w := f.request(t, request.operation, request.body, "reject")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, request := range memberToolHTTPRequests[2:] {
		for _, keys := range [][]string{nil, {""}, {" "}, {"has space"}, {"a,b"}, {"same", "same"}, {strings.Repeat("x", 129)}} {
			w := f.request(t, request.operation, request.body, keys...)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		}
	}
	body, _ := json.Marshal(map[string]any{"content": "cross-group denied", "recipient_ids": []string{f.peer.ID, f.foreign.ID}})
	w := f.request(t, "send_message", string(body), "reject")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	if f.counts(t) != before {
		t.Fatal("invalid input left reservations, messages, or members")
	}
	decoded[memberToolHTTPSendResult](t, f.request(t, "send_message", `{"content":"valid retry"}`, "reject"), 201)
}

func TestMemberToolHTTPGrantsAndDurableLifecycleDenials(t *testing.T) {
	for _, transition := range []string{"omitted-grant", "cancel", "recovery", "terminal"} {
		t.Run(transition, func(t *testing.T) {
			operations := []string{}
			if transition == "omitted-grant" {
				operations = []string{"read_channel"}
			}
			f := newMemberToolHTTP(t, operations...)
			ctx := context.Background()
			var err error
			switch transition {
			case "cancel":
				_, err = f.db.RequestTurnCancel(ctx, f.claim.GroupID, f.claim.TurnID)
			case "recovery":
				_, err = f.db.RecoverInFlight(ctx)
			case "terminal":
				_, err = f.db.ApplyTurnReceipt(ctx, f.claim, provider.ExecutionReceipt{Outcome: provider.OutcomeSucceeded, Terminal: provider.OutcomeSucceeded, Started: true, Accepted: true, NativeSessionID: "native", DirectChildReaped: true, OutputComplete: true})
			}
			if err != nil {
				t.Fatal(err)
			}
			before := f.counts(t)
			for _, request := range memberToolHTTPRequests {
				w := f.request(t, request.operation, request.body, "lifecycle")
				if transition == "omitted-grant" && request.operation == "read_channel" {
					decoded[listResult[domain.Message]](t, w, 200)
				} else {
					assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
				}
			}
			if f.counts(t) != before {
				t.Fatal("denied operations changed durable data")
			}
		})
	}
}

func TestMemberToolHTTPRealNativeListenerPreservesOuterGuard(t *testing.T) {
	f := newMemberToolHTTP(t)
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = mustServer(t, ts.Listener.Addr().String(), f.routes...)
	ts.Start()
	t.Cleanup(ts.Close)
	for _, mode := range []string{"native", "duplicate-auth", "same-origin", "node-fetch", "wrong-host"} {
		r, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/member-tools/send_message", strings.NewReader(`{"content":"native listener"}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+f.token)
		r.Header.Set("Idempotency-Key", "listener")
		r.Header.Set("User-Agent", "node")
		switch mode {
		case "duplicate-auth":
			r.Header.Add("Authorization", "Bearer "+f.token)
		case "same-origin":
			r.Header.Set("Origin", ts.URL)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		case "node-fetch":
			r.Header.Set("Sec-Fetch-Mode", "cors")
		case "wrong-host":
			r.Host = "attacker.example"
		}
		response, err := ts.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		owned := mode != "node-fetch" && mode != "wrong-host"
		f.safeResponse(t, response.StatusCode, response.Header, body, owned)
		if mode == "native" {
			var result memberToolHTTPSendResult
			if err := json.Unmarshal(body, &result); err != nil || response.StatusCode != 201 || result.MessageID == "" || !result.ContentOmitted {
				t.Fatalf("native client failed: %d %s", response.StatusCode, body)
			}
			message, err := f.db.GetMessage(context.Background(), f.groupID, result.MessageID)
			if err != nil || message.Content != "native listener" || message.SenderID != f.caller.ID {
				t.Fatal("listener did not execute the durable bound operation")
			}
		} else {
			code := map[string]string{"duplicate-auth": "forbidden", "same-origin": "forbidden", "node-fetch": "invalid_origin", "wrong-host": "invalid_host"}[mode]
			assertError(t, response.StatusCode, response.Header, body, 403, code)
		}
	}
	if f.counts(t) != ([6]int{3, 2, 1, 1, 1, 1}) {
		t.Fatal("rejected listener requests duplicated the native mutation")
	}
}
