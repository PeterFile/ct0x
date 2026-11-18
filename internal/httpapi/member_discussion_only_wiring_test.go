package httpapi

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestMemberIntakeDiscussionOnlyExcerpt(t *testing.T) {
	f := chatHTTPSetup(t)
	ctx := t.Context()
	// A new genuine local-user source has text whose bytes must not be normalized.
	f.tools.Revoke(f.token)
	if _, err := f.db.ApplyTurnReceipt(ctx, f.claim, provider.ExecutionReceipt{Outcome: provider.OutcomeNotStarted, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	const prefix = " \n只讨论🙂，不要开工。保留 e\u0301 和 �。\n"
	exact := prefix + strings.Repeat(" ", 4096-len(prefix))
	root, err := platform.NewLocalUser(f.db).CreateMessage(ctx, f.groupID, platform.MessageInput{
		TopicID: "trusted-topic", Content: "现在：" + exact + "完", RecipientIDs: []string{f.caller.ID, f.peer.ID},
	}, "exact-discuss-source")
	if err != nil || len(root.Deliveries) != 2 {
		t.Fatal("could not persist discussion source", err)
	}
	bind := func(d domain.Delivery) {
		t.Helper()
		turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: d.ID, MaxActive: 2})
		if err != nil {
			t.Fatal(err)
		}
		f.claim = store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
		f.token, err = f.tools.IssueChatIntake(ctx, f.claim)
		if err != nil {
			t.Fatal(err)
		}
	}
	bind(root.Deliveries[0])
	f.handler = mustServer(t, DefaultAddress, append(f.routes, MemberMCPRoutes(f.tools)...)...).Handler
	const plain = `{"content":"先讨论。","intent_assessment":{"disposition":"discuss","reason":"解释当前范围"`
	quote := func(raw string) string { return plain + `,"discussion_only_excerpt":` + raw + `}}` }
	quoted, _ := json.Marshal(exact)
	malformed := quote(strings.Replace(string(quoted), "�", `\ud800`, 1))
	before := f.counts(t)
	w := f.request(t, "send_message", malformed, "malformed-excerpt")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusBadRequest, "invalid_json")
	for _, invalid := range []string{quote(`null`), quote(`"  \u0085"`), quote(`"不存在的原文"`)} {
		w := f.request(t, "send_message", invalid, "invalid-excerpt")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), http.StatusBadRequest, "invalid_input")
	}
	for _, call := range []string{memberMCPCall(`"discuss"`, "send_message", malformed), memberMCPCall(`"\ud800"`, "send_message", quote(string(quoted)))} {
		w := memberMCPHTTP(t, f, call)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":-32602`) {
			t.Fatal("MCP accepted malformed scalar text or ID")
		}
	}
	if f.counts(t) != before {
		t.Fatal("invalid discussion excerpt changed durable state")
	}
	const list = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if list := memberMCPHTTP(t, f, list); list.Code != 200 || !strings.Contains(list.Body.String(), `"discussion_only_excerpt"`) {
		t.Fatal("issued intake omitted the exact discussion option")
	}
	body := quote(strings.Replace(string(quoted), "🙂", `\ud83d\ude42`, 1))
	text := memberMCPText(t, memberMCPHTTP(t, f, memberMCPCall(`"discuss"`, "send_message", body)), false)
	var out struct {
		Receipt chatIntentToolReceipt `json:"intent_assessment"`
	}
	if json.Unmarshal([]byte(text), &out) != nil || out.Receipt.ID == "" {
		t.Fatal("missing discussion receipt")
	}
	raw, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	var recorded []byte
	var receipt domain.ChatAssessmentReceipt
	if err := raw.QueryRowContext(ctx, "SELECT receipt_json FROM chat_assessments WHERE id=?", out.Receipt.ID).Scan(&recorded); err != nil ||
		json.Unmarshal(recorded, &receipt) != nil || !receipt.DiscussionOnly || receipt.DiscussionOnlyExcerpt != exact ||
		receipt.MessageID != root.ID || receipt.SourceID != root.SourceID || receipt.TurnID != f.claim.TurnID || receipt.TaskID != "" {
		t.Fatal("did not retain the exact bound discussion evidence")
	}
	if replay := memberMCPText(t, memberMCPHTTP(t, f, memberMCPCall(`"discuss"`, "send_message", quote(string(quoted)))), false); replay != text {
		t.Fatal("equivalent JSON scalar spelling did not replay")
	}
	f.tools.Revoke(f.token)
	bind(root.Deliveries[1])
	w = f.request(t, "send_message", chatHTTPBody(""), "other-recipient-cannot-admit")
	var denied struct {
		Receipt domain.ChatAssessmentReceipt `json:"intent_assessment"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &denied) != nil || denied.Receipt.ReasonCode != "discussion_only" || denied.Receipt.TaskID != "" {
		t.Fatal("the second recipient escaped the recorded discussion restriction")
	}
}
