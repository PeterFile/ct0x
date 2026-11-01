package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
)

func coordinationHTTPCapture(t *testing.T, a *groupAPI, groupID, topic string) domain.Trigger {
	t.Helper()
	request := coordinationHTTPSource(t, a, groupID, topic)
	trigger, err := coordination.New(a.db).ReceiveLocalMessage(context.Background(), domain.ThreadRef{GroupID: groupID, TopicID: topic}, request.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	return trigger
}

func coordinationHTTPCursor(t *testing.T, cursor pageCursor) string {
	t.Helper()
	raw, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestCoordinationHTTPPaginationBoundsScopeAndActualRows(t *testing.T) {
	a := openCoordinationAPI(t, "")
	g, other := newGroup(t, a, "group"), newGroup(t, a, "other")
	base := coordinationHTTPBase(g.ID) + "/triggers"
	want := make([]domain.Trigger, 52)
	for i := range want {
		want[i] = coordinationHTTPCapture(t, a, g.ID, "")
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].CreatedAt.Equal(want[j].CreatedAt) {
			return want[i].ID > want[j].ID
		}
		return want[i].CreatedAt.After(want[j].CreatedAt)
	})
	foreignTopic := coordinationHTTPCapture(t, a, g.ID, "other-topic")
	foreignGroup := coordinationHTTPCapture(t, a, other.ID, "")
	raw := messageSQL(t, a)
	before := coordinationHTTPSnapshot(t, raw)
	defer coordinationHTTPUnchanged(t, raw, before)
	defaults := decoded[listResult[domain.Trigger]](t, a.request("GET", base, ""), 200)
	maximum := decoded[listResult[domain.Trigger]](t, a.request("GET", base+"?limit=200", ""), 200)
	minimum := decoded[listResult[domain.Trigger]](t, a.request("GET", base+"?limit=1", ""), 200)
	if !reflect.DeepEqual(defaults.Items, want[:50]) || defaults.NextCursor == "" || !reflect.DeepEqual(maximum.Items, want) || maximum.NextCursor != "" || !reflect.DeepEqual(minimum.Items, want[:1]) || minimum.NextCursor == "" {
		t.Fatalf("pagination bounds: default=%d max=%d min=%d", len(defaults.Items), len(maximum.Items), len(minimum.Items))
	}
	first := decoded[listResult[domain.Trigger]](t, a.request("GET", base+"?limit=26", ""), 200)
	tail := decoded[listResult[domain.Trigger]](t, a.request("GET", base+"?topic_id=&limit=26&before="+first.NextCursor, ""), 200)
	if !reflect.DeepEqual(first.Items, want[:26]) || first.NextCursor == "" || !reflect.DeepEqual(tail.Items, want[26:]) || tail.NextCursor != "" {
		t.Fatalf("exact-final continuation: first=%+v tail=%+v", first, tail)
	}
	var cursor pageCursor
	if err := parseCursor(first.NextCursor, &cursor); err != nil || cursor.GroupID != g.ID || cursor.ID != want[25].ID || !cursor.CreatedAt.Equal(want[25].CreatedAt) {
		t.Fatalf("cursor position/context: %+v, %v", cursor, err)
	}
	for _, changed := range []pageCursor{
		{1, "tasks", g.ID, cursor.CreatedAt, cursor.ID},
		{1, cursor.Scope, other.ID, cursor.CreatedAt, cursor.ID},
		{2, cursor.Scope, g.ID, cursor.CreatedAt, cursor.ID},
		{1, cursor.Scope, g.ID, time.Time{}, cursor.ID},
		{1, cursor.Scope, g.ID, cursor.CreatedAt, "has space"},
	} {
		w := a.request("GET", base+"?before="+coordinationHTTPCursor(t, changed), "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, path := range []string{
		coordinationHTTPBase(other.ID) + "/triggers?before=" + first.NextCursor,
		base + "?topic_id=other-topic&before=" + first.NextCursor,
		"/api/v1/groups/" + g.ID + "/tasks?before=" + first.NextCursor,
	} {
		w := a.request("GET", path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	// Matching transport context is not authorization: the real cursor row must match.
	for _, row := range []domain.Trigger{foreignTopic, foreignGroup, {ID: "missing", CreatedAt: cursor.CreatedAt}, {ID: cursor.ID, CreatedAt: cursor.CreatedAt.Add(time.Nanosecond)}} {
		forged := cursor
		forged.ID, forged.CreatedAt = row.ID, row.CreatedAt
		w := a.request("GET", base+"?before="+coordinationHTTPCursor(t, forged), "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 404, "not_found")
	}
	last := want[len(want)-1]
	cursor.ID, cursor.CreatedAt = last.ID, last.CreatedAt
	end := decoded[listResult[domain.Trigger]](t, a.request("GET", base+"?before="+coordinationHTTPCursor(t, cursor), ""), 200)
	if end.Items == nil || len(end.Items) != 0 || end.NextCursor != "" {
		t.Fatalf("empty final page: %+v", end)
	}
	for _, body := range []string{
		`{"v":1,"v":1}`, `{"v":1,"extra":true}`, `{"v":1} {}`, `null`, `[]`,
	} {
		w := a.request("GET", base+"?before="+base64.RawURLEncoding.EncodeToString([]byte(body)), "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
}

func TestCoordinationHTTPLongOpaqueTopicFollowsOwnCursor(t *testing.T) {
	a := openCoordinationAPI(t, "")
	g := newGroup(t, a, "group")
	for _, topic := range []string{" \n\t ", strings.Repeat("原始 topic\n", 600)} {
		want := make([]domain.Trigger, 4)
		for i := range want {
			want[3-i] = coordinationHTTPCapture(t, a, g.ID, topic)
		}
		base := coordinationHTTPBase(g.ID) + "/triggers?topic_id=" + url.QueryEscape(topic) + "&limit=2"
		first := decoded[listResult[domain.Trigger]](t, a.request("GET", base, ""), 200)
		if !reflect.DeepEqual(first.Items, want[:2]) || first.NextCursor == "" || len(first.NextCursor) > 2048 {
			t.Fatalf("long opaque topic cannot produce bounded cursor: bytes=%d %+v", len(topic), first)
		}
		second := decoded[listResult[domain.Trigger]](t, a.request("GET", base+"&before="+first.NextCursor, ""), 200)
		if !reflect.DeepEqual(second.Items, want[2:]) || second.NextCursor != "" {
			t.Fatalf("long opaque topic cannot follow its own cursor: %+v", second)
		}
		w := a.request("GET", coordinationHTTPBase(g.ID)+"/triggers?topic_id="+url.QueryEscape(strings.TrimSpace(topic))+"&before="+first.NextCursor, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
}

func TestCoordinationHTTPRejectsWrongQueriesAndMalformedIDs(t *testing.T) {
	a := openCoordinationAPI(t, "")
	g := newGroup(t, a, "group")
	request := coordinationHTTPSource(t, a, g.ID, "")
	receipt := decoded[domain.AdmissionReceipt](t, a.request("POST", coordinationHTTPBase(g.ID)+"/requests", coordinationHTTPBody(t, request), "query"), 201)
	paths := coordinationHTTPGetPaths(receipt)
	for i := range paths {
		paths[i] = strings.Split(paths[i], "?")[0]
	}
	raw := messageSQL(t, a)
	before := coordinationHTTPSnapshot(t, raw)
	defer coordinationHTTPUnchanged(t, raw, before)
	for _, path := range paths[:3] {
		for _, query := range []string{
			"topic_id=a&topic_id=b", "topic_id=&%74opic_id=", "Topic_id=", "topic_id=%ZZ", "topic_id=%", "topic_id=a;b", "unknown=", "after=x", "cursor=x", "actor_id=user", "group_id=other", "grants=x",
		} {
			w := a.request("GET", path+"?"+query, "")
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		}
		if path != paths[1] {
			for _, query := range []string{"limit=1", "before=x", "before="} {
				w := a.request("GET", path+"?"+query, "")
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
			}
		}
	}
	for _, query := range []string{
		"limit=", "limit=0", "limit=-1", "limit=201", "limit=1.5", "limit=x", "limit=+1", "limit=1&limit=2", "limit=1&%6cimit=2", "before=", "before=x", "before=x&before=x", "before=" + strings.Repeat("A", 2049),
	} {
		w := a.request("GET", paths[1]+"?"+query, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, query := range []string{"topic_id=", "topic_id=x", "before=x", "limit=1", "unknown=", "%ZZ=x"} {
		w := a.request("GET", paths[3]+"?"+query, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
		w = a.request("POST", coordinationHTTPBase(g.ID)+"/requests?"+query, coordinationHTTPBody(t, request), "query")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, value := range []string{" ", "bad\n", "nul\x00id", strings.Repeat("x", 4097)} {
		for _, target := range []string{
			coordinationHTTPBase(g.ID) + "/triggers/" + url.PathEscape(value),
			coordinationHTTPBase(g.ID) + "/decisions/" + url.PathEscape(value),
			"/api/v1/groups/" + g.ID + "/tasks/" + url.PathEscape(value) + "/coordination",
			"/api/v1/groups/" + url.PathEscape(value) + "/coordination/triggers",
		} {
			r := httptest.NewRequest("GET", "http://"+DefaultAddress+target, nil)
			w := httptest.NewRecorder()
			a.handler.ServeHTTP(w, r)
			if w.Code != 400 {
				t.Fatalf("malformed reference %q: %d %s", fmt.Sprint(value), w.Code, w.Body)
			}
		}
	}
}
