package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func TestMemberHistoryToolHTTPExplicitOptIn(t *testing.T) {
	f := newMemberToolHTTP(t, "list_members", "read_channel", "send_message", "create_member", "search_history", "read_history")
	if len(f.routes) != 4 {
		t.Fatalf("ordinary profile changed: %d routes", len(f.routes))
	}
	history := MemberHistoryToolRoutes(f.tools)
	if len(history) != 2 {
		t.Fatalf("history opt-in has %d routes, want two", len(history))
	}
	for i, operation := range []string{"search_history", "read_history"} {
		if history[i].Method != http.MethodPost || history[i].Pattern != "/api/v1/member-tools/"+operation {
			t.Fatalf("unexpected opt-in route: %+v", history[i])
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, f.newRequest(operation, `{}`))
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	f.routes = append(f.routes, history...)
	f.handler = mustServer(t, DefaultAddress, f.routes...).Handler
	before := memberHistoryToolState(t, f)
	decoded[listResult[json.RawMessage]](t, f.request(t, "list_members", `{}`), 200)
	decoded[listResult[json.RawMessage]](t, f.request(t, "read_channel", `{}`), 200)
	// Read-only history ignores even malformed duplicate write keys.
	page := decoded[platform.MemberHistorySearchResult](t, f.request(t, "search_history", `{"query":"initial"}`, "has space", "duplicate"), 200)
	decoded[platform.MemberHistoryReadResult](t, f.request(t, "read_history", fmt.Sprintf(`{"message_id":%q}`, page.Items[0].MessageID), "has space", "duplicate"), 200)
	if memberHistoryToolState(t, f) != before {
		t.Fatal("opt-in reads created receipts or other durable effects")
	}
}

func TestMemberHistoryToolHTTPPaginationPreservesStoreCursor(t *testing.T) {
	f := newMemberHistoryToolHTTP(t)
	for i := range 53 {
		memberHistoryToolMessage(t, f, fmt.Sprintf("paged-history-%03d", i), f.groupID, "trusted-topic", "needle 中😀")
	}
	before := memberHistoryToolState(t, f)
	first := f.request(t, "search_history", `{"query":" \u3000needle\n"}`)
	page := decoded[platform.MemberHistorySearchResult](t, first, 200)
	if len(page.Items) != 20 || page.NextCursor == "" {
		t.Fatalf("omission did not preserve default 20: %+v", page)
	}
	emptyBefore := f.request(t, "search_history", `{"query":"needle","before":""}`)
	if !bytes.Equal(first.Body.Bytes(), emptyBefore.Body.Bytes()) {
		t.Fatal("empty before or normalized query changed the first page")
	}
	native, err := f.tools.SearchHistory(context.Background(), f.token, platform.MemberHistorySearchInput{Query: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(native)
	if err != nil || !bytes.Equal(first.Body.Bytes(), append(encoded, '\n')) {
		t.Fatalf("HTTP changed the Store DTO or opaque cursor: %v", err)
	}
	maximum := decoded[platform.MemberHistorySearchResult](t, f.request(t, "search_history", `{"query":"needle","limit":50}`), 200)
	if len(maximum.Items) != 50 || maximum.NextCursor == "" {
		t.Fatal("explicit maximum limit was not preserved")
	}
	for offset := 0; ; {
		end := min(offset+20, 53)
		if len(page.Items) != end-offset {
			t.Fatalf("unexpected page size at %d: %d", offset, len(page.Items))
		}
		for i, hit := range page.Items {
			if hit.MessageID != fmt.Sprintf("paged-history-%03d", 52-offset-i) || hit.Excerpt.Content != "needle 中😀" || !hit.Excerpt.ContentComplete {
				t.Fatalf("pagination skipped, duplicated or changed a hit: %+v", hit)
			}
		}
		if end == 53 {
			if page.NextCursor != "" {
				t.Fatal("final page claims more results")
			}
			break
		}
		if page.NextCursor == "" {
			t.Fatal("intermediate page omitted its cursor")
		}
		body, _ := json.Marshal(map[string]string{"query": "\tneedle", "before": page.NextCursor})
		page = decoded[platform.MemberHistorySearchResult](t, f.request(t, "search_history", string(body)), 200)
		offset = end
	}
	empty := f.request(t, "search_history", `{"query":"absent"}`)
	if empty.Code != 200 || empty.Body.String() != "{\"items\":[],\"next_cursor\":\"\"}\n" {
		t.Fatalf("empty page projection changed: %d %s", empty.Code, empty.Body)
	}
	if memberHistoryToolState(t, f) != before {
		t.Fatal("pagination acknowledged delivery or created durable work")
	}
}

func TestMemberHistoryToolHTTPUnicodeFragmentReassembly(t *testing.T) {
	f := newMemberHistoryToolHTTP(t)
	body := " \tUNICODE-MATCH " + strings.Repeat("中😀e\u0301<>&\x00\x01\n\\\"", 4096) + " \n"
	if len(body) <= 65536 {
		t.Fatal("fixture must exceed one complete tool response")
	}
	memberHistoryToolMessage(t, f, "unicode-evidence", f.groupID, "trusted-topic", body)
	late := strings.Repeat("界😀", 500) + "MATCH-LATER"
	memberHistoryToolMessage(t, f, "late-evidence", f.groupID, "trusted-topic", late)
	before := memberHistoryToolState(t, f)
	runes := []rune(body)
	page := decoded[platform.MemberHistorySearchResult](t, f.request(t, "search_history", `{"query":"UNICODE-MATCH"}`), 200)
	if len(page.Items) != 1 {
		t.Fatalf("Unicode search: %+v", page)
	}
	excerpt := page.Items[0].Excerpt
	if page.Items[0].MessageID != "unicode-evidence" || excerpt.Offset != 0 || excerpt.NextOffset != 256 || excerpt.TotalRunes != len(runes) || excerpt.Content != string(runes[:256]) || excerpt.End || excerpt.ContentComplete {
		t.Fatalf("search lost its explicit prefix semantics: %+v", excerpt)
	}
	latePage := decoded[platform.MemberHistorySearchResult](t, f.request(t, "search_history", `{"query":"MATCH-LATER"}`), 200)
	if len(latePage.Items) != 1 || latePage.Items[0].Excerpt.Content != string([]rune(late)[:256]) || latePage.Items[0].Excerpt.ContentComplete || strings.Contains(latePage.Items[0].Excerpt.Content, "MATCH-LATER") {
		t.Fatal("late match was misrepresented as a centered excerpt or complete message")
	}
	defaultFragment := decoded[platform.MemberHistoryReadResult](t, f.request(t, "read_history", `{"message_id":"unicode-evidence"}`), 200)
	if defaultFragment.Offset != 0 || defaultFragment.NextOffset != 1024 || defaultFragment.Content != string(runes[:1024]) || defaultFragment.End || defaultFragment.ContentComplete {
		t.Fatal("omitted offset/length did not preserve the Store defaults")
	}
	var reconstructed strings.Builder
	for offset := 0; offset < len(runes); {
		input := fmt.Sprintf(`{"message_id":"unicode-evidence","offset":%d,"length":4096}`, offset)
		fragment := decoded[platform.MemberHistoryReadResult](t, f.request(t, "read_history", input), 200)
		next := min(offset+4096, len(runes))
		if fragment.MessageID != "unicode-evidence" || fragment.Offset != offset || fragment.NextOffset != next || fragment.TotalRunes != len(runes) || fragment.Content != string(runes[offset:next]) || fragment.End != (next == len(runes)) || fragment.ContentComplete {
			t.Fatalf("incorrect exact rune fragment at %d: %+v", offset, fragment)
		}
		reconstructed.WriteString(fragment.Content)
		offset = fragment.NextOffset
	}
	if reconstructed.String() != body {
		t.Fatal("HTTP fragments corrupted original Unicode, NUL, whitespace or escapes")
	}
	end := decoded[platform.MemberHistoryReadResult](t, f.request(t, "read_history", fmt.Sprintf(`{"message_id":"unicode-evidence","offset":%d,"length":1}`, len(runes))), 200)
	if end.Offset != len(runes) || end.NextOffset != len(runes) || end.Content != "" || !end.End || end.ContentComplete {
		t.Fatalf("end fragment changed: %+v", end)
	}
	if memberHistoryToolState(t, f) != before {
		t.Fatal("fragment reads consumed delivery, changed claims or created work")
	}
}

func TestMemberHistoryToolHTTPScopeIsolationAndForeignCursor(t *testing.T) {
	f := newMemberHistoryToolHTTP(t)
	ctx := context.Background()
	issue := func(groupID, topicID, memberID string) string {
		t.Helper()
		message, err := f.db.CreateMessage(ctx, store.MessageRequest{GroupID: groupID, ActorID: "user", TopicID: topicID, Content: "scope trigger", RecipientIDs: []string{memberID}}, []string{memberID})
		if err != nil {
			t.Fatal(err)
		}
		turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 3, ResourceKeys: []string{"history-fixture/" + memberID}})
		if err != nil {
			t.Fatal(err)
		}
		token, err := f.tools.Issue(ctx, store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}, platform.MemberToolGrants{Operations: []string{"search_history", "read_history"}})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	scopes := []struct{ name, token, groupID, topicID, cursor string }{
		{name: "bound-topic", token: f.token, groupID: f.groupID, topicID: "trusted-topic"},
		{name: "main-topic", token: issue(f.groupID, "", f.peer.ID), groupID: f.groupID},
		{name: "foreign-group", token: issue(f.otherID, "trusted-topic", f.foreign.ID), groupID: f.otherID, topicID: "trusted-topic"},
	}
	for _, scope := range scopes {
		for i := range 2 {
			memberHistoryToolMessage(t, f, fmt.Sprintf("%s-%d", scope.name, i), scope.groupID, scope.topicID, "scope-needle PRIVATE-"+scope.name)
		}
	}
	request := func(token, operation, body string) *httptest.ResponseRecorder {
		r := f.newRequest(operation, body)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
		if bytes.Contains(w.Body.Bytes(), []byte(token)) {
			t.Fatal("response exposed a scope capability")
		}
		return w
	}
	before := memberHistoryToolState(t, f)
	for i := range scopes {
		scope := &scopes[i]
		page := decoded[platform.MemberHistorySearchResult](t, request(scope.token, "search_history", `{"query":"scope-needle","limit":1}`), 200)
		if len(page.Items) != 1 || page.Items[0].MessageID != scope.name+"-1" || page.NextCursor == "" {
			t.Fatalf("scope %s leaked or widened: %+v", scope.name, page)
		}
		native, err := f.tools.SearchHistory(ctx, scope.token, platform.MemberHistorySearchInput{Query: "scope-needle", Limit: 1})
		if err != nil || native.NextCursor != page.NextCursor {
			t.Fatalf("scope %s wrapped the native cursor: %v", scope.name, err)
		}
		scope.cursor = native.NextCursor
		fragment := decoded[platform.MemberHistoryReadResult](t, request(scope.token, "read_history", fmt.Sprintf(`{"message_id":%q}`, scope.name+"-1")), 200)
		if fragment.Content != "scope-needle PRIVATE-"+scope.name || !fragment.ContentComplete {
			t.Fatalf("scope %s could not read its own exact record", scope.name)
		}
	}
	for i, scope := range scopes {
		body, _ := json.Marshal(map[string]string{"query": "scope-needle", "before": scope.cursor})
		tail := decoded[platform.MemberHistorySearchResult](t, request(scope.token, "search_history", string(body)), 200)
		if len(tail.Items) != 1 || tail.Items[0].MessageID != scope.name+"-0" || tail.NextCursor != "" {
			t.Fatalf("scope %s could not reuse its native cursor: %+v", scope.name, tail)
		}
		for j, other := range scopes {
			if i == j {
				continue
			}
			body, _ := json.Marshal(map[string]string{"query": "scope-needle", "before": other.cursor})
			w := request(scope.token, "search_history", string(body))
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
			w = request(scope.token, "read_history", fmt.Sprintf(`{"message_id":%q}`, other.name+"-1"))
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
		}
		body, _ = json.Marshal(map[string]string{"query": "different-query", "before": scope.cursor})
		w := request(scope.token, "search_history", string(body))
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		w = request(scope.token, "read_history", `{"message_id":"missing"}`)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	if memberHistoryToolState(t, f) != before {
		t.Fatal("scoped reads changed durable delivery, task or claim authority")
	}
}
