package httpapi

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/store"
)

func TestHistoryRoutesStrictQueries(t *testing.T) {
	a := openHistoryAPI(t)
	group := newGroup(t, a, "validation")
	m := historyMessage(t, a, group.ID, "", "x")
	base := "/api/v1/groups/" + group.ID + "/history"
	queries := []string{
		"", "q", "q=", "q=%20%09%E3%80%80", "q=x&q=x", "q=x&%71=x", "Q=x",
		"q=%", "q=%2", "q=%ZZ", "q=x&%ZZ=y", "q=x;limit=1", "q=x&topic_id=%GG",
		"q=%FF", "q=%00", "q=x&topic_id=%FF", "q=x&topic_id=%00",
		"q=x&topic_id=a&topic_id=b", "q=x&topic_id=&%74opic_id=", "q=x&limit=1&limit=2",
		"q=x&before=a&before=a", "q=x&before=", "q=x&before=not-a-cursor", "q=x&before=%20",
		"q=x&before=" + strings.Repeat("A", 2049),
	}
	for _, field := range []string{"after", "hwm", "actor_id", "member_id", "run_id", "group_id", "grants", "extra"} {
		queries = append(queries, "q=x&"+field+"=forged")
	}
	for _, limit := range []string{"", "0", "-1", "201", "1.5", "1e2", "x", " 1", "+1", "１", strings.Repeat("9", 40)} {
		queries = append(queries, "q=x&limit="+url.QueryEscape(limit))
	}
	for _, query := range []string{strings.Repeat("a", 257), strings.Repeat("😀", 257), strings.Repeat(" ", 4096) + "x"} {
		queries = append(queries, "q="+url.QueryEscape(query))
	}
	for _, topic := range []string{strings.Repeat("a", 257), strings.Repeat("😀", 65)} {
		queries = append(queries, "q=x&topic_id="+url.QueryEscape(topic))
	}
	for _, query := range queries {
		w := a.request("GET", base+"?"+query, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
	for _, query := range []string{
		"q=x", "limit=1", "before=x", "after=x", "actor_id=forged", "member_id=forged", "run_id=forged", "grants=forged",
		"extra=x", "topic_id=a&topic_id=a", "topic_id=&%74opic_id=", "topic_id=%", "topic_id=%ZZ", "topic_id=x;y",
		"topic_id=%FF", "topic_id=%00", "topic_id=" + strings.Repeat("a", 257), "topic_id=" + url.QueryEscape(strings.Repeat("😀", 65)),
	} {
		w := a.request("GET", base+"/"+m.ID+"?"+query, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	}
}

func TestHistoryRoutesQueryBoundaries(t *testing.T) {
	a := openHistoryAPI(t)
	group := newGroup(t, a, "boundaries")
	content := "x " + strings.Repeat("a", 256) + " " + strings.Repeat("界", 256) + " " + strings.Repeat("😀", 256)
	m := historyMessage(t, a, group.ID, "", content)
	base := "/api/v1/groups/" + group.ID + "/history"
	for _, query := range []string{
		"q=x&limit=1", "q=x&limit=200", "q=x&limit=001",
		"q=" + url.QueryEscape(strings.Repeat("a", 256)), "q=" + url.QueryEscape(strings.Repeat("界", 256)),
		"q=" + url.QueryEscape(strings.Repeat("😀", 256)), "q=" + url.QueryEscape(strings.Repeat(" ", 4095)+"x"),
	} {
		page := decoded[store.HistoryPage](t, a.request("GET", base+"?"+query, ""), 200)
		if len(page.Items) != 1 || page.Items[0].ID != m.ID || page.NextCursor != "" {
			t.Fatalf("valid boundary query failed (%d raw bytes): %+v", len(query), page)
		}
	}
	// Both have valid decoded values: only the HTTP raw-query bound differs.
	raw := "q=" + strings.Repeat("%20", 2727) + "x&limit=1"
	if len(raw) != 8192 {
		t.Fatal("incorrect raw-query boundary fixture")
	}
	page := decoded[store.HistoryPage](t, a.request("GET", base+"?"+raw, ""), 200)
	if len(page.Items) != 1 || page.Items[0].ID != m.ID {
		t.Fatalf("8192-byte query rejected: %+v", page)
	}
	w := a.request("GET", base+"?"+raw+"&", "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	raw = "topic_id=" + strings.Repeat("&", 8192-len("topic_id="))
	if got := decoded[domain.Message](t, a.request("GET", base+"/"+m.ID+"?"+raw, ""), 200); got.ID != m.ID {
		t.Fatalf("8192-byte detail query rejected: %+v", got)
	}
	w = a.request("GET", base+"/"+m.ID+"?"+raw+"&", "")
	assertError(t, w.Code, w.Header(), w.Body.Bytes(), 400, "invalid_input")
	for _, topic := range []string{"", " ", strings.Repeat("a", 256), strings.Repeat("😀", 64)} {
		topicMessage := historyMessage(t, a, group.ID, topic, "topic-boundary")
		query := url.Values{"q": {"topic-boundary"}, "topic_id": {topic}}
		page := decoded[store.HistoryPage](t, a.request("GET", base+"?"+query.Encode(), ""), 200)
		if len(page.Items) != 1 || page.Items[0].ID != topicMessage.ID {
			t.Fatalf("valid topic boundary lost (%d bytes): %+v", len(topic), page)
		}
		got := decoded[domain.Message](t, a.request("GET", base+"/"+topicMessage.ID+"?topic_id="+url.QueryEscape(topic), ""), 200)
		if got.ID != topicMessage.ID || got.TopicID != topic {
			t.Fatalf("detail topic bytes changed: %+v", got)
		}
	}
}

func TestHistoryRoutesSharedOperationErrors(t *testing.T) {
	a := openHistoryAPI(t)
	group := newGroup(t, a, "errors")
	m := historyMessage(t, a, group.ID, "", "history")
	base := "/api/v1/groups/" + group.ID + "/history"
	paths := []string{base + "?q=history", base + "/" + m.ID}
	for _, expired := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		status, code := 408, "request_canceled"
		if expired {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			status, code = 504, "deadline_exceeded"
		}
		cancel()
		for _, path := range paths {
			r := httptest.NewRequest("GET", "http://"+DefaultAddress+path, nil).WithContext(ctx)
			w := httptest.NewRecorder()
			a.handler.ServeHTTP(w, r)
			assertError(t, w.Code, w.Header(), w.Body.Bytes(), status, code)
		}
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		w := a.request("GET", path, "")
		assertError(t, w.Code, w.Header(), w.Body.Bytes(), 500, "internal_error")
		if strings.Contains(w.Body.String(), a.path) || strings.Contains(w.Body.String(), "sql") || strings.Contains(w.Body.String(), "closed") {
			t.Fatalf("internal store error leaked: %s", w.Body)
		}
	}
}
