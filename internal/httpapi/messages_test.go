package httpapi

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
)

func openMessageAPI(t *testing.T, path string) *groupAPI {
	t.Helper()
	a := openGroupAPI(t, path)
	local := platform.NewLocalUser(a.db)
	routes := append(LocalUserRoutes(local), MessageRoutes(local)...)
	a.handler = mustServer(t, DefaultAddress, routes...).Handler
	return a
}

func messageSQL(t *testing.T, a *groupAPI) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", a.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMessageHTTPReceiptsAndLatestDeliveries(t *testing.T) {
	a := openMessageAPI(t, "")
	g, other := newGroup(t, a, "group"), newGroup(t, a, "other")
	path := "/api/v1/groups/" + g.ID + "/messages"
	empty := decoded[listResult[domain.Message]](t, a.request("GET", path, ""), 200)
	if empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatalf("empty message feed: %+v", empty)
	}
	member := decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+g.ID+"/members", `{"name":"member","role":"role"}`), 201)
	body := `{"content":" exact content ","topic_id":"topic"}`
	first := a.request("POST", path, body, "message-key")
	message := decoded[domain.Message](t, first, 201)
	if message.ID == "" || message.GroupID != g.ID || message.SenderID != "user" || message.Content != " exact content " || message.TopicID != "topic" || message.CreatedAt.IsZero() || message.CreatedAt.Location() != time.UTC || len(message.Deliveries) != 1 || message.Deliveries[0].RecipientID != member.ID || message.Deliveries[0].State != domain.DeliveryStateQueued || message.Deliveries[0].TurnID != "" {
		t.Fatalf("created message contract: %+v", message)
	}
	retry := a.request("POST", path, body, "message-key")
	if retry.Code != 201 || !bytes.Equal(first.Body.Bytes(), retry.Body.Bytes()) {
		t.Fatalf("201 replay changed receipt: %d %s", retry.Code, retry.Body)
	}
	for _, body := range []string{`{"content":"changed","topic_id":"topic"}`, `{"content":" exact content ","topic_id":"topic","recipient_ids":[]}`} {
		w := a.request("POST", path, body, "message-key")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 409, "conflict")
	}
	otherPath := "/api/v1/groups/" + other.ID + "/messages"
	otherMessage := decoded[domain.Message](t, a.request("POST", otherPath, body, "message-key"), 201)
	if otherMessage.ID == message.ID || otherMessage.GroupID != other.ID || len(otherMessage.Deliveries) != 0 {
		t.Fatalf("group-scoped message key/routing: %+v", otherMessage)
	}
	raw := messageSQL(t, a)
	if _, err := raw.Exec("UPDATE deliveries SET state='accepted',turn_id='turn-receipt' WHERE message_id=?", message.ID); err != nil {
		t.Fatal(err)
	}
	detail := decoded[domain.Message](t, a.request("GET", path+"/"+message.ID, ""), 200)
	if len(detail.Deliveries) != 1 || detail.Deliveries[0].State != domain.DeliveryStateAccepted || detail.Deliveries[0].TurnID != "turn-receipt" {
		t.Fatalf("detail did not load latest delivery receipt: %+v", detail)
	}
	replayed := decoded[domain.Message](t, a.request("POST", path, body, "message-key"), 201)
	page := decoded[listResult[domain.Message]](t, a.request("GET", path, ""), 200)
	if !reflect.DeepEqual(replayed, detail) || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], detail) {
		t.Fatalf("replay/list delivery state is stale: %+v %+v", replayed, page)
	}
	visible := decoded[domain.Message](t, a.request("POST", path, `{"content":"visible","recipient_ids":[]}`, "explicit-empty"), 201)
	if visible.Deliveries == nil || len(visible.Deliveries) != 0 {
		t.Fatalf("explicit [] was routed: %+v", visible)
	}
	for _, path := range []string{otherPath + "/" + message.ID, path + "/missing", "/api/v1/groups/missing/messages"} {
		w := a.request("GET", path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	w := a.request("POST", "/api/v1/groups/missing/messages", body)
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	w = a.request("DELETE", path+"/"+message.ID, "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 405, "method_not_allowed")
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openMessageAPI(t, a.path)
	replayed = decoded[domain.Message](t, reopened.request("POST", path, body, "message-key"), 201)
	if !reflect.DeepEqual(replayed, detail) {
		t.Fatalf("HTTP replay did not survive reopen: %+v", replayed)
	}
}

func TestMessageHTTPRejectsInvalidInputWithoutWrites(t *testing.T) {
	a := openMessageAPI(t, "")
	g, other := newGroup(t, a, "group"), newGroup(t, a, "other")
	path := "/api/v1/groups/" + g.ID + "/messages"
	member := decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+g.ID+"/members", `{"name":"member","role":"role"}`), 201)
	foreign := decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+other.ID+"/members", `{"name":"foreign","role":"role"}`), 201)
	raw := messageSQL(t, a)
	assertNoWrites := func(t *testing.T) {
		t.Helper()
		for _, table := range []string{"messages", "deliveries", "conversations", "mutation_idempotency"} {
			var count int
			if err := raw.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("invalid request wrote %s: %d, %v", table, count, err)
			}
		}
	}
	for i, body := range []string{
		`null`, `[]`, `{`, `{"content":"ok"} {}`, `{"content":42}`, `{"content":"ok","recipient_ids":"member"}`,
		`{"content":"ok","content":"again"}`, `{"content":"ok","Content":"again"}`, `{"content":"ok","topic_id":{}}`,
	} {
		t.Run(fmt.Sprintf("json-%d", i), func(t *testing.T) {
			w := a.request("POST", path, body, "rejected")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
			assertNoWrites(t)
		})
	}
	for _, field := range []string{"actor_id", "sender_id", "group_id", "id", "idempotency_key"} {
		t.Run(field, func(t *testing.T) {
			w := a.request("POST", path, `{"content":"ok","`+field+`":"forged"}`, "rejected")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_json")
			assertNoWrites(t)
		})
	}
	for i, keys := range [][]string{{""}, {"same", "same"}, {"a,b"}, {"has space"}, {strings.Repeat("x", 129)}} {
		t.Run(fmt.Sprintf("key-%d", i), func(t *testing.T) {
			w := a.request("POST", path, `{"content":"ok"}`, keys...)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
			assertNoWrites(t)
		})
	}
	for _, body := range []string{`{}`, `{"content":" \t"}`} {
		w := a.request("POST", path, body, "rejected")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		assertNoWrites(t)
	}
	crossGroup, _ := json.Marshal(platform.MessageInput{Content: "no partial writes", RecipientIDs: []string{member.ID, foreign.ID}})
	w := a.request("POST", path, string(crossGroup), "rejected")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	assertNoWrites(t)
	w = a.request("POST", path, `{"content":"`+strings.Repeat("x", MaxJSONBodyBytes)+`"}`, "rejected")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 413, "body_too_large")
	assertNoWrites(t)
	for _, method := range []string{"POST", "GET"} {
		target := path + "?after="
		if method == "GET" {
			target = path + "/missing?after="
		}
		w := a.request(method, target, `{"content":"ok"}`, "rejected")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		assertNoWrites(t)
	}
	// Rejected input must not poison its key for a later valid request.
	decoded[domain.Message](t, a.request("POST", path, `{"content":"ok"}`, "rejected"), 201)
}

func TestMessageHTTPBoundedAllTopicBeforePagination(t *testing.T) {
	a := openMessageAPI(t, "")
	g, other := newGroup(t, a, "group"), newGroup(t, a, "other")
	path := "/api/v1/groups/" + g.ID + "/messages"
	raw := messageSQL(t, a)
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	want := make([]domain.Message, 0, 204)
	for i := range 204 {
		n := (i * 73) % 204
		message := domain.Message{
			ID: fmt.Sprintf("m-%03d", n), GroupID: g.ID, TopicID: []string{"", "topic-a", "topic-b"}[n%3],
			SenderID: "user", Content: "fixture", CreatedAt: base.Add(time.Duration(n%5) * time.Nanosecond), Deliveries: []domain.Delivery{},
		}
		if _, err := tx.Exec(`INSERT INTO messages(id,group_id,topic_id,sender_id,content,created_at) VALUES(?,?,?,?,?,?)`, message.ID, message.GroupID, message.TopicID, message.SenderID, message.Content, message.CreatedAt.Format("2006-01-02T15:04:05.000000000Z")); err != nil {
			t.Fatal(err)
		}
		want = append(want, message)
	}
	if _, err := tx.Exec(`INSERT INTO messages(id,group_id,sender_id,content,created_at) VALUES('foreign',?,'user','secret',?)`, other.ID, base.Add(time.Hour).Format("2006-01-02T15:04:05.000000000Z")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].CreatedAt.Equal(want[j].CreatedAt) {
			return want[i].ID > want[j].ID
		}
		return want[i].CreatedAt.After(want[j].CreatedAt)
	})
	defaultPage := decoded[listResult[domain.Message]](t, a.request("GET", path, ""), 200)
	if len(defaultPage.Items) != 50 || defaultPage.NextCursor == "" || !reflect.DeepEqual(defaultPage.Items, want[:50]) {
		t.Fatalf("default all-topic page is not bounded to 50: %+v", defaultPage)
	}
	maximum := decoded[listResult[domain.Message]](t, a.request("GET", path+"?limit=200", ""), 200)
	if len(maximum.Items) != 200 || maximum.NextCursor == "" || !reflect.DeepEqual(maximum.Items, want[:200]) {
		t.Fatalf("maximum all-topic page: %+v", maximum)
	}
	tail := decoded[listResult[domain.Message]](t, a.request("GET", path+"?limit=200&before="+maximum.NextCursor, ""), 200)
	if !reflect.DeepEqual(tail.Items, want[200:]) || tail.NextCursor != "" {
		t.Fatalf("maximum page continuation: %+v", tail)
	}
	page := decoded[listResult[domain.Message]](t, a.request("GET", path+"?limit=51", ""), 200)
	cursor := page.NextCursor
	got := append([]domain.Message{}, page.Items...)
	decoded[domain.Message](t, a.request("POST", path, `{"content":"new after first page","recipient_ids":[]}`), 201)
	for pages := 1; page.NextCursor != ""; pages++ {
		if pages >= 4 {
			t.Fatal("pagination did not end on an exactly full last page")
		}
		page = decoded[listResult[domain.Message]](t, a.request("GET", path+"?limit=51&before="+page.NextCursor, ""), 200)
		got = append(got, page.Items...)
	}
	if !reflect.DeepEqual(got, want) || len(page.Items) != 51 {
		t.Fatalf("all-topic history skipped, duplicated, or included later inserts: got %d, want %d", len(got), len(want))
	}
	groups := decoded[listResult[domain.Group]](t, a.request("GET", "/api/v1/groups?limit=1", ""), 200)
	for _, target := range []string{
		"/api/v1/groups/" + other.ID + "/messages?before=" + cursor,
		path + "?before=" + groups.NextCursor,
		"/api/v1/groups?before=" + cursor,
	} {
		w := a.request("GET", target, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, query := range []string{"after=x", "after=", "after", "before=" + cursor + "&after=", "topic_id=topic-a", "limit=0", "limit=201", "limit=1&limit=2", "before="} {
		w := a.request("GET", path+"?"+query, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
}
