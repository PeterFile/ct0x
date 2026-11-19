package httpapi

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

type quoteProjection struct {
	MessageID  string `json:"message_id"`
	GroupID    string `json:"group_id"`
	TopicID    string `json:"topic_id"`
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	SourceKind string `json:"source_kind"`
	Excerpt    string `json:"excerpt"`
}

type quotedMessage struct {
	domain.Message
	Quote *quoteProjection `json:"reply_to"`
}

// Install the exact schema that B will register, without changing the common
// migration registry or recreating old messages and idempotency receipts.
func installQuoteFixture(t *testing.T, raw *sql.DB) {
	t.Helper()
	var installed bool
	if err := raw.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='message_quotes')").Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if installed {
		return // The fixture also works after B registers the migration.
	}
	schema, err := os.ReadFile("../store/message_quote_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
}

func quoteRequest(content, targetID string, recipients []string) string {
	body := map[string]any{"content": content, "reply_to_message_id": targetID}
	if recipients != nil {
		body["recipient_ids"] = recipients
	}
	encoded, _ := json.Marshal(body)
	return string(encoded)
}

func TestMessageHTTPSelectiveQuoteRouting(t *testing.T) {
	a := openMessageAPI(t, "")
	g, other := newGroup(t, a, "group"), newGroup(t, a, "other")
	path := "/api/v1/groups/" + g.ID + "/messages"
	createMember := func(name string) domain.Member {
		return decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+g.ID+"/members", `{"name":"`+name+`","role":"分析"}`), 201)
	}
	earliest, author := createMember("成员甲"), createMember("成员乙")
	raw := messageSQL(t, a)
	if _, err := raw.Exec("UPDATE members SET created_at='2026-01-01T00:00:00.000000000Z' WHERE id=?", earliest.ID); err != nil {
		t.Fatal(err)
	}
	// Both the old source row and a no-quote idempotency receipt predate the quote schema.
	old, err := a.db.CreateMessage(t.Context(), store.MessageRequest{
		GroupID: g.ID, TopicID: "old-topic", ActorID: author.ID, Content: "  只看本季度\n" + strings.Repeat("🙂", 130),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plainBody := `{"content":"无引用，直接接话"}`
	plain := a.request("POST", path, plainBody, "plain")
	adjacent := decoded[quotedMessage](t, plain, 201)
	if adjacent.Quote != nil || len(adjacent.Deliveries) != 1 || adjacent.Deliveries[0].RecipientID != earliest.ID {
		t.Fatalf("unquoted default routing changed: %+v", adjacent)
	}
	foreign := decoded[domain.Message](t, a.request("POST", "/api/v1/groups/"+other.ID+"/messages", `{"content":"foreign-origin","recipient_ids":[]}`), 201)
	installQuoteFixture(t, raw)

	want := &quoteProjection{MessageID: old.ID, GroupID: g.ID, TopicID: old.TopicID, SenderID: author.ID,
		SenderName: author.Name, SourceKind: "unknown", Excerpt: "只看本季度 " + strings.Repeat("🙂", 114) + "…"}
	body := quoteRequest("那去年同期呢？", old.ID, nil)
	first := a.request("POST", path, body, "quote")
	quoted := decoded[quotedMessage](t, first, 201)
	if !reflect.DeepEqual(quoted.Quote, want) || quoted.SourceKind != "local_user_chat" || quoted.TopicID != "" ||
		len(quoted.Deliveries) != 1 || quoted.Deliveries[0].RecipientID != author.ID || quoted.Deliveries[0].State != "queued" {
		t.Fatalf("quoted receipt did not bind original facts and sole author delivery: %+v quote=%+v", quoted, quoted.Quote)
	}
	list := decoded[listResult[quotedMessage]](t, a.request("GET", path+"?limit=1", ""), 200)
	if len(list.Items) != 1 || list.Items[0].ID != quoted.ID || !reflect.DeepEqual(list.Items[0].Quote, want) || list.NextCursor == "" {
		t.Fatalf("latest page lost an out-of-page quote: %+v", list)
	}
	tail := decoded[listResult[quotedMessage]](t, a.request("GET", path+"?limit=2&before="+list.NextCursor, ""), 200)
	if len(tail.Items) != 2 || tail.Items[0].ID != adjacent.ID || tail.Items[0].Quote != nil || tail.Items[1].ID != old.ID || tail.Items[1].Quote != nil {
		t.Fatalf("legacy continuation changed: %+v", tail)
	}
	detail := decoded[quotedMessage](t, a.request("GET", path+"/"+quoted.ID, ""), 200)
	original := decoded[domain.Message](t, a.request("GET", path+"/"+old.ID, ""), 200)
	if !reflect.DeepEqual(detail.Quote, want) || original.Content != old.Content {
		t.Fatalf("detail failed to read quote and exact original: %+v %+v", detail, original)
	}

	for _, tc := range []struct {
		key, recipient string
		ids            []string
	}{
		{"other", earliest.ID, []string{earliest.ID}},
		{"explicit-author", author.ID, []string{author.ID, author.ID}},
		{"public", "", []string{}},
	} {
		got := decoded[quotedMessage](t, a.request("POST", path, quoteRequest(tc.key, old.ID, tc.ids), tc.key), 201)
		if !reflect.DeepEqual(got.Quote, want) || (tc.recipient == "" && len(got.Deliveries) != 0) ||
			(tc.recipient != "" && (len(got.Deliveries) != 1 || got.Deliveries[0].RecipientID != tc.recipient)) {
			t.Fatalf("%s changed quote or explicit routing: %+v", tc.key, got)
		}
	}
	for _, id := range []string{foreign.ID, "missing"} {
		w := a.request("POST", path, quoteRequest("rejected", id, []string{author.ID}), "still-unused")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	for _, change := range []string{`{"content":"那去年同期呢？"}`, quoteRequest("那去年同期呢？", adjacent.ID, nil), quoteRequest("那去年同期呢？", old.ID, []string{})} {
		w := a.request("POST", path, change, "quote")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
	}
	if _, err := raw.Exec("UPDATE members SET status='left' WHERE id=?", author.ID); err != nil {
		t.Fatal(err)
	}
	left := decoded[quotedMessage](t, a.request("POST", path, quoteRequest("引用离群成员", old.ID, nil), "still-unused"), 201)
	if !reflect.DeepEqual(left.Quote, want) || len(left.Deliveries) != 0 {
		t.Fatalf("left author must stay readable without a fallback wake: %+v", left)
	}
	self := decoded[quotedMessage](t, a.request("POST", path, quoteRequest("引用用户", adjacent.ID, nil), "user-origin"), 201)
	if self.Quote == nil || self.Quote.SourceKind != "local_user_chat" || len(self.Deliveries) != 0 {
		t.Fatalf("a non-Agent quote fell back to another member: %+v", self)
	}
	for table, n := range map[string]int{"tasks": 0, "messages": 9, "message_quotes": 6, "deliveries": 4, "message_command_sources": 8} {
		var count int
		if err := raw.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != n {
			t.Fatalf("%s: got %d want %d: %v", table, count, n, err)
		}
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openMessageAPI(t, a.path)
	for _, tc := range []struct{ key, body, want string }{{"quote", body, first.Body.String()}, {"plain", plainBody, plain.Body.String()}} {
		w := reopened.request("POST", path, tc.body, tc.key)
		if w.Code != 201 || !bytes.Equal(w.Body.Bytes(), []byte(tc.want)) {
			t.Fatalf("%s replay changed on reopen: %d %s", tc.key, w.Code, w.Body)
		}
	}
}

func TestMemberToolHTTPSelectiveQuoteScope(t *testing.T) {
	f := newMemberToolHTTP(t, "send_message", "read_channel")
	seed := func(groupID, topicID, actorID, content string) domain.Message {
		t.Helper()
		m, err := f.db.CreateMessage(t.Context(), store.MessageRequest{GroupID: groupID, TopicID: topicID, ActorID: actorID, Content: content}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	origin := seed(f.groupID, "trusted-topic", f.peer.ID, "只看本季度")
	foreignTopic := seed(f.groupID, "other-topic", f.peer.ID, "hidden-topic-text")
	foreignGroup := seed(f.otherID, "trusted-topic", f.foreign.ID, "hidden-group-text")
	installQuoteFixture(t, f.raw)
	body := quoteRequest("针对旧说法补充", origin.ID, nil)
	first := f.request(t, "send_message", body, "quoted")
	receipt := decoded[memberToolHTTPSendResult](t, first, 201)
	channel := decoded[listResult[quotedMessage]](t, f.request(t, "read_channel", `{}`), 200)
	got := channel.Items[0]
	if got.ID != receipt.MessageID || got.SenderID != f.caller.ID || got.GroupID != f.groupID || got.TopicID != "trusted-topic" ||
		got.SourceKind != "member" || receipt.DeliveryCount != 0 || got.Quote == nil || got.Quote.MessageID != origin.ID || got.Quote.SenderID != f.peer.ID {
		t.Fatalf("claimed send or bound channel read lost the original: %+v quote=%+v", got, got.Quote)
	}
	before := f.counts(t)
	if w := f.request(t, "send_message", body, "quoted"); w.Code != 201 || !bytes.Equal(w.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("member quote replay: %d %s", w.Code, w.Body)
	}
	w := f.request(t, "send_message", `{"content":"针对旧说法补充"}`, "quoted")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
	for _, id := range []string{foreignTopic.ID, foreignGroup.ID, "missing"} {
		w := f.request(t, "send_message", quoteRequest("越界", id, nil), "rejected")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
	}
	for _, extra := range []string{`"group_id":"foreign"`, `"topic_id":"other-topic"`, `"sender_id":"user"`, `"grants":{}`} {
		w := f.request(t, "send_message", strings.TrimSuffix(body, "}")+","+extra+"}", "rejected")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
	}
	if f.counts(t) != before {
		t.Fatal("replay or rejected sends changed durable state")
	}
	adjacent := decoded[memberToolHTTPSendResult](t, f.request(t, "send_message", `{"content":"直接接话"}`, "adjacent"), 201)
	plain, err := f.db.GetMessage(t.Context(), f.groupID, adjacent.MessageID)
	if err != nil || plain.ReplyTo != nil || plain.SourceKind != "member" || adjacent.DeliveryCount != 0 {
		t.Fatalf("a plain adjacent reply gained a quote or wake: %+v %v", plain, err)
	}
	local := platform.NewLocalUser(f.db)
	for _, tc := range []struct {
		original domain.Message
		visible  bool
	}{{origin, true}, {foreignTopic, false}} {
		trigger, err := local.CreateMessage(t.Context(), f.groupID, platform.MessageInput{TopicID: "trusted-topic", Content: "查原文",
			RecipientIDs: []string{f.caller.ID}, ReplyToMessageID: tc.original.ID}, "")
		if err != nil || trigger.ReplyTo == nil || len(trigger.Deliveries) != 1 {
			t.Fatalf("local trigger: %+v %v", trigger, err)
		}
		deliveryID := trigger.Deliveries[0].ID
		quote, err := f.db.ReadDeliveryQuote(t.Context(), f.groupID, deliveryID)
		if err != nil || (quote != nil) != tc.visible || quote != nil && (quote.MessageID != tc.original.ID || quote.Content != tc.original.Content) {
			t.Fatalf("delivery quote: %+v, %v", quote, err)
		}
		if quote, err := f.db.ReadDeliveryQuote(t.Context(), f.otherID, deliveryID); err != nil || quote != nil {
			t.Fatalf("wrong group resolved a delivery quote: %+v %v", quote, err)
		}
		latest := f.request(t, "read_channel", `{"limit":1}`)
		read := decoded[listResult[quotedMessage]](t, latest, 200).Items[0]
		if read.ID != trigger.ID || (read.Quote != nil) != tc.visible || strings.Contains(latest.Body.String(), "hidden-topic-text") {
			t.Fatalf("scoped page disclosed a reference outside its topic: %s", latest.Body)
		}
	}
	if _, err := f.raw.Exec("UPDATE turns SET cancel_requested_at=created_at WHERE id=?", f.claim.TurnID); err != nil {
		t.Fatal(err)
	}
	w = f.request(t, "send_message", body, "quoted")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 403, "forbidden")
}

func TestMessageHTTPQuoteRollsBackWithSource(t *testing.T) {
	a := openMessageAPI(t, "")
	g := newGroup(t, a, "group")
	path := "/api/v1/groups/" + g.ID + "/messages"
	author := decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+g.ID+"/members", `{"name":"成员","role":"分析"}`), 201)
	origin, err := a.db.CreateMessage(t.Context(), store.MessageRequest{GroupID: g.ID, ActorID: author.ID, Content: "原文"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := messageSQL(t, a)
	installQuoteFixture(t, raw)
	for _, value := range []string{`null`, `""`, `{}`, `"\ud800"`} {
		w := a.request("POST", path, `{"content":"未保存","reply_to_message_id":`+value+`}`, "retry")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
	}
	if _, err := raw.Exec(`CREATE TRIGGER fail_quote_source BEFORE INSERT ON message_command_sources
	 BEGIN SELECT RAISE(ABORT, 'fixture source failure'); END`); err != nil {
		t.Fatal(err)
	}
	body := quoteRequest("必须原子提交", origin.ID, nil)
	w := a.request("POST", path, body, "retry")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 500, "internal_error")
	for table, want := range map[string]int{"messages": 1, "message_log": 1, "message_quotes": 0, "deliveries": 0,
		"conversations": 0, "message_command_sources": 0, "mutation_idempotency": 0} {
		var count int
		query := "SELECT count(*) FROM " + table
		if table == "mutation_idempotency" {
			query += " WHERE operation='create_message'"
		}
		if err := raw.QueryRow(query).Scan(&count); err != nil || count != want {
			t.Fatalf("failed transaction wrote %s: %d want %d: %v", table, count, want, err)
		}
	}
	if _, err := raw.Exec("DROP TRIGGER fail_quote_source"); err != nil {
		t.Fatal(err)
	}
	stored := decoded[quotedMessage](t, a.request("POST", path, body, "retry"), 201)
	if stored.Quote == nil || len(stored.Deliveries) != 1 || stored.SourceID == "" {
		t.Fatalf("retry did not commit the complete operation: %+v", stored)
	}
}

func TestMemberToolHTTPQuoteCannotBeDiscardedByAssessment(t *testing.T) {
	f := chatHTTPSetup(t)
	before := f.counts(t)
	for _, body := range []string{chatHTTPBody(""), `{"content":"继续。","intent_assessment":{"disposition":"continue_work","reason":"请续办"}}`} {
		w := f.request(t, "send_message", strings.Replace(body, `"content":`, `"reply_to_message_id":"some-message","content":`, 1), "quote-assess")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	if f.counts(t) != before {
		t.Fatal("rejected assessment with quote had side effects")
	}
	// An actual platform notice may carry the member ID of the assessing run.
	// It is readable as an origin but must not be routed as that member's speech.
	admitted := decoded[memberToolHTTPSendResult](t, f.request(t, "send_message", chatHTTPBody(""), "plain-assess"), 201)
	installQuoteFixture(t, f.raw)
	local := platform.NewLocalUser(f.db)
	reply, err := local.CreateMessage(t.Context(), f.groupID, platform.MessageInput{
		Content: "引用系统通知", TopicID: "trusted-topic", ReplyToMessageID: admitted.MessageID,
	}, "quote-notice")
	if err != nil || reply.ReplyTo == nil || reply.ReplyTo.SenderID != f.caller.ID || reply.ReplyTo.SourceKind != "platform" || len(reply.Deliveries) != 0 {
		t.Fatalf("a platform notice was treated as member speech: %+v, %v", reply, err)
	}
}
