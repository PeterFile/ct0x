package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

type memberToolHTTPSendResult struct {
	MessageID      string    `json:"message_id"`
	CreatedAt      time.Time `json:"created_at"`
	DeliveryCount  int       `json:"delivery_count"`
	ContentOmitted bool      `json:"content_omitted"`
}

type memberToolHTTPCreateResult struct {
	MemberID      string `json:"member_id"`
	Reused        bool   `json:"reused"`
	JoinMessageID string `json:"join_message_id"`
}

func memberToolHTTPKeys(t *testing.T, w *httptest.ResponseRecorder, keys ...string) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &object); err != nil || len(object) != len(keys) {
		t.Fatalf("response does not have the exact projection fields: %s, %v", w.Body, err)
	}
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			t.Fatalf("projection omitted %s", key)
		}
	}
}

type memberToolHTTPFixture struct {
	db                     *store.Store
	raw                    *sql.DB
	path, groupID, otherID string
	caller, peer, foreign  domain.Member
	claim                  store.TurnClaim
	token                  string
	tools                  *platform.MemberTools
	routes                 []Route
	handler                http.Handler
}

func newMemberToolHTTP(t *testing.T, operations ...string) *memberToolHTTPFixture {
	t.Helper()
	f := &memberToolHTTPFixture{path: filepath.Join(t.TempDir(), "member-tools.db")}
	var err error
	f.db, err = store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	f.raw, err = sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.raw.Close() })
	ctx := context.Background()
	for i := range 2 {
		group, err := f.db.CreateGroup(ctx, store.CreateGroupInput{Name: fmt.Sprint("group-", i)})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			f.groupID = group.ID
		} else {
			f.otherID = group.ID
		}
	}
	for i, target := range []*domain.Member{&f.caller, &f.peer, &f.foreign} {
		groupID := f.groupID
		if i == 2 {
			groupID = f.otherID
		}
		*target, err = f.db.CreateMember(ctx, store.CreateMemberInput{GroupID: groupID, Name: fmt.Sprint("member-", i), Role: fmt.Sprint("role-", i), Workspace: "/configured/private-workspace"})
		if err != nil {
			t.Fatal(err)
		}
	}
	message, err := f.db.CreateMessage(ctx, store.MessageRequest{GroupID: f.groupID, ActorID: "user", TopicID: "trusted-topic", Content: "initial question", RecipientIDs: []string{f.caller.ID}}, []string{f.caller.ID})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.groupID, DeliveryID: message.Deliveries[0].ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.claim = store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}
	if len(operations) == 0 {
		operations = []string{"list_members", "read_channel", "send_message", "create_member"}
	}
	f.tools = platform.NewMemberTools(f.db)
	f.token, err = f.tools.Issue(ctx, f.claim, platform.MemberToolGrants{Operations: operations, ProviderID: "trusted-provider", Model: "trusted-model", MaxGroupMembers: 3})
	if err != nil || f.token == f.claim.Token {
		t.Fatalf("issue a distinct real capability: %v", err)
	}
	f.routes = MemberToolRoutes(f.tools)
	f.handler = mustServer(t, DefaultAddress, f.routes...).Handler
	return f
}

func (f *memberToolHTTPFixture) newRequest(operation, body string, keys ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://"+DefaultAddress+"/api/v1/member-tools/"+operation, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.token)
	for _, key := range keys {
		r.Header.Add("Idempotency-Key", key)
	}
	return r
}

func (f *memberToolHTTPFixture) request(t *testing.T, operation, body string, keys ...string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, f.newRequest(operation, body, keys...))
	f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
	return w
}

func (f *memberToolHTTPFixture) safeResponse(t *testing.T, status int, header http.Header, body []byte, owned bool) {
	t.Helper()
	if owned && len(body) > 64*1024 {
		t.Fatalf("route response exceeded the complete JSON byte cap: %d", len(body))
	}
	if owned && header.Get("Cache-Control") != "no-store" {
		t.Fatalf("route response can be cached: %v", header)
	}
	for key := range header {
		if strings.HasPrefix(strings.ToLower(key), "access-control-") {
			t.Fatal("private transport emitted CORS headers")
		}
	}
	if bytes.Contains(body, []byte(f.token)) || bytes.Contains(body, []byte(f.claim.Token)) {
		t.Fatal("response exposed a capability or claim secret")
	}
	if status >= 400 {
		for _, secret := range []string{f.path, "/configured/private-workspace", "PRIVATE-SQL"} {
			if bytes.Contains(body, []byte(secret)) {
				t.Fatal("error reflected internal storage or configuration")
			}
		}
	}
}

func (f *memberToolHTTPFixture) counts(t *testing.T) [6]int {
	t.Helper()
	var result [6]int
	for i, table := range []string{"members", "messages", "deliveries", "conversations", "mutation_idempotency", "turns"} {
		if err := f.raw.QueryRow("SELECT count(*) FROM " + table).Scan(&result[i]); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func TestMemberToolHTTPDurableOperationsAndStoredReplay(t *testing.T) {
	f := newMemberToolHTTP(t)
	ctx := context.Background()
	members := decoded[listResult[domain.Member]](t, f.request(t, "list_members", `{}`), 200)
	if members.Items == nil || len(members.Items) != 2 || members.NextCursor != "" {
		t.Fatalf("bound member list: %+v", members)
	}
	for _, m := range members.Items {
		if m.GroupID != f.groupID || m.ID == f.foreign.ID {
			t.Fatal("member list escaped the bound group")
		}
	}
	content := "complete evidence:\n" + strings.Repeat("preserve this evidence\n", 1024)
	input := platform.MemberMessageInput{Content: content, RecipientIDs: []string{f.peer.ID, f.caller.ID}}
	body, _ := json.Marshal(input)
	first := f.request(t, "send_message", string(body), "Key/Exact:1")
	message := decoded[memberToolHTTPSendResult](t, first, 201)
	memberToolHTTPKeys(t, first, "message_id", "created_at", "delivery_count", "content_omitted")
	if message.MessageID == "" || message.DeliveryCount != 2 || !message.ContentOmitted {
		t.Fatalf("send projection lost its durable reference: %+v", message)
	}
	stored, err := f.db.GetMessage(ctx, f.groupID, message.MessageID)
	if err != nil || stored.SenderID != f.caller.ID || stored.GroupID != f.groupID || stored.TopicID != "trusted-topic" || stored.Content != content || !stored.CreatedAt.Equal(message.CreatedAt) || len(stored.Deliveries) != message.DeliveryCount {
		t.Fatalf("projection changed the full durable message or bound identity: %v", err)
	}
	for _, d := range stored.Deliveries {
		if d.State != domain.DeliveryStateQueued || d.TurnID != "" {
			t.Fatal("HTTP send started a model instead of persisting a queued delivery")
		}
	}
	var storedKey string
	if err := f.raw.QueryRow("SELECT idempotency_key FROM mutation_idempotency WHERE record_id=?", message.MessageID).Scan(&storedKey); err != nil || storedKey != "Key/Exact:1" {
		t.Fatalf("transport changed idempotency key bytes: %v", err)
	}
	before := f.counts(t)
	replay := f.request(t, "send_message", string(body), "Key/Exact:1")
	if replay.Code != 201 || !bytes.Equal(replay.Body.Bytes(), first.Body.Bytes()) || f.counts(t) != before {
		t.Fatal("send replay duplicated or reinterpreted the persisted result")
	}
	input.Content = "changed"
	changed, _ := json.Marshal(input)
	w := f.request(t, "send_message", string(changed), "Key/Exact:1")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
	for i, body := range []string{`{"content":"public"}`, `{"content":"public","recipient_ids":[]}`} {
		key := fmt.Sprint("public-", i)
		public := decoded[memberToolHTTPSendResult](t, f.request(t, "send_message", body, key), 201)
		stored, err := f.db.GetMessage(ctx, f.groupID, public.MessageID)
		if err != nil || public.DeliveryCount != 0 || !public.ContentOmitted || stored.Deliveries == nil || len(stored.Deliveries) != 0 {
			t.Fatal("member public message default-routed or lost its projection")
		}
		other := []string{`{"content":"public","recipient_ids":[]}`, `{"content":"public"}`}[i]
		w := f.request(t, "send_message", other, key)
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
	}
	createBody := `{"name":"Analyst","role":"Analyze dataset A","reason":"Explain dataset A, the assigned question, and its outliers."}`
	createdResponse := f.request(t, "create_member", createBody, "create")
	created := decoded[memberToolHTTPCreateResult](t, createdResponse, 201)
	memberToolHTTPKeys(t, createdResponse, "member_id", "reused", "join_message_id")
	member, err := f.db.GetMember(ctx, f.groupID, created.MemberID)
	if err != nil || created.Reused || created.JoinMessageID == "" || member.CreatorID != f.caller.ID || member.OwnerID != "user" || member.ProviderID != "trusted-provider" || member.Model != "trusted-model" || member.Workspace != "" {
		t.Fatalf("create did not persist bound configuration and ownership: %v", err)
	}
	join, err := f.db.GetMessage(ctx, f.groupID, created.JoinMessageID)
	if err != nil || !strings.Contains(join.Content, "Analyst") || !strings.Contains(join.Content, "Explain dataset A, the assigned question, and its outliers.") || join.SenderID != f.caller.ID || join.TopicID != "trusted-topic" || len(join.Deliveries) != 1 || join.Deliveries[0].RecipientID != created.MemberID || join.Deliveries[0].State != domain.DeliveryStateQueued {
		t.Fatalf("actual join trigger lacks the assigned background or identity: %v", err)
	}
	before = f.counts(t)
	createdReplay := f.request(t, "create_member", createBody, "create")
	if createdReplay.Code != 201 || !bytes.Equal(createdResponse.Body.Bytes(), createdReplay.Body.Bytes()) || f.counts(t) != before {
		t.Fatal("created replay was reinterpreted as reuse or created another join")
	}
	reuseBody := strings.Replace(createBody, "Analyst", "Different display name", 1)
	reusedResponse := f.request(t, "create_member", reuseBody, "reuse")
	reused := decoded[memberToolHTTPCreateResult](t, reusedResponse, 200)
	memberToolHTTPKeys(t, reusedResponse, "member_id", "reused", "join_message_id")
	if !reused.Reused || reused.JoinMessageID != "" || reused.MemberID != created.MemberID {
		t.Fatalf("full-capacity reuse returned the wrong outcome: %+v", reused)
	}
	before = f.counts(t)
	reusedReplay := f.request(t, "create_member", reuseBody, "reuse")
	if reusedReplay.Code != 200 || !bytes.Equal(reusedResponse.Body.Bytes(), reusedReplay.Body.Bytes()) || f.counts(t) != before {
		t.Fatal("reused replay emitted another creation or join")
	}
	w = f.request(t, "create_member", strings.Replace(createBody, "Analyze dataset A", "Different role", 1), "full")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "member_capacity")
	if !strings.Contains(w.Body.String(), "容量已满") || !strings.Contains(w.Body.String(), "重试") || !strings.Contains(w.Body.String(), "名额") || f.counts(t) != before {
		t.Fatal("capacity response omitted retry guidance or left partial effects")
	}
	if _, err := f.db.CreateMessage(ctx, store.MessageRequest{GroupID: f.groupID, ActorID: "user", TopicID: "other-topic", Content: "PRIVATE other topic"}, nil); err != nil {
		t.Fatal(err)
	}
	history := decoded[listResult[domain.Message]](t, f.request(t, "read_channel", `{"limit":200}`), 200)
	if history.Items == nil || len(history.Items) != 5 || history.NextCursor != "" {
		t.Fatalf("current topic history: %+v", history)
	}
	for _, item := range history.Items {
		if item.TopicID != "trusted-topic" || item.GroupID != f.groupID {
			t.Fatal("read_channel escaped its bound group/topic")
		}
	}
	if f.counts(t)[5] != 1 {
		t.Fatal("HTTP tools executed another turn")
	}
}

func TestMemberToolHTTPBodyPaginationAndCursorCannotChooseAuthority(t *testing.T) {
	f := newMemberToolHTTP(t)
	base := f.caller.CreatedAt.UTC().Add(time.Hour).Truncate(time.Second).Add(123456789 * time.Nanosecond)
	tx, err := f.raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 50 {
		stamp := base.Add(time.Duration(i%3) * time.Nanosecond).Format("2006-01-02T15:04:05.000000000Z")
		if _, err := tx.Exec(`INSERT INTO members(id,group_id,name,role,created_at) VALUES(?,?,'fixture','fixture',?)`, fmt.Sprintf("paged-member-%03d", i), f.groupID, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at) VALUES(?,?,'trusted-topic','user','fixture',?)`, fmt.Sprintf("paged-message-%03d", i), f.groupID, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at) VALUES('noise',?,'other-topic','user','PRIVATE',?)`, f.groupID, base.Add(time.Hour).Format("2006-01-02T15:04:05.000000000Z")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"list_members", "read_channel"} {
		first := decoded[listResult[json.RawMessage]](t, f.request(t, operation, `{}`), 200)
		if first.Items == nil || len(first.Items) != 50 || first.NextCursor == "" {
			t.Fatalf("%s omitted limit did not select 50: %d items", operation, len(first.Items))
		}
		raw, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
		if err != nil || !bytes.Contains(raw, []byte(base.Format(time.RFC3339Nano))) {
			t.Fatalf("cursor lost its nanosecond position: %s, %v", raw, err)
		}
		body, _ := json.Marshal(map[string]any{"limit": 200, "before": first.NextCursor})
		tail := decoded[listResult[json.RawMessage]](t, f.request(t, operation, string(body)), 200)
		wantTail := map[string]int{"list_members": 2, "read_channel": 1}[operation]
		if len(tail.Items) != wantTail || tail.NextCursor != "" {
			t.Fatalf("%s before paging skipped or duplicated rows: %+v", operation, tail)
		}
		maximum := decoded[listResult[json.RawMessage]](t, f.request(t, operation, `{"limit":200}`), 200)
		if len(maximum.Items) != 50+wantTail || maximum.NextCursor != "" {
			t.Fatalf("%s maximum limit was not passed through", operation)
		}
		var cursor pageCursor
		if err := json.Unmarshal(raw, &cursor); err != nil {
			t.Fatal(err)
		}
		for _, change := range []func(*pageCursor){
			func(c *pageCursor) { c.Scope = "other-operation" },
			func(c *pageCursor) { c.GroupID = f.otherID },
		} {
			bad := cursor
			change(&bad)
			encoded, _ := json.Marshal(bad)
			body, _ := json.Marshal(map[string]string{"before": base64.RawURLEncoding.EncodeToString(encoded)})
			w := f.request(t, operation, string(body))
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		}
		cursor.CreatedAt = time.Date(2000, 1, 1, 0, 0, 0, 1, time.UTC)
		emptyCursor, _ := json.Marshal(cursor)
		emptyBody, _ := json.Marshal(map[string]string{"before": base64.RawURLEncoding.EncodeToString(emptyCursor)})
		empty := decoded[listResult[json.RawMessage]](t, f.request(t, operation, string(emptyBody)), 200)
		if empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
			t.Fatal("empty page must contain [] and an empty next_cursor")
		}
	}
	ctx := context.Background()
	message, err := f.db.CreateMessage(ctx, store.MessageRequest{GroupID: f.otherID, ActorID: "user", TopicID: "other-bound-topic", Content: "other bound question", RecipientIDs: []string{f.foreign.ID}}, []string{f.foreign.ID})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.ClaimTurn(ctx, store.ClaimTurnInput{GroupID: f.otherID, DeliveryID: message.Deliveries[0].ID, MaxActive: 2})
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := f.tools.Issue(ctx, store.TurnClaim{GroupID: turn.GroupID, TurnID: turn.ID, Token: turn.ClaimToken}, platform.MemberToolGrants{Operations: []string{"read_channel"}})
	if err != nil {
		t.Fatal(err)
	}
	first := decoded[listResult[domain.Message]](t, f.request(t, "read_channel", `{"limit":1}`), 200)
	body, _ := json.Marshal(map[string]string{"before": first.NextCursor})
	r := f.newRequest("read_channel", string(body))
	r.Header.Set("Authorization", "Bearer "+otherToken)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	f.safeResponse(t, w.Code, w.Header(), w.Body.Bytes(), true)
	other := decoded[listResult[domain.Message]](t, w, 200)
	if len(other.Items) != 1 || other.Items[0].ID != message.ID || other.Items[0].GroupID != f.otherID || other.Items[0].TopicID != "other-bound-topic" || bytes.Contains(w.Body.Bytes(), []byte(otherToken)) || bytes.Contains(w.Body.Bytes(), []byte(turn.ClaimToken)) {
		t.Fatal("cursor selected authority instead of merely a position")
	}
}
