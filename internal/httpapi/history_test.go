package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func openHistoryAPI(t *testing.T) *groupAPI {
	t.Helper()
	a := openGroupAPI(t, "")
	local := platform.NewLocalUser(a.db)
	routes := append(LocalUserRoutes(local), HistoryRoutes(local)...)
	a.handler = mustServer(t, DefaultAddress, routes...).Handler
	return a
}

func historyMessage(t *testing.T, a *groupAPI, group, topic, content string, recipients ...string) domain.Message {
	t.Helper()
	m, err := a.db.CreateMessage(context.Background(), store.MessageRequest{GroupID: group, ActorID: "user", TopicID: topic, Content: content}, recipients)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHistoryRoutesLiteralScopeAndDetails(t *testing.T) {
	a := openHistoryAPI(t)
	group, other := newGroup(t, a, "history"), newGroup(t, a, "other")
	base := "/api/v1/groups/" + group.ID + "/history"
	member := decoded[domain.Member](t, a.request("POST", "/api/v1/groups/"+group.ID+"/members", `{"name":"reader","role":"reader"}`), 201)
	main := historyMessage(t, a, group.ID, "", "中文 100% under_score", member.ID)
	topic := "话题 +%_/"
	reply := historyMessage(t, a, group.ID, topic, "中文 100% under_score 回复")
	historyMessage(t, a, group.ID, "", "1000 underXscore")
	historyMessage(t, a, other.ID, "", "中文 100% under_score")
	for _, query := range []string{"中文", "100%", "_", "under_score"} {
		page := decoded[store.HistoryPage](t, a.request("GET", base+"?q="+url.QueryEscape(query), ""), 200)
		if !reflect.DeepEqual(page.Items, []domain.Message{reply, main}) || page.NextCursor != "" {
			t.Fatalf("literal query %q: %+v", query, page)
		}
	}
	for _, query := range []string{"q=中文&topic_id=", "q=中文&topic_id", "q=中文&%74opic_id="} {
		page := decoded[store.HistoryPage](t, a.request("GET", base+"?"+query+"&limit=1", ""), 200)
		if !reflect.DeepEqual(page.Items, []domain.Message{main}) || page.NextCursor != "" {
			t.Fatalf("present empty topic is not exact main: %+v", page)
		}
	}
	query := url.Values{"q": {"中文"}, "topic_id": {topic}}
	page := decoded[store.HistoryPage](t, a.request("GET", base+"?"+query.Encode(), ""), 200)
	if !reflect.DeepEqual(page.Items, []domain.Message{reply}) {
		t.Fatalf("encoded exact topic: %+v", page)
	}
	for _, tc := range []struct {
		path string
		want domain.Message
	}{
		{base + "/" + main.ID, main}, {base + "/" + main.ID + "?topic_id=", main},
		{base + "/" + reply.ID, reply}, {base + "/" + reply.ID + "?topic_id=" + url.QueryEscape(topic), reply},
	} {
		if got := decoded[domain.Message](t, a.request("GET", tc.path, ""), 200); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("ordinary message/delivery detail changed: %+v", got)
		}
	}
	for _, path := range []string{
		base + "/" + reply.ID + "?topic_id=", base + "/" + main.ID + "?topic_id=" + url.QueryEscape(topic),
		"/api/v1/groups/" + other.ID + "/history/" + main.ID, base + "/missing",
		"/api/v1/groups/missing/history?q=中文", "/api/v1/groups/missing/history/" + main.ID,
	} {
		w := a.request("GET", path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	for _, query := range []string{"q=absent", "q=中文&topic_id=missing"} {
		w := a.request("GET", base+"?"+query, "")
		page := decoded[store.HistoryPage](t, w, 200)
		if page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" || w.Body.String() != "{\"items\":[],\"next_cursor\":\"\"}\n" {
			t.Fatalf("empty JSON page: %s", w.Body)
		}
	}
}

func TestHistoryRoutesPaginationAndCursorScopes(t *testing.T) {
	a := openHistoryAPI(t)
	group, other := newGroup(t, a, "pages"), newGroup(t, a, "other")
	base := "/api/v1/groups/" + group.ID + "/history"
	want := make([]domain.Message, 0, 52)
	for i := range 52 {
		topic := ""
		if i%2 != 0 {
			topic = "other"
		}
		want = append(want, historyMessage(t, a, group.ID, topic, fmt.Sprintf("page 中文%%_ %d", i)))
	}
	historyMessage(t, a, other.ID, "", "page 中文%_ foreign")
	slices.Reverse(want)
	first := decoded[store.HistoryPage](t, a.request("GET", base+"?q=page", ""), 200)
	if !reflect.DeepEqual(first.Items, want[:50]) || first.NextCursor == "" {
		t.Fatalf("default page: %+v", first)
	}
	direct, err := a.db.SearchHistory(context.Background(), group.ID, store.HistoryQuery{Query: "page"})
	if err != nil || !reflect.DeepEqual(first, direct) {
		t.Fatalf("HTTP replaced the store cursor/page: %+v, %v", direct, err)
	}
	query := url.Values{"q": {" \u3000page \n"}, "limit": {"2"}, "before": {first.NextCursor}}
	last := decoded[store.HistoryPage](t, a.request("GET", base+"?"+query.Encode(), ""), 200)
	if !reflect.DeepEqual(last.Items, want[50:]) || last.NextCursor != "" {
		t.Fatalf("normalized query/exact-full final page: %+v", last)
	}
	for _, tc := range []struct {
		group string
		query url.Values
	}{
		{other.ID, url.Values{"q": {"page"}}}, {group.ID, url.Values{"q": {"PAGE"}}},
		{group.ID, url.Values{"q": {"page"}, "topic_id": {""}}}, {group.ID, url.Values{"q": {"page"}, "topic_id": {"other"}}},
	} {
		tc.query.Set("before", first.NextCursor)
		w := a.request("GET", "/api/v1/groups/"+tc.group+"/history?"+tc.query.Encode(), "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	exact := decoded[store.HistoryPage](t, a.request("GET", base+"?q=page&topic_id=&limit=10", ""), 200)
	if len(exact.Items) != 10 || exact.NextCursor == "" {
		t.Fatalf("main-topic first page: %+v", exact)
	}
	for _, raw := range []string{"q=page", "q=page&topic_id=other"} {
		w := a.request("GET", base+"?"+raw+"&before="+url.QueryEscape(exact.NextCursor), "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	tail := decoded[store.HistoryPage](t, a.request("GET", base+"?q=page&topic_id=&before="+url.QueryEscape(exact.NextCursor), ""), 200)
	if len(tail.Items) != 16 || tail.NextCursor != "" {
		t.Fatalf("main-topic continuation: %+v", tail)
	}
	for _, m := range tail.Items {
		if m.TopicID != "" {
			t.Fatalf("main-topic cursor leaked a reply: %+v", m)
		}
	}
}

func TestHistoryRoutesRealHTTPHostAndMethods(t *testing.T) {
	a := openHistoryAPI(t)
	group := newGroup(t, a, "transport")
	m := historyMessage(t, a, group.ID, "", "网络历史")
	base := "/api/v1/groups/" + group.ID + "/history"
	local := platform.NewLocalUser(a.db)
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = mustServer(t, ts.Listener.Addr().String(), append(LocalUserRoutes(local), HistoryRoutes(local)...)...)
	ts.Start()
	defer ts.Close()
	client := ts.Client()
	client.Timeout = 5 * time.Second
	for _, tc := range []struct {
		method, path, host, code string
		status                   int
		want                     any
	}{
		{"GET", base + "?q=" + url.QueryEscape("网络"), "", "", 200, store.HistoryPage{Items: []domain.Message{m}}},
		{"GET", base + "/" + m.ID, "", "", 200, m},
		{"GET", base + "?q=x", "attacker.example", "invalid_host", 403, nil},
		{"GET", base + "/" + m.ID, "attacker.example", "invalid_host", 403, nil},
		{"POST", base, "", "method_not_allowed", 405, nil},
		{"OPTIONS", base + "/" + m.ID, "", "method_not_allowed", 405, nil},
	} {
		r, err := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = tc.host
		r.Header.Set("X-Forwarded-Host", ts.Listener.Addr().String())
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if tc.code != "" {
			assertError(t, response.StatusCode, response.Header, body, tc.status, tc.code)
			if tc.status == 405 && response.Header.Get("Allow") != "GET" {
				t.Fatalf("unexpected methods: %v", response.Header)
			}
			continue
		}
		want, err := json.Marshal(tc.want)
		if err != nil || response.StatusCode != tc.status || string(body) != string(want)+"\n" {
			t.Fatalf("real HTTP response changed: %d %s, %v", response.StatusCode, body, err)
		}
	}
}
