package store_test

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/httpapi"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

var discussionProfile = store.BackgroundDispatchProfile{ProviderID: "approved-pi", Model: "verified-model"}

func backgroundOptions() store.BackgroundDispatchOptions {
	return store.BackgroundDispatchOptions{
		MaxActiveMembers: 2,
		ReadyProfiles:    []store.BackgroundDispatchProfile{discussionProfile},
		Create:           &store.BackgroundDispatchTemplate{ID: "text-helper-v1", Name: "讨论助手", Role: "纯文本讨论", Profile: discussionProfile},
	}
}

type backgroundFixture struct {
	db    *store.Store
	raw   *sql.DB
	local *platform.LocalUser
	path  string
}

func newBackgroundFixture(t *testing.T, path string, enabled bool) backgroundFixture {
	t.Helper()
	if path == "" {
		path = filepath.Join(t.TempDir(), "workspace.sqlite")
	}
	db := store.OpenGroupBackgroundFixture(t, path)
	if enabled {
		if err := db.EnableGroupBackgroundDispatch(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"}
	raw, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	return backgroundFixture{db, raw, platform.NewLocalUser(db), path}
}

func (f backgroundFixture) group(t *testing.T, key string) domain.Group {
	t.Helper()
	g, err := f.local.CreateGroup(context.Background(), platform.GroupInput{Name: "测试群"}, key)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func (f backgroundFixture) post(t *testing.T, group, key string, input platform.MessageInput) domain.Message {
	t.Helper()
	m, err := f.local.CreateMessage(context.Background(), group, input, key)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f backgroundFixture) pass(t *testing.T, options store.BackgroundDispatchOptions) {
	t.Helper()
	if err := f.db.RunGroupBackgroundDispatch(context.Background(), options); err != nil {
		t.Fatal(err)
	}
}

func (f backgroundFixture) assertCount(t *testing.T, want int, query string, args ...any) {
	t.Helper()
	var got int
	if err := f.raw.QueryRow(query, args...).Scan(&got); err != nil || got != want {
		t.Fatalf("%s = %d, %v; want %d", query, got, err, want)
	}
}

func (f backgroundFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.raw.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

type backgroundPage[T any] struct {
	Items []T `json:"items"`
}

func backgroundHTTP[T any](t *testing.T, handler http.Handler, method, path, body, key string, status int) T {
	t.Helper()
	r := httptest.NewRequest(method, "http://"+httpapi.DefaultAddress+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var result T
	if w.Code != status || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("%s %s: got %d %s; want %d", method, path, w.Code, w.Body, status)
	}
	return result
}

func backgroundHandler(t *testing.T, f backgroundFixture) http.Handler {
	t.Helper()
	routes := append(httpapi.LocalUserRoutes(f.local), httpapi.MessageRoutes(f.local)...)
	server, err := httpapi.NewServer(httpapi.DefaultAddress, routes...)
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler
}

func TestGroupBackgroundDispatchHTTP(t *testing.T) {
	f := newBackgroundFixture(t, "", true)
	const groups = "/api/v1/groups"
	h := backgroundHandler(t, f)
	backgroundHTTP[any](t, h, "POST", groups, `{"name":"伪造","background_dispatch":true}`, "", 400)
	g := backgroundHTTP[domain.Group](t, h, "POST", groups, `{"name":"空群"}`, "group", 201)
	members := groups + "/" + g.ID + "/members"
	messages := groups + "/" + g.ID + "/messages"
	if empty := backgroundHTTP[backgroundPage[domain.Member]](t, h, "GET", members, "", "", 200); empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("new group is not empty: %+v", empty)
	}
	backgroundHTTP[any](t, h, "POST", messages, `{"content":"伪造","source_kind":"local_user_chat"}`, "", 400)
	public := backgroundHTTP[domain.Message](t, h, "POST", messages, `{"content":"仅公开 @其他人","topic_id":"讨论","recipient_ids":[]}`, "public", 201)
	const firstBody = `{"content":"@其他人 一起讨论？","topic_id":"讨论"}`
	first := backgroundHTTP[domain.Message](t, h, "POST", messages, firstBody, "first", 201)
	if first.SenderID != "user" || first.SourceKind != "local_user_chat" || first.SourceID == "" || first.TopicID != "讨论" || len(first.Deliveries) != 0 || len(public.Deliveries) != 0 {
		t.Fatalf("unexpected initial receipt: %+v / %+v", first, public)
	}
	if roster := backgroundHTTP[backgroundPage[domain.Member]](t, h, "GET", members, "", "", 200); len(roster.Items) != 0 {
		t.Fatalf("POST created a member before the pass: %+v", roster)
	}
	cfg := backgroundOptions()
	cfg.Presets = []store.BackgroundDispatchTemplate{
		{ID: "offline", Name: "其他人", Role: "不就绪", Profile: store.BackgroundDispatchProfile{ProviderID: discussionProfile.ProviderID, Model: "unverified-model"}},
		{ID: "text-preset-v1", Name: "预设成员", Role: "讨论", Profile: discussionProfile},
	}
	f.pass(t, cfg)
	roster := backgroundHTTP[backgroundPage[domain.Member]](t, h, "GET", members, "", "", 200)
	if len(roster.Items) != 1 || roster.Items[0].Name != "预设成员" {
		t.Fatalf("did not add exactly the ready approved preset: %+v", roster)
	}
	m := roster.Items[0]
	if m.CreatorID != "system" || m.OwnerID != "user" || m.GroupID != g.ID || m.Workspace != "" || m.ProviderID != discussionProfile.ProviderID || m.Model != discussionProfile.Model {
		t.Fatalf("unbound or overprivileged member: %+v", m)
	}
	// A suitable existing member wins even over an available, different preset.
	cfg.Presets[1].ID = "another-ready-preset"
	second := backgroundHTTP[domain.Message](t, h, "POST", messages, `{"content":"下一句","topic_id":"讨论"}`, "second", 201)
	if len(second.Deliveries) != 0 {
		t.Fatalf("bound default routed inline: %+v", second)
	}
	explicit := backgroundHTTP[domain.Message](t, h, "POST", messages, fmt.Sprintf(`{"content":"明确接收者","topic_id":"讨论","recipient_ids":[%q]}`, m.ID), "explicit", 201)
	if len(explicit.Deliveries) != 1 || explicit.Deliveries[0].RecipientID != m.ID {
		t.Fatalf("explicit delivery not persisted atomically: %+v", explicit)
	}
	f.pass(t, cfg)
	for _, id := range []string{first.ID, second.ID} {
		got := backgroundHTTP[domain.Message](t, h, "GET", messages+"/"+id, "", "", 200)
		if len(got.Deliveries) != 1 || got.Deliveries[0].MessageID != id || got.Deliveries[0].RecipientID != m.ID || got.Deliveries[0].State != domain.DeliveryStateQueued {
			t.Fatalf("wrong delivery on original message: %+v", got)
		}
	}
	if got := backgroundHTTP[domain.Message](t, h, "POST", messages, firstBody, "first", 201); !reflect.DeepEqual(got, first) {
		t.Fatalf("POST replay did not return original receipt: %+v; want %+v", got, first)
	}
	backgroundHTTP[any](t, h, "POST", messages, `{"content":"@其他人 一起讨论？","topic_id":"讨论","recipient_ids":[]}`, "first", 409)
	page := backgroundHTTP[backgroundPage[domain.Message]](t, h, "GET", messages, "", "", 200)
	var notices int
	for _, message := range page.Items {
		if message.SourceKind == "platform" {
			notices++
			if message.TopicID != first.TopicID || message.SourceID == "" || message.SenderID == "user" || message.SenderID == m.ID || len(message.Deliveries) != 0 || !strings.Contains(message.Content, m.Name) {
				t.Fatalf("bad platform join event: %+v", message)
			}
		}
	}
	if notices != 1 || len(page.Items) != 5 {
		t.Fatalf("want one visible event and four originals: %+v", page.Items)
	}
	f.assertCount(t, 1, `SELECT count(*) FROM group_background_joins WHERE member_id=? AND kind='preset' AND template_id='text-preset-v1'`, m.ID)
	f.assertCount(t, 2, `SELECT count(*) FROM group_background_intents WHERE delivery_id IS NOT NULL`)
	f.assertCount(t, 0, `SELECT count(*) FROM deliveries WHERE message_id=?`, public.ID)
	f.assertCount(t, 0, `SELECT count(*) FROM tasks`)
}

func TestGroupBackgroundDispatchHTTPThreadRouting(t *testing.T) {
	store.EnableThreadRoutingForBackgroundFixture(t)
	f := newBackgroundFixture(t, "", true)
	g := f.group(t, "")
	quotedAuthor := backgroundMember(t, f, g.ID, discussionProfile)
	quoted, err := f.db.CreateMessage(t.Context(), store.MessageRequest{
		GroupID: g.ID, ActorID: quotedAuthor.ID, TopicID: "previous", Content: "旧结果",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/groups/" + g.ID + "/messages"
	h := backgroundHandler(t, f)
	const content = "  新议题：OPS-498 锁等待。\n引用仅作对照。  "
	const key = "topic-only"
	body := fmt.Sprintf(`{"content":%q,"reply_to_message_id":%q}`, content, quoted.ID)
	first := backgroundHTTP[domain.Message](t, h, "POST", path, body, key, 201)
	if first.GroupID != g.ID || first.TopicID == "" || first.TopicID == quoted.TopicID || first.Content != content ||
		first.ReplyToMessageID != quoted.ID || first.SourceKind != "local_user_chat" || first.SourceID == "" || len(first.Deliveries) != 0 {
		t.Fatalf("E topic-only result did not preserve the original A1 receipt: %+v", first)
	}
	inputPayload := func(mode string) []byte {
		var payload []byte
		for _, field := range []string{g.ID, "", content, mode, quoted.ID, ""} {
			payload = binary.BigEndian.AppendUint64(payload, uint64(len(field)))
			payload = append(payload, field...)
		}
		return payload
	}
	const mode = "omitted+thread-links/v1"
	f.assertCount(t, 1, `SELECT count(*) FROM message_command_sources WHERE message_id=? AND id=? AND idempotency_key=?
	 AND payload=? AND turn_id IS NULL AND claim_digest IS NULL`, first.ID, first.SourceID, key, inputPayload(mode))
	f.assertCount(t, 1, `SELECT count(*) FROM mutation_idempotency WHERE record_id=? AND group_id=? AND actor_id='user'
	 AND idempotency_key=? AND payload=?`, first.ID, g.ID, key, inputPayload(mode+"+topic-selector/v1:auto"))
	f.assertCount(t, 1, `SELECT count(*) FROM group_background_intents i
	 JOIN message_log l ON (l.sequence,l.message_id,l.group_id,l.topic_id)=(i.sequence,i.message_id,i.group_id,i.topic_id)
	 JOIN message_quotes q ON (q.message_id,q.group_id,q.topic_id)=(i.message_id,i.group_id,i.topic_id)
	 WHERE i.message_id=? AND i.source_id=? AND i.group_id=? AND i.topic_id=? AND i.delivery_id IS NULL
	 AND q.quoted_message_id=? AND q.quoted_topic_id=?`, first.ID, first.SourceID, g.ID, first.TopicID, quoted.ID, quoted.TopicID)
	f.assertCount(t, 0, `SELECT count(*) FROM thread_routing_links WHERE message_id=?`, first.ID)

	cfg := backgroundOptions()
	f.pass(t, cfg)
	d := backgroundDelivery(t, f, first).Delivery
	if d.RecipientID == quotedAuthor.ID {
		t.Fatal("A1 woke the ready author of a comparison-only quote")
	}
	f.assertCount(t, 1, `SELECT count(*) FROM deliveries d
	 JOIN conversations c ON (c.id,c.group_id,c.topic_id,c.member_id)=(d.conversation_id,d.group_id,d.topic_id,d.recipient_id)
	 JOIN group_background_joins j ON (j.origin_message_id,j.group_id,j.topic_id,j.member_id)=(d.message_id,d.group_id,d.topic_id,d.recipient_id)
	 JOIN messages n ON (n.id,n.group_id,n.topic_id)=(j.notice_message_id,j.group_id,j.topic_id)
	 WHERE d.id=? AND d.group_id=? AND d.topic_id=? AND j.kind='create' AND n.sender_id='platformnotice'`, d.ID, g.ID, first.TopicID)

	// Once the origin has a recipient, E's issue match takes the normal path.
	for _, tc := range []struct{ key, body, recipient string }{
		{"e-default", `{"content":"跟进 OPS-498"}`, d.RecipientID},
		{"explicit", fmt.Sprintf(`{"content":"跟进 OPS-498","recipient_ids":[%q]}`, quotedAuthor.ID), quotedAuthor.ID},
		{"public", `{"content":"跟进 OPS-498","recipient_ids":[]}`, ""},
	} {
		m := backgroundHTTP[domain.Message](t, h, "POST", path, tc.body, tc.key, 201)
		if m.TopicID != first.TopicID || (tc.recipient == "" && len(m.Deliveries) != 0) ||
			(tc.recipient != "" && (len(m.Deliveries) != 1 || m.Deliveries[0].RecipientID != tc.recipient)) {
			t.Fatalf("%s did not respect the verified topic and recipient choice: %+v", tc.key, m)
		}
	}
	request := store.MessageRequest{GroupID: g.ID, ActorID: "user", Content: content, IdempotencyKey: key,
		ThreadLinks: domain.ThreadLinks{ReplyToMessageID: quoted.ID}}
	if m, found, err := f.db.LookupMessageReceipt(t.Context(), request); err != nil || !found || !reflect.DeepEqual(m, first) {
		t.Fatalf("lookup lost the deferred receipt: %+v, %t, %v", m, found, err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	f = newBackgroundFixture(t, f.path, true)
	h = backgroundHandler(t, f)
	if m := backgroundHTTP[domain.Message](t, h, "POST", path, body, key, 201); !reflect.DeepEqual(m, first) {
		t.Fatalf("reopen did not replay the saved topic, link and source without a delivery: %+v", m)
	}
	mainBody := fmt.Sprintf(`{"content":%q,"reply_to_message_id":%q,"topic_id":""}`, content, quoted.ID)
	backgroundHTTP[any](t, h, "POST", path, mainBody, key, 409)
	f.pass(t, cfg)
	f.assertCount(t, 1, `SELECT count(*) FROM group_background_intents WHERE delivery_id=? AND source_id=?`, d.ID, first.SourceID)
	f.assertCount(t, 1, `SELECT count(*) FROM group_background_intents`)
	f.assertCount(t, 1, `SELECT count(*) FROM group_background_joins`)
	f.assertCount(t, 3, `SELECT count(*) FROM deliveries`)
	f.assertCount(t, 4, `SELECT count(*) FROM message_command_sources`)
}

func TestGroupBackgroundDispatchCompetingPasses(t *testing.T) {
	first := newBackgroundFixture(t, "", true)
	second := newBackgroundFixture(t, first.path, true)
	g := first.group(t, "")
	cfg := backgroundOptions()
	cfg.MaxActiveMembers = 1
	cfg.Presets = []store.BackgroundDispatchTemplate{{ID: "no-match", Name: "不合适", Role: "讨论", Profile: store.BackgroundDispatchProfile{ProviderID: "not-ready", Model: "not-ready"}}}
	const posters = 4
	start, posted, done := make(chan struct{}), make(chan domain.Message, posters), make(chan error, posters)
	for i := range posters {
		f := [2]backgroundFixture{first, second}[i%2]
		go func() {
			<-start
			m, err := f.local.CreateMessage(context.Background(), g.ID, platform.MessageInput{Content: fmt.Sprintf("讨论 %d", i/2), TopicID: "同话题"}, fmt.Sprintf("post-%d", i/2))
			posted <- m
			done <- err
		}()
	}
	close(start)
	originals := make(map[string]domain.Message)
	for range posters {
		if err := <-done; err != nil {
			t.Fatalf("competing posts: %v", err)
		}
		m := <-posted
		if prior, ok := originals[m.ID]; ok && !reflect.DeepEqual(m, prior) {
			t.Fatalf("concurrent receipts diverged: %+v / %+v", m, prior)
		}
		originals[m.ID] = m
	}
	if len(originals) != 2 {
		t.Fatalf("expected one receipt per key, got %d messages", len(originals))
	}
	start = make(chan struct{})
	for _, f := range []backgroundFixture{first, second} {
		go func() { <-start; done <- f.db.RunGroupBackgroundDispatch(context.Background(), cfg) }()
	}
	close(start)
	if a, b := <-done, <-done; a != nil || b != nil {
		t.Fatalf("competing passes: %v / %v", a, b)
	}
	roster, err := first.local.ListMembers(context.Background(), g.ID, store.ListOptions{})
	if err != nil || len(roster.Items) != 1 || roster.Items[0].Name != cfg.Create.Name {
		t.Fatalf("competing messages did not share an actual created member: %+v, %v", roster, err)
	}
	m := roster.Items[0]
	for _, original := range originals {
		got, err := first.db.GetMessage(context.Background(), g.ID, original.ID)
		if err != nil || len(got.Deliveries) != 1 || got.Deliveries[0].RecipientID != m.ID || got.SourceID != original.SourceID {
			t.Fatalf("lost or duplicate original delivery: %+v, %v", got, err)
		}
	}
	first.assertCount(t, 1, `SELECT count(*) FROM group_background_joins WHERE kind='create' AND template_id=?`, cfg.Create.ID)
	first.assertCount(t, 1, `SELECT count(*) FROM conversations`)
	first.assertCount(t, 2, `SELECT count(*) FROM deliveries`)
	first.assertCount(t, 2, `SELECT count(*) FROM group_background_intents WHERE delivery_id IS NOT NULL`)
	first.assertCount(t, 2, `SELECT count(*) FROM message_command_sources`)
}

func TestGroupBackgroundDispatchRestartCapacityAndFairness(t *testing.T) {
	// First use the production migrator without the test registration, then
	// upgrade that exact file and retry its old group/message commands.
	path := filepath.Join(t.TempDir(), "workspace.sqlite")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	before := backgroundFixture{local: platform.NewLocalUser(db)}
	legacy := before.group(t, "legacy-group")
	msg := platform.MessageInput{Content: "讨论"}
	old := before.post(t, legacy.ID, "historic", msg)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	f := newBackgroundFixture(t, path, true)
	if replay := f.group(t, "legacy-group"); replay.ID != legacy.ID {
		t.Fatal("group replay changed identity")
	}
	if replay := f.post(t, legacy.ID, "historic", msg); !reflect.DeepEqual(replay, old) {
		t.Fatal("old replay changed")
	}
	bound := f.group(t, "bound-group")
	blocker, err := f.local.CreateMember(context.Background(), bound.ID, platform.MemberInput{Name: "无运行配置", Role: "讨论"}, "")
	if err != nil {
		t.Fatal(err)
	}
	const backlog = 32
	for i := range backlog {
		m := f.post(t, bound.ID, fmt.Sprintf("backlog-%d", i), msg)
		if len(m.Deliveries) != 0 {
			t.Fatalf("unready member was routed inline: %+v", m)
		}
	}
	other := f.group(t, "other-group")
	later := f.post(t, other.ID, "later", msg)
	cfg := backgroundOptions()
	cfg.MaxActiveMembers = 1
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	disabled := newBackgroundFixture(t, f.path, false)
	disabled.pass(t, cfg)
	if m := disabled.post(t, bound.ID, "while-disabled", msg); len(m.Deliveries) != 0 {
		t.Fatalf("bound message while disabled must wait: %+v", m)
	}
	disabled.assertCount(t, backlog+2, `SELECT count(*) FROM group_background_intents WHERE delivery_id IS NULL`)
	if err := disabled.db.Close(); err != nil {
		t.Fatal(err)
	}
	on := newBackgroundFixture(t, f.path, true)
	on.pass(t, cfg)
	on.assertCount(t, 0, `SELECT count(*) FROM group_background_intents WHERE delivery_id IS NOT NULL`)
	for range backlog + 1 {
		on.pass(t, cfg)
		got, err := on.db.GetMessage(context.Background(), other.ID, later.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Deliveries) == 1 {
			break
		}
	}
	on.assertCount(t, 1, `SELECT count(*) FROM group_background_intents WHERE delivery_id IS NOT NULL AND group_id=?`, other.ID)
	on.assertCount(t, backlog+1, `SELECT count(*) FROM group_background_intents WHERE delivery_id IS NULL AND group_id=?`, bound.ID)
	on.exec(t, `UPDATE members SET status='left' WHERE id=?`, blocker.ID)
	denied := cfg
	denied.Create = nil
	on.pass(t, denied)
	on.assertCount(t, backlog+1, `SELECT count(*) FROM group_background_intents WHERE delivery_id IS NULL AND group_id=?`, bound.ID)
	// A sweep that already passed the blocked topic head must wrap before
	// retrying it. Two full bounded sweeps suffice for this backlog.
	for range 6 {
		on.pass(t, cfg)
	}
	on.assertCount(t, backlog+1, `SELECT count(*) FROM group_background_intents WHERE delivery_id IS NOT NULL AND group_id=?`, bound.ID)
	on.assertCount(t, 1, `SELECT count(*) FROM group_background_joins WHERE group_id=?`, bound.ID)
	on.assertCount(t, 0, `SELECT count(*) FROM group_background_bindings WHERE group_id=?`, legacy.ID)
	if err := on.db.Close(); err != nil {
		t.Fatal(err)
	}
	settled := newBackgroundFixture(t, f.path, true)
	settled.pass(t, cfg)
	if replay := settled.post(t, other.ID, "later", msg); !reflect.DeepEqual(replay, later) {
		t.Fatalf("restart changed original receipt: %+v", replay)
	}
	settled.assertCount(t, 2, `SELECT count(*) FROM group_background_joins`)
}

func TestGroupBackgroundDispatchRollbackAndSource(t *testing.T) {
	f := newBackgroundFixture(t, "", true)
	ctx := context.Background()
	f.exec(t, `CREATE TRIGGER reject_binding BEFORE INSERT ON group_background_bindings BEGIN SELECT RAISE(ABORT,'rollback fixture'); END;`)
	if g, err := f.local.CreateGroup(ctx, platform.GroupInput{Name: "测试群"}, "rollback-group"); err == nil || g.ID != "" {
		t.Fatalf("binding failure returned a partial group: %+v %v", g, err)
	}
	f.assertCount(t, 0, `SELECT count(*) FROM groups`)
	f.assertCount(t, 0, `SELECT count(*) FROM mutation_idempotency`)
	f.exec(t, `DROP TRIGGER reject_binding`)
	g := f.group(t, "rollback-group")
	f.exec(t, `CREATE TRIGGER reject_intent BEFORE INSERT ON group_background_intents BEGIN SELECT RAISE(ABORT,'rollback fixture'); END;`)
	input := platform.MessageInput{Content: "只是讨论", TopicID: "主题"}
	if m, err := f.local.CreateMessage(ctx, g.ID, input, "rollback-message"); err == nil || m.ID != "" {
		t.Fatalf("intent failure returned a partial receipt: %+v %v", m, err)
	}
	f.assertCount(t, 0, `SELECT count(*) FROM messages`)
	f.assertCount(t, 0, `SELECT count(*) FROM message_command_sources`)
	f.assertCount(t, 1, `SELECT count(*) FROM mutation_idempotency`)
	f.exec(t, `DROP TRIGGER reject_intent`)
	unproven, err := f.db.CreateMessage(ctx, store.MessageRequest{GroupID: g.ID, TopicID: input.TopicID, Content: input.Content, ActorID: "user", IdempotencyKey: "unproven"}, nil)
	if err != nil || unproven.SourceKind != "unknown" {
		t.Fatalf("unproven legacy sender: %+v %v", unproven, err)
	}
	if m := f.post(t, g.ID, "unproven", input); !reflect.DeepEqual(m, unproven) {
		t.Fatalf("legacy replay promoted to a real source: %+v", m)
	}
	m := f.post(t, g.ID, "rollback-message", input)
	f.assertCount(t, 1, `SELECT count(*) FROM group_background_intents WHERE message_id=? AND source_id=?`, m.ID, m.SourceID)
	if _, err := f.raw.Exec(`UPDATE group_background_intents SET topic_id='别的话题' WHERE message_id=?`, m.ID); err == nil {
		t.Fatal("intent accepted topic substitution")
	}
	if _, err := f.raw.Exec(`UPDATE group_background_intents SET message_id=? WHERE message_id=?`, unproven.ID, m.ID); err == nil {
		t.Fatal("intent accepted unproven message substitution")
	}
	f.exec(t, `CREATE TRIGGER reject_finish BEFORE UPDATE OF delivery_id ON group_background_intents BEGIN SELECT RAISE(ABORT,'rollback fixture'); END;`)
	if err := f.db.RunGroupBackgroundDispatch(ctx, backgroundOptions()); err == nil {
		t.Fatal("delivery transaction did not report failure")
	}
	for _, table := range []string{"members", "group_background_joins", "conversations", "deliveries", "turns"} {
		f.assertCount(t, 0, `SELECT count(*) FROM `+table)
	}
	f.assertCount(t, 2, `SELECT count(*) FROM messages`)
	f.exec(t, `DROP TRIGGER reject_finish`)
	f.pass(t, backgroundOptions())
	f.assertCount(t, 1, `SELECT count(*) FROM deliveries WHERE message_id=?`, m.ID)
	f.assertCount(t, 0, `SELECT count(*) FROM deliveries WHERE message_id=?`, unproven.ID)
}

func TestGroupBackgroundDispatchPrepareScope(t *testing.T) {
	f := newBackgroundFixture(t, "", true)
	g, foreign := f.group(t, ""), f.group(t, "")
	const topic = "原话题"
	f.post(t, g.ID, "", platform.MessageInput{TopicID: topic, Content: "SAME_TOPIC", RecipientIDs: []string{}})
	f.post(t, g.ID, "", platform.MessageInput{TopicID: "其他话题", Content: "OTHER_TOPIC", RecipientIDs: []string{}})
	f.post(t, foreign.ID, "", platform.MessageInput{TopicID: topic, Content: "OTHER_GROUP", RecipientIDs: []string{}})
	m := f.post(t, g.ID, "original", platform.MessageInput{TopicID: topic, Content: "ORIGINAL_TRIGGER"})
	f.pass(t, backgroundOptions())
	ctx := context.Background()
	queued, err := f.db.ListPendingDeliveries(ctx, store.PendingOptions{Lane: "human"})
	if err != nil || len(queued.Items) != 1 || queued.Items[0].Delivery.MessageID != m.ID {
		t.Fatalf("original absent from existing human lane: %+v, %v", queued, err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "executable-not-run")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	p, err := platform.NewPreparer(f.db, platform.PrepareOptions{PrivateWorkspaceRoot: root, Providers: []provider.Provider{
		{ID: discussionProfile.ProviderID, Name: provider.Pi, Version: "1.4.0", Path: bin, Installation: provider.Installed},
	}})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := p.Prepare(ctx, queued.Items[0])
	prompt := prepared.Request.Prompt
	if err != nil || prepared.Request.Model != discussionProfile.Model || !strings.Contains(prompt, "ORIGINAL_TRIGGER") ||
		!strings.Contains(prompt, "SAME_TOPIC") || !strings.Contains(prompt, "background_reason") ||
		strings.Contains(prompt, "OTHER_TOPIC") || strings.Contains(prompt, "OTHER_GROUP") || strings.Contains(prompt, "intent_assessment") {
		t.Fatalf("unexpected discussion preparation: %s, %v", prompt, err)
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: g.ID, DeliveryID: queued.Items[0].Delivery.ID,
		MaxActive: 1, ResourceKeys: prepared.ResourceKeys, BackgroundProfile: prepared.BackgroundProfile})
	if err != nil || turn.MemberID != queued.Items[0].Delivery.RecipientID {
		t.Fatalf("existing scheduler claim rejected original delivery: %+v, %v", turn, err)
	}
	if _, err := f.local.GetMessage(ctx, foreign.ID, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-group original: %v", err)
	}
}
