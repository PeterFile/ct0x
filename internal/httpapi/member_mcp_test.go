package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

const memberMCPInitialize = `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`

func memberMCPRequest(f *memberToolHTTPFixture, body string) *http.Request {
	r := f.newRequest("mcp", body)
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-06-18")
	return r
}

func memberMCPHTTP(t *testing.T, f *memberToolHTTPFixture, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, memberMCPRequest(f, body))
	f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
	if w.Header().Get("Mcp-Session-Id") != "" {
		t.Fatal("stateless transport created a session")
	}
	return w
}

func memberMCPCall(id, name, arguments string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, arguments)
}

func memberMCPText(t *testing.T, w *httptest.ResponseRecorder, isError bool) string {
	t.Helper()
	got := decoded[struct {
		JSONRPC string `json:"jsonrpc"`
		Result  struct {
			Content []struct{ Type, Text string } `json:"content"`
			IsError bool                          `json:"isError"`
		} `json:"result"`
	}](t, w, 200)
	if got.JSONRPC != "2.0" || got.Result.IsError != isError || len(got.Result.Content) != 1 || got.Result.Content[0].Type != "text" {
		t.Fatalf("invalid MCP tool result: %s", w.Body)
	}
	return got.Result.Content[0].Text
}

func newMemberMCPHTTP(t *testing.T, operations ...string) *memberToolHTTPFixture {
	t.Helper()
	if len(operations) == 0 {
		operations = []string{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"}
	}
	f := newMemberToolHTTP(t, operations...)
	f.routes = append(f.routes, MemberHistoryToolRoutes(f.tools)...)
	f.routes = append(f.routes, MemberRoleToolRoutes(f.tools)...)
	f.routes = append(f.routes, MemberMCPRoutes(f.tools)...)
	f.handler = mustServer(t, DefaultAddress, f.routes...).Handler
	return f
}

func TestMemberMCPProtocolOptInLifecycleAndProfile(t *testing.T) {
	f := newMemberToolHTTP(t)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, memberMCPRequest(f, memberMCPInitialize))
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	f = newMemberMCPHTTP(t)
	before := f.counts(t)
	r := memberMCPRequest(f, strings.Replace(memberMCPInitialize, "2025-06-18", "2025-03-26", 1))
	r.Header.Del("MCP-Protocol-Version")
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	init := decoded[struct {
		ID     string `json:"id"`
		Result struct {
			ProtocolVersion string                         `json:"protocolVersion"`
			Capabilities    map[string]json.RawMessage     `json:"capabilities"`
			ServerInfo      struct{ Name, Version string } `json:"serverInfo"`
		} `json:"result"`
	}](t, w, 200)
	if init.ID != "init" || init.Result.ProtocolVersion != "2025-06-18" || len(init.Result.Capabilities) != 1 || string(init.Result.Capabilities["tools"]) != "{}" || init.Result.ServerInfo.Name == "" || init.Result.ServerInfo.Version == "" {
		t.Fatalf("wrong negotiation/capabilities: %s", w.Body)
	}
	w = memberMCPHTTP(t, f, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if w.Code != 202 || w.Body.Len() != 0 {
		t.Fatal("initialized notification must have an empty 202 response")
	}
	w = memberMCPHTTP(t, f, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var list struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Type                 string                     `json:"type"`
					AdditionalProperties bool                       `json:"additionalProperties"`
					Properties           map[string]json.RawMessage `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Result.Tools {
		names = append(names, tool.Name)
		if tool.InputSchema.Type != "object" || tool.InputSchema.AdditionalProperties {
			t.Fatal("tool schema must be a closed object")
		}
		for _, forbidden := range []string{"caller", "group_id", "member_id", "conversation_id", "turn_id", "claim", "purpose", "grants", "idempotency_key"} {
			if _, exists := tool.InputSchema.Properties[forbidden]; exists {
				t.Fatalf("schema exposes %s", forbidden)
			}
		}
		if tool.Name == "send_message" {
			var assessment struct {
				Properties           map[string]json.RawMessage `json:"properties"`
				AdditionalProperties bool                       `json:"additionalProperties"`
				Required             []string                   `json:"required"`
			}
			if err := json.Unmarshal(tool.InputSchema.Properties["intent_assessment"], &assessment); err != nil || len(assessment.Properties) != 8 || assessment.AdditionalProperties || !slices.Equal(assessment.Required, []string{"disposition", "reason"}) {
				t.Fatal("source-chat schema lost the complete bridge contract")
			}
			var action struct {
				Enum []string `json:"enum"`
			}
			if json.Unmarshal(assessment.Properties["action"], &action) != nil || !slices.Equal(action.Enum, []string{"text_submission", "workspace_change"}) {
				t.Fatal("intake action schema lost text/file compatibility")
			}
			var repair struct {
				Properties           map[string]json.RawMessage `json:"properties"`
				Required             []string                   `json:"required"`
				AdditionalProperties bool                       `json:"additionalProperties"`
			}
			if json.Unmarshal(assessment.Properties["repair"], &repair) != nil || len(repair.Properties) != 2 || repair.AdditionalProperties || !slices.Equal(repair.Required, []string{"review_id", "authorization"}) {
				t.Fatal("repair schema must not expose authority selectors")
			}
			var authorization struct {
				Enum []string `json:"enum"`
			}
			if json.Unmarshal(repair.Properties["authorization"], &authorization) != nil || !slices.Equal(authorization.Enum, []string{"review_request", "original_agreement", "chat_request"}) {
				t.Fatal("repair authorization labels changed")
			}
			var disposition struct{ Enum []string }
			raw := tool.InputSchema.Properties["intent_assessment"]
			if json.Unmarshal(assessment.Properties["disposition"], &disposition) != nil || !slices.Equal(disposition.Enum, []string{"discuss", "clarify", "request_work", "prepare_work"}) ||
				bytes.Contains(raw, []byte("continue_work")) || bytes.Contains(raw, []byte("followup")) {
				t.Fatal("default member schema advertised opt-in continuation")
			}
		}
	}
	if !slices.Equal(names, []string{"list_members", "read_channel", "send_message", "create_member", "search_history", "read_history"}) || f.counts(t) != before {
		t.Fatalf("discovery changed authority or persistence: %v", names)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		r := memberMCPRequest(f, "")
		r.Method = method
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != 405 || w.Header().Get("Allow") != "POST" {
			t.Fatalf("JSON-only endpoint accepted %s: %d %v", method, w.Code, w.Header())
		}
	}
}

func TestMemberMCPNativeHandlersIdentityReplayAndConflicts(t *testing.T) {
	f := newMemberMCPHTTP(t)
	ctx := context.Background()
	body := memberMCPCall(`"write-1"`, "send_message", fmt.Sprintf(`{"content":"bound evidence","recipient_ids":[%q]}`, f.peer.ID))
	first := memberMCPHTTP(t, f, body)
	text := memberMCPText(t, first, false)
	var projection memberToolHTTPSendResult
	if err := json.Unmarshal([]byte(text), &projection); err != nil || projection.MessageID == "" {
		t.Fatalf("lost durable receipt: %s, %v", text, err)
	}
	stored, err := f.db.GetMessage(ctx, f.groupID, projection.MessageID)
	if err != nil || stored.SenderID != f.caller.ID || stored.TopicID != "trusted-topic" || len(stored.Deliveries) != 1 || stored.Deliveries[0].State != domain.DeliveryStateQueued {
		t.Fatalf("MCP did not use the native bound identity: %+v, %v", stored, err)
	}
	var key string
	if err := f.raw.QueryRow("SELECT idempotency_key FROM mutation_idempotency WHERE record_id=?", stored.ID).Scan(&key); err != nil || strings.Contains(key, f.token) || len(key) > 128 {
		t.Fatalf("unsafe server-generated key: %v", err)
	}
	before := f.counts(t)
	replay := memberMCPHTTP(t, f, body)
	if replay.Body.String() != first.Body.String() || f.counts(t) != before {
		t.Fatal("same RPC id/payload did not replay the original receipt")
	}
	canonical := strings.Replace(body, `"write-1"`, `"\u0077rite-1"`, 1)
	canonical = strings.Replace(canonical, `"name":`, `"_meta":{"progressToken":"ignored"},"name":`, 1)
	r := memberMCPRequest(f, canonical)
	r.Header.Set("Idempotency-Key", "client-selected-key-is-ignored")
	canonicalReplay := httptest.NewRecorder()
	f.handler.ServeHTTP(canonicalReplay, r)
	if canonicalReplay.Body.String() != first.Body.String() || f.counts(t) != before {
		t.Fatal("ID escaping, metadata or an HTTP header changed the server-owned key")
	}
	native := f.request(t, "send_message", fmt.Sprintf(`{"content":"bound evidence","recipient_ids":[%q]}`, f.peer.ID), key)
	if native.Body.String() != text || f.counts(t) != before {
		t.Fatal("MCP created a second authorization or persistence implementation")
	}
	conflict := memberMCPHTTP(t, f, strings.Replace(body, "bound evidence", "different evidence", 1))
	if got := memberMCPText(t, conflict, true); !strings.Contains(got, `"code":"conflict"`) || f.counts(t) != before {
		t.Fatalf("changed body lost the native conflict: %s", got)
	}
	f.tools.Revoke(f.token)
	f.token, err = f.tools.Issue(ctx, f.claim, platform.MemberToolGrants{Operations: []string{"send_message"}})
	if err != nil {
		t.Fatal(err)
	}
	fresh := memberMCPText(t, memberMCPHTTP(t, f, body), false)
	if fresh == text || f.counts(t)[1] != before[1]+1 {
		t.Fatal("different run capability collided with the old RPC id")
	}
}

func TestLegacySendMessageRetainsSurrogateDecode(t *testing.T) {
	f := newMemberMCPHTTP(t, "send_message")
	const body = `{"content":"before\ud800after"}`
	native := decoded[memberToolHTTPSendResult](t, f.request(t, "send_message", body, "legacy-surrogate"), 201)
	var mcp memberToolHTTPSendResult
	text := memberMCPText(t, memberMCPHTTP(t, f, memberMCPCall(`"legacy-mcp"`, "send_message", body)), false)
	if err := json.Unmarshal([]byte(text), &mcp); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{native.MessageID, mcp.MessageID} {
		stored, err := f.db.GetMessage(context.Background(), f.groupID, id)
		if err != nil || stored.Content != "before�after" {
			t.Fatal("new work tool validation changed legacy send_message decoding")
		}
	}
}

func TestMemberMCPAuthAndFramingFailClosed(t *testing.T) {
	f := newMemberMCPHTTP(t)
	before := f.counts(t)
	for _, change := range []func(*http.Request){
		func(r *http.Request) { r.Host = "attacker.example" },
		func(r *http.Request) { r.Header.Set("Origin", "http://"+DefaultAddress) },
		func(r *http.Request) { r.Header.Set("Sec-Fetch-Mode", "cors") },
		func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+f.token) },
		func(r *http.Request) { r.URL.RawQuery = "group_id=forged" },
		func(r *http.Request) { r.URL.ForceQuery = true },
		func(r *http.Request) { r.Header.Set("Accept", "application/json") },
		func(r *http.Request) { r.Header.Set("Accept", "application/json, text/event-stream;q=0") },
		func(r *http.Request) { r.Header.Set("MCP-Protocol-Version", "2025-03-26") },
		func(r *http.Request) { r.Header.Add("MCP-Protocol-Version", "2025-06-18") },
		func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
	} {
		r := memberMCPRequest(f, memberMCPInitialize)
		change(r)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code < 400 || w.Code >= 500 {
			t.Fatalf("unsafe HTTP boundary accepted: %d %s", w.Code, w.Body)
		}
	}
	for _, body := range []string{
		`[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`,
		`{"jsonrpc":"2.0","id":null,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1.5,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":9007199254740992,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"ID":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","group_id":"forged"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_message","arguments":{"content":"a","Content":"b"}}}`,
		memberMCPCall("1", "send_message", `[]`),
		memberMCPCall("1", "send_message", `null`),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_message","arguments":{"content":"forged"},"purpose":"chat_intake/v1"}}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"send_message","arguments":{"content":"notification write"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"notifications/initialized"}`,
		memberMCPInitialize + `{}`,
	} {
		w := memberMCPHTTP(t, f, body)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"jsonrpc":"2.0"`) {
			t.Fatalf("invalid frame did not fail closed: %d %s", w.Code, w.Body)
		}
	}
	for _, call := range []string{
		memberMCPCall("1", "send_message", `{"content":"forged","group_id":"other"}`),
		memberMCPCall("2", "send_message", `{"content":"forged","idempotency_key":"chosen"}`),
		memberMCPCall("3", "send_message", `{"content":"not an intake","intent_assessment":{"disposition":"discuss","reason":"untrusted"}}`),
		memberMCPCall("4", "decide_dispatch", `{"kind":"decline","reason":"descriptive role is not authority"}`),
	} {
		memberMCPText(t, memberMCPHTTP(t, f, call), true)
	}
	w := memberMCPHTTP(t, f, `{"jsonrpc":"2.0","id":1,"method":"unknown"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"code":-32601`) {
		t.Fatal("unknown RPC method did not return method-not-found")
	}
	w = memberMCPHTTP(t, f, memberMCPCall("1", "../send_message", `{}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"code":-32602`) {
		t.Fatal("tool dispatch is not a fixed server-side allowlist")
	}
	valid := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	atLimit := valid + strings.Repeat(" ", memberToolJSONBytes-len(valid))
	if memberMCPHTTP(t, f, atLimit).Code != 200 || memberMCPHTTP(t, f, atLimit+" ").Code != 413 {
		t.Fatal("frame cap must include every trailing byte")
	}
	for _, test := range []struct{ token, body string }{
		{"unknown-capability", memberMCPInitialize},
		{f.claim.Token, memberMCPCall("1", "send_message", `{"content":"claim is not a capability"}`)},
	} {
		r := memberMCPRequest(f, test.body)
		r.Header.Set("Authorization", "Bearer "+test.token)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code == 200 {
			if !strings.Contains(memberMCPText(t, w, true), `"code":"forbidden"`) {
				t.Fatal("claim/unknown token gained MCP authority")
			}
		} else if w.Code != 403 {
			t.Fatalf("actual token was not checked: %d %s", w.Code, w.Body)
		}
	}
	if f.counts(t) != before {
		t.Fatal("rejected framing/authorization mutated durable state")
	}
}

func TestMemberMCPEveryRequestRevalidatesClaimAndProfile(t *testing.T) {
	f := newMemberMCPHTTP(t, "read_channel", "search_history", "read_history")
	ctx := context.Background()
	foreign, err := f.db.CreateMessage(ctx, store.MessageRequest{GroupID: f.otherID, ActorID: "user", TopicID: "trusted-topic", Content: "foreign-secret", RecipientIDs: []string{f.foreign.ID}}, []string{f.foreign.ID})
	if err != nil {
		t.Fatal(err)
	}
	denied := memberMCPText(t, memberMCPHTTP(t, f, memberMCPCall("1", "read_history", fmt.Sprintf(`{"message_id":%q}`, foreign.ID))), true)
	if !strings.Contains(denied, `"code":"not_found"`) || strings.Contains(denied, "foreign-secret") {
		t.Fatal("history crossed the token's group")
	}
	list := memberMCPHTTP(t, f, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if strings.Contains(list.Body.String(), `"name":"send_message"`) || strings.Contains(list.Body.String(), `"name":"list_members"`) {
		t.Fatal("listing exposed tools outside the actual capability")
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.otherID, DeliveryID: foreign.Deliveries[0].ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := f.tools.Issue(ctx, store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}, platform.MemberToolGrants{Operations: []string{"read_channel"}})
	if err != nil {
		t.Fatal(err)
	}
	r := memberMCPRequest(f, memberMCPCall("4", "read_channel", `{}`))
	r.Header.Set("Authorization", "Bearer "+otherToken)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	other := memberMCPText(t, w, false)
	if !strings.Contains(other, foreign.ID) || !strings.Contains(other, "foreign-secret") || strings.Contains(other, "initial question") || strings.Contains(other, otherToken) || strings.Contains(other, turn.ClaimToken) {
		t.Fatal("another real capability did not keep its own group/channel identity")
	}
	if _, err := f.db.RequestTurnCancel(ctx, f.claim.GroupID, f.claim.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{memberMCPInitialize, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, memberMCPCall("3", "read_channel", `{}`)} {
		w := memberMCPHTTP(t, f, body)
		if w.Code == 200 {
			if text := memberMCPText(t, w, true); !strings.Contains(text, `"code":"forbidden"`) {
				t.Fatal("expired capability retained a tool call")
			}
		} else if w.Code != 403 {
			t.Fatalf("expired claim bypassed authentication: %d %s", w.Code, w.Body)
		}
	}
}

func TestMemberMCPReadResultEnvelopeBudgetAndCommittedWrite(t *testing.T) {
	f := newMemberMCPHTTP(t)
	ctx := context.Background()
	// Native JSON fits, while quoting it as MCP text exceeds the same wire budget.
	content := strings.Repeat(`"`, 18000)
	message, err := f.db.CreateMessage(ctx, store.MessageRequest{GroupID: f.groupID, ActorID: "user", TopicID: "trusted-topic", Content: content}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := memberMCPHTTP(t, f, memberMCPCall("1", "read_channel", `{"limit":1}`))
	text := memberMCPText(t, w, true)
	if !strings.Contains(text, `"code":"tool_result_too_large"`) || strings.Contains(text, message.ID) {
		t.Fatalf("oversized MCP envelope leaked partial data: %s", text)
	}
	body := memberMCPCall("2", "send_message", `{"content":`+fmt.Sprintf("%q", content)+`}`)
	first := memberMCPHTTP(t, f, body)
	if text := memberMCPText(t, first, false); !strings.Contains(text, `"content_omitted":true`) {
		t.Fatal("a large committed write became an output-size error")
	}
	if replay := memberMCPHTTP(t, f, body); !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("large evidence replay lost its fixed native projection")
	}
}
